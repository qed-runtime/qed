# QED Runtime

QED Runtime is an embeddable agent runtime written in Go

> Proof by execution

The project is an early prototype, but its main architectural boundaries are
executable today

- asynchronous Runs with ordered streaming Events
- OpenAI Responses, OpenAI Chat Completions, Anthropic Messages, and
  ChatGPT-authenticated Codex Responses Providers
- multiple Provider profiles in one Agent graph
- concurrent subagents with collect, select, and consensus strategies
- content-addressed subagent Result Packets with injectable Profile reduction
- profile-shared Provider concurrency limits, cooldowns, and bounded retries
- active-Run steering, persistent Sessions, terminal follow-ups, and durable
  approval resume
- capability-controlled Coding Tools behind process-isolated Extensions
- multiple Run-pinned Extension generations, Hooks, Commands, and host-owned state
- manifest discovery, atomic development reload, and bounded crash restart
- non-overwriting Go Extension scaffolds with lifecycle contract tests
- fork-free, application-owned `extensions.lock` catalogs with live manifest validation
- host-owned Evidence Bundles
- deterministic Artifact, Execution, Constraint, Policy, and Task Ledgers with
  explicit Fact lifecycle, rebuilt from ordered Session Events
- canonical Current World State snapshots for relevant file hashes, Git state,
  and observed check freshness
- Evidence-preserving Context compression with approval, subagent,
  edit-verification, commit, and Tool-transaction safe cuts, deterministic
  preservation reports, and pre-Provider rollback
- hierarchical Session Synopsis, Task, and Episode Checkpoints with
  model-facing selection of populated levels
- content-free Context inspect, explain, diff, and aggregate quality metrics
- opt-in bounded Context search with explainable relevance ranking, scoped
  Evidence fetch, Session timeline, and Ledger history Tools
- host- or Provider-supplied Token Estimation with a deterministic byte
  fallback and content-free Provider Usage comparison
- predictive model-context budgeting with output, safety, and expected Tool
  reserves, validated soft preparation, and hard-limit adoption
- Prefix Manifests, prompt-cache Plans, and normalized cache Usage
- Nagi-based CLI and multi-turn TUI
- safe structured diagnostics propagated to the final Extension process

## Requirements

- Go 1.25 or later
- Linux or macOS for the experimental TUI

The provider-neutral Runtime uses the Go standard library. Nagi is confined to
the CLI and TUI adapters and its types do not cross the Runtime API. Development
tools are pinned in the separate `tools` module

## Smoke test

```sh
go run ./cmd/qed run "hello"
```

Expected output

```text
hello
```

Inspect every Run Event as JSON Lines

```sh
go run ./cmd/qed exec --json "hello"
```

`qed` opens an idle TUI, while `qed "prompt"` opens the same TUI and starts
the first Run immediately. `exec` is an alias for the non-interactive `run`
command. Common short options are `-m` for `--model`, `-C` for `--cd`, and
`-v` for `--verbose`. `--workspace` and `--cd` select the same configured
Profile workspace and cannot be supplied together

The echo Provider emits the complete lifecycle, including `run.started`, user
and model events, text deltas, and `run.completed`

`model.request.started` includes a content-free Prefix Manifest, Cache Plan,
and configured Predictive Budget decision with the resolved Token Estimate
kind and count. Provider Usage remains authoritative, and `qed cache status`
compares it with the latest estimate

QED cache controls are disabled until configured; Provider-side implicit
behavior may still apply. Providers that report prompt-cache usage populate
normalized cache read, write, and uncached input counts. Configured JSON
Evidence Stores also retain exact compacted context objects

```sh
qed cache status --store .qed/evidence
qed context inspect <run-id> --store .qed/evidence
qed evidence fetch sha256:<digest> --run-id <run-id> --store .qed/evidence
```

Configured Context Evidence is bound to tenant, Session or ephemeral Run,
execution Profile, and required capabilities. The content digest is not an
access token. See [Context compilation, compression, and prompt caching](docs/context-caching.md)

Enable safe structured diagnostics on stderr with the root flag

```sh
go run ./cmd/qed run -v "hello"
```

Verbose mode propagates through Runtime, Extension Host, Initialize RPC, and the
Extension Server. QED diagnostics contain identifiers, operation names, counts,
durations, and error types. They intentionally omit prompts, message content,
Tool arguments and output, metadata values, credentials, environment values,
and Extension configuration values

## Connect a model API

QED implements four streaming HTTP API dialects without model SDK dependencies

- `openai-responses`
- `openai-chat`
- `openai-codex`
- `anthropic`

The model ID is always explicit

```sh
export OPENAI_API_KEY="<token>"
go run ./cmd/qed run \
  --provider openai-responses \
  --model "<model-id>" \
  "Reply with a short greeting"
```

```sh
export ANTHROPIC_API_KEY="<token>"
go run ./cmd/qed run \
  --provider anthropic \
  --model "<model-id>" \
  --max-output-tokens 1024 \
  "Reply with a short greeting"
```

For a trusted OpenAI-compatible endpoint, pass the API base URL rather than an
operation URL

```sh
go run ./cmd/qed run \
  --provider openai-chat \
  --base-url "http://127.0.0.1:8080/v1" \
  --model "local-model" \
  "hello"
```

Custom base URLs never receive the default OpenAI or Anthropic credential. Set
`QED_API_KEY` only when the custom endpoint is trusted and requires a key

Provider adapter implementers can apply the reusable deterministic suite in
[`provider/contracttest`](docs/providers.md) without calling a live API

### Use a ChatGPT subscription

`openai-codex` uses a named ChatGPT OAuth profile instead of an OpenAI API key

```sh
go run ./cmd/qed auth login --auth-profile personal
go run ./cmd/qed run \
  --provider openai-codex \
  --auth-profile personal \
  --model "<codex-model-id>" \
  "Reply with a short greeting"
```

For a headless machine, use `qed auth login --device-code`. Inspect profile
metadata with `qed auth status` and remove a profile with `qed auth logout`.
The access and refresh tokens are stored outside project configuration in the
OS user configuration directory

ChatGPT subscription access is separate from OpenAI API billing. The Provider
uses the fixed ChatGPT Codex backend and does not accept `--base-url` or
`--max-output-tokens`. See [ChatGPT subscription authentication](docs/chatgpt-auth.md)
for security details and current protocol limitations

## Configure multiple Providers and Agents

Provider endpoint, protocol, model, and credential reference stay
together in one profile. Each Agent independently selects a Provider, so an
OpenAI-compatible coordinator can use an Anthropic subagent

```json
{
  "version": 1,
  "default_agent": "coordinator",
  "providers": {
    "primary": {
      "protocol": "openai-responses",
      "model": "<openai-model-id>",
      "token_env": "PRIMARY_API_TOKEN"
    },
    "review": {
      "protocol": "anthropic",
      "model": "<anthropic-model-id>",
      "token_env": "REVIEW_API_TOKEN"
    }
  },
  "agents": {
    "reviewer": {
      "provider": "review",
      "instructions": "Review the delegated task independently"
    },
    "coordinator": {
      "provider": "primary",
      "delegations": [
        {
          "name": "consult_reviewer",
          "strategy": "delegate",
          "agents": ["reviewer"]
        }
      ]
    }
  }
}
```

```sh
export PRIMARY_API_TOKEN="<token>"
export REVIEW_API_TOKEN="<token>"
go run ./cmd/qed run --config ./qed.json "Review this plan"
```

Use `--agent <id>` to override `default_agent`. Configuration contains
credential environment or auth profile names, never token values. See
[Agent configuration](docs/configuration.md) for the complete schema. Agents
that reference one Provider profile share its default four-stream outbound
limit and observed rate-limit cooldown; configure
`rate_limit.max_concurrency` on that profile when needed

## Run the Coding Profile

The standard Coding Profile exposes six model-facing Tools

- `search_text`
- `read_file`
- `apply_patch`
- `run_command`
- `git_status`
- `git_diff`

For `worktree` and `base`, `git_diff` appends untracked regular files that are
not excluded by standard Git ignore rules within the same bounded patch.
`staged` remains index-only

The checked-in `extensions.lock` selects the reusable `qed.workspace`,
`qed.process`, and `qed.git` Extensions for this binary. Each runs across
Extension Protocol v1, including the single-binary self-exec mode

```json
{
  "version": 1,
  "default_agent": "coding",
  "providers": {
    "model": {
      "protocol": "openai-responses",
      "model": "<model-id>",
      "token_env": "MODEL_API_TOKEN"
    }
  },
  "extensions": {
    "qed.workspace": {"mode": "self-exec"},
    "qed.process": {"mode": "self-exec"},
    "qed.git": {"mode": "self-exec"}
  },
  "profiles": {
    "workspace": {
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
    "coding": {"provider": "model", "profile": "workspace"}
  },
  "session": {"store": "jsonl", "path": ".qed/sessions"},
  "evidence": {"store": "json", "path": ".qed/evidence"},
  "extension_state": {"store": "json", "path": ".qed/extension-state"}
}
```

```sh
export MODEL_API_TOKEN="<token>"
go run ./cmd/qed run \
  --config ./qed.json \
  --cd . \
  --session-id work-1 \
  "Find the failing test, fix it, and run the relevant checks"
```

The Profile accepts workspace-relative file paths, requires digest or absence
preconditions for edits, and records capability decisions and Tool digests in
host-owned Evidence. `run_command` invokes argv directly and is bounded, but it
is not an operating-system sandbox

See [Coding Profile](docs/coding-profile.md) and
[Extension processes](docs/extensions.md) for the detailed boundaries

## Steering, follow-up, approval resume, and Evidence

The Runtime and Host APIs distinguish three forms of later input

- `RunHandle.Steer` queues one user Message for the next safe Provider boundary
  of the active Run
- a follow-up starts a new Run with the same Session ID and configured Session
  Store after the previous handle reaches a terminal result
- `RunHandle.Resume` answers an explicit `run.waiting` request such as approval

Steering is a bounded, non-blocking queue operation. It does not interrupt an
in-flight Provider request, retry, or Tool batch. A `user.message.added` Event
whose `UserMessageOrigin` is `steering` confirms that the Message entered
Session state. Cancellation, deadline expiry, or terminal Run failure may
discard steering that has not reached that Event. Follow-ups get a new Run ID
and local limits; without a Session Store the caller must provide prior context
itself. Reuse the same `*agent.Budget` explicitly when limits must span Runs

Hosts can attach `FactLifecycleDirective` to a user Message to supersede or
resolve earlier active Constraint Fact IDs. Runtime moves the directive to the
committed `user.message.added` Event, keeps it out of Provider conversation
history, and deterministically rebuilds active, superseded, and resolved state
without interpreting natural language. See
[Context and caching](docs/context-caching.md) for the lifecycle contract

The experimental TUI maps Enter to active-Run steering and, after the current
Run reaches its terminal result, to a follow-up Run. A configured `--session-id`
replays the persisted Session. Without one, the TUI carries the preceding Run
messages forward for the lifetime of that chat

Put capabilities in a Profile's `ask` list and use interactive approval

```sh
go run ./cmd/qed run \
  --config ./qed.json \
  --approval prompt \
  --session-id work-1 \
  "Run the relevant checks"
```

Approval creates `run.waiting` and `run.resumed` Events. If the process stops
while a JSONL Session is waiting, resume the exact pending Tool call without
repeating the preceding Provider request

When a Tool implements `extension.ApprovalPreviewer`, the Host asks it for a
bounded, side-effect-free description only after Policy returns `ask`. The Host
validates the preview and binds it to the exact argument digest, Extension ID,
and pinned generation before the approval is persisted or displayed. Raw Tool
arguments remain outside the wait payload. The built-in `apply_patch` preview
shows validated file operations and line counts, while `run_command` shows the
exact argv, workspace-relative directory, and effective timeout. An Extension
without preview support remains usable and is shown as unavailable detail

Approval previews are content-bearing and may contain paths or command
arguments. Protect Session Events that contain them and do not place secrets in
command arguments

```sh
go run ./cmd/qed session resume work-1 --config ./qed.json
```

Configured Runs, every completed Run in a configured TUI chat, and resumed Runs
save a separate Evidence Bundle when an Evidence Store is configured

```sh
go run ./cmd/qed evidence inspect <run-id> --store .qed/evidence
go run ./cmd/qed evidence export <run-id> --store .qed/evidence
go run ./cmd/qed context inspect <run-id> --store .qed/evidence
go run ./cmd/qed context explain <run-id> --store .qed/evidence
```

`qed context diff --before RUN_ID[@EVENT_SEQUENCE] --after
RUN_ID[@EVENT_SEQUENCE]` compares two compaction decisions without printing
message, path, command, or Evidence object content
`context inspect` and `context explain` also report which hierarchical
Checkpoint levels entered each compiled model view

## Experimental TUI

```sh
go run ./cmd/qed
go run ./cmd/qed "hello"
```

The explicit `qed tui [PROMPT]` form is also available. The TUI accepts
`--config`, `--agent`, `--workspace` or `--cd`, and `--session-id`, and uses
the same configured Agent graph. Without a prompt it waits without starting a
Run. Type a message and press Enter to start the first Run, steer an active Run,
or start a follow-up after it finishes. When a Run waits for approval, press
`Y` to approve or `N` to deny. Ctrl-C cancels only the current Run and keeps the
chat open; Escape exits and cancels an active Run

The view keeps recent user and assistant messages, streams assistant text, and
shows Run activity with Agent, Session, Run, Tool, and approval capability
metadata. During approval it also shows the bounded Tool-supplied preview,
Extension generation, and argument digest. Failed Tool activity uses a small
allowlist of safe failure classifications such as invalid patch, stale file,
command failure, timeout, or permission denial. Raw Tool arguments, Tool
output, raw wait payloads, and raw Run errors are not copied into rendered view
state

The multiline Composer uses Enter to submit and Shift-Enter, Alt-Enter, or
Ctrl-O to insert a line break. Up and Down recall the bounded submission
history when the caret is at an editor boundary. The transcript uses a
variable-height VirtualFeed, retains at most 2,048 user and assistant entries,
and builds only the visible range plus overscan. Click the transcript to move
focus from the Composer, then use PageUp or PageDown to navigate it. The mouse
wheel scrolls the region under the pointer. Click the Composer to resume input.
QED enables SGR press mouse tracking while the TUI is open so click and wheel
reports reach the application. Terminal-native pointer selection may therefore
require the terminal's own override gesture or leaving the TUI first

Press F2 to toggle content-free Context, predictive-budget, cache, and scoped
Evidence availability details. Evidence content is not read directly by the
TUI; ask the Agent to use `context_search` or `context_fetch` so the configured
Capability and access-audit boundary remains authoritative. With a configured
standard Session Store, F6 opens the next older recent Session, Shift-F6 moves
newer, and F7 returns to the current chat. Historical Sessions are read-only
and an active current Run continues while they are displayed. Starting the TUI
with an existing Session seeds the bounded transcript, Composer recall, and
Context status before the new Run begins

## Develop an external Extension

Create a Go reference scaffold inside an existing Go module. The parent
directory must already exist and the destination must be new

```sh
mkdir -p ./extensions
go run ./cmd/qed extension scaffold \
  ./extensions/my-extension \
  --id example.my-extension
```

The command generates an external manifest and executable, an importable
`ServerOptions` implementation for self-exec, and a process-level lifecycle
contract test. It never modifies `go.mod`, `go.sum`, or `extensions.lock`, and
it refuses to overwrite even an empty destination directory

An external Extension directory contains `qed-extension.json`. Start a
development host directly from source

```sh
go run ./cmd/qed extension dev ./extensions/my-extension
```

The default build command is `go build -o {output} .`. QED watches source
metadata, builds each candidate into a distinct temporary executable, validates
and restores it, then atomically routes new Runs to the new generation. A
failed build or candidate leaves the active generation unchanged

From another process

```sh
go run ./cmd/qed extension inspect my-extension
go run ./cmd/qed extension reload my-extension
```

The local control endpoint uses a private descriptor, random bearer token, and
loopback TCP. See [Extension processes](docs/extensions.md) for manifest and
custom build details

## Build a self-exec Extension catalog

`extensions.lock` selects Go Extension packages for static linking and records
the manifest declaration each child process must match. Generate the checked-in
catalog after changing it, and use `--check` in CI

```sh
go run ./cmd/qed extension generate
go run ./cmd/qed extension generate --check
```

Another Host repository uses the dependency-light generator and chooses its
own generated package and exported Catalog variable

```sh
go run github.com/qed-runtime/qed/cmd/qed-extension-gen \
  --lock extensions.lock \
  --output extensionregistry/registry_gen.go \
  --package extensionregistry \
  --variable Catalog
```

The generated source depends only on public QED packages and the Extension
packages named by that application's lock. No QED fork or copied internal
registry glue is required

The lock does not distinguish first-party and third-party packages. Dependency
versions and checksums remain in `go.mod` and `go.sum`; generation never changes
them. Non-Go Extensions continue to use the same protocol through an external
executable. See [Extension processes](docs/extensions.md) for the lock schema
and validation sequence

## Embed the Runtime

Use the root `qed.Host` API when an existing server should load a declarative
Agent graph and own Provider, Profile, Store, Evidence, and Extension lifecycle

```go
host, err := qed.LoadHost("qed.json", qed.HostLoadOptions{
	LookupEnv:       os.LookupEnv,
	WorkspaceRoot:   workspaceRoot,
	SelfExecutable:  executable,
	SelfExecCatalog: extensionregistry.Catalog,
})
if err != nil {
	log.Fatal(err)
}
defer host.Close()

outcome, err := host.Run(ctx, agent.RunRequest{
	Input: []agent.Message{{Role: agent.RoleUser, Text: prompt}},
}, nil)
```

`Host` is transport-neutral and safe for concurrent Runs. The embedding
application continues to own HTTP or gRPC schemas, authentication,
authorization, inbound client or tenant rate limiting, and shutdown ordering. See
[Embedding QED](docs/embedding.md) and the
[standard-library server example](examples/embedded-server/README.md)

Use `agent.Runtime` directly for a smaller programmatic integration

```go
package main

import (
	"context"
	"log"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/provider/echo"
)

func main() {
	runtime, err := agent.NewRuntime(agent.Options{Provider: echo.New()})
	if err != nil {
		log.Fatal(err)
	}

	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID: "example",
		Input: []agent.Message{
			{Role: agent.RoleUser, Text: "hello"},
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	for event := range handle.Events() {
		log.Printf("event: %s", event.Type)
	}
	result, err := handle.Wait()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("status: %s", result.Status)
}
```

Use `agent.ComponentSource` to pin Tools and Hooks atomically for a Run. Standard
Session implementations are available in `session`; orchestration remains a
separate package above Runtime Core

### Compose multiple Providers

Each Runtime stays bound to one Provider. `orchestration.AgentRegistry`
composes named Runtimes without converting provider-private continuation state

```go
registry, err := orchestration.NewAgentRegistry(orchestration.AgentRegistryOptions{
	Agents: []orchestration.AgentDefinition{
		{
			ID:           "anthropic-specialist",
			Runtime:      anthropicRuntime,
			Instructions: "Review the delegated task",
		},
	},
})
if err != nil {
	log.Fatal(err)
}

delegateTool, err := orchestration.NewSubagentTool(orchestration.SubagentToolOptions{
	Name:     "consult_specialist",
	Registry: registry,
	Strategy: orchestration.TeamStrategyDelegate,
	AgentIDs: []string{"anthropic-specialist"},
})
if err != nil {
	log.Fatal(err)
}

openAIRuntime, err := agent.NewRuntime(agent.Options{
	Provider: openAIProvider,
	Tools:    []agent.Tool{delegateTool},
})
if err != nil {
	log.Fatal(err)
}
if err := registry.Register(orchestration.AgentDefinition{
	ID: "openai-coordinator", Runtime: openAIRuntime,
}); err != nil {
	log.Fatal(err)
}
```

`delegate` runs exactly one candidate, `collect` returns every outcome,
`select` asks a judge to select a candidate, and `consensus` asks a judge to
synthesize a result. Candidates run concurrently and may use different
Provider protocols. Shared default limits are 16 Agent Runs, depth 4, and 64
Provider calls. Each configured Provider profile also shares a default limit
of four active Provider streams and any observed rate-limit cooldown

Every successful candidate also returns a versioned `ResultPacket` bound to
its terminal Context Ledger. The default `LedgerResultReducer` returns typed
Artifact and Execution entries without inferring semantic facts
`AgentDefinition.ResultReducer` may add source-backed Facts and bounded
Profile-owned JSON without adding domain fields to Runtime Core. Declarative
Coding Profiles install their reducer automatically. Invalid or unverifiable
reduction fails only that candidate when another candidate remains usable

Result Packet Facts and Profile state are untrusted content shown to the parent
model. Evidence references preserve their original authorization scope and do
not grant the parent access by themselves

## Main import paths

- `github.com/qed-runtime/qed`
- `github.com/qed-runtime/qed/agent`
- `github.com/qed-runtime/qed/orchestration`
- `github.com/qed-runtime/qed/session`
- `github.com/qed-runtime/qed/provider/openai`
- `github.com/qed-runtime/qed/provider/openaicodex`
- `github.com/qed-runtime/qed/provider/anthropic`
- `github.com/qed-runtime/qed/capability`
- `github.com/qed-runtime/qed/evidence`
- `github.com/qed-runtime/qed/extension`
- `github.com/qed-runtime/qed/extension/host`
- `github.com/qed-runtime/qed/extension/manifest`
- `github.com/qed-runtime/qed/extension/protocol`
- `github.com/qed-runtime/qed/extension/reload`
- `github.com/qed-runtime/qed/extension/server`
- `github.com/qed-runtime/qed/extension/selfexec`
- `github.com/qed-runtime/qed/profile/coding`

## Development

```sh
go -C tools tool goimports -w ..
go run ./cmd/qed extension generate --check
go test ./...
go vet ./...
go build ./...
```

## Current limitations

- Automatic Extension restart does not replay an interrupted Tool call and can
  restore only the latest host-owned Snapshot
- `run_command` and Extension child processes use host-account authority and are not OS sandboxes
- Tool Trace records use hashes, but the Bundle's public Events may contain prompts, messages, Tool arguments, Tool output, and errors; protect the Evidence Store as sensitive data
- Evidence is not a complete workspace archive
- Tool input uses a bounded JSON Schema subset plus strict concrete decoders; embedders can inject another validator, but QED does not implement the complete JSON Schema vocabulary
- Shared token and cost limits depend on Provider-reported usage, which may be late or absent
- The TUI retains at most 2,048 transcript entries, 256 activity entries, 128 Composer history entries, and 64 recent Session summaries; older content remains in the configured Session Store and is not all resident in the view
- A built-in HTTP service, GitHub Actions adapter, SQLite Session Store, and WebAssembly backend are not implemented; existing servers can embed `qed.Host`
- Compatibility with every third-party OpenAI-compatible API is not guaranteed
- `openai-codex` follows an experimental ChatGPT backend contract and currently uses full Responses over SSE without model discovery, Responses Lite, or WebSocket transport

## License

[MIT](LICENSE)
