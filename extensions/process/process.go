// Package process provides a bounded direct-command Tool scoped to one Workspace
package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	return agent.ToolResult{Output: string(encoded), IsError: !response.Success}, nil
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
