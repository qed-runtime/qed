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

Runtimeはcustom Compiler callごとにhost所有Run IDを復元し、Agent ID、Session ID、request metadataの変更を拒否します
Provider Adapterはlocal scopeにidentityを利用できますが、明示的なprotocol要件なしにupstreamへ公開してはいけません

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

Reducerはmodel callやlive workspace readを行わず5つのtyped Ledgerを生成します

| Ledger | Runtimeから観測できる内容 |
| --- | --- |
| Artifact | Tool outputと外部化したEvidence Objectの正確なdigest |
| Execution | pending、succeeded、failed、canceledを持つProvider attemptとTool call |
| Constraint | steering originを含むactive、superseded、resolved状態の正確なuser Fact |
| Policy | host Tool authorization metadataとhuman approval decision |
| Task | Runごとのrunning、waiting、completed、failed、canceled state |

`BuildContextLedger`はEvent順を正本とし、連続するSession revisionとRun内sequence、ProviderとTool transactionの対応を検証し、domain-separatedなsource digestとsnapshot digestを生成します
malformedなTool JSONも実行や修復を行わず正確なbyte identityを保持します
`ValidateContextLedger`はsnapshotを再構築してderived stateの改ざんを拒否します

Ledgerはderived stateであり2つ目の正本として保存しません
MemoryとJSONL Session Storeは同じEventを同じdigestへreplayします
新しいCheckpointは本文を含まない`ContextLedgerReference`を持ち、準備またはcompaction Eventのreplay時に正確なEvent prefixと照合します
採用時は`context.compaction.prepared`で検証済みのprefix参照を再利用できます

Fact lifecycle宣言がないuser MessageはactiveなConstraint Factを作ります
hostは`Message.FactDirective`へ`supersede`または`resolve` actionと1つ以上の過去のactive Fact IDを明示できます
supersedeは全targetを失効させ、現在のMessageから1つのactive replacement Factを作ります
resolveは全targetを解決済みにし、解決を伝える現在のMessage自体はConstraint Factにしません
Runtimeは自然言語の類似性からtransitionを推測しません

`ConstraintFactID`はsource Event参照からstable IDを生成します
Constraint entryはraw Session message index、現在state、stateを確定したEvent、supersedes関係の両側、全transition sourceを公開します
targetは過去の一意なactive Factである必要があり、missing、future、duplicate、retired済み、malformed、循環関係を拒否します
1つの宣言は`MaxFactLifecycleTargets`を上限とします

Runtimeはinputの`Message.FactDirective`をHook、永続化、publishより前に独立した`Event.FactDirective` fieldへ移します
このEventがlifecycleのcommit pointです
保存conversation、Provider request、terminal `RunResult.Messages`にはhost control metadataを含めません
RuntimeはHookやSession Storeが観測する前に完全なEvent prefixへcandidate transitionを適用して検証します
shape errorは`Runtime.Run`または`RunHandle.Steer`が返し、安全なsteering境界へ到達する前にtargetが無効になった場合はcandidate EventをcommitせずRunを失敗させます

Fact IDはsource Eventを識別するため、terminal RunをまたぐtransitionにはEventをreplayするSession Storeが必要です
ephemeral Runでも`user.message.added` Eventを観測した後のsteeringなら同じRun内のFactを遷移できますが、過去Messageだけを後続Runへ渡しても以前のEvent identityは再生成されません

Ledger schemaはFact lifecycle fieldを含みます
新しいCheckpointは正確なcurrent Ledger generationを参照します
deterministic Checkpoint Strategyは新しいmodel向けCheckpointからsupersededとresolvedのConstraint Factを除外し、Ledger参照が完全なlifecycle snapshotをcommitします
過去のCheckpointを再利用する場合も、Compilerは永続Checkpointを変更せず現在Ledgerに照らして一時的なmodel viewをfilterします
Artifact entryは現在のfileやGit stateを表さず、これらの値はCurrent World Stateが所有します

## Current World State

`agent.Options.CurrentWorldStateSource`が設定されている場合、Runtimeは各logical Provider requestをcompileする前のsafe boundaryでSourceを呼びます
requestは完全なEvent prefixとdeterministic Ledgerのisolated copyを含みます
Sourceはconcurrency-safeでcancelを尊重し、mutationを行わず、`agent.MaxCurrentWorldStateBytes`以下のcanonical snapshotを返す必要があります

Runtimeはsnapshotを正規化し、直前の正確な`ContextLedgerReference`へ結び付け、domain-separated digestを計算して次のmodel requestより前に`current_world_state.captured`をemitします
Ledger replayはsource generation、digest、参照された全Tool completionを検証します
external callerは`ValidateCurrentWorldState`で同じ検証を実行できます
MemoryとJSONL Session Storeは`SessionSnapshot.CurrentWorldState`から最新のcapture済みEvent valueを公開します

snapshotは必須かつvolatileな`current_world_state` Context Segmentとhost context Messageになります
Session messageや`RunResult.Messages`へ追加しません
RuntimeはTool transactionを分断せず配置し、actual user Messageがraw tailだった場合はそのMessageを最後に維持します
compacting Compilerはsafe cutを選ぶ前にrender済みbyteを予約し、Current World Stateは毎回再生成してCheckpointへコピーしません

fileとGit observationはboundedなidentityとTool provenanceだけを持ちます
check resultはstdoutやstderrではなくcommand identityと正確なstructured Tool outputのdigestを持ちます
pathとargumentはuntrusted contentでありinstructionとして解釈しません
state captureはvolatile suffixを変更するため、stableな先行Segmentを書き換えずPrefix Manifestとcache planningへ反映されます

`ContextLedgerVersion`とsnapshot digest domainはversion 1です
Ledger snapshotはderived dataでありEvent Logから再構築できます
`ValidateContextLedger`は渡されたstateをそのdeterministic reductionと比較します

互換性のためRuntimeはcallerが渡したassistantまたはTool履歴を含む全`RunRequest.Input` entryを`user.message.added` Eventとしてemitします
Reducerはすべてをsourceとして保持し、roleが`user`のMessageだけをConstraint entryにします
steeringは引き続きplainかつnon-emptyなuser Messageだけを受理します

Constraint entryはuser本文を保持するためLedgerはprivateなcontent-bearing dataです
対応entryではTool argument、Tool output、terminal error、Policy reasonをdigestとして保持し、source Event自体にはSessionとEvidenceのstorage policyを適用します

## Evidence-preserving compression

`agent.CompactingContextCompiler`はimmutableなraw messageの上にboundedなmodel viewを作ります

- 巨大Tool outputをcontent-addressed Evidence Objectへ外部化
- exact Event prefixをdeterministic Ledgerと照合
- approval requestとdecisionを含むassistant Tool Callと全resultを分断しないcut pointを選択
- delegated subagent Callとterminalなparent Tool resultを同じ範囲へ保持
- mutationから最初のannotation付きverificationまたはcommit attemptまでの後続workを保持し、どちらもない場合は次のuser Messageまで保持
- compact対象の正確なraw prefixをEvidence Objectへ保存
- 型付きでsize上限のある`ContextCheckpoint`を生成
- compact済みprefixを重複しないSession Synopsis、Task、Episode layerへ分割
- source hash、message参照、Tool outcome、generation、Session revision、encoded size、Evidence実在性を検証
- active Constraint、現在のGit change、保持済みfailed check、pending Tool、required Evidenceの本文なし保存件数を公開
- 検証済みCheckpointの後ろへrecent raw message tailを配置

新規Checkpointはschema version 1を使います
ordered `Layers`はcompact済みprefix全体を一度だけcoverします
Session Synopsisはcurrent Runの開始位置まで、Taskは同じRunのうち以前にcompactされたmessage、Episodeはprefix内で最新のtransaction safeな範囲です
`recent_messages`はraw tail幅に加えてEpisode幅の目安にもなります
Tool、approval、subagent、mutation-verification、commit境界を守るためEpisodeが広くなる場合があります
Run境界がないmessage-only direct callerではTaskとEpisodeだけに分割します

保存済みCheckpointは利用可能な全layerと検証metadataを保持します
Provider向けJSONにはretained Goal、Fact、Decision、Executionを持つlevelだけを選択し、空のTaskまたはSession layerは送りません
選択済みitemはどれも1つのlevelにだけ現れます
Rebase generation、Session revision、source hash、Ledger referenceは保存済みCheckpointとpublic Eventに残し、model向けprojectionには入れません
`ContextCompactionReport.ModelLevels`が選択済みlevelの順序を記録し、`qed context inspect`と`qed context explain`でmessage本文なしに確認できます
Compilerはimmutableなsource rangeをcurrent requestのRun境界に対して再projectionします
follow-upでは保存済みgenerationをpublishし直したり変更したりせず、以前のRunのCheckpointをSession Synopsisとして提示します

Coreはcustom `CheckpointStrategy`の返却後にlayer境界を導出して検証します
Strategyはprotected transactionを分割したり別のsource partitionを偽装したりできません
publishされるversion 1 Checkpointはすべて検証済みhierarchyを持ちます

CompilerはSession messageを削除も書き換えもしません
検証済みCheckpointのpublish時に`context.compacted`を発行し、`SessionSnapshot.Checkpoint`が最新generationを保持します
raw Event Logは引き続きreplay可能です

custom `CheckpointStrategy`も利用できます
QEDが結果を検証し、失敗時はstrategy errorやmessage本文をfallback labelへ含めずdeterministic strategyへ戻します
有効なcandidateがhard limit内に収まらない場合はProviderを呼ぶ前にRunを停止します

`ContextCompactionReport.Validation`はcandidate generationとraw source境界を識別し、required item数とpreserved item数を記録します
Evidenceは正確なbyte総数も記録します
active Constraintはsource identityがCheckpoint GoalまたはFactsに残るか、正確なMessageがraw tailに残る場合だけpreservedになります
現在のGit changeと保持済みfailed checkは必須Current World State segmentに残ります
pending Tool Callはraw tailに残る必要があります
required Evidence参照はcandidate Context Programに残り、digestとsizeが一致するbyteへ解決できる必要があります

reportはstable failure codeと件数だけを持ち、message本文、path、command、object contentを含みません
Runtimeはcodeとcountの一致およびpassed reportがpublish対象の正確なCheckpointを識別することを検証します
MemoryとJSONL Session replayもcandidate generationとrollback transitionを検証します
このreportより前に保存されたEventは`validation` fieldなしで引き続き有効です

custom candidateがrequired stateを失った場合、QEDは最初に同じsafe cutをdeterministic strategyで再試行します
deterministic strategyは`checkpoint_max_bytes`へ収める目的でactive Constraint Factやrequired Evidenceを破棄しません
deterministic candidateも失敗した場合は別のsafe cutを試します
その後も失敗し、前回の検証済みCheckpointとraw tailが上限内なら、そのviewを維持してrollback `previous_checkpoint`付きのfailed reportをpublishし、失敗candidate自体はpublishせず継続します
Checkpointがないraw viewを利用できる場合はrollback `raw_context`を使います
検証済みeffective viewが上限内に存在しなければ次のProvider call前にRunを停止します

Runtimeは常にLedgerを渡すためreportがactive Constraintを対象にします
`ContextCompileRequest.Ledger`を省略するdirect Compiler callerではQEDがmessage本文からlifecycle stateを推測しないためrequired active Constraintは0件になります

Checkpoint生成には2つの明示的なmodeがあります
incremental buildは正確なraw sourceと最新の検証済み`Previous` Checkpointを受け取ります
`CheckpointBuildRawRebase`はtarget generationと`RebaseReason`を受け取りますが`Previous`はnilです
`Messages`、isolatedなordered `Events`、対応するdeterministic Ledgerから再構築する必要があります
QEDはStrategyを呼ぶ前にEvent prefixを検証します
custom Strategyは複数のsafe candidate cutで呼ばれる場合があるためmutableなrequest valueを保持してはいけません
従来`Previous`だけから全generationを導出していたStrategyは`CheckpointRequest.Generation`を使う必要があります
未対応の場合は後続Rebase candidateが拒否され、QEDはdeterministic fallbackを使います

最初のCheckpointは`initial` Raw Event Rebaseとしてreportされます
以後は次の順で最初に該当した決定的なtriggerを選びます

1. Checkpoint保存済みFactがcurrent Ledgerでactiveではない場合は`checkpoint_inconsistent`
2. CheckpointのLedger generationより後に明示的なFact lifecycle宣言がある場合は`fact_lifecycle_changed`
3. 次generationが前回Rebaseから`rebase_generation_interval` generationへ到達した場合は`generation_interval`

intervalの既定値は`4`で上限は`64`です
triggerされたRebaseは次のcompile boundaryで実行します
設定したrecent raw tailを維持できる最新safe cutまで進み、優先範囲に後続cutがなければ既存compact済みraw prefixを再構築します
input圧力によるcompactionも必要ならさらに後続のsafe cutへ進む場合があります
`ContextCheckpoint.LastRebaseGeneration`が最新の完全再構築を記録し、`ContextCompactionReport.Rebased`と`RebaseReason`が`context.compacted`上で判断を観測可能にします
messageだけを渡すdirect callerもinitialとgeneration triggerを利用できますが、明示的なlifecycle検出にはRuntimeが常に渡すEvent prefixが必要です

Runtimeは`ContextCompileRequest.Events`へexact Event prefixのisolated copyを渡します
Eventsを省略するdirect callerは従来のTool Callとresultだけを保護するsafe cutを使います
`ToolResult.ContextOperation`は`mutation`、`verification`、`commit`、`subagent`のhost-only metadataです
authorizationやoperation成功の証明ではなくcut分類であり、未知のkindはTool境界で拒否します

## 本文を含まないContext report

設定済みRunはpublic `context.compaction.prepared`と`context.compacted` EventをEvidence Bundleへ保存します
Context reportはactive compactionだけをembedding hostと同じexported read modelでprojectします

```sh
qed context inspect <run-id> --store .qed/evidence
qed context explain RUN_ID[@EVENT_SEQUENCE] --store .qed/evidence
qed context diff \
  --before RUN_ID[@EVENT_SEQUENCE] \
  --after RUN_ID[@EVENT_SEQUENCE] \
  --store .qed/evidence
```

3つのcommandはすべて`--output text|json`に対応します
`inspect`はRun内のContext timeline全体とpublish済みCheckpoint generation数、full Rebase数、rollback数、custom strategy fallback数、validation failure数、externalized object、validation preservation countの集計を表示します
compression ratioはcompiled byte総数をoriginal byte総数で割った値です
preservation rateはaggregate preserved countをaggregate required countで割った値で、required itemが0件ならunavailableのままです

`explain`は既定で最新Context Eventを選びます
正確なRun Event sequenceを追加すると過去の判断を選択できます
`diff`も両側で同じselectorを使います
失敗candidateはrollback後に同じcandidate generationを再試行できるため、selector identityにはEvent sequenceを使います
selectorは同じSessionのterminal follow-upを含む異なるRun Bundleも参照できます

projectionはstable reasonとfailure code、byteとitem count、ratio、generation番号、validation outcomeを含みます
message、path、command、object digest、object contentはcopyしません
このcontent-free出力はEvidence Bundleのsecurity境界を変えず、Bundle内のpublic Eventは通常のmessageとTool payloadを引き続き含む場合があります
QEDのstable allowlistにないcompaction reasonとfallback labelは`unrecognized`へ変換します

candidate validation reportがないEventはunreported validation countとして表示されます
対象にはdeterministic validation以前のEventと現在のEvidence-only compaction Eventの両方が含まれます
選択Bundleが以前のRunから継承したgenerationを確定できない場合もunavailableとして表示します
選択Event streamにbuilt-in retrieval metadataがない場合、compaction後のmodel rereadはunavailableのままです
retrieval completionがある場合は、可視履歴にcompactionが存在した後で成功した`context_fetch`を数えます
validation時のStore readは数えません

embedding hostは`agent.BuildContextReport`で同じJSON互換構造を構築し、`ContextReport.Snapshot`で選択し、`agent.DiffContextSnapshots`で比較できます

TUIは同じordered Eventからより小さいlive projectionを構築します
常時表示する行にはcompaction件数、effective generation、predictive input上限、最新Cache Plan modeとbreakpoint件数、報告済みinput usageを表示します
F2ではbyteとmessage件数、externalized object合計、cache TTLとUsage detail、正規化済みfallback labelを確認できます
Evidence digestとobject contentは保持せず、正確なretrievalはscope付き`context_search`または`context_fetch` ToolをAgentへ依頼します

`max_input_bytes`はProvider-neutralなcanonical byte上限であり、tokenizerやmodelの公開context windowではありません
選択modelに対して安全側に調整してください
Predictive Budgetは任意であり、この独立したhard byte境界を置き換えません

RuntimeはContext Segmentとrelevance snippetに1つの`TokenEstimator`を解決します
`agent.Options.TokenEstimator`は同じinterfaceを実装するProviderより優先され、どちらもない場合はbyte数を4で割って切り上げる`CanonicalByteTokenEstimator`を使います
1 batchは`[a-z0-9][a-z0-9._/:-]{0,127}`に一致する本文を含まないstable kindとisolated itemごとの非負countを返します
built-in compilerは両方をSegment fingerprintへ保存しますがPrefix epochとcontent hashは影響を受けません
cache planningは全Segmentで共通の設定済みkindを使い、利用できなければcanonical近似へfallbackします

call完了後はProvider Usageを正とします
publicな`agent.BuildTokenUsageReport`はestimate付き`model.request.started` Eventをcompletion、retry、failure、cancelと対応付け、Provider inputからestimateを引いた差を返します
Usage欠落時はestimateで置換せず欠落として明示します
完全なRun Event streamがあれば`qed cache status`は最新比較を表示します

## Predictive Budget

`agent.PredictiveBudgetPolicy`は解決済みSegment estimateを使うmodel固有のrequest preflightを追加します

```text
required reserve = max(output reserve, safety margin)
predicted total  = input estimate + predicted Tool output + required reserve
hard input limit = context window - predicted Tool output - required reserve
```

必要なheadroomはmodelとworkloadで異なるためsoft thresholdは絶対値で設定します
soft thresholdへ到達するとRuntimeは`PredictiveContextCompiler`へsoft threshold未満に戻る検証済みcandidateを要求します
built-in compacting compilerはcandidate Segmentをestimateし、canonical byte上限も維持しながらtransaction safeなCheckpoint cutだけを試します
Runtimeは最終的な元viewとcandidate viewを解決済みestimatorで独立して再estimateします
成功したcandidateは`context.compaction.prepared`と`SessionSnapshot.PreparedContext`へ永続化しますが、Providerには変更前のrequestを送ります
後続follow-upまたはactive Run requestは準備済みgenerationを再利用でき、Runをまたぐ利用にはSession Storeが必要です

元のpredicted totalが設定済みcontext windowを超える場合、Runtimeは収まる準備済みcandidateまたは新規candidateを`context.compacted`で採用します
収まるcandidateがなければProvider I/Oを拒否します
元の予測がhard limit未満ならsoft準備failureはterminalになりません
別の通常compactionをpublishした場合は古いprepared candidateを消去します

`PredictiveBudgetPlan`は本文を含まず、準備、採用、`model.request.started` Eventとterminal Run resultに現れます
元、candidate、Providerのestimate、estimator kind、reserve、softとhard limit、level、action、candidate generationを記録します
Event replayは算術、Checkpoint transition、正確なLedger prefix、Evidence参照を検証します
prediction errorの解析ではProvider Usageを正とします
actionが`none`の場合はcandidate fieldが元の値と一致しgenerationはありません
hardかつ未採用のplanは失敗したRun resultだけに現れProviderへ到達しません

Evidence Objectはprivate contentです
設定済みContext Compilerは新規参照をtenant、Sessionまたはephemeral Run、execution Profileのopaque digest、required retrieval Capability、sensitivityへbindingします
content digestはbyte列を識別しますがaccessを許可しません
同じSessionとProfileのfollow-up Runはobjectを再利用できますが、ephemeral Run、別tenant、別Profileからは利用できません

built-in JSON Storeはscope付きobjectをbinding digest名で`scoped-objects/`配下へ保存し、mode `0600`、bounded read、atomic rename、content digest検証を適用します
object単位の上限は64 MiBです
`private` contentは保存でき、`secret` contentはbuilt-in Storeがat-rest encryptionを持たないため保存前に拒否します
embedding hostは暗号化するscoped Store Adapterを供給できます

有効なscope付きputとretrieval attemptは保護済み`object-access.jsonl`へ本文なしrecordを追記します
recordはdigest、operation、outcome、size、timeを含みますがraw tenant、Session、Run、Profile、principal、Capability、object contentを含みません
許可済みretrievalもaudit recordを書けない場合はfail closedになります

Run Bundleから完全なscope参照を解決し、監査付きlocal administrative readを行うcommandは次です

```sh
qed evidence fetch sha256:<digest> --run-id <run-id> --store .qed/evidence
```

`--run-id`なしのcommandは`objects/`配下にあるlegacy unscoped object向けに維持されます
scope付き参照はlegacy Object Store APIから取得できません

## built-in Context retrieval Tool

retrievalは`agent.Options.ContextRetrieval`または宣言的Agentの`context.retrieval` objectでopt-inします
有効化するとProvider向けのportableな5 Toolを登録します

| Tool | bounded result |
| --- | --- |
| `context_search` | 既定ではexactな新しい順の一致を返し、明示時は固定されたbounded Event prefixを説明可能なrelevance順にしてsource参照とbounded snippetを返す |
| `context_fetch` | 現在のRunまたはSessionで参照済みのscope付きEvidence Objectから1つのUTF-8 chunkを返す |
| `session_timeline` | 本文を含まないEvent identityとactivity metadataを新しい順で返す |
| `artifact_history` | immutable Artifact Ledger entryを新しい順で返す |
| `execution_history` | 引数と出力本文を含まないProviderとTool execution Ledger entryを新しい順で返す |

listと既定searchは現在のEventまたはLedger snapshotに対するnumeric `cursor`を使い、`next_cursor`を返します
relevance searchは最初のpageでaccepted Event prefixを固定して`snapshot_event_count`を返し、ranking内のoffsetとして`next_cursor`を使います
後続pageでは同じsnapshot、`snapshot_query_digest`、queryを繰り返します
Runtimeはbindingの欠落と不一致を拒否するため、途中でretrieval Tool Eventが追記されてもresult setは移動しません
fetchはbyte `offset`を使い、UTF-8境界に制限された`next_offset`を返します
raw snippetと取得contentは`untrusted: true`を持ち、実行対象の命令ではなく過去dataとして扱います

relevance resultはtask、file、symbol、active Constraint、unresolved error、recency、過去の参照頻度、任意のsemantic、token costの正規化済みfactorと決定的なweighted totalを公開します
各resultはsnippetの正確なbyte数、token estimate、estimator kindを含みます
Runtimeは新しい順で最大`512`件の検索可能Eventだけを対象にし、さらに存在する場合は`candidate_pool_truncated`を返し、1 Event当たり最大`16384` bytesを解析します
active Constraint本文は新しい順で最大`128`件をranking signalに使います
過去の参照頻度は直近`64`件の成功search resultだけを解析し、1 resultが`262144` bytesを超える場合はskipします
signal poolが不完全な場合は`constraint_pool_truncated`または`reference_history_truncated`を返します
hostは`ContextSemanticScorer`を注入できます
Runtimeは最大`512`件のboundedなuntrusted excerptを渡し、itemごとに`0..1000`のscoreを検証します
embeddingは必須ではなく、宣言設定では選択せず、既定exact searchでは呼び出しません

各Runは全試行call数、成功時の返却item数、成功JSON出力全体のbyte数を独立して制限します
call単位のitemと出力byte上限もRun全体の上限内で適用され、Runtimeの通常Tool call上限も適用されます
limit、malformed cursor、Store未設定、未対応media type、access拒否はboundedな通常error Tool resultとなり、Runを失敗させずmodelが回復できます

`context_fetch`は要求digestをaccepted `context.compaction.prepared`または`context.compacted` Event内の完全なscope参照に対して最初に解決します
履歴にないdigestはObject Storeへ到達しません
Storeはtenant、Sessionまたはephemeral Run、Profile、required Capabilityを認可してaccess attemptを記録します
valid UTF-8のtext media typeだけを返します
Runtimeはcontentを公開する前に返却byte長とSHA-256 digestを完全なscope付き参照と照合します

built-in retrieval resultは対応する`tool.completed` Eventに本文を含まない`ToolResult.ContextRetrieval` metadataを持ちます
Session replayはoperation、outcome、count、truncation、任意のobject digest、post-compaction statusを保持します
Runtimeはmodel向けTool Messageからmetadataを除外します

設定済みRuntimeはlegacy unscoped参照を含むCheckpointのscopeを推測しません
旧Sessionはscope付き設定の下で新しく開始する必要があります
旧contentを暗黙にrebindすると新しいisolation境界を迂回するためです

## Subagent result projection

成功した各Team candidateはparentへ届く前にversion付きResult Packetへprojectionされます
packetはtypedなFact、Artifact、Execution、Evidence reference、optionalなProfile stateをchild Runのterminal Context Ledgerへ結び付けます
QEDはbound、canonical JSON、source Event所属、Evidence所属、entry ID、packet digestを検証します

既定reducerは現在childのArtifactとterminal Execution Ledger entryをコピーし、Factを推測しません
Profile reducerはdomain固有kindとstateを追加できます
これらの値はRuntime Coreの新しいLedger fieldにならず、orchestration packet内に留まります

parent Runでは完全なpacketが通常のsubagent Tool outputの一部として残ります
parent Ledgerはchild claimをparent Constraint Factとして扱わず、exact Tool output digestとsubagent transactionを記録します
Context圧縮はdelegated Callとresultを同じ範囲に保ち、その範囲を圧縮する場合はraw message Evidenceによってexact packetを保持します

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
| OrcaRouter ResponsesまたはChat Completions | `cache_capabilities`宣言がなければ無効、Session Affinityとupstream implicit cacheは独立して動作 |
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

`qed cache status [run-id] --store .qed/evidence`は保存済みPlan、最新input estimate比較、normalized Usage、cache read ratio、forecast、pricingから求めたactual estimate、最初のPrefix divergence、最新compaction reportを表示します
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

- deterministic Ledgerは明示的なFact lifecycleとRuntimeから観測できるstateを扱うが、canonical workspace再構築とmodel-based semantic verificationは未実装
- RuntimeはCompiler call前に完全なEvent prefixからLedgerを再構築し、incremental reducer indexは未実装
- Predictive model limitとreserveはoperator supplied factであり、QEDはProvider catalogからmodel context windowを発見または自動更新しない
- soft candidate準備はbackground workerではなくrequest boundaryで同期実行する
- `context_search`のexact modeはaccepted Event prefixをscanし、relevance modeは固定された完全なprefixからLedgerを再構築して新しいcandidateとsignal解析poolだけを制限し、retrieval indexと自動retrieval policyは未実装
- built-inのtokenizer-backed estimatorとembedding実装は存在せず、canonical byteを4で割った値を依存なしのtoken fallbackとして使う
- `context_fetch`はbounded chunkを返す前にscope付きObject全体を検証し、現在のStore contractはrange readを持たない
- Cache Planは複数stability layer breakpointではなく1つのuser-message breakpointを選ぶ
- rendered-wire Prefix Manifest、cache compareまたはexplain、keepalive、singleflight warmup、fleet coordinationは未実装
- pricingとProvider Capabilityはoperator supplied factであり古くなる可能性があるため最新Provider仕様との照合が必要
