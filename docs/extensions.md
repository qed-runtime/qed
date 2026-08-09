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

Tool definitions receive Extension ID and generation metadata before entering
Runtime. Evidence records that origin plus hashes of arguments and output

### Hooks

An Extension registers exact Agent Event type strings and one handler. Runtime
invokes matching Hooks synchronously in registration order before Session
persistence and Event publication. A Hook error rejects the candidate Event and
fails the Run. Tools and Hooks are acquired atomically from the same generation
set for the complete Run

Hook handlers must honor context cancellation and should avoid long-running or
irreversible side effects. A successful Hook can still be followed by a Session
Store failure because Extension RPC and Store append are not one transaction

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

`extensions.lock` selects the Go Extensions linked into one QED binary. It is
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

The Coding Profile composes all three, while another host build may select only
the packages it needs. Each self-exec Extension still has an independent
process, identity, generation, reload, and state namespace

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
Host. Automatic restart is not implemented

## Host-owned Extension state

`extension.StateStore` stores opaque values under Extension ID, host scope, and
key. QED includes a concurrent memory store and a private atomic JSON store with
a 1 MiB value limit

Manager restores the `snapshot` key on initial startup, updates it during
reload, and persists the current process during orderly close. Declarative
Coding Profiles scope it to a digest of workspace and Profile ID, preventing
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
