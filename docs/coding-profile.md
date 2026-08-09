# Coding Profile

The standard Coding Profile adds workspace-aware coding behavior around the
provider-neutral Agent Runtime. It assembles bounded project context, one
capability Policy, host-owned Evidence, and one or more process-isolated
Extensions without adding coding behavior to `agent`

## Standard model-facing Tools

Three reusable official Extensions contribute six Tools

| Tool | Extension | Capability | Behavior |
| --- | --- | --- | --- |
| `search_text` | `qed.workspace` | `filesystem.read` | Search bounded UTF-8 files by literal or regular expression |
| `read_file` | `qed.workspace` | `filesystem.read` | Read a bounded line range and return the full-file SHA-256 digest |
| `apply_patch` | `qed.workspace` | `filesystem.write` | Apply a unified diff when every path precondition matches |
| `run_command` | `qed.process` | `process.execute` | Run one executable directly without shell evaluation |
| `git_status` | `qed.git` | `git.read` | Return bounded porcelain v2 status |
| `git_diff` | `qed.git` | `git.read` | Return a bounded worktree, staged, or base-relative patch |

Deleting through `apply_patch` additionally requires `filesystem.delete`.
Official Tool arguments use strict bounded decoders that reject unknown fields,
duplicate keys, trailing values, invalid types, and Tool-specific limit
violations

The QED `extensions.lock` selects these three for self-exec. Additional linked
or external Extensions attached to the same Profile may contribute Tools and
Hooks. A Run acquires all configured Extensions as one generation set

## Declarative configuration

Define Extensions and the Profile independently from Provider profiles

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
    "coding": {
      "provider": "model",
      "profile": "workspace",
      "instructions": "Make the smallest verified change"
    }
  },
  "session": {"store": "jsonl", "path": ".qed/sessions"},
  "evidence": {"store": "json", "path": ".qed/evidence"}
}
```

```sh
export MODEL_API_TOKEN="<token>"
go run ./cmd/qed run \
  --config ./qed.json \
  --workspace . \
  --session-id coding-1 \
  --prompt "Fix the failing test and run the relevant checks"
```

The workspace is an Adapter input rather than a machine-specific value in the
JSON file. It defaults to the current directory. Every configured environment
name must exist when loading the graph. Values are passed to the selected Tools
but are not added to model messages or verbose diagnostics. Do not select
credential variables

Capability outcomes

- `allow` executes without another decision
- `ask` calls the host-supplied Approver and may suspend the Run
- `deny`, and every unspecified capability, rejects the invocation

Non-interactive configuration defaults to `--approval deny`. Use
`--approval prompt` for serialized terminal approval. The TUI displays a wait
and accepts `Y` or `N`. Both paths resume through `RunHandle.Resume` and produce
`run.waiting` and `run.resumed` Events

## Go API

An embedding host supplies explicit Extension commands and owns their lifetime

```go
policy, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{
	Allow: []capability.Name{
		capability.FilesystemRead,
		capability.FilesystemWrite,
		capability.ProcessExecute,
		capability.GitRead,
	},
	Deny: []capability.Name{capability.FilesystemDelete},
})
if err != nil {
	log.Fatal(err)
}

codingProfile, err := coding.New(ctx, coding.Options{
	Root: workspaceRoot,
	Extensions: []coding.ExtensionOptions{
		{
			ID: workspaceextension.ID,
			Command: host.Command{
				Path: qedExecutable,
				Args: []string{"__extension", workspaceextension.ID},
			},
		},
		{
			ID: processextension.ID,
			Command: host.Command{
				Path: qedExecutable,
				Args: []string{"__extension", processextension.ID},
			},
		},
		{
			ID: gitextension.ID,
			Command: host.Command{
				Path: qedExecutable,
				Args: []string{"__extension", gitextension.ID},
			},
		},
	},
	Policy: policy,
	CommandEnvironment: map[string]string{
		"PATH": os.Getenv("PATH"),
		"HOME": os.Getenv("HOME"),
	},
})
if err != nil {
	log.Fatal(err)
}
defer codingProfile.Close()

runtime, err := agent.NewRuntime(agent.Options{
	Provider:        modelProvider,
	ComponentSource: codingProfile.ComponentSource(),
})
if err != nil {
	log.Fatal(err)
}

handle, err := runtime.Run(ctx, agent.RunRequest{
	AgentID:      "coding",
	Instructions: codingProfile.Instructions(),
	Input: []agent.Message{
		{Role: agent.RoleUser, Text: "Fix the failing test"},
	},
})
```

`coding.Options` also accepts an Approver, Evidence Recorder, Extension State
Store, verbose logger, lifecycle timeouts, project-context limits, and limits
for each official Tool implementation. Every `host.Command.Path` must be
absolute

`ComponentSource` pins Tools and Hooks from every Extension until the Run ends.
`ToolSource` remains available for Tool-only integrations. Use
`AcquireCommands` for host-invoked Commands

Reload one Extension for future Runs

```go
generation, err := codingProfile.Reload(
	ctx,
	workspaceextension.ID,
	host.Command{
		Path: replacementExecutable,
		Args: []string{"__extension", workspaceextension.ID},
	},
)
```

The candidate must pass protocol, identity, component, state Restore, and health
validation before publication. Existing Runs retain their old generation.
Each Extension reloads independently. `CurrentGenerations` reports the complete
set selected by the next Run

## Workspace and mutation safety

- model-facing file paths must be relative to one canonical workspace
- file open, removal, and replacement use traversal-resistant `os.Root` operations
- search skips `.git`, symlinks, non-regular files, oversized files, binary files, and invalid UTF-8
- `read_file` returns a full-file digest even for a partial line range
- `apply_patch` requires one current digest or `absent` precondition for every changed path
- the complete patch is prevalidated and every preimage is checked again before commit
- each replacement is atomic on supported filesystems; multi-file rollback is best effort and not crash-atomic
- Git Tools require workspace root to equal repository root
- Git optional locks, pagers, external diff drivers, text conversion, and global configuration are disabled

Workspace locks coordinate `search_text`, `read_file`, and `apply_patch` within
one `qed.workspace` process. The `qed.process` and `qed.git` processes do not
share that lock. Old and new processes can also overlap during reload, and
unrelated host processes may mutate files, so digest preconditions remain
mandatory

## Process execution boundary

`run_command` accepts argv, an optional workspace-relative working directory,
and an optional timeout. It starts the executable directly, captures stdout and
stderr separately, bounds both, reports exit code, and terminates the process
group after cancellation or timeout on Unix

Executable lookup uses only the Profile's selected `PATH`. It does not fall back
to Extension or Host process environment

This is not an OS sandbox. A permitted executable can access paths outside the
workspace, start programs, or use the network with host-account authority.
Grant `process.execute` only when acceptable and use an external sandbox or
container for stronger isolation

## Project context

The Profile eagerly loads bounded valid UTF-8 context from these root paths

- `QED.md`
- `AGENTS.md`
- `README.md`
- `CONTRIBUTING.md`
- `.qed/context/*.md`

Each section retains its relative path. Oversized, invalid, binary, or missing
files are skipped. Nested `AGENTS.md` files are not loaded eagerly; the standard
instruction tells the model to locate applicable nested files before editing

## Evidence, Sessions, and diagnostics

Every proxied Tool invocation records Run, parent Run, Agent, Session,
Extension, generation, call, capability, Policy, digest, timing, and error
metadata in the Host. Raw arguments and output are represented by SHA-256
digests in Tool trace records. If the Profile created its recorder,
`ToolInvocations` returns those records

Declarative configured Runs combine terminal result, public Events, and Tool
records into a persisted Evidence Bundle. Session persistence belongs to
Runtime and may use `session.MemoryStore` or `session.JSONLStore`; the Coding
Profile does not hide a second conversation store

Bundle Events retain public Run payloads and may include prompts, Tool arguments,
Tool output, and errors. Evidence storage is content-bearing even though the
host-owned Tool trace uses digests

Verbose mode propagates from CLI or `coding.Options.Verbose` through every
Extension Initialize request. Structured diagnostics omit project context,
prompts, file names supplied as Tool arguments, command output, environment
values, and credentials

## Current limitations

- Extension crash is isolated but is not automatically restarted
- `git_diff` does not include untracked file content
- official Tool enforcement uses concrete strict decoders rather than a general JSON Schema engine
- no standard network, Git write, GitHub, host-wide filesystem, or shell-expansion Tool is included
- multiple Extension processes and unrelated host processes require digest preconditions rather than one global filesystem lock
- process execution requires an external security boundary when host-account authority is too broad
