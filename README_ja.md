# QED Runtime

QED RuntimeはGoで実装された組み込み可能なエージェントランタイムです

> Proof by execution

このプロジェクトは初期プロトタイプですが、主要なアーキテクチャ境界は現在の実装で動作します

- 非同期Runと順序付きストリーミングEvent
- OpenAI Responses、OpenAI Chat Completions、Anthropic Messages、ChatGPT認証のCodex Responses Provider
- 1つのAgent graphで利用できる複数Provider profile
- collect、select、consensusに対応する並行サブエージェント
- 注入可能なProfile reductionを持つcontent-addressedなsubagent Result Packet
- profile共有のProvider concurrency上限、cooldown、bounded retry
- active Runへのsteering、永続Session、terminal後のfollow-up、永続的な承認resume
- process分離Extension内のcapability制御されたCoding Tool
- 複数のRun固定Extension generation、Hook、Command、host所有state
- manifest discovery、開発時のatomic reload、bounded crash restart
- lifecycle contract test付きで既存fileを上書きしないGo Extension scaffold
- fork不要でapplicationが所有する`extensions.lock` catalogとlive manifest validation
- host所有Evidence Bundle
- ordered Session Eventから明示的なFact lifecycleを含めて再構築するdeterministicなArtifact、Execution、Constraint、Policy、Task Ledger
- relevant file hash、Git state、観測済みcheck freshnessを持つcanonical Current World State snapshot
- approval、subagent、edit-verification、commit、Tool transactionを分断しないsafe cut、決定的なpreservation report、Provider call前rollbackを持つEvidence preservingなContext圧縮
- populatedなlevelだけをmodel viewへ選択するSession Synopsis、Task、Episodeの階層Checkpoint
- 本文を含まないContext inspect、explain、diff、品質metrics集計
- opt-inでboundedかつ説明可能なrelevance rankingを持つContext search、scope付きEvidence fetch、Session timeline、Ledger history Tool
- hostまたはProvider注入のToken Estimator、決定的なbyte fallback、本文を含まないProvider Usage比較
- output、safety、予測Tool outputを予約し、softで検証済みcandidateを準備してhardで採用するmodel contextのpredictive budget
- Prefix Manifest、prompt cache Plan、正規化cache Usage
- NagiベースのCLIとmulti-turn TUI
- 末端のExtension processまで伝搬する安全な構造化diagnostics

## 要件

- Go 1.25以降
- 実験的TUIにはLinuxまたはmacOS

Provider非依存のRuntimeはGo標準ライブラリを使用します
NagiはCLIとTUI Adapter内に限定され、その型はRuntime API境界を越えません
開発ツールは独立した`tools` moduleに固定されています

## Smoke test

```sh
go run ./cmd/qed run "hello"
```

期待する出力

```text
hello
```

すべてのRun EventをJSON Linesで確認できます

```sh
go run ./cmd/qed exec --json "hello"
```

`qed`は入力待ちのTUIを開き、`qed "prompt"`は同じTUIを開いて最初のRunをすぐ開始します
`exec`は非対話`run` commandのaliasです
共通short optionは`--model`の`-m`、`--cd`の`-C`、`--verbose`の`-v`です
`--workspace`と`--cd`は同じ設定Profile workspaceを選択し、同時には指定できません

echo Providerは`run.started`、userとmodelのEvent、text delta、`run.completed`を含む完全なlifecycleを出力します

`model.request.started`は本文を含まないPrefix Manifest、Cache Plan、設定済みPredictive Budget decisionを出力します
解決済みToken Estimateのkindとcountも含み、Provider Usageを正として`qed cache status`で最新estimateとの差を確認できます

QED側のcache制御は設定するまで無効ですが、Provider側のimplicit behaviorは適用される場合があります
prompt cache usageを返すProviderではcache read、write、uncached inputの正規化された内訳も記録します
設定済みJSON Evidence Storeはcompactされたcontextの正確なobjectも保持します

```sh
qed cache status --store .qed/evidence
qed context inspect <run-id> --store .qed/evidence
qed evidence fetch sha256:<digest> --run-id <run-id> --store .qed/evidence
```

設定済みContext Evidenceはtenant、Sessionまたはephemeral Run、execution Profile、required Capabilityへbindingされます
content digest自体はaccess tokenではありません
[Context compilation、圧縮、prompt cache](docs/context-caching_ja.md)を参照してください

root flagでstderrへの安全な構造化diagnosticsを有効にできます

```sh
go run ./cmd/qed run -v "hello"
```

verbose modeはRuntime、Extension Host、Initialize RPC、Extension Serverへ伝搬します
QEDのdiagnosticsは識別子、操作名、件数、処理時間、error typeを含みます
prompt、message本文、Tool引数と出力、metadata value、credential、environment value、Extension configuration valueは意図的に含みません

## Model APIへの接続

QEDはmodel SDKへ依存せず、4つのストリーミングHTTP API dialectを実装しています

- `openai-responses`
- `openai-chat`
- `openai-codex`
- `anthropic`

model IDは常に明示します

```sh
export OPENAI_API_KEY="<token>"
go run ./cmd/qed run \
  --provider openai-responses \
  --model "<model-id>" \
  "Reply with a short greeting"
```

```sh
export ANTHROPIC_API_KEY="<token>"
go run ./cmd/qed run \
  --provider anthropic \
  --model "<model-id>" \
  --max-output-tokens 1024 \
  "Reply with a short greeting"
```

信頼できるOpenAI互換endpointでは、operation URLではなくAPI base URLを渡します

```sh
go run ./cmd/qed run \
  --provider openai-chat \
  --base-url "http://127.0.0.1:8080/v1" \
  --model "local-model" \
  "hello"
```

custom base URLへ既定のOpenAIまたはAnthropic credentialを送りません
信頼でき、認証が必要なcustom endpointに限り`QED_API_KEY`を設定します

Provider adapterの実装者は実APIを呼ばずに[`provider/contracttest`](docs/providers_ja.md)の再利用可能な決定的suiteを適用できます

### ChatGPT subscriptionの利用

`openai-codex`はOpenAI API keyではなく、名前付きChatGPT OAuth profileを利用します

```sh
go run ./cmd/qed auth login --auth-profile personal
go run ./cmd/qed run \
  --provider openai-codex \
  --auth-profile personal \
  --model "<codex-model-id>" \
  "Reply with a short greeting"
```

headless環境では`qed auth login --device-code`を利用します
`qed auth status`はcredentialを表示せずprofile metadataだけを表示し、`qed auth logout`はprofileを削除します
access tokenとrefresh tokenはproject設定とは別にOS user configuration directoryへ保存されます

ChatGPT subscriptionとOpenAI API課金は別の認証経路です
Providerは固定のChatGPT Codex backendを使い、`--base-url`と`--max-output-tokens`を受け取りません
securityと現在のprotocol制限は[ChatGPT subscription認証](docs/chatgpt-auth_ja.md)を参照してください

## 複数ProviderとAgentの設定

Provider endpoint、protocol、model、credential参照を1つのprofileとして対応付けます
各AgentはProviderを独立に選択できるため、OpenAI互換のcoordinatorからAnthropicのsubagentを利用できます

```json
{
  "version": 1,
  "default_agent": "coordinator",
  "providers": {
    "primary": {
      "protocol": "openai-responses",
      "model": "<openai-model-id>",
      "token_env": "PRIMARY_API_TOKEN"
    },
    "review": {
      "protocol": "anthropic",
      "model": "<anthropic-model-id>",
      "token_env": "REVIEW_API_TOKEN"
    }
  },
  "agents": {
    "reviewer": {
      "provider": "review",
      "instructions": "Review the delegated task independently"
    },
    "coordinator": {
      "provider": "primary",
      "delegations": [
        {
          "name": "consult_reviewer",
          "strategy": "delegate",
          "agents": ["reviewer"]
        }
      ]
    }
  }
}
```

```sh
export PRIMARY_API_TOKEN="<token>"
export REVIEW_API_TOKEN="<token>"
go run ./cmd/qed run --config ./qed.json "Review this plan"
```

`--agent <id>`で`default_agent`を1回の実行だけ上書きできます
設定にはtoken valueではなくcredential environment名またはauth profile名を記載します
完全なschemaは[Agent設定](docs/configuration_ja.md)を参照してください
同じProvider profileを参照するAgentは既定のoutbound 4 stream上限と観測したrate limit cooldownを共有します
必要な場合はそのprofileの`rate_limit.max_concurrency`を設定します

## Coding Profileの実行

標準Coding Profileは6つのmodel-facing Toolを公開します

- `search_text`
- `read_file`
- `apply_patch`
- `run_command`
- `git_status`
- `git_diff`

`git_diff`の`worktree`と`base`はstandard Git ignore ruleで除外されないuntracked regular fileを同じbounded patchへ追加します
`staged`はindexだけを対象にします

checked-in `extensions.lock`はこのbinary向けに再利用可能な`qed.workspace`、`qed.process`、`qed.git` Extensionを選択します
各Extensionはsingle binaryのself-exec modeを含め、常にExtension Protocol v1境界で実行されます

```json
{
  "version": 1,
  "default_agent": "coding",
  "providers": {
    "model": {
      "protocol": "openai-responses",
      "model": "<model-id>",
      "token_env": "MODEL_API_TOKEN"
    }
  },
  "extensions": {
    "qed.workspace": {"mode": "self-exec"},
    "qed.process": {"mode": "self-exec"},
    "qed.git": {"mode": "self-exec"}
  },
  "profiles": {
    "workspace": {
      "kind": "coding",
      "extensions": ["qed.workspace", "qed.process", "qed.git"],
      "capabilities": {
        "allow": [
          "filesystem.read",
          "filesystem.write",
          "process.execute",
          "git.read"
        ],
        "deny": ["filesystem.delete"]
      },
      "environment": ["PATH", "HOME"]
    }
  },
  "agents": {
    "coding": {"provider": "model", "profile": "workspace"}
  },
  "session": {"store": "jsonl", "path": ".qed/sessions"},
  "evidence": {"store": "json", "path": ".qed/evidence"},
  "extension_state": {"store": "json", "path": ".qed/extension-state"}
}
```

```sh
export MODEL_API_TOKEN="<token>"
go run ./cmd/qed run \
  --config ./qed.json \
  --cd . \
  --session-id work-1 \
  "Find the failing test, fix it, and run the relevant checks"
```

Profileはworkspace相対pathを受け取り、編集時にdigestまたはabsence preconditionを必須とし、capability decisionとTool digestをhost所有Evidenceへ記録します
`run_command`はargvを直接実行して上限を適用しますが、OS sandboxではありません

詳細な境界は[Coding Profile](docs/coding-profile_ja.md)と[Extension process](docs/extensions_ja.md)を参照してください

## steering、follow-up、承認resume、Evidence

RuntimeとHost APIは後から与える入力を3種類に分けます

- `RunHandle.Steer`はactive Runの次の安全なProvider境界へ1つのuser Messageをqueueする
- follow-upは前のhandleがterminal resultへ達した後、設定済みSession Storeと同じSession IDで新しいRunを開始する
- `RunHandle.Resume`はapprovalなど明示的な`run.waiting` requestへ応答する

steeringは上限付きのnon-blocking queue操作です
in-flight Provider request、retry、Tool batchは中断しません
`UserMessageOrigin`が`steering`の`user.message.added` Eventが、MessageをSession stateへ反映した確定点です
cancel、Deadline切れ、terminal Run failureでは、このEventへ到達していないsteeringを破棄する場合があります
follow-upは新しいRun IDとRun local上限を持ち、Session Storeがない場合はcallerが過去contextを渡す必要があります
複数Runで上限を共有する場合だけ同じ`*agent.Budget`を明示的に再利用します

hostはuser Messageへ`FactLifecycleDirective`を付け、過去のactive Constraint Fact IDをsupersedeまたはresolveできます
Runtimeはdirectiveをcommit対象の`user.message.added` Eventへ移し、Provider conversation historyへ含めず、自然言語を解釈せずにactive、superseded、resolved stateを決定的に再構築します
lifecycle契約は[Contextとcache](docs/context-caching_ja.md)を参照してください

実験的TUIではEnterをactive Runへのsteeringへ割り当て、現在のRunがterminal resultへ達した後はfollow-up Runを開始します
設定済み`--session-id`がある場合は永続Sessionをreplayし、ない場合はそのchatが続く間だけ前Runのmessageを引き継ぎます

Profileの`ask` listへcapabilityを置き、対話承認を有効にできます

```sh
go run ./cmd/qed run \
  --config ./qed.json \
  --approval prompt \
  --session-id work-1 \
  "Run the relevant checks"
```

承認は`run.waiting`と`run.resumed` Eventを生成します
JSONL Sessionの待機中にprocessが終了した場合、直前のProvider requestを繰り返さず、保留中のTool callだけをresumeできます

```sh
go run ./cmd/qed session resume work-1 --config ./qed.json
```

Evidence Storeが設定されている場合、設定Run、設定TUI chat内で完了した各Run、resume Runは個別のEvidence Bundleを保存します

```sh
go run ./cmd/qed evidence inspect <run-id> --store .qed/evidence
go run ./cmd/qed evidence export <run-id> --store .qed/evidence
go run ./cmd/qed context inspect <run-id> --store .qed/evidence
go run ./cmd/qed context explain <run-id> --store .qed/evidence
```

`qed context diff --before RUN_ID[@EVENT_SEQUENCE] --after RUN_ID[@EVENT_SEQUENCE]`はmessage、path、command、Evidence object contentを出力せず2つのcompaction判断を比較します
`context inspect`と`context explain`では各compiled model viewへ入った階層Checkpoint levelも確認できます

## 実験的TUI

```sh
go run ./cmd/qed
go run ./cmd/qed "hello"
```

明示的な`qed tui [PROMPT]`形式も利用できます
TUIは`--config`、`--agent`、`--workspace`または`--cd`、`--session-id`にも対応し、同じ設定Agent graphを利用します
promptなしではRunを開始せず入力を待ちます
messageを入力してEnterを押すと最初のRunを開始し、active Runへsteeringし、完了後はfollow-upを開始します
Runが承認待ちになった場合は`Y`で許可、`N`で拒否します
Ctrl-Cは現在のRunだけをcancelしてchatを維持し、Escapeはactive RunをcancelしてTUIを終了します

TUIは最近のuserとassistant messageを保持し、assistant textをstream表示し、Agent、Session、Run、Tool、approval Capabilityの本文なしactivityを表示します
Tool引数、Tool出力、raw wait payload、raw Run errorはrendering用の表示状態へ保持しません

複数行ComposerはEnterで送信し、Shift-Enter、Alt-Enter、Ctrl-Oで改行します
caretがeditor境界にある場合はUpとDownでboundedな送信履歴を呼び出します
transcriptは可変高VirtualFeedを使い、userとassistant entryを最大2048件保持し、可視範囲とoverscanだけを構築します

F2で本文を含まないContext、predictive budget、cache、scope付きEvidenceの利用可能状態を切り替えます
TUIはEvidence本文を直接読み取らず、設定済みCapabilityとaccess audit境界を維持するためAgentへ`context_search`または`context_fetch`の利用を依頼します
標準Session Storeを設定した場合はF6で1つ古いrecent Session、Shift-F6で1つ新しいSessionへ移動し、F7でcurrent chatへ戻ります
過去Sessionはread-onlyで表示し、その間もactiveなcurrent Runは継続します
既存SessionでTUIを開始すると新しいRunの開始前にbounded transcript、Composer履歴、Context statusをseedします

## 外部Extensionの開発

既存Go module内へGo reference scaffoldを作成できます
parent directoryは既に存在し、destinationは新規である必要があります

```sh
mkdir -p ./extensions
go run ./cmd/qed extension scaffold \
  ./extensions/my-extension \
  --id example.my-extension
```

commandはexternal manifestとexecutable、self-exec向けにimport可能な`ServerOptions` implementation、process-level lifecycle contract testを生成します
`go.mod`、`go.sum`、`extensions.lock`は変更せず、空directoryを含む既存destinationへの上書きを拒否します

外部Extension directoryには`qed-extension.json`を配置します
sourceから直接development hostを開始できます

```sh
go run ./cmd/qed extension dev ./extensions/my-extension
```

既定build commandは`go build -o {output} .`です
QEDはsource metadataを監視し、candidateごとに異なる一時実行ファイルをbuildし、検証とrestoreの後に新規Runを新generationへatomicに切り替えます
buildまたはcandidateが失敗した場合はactive generationを維持します

別processから操作できます

```sh
go run ./cmd/qed extension inspect my-extension
go run ./cmd/qed extension reload my-extension
```

local control endpointはprivate descriptor、random bearer token、loopback TCPを使用します
manifestとcustom buildの詳細は[Extension process](docs/extensions_ja.md)を参照してください

## self-exec Extension catalogのbuild

`extensions.lock`はstatic linkするGo Extension packageを選択し、各child processが一致すべきmanifest declarationを記録します
変更後はchecked-in catalogを生成し、CIでは`--check`で確認します

```sh
go run ./cmd/qed extension generate
go run ./cmd/qed extension generate --check
```

別Host repositoryではdependency-lightなgeneratorを使い、生成packageとexported Catalog変数を自分で選択します

```sh
go run github.com/qed-runtime/qed/cmd/qed-extension-gen \
  --lock extensions.lock \
  --output extensionregistry/registry_gen.go \
  --package extensionregistry \
  --variable Catalog
```

生成sourceは公開QED packageとapplication側lockに記載したExtension packageだけへ依存します
QEDのforkやinternal registry glueの複製は不要です

lockはfirst-partyとthird-party packageを区別しません
dependencyのversionとchecksumは`go.mod`と`go.sum`を正とし、生成処理は変更しません
Go以外のExtensionは外部executableから同じprotocolを引き続き利用できます
lock schemaと検証順序は[Extension process](docs/extensions_ja.md)を参照してください

## Runtimeの組み込み

既存serverが宣言的Agent graphを読み込み、Provider、Profile、Store、Evidence、Extension lifecycleを所有する場合はrootの`qed.Host` APIを使います

```go
host, err := qed.LoadHost("qed.json", qed.HostLoadOptions{
	LookupEnv:       os.LookupEnv,
	WorkspaceRoot:   workspaceRoot,
	SelfExecutable:  executable,
	SelfExecCatalog: extensionregistry.Catalog,
})
if err != nil {
	log.Fatal(err)
}
defer host.Close()

outcome, err := host.Run(ctx, agent.RunRequest{
	Input: []agent.Message{{Role: agent.RoleUser, Text: prompt}},
}, nil)
```

`Host`はtransport-neutralで、複数Runから並行利用できます
HTTPまたはgRPC schema、authentication、authorization、inbound clientまたはtenant rate limit、shutdown順序は組み込み先applicationが引き続き所有します
[QEDの組み込み](docs/embedding_ja.md)と[標準library server example](examples/embedded-server/README.md)を参照してください

より小さいprogrammatic integrationでは`agent.Runtime`を直接利用します

```go
package main

import (
	"context"
	"log"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/provider/echo"
)

func main() {
	runtime, err := agent.NewRuntime(agent.Options{Provider: echo.New()})
	if err != nil {
		log.Fatal(err)
	}

	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID: "example",
		Input: []agent.Message{
			{Role: agent.RoleUser, Text: "hello"},
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	for event := range handle.Events() {
		log.Printf("event: %s", event.Type)
	}
	result, err := handle.Wait()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("status: %s", result.Status)
}
```

`agent.ComponentSource`を使うと、ToolsとHooksをRun単位でatomicに固定できます
標準Session実装は`session` packageで提供され、orchestrationはRuntime Coreより上位の独立packageです

### 複数Providerの合成

各Runtimeは1つのProviderに固定されます
`orchestration.AgentRegistry`はprovider-privateなcontinuation stateを変換せず、名前付きRuntimeを合成します

```go
registry, err := orchestration.NewAgentRegistry(orchestration.AgentRegistryOptions{
	Agents: []orchestration.AgentDefinition{
		{
			ID:           "anthropic-specialist",
			Runtime:      anthropicRuntime,
			Instructions: "Review the delegated task",
		},
	},
})
if err != nil {
	log.Fatal(err)
}

delegateTool, err := orchestration.NewSubagentTool(orchestration.SubagentToolOptions{
	Name:     "consult_specialist",
	Registry: registry,
	Strategy: orchestration.TeamStrategyDelegate,
	AgentIDs: []string{"anthropic-specialist"},
})
if err != nil {
	log.Fatal(err)
}

openAIRuntime, err := agent.NewRuntime(agent.Options{
	Provider: openAIProvider,
	Tools:    []agent.Tool{delegateTool},
})
if err != nil {
	log.Fatal(err)
}
if err := registry.Register(orchestration.AgentDefinition{
	ID: "openai-coordinator", Runtime: openAIRuntime,
}); err != nil {
	log.Fatal(err)
}
```

`delegate`は1つのcandidateを実行し、`collect`は全outcomeを返し、`select`はjudgeにcandidateを選択させ、`consensus`はjudgeに結果を統合させます
candidateは並行実行され、異なるProvider protocolを利用できます
共有既定上限はAgent Run 16、depth 4、Provider call 64です
設定済みProvider profileごとにactive Provider stream 4つの既定上限と、観測したrate limit cooldownも共有します

成功した各candidateはterminal Context Ledgerへ結び付いたversion付き`ResultPacket`も返します
既定の`LedgerResultReducer`はsemantic factを推測せず、typedなArtifactとExecutionを返します
`AgentDefinition.ResultReducer`はRuntime Coreへdomain fieldを追加せず、source付きFactとboundedなProfile所有JSONを追加できます
宣言的Coding Profileはreducerを自動設定します
無効または検証不能なreductionは、別のcandidateを利用できる場合はそのcandidateだけをfailureにします

Result PacketのFactとProfile stateはparent modelへ提示されるuntrusted contentです
Evidence referenceは元のauthorization scopeを維持し、それ自体はparentへaccessを付与しません

## 主なimport path

- `github.com/qed-runtime/qed`
- `github.com/qed-runtime/qed/agent`
- `github.com/qed-runtime/qed/orchestration`
- `github.com/qed-runtime/qed/session`
- `github.com/qed-runtime/qed/provider/openai`
- `github.com/qed-runtime/qed/provider/openaicodex`
- `github.com/qed-runtime/qed/provider/anthropic`
- `github.com/qed-runtime/qed/capability`
- `github.com/qed-runtime/qed/evidence`
- `github.com/qed-runtime/qed/extension`
- `github.com/qed-runtime/qed/extension/host`
- `github.com/qed-runtime/qed/extension/manifest`
- `github.com/qed-runtime/qed/extension/protocol`
- `github.com/qed-runtime/qed/extension/reload`
- `github.com/qed-runtime/qed/extension/server`
- `github.com/qed-runtime/qed/extension/selfexec`
- `github.com/qed-runtime/qed/profile/coding`

## 開発

```sh
go -C tools tool goimports -w ..
go run ./cmd/qed extension generate --check
go test ./...
go vet ./...
go build ./...
```

## 現在の制限

- Extension automatic restartは中断したTool callを再実行せず、最新のhost所有Snapshotだけを
  restoreします
- `run_command`とExtension child processはhost account権限で動作し、OS sandboxではありません
- Tool Trace recordはhashを使いますが、Bundleのpublic Eventはprompt、message、Tool引数、Tool output、errorを含む場合があるためEvidence Storeを機密データとして保護する必要があります
- Evidenceは完全なworkspace archiveではありません
- Tool inputは上限付きJSON Schema subsetとstrictな具象argument decoderで検証しますが、完全なJSON Schema vocabularyは実装せず、別validatorは組み込み側から注入します
- 共有tokenとcost上限はProviderがusageを遅れて返す場合や返さない場合に完全には強制できません
- TUIはtranscriptを最大2048件、activityを256件、Composer履歴を128件、recent Session summaryを64件保持し、それ以前の内容は設定済みSession Storeへ残してview memoryへ全件保持しません
- built-in HTTP service、GitHub Actions Adapter、SQLite Session Store、WebAssembly backendは未実装ですが、既存serverは`qed.Host`を組み込めます
- すべてのthird-party OpenAI互換APIとの互換性は保証しません
- `openai-codex`はexperimentalなChatGPT backend contractに追従し、現在はmodel discovery、Responses Lite、WebSocket transportを持たないfull ResponsesのSSE経路だけを利用します

## License

[MIT](LICENSE)
