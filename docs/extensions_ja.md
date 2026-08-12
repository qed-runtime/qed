# Extension process

QEDはTool、Event Hook、host-invoked Commandをversion付きprocess境界の背後に置きます
development executable、discovered package、QED self-exec childは同じProtocol v2 contractを使います

```text
Agent Run
   |
   v
atomic Extension generation set
   |
   v
Host Policy + approval + Evidence
   |
   v
4-byte length-prefixed JSON over stdio
   |
   +-- external or discovered executable
   |
   +-- QED self-exec child
```

process分離はlifecycleとcrashを隔離しますがsecurity sandboxではありません
Extensionと起動したprogramはchild process accountの権限を保持します

## Go Extension scaffold

既存Go module内へ最初のGo layoutを作成します

```sh
mkdir -p ./extensions
qed extension scaffold \
  ./extensions/example-extension \
  --id example.extension
```

destinationより上にある最も近い`go.mod`から生成import pathを決定します
parent directoryは既に存在し、destination自体は存在しない必要があります
IDはASCII letterまたはdigitで始まり、以降はletter、digit、dot、underscore、hyphenを利用でき、上限は256 byteです
implementation versionもASCII letterまたはdigitで始まり、同じ文字に加えて先頭以外で`+`を利用でき、上限は128 byteです
既定値は`0.1.0`で、`--extension-version`から指定できます
生成import path要素は[Go Modules Reference](https://go.dev/ref/mod)に従います
通常のbuild可能package layoutにならないhidden、underscore prefix、`testdata`のdestination要素は拒否します
module discoveryはregularかつnon-symlinkの`go.mod`から最大1 MiBを読みます

commandは次のlayoutを作成します

```text
example-extension/
  .gitignore
  README.md
  extension/
    extension.go
  main.go
  main_test.go
  qed-extension.json
```

`main.go`はexternal process entrypointです
nested `extension` packageは`Declaration`と`ServerOptions`をexportするため、同じimplementationをapplication所有`extensions.lock`からself-exec向けに選択できます
生成testはactual test processに対して`contracttest.RunLifecycle`を実行します
Tool、Hook、Commandを追加する場合はcomponent behavior testも追加します

scaffoldは新規directoryだけを作成します
空directoryを含む既存destinationを拒否し、生成失敗時は自身が作成したfileをrollbackします
Go dependency commandを実行せず、`go.mod`、`go.sum`、`extensions.lock`を変更しません
所有moduleは通常のdependency workflowでQED dependencyを追加する必要があります

## Protocol v2

各messageは4-byte unsigned big-endian payload lengthを前置したUTF-8 JSON objectです
envelope上限は8 MiBです
unknown field、trailing value、不正frame、空のcorrelation ID、version mismatchを拒否します

Protocol v2は`invoke_tool` resultへ任意の`context_operation` metadataを追加します
v1 decoderはunknown fieldを拒否するため、exact version negotiationではwire変更を互換扱いせずv1 peerを意図的に拒否します

requestは`version`、`id`、`method`、optionalな`params`を含みます
responseは`version`と`id`を反復し、`result`または`error`のどちらか一方だけを含みます
requestはmultiplexされ、responseは順不同で到着できます

| Method | 目的 |
| --- | --- |
| `handshake` | exact protocolとimplementation identityのnegotiate |
| `initialize` | workspace、選択environment、opaque configuration、verbose stateの供給 |
| `describe` | capability、Tool、Hook、Commandの登録 |
| `required_capabilities` | authorization前にTool invocation固有capabilityを解決 |
| `invoke_tool` | Run identity付きのauthorized Tool callを実行 |
| `handle_event` | 選択されたRun EventをHookへ配送 |
| `invoke_command` | authorizedなhost-requested Commandを実行 |
| `health_check` | initializedとdraining stateを報告 |
| `snapshot` | boundedなopaque JSON stateを返却 |
| `restore` | compatible generationへstateを適用 |
| `drain` | 新規workを拒否してaccepted requestを待機 |
| `cancel` | correlation IDでin-flight requestをcancel |
| `shutdown` | drain、cleanup callback、response、exit |

provider-private continuation stateはこのprotocol境界を越えません
Hookはpublic Agent Event JSONとRun identityだけを受け取ります

## Component

### Tool

各Toolはname、description、input schema、static capability、invocation固有capabilityの有無を宣言します
Hostはdynamic capabilityを問い合わせ、結合したsetを`capability.Policy`で評価し、必要なapprovalを取得した後だけ`invoke_tool`を送ります

RuntimeはProviderが返したargumentをdynamic capability解決、Policy、approval、Tool実行より前に検証します
Extension serverはdirect protocol callを含むRPC境界でも同じargumentを再検証します
validation failureは通常の失敗Tool resultとなり、modelは次の通常turnで修正できます
Provider failureではないためProvider retryは起動しません

全validator経路でschemaを1 MiB、argumentを8 MiB、nestingを64に制限し、重複JSON key、trailing value、不正JSONを拒否します
依存追加のない既定実装はJSON Schema subsetです
`integer`を含む全JSON `type`に加えて`properties`、`required`、booleanの`additionalProperties`、単一schemaの`items`、`minItems`、`minimum`、`maximum`、`enum`をサポートします
`description`と`title`はannotationとして受理します
既定実装ではcompile済みschema nodeと`required` nameは4096、`enum` entryは256が上限です
不正schemaと未対応keywordは無視せず拒否します
schema省略時はobject schemaを使用します

別dialectが必要なapplicationは`agent.ToolInputValidator`と`agent.CompiledToolInputValidator`を実装できます
`agent.Options`、`qed.HostLoadOptions`、`extension.ToolOptions`、`host.ManagerOptions`、`coding.Options`、`server.Options`から注入できます
custom host validatorはprocess-localであり、process分離Extensionは自身の`server.Options.ToolInputValidator`にも設定する必要があります
具象Tool decoderはdefense in depthとして引き続き必要です

Tool definitionはRuntimeへ入る前にExtension IDとgeneration metadataを受け取ります
Evidenceはそのoriginとargumentおよびoutputのhashを記録します
Host proxyは最終authorization outcome、sort済みcapability name、Policy reasonのdigestも`ToolResult.Policy`へ追加します
raw reasonはpublic Eventへ入れず、Runtimeはこのmetadataをmodel向けTool Messageへコピーしません
これによりSession replayは別のmutable state storeを信頼せずPolicy Ledgerを再構築できます

Toolは`ToolResult.ContextOperation`へ`mutation`、`verification`、`commit`、`subagent`のkindも返せます
Protocol実装はTool result内で`{"context_operation":{"kind":"mutation"}}`としてencodeします
Runtimeはこのcontent-free metadataをEventとSession replayへ保持しますがmodel向けTool Messageへは入れません
context圧縮がsemantic transactionを分断しないための分類であり、callをauthorizeせずresult成功も証明しません
HostとExtension serverは未知のkindを拒否します

`ToolResult.ContextRetrieval`はRuntime所有のbuilt-in retrieval Tool向けに予約され、Extension Protocol v2には含まれません
third-party Extensionは通常outputと任意の`ContextOperation`を返してください
Runtimeは明示的に設定されたbuilt-in Toolだけにretrieval metadataを付与します

### Hook

Extensionはexact Agent Event type文字列と1つのhandlerを登録します
Runtimeはmatching Hookをregistration順に、Session persistenceとEvent publicationより前に同期実行します
Hook errorはcandidate Eventを拒否してRunをfailさせます
ToolとHookは完全なRunに対して同じgeneration setからatomicに取得されます

Hook handlerはcontext cancellationに従い、長時間または不可逆なside effectを避ける必要があります
Extension RPCとStore appendは単一transactionではないため、Hook成功後にSession Storeが失敗する可能性があります

active Runのsteeringは`user.message.added` Event typeを維持し、任意fieldの`user_message_origin`を`steering`に設定します
このtypeを購読するHookはRun inputとsteering Messageの両方を受け取るため、strict protocol decoderは任意fieldを含める必要があります
明示的なFact lifecycle transitionも同じEvent typeを使い、Runtimeがtarget Event prefixを検証した後に任意の`fact_directive` metadataを追加します
nested Messageにはhost専用directiveを残しません

Hookは`current_world_state.captured`を購読できます
payloadはfile、diff、stdout、stderr contentを含みませんが、workspace path、command argument、digest、provenanceを含みます
sensitiveなSession metadataとして扱う必要があります

### Command

Commandはname、description、JSON input schema、capabilityを宣言します
Hostが`extension.Command`として明示的に呼び出し、自動的にmodel-facing Toolにはなりません
`Manager.AcquireCommands`、`GenerationSet.AcquireCommands`、`coding.Profile.AcquireCommands`はRunと同じgeneration semanticsを固定します

Hostは`invoke_command`より前にCommand capabilityを評価します

## Go Server Adapter

`extension/server.Serve`はGo componentとlifecycle callbackを適合させます

```go
options := server.Options{
	ID:      "example-extension",
	Version: "0.1.0",
	InitializeComponents: func(
		ctx context.Context,
		request protocol.InitializeRequest,
	) (server.Components, error) {
		return server.Components{
			Tools:    tools,
			Hooks:    []string{string(agent.EventRunStarted)},
			Commands: commands,
			HandleEvent: func(
				ctx context.Context,
				request protocol.HandleEventRequest,
			) error {
				return handleEvent(ctx, request)
			},
		}, nil
	},
	Snapshot: snapshot,
	Restore:  restore,
}

if err := server.Serve(ctx, os.Stdin, os.Stdout, options); err != nil {
	log.Fatal(err)
}
```

Tool-only server向けに従来の`Initialize` callbackも利用できます
serverは`Initialize`または`InitializeComponents`のどちらか一方だけを指定します

Protocol stdoutにはframeだけを出力します
humanまたはsafe debug diagnosticsは`Options.DebugWriter`経由でstderrへ出力します

## Contract test kit

`extension/contracttest`はlauncherとProtocol behaviorを確認するreference Extensionと再利用可能なsuiteを提供します
process起動ごとに新しい`contracttest.ServerOptions()` fixtureをserveするcommandを渡します

同じsuiteが次を検証します

- Handshake、Initialize payload伝搬、Describe、HealthCheck
- Tool invocation、dynamic capability、Hook delivery、Command invocation
- Snapshot、Restore、Drain、Cancel、graceful Shutdown
- child process crash isolation

実際のExtension executableには`RunLifecycle`を使います
Go以外の言語で実装したexecutableも対象にでき、指定したdeclarationをHandshake、Initialize、Describe、HealthCheck、Snapshot、Restore、Drain、Shutdownまで検証します

```go
contracttest.RunLifecycle(t, contracttest.LifecycleOptions{
	Command:     command,
	Declaration: declaration,
	Initialize:  initializeRequest,
})
```

Component semanticsと意図的なcrash behaviorはExtensionごとに異なるため、完全な`Run` suiteは標準reference fixtureを使います

external executable testでは`TestMain`からfixtureをdispatchし、そのexecutableをsuiteへ渡せます

```go
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == contracttest.ExternalChildArgument {
		options := contracttest.ServerOptions()
		if err := server.Serve(context.Background(), os.Stdin, os.Stdout, options); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestExternalContract(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contracttest.Run(t, contracttest.SuiteOptions{
		Command: host.Command{
			Path: executable,
			Args: []string{contracttest.ExternalChildArgument},
		},
	})
}
```

self-execでは`contracttest.Declaration()`と`contracttest.ServerOptions`を`selfexec.Definition`へ登録します
そのCatalogを`TestMain`からdispatchし、`Definition.Command`で生成したcommandを同じ`contracttest.Run`へ渡します
両launcherの実装例は[package contract test](../extension/contracttest/contracttest_test.go)を参照してください

fixtureには意図的なprocess exit probeが含まれるため、専用のtest child processだけでserveします
suiteが検証するのは共通processとProtocol contractです
Extension固有の業務behavior、Policy decision、OS sandboxは個別にtestします

## External manifest

conventional filenameは`qed-extension.json`です

```json
{
  "id": "example-extension",
  "version": "0.1.0",
  "protocol_version": 2,
  "entrypoint": "bin/example-extension",
  "capabilities": ["filesystem.read"],
  "hooks": ["run.started"],
  "commands": [
    {
      "name": "inspect_state",
      "description": "Return public Extension state",
      "input_schema": {
        "type": "object",
        "properties": {},
        "additionalProperties": false
      },
      "capabilities": ["filesystem.read"]
    }
  ]
}
```

manifestは上限1 MiBのstrict JSONです
ID、version、exact protocol、local relative entrypoint、uniqueかつvalidなcapability名、Hook Event type、Command definitionを検証します
runtime loaderはmanifest directory内へ解決されるregularかつnon-symlinkのentrypointを要求します

`entrypoint`以外のfieldは`manifest.Declaration`を構成し、組み込みExtensionと共有するtransport-independent contractになります

Tool definitionは外部fileへ固定せずDescribeで返します
Hostは外部identity、version、capability、Hook、Commandをlive processと比較してからpublishします

`manifest.Discover`は最大1024 manifestを再帰検索し、directory symlinkを無視し、ID順にsortし、重複IDを拒否します

## 組み込みself-exec catalog

`extensions.lock`は1つのHost binaryへlinkするGo Extensionを選択します
上限1 MiBのstrict JSONで、最大1024の一意なExtension IDと外部manifestと同じdeclaration validationを持ちます

```json
{
  "version": 1,
  "extensions": [
    {
      "go_package": "example.com/qed-extension/example",
      "factory": "ServerOptions",
      "manifest": {
        "id": "example-extension",
        "version": "0.1.0",
        "protocol_version": 2,
        "capabilities": ["example.read"]
      }
    }
  ]
}
```

`go_package`はHostのGo moduleですでに利用可能なpackageを指定します
`factory`の既定値は`ServerOptions`で、`func() server.Options` signatureを持つexported functionを指定します
Go dependencyのversionとintegrityは`go.mod`と`go.sum`を正とし、catalog生成はdependencyを追加または更新しません

lock変更後にchecked-in catalogを生成し、CIでは書き込まず鮮度を確認できます

```sh
qed extension generate
qed extension generate --check
```

外部Hostはstandalone generatorを使い、出力packageを自分のrepositoryで所有します

```sh
go run github.com/qed-runtime/qed/cmd/qed-extension-gen \
  --lock extensions.lock \
  --output extensionregistry/registry_gen.go \
  --package extensionregistry \
  --variable Catalog
```

生成sourceは公開`*selfexec.Catalog`を構築し、QEDのinternal packageへ依存しません
application本来のargumentを解析する前にchild modeをdispatchします

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

同じCatalogと現在のabsolute executableを`qed.LoadHost`へ渡します
外部repository全体の流れは[QEDの組み込み](embedding_ja.md)を参照してください

生成catalogは外部manifest検証と同じexpected identity、version、capability、Hook、Commandを提供します
startupではlink済みServer optionsがlockのidentityとversionに一致し、HandshakeとDescribeがlock済みdeclaration全体に一致した後だけgenerationをpublishします

lockはExtensionがfirst-partyかthird-partyかを区別しません
compatibleなGo packageはself-execへ選択でき、任意の言語によるExtensionは外部executableとmanifestを引き続き利用できます
runtime上の差はlauncherだけです
`extensions.lock`は再現可能なbuild inputであり、code signingまたはtrust policyではありません

QED repositoryは現在次の3つの再利用可能なExtensionを選択しています

| Extension | Component |
| --- | --- |
| `qed.workspace` | `search_text`、`read_file`、`apply_patch` |
| `qed.process` | `run_command` |
| `qed.git` | `git_status`、`git_diff` |

Coding Profileは3つを合成し、別Host repositoryは自身のlockで必要なpackageだけを選択できます
self-execでも各Extensionは独立したprocess、identity、generation、reload、state namespaceを持ちます

test実行は専用Test Extensionではなく汎用`qed.process` command Toolの用途として扱います
permission判断はPermission ExtensionではなくHost Policyとoptional Approverに維持します
このownershipはfirst-partyとthird-partyのExtensionへ同じように適用します

## Host enforcementとlifecycle

initial startup

```text
Start process
  -> Handshake
  -> Initialize
  -> Describe and validate components
  -> validate the locked or external manifest declaration when configured
  -> HealthCheck
  -> publish generation
```

`host.Process`は1つのprocessを表します
`host.Manager`はauthorization、Evidence、state、reloadを追加します
`host.GenerationSet`は複数Managerからcomponentをatomicに取得するため、Runはpartial Extension setを観測しません

reload

```text
start and validate candidate
  -> Snapshot active generation
  -> persist Snapshot in the host State Store when configured
  -> Restore candidate
  -> HealthCheck candidate
  -> atomically publish candidate for new Runs
  -> wait for old leases
  -> Drain and Shutdown old process
```

publication前のfailureはcandidateを閉じてactive generationを維持します
process crashはHostを終了させずpending RPC requestをfailさせます

直接構築するManagerではautomatic restartはopt-inです

```go
manager, err := host.NewManager(ctx, host.ManagerOptions{
    Process:       processOptions,
    Policy:        policy,
    StateStore:    stateStore,
    RestartPolicy: host.DefaultRestartPolicy(),
})
```

Coding Profileとdevelopment hostはpolicy pointerがnilの場合に`DefaultRestartPolicy`を使います
既定値はreplacement candidate 3回、initial backoff 100ms、exponential backoff上限2秒、generationが30秒生存した後の連続count resetです
直接構築するManagerではzero-value policyがautomatic restartを無効にします
Coding Profileまたはdevelopment hostで無効にする場合はzero-value `host.RestartPolicy`へのpointerを設定します

unexpected exitは次の順序で処理します

```text
fail RPC requests pinned to the crashed generation
  -> remove that generation from new lease selection
  -> return ErrExtensionRestarting to new acquisition
  -> wait for bounded backoff
  -> start with the last successfully published ProcessOptions
  -> revalidate identity and the locked manifest when configured
  -> load and Restore the latest host-owned Snapshot when configured
  -> HealthCheck and, when a State Store is configured, Snapshot and persist
  -> publish the next generation for new Runs
```

既存Run leaseは別generationへ移動せず、QEDは中断したTool callを再実行しません
Toolの副作用がcrash前に発生している可能性があるためです
失敗candidateはgeneration番号を消費しません
candidate startup failureとstability window前に終了したreplacement generationは同じattempt上限を消費します
上限到達後の新規acquisitionは`ErrExtensionCircuitOpen`を返します
明示的な`Manager.Reload`がcandidate検証に成功するとcircuitを閉じます

`Manager.RestartStatus`は`disabled`、`ready`、`restarting`、`circuit_open`、`closed`、連続attempt数、current generation、payloadを含まないlast error typeを返します
verbose lifecycle logもRPC payloadを含めず同じ安全なIDとcounterを記録します

## Host所有Extension state

`extension.StateStore`はopaque valueをExtension ID、host scope、keyで保存します
QEDはconcurrent memory storeと、1 MiB value上限を持つprivateかつatomicなJSON storeを提供します

Managerはinitial startupで`snapshot` keyをrestoreし、reload時に更新し、orderly closeでcurrent processをpersistします
automatic restart有効時はinitial startupと各replacementもpublication前にbaseline Snapshotをpersistします
crash後にrestoreできるのは最新のhost所有Snapshotであり、それより新しいprocess stateは失われる場合があります
declarative Coding ProfileはworkspaceとProfile IDのdigestでscopeを分け、無関係なProfile間の意図しないstate共有を防ぎます

SnapshotとRestoreは必要なprocess-local stateに利用します
永続Agent conversationはSession Storeへ、execution proofはEvidence Storeへ置きます

## Development reload

Extension source directoryまたはmanifest pathから開始します

```sh
qed extension dev ./extensions/example-extension
```

development loaderは最初のbuild前にmanifest entrypointのbuild outputが存在することを要求しません
既定の直接buildは次です

```text
go build -o {output} .
```

`--build-program`でexecutableを上書きし、`--build-arg`を繰り返してargumentを渡します
少なくとも1つのargumentに`{output}`が必要です
programとargumentをshellで解釈しません
build output上限は1 MiBです

watcherはregular fileのsize、modification time、modeをpollingし、`.git`、`.qed`、symlinkを無視し、1 snapshotを10,000 fileに制限してchangeをdebounceします
candidateごとに異なる一時executableを使います
build、startup、manifest比較、Restore、HealthCheckが失敗した場合は報告し、old generationをactiveに保ちます

別processからinspectまたは強制reloadできます

```sh
qed extension inspect example-extension
qed extension reload example-extension
```

`inspect`はmanifest pathまたはdirectoryも受け取り、resolved JSONを出力します
CLI control directoryの既定値はcurrent directory配下の`.qed/extension-dev`で、すべてのcommandに同じ`--control-dir`を指定して変更できます

control serverはloopbackだけをlistenし、`0700` directory配下へ`0600` descriptorを書き、random tokenですべてのrequestを認証し、同じIDの2つ目のactive serverを拒否し、close時に自身と一致するdescriptorだけを削除します

## Cancellationとverbose diagnostics

Tool、Hook、Command contextのcancelは独立した`cancel` requestを送り、original RPCがin-flightでもserver側workをcancelできます
Drainはaccepted workを待ち、新規invocationを拒否します

root `--verbose`は`InitializeRequest.Verbose`へ伝搬します
ServerはInitialize後にsafeなstructured stderr diagnosticsを有効にします
Hostはpayload valueを出力せず、operation名、ID、generation、件数、duration、error typeをlogします
arbitrary child stderrはbounded process diagnosticsとしてだけ保持され、safe verbose outputへ転送されません

## Security boundary

Host側authorizationはPolicy approvalなしにprotocol AdapterがToolまたはCommandを呼び出すことを防ぎます
ただしmalicious executableはprocess権限でRPC外の操作を実行できるため隔離できません
信頼できるExtensionだけを設定し、hostile codeを扱う場合はcontainer、OS sandbox、別accountを追加してください
