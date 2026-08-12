# Context compilation, compression, and prompt caching

QED treats persisted Session messages as an immutable source and compiles a
temporary model view before every Provider call. The same compilation produces
content-free Prefix observability, an optional semantic Checkpoint, and a
Provider-neutral Cache Plan

```text
immutable Session messages + Run input + pinned Tools
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

## Evidence-preserving compression

`agent.CompactingContextCompiler` adds a bounded model view above the immutable
raw messages

- it externalizes large Tool output into a content-addressed Evidence Object
- it selects a cut that does not split an assistant Tool Call from its results
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

- Checkpoints contain a deterministic compact semantic model, not full domain
  ledgers or model-based semantic verification
- no tokenizer-backed context limit or predictive output reserve exists
- Evidence retrieval is CLI/API based and is not automatically exposed as a
  model Tool
- Cache Plans select one user-message breakpoint rather than multiple
  stability-layer breakpoints
- no rendered-wire Prefix Manifest, cache compare/explain command, keepalive,
  singleflight warmup, or fleet coordination exists
- pricing and Provider capabilities are operator-supplied facts and can become
  stale; verify them against current Provider documentation
