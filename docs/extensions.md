# Extension processes

QED places Tools, Event Hooks, and host-invoked Commands behind a versioned
process boundary. Development executables, discovered packages, and the QED
self-exec child use the same Protocol v1 contract

```text
Agent Run
   |
   v
atomic Extension generation set
   |
   v
Host Policy + approval + Evidence
   |
   v
4-byte length-prefixed JSON over stdio
   |
   +-- external or discovered executable
   |
   +-- QED self-exec child
```

Process separation provides lifecycle and crash isolation. It is not a security
sandbox: an Extension and programs it starts retain their child-process account
authority

## Go Extension scaffold

Create the initial Go layout inside an existing Go module

```sh
mkdir -p ./extensions
qed extension scaffold \
  ./extensions/example-extension \
  --id example.extension
```

The nearest `go.mod` above the destination supplies the generated import path.
The parent directory must already exist, while the destination itself must not
exist. IDs must start with an ASCII letter or digit and then use letters,
digits, dots, underscores, or hyphens, with a 256-byte limit. The implementation
version uses the same starting rule and character set, additionally permits `+`
after its first character, has a 128-byte limit, defaults to `0.1.0`, and can be
selected with `--extension-version`. Generated import path
elements follow the [Go Modules Reference](https://go.dev/ref/mod); hidden,
underscore-prefixed, and `testdata` destination elements are rejected because
they do not form a normal buildable package layout. Module discovery reads at
most 1 MiB from a regular non-symlink `go.mod`

The command creates this layout

```text
example-extension/
  .gitignore
  README.md
  extension/
    extension.go
  main.go
  main_test.go
  qed-extension.json
```

`main.go` is the external process entrypoint. The nested `extension` package
exports `Declaration` and `ServerOptions`, so the same implementation can be
selected by an application-owned `extensions.lock` for self-exec. The generated
test runs `contracttest.RunLifecycle` against the actual test process. Add
component behavior tests when adding Tools, Hooks, or Commands

Scaffolding creates only a new directory. It rejects an existing destination,
including an empty directory, and rolls back files it created if generation
fails. It does not run Go dependency commands or modify `go.mod`, `go.sum`, or
`extensions.lock`. The owning module must add its QED dependency through its
normal dependency workflow

## Protocol v1

Each message is one UTF-8 JSON object prefixed by a 4-byte unsigned big-endian
payload length. The maximum envelope is 8 MiB. Unknown fields, trailing values,
malformed frames, missing correlation IDs, and version mismatches are rejected

Requests contain `version`, `id`, `method`, and optional `params`. Responses
repeat `version` and `id` and contain exactly one of `result` or `error`.
Requests are multiplexed and responses may arrive out of order

| Method | Purpose |
| --- | --- |
| `handshake` | Negotiate the exact protocol and implementation identity |
| `initialize` | Supply workspace, selected environment, opaque configuration, and verbose state |
| `describe` | Register capabilities, Tools, Hooks, and Commands |
| `required_capabilities` | Resolve Tool invocation-specific capabilities before authorization |
| `invoke_tool` | Execute an authorized Tool call with Run identity |
| `handle_event` | Deliver one selected Run Event to a Hook |
| `invoke_command` | Execute an authorized host-requested Command |
| `health_check` | Report initialized and draining state |
| `snapshot` | Return bounded opaque JSON state |
| `restore` | Apply state to a compatible generation |
| `drain` | Reject new work and wait for accepted requests |
| `cancel` | Cancel one in-flight request by correlation ID |
| `shutdown` | Drain, invoke cleanup, respond, and exit |

Provider-private continuation state never crosses this protocol. A Hook receives
the public Agent Event JSON and Run identity only

## Components

### Tools

Each Tool declares a name, description, input schema, static capabilities, and
whether it has invocation-specific capabilities. The Host asks for dynamic
capabilities, evaluates the combined set through `capability.Policy`, obtains
approval if required, and sends `invoke_tool` only after authorization

Runtime validates Provider-supplied arguments before dynamic capability
resolution, Policy, approval, or Tool execution. The Extension server validates
the same arguments again at the RPC boundary, including direct protocol calls.
A validation failure is an ordinary failed Tool result and can be corrected by
the model on its next normal turn; it is not a Provider failure and does not
activate Provider retry

Every validator path limits schemas to 1 MiB, arguments to 8 MiB, and nesting
to 64, and rejects duplicate JSON keys, trailing values, and malformed JSON.
The dependency-free default adds a JSON Schema subset supporting every JSON
`type` value, including `integer`, plus `properties`, `required`,
`additionalProperties` as a boolean, `items` as one schema, `minItems`,
`minimum`, `maximum`, and `enum`. `description` and `title` are accepted as
annotations. The default limits compiled schema nodes and `required` names to
4096 and `enum` entries to 256. Invalid schemas and unsupported keywords are
rejected rather than ignored. An omitted schema defaults to an object schema

Applications that need another dialect can implement
`agent.ToolInputValidator` and `agent.CompiledToolInputValidator`. Injection is
available through `agent.Options`, `qed.HostLoadOptions`,
`extension.ToolOptions`, `host.ManagerOptions`, `coding.Options`, and
`server.Options`. A custom host validator is process-local; a process-isolated
Extension must configure its own `server.Options.ToolInputValidator`. Concrete
Tool decoders remain required as defense in depth

Tool definitions receive Extension ID and generation metadata before entering
Runtime. Evidence records that origin plus hashes of arguments and output
The Host proxy also attaches the final authorization outcome, sorted capability
names, and a digest of the Policy reason to `ToolResult.Policy`. It never places
the raw reason in public Events, and Runtime does not copy this metadata into
the model-facing Tool Message. These fields let Session replay reconstruct the
Policy Ledger without trusting a second mutable state store

### Hooks

An Extension registers exact Agent Event type strings and one handler. Runtime
invokes matching Hooks synchronously in registration order before Session
persistence and Event publication. A Hook error rejects the candidate Event and
fails the Run. Tools and Hooks are acquired atomically from the same generation
set for the complete Run

Hook handlers must honor context cancellation and should avoid long-running or
irreversible side effects. A successful Hook can still be followed by a Session
Store failure because Extension RPC and Store append are not one transaction

Active-Run steering retains the `user.message.added` Event type and sets the
optional `user_message_origin` field to `steering`. A Hook subscribed to that
type receives both Run input and steering Messages, so strict protocol decoders
must include the optional field

### Commands

Commands declare a name, description, JSON input schema, and capabilities. They
are invoked explicitly by a Host through `extension.Command`; they are not
automatically model-facing Tools. `Manager.AcquireCommands`,
`GenerationSet.AcquireCommands`, and `coding.Profile.AcquireCommands` pin the
same generation semantics used by Runs

The Host evaluates Command capabilities before `invoke_command`

## Go Server adapter

`extension/server.Serve` adapts Go components and lifecycle callbacks

```go
options := server.Options{
	ID:      "example-extension",
	Version: "0.1.0",
	InitializeComponents: func(
		ctx context.Context,
		request protocol.InitializeRequest,
	) (server.Components, error) {
		return server.Components{
			Tools:    tools,
			Hooks:    []string{string(agent.EventRunStarted)},
			Commands: commands,
			HandleEvent: func(
				ctx context.Context,
				request protocol.HandleEventRequest,
			) error {
				return handleEvent(ctx, request)
			},
		}, nil
	},
	Snapshot: snapshot,
	Restore:  restore,
}

if err := server.Serve(ctx, os.Stdin, os.Stdout, options); err != nil {
	log.Fatal(err)
}
```

The older `Initialize` callback remains available for Tool-only servers. A
server must provide exactly one of `Initialize` or `InitializeComponents`

Protocol stdout must contain frames only. Human or safe debug diagnostics go to
stderr through `Options.DebugWriter`

## Contract test kit

`extension/contracttest` provides one reference Extension and a reusable suite
for checking launcher and Protocol behavior. Pass a command that serves a fresh
`contracttest.ServerOptions()` fixture on every process start

The same suite verifies:

- Handshake, Initialize payload propagation, Describe, and HealthCheck
- Tool invocation, dynamic capabilities, Hook delivery, and Command invocation
- Snapshot, Restore, Drain, Cancel, and graceful Shutdown
- child-process crash isolation

Use `RunLifecycle` against an actual Extension executable, including one written
in a non-Go language. It validates the supplied declaration through Handshake,
Initialize, Describe, HealthCheck, Snapshot, Restore, Drain, and Shutdown

```go
contracttest.RunLifecycle(t, contracttest.LifecycleOptions{
	Command:     command,
	Declaration: declaration,
	Initialize:  initializeRequest,
})
```

Component semantics and intentional crash behavior differ between Extensions,
so the complete `Run` suite uses the standard reference fixture

An external executable test can dispatch the fixture from `TestMain` and pass
that executable back to the suite

```go
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == contracttest.ExternalChildArgument {
		options := contracttest.ServerOptions()
		if err := server.Serve(context.Background(), os.Stdin, os.Stdout, options); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestExternalContract(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contracttest.Run(t, contracttest.SuiteOptions{
		Command: host.Command{
			Path: executable,
			Args: []string{contracttest.ExternalChildArgument},
		},
	})
}
```

For self-exec, register `contracttest.Declaration()` and
`contracttest.ServerOptions` in a `selfexec.Definition`, dispatch that Catalog
from `TestMain`, build its command with `Definition.Command`, and pass the
command to the same `contracttest.Run` call. See the
[package contract test](../extension/contracttest/contracttest_test.go) for
both launcher paths

The fixture includes an intentional process-exit probe, so serve it only from a
dedicated test child. The suite checks the common process and Protocol contract.
Extension-specific business behavior, Policy decisions, and OS sandboxing need
separate tests

## External manifest

The conventional filename is `qed-extension.json`

```json
{
  "id": "example-extension",
  "version": "0.1.0",
  "protocol_version": 1,
  "entrypoint": "bin/example-extension",
  "capabilities": ["filesystem.read"],
  "hooks": ["run.started"],
  "commands": [
    {
      "name": "inspect_state",
      "description": "Return public Extension state",
      "input_schema": {
        "type": "object",
        "properties": {},
        "additionalProperties": false
      },
      "capabilities": ["filesystem.read"]
    }
  ]
}
```

The manifest is strict JSON with a 1 MiB limit. ID, version, exact protocol,
local relative entrypoint, unique valid capability names, Hook Event types, and
Command definitions are validated. Runtime loading requires a regular
non-symlink entrypoint that resolves inside the manifest directory

All fields except `entrypoint` form `manifest.Declaration`, the
transport-independent contract shared with embedded Extensions

Tool definitions are intentionally returned by Describe rather than frozen in
the external file. The Host compares external identity, version, capabilities,
Hooks, and Commands with the live process before publishing it

`manifest.Discover` recursively searches up to 1024 manifests, skips directory
symlinks, sorts by ID, and rejects duplicate IDs

## Embedded self-exec catalog

`extensions.lock` selects the Go Extensions linked into one Host binary. It is
strict JSON with a 1 MiB limit, a maximum of 1024 unique Extension IDs, and the
same declaration validation as an external manifest

```json
{
  "version": 1,
  "extensions": [
    {
      "go_package": "example.com/qed-extension/example",
      "factory": "ServerOptions",
      "manifest": {
        "id": "example-extension",
        "version": "0.1.0",
        "protocol_version": 1,
        "capabilities": ["example.read"]
      }
    }
  ]
}
```

`go_package` names a package already available to the host Go module. `factory`
defaults to `ServerOptions` and must name an exported function with signature
`func() server.Options`. Go dependency versions and integrity remain owned by
`go.mod` and `go.sum`; catalog generation does not add or update dependencies

Generate the checked-in catalog after changing the lock, and verify freshness
without writing in CI

```sh
qed extension generate
qed extension generate --check
```

A downstream Host uses the standalone generator and owns the output package

```sh
go run github.com/qed-runtime/qed/cmd/qed-extension-gen \
  --lock extensions.lock \
  --output extensionregistry/registry_gen.go \
  --package extensionregistry \
  --variable Catalog
```

Generated source constructs a public `*selfexec.Catalog` and does not depend on
QED internal packages. The application dispatches child mode before parsing
its ordinary arguments

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

The same Catalog is passed to `qed.LoadHost` with the absolute current
executable. See [Embedding QED](embedding.md) for the complete downstream flow

The generated catalog supplies the same expected identity, version,
capabilities, Hooks, and Commands used for external manifest validation. At
startup, the linked Server options must match the locked identity and version,
then Handshake and Describe must match the complete locked declaration before
the generation is published

The lock does not encode whether an Extension is first-party or third-party.
Any compatible Go package can be selected for self-exec, while an Extension in
any language can continue to use an external executable and manifest. The only
runtime distinction is the launcher. `extensions.lock` is reproducible build
input, not a code-signing or trust policy

The QED repository currently selects these three reusable Extensions

| Extension | Components |
| --- | --- |
| `qed.workspace` | `search_text`, `read_file`, `apply_patch` |
| `qed.process` | `run_command` |
| `qed.git` | `git_status`, `git_diff` |

The Coding Profile composes all three, while another Host repository selects
only the packages it needs in its own lock. Each self-exec Extension still has
an independent process, identity, generation, reload, and state namespace

Testing remains a use of the generic `qed.process` command Tool rather than a
special Test Extension. Permission decisions remain in Host Policy and the
optional Approver rather than a Permission Extension. These ownership choices
apply equally to first-party and third-party Extensions

## Host enforcement and lifecycle

Initial startup

```text
Start process
  -> Handshake
  -> Initialize
  -> Describe and validate components
  -> validate the locked or external manifest declaration when configured
  -> HealthCheck
  -> publish generation
```

`host.Process` represents one process. `host.Manager` adds authorization,
Evidence, state, and reload. `host.GenerationSet` atomically acquires components
from multiple Managers so a Run cannot observe a partial Extension set

Reload

```text
start and validate candidate
  -> Snapshot active generation
  -> persist Snapshot in the host State Store when configured
  -> Restore candidate
  -> HealthCheck candidate
  -> atomically publish candidate for new Runs
  -> wait for old leases
  -> Drain and Shutdown old process
```

Any failure before publication closes the candidate and retains the active
generation. Process crash fails pending RPC requests without terminating the
Host

Automatic restart is opt-in for a directly constructed Manager

```go
manager, err := host.NewManager(ctx, host.ManagerOptions{
    Process:       processOptions,
    Policy:        policy,
    StateStore:    stateStore,
    RestartPolicy: host.DefaultRestartPolicy(),
})
```

Coding Profiles and the development host use `DefaultRestartPolicy` when their
policy pointer is nil. The default allows three replacement candidates, starts
with 100 milliseconds of backoff, caps exponential backoff at 2 seconds, and
resets the consecutive count after one generation survives for 30 seconds. A
direct Manager's zero-value policy disables automatic restart. Set a Coding
Profile or development host policy pointer to a zero `host.RestartPolicy` to
disable it there

Unexpected exit follows this order

```text
fail RPC requests pinned to the crashed generation
  -> remove that generation from new lease selection
  -> return ErrExtensionRestarting to new acquisition
  -> wait for bounded backoff
  -> start with the last successfully published ProcessOptions
  -> revalidate identity and the locked manifest when configured
  -> load and Restore the latest host-owned Snapshot when configured
  -> HealthCheck and, when a State Store is configured, Snapshot and persist
  -> publish the next generation for new Runs
```

Existing Run leases never migrate and QED never replays an interrupted Tool
call because its side effect may already have occurred. Failed candidates do
not consume generation numbers. Candidate startup failures and replacement
generations that exit before the stability window consume the same attempt
limit. Exhaustion returns `ErrExtensionCircuitOpen` to new acquisition. A
successful explicit `Manager.Reload` validates a candidate and closes the
circuit

`Manager.RestartStatus` reports `disabled`, `ready`, `restarting`,
`circuit_open`, or `closed`, the consecutive attempt count, current generation,
and a payload-free last error type. Verbose lifecycle logs contain the same
safe identifiers and counters without RPC payloads

## Host-owned Extension state

`extension.StateStore` stores opaque values under Extension ID, host scope, and
key. QED includes a concurrent memory store and a private atomic JSON store with
a 1 MiB value limit

Manager restores the `snapshot` key on initial startup, updates it during
reload, and persists the current process during orderly close. With automatic
restart enabled, initial startup and every replacement also persist a baseline
Snapshot before publication. A crash can therefore restore the latest
host-owned Snapshot but not process state created after it. Declarative Coding
Profiles scope state to a digest of workspace and Profile ID, preventing
unrelated Profiles from sharing state accidentally

Snapshot and Restore are for necessary process-local state. Durable Agent
conversation belongs in Session Store, and execution proof belongs in Evidence
Store

## Development reload

Start from an Extension source directory or manifest path

```sh
qed extension dev ./extensions/example-extension
```

The development loader does not require the manifest entrypoint build output to
exist yet. The default direct build is

```text
go build -o {output} .
```

Override the executable with `--build-program` and supply repeated `--build-arg`
values. At least one argument must contain `{output}`. No shell interprets the
program or arguments. Build output is bounded to 1 MiB

The watcher polls regular-file size, modification time, and mode, skips
`.git`, `.qed`, and symlinks, caps one snapshot at 10,000 files, and debounces
changes. Each candidate uses a distinct temporary executable. Build, startup,
manifest comparison, Restore, or HealthCheck failure is reported while the old
generation stays active

Use another process to inspect or force reload

```sh
qed extension inspect example-extension
qed extension reload example-extension
```

`inspect` also accepts a manifest path or directory and emits resolved JSON.
The CLI control directory defaults to `.qed/extension-dev` below its current
directory and can be changed consistently with `--control-dir`

The control server listens only on loopback, writes a `0600` descriptor below a
`0700` directory, authenticates every request with a random token, rejects a
second active server for the same ID, and removes only its matching descriptor
on close

## Cancellation and verbose diagnostics

Canceling Tool, Hook, or Command context sends an independent `cancel` request,
allowing the server to cancel work while the original RPC remains in flight.
Drain waits for accepted work and rejects new invocations

Root `--verbose` propagates to `InitializeRequest.Verbose`. The Server activates
safe structured stderr diagnostics only after Initialize. Host logs operation
names, IDs, generations, counts, durations, and error types without logging
payload values. Arbitrary child stderr is retained only as bounded process
diagnostics and is not forwarded as safe verbose output

## Security boundary

Authorization in the Host prevents the protocol adapter from invoking a Tool
or Command without Policy approval. It does not contain a malicious executable,
which could act outside RPC using its process authority. Configure trusted
Extensions only and add a container, OS sandbox, or separate account when
hostile code must be contained
