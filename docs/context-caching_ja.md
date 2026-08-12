# Context compilation、圧縮、prompt cache

QEDは永続Session messageをimmutableな正本として扱い、Provider callごとに一時的なmodel viewをcompileします
同じcompile処理から本文を含まないPrefix observability、任意のsemantic Checkpoint、Provider-neutralなCache Planを生成します

```text
immutable Session Events + Run input + pinned Tools
  -> Deterministic Ledgers
     -> Artifact + Execution + Constraint + Policy + Task
  -> Context Compiler
     -> canonical ModelRequest
     -> optional Checkpoint + recent raw tail
     -> externalized Evidence references
     -> logical Context Segments
  -> Cache Planner
     -> Cache Family + mode + TTL + breakpoint
     -> optional Cost Forecast
  -> Provider adapter
  -> normalized Provider Usage
```

QEDのcache制御は既定で無効です
Agentで`cache.mode`を明示した場合だけcache key、automatic cache control、explicit write markerを送ります
Provider側のimplicit behaviorはこれとは独立して適用される場合があります

## Canonical compilation

`agent.Options.ContextCompiler`へapplication独自compilerを設定できます
nilでは並行利用可能な`agent.DefaultContextCompiler`を使い、次を行います

- Tool definitionをTool名の辞書順へ固定
- Tool input schemaのJSON object key順をcanonicalize
- duplicate JSON object keyを拒否
- 空のTool schemaをcanonicalなobject schemaとして実体化
- valid JSONであるTool Call argumentをcanonicalize
- instruction本文、message本文、opaque Provider state byte列を保持

custom Providerから見えるTool順はregistration順やExtension順ではなくcanonicalな名前順です
Unicode、改行、末尾空白はuserまたはTool contentを変える可能性があるため変更しません

既定compilerは`instructions`、`tool-abi`、messageごとのappend-only Segment、任意のvolatileな`request-metadata`を生成します
各Segmentにはprompt本文ではなくdomain-separated SHA-256 digestとcanonical byte countが入ります

steeringはin-flight Provider requestやretryを変更せず、再compileもしません
Runtimeはassistant responseとそのTool batch全体を完了し、次のcompileより前にqueue済みsteering Messageを`user.message.added`として発行します
steeringがすでにqueueされている場合、end-turn responseもRunを完了せず同じ境界へ進みます
このEventを伴わないqueue受理はSession、Checkpoint、Prefix Manifest、Cache Planを変更しません

terminal後のfollow-upは設定済みSession Storeから同じSessionをreplayする新しいRunです
新しいinputはraw tail Segmentとして追加され、以前のSegmentを書き換えません
Session Storeがない場合はcallerが過去contextを明示的に渡す必要があります

## Deterministic Ledger

RuntimeはContext Compiler callの前に完全なordered Event prefixから`agent.ContextLedger`を再構築します
terminal `agent.RunResult`にはterminal Event反映後のLedgerが入ります
custom Compilerは`ContextCompileRequest.Ledger`からisolated copyを受け取り、変更してもRuntime stateには影響しません

v1 Reducerはmodel callやlive workspace readを行わず5つのtyped Ledgerを生成します

| Ledger | Runtimeから観測できる内容 |
| --- | --- |
| Artifact | Tool outputと外部化したEvidence Objectの正確なdigest |
| Execution | pending、succeeded、failed、canceledを持つProvider attemptとTool call |
| Constraint | steering originを含む解釈前の正確なuser input |
| Policy | host Tool authorization metadataとhuman approval decision |
| Task | Runごとのrunning、waiting、completed、failed、canceled state |

`BuildContextLedger`はEvent順を正本とし、連続するSession revisionとRun内sequence、ProviderとTool transactionの対応を検証し、domain-separatedなsource digestとsnapshot digestを生成します
malformedなTool JSONも実行や修復を行わず正確なbyte identityを保持します
`ValidateContextLedger`はsnapshotを再構築してderived stateの改ざんを拒否します

Ledgerはderived stateであり2つ目の正本として保存しません
MemoryとJSONL Session Storeは同じEventを同じdigestへreplayします
新しいCheckpointは本文を含まない`ContextLedgerReference`を持ち、後続`context.compacted` Eventのreplay時に直前の正確なEvent prefixと照合します

Constraint entryはすべてのuser messageがactiveだと判断するものではありません
次のstageでFact lifecycleとしてactive、superseded、resolvedの関係を追加します
Artifact entryも現在のfileやGit stateではなく、Current World Stateで正規sourceから取得する予定です

互換性のためRuntimeはcallerが渡したassistantまたはTool履歴を含む全`RunRequest.Input` entryを`user.message.added` Eventとしてemitします
Reducerはすべてをsourceとして保持し、roleが`user`のMessageだけをConstraint entryにします
steeringは引き続きplainかつnon-emptyなuser Messageだけを受理します

Constraint entryはuser本文を保持するためLedgerはprivateなcontent-bearing dataです
対応entryではTool argument、Tool output、terminal error、Policy reasonをdigestとして保持し、source Event自体にはSessionとEvidenceのstorage policyを適用します

## Evidence-preserving compression

`agent.CompactingContextCompiler`はimmutableなraw messageの上にboundedなmodel viewを作ります

- 巨大Tool outputをcontent-addressed Evidence Objectへ外部化
- assistant Tool Callと対応resultを分断しないcut pointを選択
- compact対象の正確なraw prefixをEvidence Objectへ保存
- 型付きでsize上限のある`ContextCheckpoint`を生成
- source hash、message参照、Tool outcome、generation、Session revision、encoded size、Evidence実在性を検証
- 検証済みCheckpointの後ろへrecent raw message tailを配置

CompilerはSession messageを削除も書き換えもしません
検証済みCheckpointのpublish時に`context.compacted`を発行し、`SessionSnapshot.Checkpoint`が最新generationを保持します
raw Event Logは引き続きreplay可能です

custom `CheckpointStrategy`も利用できます
QEDが結果を検証し、失敗時はstrategy errorやmessage本文をfallback labelへ含めずdeterministic strategyへ戻します
有効なcandidateがhard limit内に収まらない場合はProviderを呼ぶ前にRunを停止します

`max_input_bytes`はProvider-neutralなcanonical byte上限であり、tokenizerやmodelの公開context windowではありません
選択modelに対して安全側に調整してください
cache planningのtoken estimateだけはcanonical byteを4で割った決定的な近似を使い、実行後はProvider Usageを正とします

Evidence Objectはprivate contentです
JSON Evidence Storeは`objects/`配下へSHA-256名、mode `0600`、bounded read、atomic rename、digest検証を使って保存します
object単位の上限は64 MiBです
正確なobjectは次で取得できます

```sh
qed evidence fetch sha256:<digest> --store .qed/evidence
```

## Prefix Manifest

各`model.request.started` EventはProvider、model、任意のhashed Cache Family、Prefix Epoch、ordered Segment fingerprintを含む`prefix_manifest`を持ちます
prompt本文は含みませんがhashとsizeもcontent由来metadataなので保護してください

Prefix Epochはobservability digestでありProvider cache keyではありません
logical requestのwire renderはAdapterごとに異なるため、実際のcache再利用はProviderを正とします

JSONL Session StoreはManifestをcommon-prefix countと変更suffixで記録します
load時は完全なpublic Eventへ再構築し、以前のfull Manifest形式も読み取れます
append-only Sessionで成長済みManifest全体をturnごとに複製する二次増加を避けます

## Cache CapabilityとPlan

Providerは`agent.CacheCapabilities`を公開し、`agent.DefaultCachePlanner`が`agent.CachePolicy`と組み合わせます

| Mode | 挙動 |
| --- | --- |
| 空または`disabled` | QED側cache controlを送らない |
| `adaptive` | explicitを優先し、automaticまたはdisabledへ安全にfallback |
| `automatic` | Providerのautomatic cacheを対応時に要求 |
| `explicit` | 最長のeligible user-message境界へwrite markerを配置 |

`required: true`では未対応mode、TTL、explicit boundaryをfallbackせずcall前errorにします
それ以外は本文を含まない`fallback_reason`を記録して対応modeで継続します

Cache Family IDはProvider、model、Agent、SessionまたはRun scope、任意のhost `family`と`isolation_key`をdomain-separated SHA-256へ入力して生成します
raw isolation valueは永続化も送信もしません
異なるtenantで同名Sessionが存在し得るembedding hostはisolation keyを必ず設定してください

現在の組み込みAdapter behaviorは次のとおりです

| Adapter | QED control |
| --- | --- |
| 公式OpenAI ResponsesまたはChat Completionsの`gpt-5.6*`か検出可能な後続GPT family | automatic cache、`prompt_cache_key`、explicit content breakpoint、`prompt_cache_options`、`30m` TTL |
| 以前の公式OpenAI model | automatic cacheと`prompt_cache_key`、QED側retention overrideなし |
| custom OpenAI-compatible endpoint | `cache_capabilities`宣言がなければ無効 |
| 公式Anthropic Messages | automaticまたはexplicit `cache_control`、model別minimum、`5m`と`1h` TTL |
| custom Anthropic-compatible endpoint | `cache_capabilities`宣言がなければ無効 |
| ChatGPT Codex backend | 観測可能なautomatic cacheのみ、新しいcache request fieldなし |
| Echo | 未対応 |

未知の公式Anthropic model IDにはAdapter更新またはtrusted Capability overrideまで保守的な4,096 token minimumを使います

OpenAI explicit mappingは現在の[OpenAI prompt caching guide](https://developers.openai.com/api/docs/guides/prompt-caching)に従い、古いmodelが新fieldを拒否するためmodel gateを持ちます
Anthropic mappingは現在の[Anthropic prompt caching guide](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)に従います

trustedなcustom endpointは対応wire fieldが公式仕様にある場合だけ設定でCapabilityを宣言できます

## Cost Forecast

QEDはmodel price tableを内蔵しません
Provider profileから1 million token当たりのcurrency micro単位整数rateを注入できます
uncached、cache read、cache writeのrateが揃う場合、Plannerは次を計算します

```text
without cache = expected uses * uncached prefix cost
with cache    = one write + subsequent reads
```

v0 forecastの対象は再利用可能と推定したprefixだけです
output、volatile suffix、retry、task成功率、retrieval costは予測しません
explicit cacheのsavingが0以下ならrequired policy以外ではfallbackします

`qed cache status [run-id] --store .qed/evidence`は保存済みPlan、normalized Usage、cache read ratio、forecast、pricingから求めたactual estimate、最初のPrefix divergence、最新compaction reportを表示します
Run IDを省略すると最新Evidence Bundleを選びます

## Usage normalization

`input_token_details_reported`がtrueの場合、QEDは次を検証します

```text
input_tokens =
  uncached_input_tokens
  + cache_read_input_tokens
  + cache_write_input_tokens
```

OpenAI ResponsesとChat Completionsはinput-token detail fieldを変換します
Anthropicのtotal inputはnormal input、cache creation、cache readの合計です
experimentalなChatGPT Codex backendはbest effortで読み取ります

Run-level UsageはすべてのProvider callが完全な内訳を返した場合だけcache categoryを集計します
messageごとのUsageは個別resultを保持します

## 現在の制限

- deterministic LedgerはRuntimeから観測できるstateを扱うが、Fact lifecycle、canonical workspace再構築、model-based semantic verificationは未実装
- RuntimeはCompiler call前に完全なEvent prefixからLedgerを再構築し、incremental reducer indexは未実装
- tokenizer-backed context limitとpredictive output reserveは未実装
- Evidence retrievalはCLIとAPI経由でありmodel Toolへ自動公開しない
- Cache Planは複数stability layer breakpointではなく1つのuser-message breakpointを選ぶ
- rendered-wire Prefix Manifest、cache compareまたはexplain、keepalive、singleflight warmup、fleet coordinationは未実装
- pricingとProvider Capabilityはoperator supplied factであり古くなる可能性があるため最新Provider仕様との照合が必要
