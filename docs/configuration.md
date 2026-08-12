# Agent configuration

QED constructs Provider profiles, process-isolated Extensions, execution
Profiles, Stores, and an Agent graph from one strict JSON document

The format is used by `qed run`, `qed tui`, and `qed session resume`

## Complete example

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
        "recent_messages": 12,
        "rebase_generation_interval": 4,
        "predictive_budget": {
          "context_window_tokens": 128000,
          "output_reserve_tokens": 8192,
          "safety_margin_tokens": 4096,
          "predicted_tool_output_tokens": 4096,
          "soft_threshold_tokens": 102400
        },
        "retrieval": {
          "max_calls_per_run": 16,
          "max_items_per_call": 32,
          "max_items_per_run": 128,
          "max_output_bytes_per_call": 65536,
          "max_output_bytes_per_run": 262144
        }
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
    "path": ".qed/evidence",
    "isolation_key": "tenant-a"
  },
  "extension_state": {
    "store": "json",
    "path": ".qed/extension-state"
  }
}
```

Set the referenced credentials, then run the default Agent

```sh
export PRIMARY_API_TOKEN="<token>"
export REVIEW_API_TOKEN="<token>"
go run ./cmd/qed run \
  --config ./qed.json \
  --workspace . \
  --session-id review-1 \
  --prompt "Propose a migration plan"
```

## Top-level fields

| Field | Required | Meaning |
| --- | --- | --- |
| `version` | yes | Must be `1` |
| `default_agent` | no | Agent selected when `--agent` is omitted |
| `limits` | no | Shared orchestration limits |
| `providers` | yes | Named Provider profiles |
| `extensions` | when referenced | Explicit Extension process definitions |
| `extension_directories` | no | Directories recursively searched for external manifests |
| `profiles` | no | Named execution Profiles |
| `agents` | yes | Named Agent definitions |
| `session` | no | Memory or JSONL Session Store |
| `evidence` | no | JSON Evidence Bundle, scoped Object, and access-audit Store |
| `extension_state` | no | Memory or JSON Extension State Store |

If `default_agent` is absent, the CLI requires `--agent`

All relative paths are resolved against the directory containing the JSON file,
except `--workspace`, which is an Adapter input and defaults to the current
directory

## Provider profiles

A Provider profile keeps protocol, endpoint, model, credential source, and
Provider options together. Separate profiles can use different API dialects or
different endpoints of the same dialect

| Field | Required | Meaning |
| --- | --- | --- |
| `protocol` | yes | `echo`, `openai-responses`, `openai-chat`, `openai-codex`, or `anthropic` |
| `base_url` | no | Trusted API base URL; omit for the official endpoint |
| `model` | model Providers | Exact model identifier |
| `token_env` | API-key Providers at official endpoints | Environment variable containing the credential |
| `auth_profile` | `openai-codex` | Named ChatGPT credential profile |
| `max_output_tokens` | no | Output limit; `0` selects Provider behavior |
| `api_version` | no | Anthropic API version override only |
| `pricing` | no | Host-supplied rates for forecasting and usage-cost estimates |
| `cache_capabilities` | no | Trusted override for the configured endpoint and model |
| `rate_limit` | no | QED-side outbound concurrency policy for this profile |

`echo` accepts no endpoint, model, credential, or API options. It may use the
QED-side `rate_limit` policy for deterministic concurrency tests

A custom endpoint may omit `token_env` for an unauthenticated local service.
Configuration never falls back to `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, or
`QED_API_KEY`. When `token_env` is present, the credential is resolved for each
HTTP request, which permits rotation by an embedding host

The Provider profile ID is part of Provider identity and opaque continuation
state, preventing state from one endpoint/profile from being reused by another

`openai-codex` reads a separately stored ChatGPT OAuth profile and uses the
fixed ChatGPT Codex backend. It accepts `protocol`, `model`, `auth_profile`, and
optional `pricing` and `rate_limit`; `base_url`, `token_env`,
`max_output_tokens`, `api_version`, and `cache_capabilities` are rejected. The
named profile must already exist when configuration is loaded

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

See [ChatGPT subscription authentication](chatgpt-auth.md) for credential
storage, refresh, and protocol limitations

### Outbound Provider rate control

`rate_limit` is configured per Provider profile

| Field | Required | Meaning and default |
| --- | --- | --- |
| `max_concurrency` | no | Maximum active Provider streams; `0` or omission selects `4`, otherwise range `1` through `1024` |

Every Agent that references the same profile shares one limiter, including
concurrent top-level `Host` Runs and parallel subagents. Different profiles do
not share capacity or cooldown state, even when they target the same account or
endpoint, because QED cannot infer an upstream shared-limit bucket safely

A `rate_limited` response updates the profile-wide cooldown with the effective
retry delay. `Retry-After` remains the minimum; fallback exponential backoff
and a small bounded per-Run jitter prevent concentrated retries. A queued Run
honors cancellation and Deadline and does not consume a Provider call budget
until it acquires capacity and is ready to start an actual attempt

`max_concurrency` is a local protective bound, not an RPM or token-rate
guarantee. The Runtime-local call limit and the orchestration-wide Agent Run
and Provider call limits remain independent hard bounds

## Extension definitions

One execution Profile may reference multiple Extensions. Definitions support
three startup modes

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

Self-exec starts the current Host executable with its hidden Extension
entrypoint. Available IDs come from the catalog generated from the Host's
`extensions.lock`; the official QED executable currently selects
`qed.workspace`, `qed.process`, and `qed.git`. An embedding application supplies
its own Catalog through `qed.HostLoadOptions`. The Host validates the locked
declaration against the live process in the same way as an external manifest.
This mode rejects `command` and `manifest`

Regenerate the catalog after changing `extensions.lock`, or check that the
checked-in source is current

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

`command[0]` and `directory` are resolved relative to the configuration file.
Arguments are passed directly without shell evaluation. `environment` names
the complete selected environment available to the Extension process

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

Manifest mode resolves the validated entrypoint and requires Handshake and
Describe identity, version, capabilities, Hooks, and Commands to match the
external declaration. It rejects `command` and `directory`

### Discovery

```json
{
  "extension_directories": ["./extensions"]
}
```

Discovery recursively finds `qed-extension.json`, skips directory symlinks,
and rejects duplicate IDs. Discovered Extensions use their manifest directory
as the child working directory and receive no environment or configuration by
default. An ID cannot be both explicit and discovered

Extension process environment and Tool command environment are separate.
`extensions.<id>.environment` reaches the Extension executable.
`profiles.<id>.environment` is sent to every selected Extension through
Initialize. The QED catalog's `qed.process` and `qed.git` Extensions use it for
commands, while `qed.workspace` ignores it. Do not select Provider credential
variables for either environment

See [Extension processes](extensions.md) for the manifest and lifecycle

Declarative Coding Profiles use `host.DefaultRestartPolicy` for every selected
Extension. Restart policy is not a version 1 JSON field. Programmatic Coding
Profiles can provide `Options.ExtensionRestartPolicy`; nil selects the default,
while a pointer to the zero policy disables automatic restart

## Execution Profiles

Version 1 supports `kind: "coding"`

| Field | Required | Meaning |
| --- | --- | --- |
| `kind` | yes | Currently `coding` |
| `extensions` | yes | One or more Extension IDs, acquired atomically for each Run |
| `capabilities` | yes | Static `allow`, `ask`, and `deny` lists |
| `environment` | no | Selected environment passed through Initialize |

Capability names must be valid and may come from external Extensions. The
official Workspace, Process, and Git Extensions use `filesystem.read`, `filesystem.write`,
`filesystem.delete`, `process.execute`, and `git.read`. Duplicate or conflicting
rules are rejected and unspecified capabilities are denied

An `ask` decision calls a host Approver. `qed run` defaults to
`--approval deny`; `--approval prompt` creates a durable wait and reads a
serialized yes-or-no answer from stdin. Prompt diagnostics include Tool name
and capabilities, not raw Tool arguments

Every selected environment name must exist. Executable lookup uses only the
selected `PATH` and does not fall back to the Host environment

A declarative Coding Profile also installs its default Current World State
Source on every referencing Agent. Canonical file and Git reads obey the same
Profile Policy and optional Run capability restriction. Background capture
does not request approval for an `ask` outcome. See
[Coding Profile](coding-profile.md#current-world-state) for scope and limits

## Agent definitions and delegation

| Field | Required | Meaning |
| --- | --- | --- |
| `provider` | yes | Provider profile ID |
| `profile` | no | Execution Profile ID |
| `instructions` | no | Base instructions for this Agent |
| `max_provider_calls` | no | Runtime-local Provider call limit |
| `max_tool_calls` | no | Runtime-local Tool call limit |
| `provider_retry` | no | Bounded retry policy for transient Provider failures |
| `context` | no | Evidence-preserving context compression policy |
| `cache` | no | Provider-neutral prompt-cache policy |
| `delegations` | no | Subagent Tools exposed to this Agent |

Delegation fields

| Field | Required | Meaning |
| --- | --- | --- |
| `name` | yes | Parent model-facing Tool name |
| `description` | no | Tool description |
| `strategy` | yes | `delegate`, `collect`, `select`, or `consensus` |
| `agents` | yes | Fixed candidate Agent IDs |
| `judge` | select and consensus | Reduction Agent ID |
| `instructions` | no | Additional candidate instructions |
| `judge_instructions` | no | Additional judge instructions |

Candidates execute concurrently in requested Agent order. The loader rejects
direct and indirect delegation cycles. A Subagent Tool passes only its explicit
prompt, not the parent's full conversation, Session ID, or Metadata

Each successful candidate returns a `result_packet` beside its text output and
Run linkage. The packet is versioned, content-addressed, and bound to the
candidate's terminal Context Ledger. A Profile reducer may add typed Facts,
Artifacts, Executions, Evidence references, and bounded Profile state. QED
validates source Event membership and exact identities before the parent sees
the packet. Coding Profiles install their reducer automatically; an Agent with
no Profile uses the deterministic Ledger reducer

Facts and Profile state remain untrusted model-visible content. Evidence
references retain the child scope and do not authorize parent retrieval. A
reducer error or invalid packet makes that candidate unsuccessful without
discarding other successful candidates

Shared limits default to 16 Agent Runs, depth 4, and 64 Provider calls. Parent,
candidate, and judge Runs count against the same top-level budget

## Provider retry

Provider retry is configured per Agent

| Field | Required | Meaning and default |
| --- | --- | --- |
| `max_attempts` | no | Total attempts for one logical model request, default `3`; use `1` to disable retry |
| `initial_backoff` | no | Positive Go duration used after the first failure, default `1s` |
| `max_backoff` | no | Positive Go duration capping exponential fallback delay, default `8s` |

QED retries only `retryable` and `rate_limited` failures. A valid
`Retry-After` response header is a minimum delay and may exceed `max_backoff`.
QED adds a small bounded per-Run jitter to the effective delay. All attempts
consume the Runtime-local and shared Provider call budgets and respect Run
cancellation and Deadline

Automatic retry is limited to failures before the first observable
`ModelStream` item. QED does not retry after a text delta or completed message,
so retry cannot duplicate published output or Tool side effects. The ordered
Event stream emits `provider.retry.scheduled` before each delay. See
[Provider errors and retry](providers.md#provider-errors-and-retry) for the
public error codes and Event fields

## Context compression and prompt caching

Context compression is configured per Agent and requires a JSON Evidence Store
because QED stores both exact compacted message prefixes and externalized Tool
output as content-addressed objects

| Context field | Required | Meaning and default |
| --- | --- | --- |
| `max_input_bytes` | yes | Hard canonical logical-input byte limit |
| `recent_messages` | no | Preferred raw tail and hierarchical Episode lengths, default `12` |
| `evidence_threshold_bytes` | no | Externalize Tool output at this size, default `16384` |
| `evidence_excerpt_bytes` | no | Retain this many bytes from both ends, default `2048` |
| `checkpoint_max_bytes` | no | Maximum encoded Checkpoint size, default `8192` |
| `rebase_generation_interval` | no | Rebuild without the previous semantic Checkpoint after this many newer generations, default `4`, maximum `64` |
| `evidence_sensitivity` | no | `private` or `secret`, default `private`; the built-in JSON Store rejects `secret` because it is not encrypted |
| `predictive_budget` | no | Model-context preflight, soft preparation, and hard adoption policy described below |

`predictive_budget` is optional. Its required values are operator-supplied model
or workload facts and have no implicit defaults

| Predictive Budget field | Required | Meaning |
| --- | --- | --- |
| `context_window_tokens` | yes | Complete model input and output context limit |
| `output_reserve_tokens` | yes | Reserved capacity for the next model response; keep it at least as large as the effective Provider output limit |
| `safety_margin_tokens` | no | Alternate reserve for estimate error, default `0`; QED uses the larger of this and `output_reserve_tokens` |
| `predicted_tool_output_tokens` | no | Capacity reserved for the next likely Tool result, default `0` |
| `soft_threshold_tokens` | yes | Predicted total at which QED prepares a validated inactive Checkpoint; it must exceed fixed reserves and remain below the context window |

The optional `retrieval` object registers five Runtime-owned read-only Tools:
`context_search`, `context_fetch`, `session_timeline`, `artifact_history`, and
`execution_history`. It requires the same scoped JSON Evidence Store as Context
compression. Omitting `retrieval` registers none of these Tools

| Retrieval field | Required | Meaning and default |
| --- | --- | --- |
| `max_calls_per_run` | no | All attempted retrieval Tool calls, default `16` |
| `max_items_per_call` | no | Records returned by one search or list call, default `32` |
| `max_items_per_run` | no | Records returned across one Run, default `128` |
| `max_output_bytes_per_call` | no | Complete successful JSON Tool output bytes per call, default `65536`, minimum `1024` |
| `max_output_bytes_per_run` | no | Complete successful JSON Tool output bytes across one Run, default `262144` |

The per-Run limits reset for a terminal follow-up Run. Runtime's ordinary
`max_tool_calls` limit still applies in addition to these retrieval limits

`context_search` preserves exact case-insensitive, newest-first matching when
`order` is omitted or is `recency`. Set `order` to `relevance` for an
explainable bounded ranking over task, file, symbol, active Constraint,
unresolved error, recency, prior-reference frequency, and resolved token-cost
signals. The first ranked response returns `snapshot_event_count` and
`snapshot_query_digest`. Repeat both values, the same query, and `next_cursor`
for every later page. Runtime rejects a missing or mismatched snapshot binding,
so newly appended Events cannot move the result set.
`candidate_pool_truncated: true` means the bounded recent candidate pool was
incomplete and the query should be narrowed. `constraint_pool_truncated` and
`reference_history_truncated` report incomplete active-Constraint and prior
search-reference signal pools; they do not make the returned ranking page
unstable

Declarative configuration always provides deterministic relevance factors and
does not select an embedding service. An embedding application may add one
normalized semantic factor with `qed.HostLoadOptions.ContextSemanticScorer` or
`agent.ContextRetrievalOptions.SemanticScorer`. The host owns external data
disclosure, credentials, cost, and scorer determinism

Declarative configuration also does not select a tokenizer. An embedding host
may set `qed.HostLoadOptions.TokenEstimator`; programmatic Runtime users set
`agent.Options.TokenEstimator`. The explicit host value takes precedence over a
Provider that implements `agent.TokenEstimator`. With neither, QED uses the
deterministic `canonical_bytes_div_4` fallback. Context Segment fingerprints,
Cache Plans, and relevance results record the stable estimator kind and count.
The kind must match `[a-z0-9][a-z0-9._/:-]{0,127}`.

Configured estimator calls are not Provider attempts and Runtime does not retry
them. A Context compilation estimate failure stops before the Provider call; a
relevance estimate failure becomes a normal bounded Tool error. The host owns
external disclosure, credentials, rate limits, cost, and determinism

`max_input_bytes` remains a deterministic Provider-neutral hard boundary and is
independent of the tokenizer-backed Predictive Budget. QED never rewrites raw
Session messages. It compiles a validated Checkpoint followed by a recent raw
tail, and stops before a Provider call when no safe Tool, approval, subagent,
edit-verification, or commit transaction boundary fits either hard limit

Predictive preflight evaluates `input estimate + predicted Tool output +
max(output reserve, safety margin)`. At the soft threshold QED asks the
configured compacting compiler for a candidate below the soft threshold and
persists it as `context.compaction.prepared` without changing the Provider
request. When the original prediction exceeds `context_window_tokens`, QED
adopts a fitting validated candidate with `context.compacted`. If no fitting
candidate exists, the Run stops before Provider I/O. `model.request.started`
and `RunResult.PredictiveBudget` expose content-free levels, actions, original,
candidate, and Provider estimates, reserves, limits, and candidate generation.
A soft preparation failure remains
non-terminal because the original request is still below the hard limit

The first Checkpoint and every configured Raw Event Rebase are rebuilt without
the previous semantic Checkpoint. Explicit Fact lifecycle changes and a
Checkpoint Fact retired by the current Ledger also trigger a Rebase. The
`context.compacted` Event reports `rebased` and `rebase_reason`

Each new Checkpoint candidate reports deterministic preservation counts in
`context_compaction.validation`. QED does not shrink away active Constraint
Facts or required Evidence to satisfy `checkpoint_max_bytes`. An undersized
limit can therefore retain the prior validated Checkpoint and raw tail with a
recorded rollback, or stop before Provider I/O when no validated view fits

Prompt-cache control is disabled when `cache` is omitted or `mode` is empty or
`disabled`. Provider-side implicit behavior may still occur independently

| Cache field | Required | Meaning and default |
| --- | --- | --- |
| `mode` | yes | `disabled`, `adaptive`, `automatic`, or `explicit` |
| `ttl` | no | Requested Provider lifetime such as `5m`, `30m`, or `1h` |
| `expected_reuse` | no | Expected total prefix uses, default `2` |
| `required` | no | Fail instead of falling back from an unsupported request |
| `isolation_key` | no | Host isolation label included only in a hashed family ID |
| `family` | no | Host sharing label included only in a hashed family ID |

`adaptive` prefers an explicit breakpoint, then automatic caching, then a
disabled Plan. A Cache Family also includes Provider, model, Agent, and Session
ID, or Run ID when no Session exists. Raw `isolation_key` and `family` values are
not persisted in Events or sent to a Provider

Operator-supplied pricing is never inferred from a model name

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

Rates are millionths of `currency` per one million tokens. The three input
rates must all be positive when `pricing` is present; the output rate may be
zero. These numbers are illustrative, not current Provider prices

A trusted custom endpoint may declare the capability facts its adapter can
render

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

Do not declare fields that the selected wire adapter cannot render. See
[Context compilation, compression, and prompt caching](context-caching.md) for
built-in endpoint and model detection, wire mappings, and current limits

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

Memory Sessions are process-local. JSONL Sessions are append-only, use private
files and revision locks, preserve provider-private continuation state, and can
be resumed by a later CLI process. Prefix Manifests are stored as a common
prefix plus changed suffix and reconstructed on load; the earlier full-Manifest
record format remains readable

`--session-id` requires a configured Session Store. The Runtime appends public
Events and reconstructs messages, pending waits, and pending Tool calls. Use
`qed session resume <id> --config <path>` for a persisted wait. Approval resume
accepts `--approval prompt|approve|deny`; other wait kinds require
`--response-json`

Active-Run steering keeps the existing `user.message.added` Event type and sets
`Event.UserMessageOrigin` to `steering`. Queue acceptance is process-local; the
Event is the durable Session boundary. Steering that has not emitted that Event
may be discarded by cancellation, deadline expiry, or terminal Run
failure

Fact lifecycle uses the same append-only boundary. Runtime moves an explicit
input `Message.FactDirective` to `Event.FactDirective`; Session messages and
Provider requests retain only the user text. Memory and JSONL Stores preserve
the directive Event, so replay reconstructs the same active, superseded, and
resolved Constraint Facts. The Ledger is derived and is not stored separately

A follow-up is a new Run started with the same Session ID only after the
previous handle reaches a terminal result. It replays the Session but receives
a new Run ID and new Runtime-local Provider and Tool limits. Session Stores do
not persist `agent.Budget`; reuse the same `*agent.Budget` explicitly when one
budget must span follow-up Runs. Without a configured Session Store, Session ID
does not retain messages and the caller must supply prior context

## Evidence Store

```json
{
  "evidence": {
    "store": "json",
    "path": ".qed/evidence",
    "isolation_key": "tenant-a"
  }
}
```

| Field | Required | Meaning and default |
| --- | --- | --- |
| `store` | yes | Must be `json` |
| `path` | yes | Private Evidence Bundle, Object, and access-audit directory |
| `isolation_key` | no | Tenant or local isolation domain, default `local`; stored Object references contain only its derived scope digest |

Configured CLI Runs and every completed Run in a configured multi-turn TUI chat
save one versioned Bundle after terminal completion, including public Events,
usage, config/workspace digests, and host-owned Tool trace records. The same
Store keeps authorization-bound content-addressed objects used by context
compression. Object references bind tenant, Session or ephemeral Run,
execution Profile, required capabilities, and sensitivity without storing the
raw scope values. Inspect or export Bundles with either command family

Tool trace payloads are represented by digests, but public Events retain their
normal observable payload. A Bundle may therefore contain prompts, assistant
messages, Tool arguments, Tool output, wait payloads, and errors. Store it with
the same care as Session data

```sh
qed run inspect <run-id> --store .qed/evidence
qed run export <run-id> --store .qed/evidence
qed evidence inspect <run-id> --store .qed/evidence
qed evidence export <run-id> --store .qed/evidence
qed evidence fetch sha256:<digest> --run-id <run-id> --store .qed/evidence
qed cache status [run-id] --store .qed/evidence
qed context inspect <run-id> --store .qed/evidence
qed context explain RUN_ID[@EVENT_SEQUENCE] --store .qed/evidence
qed context diff --before RUN_ID[@EVENT_SEQUENCE] --after RUN_ID[@EVENT_SEQUENCE] --store .qed/evidence
```

`qed cache status` defaults to the newest Bundle and reports the effective
Plan, latest input-estimate comparison, normalized cache Usage, optional
forecast and usage-cost estimate, first Prefix divergence, and latest
compaction record. `agent.BuildTokenUsageReport` provides all per-attempt
comparisons to embedding applications without prompt content

The Context commands derive content-free timelines, aggregate metrics, and
before-to-after changes from the same stored public Events. They do not print
messages, paths, commands, Evidence object digests, or object content

Scoped Object access is recorded separately in `object-access.jsonl`. The log
contains protected digests and outcomes but no raw identity or content. The
fetch command uses `--run-id` to resolve the complete reference from a Bundle
and records an administrative read. Omitting `--run-id` supports legacy
unscoped objects only

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

The Host stores bounded opaque Snapshot values under Extension ID and a
workspace/Profile-derived scope. It restores state on startup, persists before
reload, and persists the current generation during orderly close. This Store
is separate from Agent Sessions and Evidence

## CLI scope and verbose diagnostics

`--config` conflicts with inline `--provider`, `--model`, `--base-url`,
`--auth-profile`, `--system`, and `--max-output-tokens`. `--agent`, `--workspace`, and
`--session-id` require `--config`. `--approval` is available on non-interactive
configured Runs; configured TUI approval uses `Y` and `N`

Place the root `--verbose` flag before the subcommand

```sh
qed --verbose run --config qed.json --prompt "inspect this project"
qed --verbose tui --config qed.json --prompt "inspect this project"
```

The boolean propagates to every configured Runtime and the final Extension
Server. Diagnostics are structured on stderr and exclude content-bearing or
secret values

## Strict parsing and compatibility

- maximum document size is 1 MiB
- unknown fields are rejected
- duplicate object keys are rejected
- trailing JSON values are rejected
- IDs, paths, environment names, references, and all graph edges are validated before use
- every referenced Extension starts and passes lifecycle validation before the graph is returned
- version 1 is experimental and may change without migration before the first stable release

Embedding applications load this schema with `qed.LoadHost`. A self-exec entry
requires the application's generated Catalog and absolute current executable in
`qed.HostLoadOptions`. Applications must call `Host.Close` or `CloseContext` to
drain and stop configured Extension processes. See [Embedding QED](embedding.md)
