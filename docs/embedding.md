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
authentication, authorization, rate limiting, tenancy, or request schemas for
the containing application

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
adapter can resume a waiting Run

Use `Host.Start` instead when another request or worker must retain the handle,
stream Events independently, or resume the Run later. A `Start` caller owns
Event draining and `Wait`, and may call `Host.SaveRunEvidence` after completion

At shutdown, stop accepting new work, cancel or finish active Runs, and then
call `Host.CloseContext` to drain and stop every owned Extension process

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
