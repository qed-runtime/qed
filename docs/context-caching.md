# Context compilation, compression, and prompt caching

QED treats persisted Session messages as an immutable source and compiles a
temporary model view before every Provider call. The same compilation produces
content-free Prefix observability, an optional semantic Checkpoint, and a
Provider-neutral Cache Plan

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

QED cache controls are disabled by default. An Agent must explicitly configure
`cache.mode` before QED sends a cache key, automatic-cache control, or explicit
write marker. Provider-side implicit behavior may still apply independently

## Canonical compilation

`agent.Options.ContextCompiler` accepts an application-defined compiler. A nil
value selects the concurrency-safe `agent.DefaultContextCompiler`, which

- sorts Tool definitions lexicographically by Tool name
- canonicalizes JSON object key order in Tool input schemas
- rejects duplicate JSON object keys
- materializes an empty Tool schema as a canonical object schema
- canonicalizes valid JSON Tool Call arguments
- preserves instruction text, message text, and opaque Provider state bytes

Tool order visible to custom Providers is canonical name order, not registration
or Extension order. Unicode, line endings, and trailing whitespace remain
unchanged because normalizing them could alter user or Tool content

The default compiler emits `instructions`, `tool-abi`, one append-only Segment
per message, and optional volatile `request-metadata`. Each Segment contains a
domain-separated SHA-256 digest and canonical byte count without prompt text

Steering never mutates or recompiles an in-flight Provider request or retry.
Runtime first completes the assistant response and its entire Tool batch, then
emits each queued steering Message as `user.message.added` before the next
compile. An end-turn response follows the same boundary instead of completing
the Run when steering is already queued. Queue acceptance without that Event
does not change the Session, Checkpoint, Prefix Manifest, or Cache Plan

A terminal follow-up is a new Run whose configured same-Session replay supplies
the previous append-only messages. Its new input becomes another raw tail
Segment; no previous Segment is rewritten. Without a Session Store, the caller
must provide prior context explicitly

## Deterministic Ledgers

Before each Context Compiler call, Runtime rebuilds `agent.ContextLedger` from
the complete ordered Event prefix. The terminal `agent.RunResult` contains the
Ledger after the terminal Event. A custom Compiler receives an isolated copy in
`ContextCompileRequest.Ledger`; changing it cannot alter Runtime state

The v2 Reducer produces five typed ledgers without calling a model or reading a
live workspace

| Ledger | Runtime-observable contents |
| --- | --- |
| Artifact | exact digests of Tool output and externalized Evidence Objects |
| Execution | Provider attempts and Tool calls with pending, succeeded, failed, or canceled state |
| Constraint | exact user Facts with active, superseded, or resolved state, including steering origin |
| Policy | host Tool authorization metadata and human approval decisions |
| Task | each Run's latest running, waiting, completed, failed, or canceled state |

`BuildContextLedger` treats Event order as authoritative, verifies contiguous
Session revisions and per-Run sequences, pairs Provider and Tool transactions,
and creates domain-separated source and snapshot digests. It preserves the
exact byte identity of malformed Tool JSON without trying to execute or repair
it. `ValidateContextLedger` rebuilds the snapshot and rejects changed derived
state

The Ledger is derived state and is not stored as a second source of truth
Memory and JSONL Session Stores replay the same Events into the same digest
New Checkpoints contain a content-free `ContextLedgerReference`; replay verifies
it against the exact preparation or compaction Event prefix. Adoption may reuse
the already validated prefix reference from `context.compaction.prepared`

Every user Message without a lifecycle directive creates an active Constraint
Fact. The host can explicitly attach `Message.FactDirective` with action
`supersede` or `resolve` and one or more earlier active Fact IDs. Superseding
retires every target and creates one active replacement Fact from the current
Message. Resolving retires every target and does not turn the resolution
Message itself into a Constraint Fact. Runtime never infers these transitions
from natural-language similarity

`ConstraintFactID` derives the stable ID from the source Event reference.
Constraint entries expose the raw Session message index, current state, the
Event that established that state, both sides of a supersedes relation, and all
transition sources. Targets must be unique earlier active Facts; missing,
future, duplicate, already retired, malformed, and cyclic relationships are
rejected. One directive is bounded by `MaxFactLifecycleTargets`

Runtime moves a directive from input `Message.FactDirective` to the separate
`Event.FactDirective` field before Hooks, persistence, and publication. The
Event is the lifecycle commit point. The stored conversation, Provider request,
and terminal `RunResult.Messages` do not contain this host control metadata.
Runtime validates the candidate transition against the complete Event prefix
before any Hook or Session Store observes it. Shape errors are returned by
`Runtime.Run` or `RunHandle.Steer`; a target that becomes invalid before the
safe steering boundary fails the Run without committing the candidate Event

Because Fact IDs identify source Events, a transition across terminal Run
boundaries requires a configured Session Store that replays those Events. An
ephemeral Run can transition a Fact through steering after observing its
`user.message.added` Event, but passing prior Messages alone to a later Run does
not recreate their earlier Event identities

Ledger schema v2 adds Fact lifecycle fields. New Checkpoints reference v2,
while replay still verifies references produced by Ledger v1. The deterministic
Checkpoint strategy omits superseded and resolved Constraint Facts from a new
model-facing Checkpoint. When reusing an earlier Checkpoint, the Compiler also
filters its transient model view against the current Ledger without mutating
the persisted Checkpoint. Its Ledger reference commits the complete lifecycle
snapshot. Artifact entries do not represent current file or Git state because
those values belong to Current World State

## Current World State

When `agent.Options.CurrentWorldStateSource` is configured, Runtime calls it at
the safe boundary before compiling each logical Provider request. The request
contains isolated copies of the complete Event prefix and its deterministic
Ledger. The Source must be concurrency-safe, honor cancellation, avoid
mutation, and return a canonical snapshot no larger than
`agent.MaxCurrentWorldStateBytes`

Runtime normalizes the snapshot, binds it to the exact preceding
`ContextLedgerReference`, computes a domain-separated digest, and emits
`current_world_state.captured` before the next model request. Ledger replay
validates the source generation, digest, and every referenced Tool completion
`ValidateCurrentWorldState` performs the same check for an external caller
Memory and JSONL Session Stores expose the latest captured Event value through
`SessionSnapshot.CurrentWorldState`

The snapshot becomes one required, volatile `current_world_state` Context
Segment and a host context Message. It is not appended to Session messages or
`RunResult.Messages`. Runtime places it without splitting a Tool transaction
and keeps an actual user Message last when that Message was already the raw
tail. The compacting Compiler reserves the rendered bytes before selecting a
safe cut; Current World State is regenerated and never copied into a
Checkpoint

File and Git observations carry only bounded identities and Tool provenance
Check results carry command identity and the digest of exact structured Tool
output, not stdout or stderr. Paths and arguments are untrusted content and are
never interpreted as instructions. State capture changes the volatile suffix
and therefore participates in Prefix Manifest and cache planning without
rewriting stable earlier Segments

`ContextLedgerVersion` and the snapshot digest domain changed from v1 to v2.
Standalone v1 snapshots are derived data and should be rebuilt from their Event
Log; `ValidateContextLedger` compares supplied state with the current v2
reduction. Compatibility support is limited to v1 references already embedded
in persisted Checkpoints whose Event prefix predates Current World State
capture

For compatibility, Runtime emits one `user.message.added` Event for every
`RunRequest.Input` entry, including caller-supplied assistant or Tool history
The Reducer retains all of those Events as sources but creates Constraint
entries only for Messages whose role is `user`. Steering remains restricted to
plain, non-empty user Messages

Ledgers are content-bearing private data because Constraint entries retain user
text. Tool arguments, Tool output, terminal errors, and Policy reasons are kept
as digests inside the corresponding entries, while their source Events remain
subject to the Session and Evidence storage policy

## Evidence-preserving compression

`agent.CompactingContextCompiler` adds a bounded model view above the immutable
raw messages

- it externalizes large Tool output into a content-addressed Evidence Object
- it validates the exact Event prefix against the deterministic Ledger
- it selects a cut that does not split an assistant Tool Call from all results,
  including an approval request and decision inside that Tool transaction
- it keeps a delegated subagent Call with its terminal parent Tool result
- it keeps a mutation and subsequent work together through the first annotated
  verification or commit attempt, or until the next user Message
- it stores the exact compacted raw prefix as an Evidence Object
- it creates a typed, size-bounded `ContextCheckpoint`
- it validates source hashes, message references, Tool outcomes, generation,
  Session revision, encoded size, and exact Evidence availability
- it publishes content-free preservation counts for active Constraints, current
  Git changes, retained failed checks, pending Tools, and required Evidence
- it injects the validated Checkpoint followed by the recent raw message tail

The Compiler never deletes or rewrites Session messages. A successfully
published Checkpoint emits `context.compacted`; `SessionSnapshot.Checkpoint`
holds the latest generation and the raw Event Log remains replayable

A custom `CheckpointStrategy` may create the semantic view. QED validates its
result and falls back to the deterministic strategy without exposing the
strategy error or message content in the fallback label. If no valid candidate
fits the configured hard limit, the Run stops before calling the Provider

`ContextCompactionReport.Validation` identifies the candidate generation and
raw source boundary, then records required and preserved item counts. Evidence
also records exact byte totals. An active Constraint is preserved only when its
source identity remains in the Checkpoint Goal or Facts, or its exact Message
remains in the raw tail. Current Git changes and every retained failed check
remain in the required Current World State segment. A pending Tool Call must
remain in the raw tail. Every required Evidence reference must remain in the
candidate Context Program and resolve to bytes matching its digest and size

The report contains stable failure codes and counts, never message text, paths,
commands, or object content. Runtime validates that the codes match the counts
and that a passed report identifies the exact published Checkpoint. Memory and
JSONL Session replay also verify the candidate generation and rollback
transition. Events written before this report existed remain valid without a
`validation` field

If a custom candidate loses required state, QED first retries the same safe cut
with the deterministic strategy. The deterministic strategy does not discard
active Constraint Facts or required Evidence merely to meet
`checkpoint_max_bytes`. If the deterministic candidate still fails, the
Compiler tries other safe cuts. It then retains the previous validated
Checkpoint plus raw tail when that view fits, publishes a failed report with
rollback `previous_checkpoint`, and continues without publishing the failed
candidate. An uncheckpointed raw view uses rollback `raw_context` when
available. If no validated effective view fits, the Run stops before its next
Provider call

Runtime always supplies a Ledger, so its reports cover active Constraints. A
direct Compiler caller that omits `ContextCompileRequest.Ledger` gets zero
required active Constraints because QED does not infer lifecycle state from
message text

Checkpoint construction has two explicit modes. An incremental build receives
the latest validated `Previous` Checkpoint together with exact raw source. A
`CheckpointBuildRawRebase` receives the target generation and
`RebaseReason`, but `Previous` is nil. It must rebuild from `Messages`, the
isolated ordered `Events`, and the matching deterministic Ledger. QED validates
the Event prefix before invoking the Strategy. A custom Strategy may be invoked
for more than one safe candidate cut and must not retain mutable request values
Strategies that previously derived every generation solely from `Previous`
must use `CheckpointRequest.Generation`; otherwise a later Rebase candidate is
rejected and QED uses the deterministic fallback

The first Checkpoint is reported as an `initial` Raw Event Rebase. After that,
the compiler selects the first applicable deterministic trigger in this order

1. a Fact stored in the Checkpoint is no longer active in the current Ledger:
   `checkpoint_inconsistent`
2. an explicit Fact lifecycle directive occurred after the Checkpoint Ledger
   generation: `fact_lifecycle_changed`
3. the next generation reaches `rebase_generation_interval` generations after
   the prior Rebase: `generation_interval`

The interval defaults to `4` and is bounded at `64`. A triggered Rebase occurs
at the next compile boundary. It advances through the latest safe cut that
preserves the configured recent raw tail, or rebuilds the existing compacted
prefix when no later preferred cut exists. If input pressure also requires
compaction, the compiler may advance farther.
`ContextCheckpoint.LastRebaseGeneration` records the latest full rebuild;
`ContextCompactionReport.Rebased` and `RebaseReason` make the decision
observable on `context.compacted`. Direct message-only callers still get the
initial and generation triggers, but explicit lifecycle detection requires the
Event prefix that Runtime always supplies

Runtime supplies `ContextCompileRequest.Events` as an isolated copy of the
exact Event prefix. A direct caller that omits Events retains the legacy Tool
Call/result safe-cut behavior. `ToolResult.ContextOperation` is host-only
metadata with `mutation`, `verification`, `commit`, or `subagent` kind. It is a
cut classification, not authorization or proof that an operation succeeded.
Unknown kinds are rejected at the Tool boundary

## Content-free Context reports

Configured Runs store public `context.compaction.prepared` and
`context.compacted` Events in their Evidence Bundles. Context reports project
only active compactions through the same exported read model used by embedding
hosts

```sh
qed context inspect <run-id> --store .qed/evidence
qed context explain RUN_ID[@EVENT_SEQUENCE] --store .qed/evidence
qed context diff \
  --before RUN_ID[@EVENT_SEQUENCE] \
  --after RUN_ID[@EVENT_SEQUENCE] \
  --store .qed/evidence
```

All three commands accept `--output text|json`. `inspect` reports the complete
Context timeline for the Run and aggregates the number of published Checkpoint
generations, full Rebases, rollbacks, custom-strategy fallbacks, validation failures,
externalized objects, and validation preservation counts. Its compression
ratio is the aggregate compiled byte total divided by the aggregate original
byte total. Preservation rates are aggregate preserved counts divided by
aggregate required counts and remain unavailable when no item was required

`explain` selects the latest Context Event by default. Appending an exact Run
Event sequence selects an earlier decision. `diff` uses the same selector for
each side. Event sequence is the selector identity because a failed candidate
may roll back and later retry the same candidate generation. Selectors may
refer to different Run Bundles, including terminal follow-ups in one Session

The projection includes stable reason and failure codes, byte and item counts,
ratios, generation numbers, and validation outcomes. It does not copy messages,
paths, commands, object digests, or object content. This content-free output
does not change the Evidence Bundle security boundary: its public Events may
still contain normal message and Tool payloads. Compaction reason and fallback
labels outside QED's stable allowlist are projected as `unrecognized`

Events without a candidate validation report remain visible with an unreported
validation count. This includes both older Events and current Evidence-only
compaction Events. A generation inherited from an earlier Run is also
unavailable when the selected Bundle does not establish it. Post-compaction
model rereads remain unavailable when the selected Event stream has no built-in
retrieval metadata. Once it contains a retrieval completion, the metric counts
successful `context_fetch` calls whose visible history already contained a
compaction. Validation-time Store reads are not counted

Embedding hosts can build the same JSON-compatible structures with
`agent.BuildContextReport`, select with `ContextReport.Snapshot`, and compare
with `agent.DiffContextSnapshots`

`max_input_bytes` is a provider-neutral canonical byte limit, not a tokenizer or
the model's advertised context window. It is deliberately deterministic but
must be calibrated conservatively for the selected model. Predictive Budgeting
is optional and does not replace that independent hard byte boundary

Runtime resolves one `TokenEstimator` for Context Segments and relevance
snippets. `agent.Options.TokenEstimator` wins over a Provider that implements
the same interface; otherwise QED uses `CanonicalByteTokenEstimator`, which
returns the ceiling of bytes divided by four. One batch returns a stable
content-free kind matching `[a-z0-9][a-z0-9._/:-]{0,127}` and one non-negative
count per isolated item. Built-in compilers store both on Segment fingerprints,
while Prefix epochs and content hashes deliberately ignore them. Cache planning
uses one common configured kind or falls back to the canonical approximation

Provider Usage remains authoritative after a call. Public
`agent.BuildTokenUsageReport` pairs each estimated `model.request.started`
Event with completion, retry, failure, or cancellation and reports the signed
Provider-input-minus-estimate difference. Missing Usage remains explicit rather
than being replaced by an estimate. `qed cache status` displays the latest
comparison when a complete Run Event stream is available

## Predictive Budgeting

`agent.PredictiveBudgetPolicy` adds a model-specific request preflight using the
resolved Segment estimate

```text
required reserve = max(output reserve, safety margin)
predicted total  = input estimate + predicted Tool output + required reserve
hard input limit = context window - predicted Tool output - required reserve
```

The absolute soft threshold is configurable because useful headroom differs by
model and workload. On reaching it, Runtime asks a
`PredictiveContextCompiler` for a validated candidate that returns below the
soft threshold. The built-in compacting compiler estimates candidate Segments,
retains its canonical-byte limit, and tries only transaction-safe Checkpoint
cuts. Runtime independently re-estimates the final original and candidate
views with the resolved estimator. A successful candidate is persisted as
`context.compaction.prepared` and `SessionSnapshot.PreparedContext`, but the
unchanged original request still reaches the Provider. A later follow-up or
active Run request can reuse the prepared generation; cross-Run reuse requires
a configured Session Store

When the original predicted total exceeds the configured context window,
Runtime adopts a fitting prepared or newly compiled candidate through
`context.compacted`. It refuses Provider I/O if no candidate fits. Soft
preparation failure remains non-terminal while the original prediction is
below the hard limit. Any separately published ordinary compaction clears an
older prepared candidate

`PredictiveBudgetPlan` is content-free and appears on preparation, adoption,
and `model.request.started` Events, plus the terminal Run result. It records the
original, candidate, and Provider estimates, estimator kind, reserves, soft and
hard limits, level, action, and candidate generation. Event replay validates
its arithmetic, Checkpoint transition, exact Ledger prefix, and Evidence
references. Provider Usage remains authoritative for prediction-error
analysis. With action `none`, the candidate fields mirror the original and no
generation exists. A hard unadopted plan appears only on the failed Run result
and never reaches a Provider

Evidence Objects are private content. Configured Context compilers bind each
new reference to an opaque digest of tenant, Session or ephemeral Run, and
execution Profile, plus the required retrieval capabilities and sensitivity.
The content digest identifies bytes but never grants access. A follow-up Run
with the same Session and Profile can reuse its objects; an ephemeral Run or a
different tenant or Profile cannot

The built-in JSON Store writes scoped objects below `scoped-objects/` by binding
digest using mode `0600`, bounded reads, atomic rename, and content-digest
verification. Individual objects are limited to 64 MiB. `private` content is
accepted. `secret` content is rejected before persistence because the built-in
Store does not encrypt at rest; an embedding host may supply a scoped Store
adapter that does

Every valid scoped put and retrieval attempt appends a content-free record to
the protected `object-access.jsonl` log. It contains digests, operation,
outcome, size, and time, but no raw tenant, Session, Run, Profile, principal,
capability, or object content. An allowed retrieval fails closed if its audit
record cannot be written

Resolve the complete scoped reference from its Run Bundle and perform an
audited local administrative read with

```sh
qed evidence fetch sha256:<digest> --run-id <run-id> --store .qed/evidence
```

The command without `--run-id` remains available for legacy unscoped objects
below `objects/`. A scoped reference cannot be read through the legacy Object
Store API

## Built-in Context retrieval Tools

Retrieval is opt-in through `agent.Options.ContextRetrieval` or the declarative
Agent `context.retrieval` object. Enabling it registers five portable
Provider-facing Tool names

| Tool | Bounded result |
| --- | --- |
| `context_search` | Exact newest-first matching by default, or explainable relevance ranking over a frozen bounded Event prefix, with source references and bounded snippets |
| `context_fetch` | One UTF-8 chunk from a scoped Evidence Object already referenced by the current Run or Session |
| `session_timeline` | Content-free Event identities and activity metadata, newest first |
| `artifact_history` | Immutable Artifact Ledger entries, newest first |
| `execution_history` | Provider and Tool execution Ledger entries without argument or output text, newest first |

Lists and default search use a numeric `cursor` over their current Event or
Ledger snapshot and return `next_cursor`. Relevance search freezes the accepted
Event prefix on its first page, returns `snapshot_event_count`, and uses
`next_cursor` as the ranking offset. Later pages must repeat that snapshot,
`snapshot_query_digest`, and the same query. Runtime rejects a missing or
mismatched binding, so retrieval Tool Events appended in between cannot shift
the result set. Fetch uses byte `offset` and returns `next_offset`, both
restricted to UTF-8 boundaries. Raw snippets and fetched content are marked
`untrusted: true`; they are historical data, not executable instructions

Relevance results expose normalized task, file, symbol, active Constraint,
unresolved error, recency, prior-reference frequency, optional semantic, and
token-cost factors plus a deterministic weighted total. Each result includes
the exact snippet byte count, estimated token count, and estimator kind. Runtime
considers at most the most recent `512` searchable Events, reports
`candidate_pool_truncated` when more exist, and analyzes at most `16384` bytes
per Event. Active Constraint text is limited to the newest `128` Facts, while
reference-frequency analysis inspects at most `64` prior successful search
results and skips an individual result larger than `262144` bytes. The response
reports `constraint_pool_truncated` or `reference_history_truncated` when those
signal pools are incomplete. A host may inject `ContextSemanticScorer`; Runtime
passes at most `512` bounded untrusted excerpts and validates one `0..1000`
score per item. Embedding is not required, selected by declarative
configuration, or called by default exact search

Each Run independently bounds attempted calls, successful returned items, and
complete successful JSON output bytes. Per-call item and output-byte limits
apply inside those Run totals, and Runtime's normal Tool-call limit still
applies. A limit, malformed cursor, unavailable Store, unsupported media type,
or access denial is a normal bounded error Tool result so the model may recover
without failing the Run

`context_fetch` first resolves the requested digest against complete scoped
references in accepted `context.compaction.prepared` or `context.compacted`
Events. A digest absent from that history never reaches the Object Store. The
Store then authorizes tenant,
Session or ephemeral Run, Profile, and required capabilities and records the
access attempt. Only valid UTF-8 text media types are returned
Runtime also verifies the returned byte length and SHA-256 digest against the
complete scoped reference before exposing content

Every built-in retrieval result carries content-free
`ToolResult.ContextRetrieval` metadata in the corresponding `tool.completed`
Event. Session replay preserves operation, outcome, counts, truncation,
optional object digest, and post-compaction status. Runtime omits that metadata
from the model-facing Tool Message

Configured Runtimes do not infer a scope for Checkpoints created with legacy
unscoped references. Such a Session must start again under the scoped
configuration; silently rebinding old content would bypass the new isolation
boundary

## Prefix Manifest

Every `model.request.started` Event contains a `prefix_manifest` with Provider,
model, optional hashed Cache Family, Prefix Epoch, and ordered Segment
fingerprints. It contains no prompt text, but its hashes and sizes are still
content-derived metadata and must be protected

The Prefix Epoch is an observability digest, not a Provider cache key. Provider
adapters may render the same logical request differently, and the Provider is
the authority on actual cache reuse

The JSONL Session Store writes each Manifest as a common-prefix count plus the
changed suffix. Loading reconstructs the complete public Event, including Event
Logs written by the earlier full-Manifest format. This prevents append-only
Session persistence from duplicating the complete growing Manifest on every
turn

## Cache capabilities and plans

Providers expose `agent.CacheCapabilities`; `agent.DefaultCachePlanner` combines
them with `agent.CachePolicy`

| Mode | Behavior |
| --- | --- |
| empty or `disabled` | no QED cache control is sent |
| `adaptive` | prefer explicit caching, then safely fall back to automatic or disabled |
| `automatic` | request Provider automatic caching when supported |
| `explicit` | select the longest eligible user-message boundary and write marker |

`required: true` converts an unsupported requested mode, TTL, or explicit
boundary into a pre-call error. Otherwise the Plan records a content-free
`fallback_reason` and continues in a supported mode

Cache Family IDs are domain-separated SHA-256 digests over Provider, model,
Agent, Session or Run scope, and optional host `family` and `isolation_key`
inputs. Raw isolation values are never persisted or sent. Embedding hosts must
set an isolation key when identically named Sessions may exist in different
tenant domains

Current built-in adapter behavior is

| Adapter | QED controls |
| --- | --- |
| Official OpenAI Responses or Chat Completions, `gpt-5.6*` or detected later GPT family | automatic caching, `prompt_cache_key`, explicit content breakpoint, `prompt_cache_options`, `30m` TTL |
| Earlier official OpenAI models | automatic caching and `prompt_cache_key`; no QED retention override |
| Custom OpenAI-compatible endpoint | disabled unless `cache_capabilities` is declared |
| Official Anthropic Messages | automatic or explicit `cache_control`, model-aware minimum, `5m` and `1h` TTL |
| Custom Anthropic-compatible endpoint | disabled unless `cache_capabilities` is declared |
| ChatGPT Codex backend | observed automatic caching only; no new cache request fields |
| Echo | unsupported |

Unknown official Anthropic model IDs use a conservative 4,096-token minimum
until the adapter is updated or a trusted capability override is configured

The OpenAI explicit mapping follows the current
[OpenAI prompt caching guide](https://developers.openai.com/api/docs/guides/prompt-caching)
and is intentionally model-gated because older models reject the new fields
The Anthropic mapping follows the current
[Anthropic prompt caching guide](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)

Custom endpoints may declare narrower or broader capabilities in configuration
when their trusted API documentation supports the corresponding wire fields

## Cost forecast

QED contains no model price table. A Provider profile may inject integer rates
as currency micros per one million tokens. With complete uncached, cache-read,
and cache-write rates, the Planner estimates

```text
without cache = expected uses * uncached prefix cost
with cache    = one write + subsequent reads
```

This v0 forecast covers the estimated reusable prefix only. It does not predict
output, volatile suffix, retries, task success, or retrieval cost. A non-positive
explicit-cache saving falls back unless the cache policy is required

`qed cache status [run-id] --store .qed/evidence` reads the latest stored Plan,
latest input-estimate comparison, normalized Usage, cache-read ratio, forecast,
pricing-derived actual estimate, first Prefix divergence, and latest compaction
report. Omitting the Run ID selects the newest Evidence Bundle

## Usage normalization

When `input_token_details_reported` is true, QED enforces

```text
input_tokens =
  uncached_input_tokens
  + cache_read_input_tokens
  + cache_write_input_tokens
```

OpenAI Responses and Chat Completions map their input-token detail fields
Anthropic total input is the sum of normal input, cache creation, and cache read
The experimental ChatGPT Codex backend is read on a best-effort basis

Run-level Usage aggregates cache categories only when every Provider call
reported a complete breakdown. Per-message Usage keeps each individual result

## Current limits

- deterministic Ledgers cover explicit Fact lifecycle and other
  Runtime-observable state, but canonical workspace reconstruction and
  model-based semantic verification do not exist yet
- Runtime currently rebuilds a Ledger from the complete Event prefix before
  every Compiler call; no incremental reducer index exists yet
- Predictive model limits and reserves are operator-supplied facts; QED does not
  discover or refresh a model context window from a Provider catalog
- soft candidate preparation currently runs synchronously at the request
  boundary rather than on a background worker
- `context_search` exact mode scans the accepted Event prefix; relevance mode
  rebuilds its Ledger from the fixed complete prefix and bounds only the newest
  candidate and signal-analysis pools; no retrieval index or automatic
  retrieval policy exists
- QED has no built-in tokenizer-backed estimator or embedding implementation;
  canonical bytes divided by four is the dependency-free token fallback
- `context_fetch` verifies the complete scoped Object before returning a bounded
  chunk; the current Store contract has no ranged-read operation
- Cache Plans select one user-message breakpoint rather than multiple
  stability-layer breakpoints
- no rendered-wire Prefix Manifest, cache compare/explain command, keepalive,
  singleflight warmup, or fleet coordination exists
- pricing and Provider capabilities are operator-supplied facts and can become
  stale; verify them against current Provider documentation
