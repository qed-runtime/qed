# Architecture

QED keeps the provider-neutral execution loop small and composes optional
capabilities around it. Coding is a Profile built above Runtime Core, not a
special case inside it

```text
CLI / TUI / embedded host
          |
          +--------- configuration -------- Provider profiles
          |                                      |
          v                                      v
    orchestration registry ---------------- Agent Runtime
          |                              Run + streaming Events
          |                                      |
          |                           Session Event Log
          |                                      |
          +-------------------------- ComponentSource
                                                 |
                                  atomic generation-set lease
                                                 |
                                  Tools + Hooks + Commands
                                                 |
                                  Host Policy + approval
                                      + Evidence recording
                                                 |
                                  framed JSON RPC over stdio
                                                 |
                             +-------------------+-------------------+
                             |                                       |
                      external executable                     self-exec child
```

## Package boundaries

`agent` owns provider-neutral Messages, model streams, Runs, active-Run
steering, budgets, ordered Events, Tool calls, waits, Hooks, Session contracts,
cancellation, and local execution limits. A `ProviderRateLimitController`
bounds active model streams and shares observed cooldowns without moving HTTP
error parsing into Runtime. It has no filesystem, Git, CLI, TUI, or Provider
wire-format behavior

Before every Provider call, `agent.ContextCompiler` produces a canonical
`ModelRequest` and logical Context Segments. Runtime persists a content-free
Prefix Manifest on `model.request.started`; Session and Evidence Stores retain
that Event. The default compiler stabilizes Tool order and JSON schemas. The
optional compacting compiler keeps raw Session messages immutable, stores exact
compacted prefixes and large Tool output in a content-addressed Evidence Object
Store, and publishes a validated typed Checkpoint plus recent raw tail. Runtime
also supplies an isolated copy of the exact Event prefix. The compiler validates
that prefix against the Ledger and derives safe cuts without relying on Tool
names in Runtime Core

The first Checkpoint is a Raw Event Rebase. Later generations may update from a
validated previous semantic view, while a deterministic generation interval,
an explicit Fact lifecycle change, or a Checkpoint Fact contradicted by the
current Ledger forces a rebuild with no previous Checkpoint. The Strategy still
receives the exact raw Messages, validated Events, and derived Ledger. Runtime
publishes the Rebase reason with `context.compacted` and persists the latest
Rebase generation in the Checkpoint

`agent.ToolResult.ContextOperation` carries a validated, content-free
classification for mutation, verification, commit, or subagent work. It never
enters the model-facing Tool Message and does not grant authority or prove a
Tool outcome. Safe-cut reconstruction protects every Tool Call and result batch,
keeps approval inside that transaction, and keeps a mutation with subsequent
work through its first verification, commit, or next user boundary

Before each Context Compiler call, Runtime reduces the complete ordered Session
and active-Run Event prefix into an `agent.ContextLedger`. Its five typed
ledgers describe only Runtime-observable artifacts, executions, explicit user
Fact lifecycle, authorization decisions, and Run tasks. A user Message creates
an active Constraint Fact unless the host explicitly marks earlier active Fact
IDs as superseded or resolved. The reducer never infers a transition from text.
It neither calls a model nor reads live filesystem or Git state. A custom
Compiler receives
an isolated Ledger copy, the terminal `RunResult` exposes the final generation,
and a compacted Checkpoint retains a content-free reference that replay verifies
against its exact preceding Event prefix

Runtime can also call an injected `agent.CurrentWorldStateSource` at each safe
logical Provider boundary. The Source reads canonical host state without
mutating it and returns a bounded snapshot tied to the exact preceding Ledger
generation. Runtime validates and publishes `current_world_state.captured`,
then supplies the snapshot as a required volatile Context Segment without
adding it to replayable conversation Messages. The Coding Profile implements
this host boundary with current workspace hashes, read-only Git status and
diff, and exact prior `run_command` outcomes; Runtime Core retains no
filesystem or Git implementation

`agent.CachePlanner` combines host policy with Provider capabilities after
compilation. QED cache controls are disabled by default. An enabled Plan carries
a hashed Cache Family, optional TTL and breakpoint, and optional host-priced
forecast. Provider adapters translate that Plan to their wire format and
normalize reported cache categories in `agent.Usage`

`provider/openai`, `provider/openaicodex`, and `provider/anthropic` translate
the common model to streaming HTTP APIs. The Codex package is deliberately a
separate dialect because it uses ChatGPT OAuth, a fixed backend, and additional
account-routing headers rather than an OpenAI API key. Provider-private
continuation state remains opaque and can be persisted by a Session Store
without being emitted in public JSON

`internal/chatauth` owns browser and device-code OAuth, named credential
profiles, refresh, and the private user-level credential file. It exposes a
credential source to `provider/openaicodex`; the Provider never owns persistent
tokens and retries only once after an authorization rejection

`orchestration` composes named Runtimes above `agent`. Each Runtime remains
bound to one Provider, so a parent and its subagents may use different endpoint,
credential, model, and protocol combinations without converting private state.
Declarative configuration shares one outbound rate controller between every
Runtime that references the same Provider profile

`session` implements the `agent.SessionStore` contract in memory and as an
append-only JSONL Event Log. Runtime serializes concurrent Runs for one Session
inside a process and Store revisions provide optimistic conflict detection
JSONL records delta-encode growing Prefix Manifests while loading both the
delta form and the earlier full form

`profile/coding` assembles bounded project context, one capability Policy, an
Evidence recorder, and one or more process-isolated Extensions. The reusable
`qed.workspace`, `qed.process`, and `qed.git` Extensions contribute its six
standard Tools while keeping them outside Runtime Core. The Profile also owns
the default read-only Current World State Source and applies the same Policy
and Run capability restriction before canonical reads

`extension.ToolProxy` and `extension.CommandProxy` are host enforcement points.
Tool input is schema-validated before invocation-specific capability discovery,
Policy, or approval. The proxies then combine capabilities, evaluate Policy,
request approval when required, and invoke the remote component only after
authorization. Tool Evidence is recorded in the Host

`extension/protocol` defines Protocol v2 as 4-byte big-endian length-prefixed
strict JSON over stdio. `extension/server` adapts Go Tools, Hooks, Commands, and
lifecycle callbacks to that contract and revalidates Tool input before direct
RPC calls reach component code. Protocol v2 adds Tool-result
`context_operation` metadata; exact version negotiation intentionally rejects
v1 peers. `extension/host` supervises processes and generation leases

`extension/manifest` validates the transport-independent declaration shared by
external and embedded Extensions, resolves distributable manifests, and
performs bounded recursive discovery. Public `extension/selfexec` validates
`extensions.lock`, generates standalone catalogs, launches child commands, and
dispatches the selected linked Server. QED's own generated catalog remains in
`internal/extensionregistry`, while another host repository owns an equivalent
generated package. `extension/reload` builds candidates, watches development
source, and exposes an authenticated local reload control endpoint

`evidence` builds versioned Bundles from a terminal Run, public Events, and
host-owned Tool traces. Tool trace fields use payload digests, while public
Events retain their observable payload for audit and replay. Evidence storage
must therefore be treated as content-bearing and potentially sensitive. Its
JSON Store also implements the content-addressed Object Store required by
context compression, with bounded reads and digest verification

`workspace` provides a canonical filesystem boundary and process-local locks
for official workspace-scoped Tools. Traversal-resistant `os.Root` operations and edit
preconditions protect file APIs from stale or escaping paths. They do not turn
child processes into an operating-system sandbox

The root `qed.Host` API is the transport-neutral embedding facade. It loads a
declarative Agent graph, owns configured Extension lifecycles, starts concurrent
Runs, drains Events, and persists Evidence when configured. HTTP, gRPC, queue,
authentication, and inbound client or tenant rate limiting remain
responsibilities of the embedding application. Outbound Provider concurrency
and observed cooldowns remain Runtime controls

`cmd/qed` and `internal/tuiapp` are adapters. `internal/extensionscaffold`
creates non-overwriting Go reference layouts without changing module or lock
files. `cmd/qed-extension-gen` is the dependency-light downstream catalog
generator. Nagi remains inside the QED CLI
frontend packages and no Nagi type crosses into Runtime, Provider, Extension,
or Host APIs. The TUI chat controller maps composer submissions to active-Run
steering or terminal follow-up Runs, keeps approval resume and Run cancellation
separate, and stores Evidence per Run rather than merging Run Event sequences

## Run and Event ordering

A Run acquires one immutable Component generation set before execution and
releases it after terminal completion or cancellation. Tool definitions and
Hooks therefore cannot change midway through a Run, even while another
generation reloads

For each candidate Event, Runtime performs this order

```text
assign Run identity and candidate sequence
  -> validate an explicit Fact transition against the Event prefix
  -> invoke matching synchronous Hooks
  -> append to the Session Store when configured
  -> publish to RunHandle.Events
```

A Hook error rejects that Event and fails the Run. In particular, rejection of
`run.completed` produces one `run.failed` terminal Event rather than persisting
two terminal states. Session revision is assigned by the Store after Hooks
succeed

`run.waiting` records the external input request. `RunHandle.Resume` emits
`run.resumed` and continues the in-memory Run. A later process can load a
persisted pending wait and resume its associated Tool call without repeating
the completed Provider request

`RunHandle.Steer` non-blockingly queues one plain, non-empty user Message in a
bounded FIFO. Steering never changes an in-flight Provider request or retry and
never interrupts a Tool batch. After the current assistant Message and all of
its Tool results are complete, or after an end-turn response, Runtime appends
queued steering as `user.message.added` Events before compiling the next
Provider request. These Events set `UserMessageOrigin` to `steering`; Event
publication is the observable point at which steering has entered Session
state. A queue acceptance alone is not a persistence acknowledgement

Fact lifecycle is host control metadata on user input. Runtime validates its
shape at submission, moves it from `Message.FactDirective` to
`Event.FactDirective`, and checks target existence and active state before Hooks
or persistence. `supersede` retires earlier targets and creates one active
replacement, while `resolve` retires targets without creating a Fact from the
resolution Message. Stored and Provider-facing conversation Messages do not
retain the directive. Ledger v2 records the raw message index, current state,
state source, transition sources, and both directions of supersedes edges;
replay also validates references emitted by Ledger v1

An observed `run.waiting` rejects new steering with `ErrRunWaiting`. Steering
queued before the wait remains pending until the matching resume and Tool
completion. Cancellation, deadline expiry, or a terminal Run failure
stops further application and may discard queued steering without an Event.
The Event stream is the authoritative applied-input record. Steering itself
does not consume Budget, while its next Provider or Tool work continues to use
the same Run Budget

A follow-up is a new Run started only after the previous handle reaches a
terminal result, using the same Session ID and configured Session Store. It
receives a new Run ID and local Run limits while replaying the Session's
messages. Without a Store, Session ID is identity only and the caller must
supply any prior context. A Budget spans both Runs only when the caller
explicitly reuses the same `*agent.Budget`. Resume, steering, and follow-up are
therefore separate operations

Before an outbound attempt, Runtime acquires Provider capacity, then charges
the Runtime-local and shared Provider call budgets, and only then emits
`model.request.started`. A queued attempt emits
`provider.rate_limit.waiting` without consuming call budget. A rate-limit
failure updates the shared cooldown before its active-stream permit is
released, preventing another waiting Run from racing through the observed
limit

## Configuration ownership

A Provider profile owns protocol, endpoint, model, output limit, credential
source, optional cache capability overrides, optional host-supplied pricing,
and an outbound rate-limit policy
An Extension definition owns its process startup. An execution Profile
references one or more Extensions and owns capability rules plus the selected
Tool-process environment. An Agent independently references a Provider, an
optional execution Profile, context compression, and cache policy

This split permits mixed-Provider Agent graphs and prevents Provider
credentials from becoming Extension environment by default. API-key values
and workspace absolute paths are supplied by the invoking host rather than
stored in portable JSON. ChatGPT tokens live in a separate permission-restricted
user credential file; portable configuration contains only its profile name

Session, Evidence, and Extension state are separate stores because their
lifetimes and disclosure risks differ. Extension state is namespaced by
Extension ID and a host-selected workspace/Profile scope

## Extension lifecycle

Initial startup must complete the following sequence before components become
available

```text
start process
  -> Handshake exact protocol and identity
  -> Initialize host-selected resources and verbose state
  -> Describe Tools, Hooks, Commands, and capabilities
  -> validate the locked or external manifest declaration when present
  -> HealthCheck
  -> publish generation
```

Reload builds or selects a separate executable, starts and validates it, takes
an opaque Snapshot from the old generation, persists it when configured,
Restores the candidate, and performs another HealthCheck. Only then does the
Host atomically publish the candidate for new Runs. Existing leases retain the
old process until it can Drain and Shutdown. Any pre-swap failure leaves the
old generation active

An enabled `RestartPolicy` makes Manager watch the published child process. An
unexpected exit fails requests already pinned to that generation without
replaying Tool calls, withdraws it from new leases, and starts replacement
candidates with bounded exponential backoff. Each candidate reuses the exact
validated `ProcessOptions`, including a locked manifest when configured,
restores the latest host-owned Snapshot, passes HealthCheck, and persists a new
Snapshot before publication when a State Store is configured. Only successful
publication advances the generation number

Candidates that fail startup and published generations that exit before the
stability window share one attempt count. Exhaustion opens the circuit until a
successful explicit Reload. New acquisition returns
`ErrExtensionRestarting` while recovery is active and
`ErrExtensionCircuitOpen` after exhaustion. Coding Profiles and the development
host use the bounded default of three attempts, 100 millisecond initial and 2
second maximum backoff, and a 30 second stability window. A direct Manager uses
the zero-value policy to disable restart

`GenerationSet` takes a read lock while acquiring one generation from every
configured Extension. Reload takes the corresponding write lock, so a Run sees
the complete set before or after a swap, never a partially acquired set

External manifests declare identity, implementation version, protocol version,
entrypoint, capabilities, Hooks, and Commands. Tool definitions remain dynamic
and are validated during Describe. The production loader requires a real
non-symlink entrypoint; development loading permits the build output to be
absent before the first build

`extensions.lock` selects Go packages for static linking and records the same
declaration without an external entrypoint. Generated self-exec entries and
external manifests both become `ProcessOptions.ExpectedManifest`, so the Host
does not branch on who supplied an Extension. The launcher differs, but
Handshake, Describe validation, lifecycle, Policy, and generation semantics do
not. Go dependency versions and checksums remain in `go.mod` and `go.sum`

## Diagnostics boundary

Verbose mode is a host-owned boolean carried through configuration,
`agent.Options`, `host.ProcessOptions`, and `InitializeRequest`. The Extension
Server activates stderr diagnostics only after it receives verbose Initialize

Host logs are structured and omit content-bearing fields. Child stderr is
bounded. Only records carrying QED's safe-debug marker and an allowlisted
message shape are forwarded as structured Host diagnostics; arbitrary child
stderr is never copied into verbose output

## Trade-offs

- Tool calls are sequential within one Run, preserving deterministic mutation
  and Evidence order; independent Agent Runs can execute concurrently
- Candidate and judge Runs share orchestration budgets, while token and cost
  limits depend on Provider-reported usage that may arrive only at response end
- Prefix Manifests describe QED's provider-neutral logical request, not the
  exact Provider-rendered prefix or an authoritative cache hit
- Context limits use canonical logical bytes and cache planning uses a
  deterministic bytes-divided-by-four estimate; neither substitutes for a
  model tokenizer or Provider-reported Usage
- Old and new Extension processes may overlap during retirement and do not
  share process-local Workspace locks, so edit digests remain the cross-process
  stale-write defense
- Automatic Extension restart never replays an interrupted Tool call and can
  restore only the latest Host-owned Snapshot, so state newer than that
  Snapshot may be lost after a crash
- `run_command` is deliberately broad and must be governed as a host permission
  or wrapped in an external sandbox
- Permission decisions belong to Host Policy and the optional Approver rather
  than an Extension, so a component cannot authorize its own invocation
- Tool Trace records hash raw payloads, but Bundle Events preserve public Run
  content for audit; an Evidence Store is not a secret-free telemetry sink or a
  complete workspace archive
- Multi-file patches are prevalidated and rolled back on ordinary failures but
  are not crash-atomic filesystem transactions
- JSONL and JSON stores are local implementations; multi-worker deployments
  need externally coordinated Store adapters
- ChatGPT OAuth follows an evolving Codex backend contract rather than the
  public OpenAI API contract; QED isolates that risk in `provider/openaicodex`
  and currently implements only the full SSE dialect
