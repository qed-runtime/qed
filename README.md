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
- persistent Sessions, durable approval waits, and resume
- capability-controlled Coding Tools behind process-isolated Extensions
- multiple Run-pinned Extension generations, Hooks, Commands, and host-owned state
- manifest discovery and atomic development reload
- fork-free, application-owned `extensions.lock` catalogs with live manifest validation
- host-owned Evidence Bundles
- Nagi-based CLI and single-turn TUI
- safe structured diagnostics propagated to the final Extension process

## Requirements

- Go 1.25 or later
- Linux or macOS for the experimental TUI

The provider-neutral Runtime uses the Go standard library. Nagi is confined to
the CLI and TUI adapters and its types do not cross the Runtime API. Development
tools are pinned in the separate `tools` module

## Smoke test

```sh
go run ./cmd/qed run --prompt "hello"
```

Expected output

```text
hello
```

Inspect every Run Event as JSON Lines

```sh
go run ./cmd/qed run --prompt "hello" --output jsonl
```

The echo Provider emits the complete lifecycle, including `run.started`, user
and model events, text deltas, and `run.completed`

Enable safe structured diagnostics on stderr with the root flag

```sh
go run ./cmd/qed --verbose run --prompt "hello"
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
  --prompt "Reply with a short greeting"
```

```sh
export ANTHROPIC_API_KEY="<token>"
go run ./cmd/qed run \
  --provider anthropic \
  --model "<model-id>" \
  --max-output-tokens 1024 \
  --prompt "Reply with a short greeting"
```

For a trusted OpenAI-compatible endpoint, pass the API base URL rather than an
operation URL

```sh
go run ./cmd/qed run \
  --provider openai-chat \
  --base-url "http://127.0.0.1:8080/v1" \
  --model "local-model" \
  --prompt "hello"
```

Custom base URLs never receive the default OpenAI or Anthropic credential. Set
`QED_API_KEY` only when the custom endpoint is trusted and requires a key

### Use a ChatGPT subscription

`openai-codex` uses a named ChatGPT OAuth profile instead of an OpenAI API key

```sh
go run ./cmd/qed auth login --auth-profile personal
go run ./cmd/qed run \
  --provider openai-codex \
  --auth-profile personal \
  --model "<codex-model-id>" \
  --prompt "Reply with a short greeting"
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
go run ./cmd/qed run --config ./qed.json --prompt "Review this plan"
```

Use `--agent <id>` to override `default_agent`. Configuration contains
credential environment or auth profile names, never token values. See
[Agent configuration](docs/configuration.md) for the complete schema

## Run the Coding Profile

The standard Coding Profile exposes six model-facing Tools

- `search_text`
- `read_file`
- `apply_patch`
- `run_command`
- `git_status`
- `git_diff`

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
  --workspace . \
  --session-id work-1 \
  --prompt "Find the failing test, fix it, and run the relevant checks"
```

The Profile accepts workspace-relative file paths, requires digest or absence
preconditions for edits, and records capability decisions and Tool digests in
host-owned Evidence. `run_command` invokes argv directly and is bounded, but it
is not an operating-system sandbox

See [Coding Profile](docs/coding-profile.md) and
[Extension processes](docs/extensions.md) for the detailed boundaries

## Approval, Session resume, and Evidence

Put capabilities in a Profile's `ask` list and use interactive approval

```sh
go run ./cmd/qed run \
  --config ./qed.json \
  --approval prompt \
  --session-id work-1 \
  --prompt "Run the relevant checks"
```

Approval creates `run.waiting` and `run.resumed` Events. If the process stops
while a JSONL Session is waiting, resume the exact pending Tool call without
repeating the preceding Provider request

```sh
go run ./cmd/qed session resume work-1 --config ./qed.json
```

Configured Runs, configured TUI Runs, and resumed Runs save an Evidence Bundle
when an Evidence Store is configured

```sh
go run ./cmd/qed run inspect <run-id> --store .qed/evidence
go run ./cmd/qed run export <run-id> --store .qed/evidence
```

The equivalent `qed evidence inspect` and `qed evidence export` commands are
also available

## Experimental TUI

```sh
go run ./cmd/qed tui --prompt "hello"
```

The TUI also accepts `--config`, `--agent`, `--workspace`, and `--session-id`
and uses the same configured Agent graph. When a Run waits for approval, press
`Y` to approve or `N` to deny. Press `Q` or Escape to exit, or Ctrl-C to report
cancellation with status 130

## Develop an external Extension

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
authorization, rate limiting, and shutdown ordering. See
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
Provider calls

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

- Extension processes are crash-isolated but are not automatically restarted
- `run_command` and Extension child processes use host-account authority and are not OS sandboxes
- Tool Trace records use hashes, but the Bundle's public Events may contain prompts, messages, Tool arguments, Tool output, and errors; protect the Evidence Store as sensitive data
- Evidence is not a complete workspace archive
- Official Tools enforce strict concrete argument decoders; QED has no general-purpose JSON Schema validation engine
- `git_diff` does not include untracked file content
- Shared token and cost limits depend on Provider-reported usage, which may be late or absent
- The TUI is a single-turn interface, not a persistent chat client
- A built-in HTTP service, GitHub Actions adapter, SQLite Session Store, and WebAssembly backend are not implemented; existing servers can embed `qed.Host`
- Compatibility with every third-party OpenAI-compatible API is not guaranteed
- `openai-codex` follows an experimental ChatGPT backend contract and currently uses full Responses over SSE without model discovery, Responses Lite, or WebSocket transport

## License

[MIT](LICENSE)
