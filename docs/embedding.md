# Embedding QED in another application

An application imports QED as a Go module. It does not fork the QED repository
to own its Agent graph or linked Extensions

The application repository owns

- its `go.mod` and `go.sum`
- its Agent configuration
- its `extensions.lock`
- its generated Extension catalog
- its HTTP, gRPC, queue, desktop, or job adapter
- its final executable and deployment policy

QED's top-level `extensions.lock` selects Extensions only for the official
`qed` executable

## Embedding layers

Use the lowest layer that matches the containing application

| Layer | Use |
| --- | --- |
| `agent.Runtime` | Construct one Provider-neutral Runtime directly |
| `orchestration.AgentRegistry` | Compose multiple named or delegated Runtimes |
| `qed.Host` | Load a complete Agent graph and own Profile, Store, and Extension lifecycle |

`qed.Host` is transport-neutral. It does not start a network listener or choose
authentication, authorization, inbound client or tenant rate limiting,
tenancy, or request schemas for the containing application. QED separately
enforces configured outbound Provider concurrency and cooldown policies

`HostLoadOptions.ToolInputValidator` replaces the default bounded JSON Schema
subset for every loaded Runtime and host-side Extension proxy. The validator is
not serialized across the process boundary. An external or self-exec Extension
that uses the same custom dialect must also set
`server.Options.ToolInputValidator`; otherwise its server applies the default
subset. See [Extension processes](extensions.md#tools) for the validation order
and supported keywords

## Application-owned self-exec catalog

A downstream application can link its Go Extensions without importing a QED
`internal` package

```text
company-agent/
├── go.mod
├── go.sum
├── qed.json
├── extensions.lock
├── extensionregistry/
│   ├── registry.go
│   └── registry_gen.go
└── cmd/company-agent/main.go
```

The application declares its generated package

```go
package extensionregistry

//go:generate go run github.com/qed-runtime/qed/cmd/qed-extension-gen --lock ../extensions.lock --output registry_gen.go --package extensionregistry --variable Catalog
```

Generate or check it without changing dependencies

```sh
go generate ./extensionregistry

go run github.com/qed-runtime/qed/cmd/qed-extension-gen \
  --lock extensions.lock \
  --output extensionregistry/registry_gen.go \
  --package extensionregistry \
  --variable Catalog \
  --check
```

The generated `Catalog` is a public `*selfexec.Catalog`. Each definition
contains the locked declaration and linked `func() server.Options` factory

## Process entrypoint

Dispatch child mode before parsing the application's ordinary command-line
arguments

```go
handled, err := extensionregistry.Catalog.Dispatch(ctx, selfexec.DispatchOptions{
    Arguments:   os.Args[1:],
    Input:       os.Stdin,
    Output:      os.Stdout,
    DebugWriter: os.Stderr,
})
if handled {
    return err
}
```

Normal host mode then loads the same catalog

```go
executable, err := os.Executable()
if err != nil {
    return err
}
executable, err = filepath.Abs(executable)
if err != nil {
    return err
}

host, err := qed.LoadHost("qed.json", qed.HostLoadOptions{
    LookupEnv:       os.LookupEnv,
    WorkspaceRoot:   workspaceRoot,
    SelfExecutable:  executable,
    SelfExecCatalog: extensionregistry.Catalog,
})
if err != nil {
    return err
}
defer host.Close()
```

An Extension configured with `"mode":"self-exec"` must exist in the supplied
catalog. `LoadHost` passes its locked declaration to the ordinary Extension
Host, so Handshake, Initialize, Describe, manifest validation, HealthCheck,
Policy, Evidence, generation leases, and shutdown remain identical to an
external executable

## Running from a server request

`Host` is safe for concurrent Runs after loading

All Runs loaded from one configuration share the limiter associated with each
Provider profile. This bounds concurrent outbound model streams across server
requests as well as subagents. The containing application must still apply its
own admission, tenant, cost, and request-rate policies before starting a Run

```go
outcome, err := host.Run(request.Context(), agent.RunRequest{
    AgentID:   input.AgentID,
    SessionID: input.SessionID,
    Input: []agent.Message{
        {Role: agent.RoleUser, Text: input.Prompt},
    },
}, func(ctx context.Context, handle *agent.RunHandle, event agent.Event) error {
    return publishEvent(ctx, event)
})
```

`Host.Run` drains the ordered Event stream, returns the terminal Result and all
Events, and saves an Evidence Bundle when configured. A handler error cancels
the Run. The handler receives the low-level handle so an in-process approval
adapter can resume a waiting Run or queue steering without blocking Event
drain

The terminal `RunResult.ContextLedger` is the deterministic five-Ledger view of
the accepted Event history. `agent.BuildContextLedger` rebuilds the same view
from ordered Events, while `agent.ValidateContextLedger` rejects changed
derived state. The Ledger is content-bearing because its Constraint entries
retain exact user text; store and transmit it with the same protection as
Session Events. A custom Context Compiler receives an isolated in-progress copy
through `ContextCompileRequest.Ledger`

A custom `CheckpointStrategy` receives an explicit `CheckpointRequest.Mode`,
target `Generation`, exact raw `Messages`, isolated `Events`, and the matching
Ledger. For `CheckpointBuildRawRebase`, `Previous` is always nil and
`RebaseReason` identifies the deterministic trigger. The Strategy must rebuild
from raw source instead of recursively summarizing its prior semantic output

## Scoped Evidence access

An embedding host that uses `CompactingContextCompiler` should configure a
scoped Object Store and Runtime identity together

```go
objects := evidence.NewMemoryObjectStore()
compiler, err := agent.NewCompactingContextCompiler(policy, objects, nil)
if err != nil {
    return err
}

runtime, err := agent.NewRuntime(agent.Options{
    Provider:        provider,
    ContextCompiler: compiler,
    EvidenceAccess: &agent.RuntimeEvidenceAccess{
        TenantID:     "tenant-a",
        ProfileID:    "coding",
        PrincipalID:  "company-agent",
        Capabilities: []string{
            agent.EvidenceReadCapability,
            agent.EvidenceWriteCapability,
        },
        Sensitivity: agent.EvidenceSensitivityPrivate,
    },
    ContextRetrieval: &agent.ContextRetrievalOptions{
        ObjectStore: objects,
        Limits: agent.ContextRetrievalLimits{
            MaxCallsPerRun:        16,
            MaxItemsPerCall:       32,
            MaxItemsPerRun:        128,
            MaxOutputBytesPerCall: 64 << 10,
            MaxOutputBytesPerRun:  256 << 10,
        },
    },
})
```

Runtime derives the concrete scope instead of accepting it from the model. A
Run with a Session ID uses tenant, Session, and Profile, so terminal follow-ups
share exact Evidence. A Run without a Session uses its generated Run ID and is
isolated from every other ephemeral Run. Subagents inherit the authenticated
tenant through context while deriving their own Run and Profile scope

For a multi-tenant server, leave `RuntimeEvidenceAccess.TenantID` empty and set
the authenticated tenant at the request boundary

```go
ctx := agent.WithEvidenceTenant(request.Context(), authenticatedTenantID)
handle, err := runtime.Run(ctx, runRequest)
```

If Runtime has a fixed tenant, a different contextual tenant is rejected with
`agent.ErrEvidenceAccessDenied`. A parent `EvidenceAccess` further restricts a
child Runtime to the intersection of parent and configured capabilities

`EvidenceObjectRef.Digest` is only content identity. Scoped retrieval requires
the complete opaque binding plus matching `EvidenceAccess`. Built-in Memory and
JSON Stores implement `ScopedEvidenceObjectStore`, reject `secret` content,
and record access attempts. An allowed retrieval returns no content when its
audit record cannot be committed. A trusted local operator may use the
optional `EvidenceObjectAdminStore`; that bypass is also audited

`ContextRetrieval` explicitly registers `context_search`, `context_fetch`,
`session_timeline`, `artifact_history`, and `execution_history`. A nil option
registers none of them. Search defaults to exact earlier Event text with
deterministic case-insensitive matching, newest first. `order: "relevance"`
ranks a bounded frozen Event prefix and returns the factor breakdown for every
snippet. Timeline and Ledger history return content-free metadata. Fetch
accepts a content digest only as a locator, resolves the full reference from
the current Run or Session Event history, and then requires the matching scoped
access before reading UTF-8 text. Runtime verifies the returned byte length and
SHA-256 digest against that complete reference

Every successful result is complete JSON bounded by per-call and per-Run item
and output-byte limits. Calls are also bounded per Run and still consume the
ordinary Runtime Tool-call budget. Lists and default search use newest-first
cursors. Relevance search returns `snapshot_event_count` and
`snapshot_query_digest`, requires both with the same query on later pages, and
uses `next_cursor` as an offset in that frozen ranking. Fetch uses a
UTF-8-boundary `next_offset`. Returned snippets and Evidence content carry
`untrusted: true` and must not be interpreted as instructions

An embedding host can augment the deterministic relevance score without
making embeddings a Runtime dependency

```go
runtime, err := agent.NewRuntime(agent.Options{
    Provider: provider,
    ContextRetrieval: &agent.ContextRetrievalOptions{
        ObjectStore:    objectStore,
        SemanticScorer: scorer,
    },
})
```

`agent.ContextSemanticScorer` receives the exact query, a bounded latest-task
prefix, and at most `agent.MaxContextSemanticCandidates` untrusted excerpts of
at most `agent.MaxContextSemanticCandidateBytes` each. It must return one
integer in the inclusive `0..1000` range per candidate, preserve order, honor
cancellation, and be safe for concurrent calls. Invalid output produces a
normal bounded Tool error. A scorer failure does not silently change ranking;
the model can retry with `recency`. The host must decide whether Session text
may be disclosed to an external scorer and owns its credentials, cost, and
determinism. `qed.HostLoadOptions.ContextSemanticScorer` wires the same scorer
into declaratively configured retrieval-enabled Agents. A scorer call runs
inside the Tool context but is not a Provider attempt and is not retried by
Runtime, so the host must also apply any external-call rate limit and usage
accounting it requires

An embedding host may also replace QED's dependency-free token approximation

```go
runtime, err := agent.NewRuntime(agent.Options{
    Provider:       provider,
    TokenEstimator: estimator,
})
```

`agent.TokenEstimator` receives one purpose-tagged batch of isolated byte
items and returns one non-negative count per item plus a stable, non-secret
kind. Implementations must preserve order, honor cancellation, be safe for
concurrent calls, not retain content or treat it as instructions, and return
the same result for the same Provider, Model, Purpose, and Content. Kind must
match `[a-z0-9][a-z0-9._/:-]{0,127}`. Runtime uses an
explicit `agent.Options.TokenEstimator` first, then a Provider that implements
the interface, then `agent.CanonicalByteTokenEstimator`. The fallback estimates
each non-empty item as `ceil(bytes / 4)` and requires no dependency

Built-in Context compilers apply the contract to canonical logical Segments;
relevance search applies it to bounded untrusted snippets. Custom Context
compilers receive the resolved estimator in `ContextCompileRequest`. Estimator
failures are not retried or silently replaced: compilation stops before the
Provider call, while retrieval returns a normal Tool error. An external
estimator has the same disclosure, credential, rate-limit, cost, and
determinism concerns as an external semantic scorer.
`qed.HostLoadOptions.TokenEstimator` wires the
host value into every declaratively configured Agent

`agent.BuildTokenUsageReport` reconstructs a content-free per-attempt report
from public Run Events. It pairs the Cache Plan estimate with Provider Usage,
reports `actual - estimate`, and preserves missing Usage as unreported. Token
estimates remain observational: `max_input_bytes` is still the deterministic
hard canonical-byte boundary until a predictive budget policy is configured

Runtime retains content-free `ToolResult.ContextRetrieval` metadata in
`tool.completed` Events and Session replay. It contains operation, outcome,
item and output-byte counts, truncation, optional object digest, and whether a
compaction preceded the call. It is not copied into the model-facing Tool
Message. Scoped fetch also creates the Object Store access audit record

An embedding host can inject canonical state independently of any Profile

```go
runtime, err := agent.NewRuntime(agent.Options{
    Provider:                provider,
    CurrentWorldStateSource: source,
})
```

`agent.CurrentWorldStateSource` receives isolated Run Events and the matching
Ledger immediately before a logical Provider request. A Source must perform
read-only bounded work and return structured file, Git, and observed-check
state. A Source error fails the Run before the Provider call. Runtime validates
and publishes `current_world_state.captured`; callers can verify a captured
value with `agent.ValidateCurrentWorldState`. When using `profile/coding`
directly, pass `codingProfile.CurrentWorldStateSource()` as shown in the Coding
Profile guide. Declarative `qed.Host` configuration wires it automatically

Constraint Facts use explicit lifecycle control rather than natural-language
inference. A user Message without a directive creates an active Fact. Use an ID
from `ContextLedger.Constraints`, or derive one from its source Event with
`agent.ConstraintFactID`, when a later Run supersedes or resolves it

```go
targetID := previous.ContextLedger.Constraints[0].ID

handle, err := runtime.Run(ctx, agent.RunRequest{
    SessionID: previous.SessionID,
    Input: []agent.Message{{
        Role: agent.RoleUser,
        Text: "Use PostgreSQL instead",
        FactDirective: &agent.FactLifecycleDirective{
            Action:  agent.FactLifecycleSupersede,
            Targets: []string{targetID},
        },
    }},
})
```

`supersede` retires every named active target and creates one active Fact from
the current Message. `resolve` retires the targets without creating a Fact from
the resolution Message. Targets must be unique earlier active Facts and are
bounded by `agent.MaxFactLifecycleTargets`. Invalid shape returns
`agent.ErrInvalidFactDirective`; `Steer` also classifies it as
`agent.ErrInvalidSteeringMessage`. Runtime validates target state against the
current Event prefix before Hooks or persistence; a steering target that is no
longer active at its safe boundary fails the Run without committing that Event

Runtime transfers input `Message.FactDirective` to `Event.FactDirective`. The
stored and Provider-facing Message remains free of host lifecycle metadata. A
published `user.message.added` Event is therefore the transition commit point

Cross-Run transitions require a Session Store because target IDs identify
source Events; replaying only prior Messages does not preserve those identities

Use `Host.Start` instead when another request or worker must retain the handle,
stream Events independently, or resume the Run later. A `Start` caller owns
Event draining and `Wait`, and may call `Host.SaveRunEvidence` after completion

Queue one plain, non-empty user Message for the active Run with
`RunHandle.Steer`

```go
err := handle.Steer(agent.Message{
    Role: agent.RoleUser,
    Text: "Prioritize the failing package before broader checks",
})
```

`Steer` is a non-blocking FIFO operation bounded by
`agent.MaxPendingSteeringMessages`. A nil error means only that the queue
accepted the Message. Invalid plain-user input, a full queue, and a closed Run
return `agent.ErrInvalidSteeringMessage`, `agent.ErrSteeringQueueFull`, and
`agent.ErrRunClosed`, respectively. The existing `user.message.added` Event,
with `UserMessageOrigin` set to `steering`, confirms that the Message entered
Session state. Runtime does not alter an in-flight Provider request or retry and
does not interrupt an assistant Tool batch. It applies queued Messages after
all Tool results in that batch, or after an end-turn response, and before
compiling the next Provider request

Once `run.waiting` is observable, `Steer` returns `agent.ErrRunWaiting`; use
`RunHandle.Resume` with the matching request instead. Steering already queued
before that wait remains pending until resume and Tool completion. Cancellation,
deadline expiry, or terminal Run failure stops further application and may
discard queued Messages without a `user.message.added` Event. The Event stream
is the authoritative record of which Messages were applied

The queue bound counts Messages, not bytes. An embedding application must
enforce request-size and tenant memory limits before calling `Steer`

Steering itself consumes no Budget. Subsequent Provider and Tool work uses the
same active Run Budget. To send a follow-up, first drain Events and call `Wait`,
then start a new Run with the same Session ID and configured Session Store. The
follow-up receives a new Run ID and Runtime-local limits while replaying the
persisted Session. Without a Store, the caller must provide prior context.
Reuse the same `*agent.Budget` explicitly only when one shared limit must span
both Runs

Compatibility note: steering adds the optional `user_message_origin` field and
Fact lifecycle adds the optional `fact_directive` field to Event JSON. Existing
Hooks subscribed to `user.message.added` also observe these Events. External
decoders must accept the optional fields. The Go `agent.Message` and
`agent.Event` structs gained exported fields, and Ledger v2 extends
`agent.ConstraintLedgerEntry`, so external composite literals should use field
names

Current World State adds the `current_world_state.captured` Event type, the
optional `current_world_state` field on Events, terminal Results, and Session
snapshots, and the `current_world_state` Segment kind. Strict Event-type switches
must accept or deliberately ignore the new Event. Paths and command arguments
inside a snapshot are content-bearing metadata even though file, diff, stdout,
and stderr content is absent

Deterministic Ledgers add optional `policy` metadata to Tool results,
`context_ledger` to terminal Run results, and `ledger` references to new
Checkpoints. The raw host Policy reason is not included, and Policy metadata is
not copied into the model-facing Tool Message. Strict external JSON decoders
must accept these additive fields. Ledger schema v2 adds Fact state and
transition provenance; replay continues to verify Checkpoint references created
by Ledger v1. Standalone v1 Ledger snapshots must be rebuilt from Events before
validation because `ValidateContextLedger` compares against the current schema

Safe-cut annotations add optional `context_operation` metadata to Tool results
and `Events` to `ContextCompileRequest`. Runtime supplies an isolated exact
Event prefix and validates it against the Ledger before semantic cuts. A direct
compiler caller may omit Events to retain Tool-only boundaries. Extension peers
must implement Protocol v2; v1 manifests and peers are rejected by exact
version negotiation

Built-in Context retrieval adds the optional `context_retrieval` field to Tool
results and `tool.completed` Event JSON. It is additive host metadata rather
than an Extension Protocol field. Strict Event decoders must accept it. The
reserved built-in Tool names use underscores and remain absent unless the host
configures `ContextRetrievalOptions`

At shutdown, stop accepting new work, cancel or finish active Runs, and then
call `Host.CloseContext` to drain and stop every owned Extension process

Loaded Coding Profiles use bounded automatic Extension restart. An interrupted
Run still observes its crashed generation's RPC failure and is never replayed
automatically. A new Run can temporarily fail component acquisition with
`host.ErrExtensionRestarting`, or with `host.ErrExtensionCircuitOpen` after the
attempt limit. An application that constructs `host.Manager` directly opts in
with `host.DefaultRestartPolicy` and can inspect `Manager.RestartStatus`

## Security boundary

The Extension process boundary isolates crashes and protocol state. It is not
an operating-system sandbox. A child process and `run_command` retain the host
account's authority unless the containing application adds a container, OS
sandbox, restricted account, network policy, or another execution backend

Provider credentials are resolved by the Host and are not added to Extension
environments by default. Select only the environment names required by each
Extension

See the complete [embedded server example](../examples/embedded-server/README.md)
for a standard-library HTTP integration with an application-owned linked
Extension
