package coding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/extensions/edit"
	"github.com/qed-runtime/qed/extensions/filesystem"
	gitextension "github.com/qed-runtime/qed/extensions/git"
	processextension "github.com/qed-runtime/qed/extensions/process"
	"github.com/qed-runtime/qed/internal/jsonstrict"
	"github.com/qed-runtime/qed/workspace"
)

const (
	defaultMaxWorldStateFiles          = 512
	defaultMaxWorldStateGitChanges     = 1024
	defaultMaxWorldStateChecks         = 64
	defaultMaxWorldStateFileBytes      = 16 << 20
	defaultMaxWorldStateTotalFileBytes = 64 << 20
	maximumWorldStateToolJSONBytes     = 2 << 20
	maximumWorldStateValueBytes        = 4096
)

type currentWorldStateSource struct {
	workspace         *workspace.Workspace
	policy            capability.Policy
	gitStatus         agent.Tool
	gitDiff           agent.Tool
	seedPaths         []string
	maxFiles          int
	maxGitChanges     int
	maxChecks         int
	maxFileBytes      int64
	maxTotalFileBytes int64
}

type observedWorldFile struct {
	ref    agent.ContextLedgerEventRef
	status agent.CurrentWorldFileStatus
	digest string
}

type observedWorldGit struct {
	ref       agent.ContextLedgerEventRef
	digest    string
	bytes     int64
	truncated bool
}

type observedWorldCheck struct {
	check agent.CurrentWorldCheck
	index int
}

type worldStateGitStatus struct {
	Branch struct {
		Head     string `json:"head,omitempty"`
		OID      string `json:"oid,omitempty"`
		Upstream string `json:"upstream,omitempty"`
		Ahead    int    `json:"ahead,omitempty"`
		Behind   int    `json:"behind,omitempty"`
	} `json:"branch"`
	Entries []agent.CurrentWorldGitChange `json:"entries"`
}

type worldStateGitDiff struct {
	Scope     string `json:"scope"`
	Base      string `json:"base,omitempty"`
	Patch     string `json:"patch"`
	Digest    string `json:"digest"`
	Truncated bool   `json:"truncated"`
}

type worldStateCommandInput struct {
	Argv []string `json:"argv"`
	CWD  string   `json:"cwd,omitempty"`
}

type worldStateCommandOutput struct {
	Argv            []string `json:"argv"`
	CWD             string   `json:"cwd"`
	ExitCode        int      `json:"exit_code"`
	Success         bool     `json:"success"`
	StdoutTruncated bool     `json:"stdout_truncated"`
	StderrTruncated bool     `json:"stderr_truncated"`
	TimedOut        bool     `json:"timed_out"`
}

func newCurrentWorldStateSource(
	scoped *workspace.Workspace,
	policy capability.Policy,
	gitOptions gitextension.Options,
	environment map[string]string,
	options CurrentWorldStateOptions,
) (*currentWorldStateSource, error) {
	if scoped == nil || policy == nil {
		return nil, errors.New("Current World State requires Workspace and Policy")
	}
	gitOptions.Environment = cloneEnvironment(environment)
	gitTools, err := gitextension.NewTools(scoped, gitOptions)
	if err != nil {
		return nil, err
	}
	var statusTool agent.Tool
	var diffTool agent.Tool
	for _, tool := range gitTools {
		switch tool.Definition().Name {
		case gitextension.StatusToolName:
			statusTool = tool
		case gitextension.DiffToolName:
			diffTool = tool
		}
	}
	if statusTool == nil || diffTool == nil {
		return nil, errors.New("Current World State Git Tools are incomplete")
	}
	maxFiles := options.MaxFiles
	if maxFiles == 0 {
		maxFiles = defaultMaxWorldStateFiles
	}
	maxGitChanges := options.MaxGitChanges
	if maxGitChanges == 0 {
		maxGitChanges = defaultMaxWorldStateGitChanges
	}
	maxChecks := options.MaxChecks
	if maxChecks == 0 {
		maxChecks = defaultMaxWorldStateChecks
	}
	maxFileBytes := options.MaxFileBytes
	if maxFileBytes == 0 {
		maxFileBytes = defaultMaxWorldStateFileBytes
	}
	maxTotalFileBytes := options.MaxTotalFileBytes
	if maxTotalFileBytes == 0 {
		maxTotalFileBytes = defaultMaxWorldStateTotalFileBytes
	}
	if maxFiles < 1 || maxFiles > agent.MaxCurrentWorldStateFiles ||
		maxGitChanges < 1 || maxGitChanges > agent.MaxCurrentWorldStateGitChanges ||
		maxChecks < 1 || maxChecks > agent.MaxCurrentWorldStateChecks ||
		maxFileBytes < 1 || maxTotalFileBytes < 1 {
		return nil, errors.New("Current World State limits must be positive and within Runtime schema bounds")
	}
	return &currentWorldStateSource{
		workspace:         scoped,
		policy:            policy,
		gitStatus:         statusTool,
		gitDiff:           diffTool,
		seedPaths:         currentWorldSeedPaths(scoped),
		maxFiles:          maxFiles,
		maxGitChanges:     maxGitChanges,
		maxChecks:         maxChecks,
		maxFileBytes:      maxFileBytes,
		maxTotalFileBytes: maxTotalFileBytes,
	}, nil
}

func (source *currentWorldStateSource) Snapshot(
	ctx context.Context,
	request agent.CurrentWorldStateRequest,
) (agent.CurrentWorldStateSnapshot, error) {
	if source == nil || source.workspace == nil || source.policy == nil {
		return agent.CurrentWorldStateSnapshot{}, errors.New("Current World State Source is not configured")
	}
	if ctx == nil {
		return agent.CurrentWorldStateSnapshot{}, errors.New("Current World State context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return agent.CurrentWorldStateSnapshot{}, err
	}

	paths, observations, gitObservation, checks, checksTruncated, err := source.reduceEvents(ctx, request.Events)
	if err != nil {
		return agent.CurrentWorldStateSnapshot{}, err
	}
	for _, seed := range source.seedPaths {
		paths[seed] = struct{}{}
	}

	gitState, gitPaths, err := source.snapshotGit(ctx, request.Run, gitObservation)
	if err != nil {
		return agent.CurrentWorldStateSnapshot{}, err
	}
	for _, gitPath := range gitPaths {
		paths[gitPath] = struct{}{}
	}

	files, filesAvailable, filesTruncated, err := source.snapshotFiles(ctx, request.Run, paths, observations)
	if err != nil {
		return agent.CurrentWorldStateSnapshot{}, err
	}
	currentChecks, err := source.snapshotChecks(ctx, request.Events, checks)
	if err != nil {
		return agent.CurrentWorldStateSnapshot{}, err
	}
	snapshot := agent.CurrentWorldStateSnapshot{
		FilesAvailable:  filesAvailable,
		Files:           files,
		FilesTruncated:  filesTruncated,
		Git:             gitState,
		Checks:          currentChecks,
		ChecksTruncated: checksTruncated,
	}
	return boundCurrentWorldStateSnapshot(snapshot), nil
}

func (source *currentWorldStateSource) reduceEvents(ctx context.Context, events []agent.Event) (
	map[string]struct{},
	map[string]observedWorldFile,
	*observedWorldGit,
	[]observedWorldCheck,
	bool,
	error,
) {
	paths := make(map[string]struct{})
	observations := make(map[string]observedWorldFile)
	checksByIdentity := make(map[string]observedWorldCheck)
	var gitObservation *observedWorldGit
	for index, event := range events {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, nil, false, err
		}
		if event.Type != agent.EventToolCompleted || event.ToolCall == nil || event.ToolResult == nil {
			continue
		}
		ref := worldStateEventRef(event)
		switch event.ToolCall.Name {
		case filesystem.ReadFileToolName:
			var input struct {
				Path string `json:"path"`
			}
			if decodeWorldJSON(event.ToolCall.Arguments, &input) == nil {
				if normalized, ok := currentWorldPath(input.Path); ok {
					paths[normalized] = struct{}{}
				}
			}
			if event.ToolResult.IsError {
				continue
			}
			var output struct {
				Path   string `json:"path"`
				Digest string `json:"digest"`
			}
			if decodeWorldJSON([]byte(event.ToolResult.Output), &output) != nil {
				continue
			}
			if normalized, ok := currentWorldPath(output.Path); ok {
				paths[normalized] = struct{}{}
				observations[normalized] = observedWorldFile{ref: ref, status: agent.CurrentWorldFilePresent, digest: output.Digest}
			}

		case filesystem.SearchTextToolName:
			if event.ToolResult.IsError {
				continue
			}
			var output struct {
				Matches []struct {
					Path string `json:"path"`
				} `json:"matches"`
			}
			if decodeWorldJSON([]byte(event.ToolResult.Output), &output) != nil {
				continue
			}
			for _, match := range output.Matches {
				if normalized, ok := currentWorldPath(match.Path); ok {
					paths[normalized] = struct{}{}
				}
			}

		case edit.ApplyPatchToolName:
			if event.ToolResult.IsError {
				continue
			}
			var output struct {
				Changes []struct {
					Path        string `json:"path"`
					Kind        string `json:"kind"`
					AfterDigest string `json:"after_digest,omitempty"`
				} `json:"changes"`
			}
			if decodeWorldJSON([]byte(event.ToolResult.Output), &output) != nil {
				continue
			}
			for _, change := range output.Changes {
				normalized, ok := currentWorldPath(change.Path)
				if !ok {
					continue
				}
				paths[normalized] = struct{}{}
				observation := observedWorldFile{ref: ref, status: agent.CurrentWorldFilePresent, digest: change.AfterDigest}
				if change.Kind == "delete" {
					observation.status = agent.CurrentWorldFileAbsent
					observation.digest = ""
				}
				observations[normalized] = observation
			}

		case gitextension.DiffToolName:
			if event.ToolResult.IsError || !comparableWorldGitDiff(event.ToolCall.Arguments) {
				continue
			}
			var output worldStateGitDiff
			if decodeWorldJSON([]byte(event.ToolResult.Output), &output) != nil || output.Scope != "worktree" || output.Base != "" {
				continue
			}
			gitObservation = &observedWorldGit{
				ref: ref, digest: output.Digest, bytes: int64(len(output.Patch)), truncated: output.Truncated,
			}

		case processextension.RunCommandToolName:
			check, ok := decodeWorldCheck(event, index)
			if ok {
				checksByIdentity[worldCheckIdentity(check.check)] = check
			}
		}
	}
	checks := make([]observedWorldCheck, 0, len(checksByIdentity))
	for _, check := range checksByIdentity {
		checks = append(checks, check)
	}
	sort.Slice(checks, func(first, second int) bool { return checks[first].index > checks[second].index })
	checksTruncated := len(checks) > source.maxChecks
	if checksTruncated {
		checks = checks[:source.maxChecks]
	}
	return paths, observations, gitObservation, checks, checksTruncated, nil
}

func (source *currentWorldStateSource) snapshotFiles(
	ctx context.Context,
	run agent.RunInfo,
	paths map[string]struct{},
	observations map[string]observedWorldFile,
) ([]agent.CurrentWorldFile, bool, bool, error) {
	ordered := make([]string, 0, len(paths))
	for candidate := range paths {
		ordered = append(ordered, candidate)
	}
	sort.Strings(ordered)
	truncated := false
	if len(ordered) > source.maxFiles {
		ordered = ordered[:source.maxFiles]
		truncated = true
	}
	files := make([]agent.CurrentWorldFile, 0, len(ordered))
	remaining := source.maxTotalFileBytes
	authorized := false
	for _, filePath := range ordered {
		arguments, _ := json.Marshal(struct {
			Path string `json:"path"`
		}{Path: filePath})
		allowed, err := source.authorized(ctx, run, filesystem.ReadFileToolName, capability.FilesystemRead, arguments)
		if err != nil {
			return nil, false, false, err
		}
		if !allowed {
			truncated = true
			continue
		}
		authorized = true
		file, consumed, err := source.snapshotFile(ctx, filePath, remaining)
		if err != nil {
			return nil, false, false, err
		}
		remaining -= consumed
		if observed, exists := observations[filePath]; exists {
			file.Observation = &agent.CurrentWorldObservation{
				Source:  observed.ref,
				Matches: file.Status == observed.status && file.Digest == observed.digest,
			}
		}
		files = append(files, file)
	}
	return files, authorized || len(ordered) == 0, truncated, nil
}

func (source *currentWorldStateSource) snapshotFile(
	ctx context.Context,
	filePath string,
	remaining int64,
) (agent.CurrentWorldFile, int64, error) {
	current := agent.CurrentWorldFile{Path: filePath, Status: agent.CurrentWorldFileUnsupported}
	info, err := source.workspace.Lstat(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			current.Status = agent.CurrentWorldFileAbsent
		}
		if ctx.Err() != nil {
			return agent.CurrentWorldFile{}, 0, ctx.Err()
		}
		return current, 0, nil
	}
	if !info.Mode().IsRegular() || info.Size() > source.maxFileBytes || info.Size() > remaining {
		return current, 0, nil
	}
	opened, err := source.workspace.Open(filePath)
	if err != nil {
		return current, 0, nil
	}
	hash := sha256.New()
	written, readErr := io.Copy(hash, io.LimitReader(currentWorldContextReader{ctx: ctx, reader: opened}, source.maxFileBytes))
	openedInfo, statErr := opened.Stat()
	closeErr := opened.Close()
	if err := ctx.Err(); err != nil {
		return agent.CurrentWorldFile{}, 0, err
	}
	currentInfo, currentErr := source.workspace.Lstat(filePath)
	if readErr != nil || statErr != nil || closeErr != nil || currentErr != nil || written > source.maxFileBytes ||
		written != info.Size() || openedInfo.Size() != info.Size() || !os.SameFile(info, openedInfo) ||
		!os.SameFile(info, currentInfo) || !openedInfo.ModTime().Equal(currentInfo.ModTime()) {
		return current, 0, nil
	}
	current.Status = agent.CurrentWorldFilePresent
	current.Digest = "sha256:" + hex.EncodeToString(hash.Sum(nil))
	current.Bytes = written
	return current, written, nil
}

func (source *currentWorldStateSource) snapshotGit(
	ctx context.Context,
	run agent.RunInfo,
	observed *observedWorldGit,
) (*agent.CurrentWorldGitState, []string, error) {
	statusAllowed, err := source.authorized(ctx, run, gitextension.StatusToolName, capability.GitRead, json.RawMessage(`{}`))
	if err != nil {
		return nil, nil, err
	}
	diffAllowed, err := source.authorized(ctx, run, gitextension.DiffToolName, capability.GitRead, json.RawMessage(`{}`))
	if err != nil {
		return nil, nil, err
	}
	if !statusAllowed || !diffAllowed {
		return &agent.CurrentWorldGitState{Available: false}, nil, nil
	}
	statusResult, err := source.gitStatus.Execute(ctx, agent.ToolCall{Name: gitextension.StatusToolName, Arguments: json.RawMessage(`{}`)})
	if err != nil || statusResult.IsError {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return &agent.CurrentWorldGitState{Available: false}, nil, nil
	}
	diffResult, err := source.gitDiff.Execute(ctx, agent.ToolCall{Name: gitextension.DiffToolName, Arguments: json.RawMessage(`{}`)})
	if err != nil || diffResult.IsError {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return &agent.CurrentWorldGitState{Available: false}, nil, nil
	}
	var status worldStateGitStatus
	var diff worldStateGitDiff
	if decodeWorldJSON([]byte(statusResult.Output), &status) != nil || decodeWorldJSON([]byte(diffResult.Output), &diff) != nil ||
		diff.Scope != "worktree" || diff.Base != "" {
		return &agent.CurrentWorldGitState{Available: false}, nil, nil
	}
	sort.Slice(status.Entries, func(first, second int) bool {
		if status.Entries[first].Path != status.Entries[second].Path {
			return status.Entries[first].Path < status.Entries[second].Path
		}
		return status.Entries[first].OriginalPath < status.Entries[second].OriginalPath
	})
	truncated := false
	if len(status.Entries) > source.maxGitChanges {
		status.Entries = status.Entries[:source.maxGitChanges]
		truncated = true
	}
	state := &agent.CurrentWorldGitState{
		Available:        true,
		Head:             status.Branch.Head,
		OID:              status.Branch.OID,
		Upstream:         status.Branch.Upstream,
		Ahead:            status.Branch.Ahead,
		Behind:           status.Branch.Behind,
		Changes:          status.Entries,
		ChangesTruncated: truncated,
		DiffDigest:       diff.Digest,
		DiffBytes:        int64(len(diff.Patch)),
		DiffTruncated:    diff.Truncated,
	}
	if observed != nil {
		state.Observation = &agent.CurrentWorldObservation{
			Source: observed.ref,
			Matches: observed.digest == state.DiffDigest && observed.bytes == state.DiffBytes &&
				observed.truncated == state.DiffTruncated,
		}
	}
	paths := make([]string, 0, len(status.Entries)*2)
	for _, entry := range status.Entries {
		if normalized, ok := currentWorldPath(entry.Path); ok {
			paths = append(paths, normalized)
		}
		if normalized, ok := currentWorldPath(entry.OriginalPath); ok {
			paths = append(paths, normalized)
		}
	}
	return state, paths, nil
}

func (source *currentWorldStateSource) snapshotChecks(
	ctx context.Context,
	events []agent.Event,
	checks []observedWorldCheck,
) ([]agent.CurrentWorldCheck, error) {
	result := make([]agent.CurrentWorldCheck, 0, len(checks))
	for _, observed := range checks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		check := observed.check
		check.Freshness = agent.CurrentWorldCheckCurrent
		for _, event := range events[observed.index+1:] {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			knownMutation, uncertain := worldStateMutation(event)
			if knownMutation {
				check.Freshness = agent.CurrentWorldCheckStale
				break
			}
			if uncertain {
				check.Freshness = agent.CurrentWorldCheckUnverified
			}
		}
		result = append(result, check)
	}
	return result, nil
}

type currentWorldContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader currentWorldContextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func (source *currentWorldStateSource) authorized(
	ctx context.Context,
	run agent.RunInfo,
	tool string,
	name capability.Name,
	arguments json.RawMessage,
) (bool, error) {
	if len(run.Capabilities) > 0 {
		allowed := false
		for _, configured := range run.Capabilities {
			if configured == string(name) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false, nil
		}
	}
	decision, err := source.policy.Evaluate(ctx, capability.Request{
		Tool: tool, Capabilities: []capability.Name{name}, Arguments: append(json.RawMessage(nil), arguments...),
	})
	if err != nil {
		return false, fmt.Errorf("evaluate Current World State Policy: %w", err)
	}
	return decision.Outcome == capability.OutcomeAllow, nil
}

func decodeWorldCheck(event agent.Event, index int) (observedWorldCheck, bool) {
	var input worldStateCommandInput
	var output worldStateCommandOutput
	if decodeWorldJSON(event.ToolCall.Arguments, &input) != nil || decodeWorldJSON([]byte(event.ToolResult.Output), &output) != nil {
		return observedWorldCheck{}, false
	}
	if input.CWD == "" {
		input.CWD = "."
	}
	if len(input.Argv) == 0 || len(input.Argv) > agent.MaxCurrentWorldStateArgv ||
		!validWorldArgv(input.Argv) || !equalWorldStrings(input.Argv, output.Argv) ||
		!validWorldCWD(input.CWD) || input.CWD != output.CWD ||
		output.Success != (output.ExitCode == 0 && !output.TimedOut) || event.ToolResult.IsError == output.Success {
		return observedWorldCheck{}, false
	}
	status := agent.CurrentWorldCheckFailed
	if output.Success {
		status = agent.CurrentWorldCheckPassed
	}
	return observedWorldCheck{index: index, check: agent.CurrentWorldCheck{
		Argv:            append([]string(nil), output.Argv...),
		CWD:             output.CWD,
		Status:          status,
		ExitCode:        output.ExitCode,
		TimedOut:        output.TimedOut,
		OutputDigest:    worldStateDigest([]byte(event.ToolResult.Output)),
		OutputTruncated: output.StdoutTruncated || output.StderrTruncated,
		Source:          worldStateEventRef(event),
	}}, true
}

func worldStateMutation(event agent.Event) (known bool, uncertain bool) {
	if event.Type != agent.EventToolCompleted || event.ToolCall == nil || event.ToolResult == nil {
		return false, false
	}
	if event.ToolCall.Name == processextension.RunCommandToolName {
		return true, false
	}
	if event.ToolCall.Name == edit.ApplyPatchToolName {
		return !event.ToolResult.IsError, false
	}
	if event.ToolResult.IsError {
		return false, false
	}
	if event.ToolResult.Policy == nil {
		switch event.ToolCall.Name {
		case filesystem.ReadFileToolName, filesystem.SearchTextToolName,
			gitextension.StatusToolName, gitextension.DiffToolName:
			return false, false
		default:
			return false, true
		}
	}
	for _, name := range event.ToolResult.Policy.Capabilities {
		switch capability.Name(name) {
		case capability.FilesystemRead, capability.GitRead:
		default:
			return true, false
		}
	}
	return false, false
}

func comparableWorldGitDiff(arguments json.RawMessage) bool {
	var input struct {
		Scope        string   `json:"scope,omitempty"`
		Base         string   `json:"base,omitempty"`
		Paths        []string `json:"paths,omitempty"`
		ContextLines *int     `json:"context_lines,omitempty"`
	}
	if decodeWorldJSON(arguments, &input) != nil {
		return false
	}
	if input.Scope == "" {
		input.Scope = "worktree"
	}
	return input.Scope == "worktree" && input.Base == "" && len(input.Paths) == 0 &&
		(input.ContextLines == nil || *input.ContextLines == 3)
}

func currentWorldSeedPaths(scoped *workspace.Workspace) []string {
	paths := []string{"AGENTS.md", "CONTRIBUTING.md", "QED.md", "README.md"}
	matches, _ := filepath.Glob(filepath.Join(scoped.Root(), ".qed", "context", "*.md"))
	for _, match := range matches {
		if relative, err := scoped.Relative(match); err == nil {
			paths = append(paths, relative)
		}
	}
	result := paths[:0]
	seen := make(map[string]struct{}, len(paths))
	for _, candidate := range paths {
		normalized, ok := currentWorldPath(filepath.ToSlash(candidate))
		if !ok {
			continue
		}
		if _, duplicate := seen[normalized]; duplicate {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	sort.Strings(result)
	return result
}

func currentWorldPath(value string) (string, bool) {
	if value == "" || len(value) > maximumWorldStateValueBytes || !utf8.ValidString(value) ||
		strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || strings.IndexByte(value, 0) >= 0 {
		return "", false
	}
	normalized := path.Clean(value)
	if normalized != value || normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") {
		return "", false
	}
	return normalized, true
}

func validWorldArgv(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "" || len(argument) > maximumWorldStateValueBytes || !utf8.ValidString(argument) ||
			strings.IndexByte(argument, 0) >= 0 {
			return false
		}
	}
	return true
}

func validWorldCWD(value string) bool {
	if value == "." {
		return true
	}
	_, valid := currentWorldPath(value)
	return valid
}

func decodeWorldJSON(data []byte, target any) error {
	var validated any
	if err := jsonstrict.Decode(data, maximumWorldStateToolJSONBytes, &validated); err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func worldStateEventRef(event agent.Event) agent.ContextLedgerEventRef {
	return agent.ContextLedgerEventRef{
		RunID: event.RunID, Sequence: event.Sequence, SessionRevision: event.SessionRevision,
	}
}

func worldStateDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func worldCheckIdentity(check agent.CurrentWorldCheck) string {
	encoded, _ := json.Marshal(struct {
		Argv []string `json:"argv"`
		CWD  string   `json:"cwd"`
	}{Argv: check.Argv, CWD: check.CWD})
	return string(encoded)
}

func equalWorldStrings(first, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func boundCurrentWorldStateSnapshot(snapshot agent.CurrentWorldStateSnapshot) agent.CurrentWorldStateSnapshot {
	if currentWorldStateFits(snapshot) {
		return snapshot
	}
	if snapshot.Git != nil {
		gitState := *snapshot.Git
		snapshot.Git = &gitState
	}
	if len(snapshot.Checks) > 0 {
		checks := snapshot.Checks
		if retained, ok := maximumRetained(len(checks), func(count int) bool {
			candidate := snapshot
			candidate.Checks = checks[:count]
			candidate.ChecksTruncated = true
			return currentWorldStateFits(candidate)
		}); ok {
			snapshot.Checks = checks[:retained]
			snapshot.ChecksTruncated = true
			return snapshot
		}
		snapshot.Checks = nil
		snapshot.ChecksTruncated = true
	}
	if snapshot.Git != nil && len(snapshot.Git.Changes) > 0 {
		changes := snapshot.Git.Changes
		if retained, ok := maximumRetained(len(changes), func(count int) bool {
			candidate := snapshot
			gitState := *snapshot.Git
			gitState.Changes = changes[:count]
			gitState.ChangesTruncated = true
			candidate.Git = &gitState
			return currentWorldStateFits(candidate)
		}); ok {
			snapshot.Git.Changes = changes[:retained]
			snapshot.Git.ChangesTruncated = true
			return snapshot
		}
		snapshot.Git.Changes = nil
		snapshot.Git.ChangesTruncated = true
	}
	if len(snapshot.Files) > 0 {
		files := snapshot.Files
		if retained, ok := maximumRetained(len(files), func(count int) bool {
			candidate := snapshot
			candidate.Files = files[:count]
			candidate.FilesTruncated = true
			return currentWorldStateFits(candidate)
		}); ok {
			snapshot.Files = files[:retained]
			snapshot.FilesTruncated = true
			return snapshot
		}
		snapshot.Files = nil
		snapshot.FilesTruncated = true
	}
	return snapshot
}

func maximumRetained(length int, fits func(int) bool) (int, bool) {
	low := 0
	high := length - 1
	best := -1
	for low <= high {
		middle := low + (high-low)/2
		if fits(middle) {
			best = middle
			low = middle + 1
			continue
		}
		high = middle - 1
	}
	return best, best >= 0
}

func currentWorldStateFits(snapshot agent.CurrentWorldStateSnapshot) bool {
	encoded, err := json.Marshal(snapshot)
	return err == nil && len(encoded) <= agent.MaxCurrentWorldStateBytes
}
