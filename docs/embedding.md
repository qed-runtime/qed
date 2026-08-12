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

Deterministic Ledgers add optional `policy` metadata to Tool results,
`context_ledger` to terminal Run results, and `ledger` references to new
Checkpoints. The raw host Policy reason is not included, and Policy metadata is
not copied into the model-facing Tool Message. Strict external JSON decoders
must accept these additive fields. Ledger schema v2 adds Fact state and
transition provenance; replay continues to verify Checkpoint references created
by Ledger v1. Standalone v1 Ledger snapshots must be rebuilt from Events before
validation because `ValidateContextLedger` compares against the current schema

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
