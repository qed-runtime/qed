// Package git provides read-only structured Git Tools scoped to one Workspace
package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/extensions/internal/command"
	"github.com/qed-runtime/qed/internal/jsonstrict"
	"github.com/qed-runtime/qed/workspace"
)

const (
	// StatusToolName is the model-facing name of the structured Git status Tool
	StatusToolName = "git_status"
	// DiffToolName is the model-facing name of the bounded Git diff Tool
	DiffToolName = "git_diff"

	defaultTimeout        = 30 * time.Second
	defaultMaxOutputBytes = 1 << 20
	maximumArgumentBytes  = 64 << 10
)

var objectIDPattern = regexp.MustCompile(`\A[0-9a-fA-F]{40,64}\z`)

// Options bounds Git Tool resource use
type Options struct {
	Timeout        time.Duration
	MaxOutputBytes int
	// Environment supplies selected command environment values
	//
	// A nil map inherits PATH only. An empty map supplies no PATH
	Environment map[string]string
}

// NewTools constructs git_status and git_diff for one Workspace
func NewTools(scoped *workspace.Workspace, options Options) ([]agent.Tool, error) {
	if scoped == nil {
		return nil, errors.New("Git Workspace is required")
	}
	timeout := options.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	maxOutputBytes := options.MaxOutputBytes
	if maxOutputBytes == 0 {
		maxOutputBytes = defaultMaxOutputBytes
	}
	if timeout <= 0 || maxOutputBytes <= 0 {
		return nil, errors.New("Git Tool limits must be positive")
	}
	environment, err := gitEnvironment(options.Environment)
	if err != nil {
		return nil, err
	}
	base := gitTool{workspace: scoped, timeout: timeout, maxOutputBytes: maxOutputBytes, environment: environment}
	return []agent.Tool{
		&statusTool{gitTool: base},
		&diffTool{gitTool: base},
	}, nil
}

type gitTool struct {
	workspace      *workspace.Workspace
	timeout        time.Duration
	maxOutputBytes int
	environment    []string
}

func (tool *gitTool) run(ctx context.Context, arguments ...string) (command.Result, error) {
	base := []string{
		"--no-optional-locks",
		"-c", "core.pager=cat",
		"-c", "color.ui=false",
		"-c", "core.fsmonitor=false",
	}
	return command.Run(ctx, command.Request{
		Executable:     "git",
		Arguments:      append(base, arguments...),
		Directory:      tool.workspace.Root(),
		Environment:    append([]string(nil), tool.environment...),
		Timeout:        tool.timeout,
		MaxOutputBytes: tool.maxOutputBytes,
	})
}

func (tool *gitTool) ensureRepositoryRoot(ctx context.Context) error {
	result, err := tool.run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if result.ExitCode != 0 || result.StdoutTruncated {
		return fmt.Errorf("resolve Git repository root: %s", strings.TrimSpace(result.Stderr))
	}
	repositoryRoot := strings.TrimSpace(result.Stdout)
	canonical, err := filepath.EvalSymlinks(repositoryRoot)
	if err != nil {
		return fmt.Errorf("resolve Git repository root: %w", err)
	}
	if filepath.Clean(canonical) != tool.workspace.Root() {
		return errors.New("workspace must equal the Git repository root")
	}
	return nil
}

type statusTool struct {
	gitTool
}

func (tool *statusTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:         StatusToolName,
		Description:  "Return structured branch, staged, unstaged, and untracked Git status for the workspace",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Capabilities: []string{string(capability.GitRead)},
	}
}

func (tool *statusTool) Execute(ctx context.Context, call agent.ToolCall) (agent.ToolResult, error) {
	var input struct{}
	if err := jsonstrict.Decode(call.Arguments, maximumArgumentBytes, &input); err != nil {
		return agent.ToolResult{}, fmt.Errorf("decode git_status arguments: %w", err)
	}
	release := tool.workspace.AcquireRead()
	defer release()
	if err := tool.ensureRepositoryRoot(ctx); err != nil {
		return agent.ToolResult{}, err
	}
	result, err := tool.run(ctx, "status", "--porcelain=v2", "-z", "--branch", "--untracked-files=all")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if result.ExitCode != 0 {
		return agent.ToolResult{}, fmt.Errorf("git status failed with exit code %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	if result.StdoutTruncated {
		return agent.ToolResult{}, errors.New("git status exceeds the output limit")
	}
	response, err := parseStatus(result.Stdout)
	if err != nil {
		return agent.ToolResult{}, err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("encode git_status result: %w", err)
	}
	return agent.ToolResult{Output: string(encoded)}, nil
}

type diffTool struct {
	gitTool
}

func (tool *diffTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:         DiffToolName,
		Description:  "Return a bounded Git patch for worktree, staged, or base-relative changes",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"scope":{"type":"string","enum":["worktree","staged","base"]},"base":{"type":"string"},"paths":{"type":"array","items":{"type":"string"}},"context_lines":{"type":"integer","minimum":0,"maximum":100}},"additionalProperties":false}`),
		Capabilities: []string{string(capability.GitRead)},
	}
}

func (tool *diffTool) Execute(ctx context.Context, call agent.ToolCall) (agent.ToolResult, error) {
	var input struct {
		Scope        string   `json:"scope,omitempty"`
		Base         string   `json:"base,omitempty"`
		Paths        []string `json:"paths,omitempty"`
		ContextLines *int     `json:"context_lines,omitempty"`
	}
	if err := jsonstrict.Decode(call.Arguments, maximumArgumentBytes, &input); err != nil {
		return agent.ToolResult{}, fmt.Errorf("decode git_diff arguments: %w", err)
	}
	if input.Scope == "" {
		input.Scope = "worktree"
	}
	if input.Scope != "worktree" && input.Scope != "staged" && input.Scope != "base" {
		return agent.ToolResult{}, fmt.Errorf("unsupported git_diff scope %q", input.Scope)
	}
	if input.Scope == "base" && input.Base == "" {
		return agent.ToolResult{}, errors.New("git_diff base is required for base scope")
	}
	if input.Scope != "base" && input.Base != "" {
		return agent.ToolResult{}, errors.New("git_diff base is only valid for base scope")
	}
	contextLines := 3
	if input.ContextLines != nil {
		contextLines = *input.ContextLines
	}
	if contextLines < 0 || contextLines > 100 {
		return agent.ToolResult{}, errors.New("git_diff context_lines must be between 0 and 100")
	}

	release := tool.workspace.AcquireRead()
	defer release()
	if err := tool.ensureRepositoryRoot(ctx); err != nil {
		return agent.ToolResult{}, err
	}
	paths, err := tool.resolvePaths(input.Paths)
	if err != nil {
		return agent.ToolResult{}, err
	}
	arguments := []string{
		"diff",
		"--no-ext-diff",
		"--no-textconv",
		"--no-color",
		"--src-prefix=a/",
		"--dst-prefix=b/",
		"--unified=" + strconv.Itoa(contextLines),
	}
	resolvedBase := ""
	switch input.Scope {
	case "staged":
		arguments = append(arguments, "--cached")
	case "base":
		resolvedBase, err = tool.resolveRevision(ctx, input.Base)
		if err != nil {
			return agent.ToolResult{}, err
		}
		arguments = append(arguments, resolvedBase)
	}
	arguments = append(arguments, "--")
	arguments = append(arguments, paths...)
	result, err := tool.run(ctx, arguments...)
	if err != nil {
		return agent.ToolResult{}, err
	}
	if result.ExitCode != 0 {
		return agent.ToolResult{}, fmt.Errorf("git diff failed with exit code %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	sum := sha256.Sum256([]byte(result.Stdout))
	response := diffResponse{
		Scope:     input.Scope,
		Base:      resolvedBase,
		Patch:     result.Stdout,
		Digest:    "sha256:" + hex.EncodeToString(sum[:]),
		Truncated: result.StdoutTruncated,
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("encode git_diff result: %w", err)
	}
	return agent.ToolResult{Output: string(encoded)}, nil
}

func (tool *diffTool) resolveRevision(ctx context.Context, revision string) (string, error) {
	if revision == "" || strings.HasPrefix(revision, "-") || strings.ContainsAny(revision, "\x00\r\n") {
		return "", errors.New("git_diff base is invalid")
	}
	result, err := tool.run(ctx, "rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
	if err != nil {
		return "", err
	}
	objectID := strings.TrimSpace(result.Stdout)
	if result.ExitCode != 0 || result.StdoutTruncated || !objectIDPattern.MatchString(objectID) {
		return "", fmt.Errorf("resolve git_diff base %q: %s", revision, strings.TrimSpace(result.Stderr))
	}
	return objectID, nil
}

func (tool *diffTool) resolvePaths(paths []string) ([]string, error) {
	result := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "." {
			if _, exists := seen[path]; !exists {
				seen[path] = struct{}{}
				result = append(result, path)
			}
			continue
		}
		resolved, _, err := tool.workspace.ResolveTarget(path)
		if err != nil {
			return nil, fmt.Errorf("resolve git_diff path %q: %w", path, err)
		}
		relative, err := tool.workspace.Relative(resolved)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[relative]; exists {
			continue
		}
		seen[relative] = struct{}{}
		result = append(result, relative)
	}
	return result, nil
}

type statusResponse struct {
	Branch  branchStatus  `json:"branch"`
	Entries []statusEntry `json:"entries"`
}

type branchStatus struct {
	Head     string `json:"head,omitempty"`
	OID      string `json:"oid,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Ahead    int    `json:"ahead,omitempty"`
	Behind   int    `json:"behind,omitempty"`
}

type statusEntry struct {
	Path           string `json:"path"`
	OriginalPath   string `json:"original_path,omitempty"`
	Kind           string `json:"kind"`
	IndexStatus    string `json:"index_status"`
	WorktreeStatus string `json:"worktree_status"`
}

type diffResponse struct {
	Scope     string `json:"scope"`
	Base      string `json:"base,omitempty"`
	Patch     string `json:"patch"`
	Digest    string `json:"digest"`
	Truncated bool   `json:"truncated"`
}

func parseStatus(output string) (statusResponse, error) {
	response := statusResponse{Entries: []statusEntry{}}
	records := strings.Split(output, "\x00")
	for index := 0; index < len(records); index++ {
		record := records[index]
		if record == "" {
			continue
		}
		if strings.HasPrefix(record, "# ") {
			parseBranchRecord(&response.Branch, strings.TrimPrefix(record, "# "))
			continue
		}
		switch record[0] {
		case '1':
			fields := strings.SplitN(record, " ", 9)
			if len(fields) != 9 || len(fields[1]) != 2 {
				return statusResponse{}, fmt.Errorf("parse ordinary Git status record %q", record)
			}
			response.Entries = append(response.Entries, statusEntry{
				Path: filepath.ToSlash(fields[8]), Kind: "ordinary",
				IndexStatus: fields[1][:1], WorktreeStatus: fields[1][1:],
			})
		case '2':
			fields := strings.SplitN(record, " ", 10)
			if len(fields) != 10 || len(fields[1]) != 2 || index+1 >= len(records) {
				return statusResponse{}, fmt.Errorf("parse renamed Git status record %q", record)
			}
			index++
			response.Entries = append(response.Entries, statusEntry{
				Path: filepath.ToSlash(fields[9]), OriginalPath: filepath.ToSlash(records[index]), Kind: "renamed",
				IndexStatus: fields[1][:1], WorktreeStatus: fields[1][1:],
			})
		case 'u':
			fields := strings.SplitN(record, " ", 11)
			if len(fields) != 11 || len(fields[1]) != 2 {
				return statusResponse{}, fmt.Errorf("parse unmerged Git status record %q", record)
			}
			response.Entries = append(response.Entries, statusEntry{
				Path: filepath.ToSlash(fields[10]), Kind: "unmerged",
				IndexStatus: fields[1][:1], WorktreeStatus: fields[1][1:],
			})
		case '?':
			response.Entries = append(response.Entries, statusEntry{Path: filepath.ToSlash(strings.TrimPrefix(record, "? ")), Kind: "untracked", IndexStatus: "?", WorktreeStatus: "?"})
		case '!':
			response.Entries = append(response.Entries, statusEntry{Path: filepath.ToSlash(strings.TrimPrefix(record, "! ")), Kind: "ignored", IndexStatus: "!", WorktreeStatus: "!"})
		default:
			return statusResponse{}, fmt.Errorf("unsupported Git status record %q", record)
		}
	}
	return response, nil
}

func parseBranchRecord(branch *branchStatus, record string) {
	name, value, found := strings.Cut(record, " ")
	if !found {
		return
	}
	switch name {
	case "branch.oid":
		branch.OID = value
	case "branch.head":
		branch.Head = value
	case "branch.upstream":
		branch.Upstream = value
	case "branch.ab":
		fields := strings.Fields(value)
		if len(fields) == 2 {
			branch.Ahead, _ = strconv.Atoi(strings.TrimPrefix(fields[0], "+"))
			branch.Behind, _ = strconv.Atoi(strings.TrimPrefix(fields[1], "-"))
		}
	}
}

func gitEnvironment(configured map[string]string) ([]string, error) {
	environment := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_OPTIONAL_LOCKS=0",
		"LC_ALL=C",
	}
	if configured == nil {
		if path, ok := os.LookupEnv("PATH"); ok {
			environment = append(environment, "PATH="+path)
		}
		return environment, nil
	}
	names := make([]string, 0, len(configured))
	for name, value := range configured {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("invalid Git environment entry %q", name)
		}
		if name == "GIT_CONFIG_NOSYSTEM" || name == "GIT_CONFIG_GLOBAL" ||
			name == "GIT_OPTIONAL_LOCKS" || name == "LC_ALL" {
			return nil, fmt.Errorf("Git environment entry %q is reserved", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		environment = append(environment, name+"="+configured[name])
	}
	return environment, nil
}
