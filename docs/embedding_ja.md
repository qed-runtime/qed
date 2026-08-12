# QEDを別applicationへ組み込む

applicationはQEDをGo moduleとしてimportします
Agent graphや組み込みExtensionを所有するためにQED repositoryをforkする必要はありません

application側のrepositoryが次を所有します

- `go.mod`と`go.sum`
- Agent設定
- `extensions.lock`
- 生成済みExtension catalog
- HTTP、gRPC、queue、desktop、job Adapter
- 最終executableとdeployment policy

QED rootの`extensions.lock`は公式`qed` executableへ組み込むExtensionだけを選択します

## 組み込みlayer

組み込み先に合う最も低いlayerを利用します

| Layer | 用途 |
| --- | --- |
| `agent.Runtime` | 1つのprovider-neutral Runtimeを直接構築する |
| `orchestration.AgentRegistry` | 複数の名前付きRuntimeやdelegationを構成する |
| `qed.Host` | 完全なAgent graphを読み込み、Profile、Store、Extension lifecycleを所有する |

`qed.Host`はtransport-neutralです
network listener、authentication、authorization、inbound clientまたはtenant rate limit、tenant、request schemaは組み込み先applicationが所有します
QEDはこれとは別に、設定済みoutbound Provider concurrencyとcooldown policyを適用します

`HostLoadOptions.ToolInputValidator`は読み込んだ全Runtimeとhost側Extension proxyの既定JSON Schema subsetを置き換えます
validatorはprocess境界を越えてserializeされません
同じcustom dialectを使うexternalまたはself-exec Extensionは`server.Options.ToolInputValidator`にも設定する必要があり、未設定時はExtension serverが既定subsetを使います
validation順序と対応keywordは[Extension process](extensions_ja.md#tool)を参照してください

## application所有のself-exec catalog

外部applicationはQEDの`internal` packageをimportせずに独自Go Extensionをlinkできます

```text
company-agent/
├── go.mod
├── go.sum
├── qed.json
├── extensions.lock
├── extensionregistry/
│   ├── registry.go
│   └── registry_gen.go
└── cmd/company-agent/main.go
```

application側で生成packageを宣言します

```go
package extensionregistry

//go:generate go run github.com/qed-runtime/qed/cmd/qed-extension-gen --lock ../extensions.lock --output registry_gen.go --package extensionregistry --variable Catalog
```

dependencyを変更せず生成または鮮度確認できます

```sh
go generate ./extensionregistry

go run github.com/qed-runtime/qed/cmd/qed-extension-gen \
  --lock extensions.lock \
  --output extensionregistry/registry_gen.go \
  --package extensionregistry \
  --variable Catalog \
  --check
```

生成される`Catalog`は公開`*selfexec.Catalog`です
各definitionはlock済みdeclarationとlink済み`func() server.Options` factoryを保持します

## process entrypoint

application本来のcommand line argumentを解析する前にchild modeをdispatchします

```go
handled, err := extensionregistry.Catalog.Dispatch(ctx, selfexec.DispatchOptions{
    Arguments:   os.Args[1:],
    Input:       os.Stdin,
    Output:      os.Stdout,
    DebugWriter: os.Stderr,
})
if handled {
    return err
}
```

通常のHost modeでは同じcatalogを読み込みます

```go
executable, err := os.Executable()
if err != nil {
    return err
}
executable, err = filepath.Abs(executable)
if err != nil {
    return err
}

host, err := qed.LoadHost("qed.json", qed.HostLoadOptions{
    LookupEnv:       os.LookupEnv,
    WorkspaceRoot:   workspaceRoot,
    SelfExecutable:  executable,
    SelfExecCatalog: extensionregistry.Catalog,
})
if err != nil {
    return err
}
defer host.Close()
```

`"mode":"self-exec"`を指定したExtensionは渡したcatalogに存在する必要があります
`LoadHost`はlock済みdeclarationを通常のExtension Hostへ渡すため、Handshake、Initialize、Describe、manifest validation、HealthCheck、Policy、Evidence、generation lease、shutdownは外部executableと同じです

## server requestから実行する

読み込み後の`Host`は複数Runから並行利用できます

1つの設定からloadした全RunはProvider profileごとのlimiterを共有します
これによりserver requestとsubagentを横断して並行outbound model streamを制限します
組み込み先applicationはRun開始前のadmission、tenant、cost、request rate policyを引き続き所有します

```go
outcome, err := host.Run(request.Context(), agent.RunRequest{
    AgentID:   input.AgentID,
    SessionID: input.SessionID,
    Input: []agent.Message{
        {Role: agent.RoleUser, Text: input.Prompt},
    },
}, func(ctx context.Context, handle *agent.RunHandle, event agent.Event) error {
    return publishEvent(ctx, event)
})
```

`Host.Run`はordered Event streamをdrainし、terminal Resultと全Eventを返し、設定済みの場合はEvidence Bundleを保存します
handler errorはRunをcancelします
handlerはlow-level handleを受け取るため、process内のapproval Adapterからwaiting Runをresumeしたり、Event drainを止めずにsteeringをqueueしたりできます

terminal `RunResult.ContextLedger`は受理済みEvent履歴から作るdeterministicな5 Ledger viewです
`agent.BuildContextLedger`はordered Eventから同じviewを再構築し、`agent.ValidateContextLedger`はderived stateの改ざんを拒否します
Constraint entryが正確なuser textを保持するためLedgerはcontent-bearingであり、Session Eventと同等に保護して保存および転送する必要があります
custom Context Compilerは`ContextCompileRequest.Ledger`から進行中Ledgerのisolated copyを受け取ります

embedding hostはProfileと独立してcanonical stateを注入できます

```go
runtime, err := agent.NewRuntime(agent.Options{
    Provider:                provider,
    CurrentWorldStateSource: source,
})
```

`agent.CurrentWorldStateSource`はlogical Provider request直前のisolated Run Eventと対応Ledgerを受け取ります
Sourceはread-onlyかつboundedな処理を行い、structuredなfile、Git、観測済みcheck stateを返す必要があります
Source errorはProvider call前にRunを失敗させます
Runtimeは`current_world_state.captured`を検証してpublishし、callerは`agent.ValidateCurrentWorldState`でcapture済みvalueを検証できます
`profile/coding`を直接使う場合はCoding Profile guideのように`codingProfile.CurrentWorldStateSource()`を渡します
宣言的`qed.Host`設定では自動接続します

Constraint Factは自然言語推論ではなく明示的なlifecycle controlを使います
directiveがないuser Messageはactive Factを作ります
後続Runで置換または解決する場合は`ContextLedger.Constraints`のIDを使うか、source Eventから`agent.ConstraintFactID`でIDを生成します

```go
targetID := previous.ContextLedger.Constraints[0].ID

handle, err := runtime.Run(ctx, agent.RunRequest{
    SessionID: previous.SessionID,
    Input: []agent.Message{{
        Role: agent.RoleUser,
        Text: "Use PostgreSQL instead",
        FactDirective: &agent.FactLifecycleDirective{
            Action:  agent.FactLifecycleSupersede,
            Targets: []string{targetID},
        },
    }},
})
```

`supersede`は指定した全active targetを失効させ、現在のMessageから1つのactive Factを作ります
`resolve`はtargetを解決済みにし、解決を伝えるMessageからFactを作りません
targetは過去の一意なactive Factである必要があり、`agent.MaxFactLifecycleTargets`を上限とします
shapeが不正な場合は`agent.ErrInvalidFactDirective`を返し、`Steer`は`agent.ErrInvalidSteeringMessage`にも分類します
RuntimeはHookと永続化より前に現在のEvent prefixでtarget stateを検証し、安全なsteering境界でactiveではなくなったtargetはEventをcommitせずRunを失敗させます

Runtimeはinputの`Message.FactDirective`を`Event.FactDirective`へ移します
保存MessageとProvider向けMessageにhost lifecycle metadataは残りません
publish済み`user.message.added` Eventがtransitionのcommit pointです

target IDはsource Eventを識別するため、RunをまたぐtransitionにはSession Storeが必要であり、過去Messageだけのreplayではidentityを維持できません

別requestやworkerがhandleを保持する場合、Eventを独立してstreamする場合、後からRunをresumeする場合は`Host.Start`を利用します
`Start`のcallerがEvent drainと`Wait`を所有し、完了後に`Host.SaveRunEvidence`を呼べます

active Runへplainかつnon-emptyなuser Messageを1つ追加する場合は`RunHandle.Steer`を使います

```go
err := handle.Steer(agent.Message{
    Role: agent.RoleUser,
    Text: "Prioritize the failing package before broader checks",
})
```

`Steer`は`agent.MaxPendingSteeringMessages`を上限とするnon-blocking FIFO操作です
nil errorはqueueがMessageを受理したことだけを表します
plain user inputが不正な場合、queueが満杯の場合、Runがclose済みの場合は、それぞれ`agent.ErrInvalidSteeringMessage`、`agent.ErrSteeringQueueFull`、`agent.ErrRunClosed`を返します
既存の`user.message.added` Eventで`UserMessageOrigin`が`steering`になった時点でSession stateへの反映が確定します
Runtimeはin-flight Provider requestやretryを変更せず、assistantのTool batchも中断しません
batch内の全Tool result完了後、またはend-turn response後、次のProvider requestをcompileする前にqueue済みMessageを適用します

`run.waiting`を観測した後の`Steer`は`agent.ErrRunWaiting`を返すため、matching requestには`RunHandle.Resume`を使います
wait前にqueue済みのsteeringはresumeとTool completionまで保留します
cancel、Deadline切れ、terminal Run failureは後続の適用を停止し、`user.message.added` Eventを持たないqueue済みMessageを破棄する場合があります
どのMessageが適用されたかはEvent streamを正とします

queue上限はMessage件数でありbyte数ではありません
組み込みapplicationは`Steer`を呼ぶ前にrequest sizeとtenant memory上限を適用する必要があります

steering自体はBudgetを消費せず、後続のProviderとTool処理が同じactive Run Budgetを使います
follow-upはEventをdrainして`Wait`を呼んだ後、設定済みSession Storeと同じSession IDで新しいRunとして開始します
follow-upは永続Sessionをreplayしつつ新しいRun IDとRuntime local上限を持ちます
Session Storeがない場合はcallerが過去contextを渡し、両方のRunで1つの共有上限が必要な場合だけ同じ`*agent.Budget`を明示的に再利用します

互換性に関する注意として、steeringはEvent JSONへ任意fieldの`user_message_origin`、Fact lifecycleは任意fieldの`fact_directive`を追加します
既存の`user.message.added` HookもこれらのEventを観測します
外部decoderは任意fieldを受理する必要があります
Goの`agent.Message`と`agent.Event` structにはexported fieldが増え、Ledger v2は`agent.ConstraintLedgerEntry`を拡張するため、外部のcomposite literalはfield名を指定してください

Current World Stateは`current_world_state.captured` Event type、Event、terminal Result、Session snapshotのoptional `current_world_state` field、`current_world_state` Segment kindを追加します
strictなEvent type switchは新しいEventを受理または明示的に無視する必要があります
snapshot内のpathとcommand argumentはfile、diff、stdout、stderr contentを含まなくてもcontent-bearing metadataです

Deterministic LedgerによりTool resultへ任意の`policy` metadata、terminal Run resultへ`context_ledger`、新しいCheckpointへ`ledger`参照が追加されます
host Policyのraw reasonは含めず、Policy metadataをmodel向けTool Messageへコピーしません
strictな外部JSON decoderはこれらの追加fieldを受理する必要があります
Ledger schema v2はFact stateとtransition provenanceを追加し、replayではLedger v1が作成したCheckpoint参照も引き続き検証します
`ValidateContextLedger`は現在schemaと比較するため、standaloneなv1 Ledger snapshotは検証前にEventから再構築する必要があります

shutdown時は新規workの受付を停止し、active Runをcancelまたは完了させてから`Host.CloseContext`で所有する全Extension processをdrainして停止します

読み込まれたCoding Profileはbounded automatic Extension restartを使います
中断したRunはcrashしたgenerationのRPC failureを受け取り、自動再実行されません
新しいRunはcomponent acquisitionで一時的に`host.ErrExtensionRestarting`を受け取り、attempt上限後は`host.ErrExtensionCircuitOpen`を受け取る場合があります
`host.Manager`を直接構築するapplicationは`host.DefaultRestartPolicy`でopt-inし、`Manager.RestartStatus`を確認できます

## security境界

Extension process境界はcrashとprotocol stateを隔離しますがOS-level sandboxではありません
child processと`run_command`は、組み込み先がcontainer、OS sandbox、制限account、network policy、別execution backendを追加しない限りHost account権限を持ちます

Provider credentialはHostが解決し、既定ではExtension environmentへ追加されません
各Extensionが必要とするenvironment名だけを選択してください

application所有Extensionを含む標準library HTTP組み込みは[embedded server example](../examples/embedded-server/README.md)を参照してください
