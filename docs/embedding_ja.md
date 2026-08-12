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

custom `CheckpointStrategy`は明示的な`CheckpointRequest.Mode`、target `Generation`、正確なraw `Messages`、isolated `Events`、対応Ledgerを受け取ります
`CheckpointBuildRawRebase`では`Previous`が常にnilになり、`RebaseReason`が決定的なtriggerを示します
Strategyは前回semantic outputを再要約せずraw sourceから再構築する必要があります

## scope付きEvidence access

`CompactingContextCompiler`を使うembedding hostはscope付きObject StoreとRuntime identityを組み合わせて設定します

```go
objects := evidence.NewMemoryObjectStore()
compiler, err := agent.NewCompactingContextCompiler(policy, objects, nil)
if err != nil {
    return err
}

runtime, err := agent.NewRuntime(agent.Options{
    Provider:        provider,
    ContextCompiler: compiler,
    EvidenceAccess: &agent.RuntimeEvidenceAccess{
        TenantID:     "tenant-a",
        ProfileID:    "coding",
        PrincipalID:  "company-agent",
        Capabilities: []string{
            agent.EvidenceReadCapability,
            agent.EvidenceWriteCapability,
        },
        Sensitivity: agent.EvidenceSensitivityPrivate,
    },
    ContextRetrieval: &agent.ContextRetrievalOptions{
        ObjectStore: objects,
        Limits: agent.ContextRetrievalLimits{
            MaxCallsPerRun:        16,
            MaxItemsPerCall:       32,
            MaxItemsPerRun:        128,
            MaxOutputBytesPerCall: 64 << 10,
            MaxOutputBytesPerRun:  256 << 10,
        },
    },
})
```

Runtimeはconcrete scopeをmodelから受け取らず自分で導出します
Session ID付きRunはtenant、Session、Profileを使うためterminal follow-up間でexact Evidenceを共有します
SessionなしRunは生成済みRun IDを使い、他のephemeral Runから分離されます
subagentはcontextから認証済みtenantを継承しつつ独自RunとProfile scopeを導出します

multi-tenant serverでは`RuntimeEvidenceAccess.TenantID`を空にし、request境界で認証済みtenantを設定します

```go
ctx := agent.WithEvidenceTenant(request.Context(), authenticatedTenantID)
handle, err := runtime.Run(ctx, runRequest)
```

Runtimeに固定tenantがある場合、異なるcontextual tenantは`agent.ErrEvidenceAccessDenied`になります
parent `EvidenceAccess`がある場合、child RuntimeのCapabilityはparentと設定値の共通部分へ制限されます

`EvidenceObjectRef.Digest`はcontent identityに限られます
scope付きretrievalには完全なopaque bindingと一致する`EvidenceAccess`が必要です
built-in MemoryとJSON Storeは`ScopedEvidenceObjectStore`を実装し、`secret` contentを拒否してaccess attemptを記録します
許可済みretrievalもaudit recordをcommitできなければcontentを返しません
trusted local operatorはoptionalな`EvidenceObjectAdminStore`を利用でき、そのbypassも監査されます

`ContextRetrieval`は`context_search`、`context_fetch`、`session_timeline`、`artifact_history`、`execution_history`を明示的に登録します
nilの場合はどのToolも登録しません
searchは既定で以前のEvent本文を決定的なcase-insensitive一致により新しい順で検索します
`order: "relevance"`ではboundedかつ固定されたEvent prefixをrankingし、各snippetのfactor内訳を返します
timelineとLedger historyは本文を含まないmetadataを返します
fetchはcontent digestをlocatorとしてだけ受け取り、現在のRunまたはSession Event履歴から完全な参照を解決し、一致するscope付きaccessを要求してからUTF-8 textを読みます
Runtimeは返却byte長とSHA-256 digestを完全な参照と照合します

成功resultは完全なJSONとしてcall単位とRun単位のitem数、出力byte数で制限されます
call回数もRun単位で制限され、通常のRuntime Tool call budgetも消費します
listと既定searchは新しい順のcursorを使います
relevance searchは`snapshot_event_count`と`snapshot_query_digest`を返し、後続pageで同じqueryと両方の値を要求し、その固定ranking内のoffsetとして`next_cursor`を使います
fetchはUTF-8境界の`next_offset`を返します
返却snippetとEvidence contentは`untrusted: true`を持ち、命令として解釈してはいけません

組み込みhostはembeddingをRuntimeの依存にせず、決定的なrelevance scoreを拡張できます

```go
runtime, err := agent.NewRuntime(agent.Options{
    Provider: provider,
    ContextRetrieval: &agent.ContextRetrievalOptions{
        ObjectStore:    objectStore,
        SemanticScorer: scorer,
    },
})
```

`agent.ContextSemanticScorer`はexact query、boundedな最新task prefix、最大`agent.MaxContextSemanticCandidates`件のuntrusted excerptを受け取ります
各excerptは最大`agent.MaxContextSemanticCandidateBytes`です
scorerはcandidate順を維持し、各candidateに`0..1000`の整数を1つ返し、cancelを尊重して並行callに対応する必要があります
不正outputはboundedな通常Tool errorになります
scorer failure時にrankingへ暗黙fallbackせず、modelは`recency`で再試行できます
Session本文を外部scorerへ開示できるか、credential、cost、決定性はhostが所有します
`qed.HostLoadOptions.ContextSemanticScorer`は同じscorerを宣言設定のretrieval有効Agentへ接続します
scorer callはTool context内で実行されますがProvider attemptではなくRuntimeもretryしないため、必要な外部call rate limitとusage accountingはhostが適用します

組み込みhostはQEDの依存なしtoken近似も置き換えられます

```go
runtime, err := agent.NewRuntime(agent.Options{
    Provider:        provider,
    ContextCompiler: compactingCompiler,
    TokenEstimator:  estimator,
    PredictiveBudget: &agent.PredictiveBudgetPolicy{
        ContextWindowTokens:       128000,
        OutputReserveTokens:       8192,
        SafetyMarginTokens:        4096,
        PredictedToolOutputTokens: 4096,
        SoftThresholdTokens:       102400,
    },
})
```

`agent.TokenEstimator`はpurpose付きbatchとしてisolated byte itemを受け取り、itemごとの非負countとstableかつsecretを含まないkindを返します
実装は順序を維持し、cancelを尊重し、並行callに対応し、contentを保持せず命令として扱わず、同じProvider、Model、Purpose、Contentから同じresultを返す必要があります
kindは`[a-z0-9][a-z0-9._/:-]{0,127}`に一致する必要があります
Runtimeは明示的な`agent.Options.TokenEstimator`、interfaceを実装するProvider、`agent.CanonicalByteTokenEstimator`の順で選択します
fallbackは各non-empty itemを`ceil(bytes / 4)`で予測し、依存を必要としません

built-in Context compilerはcanonical logical Segmentへ契約を適用し、relevance searchはboundedなuntrusted snippetへ適用します
custom Context compilerは`ContextCompileRequest`から解決済みestimatorを受け取ります
estimator failureはretryや暗黙fallbackを行わず、compileではProvider call前に停止し、retrievalでは通常Tool errorを返します
外部estimatorには外部semantic scorerと同じ開示、credential、rate limit、cost、決定性の考慮が必要です
`qed.HostLoadOptions.TokenEstimator`はhost値を宣言設定の全Agentへ接続します

`agent.BuildTokenUsageReport`はpublic Run EventからProvider attemptごとの本文なしreportを再構築します
Cache Plan estimateをProvider Usageと対応付けて`actual - estimate`を返し、Usage欠落はunreportedとして維持します

`agent.Options.PredictiveBudget`を設定するとRuntimeは次のinput、predicted Tool output、output reserveとsafety marginの大きい方を評価します
設定するcompilerは`agent.PredictiveContextCompiler`を実装する必要があり、built-in compacting compilerは対応済みです
soft thresholdではrequestを変更せず、検証済みinactive `SessionSnapshot.PreparedContext`を`context.compaction.prepared`で永続化します
context windowを超える場合は収まるcandidateを`context.compacted`で採用し、作成できなければProvider I/O前に停止します
`max_input_bytes`は独立した決定的hard境界として維持されます
各limitはmodel固有のoperator factであり、output reserveには実効Provider output上限以上を指定します

Runtimeは本文を含まない`ToolResult.ContextRetrieval` metadataを`tool.completed` EventとSession replayへ保持します
metadataはoperation、outcome、item数、出力byte数、truncation、任意のobject digest、call以前にcompactionがあったかを含みます
model向けTool Messageにはcopyされません
scope付きfetchはObject Store access audit recordも作成します

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

safe cut annotationによりTool resultへ任意の`context_operation` metadata、`ContextCompileRequest`へ`Events`が追加されます
Runtimeはisolatedなexact Event prefixを渡し、semantic cutの前にLedgerと照合します
directなCompiler callerはEventsを省略してToolだけのboundaryを維持できます
Extension peerはProtocol v2を実装する必要があり、v1 manifestとpeerはexact version negotiationで拒否されます

built-in Context retrievalはTool resultと`tool.completed` Event JSONへ任意の`context_retrieval` fieldを追加します
これはExtension Protocol fieldではなくadditiveなhost metadataです
strict Event decoderはこのfieldを許容する必要があります
予約済みbuilt-in Tool名はunderscoreを使い、hostが`ContextRetrievalOptions`を設定しない限り登録されません

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
