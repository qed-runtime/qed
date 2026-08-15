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
| `apply_patch` | `qed.workspace` | `filesystem.write` | Apply a bounded counted or marker patch when every path precondition matches |
| `run_command` | `qed.process` | `process.execute` | Run one executable directly without shell evaluation |
| `git_status` | `qed.git` | `git.read` | Return bounded porcelain v2 status |
| `git_diff` | `qed.git` | `git.read` | Return a bounded worktree, staged, or base-relative patch, including bounded untracked content for worktree and base |

Deleting through `apply_patch` additionally requires `filesystem.delete`.
Official Tool arguments use strict bounded decoders that reject unknown fields,
duplicate keys, trailing values, invalid types, and Tool-specific limit
violations

`apply_patch` accepts counted unified diffs with either `--- a/path` plus
`+++ b/path` or equivalent workspace-relative headers. It also accepts a safe
`*** Begin Patch` envelope with `*** Update File`, `*** Add File`, and
`*** Delete File` sections. Marker update hunks may use counted unified headers
or `@@` followed by exact context, deletion, and addition lines. Uncounted old
lines must identify one location; ambiguous matches are rejected without
mutation. Marker moves are not supported. Both forms still require exactly one
current digest or absence precondition for every changed path

Testing does not require a dedicated Test Extension. A Profile exposes the
generic `run_command` Tool from `qed.process`, while the Host Policy limits the
allowed executable, arguments, directory, and capabilities for that use case.
Permission decisions are not model-facing Extension components: the Host
Policy and optional Approver decide before dispatch, so an Extension never
authorizes its own call

The QED `extensions.lock` selects these three for self-exec. Additional linked
or external Extensions attached to the same Profile may contribute Tools and
Hooks. A Run acquires all configured Extensions as one generation set

## Current World State

The Profile constructs a read-only `agent.CurrentWorldStateSource` by default
Declarative Agent graphs connect it to every Agent that references the Profile
Before each logical Provider request, it reconstructs bounded current state
instead of asking the model to trust older prose

- relevant regular workspace files are represented by current byte count and
  SHA-256 digest, including project-context files, paths observed by file Tools,
  and current Git changes; oversized, concurrently changed, or non-regular
  paths are retained as `unsupported`, while missing paths are `absent`
- current worktree status and a bounded diff digest come from the same
  read-only Git implementation as the official Git Extension
- prior structured `run_command` results retain exact command identity, exit
  status, output digest, and `current`, `stale`, or `unverified` freshness
- a later known mutation makes an earlier check stale; an unknown successful
  Tool makes it unverified

The Source never reruns a command or copies file content, diff text, stdout, or
stderr into the snapshot. Paths and command arguments remain observable
metadata and must be protected like Session Events. Each canonical read is
checked against the Profile Policy and the optional Run capability set. An
`ask` outcome does not open an approval prompt for background capture; that
scope is reported unavailable. A Policy evaluation error fails the Run

Capture is bounded and not an atomic operating-system snapshot across external
processes. The defaults retain at most 512 files, 1,024 Git changes, and 64
command identities, hash at most 16 MiB per file and 64 MiB per capture, and
then enforce the Runtime's 256 KiB encoded snapshot limit. Programmatic callers
can change the Profile limits or set `CurrentWorldState.Disabled`; declarative
configuration currently uses these defaults

## Safe context operation annotations

Official mutating and process Tools attach content-free operation metadata to
successful RPC results. `apply_patch` reports `mutation`. `run_command` reports
`commit` for direct `git commit` invocations, `verification` for a conservative
allowlist of direct test, check, lint, vet, and typecheck commands, and
`mutation` for every other command. Path-qualified executables are treated as
mutation rather than inferred from their basename

The orchestration subagent Tool reports `subagent` after its delegated run
returns. Runtime validates and persists these annotations without showing them
to the model. They only constrain Context Checkpoint cuts: a verification or
commit attempt closes a preceding mutation range even when the command reports
failure. Policy, approval, Evidence, and Current World State remain the sources
of authorization and observed outcome

## Subagent result reduction

`Profile.ResultReducer` returns the Coding Profile implementation of
`orchestration.ResultReducer`. Declarative Agent graphs install it on every
Agent that references the Profile

The reducer starts from deterministic Artifact and Execution Ledger entries,
then projects canonical Current World State into `coding.git_change` Facts,
`coding.file` and `coding.git_diff` Artifacts, and `coding.check` Executions. It
also places the complete bounded Current World State in versioned Profile state
The generic Result Packet validates source Event references and identities but
does not interpret these coding-specific kinds

Only checks executed by the candidate Run become `coding.check` Executions
Checks retained from earlier Session Runs remain available in Profile state but
are not relabeled as work performed by the candidate

This projection does not copy file contents, diff text, stdout, or stderr
Facts, paths, command names, and Profile state are still content-bearing
untrusted data shown to a parent model. Hosts must protect them like Session
Events and must not place secrets in a custom reducer result

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
  "Fix the failing test and run the relevant checks"
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

`apply_patch` and `run_command` implement `extension.ApprovalPreviewer`. Before
asking, they validate the proposed patch or command without executing it. The
Host validates the bounded preview and binds it to the exact argument digest,
Extension ID, and generation. Patch previews list add, update, and delete
targets with aggregate line counts. Command previews show exact JSON argv, the
workspace-relative working directory, and effective timeout. Raw Tool arguments
are not copied into the approval wait payload

The preview is intended for an interactive decision, not for logging. It may
contain workspace paths or command arguments and therefore belongs under the
same protection as Session Events. A stale patch can still fail after approval
because `apply_patch` rechecks every precondition under the write lock

## Go API

For a declarative server integration, prefer `qed.LoadHost` with an
application-owned self-exec Catalog as described in [Embedding QED](embedding.md)

A lower-level embedding host may instead supply explicit Extension commands
and own their lifetime directly

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
				Args: []string{selfexec.ChildArgument, workspaceextension.ID},
			},
		},
		{
			ID: processextension.ID,
			Command: host.Command{
				Path: qedExecutable,
				Args: []string{selfexec.ChildArgument, processextension.ID},
			},
		},
		{
			ID: gitextension.ID,
			Command: host.Command{
				Path: qedExecutable,
				Args: []string{selfexec.ChildArgument, gitextension.ID},
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
	Provider:                modelProvider,
	ComponentSource:         codingProfile.ComponentSource(),
	CurrentWorldStateSource: codingProfile.CurrentWorldStateSource(),
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
Store, verbose logger, lifecycle timeouts, project-context limits, Current
World State limits, and limits for each official Tool implementation. Every
`host.Command.Path` must be absolute

`ComponentSource` pins Tools and Hooks from every Extension until the Run ends.
`ToolSource` remains available for Tool-only integrations. Use
`AcquireCommands` for host-invoked Commands. A lower-level Runtime must pass
`CurrentWorldStateSource` explicitly; omitting it disables capture for that
Runtime

Reload one Extension for future Runs

```go
generation, err := codingProfile.Reload(
	ctx,
	workspaceextension.ID,
	host.Command{
		Path: replacementExecutable,
		Args: []string{selfexec.ChildArgument, workspaceextension.ID},
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

`git_diff` treats each requested path literally. The `worktree` and `base`
scopes append new-file patches for untracked regular files that pass standard
Git ignore rules and the requested path filter. The `staged` scope remains
index-only. Tracked and untracked patches share one `MaxOutputBytes` limit,
untracked file enumeration has a fixed count limit, and the digest covers the
exact combined patch returned to the caller. During untracked discovery,
symlinks and non-regular files are not read, binary data is represented by
Git's binary marker, and any skipped or incomplete discovery sets `truncated`

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

A non-zero exit or timeout keeps the structured command response but marks the
Tool result as an error. Providers and Evidence Bundles can therefore distinguish
a failed check from a successful command

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

## Protected live model verification

An opt-in integration test verifies the complete write path against a real
ChatGPT Codex model

```sh
QED_LIVE_CODING_E2E=1 \
QED_LIVE_CODING_AUTH_PROFILE=personal \
QED_LIVE_CODING_MODEL=MODEL_ID \
go test -mod=readonly -count=1 -v \
  -run '^TestLiveCodingProfileWritesTemporaryWorkspace$' \
  ./profile/coding
```

Log in with `qed auth login --auth-profile personal` before running the test
and select a model available to that profile. The test is skipped unless the
opt-in variable is exactly `1`; the profile and model variables have no implicit
defaults. A live run can consume subscription quota and its success depends on
the selected model and Provider service

Verbose test output prints a content-free timeline for each Provider request,
message boundary, Tool execution, and terminal Event. It includes sequential
Provider Call numbers, elapsed time, Tool names from the fixed allowlist,
Tool error flags, fixed Tool error classes, and token counts. Tool error classes
distinguish protected Policy rejection, invalid arguments, invalid patch syntax,
precondition failure, patch conflict, cancellation, and unclassified execution
failure without reproducing the underlying output. The timeline excludes
prompts, text deltas, model output, Tool arguments, Tool output, response IDs,
and credential values. The final failure line retains the existing Runtime
error, but the timeline does not duplicate its Provider error text. A failed Run
also reports aggregate Provider and Tool Call counts, token usage, Evidence Tool
count, and the last Event

The test constructs a synthetic Git repository under the test runner's
temporary directory. Model-facing Tools never read the QED worktree. The
host-side Policy accepts only reads of four synthetic files, one preconditioned
update of `calc.go`, the exact `go test ./...` command, and read-only
`git_status` and `git_diff` calls. Before command execution, the host confirms
that every source file has one of the known safe contents. The command
environment disables Go module downloads, checksum service access, telemetry,
VCS access, CGO, and toolchain downloads

The model may recover from non-mutating `apply_patch` syntax and precondition
failures within the Runtime budgets. The default repeated-Tool guard stops the
Run after four failed `apply_patch` calls without an intervening successful
call. Evidence validation retains every attempt,
requires exactly one successful change plus successful command and Git
inspection results, and checks that each failure occurred at a consistent
validation or Policy stage. Attempts to change another path, perform an
unsupported file operation, use an unexpected Tool, or continue after an
unclassified failure still fail the test

Only the configured model request and any authentication refresh use the
network. Tool-driven network access, arbitrary commands, Git writes, additional
files, absolute paths, and credential capabilities are denied. ChatGPT
credentials stay in the Host and are not included in the complete child
Extension environment. The test also confirms unchanged Git `HEAD`, the exact
final worktree state, and a private Evidence Bundle save/load round trip in
another temporary directory

## Current limitations

- Extension restart never replays the interrupted Tool call and restores only
  the latest host-owned Snapshot
- official Tool enforcement uses concrete strict decoders rather than a general JSON Schema engine
- no standard network, Git write, GitHub, host-wide filesystem, or shell-expansion Tool is included
- multiple Extension processes and unrelated host processes require digest preconditions rather than one global filesystem lock
- process execution requires an external security boundary when host-account authority is too broad
