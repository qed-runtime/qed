# Agent設定

QEDは1つのstrictなJSON documentからProvider profile、process分離Extension、execution Profile、Store、Agent graphを構築します

このformatは`qed run`、`qed tui`、`qed session resume`で利用されます

## 完全な例

```json
{
  "version": 1,
  "default_agent": "coordinator",
  "limits": {
    "max_runs": 16,
    "max_depth": 4,
    "max_provider_calls": 64
  },
  "providers": {
    "primary": {
      "protocol": "openai-responses",
      "model": "<openai-model-id>",
      "token_env": "PRIMARY_API_TOKEN",
      "rate_limit": {"max_concurrency": 4}
    },
    "review": {
      "protocol": "anthropic",
      "model": "<anthropic-model-id>",
      "token_env": "REVIEW_API_TOKEN",
      "max_output_tokens": 2048
    }
  },
  "extensions": {
    "qed.workspace": {"mode": "self-exec"},
    "qed.process": {"mode": "self-exec"},
    "qed.git": {"mode": "self-exec"}
  },
  "profiles": {
    "coding": {
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
    "openai-candidate": {
      "provider": "primary",
      "instructions": "Propose a precise answer"
    },
    "anthropic-candidate": {
      "provider": "review",
      "instructions": "Review the task independently"
    },
    "judge": {
      "provider": "primary",
      "instructions": "Judge candidate answers for correctness"
    },
    "coordinator": {
      "provider": "primary",
      "profile": "coding",
      "instructions": "Use specialists when useful and return the final answer",
      "provider_retry": {
        "max_attempts": 3,
        "initial_backoff": "1s",
        "max_backoff": "8s"
      },
      "context": {
        "max_input_bytes": 65536,
        "recent_messages": 12
      },
      "cache": {
        "mode": "adaptive",
        "expected_reuse": 3,
        "isolation_key": "tenant-a"
      },
      "delegations": [
        {
          "name": "consult_candidates",
          "description": "Ask independent candidates and select the best answer",
          "strategy": "select",
          "agents": ["openai-candidate", "anthropic-candidate"],
          "judge": "judge"
        }
      ]
    }
  },
  "session": {
    "store": "jsonl",
    "path": ".qed/sessions"
  },
  "evidence": {
    "store": "json",
    "path": ".qed/evidence"
  },
  "extension_state": {
    "store": "json",
    "path": ".qed/extension-state"
  }
}
```

参照するcredentialを設定してdefault Agentを実行します

```sh
export PRIMARY_API_TOKEN="<token>"
export REVIEW_API_TOKEN="<token>"
go run ./cmd/qed run \
  --config ./qed.json \
  --workspace . \
  --session-id review-1 \
  --prompt "Propose a migration plan"
```

## Top-level field

| Field | 必須 | 意味 |
| --- | --- | --- |
| `version` | yes | `1`固定 |
| `default_agent` | no | `--agent`省略時に選択するAgent |
| `limits` | no | 共有orchestration上限 |
| `providers` | yes | 名前付きProvider profile |
| `extensions` | 参照時 | 明示的なExtension process定義 |
| `extension_directories` | no | 外部manifestを再帰検索するdirectory |
| `profiles` | no | 名前付きexecution Profile |
| `agents` | yes | 名前付きAgent定義 |
| `session` | no | MemoryまたはJSONL Session Store |
| `evidence` | no | JSON Evidence Bundle Store |
| `extension_state` | no | MemoryまたはJSON Extension State Store |

`default_agent`を省略した場合、CLIでは`--agent`が必要です

すべての相対pathはJSON fileのdirectoryを基準に解決されます
ただし`--workspace`はAdapter inputであり、既定値はcurrent directoryです

## Provider profile

Provider profileはprotocol、endpoint、model、credential source、Provider optionを対応付けます
異なるAPI dialectや同じdialectの異なるendpointには別profileを作成できます

| Field | 必須 | 意味 |
| --- | --- | --- |
| `protocol` | yes | `echo`、`openai-responses`、`openai-chat`、`openai-codex`、`anthropic` |
| `base_url` | no | 信頼できるAPI base URL、公式endpointでは省略 |
| `model` | model Provider | 正確なmodel identifier |
| `token_env` | API key Providerの公式endpoint | credentialを保持するenvironment変数 |
| `auth_profile` | `openai-codex` | 名前付きChatGPT credential profile |
| `max_output_tokens` | no | output上限、`0`はProvider behaviorを選択 |
| `api_version` | no | Anthropic API version override専用 |
| `pricing` | no | forecastとUsage cost estimate用のhost supplied rate |
| `cache_capabilities` | no | 設定endpointとmodelに対するtrusted override |
| `rate_limit` | no | このprofileに対するQED側のoutbound concurrency policy |

`echo`はendpoint、model、credential、API optionを受け取りません
決定的なconcurrency testではQED側の`rate_limit` policyを利用できます

custom endpointは認証不要のlocal service向けに`token_env`を省略できます
設定経路は`OPENAI_API_KEY`、`ANTHROPIC_API_KEY`、`QED_API_KEY`へfallbackしません
`token_env`がある場合はHTTP requestごとにcredentialを解決するため、embedding hostによるrotationが可能です

Provider profile IDはProvider identityとopaque continuation stateに含まれ、別endpointまたは別profileへのstate再利用を防ぎます

`openai-codex`は別途保存したChatGPT OAuth profileを読み、固定のChatGPT Codex backendを使います
`protocol`、`model`、`auth_profile`、任意の`pricing`と`rate_limit`を受け取り、`base_url`、`token_env`、`max_output_tokens`、`api_version`、`cache_capabilities`を拒否します
設定をloadする時点で名前付きprofileが存在する必要があります

```json
{
  "version": 1,
  "default_agent": "subscription-agent",
  "providers": {
    "subscription": {
      "protocol": "openai-codex",
      "model": "<codex-model-id>",
      "auth_profile": "personal"
    }
  },
  "agents": {
    "subscription-agent": {"provider": "subscription"}
  }
}
```

```sh
qed auth login --auth-profile personal
qed run --config qed.json --prompt "Reply with a short greeting"
```

credential保存、refresh、protocol制限は[ChatGPT subscription認証](chatgpt-auth_ja.md)を参照してください

### Outbound Provider rate制御

`rate_limit`はProvider profile単位で設定します

| Field | 必須 | 意味と既定値 |
| --- | --- | --- |
| `max_concurrency` | no | activeなProvider streamの最大数、`0`または省略時は`4`、それ以外の範囲は`1`から`1024` |

同じprofileを参照する全Agentは1つのlimiterを共有します
この共有範囲には並行top-level `Host` Runと並行subagentも含まれます
同じaccountまたはendpointを対象にしていても、異なるprofile間ではcapacityとcooldown stateを共有しません
QEDがupstreamのshared-limit bucketを安全に推測できないためです

`rate_limited` responseは実効retry delayでprofile全体のcooldownを更新します
`Retry-After`を最小値とし、fallback exponential backoffと小さなbounded per-Run jitterで集中retryを防ぎます
queueされたRunはcancelとDeadlineに従い、capacityを取得してactual attemptを開始できるまでProvider call budgetを消費しません

`max_concurrency`はlocalな保護上限であり、RPMやtoken rateを保証するものではありません
Runtime local call上限とorchestration全体のAgent RunおよびProvider call上限は独立したhard boundとして維持されます

## Extension定義

1つのexecution Profileは複数Extensionを参照できます
定義は3つのstartup modeをサポートします

### Self-exec mode

```json
{
  "extensions": {
    "qed.workspace": {"mode": "self-exec"},
    "qed.process": {"mode": "self-exec"},
    "qed.git": {"mode": "self-exec"}
  }
}
```

Self-execはcurrent Host executableをhidden Extension entrypointで起動します
利用可能なIDはHost側の`extensions.lock`から生成したcatalogで決まり、公式QED executableは現在`qed.workspace`、`qed.process`、`qed.git`を選択します
組み込みapplicationは独自Catalogを`qed.HostLoadOptions`へ渡します
Hostは外部manifestと同じ方法でlock済みdeclarationをlive processと照合します
このmodeは`command`と`manifest`を拒否します

`extensions.lock`変更後にcatalogを再生成し、checked-in sourceが最新か確認できます

```sh
qed extension generate
qed extension generate --check
```

### External command mode

```json
{
  "extensions": {
    "my-extension": {
      "mode": "external",
      "command": ["./bin/my-extension", "--fixed-argument"],
      "directory": "./extensions/my-extension",
      "environment": ["PATH"],
      "configuration": {"feature": true}
    }
  }
}
```

`command[0]`と`directory`は設定fileを基準に解決します
argumentはshell評価せず直接渡します
`environment`はExtension processへ渡す完全な選択済みenvironmentを表します

### Manifest mode

```json
{
  "extensions": {
    "my-extension": {
      "mode": "manifest",
      "manifest": "./extensions/my-extension/qed-extension.json",
      "environment": ["PATH"],
      "configuration": {"feature": true}
    }
  }
}
```

Manifest modeは検証済みentrypointを解決し、HandshakeとDescribeのidentity、version、capability、Hook、Commandが外部宣言と一致することを要求します
`command`と`directory`を拒否します

### Discovery

```json
{
  "extension_directories": ["./extensions"]
}
```

Discoveryは`qed-extension.json`を再帰検索し、directory symlinkを無視し、重複IDを拒否します
discovered Extensionはmanifest directoryをchild working directoryとして使い、既定ではenvironmentとconfigurationを受け取りません
同じIDを明示定義とdiscoveryの両方で利用できません

Extension process environmentとTool command environmentは別です
`extensions.<id>.environment`はExtension executableへ渡されます
`profiles.<id>.environment`はInitializeですべての選択Extensionへ渡されます
QED catalogの`qed.process`と`qed.git`はcommand実行に利用し、`qed.workspace`は無視します
どちらのenvironmentにもProvider credential変数を選択しないでください

manifestとlifecycleは[Extension process](extensions_ja.md)を参照してください

## Execution Profile

version 1は`kind: "coding"`をサポートします

| Field | 必須 | 意味 |
| --- | --- | --- |
| `kind` | yes | 現在は`coding` |
| `extensions` | yes | Runごとにatomicに取得する1つ以上のExtension ID |
| `capabilities` | yes | staticな`allow`、`ask`、`deny` list |
| `environment` | no | Initializeで渡す選択済みenvironment |

capability nameは有効な形式であれば外部Extension由来でも利用できます
公式Workspace、Process、Git Extensionは`filesystem.read`、`filesystem.write`、`filesystem.delete`、`process.execute`、`git.read`を使います
重複または競合するruleは拒否され、未指定capabilityはdenyになります

`ask` decisionはhost Approverを呼び出します
`qed run`の既定値は`--approval deny`です
`--approval prompt`は永続的なwaitを生成し、stdinから直列化されたyesまたはnoを読みます
prompt diagnosticsはTool名とcapabilityだけを含み、raw Tool引数を含みません

選択したenvironment名はすべて存在する必要があります
executable lookupは選択した`PATH`だけを使い、Host environmentへfallbackしません

## Agent定義とdelegation

| Field | 必須 | 意味 |
| --- | --- | --- |
| `provider` | yes | Provider profile ID |
| `profile` | no | Execution Profile ID |
| `instructions` | no | このAgentのbase instruction |
| `max_provider_calls` | no | Runtime local Provider call上限 |
| `max_tool_calls` | no | Runtime local Tool call上限 |
| `provider_retry` | no | transientなProvider failureに対するbounded retry policy |
| `context` | no | Evidence preservingなcontext圧縮policy |
| `cache` | no | Provider neutralなprompt cache policy |
| `delegations` | no | このAgentへ公開するSubagent Tool |

Delegation field

| Field | 必須 | 意味 |
| --- | --- | --- |
| `name` | yes | parent model-facing Tool名 |
| `description` | no | Tool description |
| `strategy` | yes | `delegate`、`collect`、`select`、`consensus` |
| `agents` | yes | 固定candidate Agent ID |
| `judge` | selectとconsensus | reduction Agent ID |
| `instructions` | no | candidate追加instruction |
| `judge_instructions` | no | judge追加instruction |

candidateは指定Agent順を維持しながら並行実行されます
loaderは直接または間接delegation cycleを拒否します
Subagent Toolは明示的なpromptだけを渡し、parentの完全なconversation、Session ID、Metadataを渡しません

共有上限の既定値はAgent Run 16、depth 4、Provider call 64です
parent、candidate、judge Runは同じtop-level budgetを消費します

## Provider retry

Provider retryはAgent単位で設定します

| Field | 必須 | 意味と既定値 |
| --- | --- | --- |
| `max_attempts` | no | 1つのlogical model requestに対する総attempt数、既定値`3`、`1`でretry無効 |
| `initial_backoff` | no | 最初のfailure後に使う正のGo duration、既定値`1s` |
| `max_backoff` | no | exponential fallback delayを制限する正のGo duration、既定値`8s` |

QEDは`retryable`と`rate_limited`だけをretryします
有効な`Retry-After` response headerは最小delayとして扱い、`max_backoff`を超える場合があります
QEDは実効delayへ小さなbounded per-Run jitterを加えます
すべてのattemptはRuntime localと共有Provider call budgetを消費し、Run cancelとDeadlineに従います

automatic retryは最初の観測可能な`ModelStream` itemより前のfailureだけに限定されます
text deltaまたはcompleted messageの後はretryしないため、公開済みoutputやTool副作用を重複させません
ordered Event streamは各delayの前に`provider.retry.scheduled`を出します
公開error codeとEvent fieldは[Provider errorとretry](providers_ja.md)を参照してください

## Context圧縮とprompt cache

Context圧縮はAgent単位で設定します
QEDは正確なcompact済みmessage prefixと外部化したTool outputをcontent-addressed objectとして保存するためJSON Evidence Storeが必要です

| Context field | 必須 | 意味と既定値 |
| --- | --- | --- |
| `max_input_bytes` | yes | canonical logical inputのhard byte上限 |
| `recent_messages` | no | 優先するraw tail長、既定値`12` |
| `evidence_threshold_bytes` | no | Tool outputを外部化するsize、既定値`16384` |
| `evidence_excerpt_bytes` | no | 両端に保持するbyte数、既定値`2048` |
| `checkpoint_max_bytes` | no | encoded Checkpoint上限、既定値`8192` |

`max_input_bytes`は決定的なProvider neutral値でありtokenizer basedなmodel context limitではありません
QEDはraw Session messageを書き換えません
検証済みCheckpointとrecent raw tailをcompileし、安全なTool transaction境界でhard limit内に収まらなければProvider call前に停止します

`cache`を省略した場合、または`mode`が空か`disabled`の場合、QED側のprompt cache制御は無効です
Provider側のimplicit behaviorは独立して発生する場合があります

| Cache field | 必須 | 意味と既定値 |
| --- | --- | --- |
| `mode` | yes | `disabled`、`adaptive`、`automatic`、`explicit` |
| `ttl` | no | `5m`、`30m`、`1h`などProviderへ要求するlifetime |
| `expected_reuse` | no | prefixの予想総利用回数、既定値`2` |
| `required` | no | 未対応requestをfallbackせず失敗させる |
| `isolation_key` | no | hashed family IDだけに含めるhost isolation label |
| `family` | no | hashed family IDだけに含めるhost sharing label |

`adaptive`はexplicit breakpoint、automatic cache、disabled Planの順に選びます
Cache FamilyにはProvider、model、Agent、Session IDも入り、SessionがなければRun IDが入ります
raw `isolation_key`と`family`はEventへ永続化せずProviderへも送りません

operator supplied pricingをmodel名から推測することはありません

```json
{
  "pricing": {
    "currency": "USD",
    "uncached_input_micros_per_million": 2500000,
    "cache_read_micros_per_million": 250000,
    "cache_write_micros_per_million": 3000000,
    "output_micros_per_million": 10000000
  }
}
```

rateは1 million token当たりの`currency`のmicro単位です
`pricing`がある場合は3つのinput rateをすべて正数にする必要があり、output rateは0でも構いません
この数値は例示であり現在のProvider価格ではありません

trustedなcustom endpointはAdapterがrender可能なCapability factを宣言できます

```json
{
  "cache_capabilities": {
    "exact_prefix": true,
    "supports_cache_key": true,
    "supports_explicit": true,
    "supports_automatic": true,
    "max_write_breakpoints": 4,
    "minimum_prefix_tokens": 1024,
    "supported_ttls": ["30m"],
    "supports_mixed_ttl": false,
    "exposes_read_tokens": true,
    "exposes_write_tokens": true
  }
}
```

選択したwire Adapterがrenderできないfieldを宣言しないでください
組み込みendpointとmodel検出、wire mapping、現在の制限は[Context compilation、圧縮、prompt cache](context-caching_ja.md)を参照してください

## Session Store

```json
{
  "session": {"store": "memory"}
}
```

```json
{
  "session": {"store": "jsonl", "path": ".qed/sessions"}
}
```

Memory Sessionはprocess localです
JSONL Sessionはappend-onlyでprivate fileとrevision lockを使い、provider-private continuation stateを保持し、後続CLI processからresumeできます
Prefix Manifestはcommon prefixと変更suffixとして保存し、load時に再構築します
以前のfull Manifest record形式も読み取れます

`--session-id`には設定済みSession Storeが必要です
Runtimeはpublic Eventを追記し、message、pending wait、pending Tool callを再構築します
永続waitには`qed session resume <id> --config <path>`を使います
approval resumeは`--approval prompt|approve|deny`を受け取り、それ以外のwait kindには`--response-json`が必要です

active Runのsteeringは既存の`user.message.added` Event typeを維持し、`Event.UserMessageOrigin`を`steering`に設定します
queue受理はprocess localであり、このEventが永続Sessionへの反映境界です
cancel、Deadline切れ、terminal Run failureでは、Event未発行のsteeringを破棄する場合があります

follow-upは前のhandleがterminal resultへ達した後、同じSession IDで開始する新しいRunです
Sessionをreplayしますが、新しいRun IDとRuntime localのProvider、Tool上限を持ちます
Session Storeは`agent.Budget`を永続化しないため、複数follow-up Runで1つのbudgetを使う場合だけ同じ`*agent.Budget`を明示的に再利用します
Session Storeが未設定の場合、Session IDはmessageを保持しないためcallerが過去contextを渡す必要があります

## Evidence Store

```json
{
  "evidence": {"store": "json", "path": ".qed/evidence"}
}
```

設定済みCLIとTUI Runはterminal完了後にversion付きBundleを保存します
Bundleはpublic Event、usage、configとworkspaceのdigest、host所有Tool traceを含みます
同じStoreがcontext圧縮用のcontent-addressed objectも保持します
2つのcommand familyからBundleをinspectまたはexportできます

Tool trace payloadはdigestで表現されますが、public Eventは通常のobservable payloadを保持します
そのためBundleはprompt、assistant message、Tool引数、Tool output、wait payload、errorを含む場合があります
Session dataと同様に保護してください

```sh
qed run inspect <run-id> --store .qed/evidence
qed run export <run-id> --store .qed/evidence
qed evidence inspect <run-id> --store .qed/evidence
qed evidence export <run-id> --store .qed/evidence
qed evidence fetch sha256:<digest> --store .qed/evidence
qed cache status [run-id] --store .qed/evidence
```

`qed cache status`はRun ID省略時に最新Bundleを選び、effective Plan、normalized cache Usage、任意のforecastとUsage cost estimate、最初のPrefix divergence、最新compaction recordを表示します

## Extension State Store

```json
{
  "extension_state": {"store": "memory"}
}
```

```json
{
  "extension_state": {"store": "json", "path": ".qed/extension-state"}
}
```

Hostはboundedなopaque Snapshot valueをExtension IDとworkspaceまたはProfile由来scopeで保存します
startup時にstateをrestoreし、reload前とorderly close時にcurrent generationをpersistします
このStoreはAgent SessionおよびEvidenceと別です

## CLI scopeとverbose diagnostics

`--config`はinlineの`--provider`、`--model`、`--base-url`、`--auth-profile`、`--system`、`--max-output-tokens`と競合します
`--agent`、`--workspace`、`--session-id`には`--config`が必要です
`--approval`はnon-interactiveな設定Runで利用し、設定TUIの承認には`Y`と`N`を使います

root `--verbose` flagはsubcommandより前に置きます

```sh
qed --verbose run --config qed.json --prompt "inspect this project"
qed --verbose tui --config qed.json --prompt "inspect this project"
```

booleanはすべての設定Runtimeと末端のExtension Serverへ伝搬します
diagnosticsはstderrへ構造化出力され、content-bearing valueとsecret valueを除外します

## Strict parsingと互換性

- document上限は1 MiB
- unknown fieldを拒否
- objectのduplicate keyを拒否
- trailing JSON valueを拒否
- ID、path、environment名、参照、すべてのgraph edgeを利用前に検証
- graphを返す前にすべての参照Extensionを起動してlifecycleを検証
- version 1はexperimentalであり、最初のstable release前はmigrationなしで変更される可能性あり

embedding applicationは`qed.LoadHost`でこのschemaを読み込みます
self-exec entryにはapplicationの生成Catalogとabsoluteな現在のexecutableを`qed.HostLoadOptions`へ渡す必要があります
applicationは`Host.Close`または`CloseContext`を呼び、設定済みExtension processをdrainして停止します
詳細は[QEDの組み込み](embedding_ja.md)を参照してください
