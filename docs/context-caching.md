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
New Checkpoints contain a content-free `ContextLedgerReference`; replay of the
subsequent `context.compacted` Event verifies that reference against the exact
preceding Event prefix

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
- it injects the validated Checkpoint followed by the recent raw message tail

The Compiler never deletes or rewrites Session messages. A successfully
published Checkpoint emits `context.compacted`; `SessionSnapshot.Checkpoint`
holds the latest generation and the raw Event Log remains replayable

A custom `CheckpointStrategy` may create the semantic view. QED validates its
result and falls back to the deterministic strategy without exposing the
strategy error or message content in the fallback label. If no valid candidate
fits the configured hard limit, the Run stops before calling the Provider

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

`max_input_bytes` is a provider-neutral canonical byte limit, not a tokenizer or
the model's advertised context window. It is deliberately deterministic but
must be calibrated conservatively for the selected model. QED currently uses
canonical bytes divided by four only for cache-planning token estimates;
Provider Usage remains authoritative

Evidence Objects are private content. The JSON Evidence Store writes objects
below `objects/` using a SHA-256 name, mode `0600`, bounded reads, atomic rename,
and digest verification. Individual objects are limited to 64 MiB. Fetch an
exact object with

```sh
qed evidence fetch sha256:<digest> --store .qed/evidence
```

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
normalized Usage, cache-read ratio, forecast, pricing-derived actual estimate,
first Prefix divergence, and latest compaction report. Omitting the Run ID
selects the newest Evidence Bundle

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
- no tokenizer-backed context limit or predictive output reserve exists
- Evidence retrieval is CLI/API based and is not automatically exposed as a
  model Tool
- Cache Plans select one user-message breakpoint rather than multiple
  stability-layer breakpoints
- no rendered-wire Prefix Manifest, cache compare/explain command, keepalive,
  singleflight warmup, or fleet coordination exists
- pricing and Provider capabilities are operator-supplied facts and can become
  stale; verify them against current Provider documentation
