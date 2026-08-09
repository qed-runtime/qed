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
network listener、authentication、authorization、rate limit、tenant、request schemaは組み込み先applicationが所有します

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
handlerはlow-level handleを受け取るため、process内のapproval Adapterからwaiting Runをresumeできます

別requestやworkerがhandleを保持する場合、Eventを独立してstreamする場合、後からRunをresumeする場合は`Host.Start`を利用します
`Start`のcallerがEvent drainと`Wait`を所有し、完了後に`Host.SaveRunEvidence`を呼べます

shutdown時は新規workの受付を停止し、active Runをcancelまたは完了させてから`Host.CloseContext`で所有する全Extension processをdrainして停止します

## security境界

Extension process境界はcrashとprotocol stateを隔離しますがOS-level sandboxではありません
child processと`run_command`は、組み込み先がcontainer、OS sandbox、制限account、network policy、別execution backendを追加しない限りHost account権限を持ちます

Provider credentialはHostが解決し、既定ではExtension environmentへ追加されません
各Extensionが必要とするenvironment名だけを選択してください

application所有Extensionを含む標準library HTTP組み込みは[embedded server example](../examples/embedded-server/README.md)を参照してください
