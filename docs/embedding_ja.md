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

互換性に関する注意として、steeringはEvent JSONへ任意fieldの`user_message_origin`を追加し、既存の`user.message.added` Hookもsteering Messageを観測します
外部decoderはこの任意fieldを受理する必要があります
Goの`agent.Event` structにはexported fieldが増えるため、外部のcomposite literalはfield名を指定してください

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
