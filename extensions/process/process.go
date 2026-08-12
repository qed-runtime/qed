// Package process provides a bounded direct-command Tool scoped to one Workspace
package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/extensions/internal/command"
	"github.com/qed-runtime/qed/internal/jsonstrict"
	"github.com/qed-runtime/qed/workspace"
)

const (
	// RunCommandToolName is the model-facing name of the direct process Tool
	RunCommandToolName = "run_command"

	defaultTimeout        = 2 * time.Minute
	defaultMaximumTimeout = 10 * time.Minute
	defaultMaxOutputBytes = 1 << 20
	maximumArgumentBytes  = 64 << 10
)

// Options configures run_command resource limits and its host-provided environment
type Options struct {
	DefaultTimeout time.Duration
	MaximumTimeout time.Duration
	MaxOutputBytes int
	Environment    map[string]string
}

// NewTool constructs run_command for one Workspace
func NewTool(scoped *workspace.Workspace, options Options) (agent.Tool, error) {
	if scoped == nil {
		return nil, errors.New("process Workspace is required")
	}
	defaultCommandTimeout := options.DefaultTimeout
	if defaultCommandTimeout == 0 {
		defaultCommandTimeout = defaultTimeout
	}
	maximumTimeout := options.MaximumTimeout
	if maximumTimeout == 0 {
		maximumTimeout = defaultMaximumTimeout
	}
	maxOutputBytes := options.MaxOutputBytes
	if maxOutputBytes == 0 {
		maxOutputBytes = defaultMaxOutputBytes
	}
	if defaultCommandTimeout <= 0 || maximumTimeout <= 0 || defaultCommandTimeout > maximumTimeout {
		return nil, errors.New("process Tool timeouts must be positive and ordered")
	}
	if maxOutputBytes <= 0 {
		return nil, errors.New("process Tool output limit must be positive")
	}
	environment, err := environmentList(options.Environment)
	if err != nil {
		return nil, err
	}
	return &runCommandTool{
		workspace:      scoped,
		defaultTimeout: defaultCommandTimeout,
		maximumTimeout: maximumTimeout,
		maxOutputBytes: maxOutputBytes,
		environment:    environment,
	}, nil
}

type runCommandTool struct {
	workspace      *workspace.Workspace
	defaultTimeout time.Duration
	maximumTimeout time.Duration
	maxOutputBytes int
	environment    []string
}

func (tool *runCommandTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:         RunCommandToolName,
		Description:  "Run one executable directly within the workspace without shell expansion",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"argv":{"type":"array","items":{"type":"string"},"minItems":1},"cwd":{"type":"string"},"timeout_ms":{"type":"integer","minimum":1}},"required":["argv"],"additionalProperties":false}`),
		Capabilities: []string{string(capability.ProcessExecute)},
	}
}

func (tool *runCommandTool) Execute(ctx context.Context, call agent.ToolCall) (agent.ToolResult, error) {
	var input struct {
		Argv      []string `json:"argv"`
		CWD       string   `json:"cwd,omitempty"`
		TimeoutMS int64    `json:"timeout_ms,omitempty"`
	}
	if err := jsonstrict.Decode(call.Arguments, maximumArgumentBytes, &input); err != nil {
		return agent.ToolResult{}, fmt.Errorf("decode run_command arguments: %w", err)
	}
	if len(input.Argv) == 0 {
		return agent.ToolResult{}, errors.New("run_command argv must not be empty")
	}
	for index, argument := range input.Argv {
		if argument == "" || !utf8.ValidString(argument) || strings.IndexByte(argument, 0) >= 0 {
			return agent.ToolResult{}, fmt.Errorf("run_command argv[%d] must be non-empty valid UTF-8 without NUL", index)
		}
	}
	if input.CWD == "" {
		input.CWD = "."
	}
	timeout := tool.defaultTimeout
	if input.TimeoutMS != 0 {
		if input.TimeoutMS < 0 || input.TimeoutMS > int64(tool.maximumTimeout/time.Millisecond) {
			return agent.ToolResult{}, fmt.Errorf("run_command timeout_ms must be between 1 and %d", tool.maximumTimeout/time.Millisecond)
		}
		timeout = time.Duration(input.TimeoutMS) * time.Millisecond
	}

	release := tool.workspace.AcquireWrite()
	defer release()
	directory, err := tool.workspace.ResolveDirectory(input.CWD)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("resolve run_command cwd: %w", err)
	}
	result, err := command.Run(ctx, command.Request{
		Executable:     input.Argv[0],
		Arguments:      append([]string(nil), input.Argv[1:]...),
		Directory:      directory,
		Environment:    append([]string(nil), tool.environment...),
		Timeout:        timeout,
		MaxOutputBytes: tool.maxOutputBytes,
	})
	if err != nil {
		return agent.ToolResult{}, err
	}
	response := commandResponse{
		Argv:            append([]string(nil), input.Argv...),
		CWD:             filepathSlash(input.CWD),
		ExitCode:        result.ExitCode,
		Success:         result.ExitCode == 0 && !result.TimedOut,
		Stdout:          result.Stdout,
		Stderr:          result.Stderr,
		StdoutTruncated: result.StdoutTruncated,
		StderrTruncated: result.StderrTruncated,
		TimedOut:        result.TimedOut,
		DurationMS:      result.Duration.Milliseconds(),
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("encode run_command result: %w", err)
	}
	return agent.ToolResult{
		Output:           string(encoded),
		IsError:          !response.Success,
		ContextOperation: classifyContextOperation(input.Argv),
	}, nil
}

func classifyContextOperation(argv []string) *agent.ContextOperation {
	if len(argv) == 0 {
		return nil
	}
	executable := strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe")
	operation := func(kind agent.ContextOperationKind) *agent.ContextOperation {
		return &agent.ContextOperation{Kind: kind}
	}
	rawExecutable := strings.ToLower(argv[0])
	if rawExecutable != executable && rawExecutable != executable+".exe" {
		return operation(agent.ContextOperationMutation)
	}
	if executable == "git" && len(argv) > 1 && argv[1] == "commit" {
		return operation(agent.ContextOperationCommit)
	}
	if commandMatches(argv, executable, "go", "test", "vet") ||
		commandMatches(argv, executable, "cargo", "test", "check", "clippy") ||
		commandMatches(argv, executable, "dotnet", "test") ||
		commandMatches(argv, executable, "swift", "test") ||
		commandMatches(argv, executable, "mvn", "test", "verify") ||
		commandMatches(argv, executable, "gradle", "test", "check") ||
		commandMatches(argv, executable, "gradlew", "test", "check") ||
		executable == "pytest" || executable == "phpunit" || executable == "rspec" ||
		executable == "golangci-lint" && len(argv) > 1 && argv[1] == "run" ||
		isPythonPytest(argv, executable) || isPackageVerification(argv, executable) ||
		isTaskVerification(argv, executable) {
		return operation(agent.ContextOperationVerification)
	}
	return operation(agent.ContextOperationMutation)
}

func commandMatches(argv []string, executable string, wantExecutable string, subcommands ...string) bool {
	if executable != wantExecutable || len(argv) < 2 {
		return false
	}
	for _, subcommand := range subcommands {
		if argv[1] == subcommand {
			return true
		}
	}
	return false
}

func isPythonPytest(argv []string, executable string) bool {
	return (executable == "python" || executable == "python3") && len(argv) > 2 &&
		argv[1] == "-m" && argv[2] == "pytest"
}

func isPackageVerification(argv []string, executable string) bool {
	switch executable {
	case "npm", "pnpm", "yarn", "bun":
	default:
		return false
	}
	if len(argv) < 2 {
		return false
	}
	if argv[1] == "test" {
		return true
	}
	return len(argv) > 2 && argv[1] == "run" && verificationTarget(argv[2])
}

func isTaskVerification(argv []string, executable string) bool {
	switch executable {
	case "make", "gmake", "just", "task":
		return len(argv) > 1 && verificationTarget(argv[1])
	default:
		return false
	}
}

func verificationTarget(target string) bool {
	switch target {
	case "test", "check", "lint", "vet", "verify", "typecheck":
		return true
	default:
		return false
	}
}

type commandResponse struct {
	Argv            []string `json:"argv"`
	CWD             string   `json:"cwd"`
	ExitCode        int      `json:"exit_code"`
	Success         bool     `json:"success"`
	Stdout          string   `json:"stdout"`
	Stderr          string   `json:"stderr"`
	StdoutTruncated bool     `json:"stdout_truncated"`
	StderrTruncated bool     `json:"stderr_truncated"`
	TimedOut        bool     `json:"timed_out"`
	DurationMS      int64    `json:"duration_ms"`
}

func environmentList(configured map[string]string) ([]string, error) {
	if configured == nil {
		configured = make(map[string]string)
		if path, ok := os.LookupEnv("PATH"); ok {
			configured["PATH"] = path
		}
	}
	names := make([]string, 0, len(configured))
	for name, value := range configured {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("invalid process environment entry %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, len(names))
	for index, name := range names {
		result[index] = name + "=" + configured[name]
	}
	return result, nil
}

func filepathSlash(path string) string {
	return strings.ReplaceAll(path, "\\", "/")
}
