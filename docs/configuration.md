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
      "token_env": "PRIMARY_API_TOKEN"
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
    "path": ".qed/evidence"
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
| `evidence` | no | JSON Evidence Bundle Store |
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

`echo` accepts no endpoint, model, credential, or API options

A custom endpoint may omit `token_env` for an unauthenticated local service.
Configuration never falls back to `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, or
`QED_API_KEY`. When `token_env` is present, the credential is resolved for each
HTTP request, which permits rotation by an embedding host

The Provider profile ID is part of Provider identity and opaque continuation
state, preventing state from one endpoint/profile from being reused by another

`openai-codex` reads a separately stored ChatGPT OAuth profile and uses the
fixed ChatGPT Codex backend. It requires only `protocol`, `model`, and
`auth_profile`; `base_url`, `token_env`, `max_output_tokens`, and `api_version`
are rejected. The named profile must already exist when configuration is
loaded

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

## Agent definitions and delegation

| Field | Required | Meaning |
| --- | --- | --- |
| `provider` | yes | Provider profile ID |
| `profile` | no | Execution Profile ID |
| `instructions` | no | Base instructions for this Agent |
| `max_provider_calls` | no | Runtime-local Provider call limit |
| `max_tool_calls` | no | Runtime-local Tool call limit |
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

Shared limits default to 16 Agent Runs, depth 4, and 64 Provider calls. Parent,
candidate, and judge Runs count against the same top-level budget

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
be resumed by a later CLI process

`--session-id` requires a configured Session Store. The Runtime appends public
Events and reconstructs messages, pending waits, and pending Tool calls. Use
`qed session resume <id> --config <path>` for a persisted wait. Approval resume
accepts `--approval prompt|approve|deny`; other wait kinds require
`--response-json`

## Evidence Store

```json
{
  "evidence": {"store": "json", "path": ".qed/evidence"}
}
```

Configured CLI and TUI Runs save one versioned Bundle after terminal completion,
including public Events, usage, config/workspace digests, and host-owned Tool
trace records. Inspect or export with either command family

Tool trace payloads are represented by digests, but public Events retain their
normal observable payload. A Bundle may therefore contain prompts, assistant
messages, Tool arguments, Tool output, wait payloads, and errors. Store it with
the same care as Session data

```sh
qed run inspect <run-id> --store .qed/evidence
qed run export <run-id> --store .qed/evidence
qed evidence inspect <run-id> --store .qed/evidence
qed evidence export <run-id> --store .qed/evidence
```

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
