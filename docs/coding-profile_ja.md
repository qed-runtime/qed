# Coding Profile

標準Coding ProfileはProvider非依存Agent Runtimeの周囲へworkspace-awareなcoding behaviorを追加します
bounded project context、1つのcapability Policy、host所有Evidence、1つ以上のprocess分離Extensionを組み立て、`agent`へcoding behaviorを追加しません

## 標準model-facing Tool

3つの再利用可能な公式Extensionが6つのToolを提供します

| Tool | Extension | Capability | Behavior |
| --- | --- | --- | --- |
| `search_text` | `qed.workspace` | `filesystem.read` | bounded UTF-8 fileをliteralまたはregular expressionで検索 |
| `read_file` | `qed.workspace` | `filesystem.read` | bounded line rangeを読みfull-file SHA-256 digestを返却 |
| `apply_patch` | `qed.workspace` | `filesystem.write` | すべてのpath preconditionが一致する場合にunified diffを適用 |
| `run_command` | `qed.process` | `process.execute` | shell評価なしで1つのexecutableを直接実行 |
| `git_status` | `qed.git` | `git.read` | bounded porcelain v2 statusを返却 |
| `git_diff` | `qed.git` | `git.read` | boundedなworktree、staged、base-relative patchを返却し、worktreeとbaseではboundedなuntracked contentも追加 |

`apply_patch`での削除には`filesystem.delete`も必要です
公式Tool argumentはunknown field、duplicate key、trailing value、invalid type、Tool固有limit violationを拒否するstrictかつboundedなdecoderを使います

`apply_patch`は`--- a/path`と`+++ b/path`、および同じ意味のworkspace-relativeな`--- path`と`+++ path` headerを受理します
model-facing Tool schemaとCoding Profile instructionはprefix付きcanonical形式を推奨し、`*** Begin Patch` markerではなく`@@` unified diff hunkを要求し、`read_file`が返した完全な`sha256:...` valueを変更せず使うようmodelへ伝えます

test実行に専用Test Extensionは必要ありません
Profileは`qed.process`の汎用`run_command` Toolを公開し、Host Policyが用途に応じて許可するexecutable、argument、directory、capabilityを制限します
permission判断はmodel-facing Extension componentではなく、dispatch前にHost Policyとoptional Approverが所有するため、Extensionが自身のcallをauthorizeすることはありません

QEDの`extensions.lock`はこの3つをself-exec向けに選択します
同じProfileへ接続した追加の組み込みまたは外部ExtensionはToolとHookを提供できます
Runはすべての設定Extensionを1つのgeneration setとして取得します

## Declarative configuration

ExtensionとProfileをProvider profileから独立して定義します

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
    "coding": {
      "provider": "model",
      "profile": "workspace",
      "instructions": "Make the smallest verified change"
    }
  },
  "session": {"store": "jsonl", "path": ".qed/sessions"},
  "evidence": {"store": "json", "path": ".qed/evidence"}
}
```

```sh
export MODEL_API_TOKEN="<token>"
go run ./cmd/qed run \
  --config ./qed.json \
  --workspace . \
  --session-id coding-1 \
  --prompt "Fix the failing test and run the relevant checks"
```

workspaceはJSON file内のmachine固有値ではなくAdapter inputです
既定値はcurrent directoryです
設定したenvironment名はgraph load時にすべて存在する必要があります
valueは選択Toolへ渡されますがmodel messageやverbose diagnosticsへ追加されません
credential変数を選択しないでください

capability outcome

- `allow`は追加decisionなしで実行
- `ask`はhost-supplied Approverを呼び、Runをsuspend可能
- `deny`とすべてのunspecified capabilityはinvocationを拒否

non-interactive設定の既定値は`--approval deny`です
serialized terminal approvalには`--approval prompt`を使います
TUIはwaitを表示して`Y`または`N`を受け取ります
両方の経路が`RunHandle.Resume`で継続し、`run.waiting`と`run.resumed` Eventを生成します

## Go API

宣言的なserver組み込みでは[QEDの組み込み](embedding_ja.md)に従い、application所有self-exec Catalogと`qed.LoadHost`を使うことを推奨します

より低水準のembedding hostは明示的なExtension commandを渡し、そのlifetimeを直接所有できます

```go
policy, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{
	Allow: []capability.Name{
		capability.FilesystemRead,
		capability.FilesystemWrite,
		capability.ProcessExecute,
		capability.GitRead,
	},
	Deny: []capability.Name{capability.FilesystemDelete},
})
if err != nil {
	log.Fatal(err)
}

codingProfile, err := coding.New(ctx, coding.Options{
	Root: workspaceRoot,
	Extensions: []coding.ExtensionOptions{
		{
			ID: workspaceextension.ID,
			Command: host.Command{
				Path: qedExecutable,
				Args: []string{selfexec.ChildArgument, workspaceextension.ID},
			},
		},
		{
			ID: processextension.ID,
			Command: host.Command{
				Path: qedExecutable,
				Args: []string{selfexec.ChildArgument, processextension.ID},
			},
		},
		{
			ID: gitextension.ID,
			Command: host.Command{
				Path: qedExecutable,
				Args: []string{selfexec.ChildArgument, gitextension.ID},
			},
		},
	},
	Policy: policy,
	CommandEnvironment: map[string]string{
		"PATH": os.Getenv("PATH"),
		"HOME": os.Getenv("HOME"),
	},
})
if err != nil {
	log.Fatal(err)
}
defer codingProfile.Close()

runtime, err := agent.NewRuntime(agent.Options{
	Provider:        modelProvider,
	ComponentSource: codingProfile.ComponentSource(),
})
if err != nil {
	log.Fatal(err)
}

handle, err := runtime.Run(ctx, agent.RunRequest{
	AgentID:      "coding",
	Instructions: codingProfile.Instructions(),
	Input: []agent.Message{
		{Role: agent.RoleUser, Text: "Fix the failing test"},
	},
})
```

`coding.Options`はApprover、Evidence Recorder、Extension State Store、verbose logger、lifecycle timeout、project-context limit、各公式Tool implementationのlimitも受け取ります
すべての`host.Command.Path`はabsoluteである必要があります

`ComponentSource`はすべてのExtensionのToolとHookをRun終了まで固定します
Tool-only integration向けに`ToolSource`も利用できます
host-invoked Commandには`AcquireCommands`を使います

future Run向けに1つのExtensionをreloadできます

```go
generation, err := codingProfile.Reload(
	ctx,
	workspaceextension.ID,
	host.Command{
		Path: replacementExecutable,
		Args: []string{selfexec.ChildArgument, workspaceextension.ID},
	},
)
```

candidateはpublication前にprotocol、identity、component、state Restore、health validationを通過する必要があります
existing Runはold generationを保持します
各Extensionは独立してreloadされます
`CurrentGenerations`は次のRunが選択するcomplete setを返します

## Workspaceとmutation safety

- model-facing file pathは1つのcanonical workspaceからのrelative pathのみ
- file open、removal、replacementはtraversal-resistantな`os.Root` operationを使用
- searchは`.git`、symlink、non-regular file、oversized file、binary file、invalid UTF-8をskip
- `read_file`はpartial line rangeでもfull-file digestを返却
- `apply_patch`は変更するすべてのpathへcurrent digestまたは`absent` preconditionを要求
- complete patchをprevalidateし、commit直前にすべてのpreimageを再確認
- 各replacementは対応filesystemでatomic、multi-file rollbackはbest effortでcrash-atomicではない
- Git Toolはworkspace rootとrepository rootの一致を要求
- Git optional lock、pager、external diff driver、text conversion、global configurationを無効化

`git_diff`は要求された各pathをliteralとして扱います
`worktree`と`base` scopeはstandard Git ignore ruleと指定path filterを通過したuntracked regular fileをnew-file patchとして追加します
`staged` scopeはindexだけを対象にします
tracked patchとuntracked patchは1つの`MaxOutputBytes`上限を共有し、untracked file列挙には固定件数上限を設け、digestは返却した合成patch全体を対象にします
untracked discovery中はsymlinkとnon-regular fileを読まず、binary dataはGitのbinary markerで表現し、skipまたは不完全な列挙があれば`truncated`を設定します

Workspace lockは1つの`qed.workspace` process内で`search_text`、`read_file`、`apply_patch`を調整します
`qed.process`と`qed.git` processはこのlockを共有しません
reload中はoldとnew processも重複可能でlockを共有しません
無関係なhost processもfileを変更できるためdigest preconditionは必須です

## Process execution boundary

`run_command`はargv、optionalなworkspace-relative working directory、optional timeoutを受け取ります
executableを直接起動し、stdoutとstderrを別々にcaptureして上限を適用し、exit codeを報告し、Unixではcancelまたはtimeout後にprocess groupを終了します

非0終了またはtimeoutでもstructured command responseは保持し、Tool resultをerrorとして記録します
これによりProviderとEvidence Bundleは失敗したcheckと成功したcommandを区別できます

executable lookupはProfileで選択した`PATH`だけを使います
ExtensionまたはHost process environmentへfallbackしません

これはOS sandboxではありません
許可したexecutableはhost account権限でworkspace外pathへのaccess、別programの起動、network利用が可能です
その権限を許容できる場合だけ`process.execute`を与え、より強い隔離には外部sandboxまたはcontainerを使ってください

## Project context

Profileは次のroot pathからboundedかつvalid UTF-8のcontextをeager loadします

- `QED.md`
- `AGENTS.md`
- `README.md`
- `CONTRIBUTING.md`
- `.qed/context/*.md`

各sectionはrelative pathを保持します
oversized、invalid、binary、missing fileはskipします
nested `AGENTS.md`はeager loadせず、標準instructionが編集前に適用対象のnested fileを探すようmodelへ指示します

## Evidence、Session、diagnostics

proxy経由のすべてのTool invocationはRun、parent Run、Agent、Session、Extension、generation、call、capability、Policy、digest、timing、error metadataをHostへ記録します
Tool trace record内のraw argumentとoutputはSHA-256 digestで表現します
ProfileがRecorderを作成した場合は`ToolInvocations`でrecordを取得できます

declarativeな設定Runはterminal result、public Event、Tool recordをpersisted Evidence Bundleへ結合します
Session persistenceはRuntimeの責務であり、`session.MemoryStore`または`session.JSONLStore`を利用できます
Coding Profileは2つ目のconversation Storeを隠して持ちません

Bundle Eventはpublic Run payloadを保持し、prompt、Tool引数、Tool output、errorを含む場合があります
host所有Tool traceがdigestを使っていてもEvidence storageはcontent-bearingです

verbose modeはCLIまたは`coding.Options.Verbose`からすべてのExtension Initialize requestへ伝搬します
structured diagnosticsはproject context、prompt、Tool引数として渡したfile名、command output、environment value、credentialを除外します

## 保護された実model検証

manual opt-inのintegration testで実ChatGPT Codex modelに対するwrite path全体を検証できます

```sh
QED_LIVE_CODING_E2E=1 \
QED_LIVE_CODING_AUTH_PROFILE=personal \
QED_LIVE_CODING_MODEL=MODEL_ID \
go test -mod=readonly -count=1 -v \
  -run '^TestLiveCodingProfileWritesTemporaryWorkspace$' \
  ./profile/coding
```

実行前に`qed auth login --auth-profile personal`でloginし、そのprofileで利用可能なmodelを選択します
opt-in変数が正確に`1`でない場合はtestをskipし、profileとmodelの変数に暗黙の既定値はありません
live runはsubscription quotaを消費する可能性があり、成功は選択modelとProvider serviceに依存します

verbose test outputはProvider request、message境界、Tool実行、terminal Eventごとのcontent-free timelineを表示します
逐次Provider Call番号、経過時間、固定allowlist内のTool名、Tool error flag、固定Tool error class、token数を含みます
Tool error classは元のoutputを再表示せず、保護Policyの拒否、引数不正、patch構文不正、precondition失敗、patch競合、cancel、未分類の実行失敗を区別します
prompt、text delta、model output、Tool引数、Tool output、response ID、credential valueは含みません
最終failure行は既存Runtime errorを保持しますが、timelineはProvider error textを重複表示しません
Run失敗時はProviderとToolの合計Call数、token usage、Evidence Tool数、最後のEventも表示します

testはtest runnerのtemporary directory配下にsynthetic Git repositoryを構築し、model-facing ToolはQED worktreeを読みません
Host側Policyは4つのsynthetic fileの読取、precondition付きの`calc.go`更新1回、正確な`go test ./...` command、read-onlyな`git_status`と`git_diff`だけを受理します
command実行前にHostがすべてのsource fileを検査し、既知の安全なcontentのいずれかであることを確認します
command environmentはGo module download、checksum service access、telemetry、VCS access、CGO、toolchain downloadを無効にします

modelはRuntime budget内で非mutatingな`apply_patch`構文とpreconditionの失敗から自己修正できます
Evidence検証は全試行を保持し、成功した変更が正確に1つあること、commandとGit確認が成功したこと、各失敗のvalidationまたはPolicy段階が整合することを要求します
別pathの変更、未対応file操作、想定外Tool、未分類失敗からの継続は引き続きtestを失敗させます

networkを使うのは設定したmodel requestと必要なauthentication refreshだけです
Tool経由のnetwork access、任意command、Git write、追加file、absolute path、credential capabilityは拒否します
ChatGPT credentialはHostに残り、Extension childのcomplete environmentへ含めません
testはGit `HEAD`が変わらないこと、正確な最終worktree state、別のtemporary directoryに置いたprivate Evidence Bundleのsaveとload round tripも確認します

## 現在の制限

- Extension restartは中断したTool callを再実行せず、最新のhost所有Snapshotだけを
  restoreします
- 公式Tool enforcementは汎用JSON Schema engineではなく具象strict decoderを使います
- 標準network、Git write、GitHub、host-wide filesystem、shell-expansion Toolは含まれません
- 複数Extension processと無関係なhost processには1つのglobal filesystem lockではなくdigest preconditionで対応します
- host-account authorityが広すぎる環境ではprocess executionに外部security boundaryが必要です
