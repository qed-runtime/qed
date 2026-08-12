// Package host starts and supervises process-isolated QED Extensions
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	extensionpkg "github.com/qed-runtime/qed/extension"
	"github.com/qed-runtime/qed/extension/protocol"
)

const (
	defaultStartupTimeout  = 10 * time.Second
	defaultShutdownTimeout = 5 * time.Second
	defaultStderrBytes     = 64 << 10
)

var (
	// ErrProcessExited indicates that an Extension child process terminated
	ErrProcessExited = errors.New("Extension process exited")
	// ErrHostClosed indicates that an Extension Host no longer accepts work
	ErrHostClosed = errors.New("Extension Host is closed")
)

// Command identifies one trusted Extension executable and its isolated environment
type Command struct {
	// Path is an absolute executable path
	Path string
	// Args are passed directly without shell evaluation
	Args []string
	// Directory is an optional absolute child working directory
	Directory string
	// Environment is the complete child environment and is not merged with os.Environ
	Environment map[string]string
}

// ProcessOptions configures one Extension process generation
type ProcessOptions struct {
	Command Command
	// ExpectedID must match the identity returned by Handshake
	ExpectedID string
	// ExpectedVersion optionally requires an exact Handshake implementation version
	ExpectedVersion string
	// ExpectedManifest optionally requires declared capabilities, Hooks, and
	// Commands to match the process Describe response. Tools remain dynamic
	ExpectedManifest *protocol.Manifest
	// Initialize contains host-selected resources sent after Handshake
	Initialize protocol.InitializeRequest
	// StartupTimeout bounds Start when ctx has no earlier deadline
	StartupTimeout time.Duration
	// ShutdownTimeout bounds graceful Drain and Shutdown before forced termination
	ShutdownTimeout time.Duration
	// MaxStderrBytes bounds retained process diagnostics
	MaxStderrBytes int
	// Verbose enables safe structured Host and Extension diagnostics
	Verbose bool
	// DebugWriter receives JSON diagnostics when Logger is nil and Verbose is true
	DebugWriter io.Writer
	// Logger receives safe structured diagnostics when Verbose is true
	Logger *slog.Logger
}

// Process is one initialized, protocol-validated Extension process
type Process struct {
	options  ProcessOptions
	command  *exec.Cmd
	client   *client
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   *boundedBuffer
	manifest protocol.Manifest
	logger   *slog.Logger

	waitMu   sync.Mutex
	waitErr  error
	waitDone chan struct{}

	closeOnce sync.Once
	closeErr  error
}

// Start launches, handshakes, initializes, and describes one Extension process
func Start(ctx context.Context, options ProcessOptions) (*Process, error) {
	if ctx == nil {
		return nil, errors.New("Extension Process context must not be nil")
	}
	configured, err := validateProcessOptions(options)
	if err != nil {
		return nil, err
	}
	environment, err := environmentList(configured.Command.Environment)
	if err != nil {
		return nil, err
	}

	command := exec.Command(configured.Command.Path, configured.Command.Args...)
	command.Dir = configured.Command.Directory
	command.Env = environment
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open Extension stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open Extension stdout: %w", err)
	}
	stderr := newBoundedBuffer(configured.MaxStderrBytes)
	command.Stderr = stderr
	if configured.Verbose && configured.Logger != nil {
		command.Stderr = io.MultiWriter(stderr, newSafeDebugForwarder(configured.Logger, configured.ExpectedID))
		configured.Logger.Debug("extension.process.starting",
			"component", "extension_host",
			"extension_id", configured.ExpectedID,
			"executable", filepath.Base(configured.Command.Path),
		)
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start Extension process: %w", err)
	}

	process := &Process{
		options:  configured,
		command:  command,
		stdin:    stdin,
		stdout:   stdout,
		stderr:   stderr,
		logger:   configured.Logger,
		waitDone: make(chan struct{}),
	}
	process.client = newClient(stdout, stdin)
	go process.wait()

	startupContext, cancel := withDefaultTimeout(ctx, configured.StartupTimeout)
	defer cancel()
	if err := process.startup(startupContext); err != nil {
		process.terminate()
		diagnostics, truncated := process.Stderr()
		if diagnostics != "" {
			return nil, fmt.Errorf("initialize Extension process: %w; stderr%s: %s", err, truncationLabel(truncated), diagnostics)
		}
		return nil, fmt.Errorf("initialize Extension process: %w", err)
	}
	process.debug("extension.process.ready",
		"extension_id", process.manifest.ID,
		"extension_version", process.manifest.Version,
		"tool_count", len(process.manifest.Tools),
		"hook_count", len(process.manifest.Hooks),
		"command_count", len(process.manifest.Commands),
	)
	return process, nil
}

func validateProcessOptions(options ProcessOptions) (ProcessOptions, error) {
	if options.Command.Path == "" || strings.IndexByte(options.Command.Path, 0) >= 0 {
		return ProcessOptions{}, errors.New("Extension executable path is required and must not contain NUL")
	}
	if !filepath.IsAbs(options.Command.Path) {
		return ProcessOptions{}, errors.New("Extension executable path must be absolute")
	}
	for index, argument := range options.Command.Args {
		if strings.IndexByte(argument, 0) >= 0 {
			return ProcessOptions{}, fmt.Errorf("Extension argument %d must not contain NUL", index)
		}
	}
	if options.Command.Directory != "" {
		if !filepath.IsAbs(options.Command.Directory) || strings.IndexByte(options.Command.Directory, 0) >= 0 {
			return ProcessOptions{}, errors.New("Extension working directory must be an absolute path without NUL")
		}
	}
	if strings.TrimSpace(options.ExpectedID) != options.ExpectedID || options.ExpectedID == "" {
		return ProcessOptions{}, errors.New("expected Extension ID is required and must not have surrounding whitespace")
	}
	if strings.TrimSpace(options.ExpectedVersion) != options.ExpectedVersion {
		return ProcessOptions{}, errors.New("expected Extension version must not have surrounding whitespace")
	}
	if options.ExpectedManifest != nil {
		expected := cloneManifest(*options.ExpectedManifest)
		if expected.ID != options.ExpectedID {
			return ProcessOptions{}, errors.New("expected manifest ID does not match expected Extension ID")
		}
		if options.ExpectedVersion != "" && expected.Version != options.ExpectedVersion {
			return ProcessOptions{}, errors.New("expected manifest version does not match expected Extension version")
		}
		if expected.ProtocolVersion != protocol.Version {
			return ProcessOptions{}, fmt.Errorf("expected manifest protocol version %d is unsupported", expected.ProtocolVersion)
		}
		if len(expected.Tools) != 0 {
			return ProcessOptions{}, errors.New("expected external manifest must not declare runtime Tools")
		}
		options.ExpectedManifest = &expected
	}
	if options.StartupTimeout == 0 {
		options.StartupTimeout = defaultStartupTimeout
	}
	if options.Initialize.Verbose {
		options.Verbose = true
	}
	options.Initialize.Verbose = options.Verbose
	if options.Verbose && options.Logger == nil && options.DebugWriter != nil {
		options.Logger = slog.New(slog.NewJSONHandler(options.DebugWriter, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	if !options.Verbose {
		options.Logger = nil
	}
	if options.ShutdownTimeout == 0 {
		options.ShutdownTimeout = defaultShutdownTimeout
	}
	if options.MaxStderrBytes == 0 {
		options.MaxStderrBytes = defaultStderrBytes
	}
	if options.StartupTimeout <= 0 || options.ShutdownTimeout <= 0 || options.MaxStderrBytes <= 0 {
		return ProcessOptions{}, errors.New("Extension process timeouts and stderr limit must be positive")
	}
	options.Command.Args = append([]string(nil), options.Command.Args...)
	options.Command.Environment = cloneEnvironment(options.Command.Environment)
	options.Initialize = cloneInitializeRequest(options.Initialize)
	return options, nil
}

func (process *Process) startup(ctx context.Context) error {
	var handshake protocol.HandshakeResponse
	if err := process.call(ctx, protocol.MethodHandshake, protocol.HandshakeRequest{
		ProtocolVersion: protocol.Version,
	}, &handshake); err != nil {
		return fmt.Errorf("Handshake: %w", err)
	}
	if handshake.ProtocolVersion != protocol.Version {
		return fmt.Errorf("Handshake protocol version %d is unsupported, want %d", handshake.ProtocolVersion, protocol.Version)
	}
	if handshake.ExtensionID != process.options.ExpectedID {
		return fmt.Errorf("Handshake Extension ID %q does not match expected %q", handshake.ExtensionID, process.options.ExpectedID)
	}
	if strings.TrimSpace(handshake.ExtensionVersion) != handshake.ExtensionVersion || handshake.ExtensionVersion == "" {
		return errors.New("Handshake Extension version is invalid")
	}
	if process.options.ExpectedVersion != "" && handshake.ExtensionVersion != process.options.ExpectedVersion {
		return fmt.Errorf("Handshake Extension version %q does not match expected %q", handshake.ExtensionVersion, process.options.ExpectedVersion)
	}
	if err := process.call(ctx, protocol.MethodInitialize, process.options.Initialize, &protocol.Empty{}); err != nil {
		return fmt.Errorf("Initialize: %w", err)
	}
	var described protocol.DescribeResponse
	if err := process.call(ctx, protocol.MethodDescribe, protocol.Empty{}, &described); err != nil {
		return fmt.Errorf("Describe: %w", err)
	}
	if err := validateManifest(described.Manifest, handshake); err != nil {
		return err
	}
	if process.options.ExpectedManifest != nil {
		if err := validateExpectedManifest(described.Manifest, *process.options.ExpectedManifest); err != nil {
			return err
		}
	}
	var health protocol.HealthCheckResponse
	if err := process.call(ctx, protocol.MethodHealthCheck, protocol.Empty{}, &health); err != nil {
		return fmt.Errorf("HealthCheck: %w", err)
	}
	if !health.Initialized || health.Draining || health.Status != "ready" {
		return fmt.Errorf("Extension is not ready after Initialize: %#v", health)
	}
	process.manifest = cloneManifest(described.Manifest)
	return nil
}

func validateManifest(manifest protocol.Manifest, handshake protocol.HandshakeResponse) error {
	if manifest.ID != handshake.ExtensionID || manifest.Version != handshake.ExtensionVersion {
		return errors.New("Describe manifest identity does not match Handshake")
	}
	if manifest.ProtocolVersion != protocol.Version {
		return fmt.Errorf("manifest protocol version %d is unsupported, want %d", manifest.ProtocolVersion, protocol.Version)
	}
	manifestCapabilities := make(map[string]struct{}, len(manifest.Capabilities))
	for _, value := range manifest.Capabilities {
		if err := capability.ValidateName(capability.Name(value)); err != nil {
			return fmt.Errorf("manifest: %w", err)
		}
		if _, duplicate := manifestCapabilities[value]; duplicate {
			return fmt.Errorf("manifest capability %q is registered more than once", value)
		}
		manifestCapabilities[value] = struct{}{}
	}
	toolNames := make(map[string]struct{}, len(manifest.Tools))
	for _, definition := range manifest.Tools {
		if strings.TrimSpace(definition.Name) != definition.Name || definition.Name == "" {
			return errors.New("manifest Tool name is required")
		}
		if _, duplicate := toolNames[definition.Name]; duplicate {
			return fmt.Errorf("manifest Tool %q is registered more than once", definition.Name)
		}
		toolNames[definition.Name] = struct{}{}
		if len(definition.InputSchema) > 0 && !json.Valid(definition.InputSchema) {
			return fmt.Errorf("manifest Tool %q has an invalid input schema", definition.Name)
		}
		seen := make(map[string]struct{}, len(definition.Capabilities))
		for _, value := range definition.Capabilities {
			if err := capability.ValidateName(capability.Name(value)); err != nil {
				return fmt.Errorf("manifest Tool %q: %w", definition.Name, err)
			}
			if _, duplicate := seen[value]; duplicate {
				return fmt.Errorf("manifest Tool %q capability %q is registered more than once", definition.Name, value)
			}
			seen[value] = struct{}{}
			if _, declared := manifestCapabilities[value]; !declared {
				return fmt.Errorf("manifest Tool %q capability %q is absent from Extension capabilities", definition.Name, value)
			}
		}
	}
	hooks := make(map[string]struct{}, len(manifest.Hooks))
	for _, eventType := range manifest.Hooks {
		if eventType == "" || strings.TrimSpace(eventType) != eventType {
			return errors.New("manifest Hook Event type is required and must not have surrounding whitespace")
		}
		if _, duplicate := hooks[eventType]; duplicate {
			return fmt.Errorf("manifest Hook %q is registered more than once", eventType)
		}
		hooks[eventType] = struct{}{}
	}
	commandNames := make(map[string]struct{}, len(manifest.Commands))
	for _, definition := range manifest.Commands {
		if definition.Name == "" || strings.TrimSpace(definition.Name) != definition.Name {
			return errors.New("manifest Command name is required and must not have surrounding whitespace")
		}
		if _, duplicate := commandNames[definition.Name]; duplicate {
			return fmt.Errorf("manifest Command %q is registered more than once", definition.Name)
		}
		commandNames[definition.Name] = struct{}{}
		if len(definition.InputSchema) > 0 && !json.Valid(definition.InputSchema) {
			return fmt.Errorf("manifest Command %q has an invalid input schema", definition.Name)
		}
		seen := make(map[string]struct{}, len(definition.Capabilities))
		for _, value := range definition.Capabilities {
			if err := capability.ValidateName(capability.Name(value)); err != nil {
				return fmt.Errorf("manifest Command %q: %w", definition.Name, err)
			}
			if _, duplicate := seen[value]; duplicate {
				return fmt.Errorf("manifest Command %q capability %q is registered more than once", definition.Name, value)
			}
			seen[value] = struct{}{}
			if _, declared := manifestCapabilities[value]; !declared {
				return fmt.Errorf("manifest Command %q capability %q is absent from Extension capabilities", definition.Name, value)
			}
		}
	}
	return nil
}

func validateExpectedManifest(actual, expected protocol.Manifest) error {
	if actual.ID != expected.ID || actual.Version != expected.Version || actual.ProtocolVersion != expected.ProtocolVersion {
		return errors.New("Extension Describe identity does not match its external manifest")
	}
	if !sameStrings(actual.Capabilities, expected.Capabilities) {
		return errors.New("Extension capabilities do not match its external manifest")
	}
	if !sameStrings(actual.Hooks, expected.Hooks) {
		return errors.New("Extension Hooks do not match its external manifest")
	}
	if len(actual.Commands) != len(expected.Commands) {
		return errors.New("Extension Commands do not match its external manifest")
	}
	actualCommands := make(map[string]protocol.CommandDefinition, len(actual.Commands))
	for _, command := range actual.Commands {
		actualCommands[command.Name] = command
	}
	for _, wanted := range expected.Commands {
		got, ok := actualCommands[wanted.Name]
		if !ok || got.Description != wanted.Description ||
			!sameJSON(got.InputSchema, wanted.InputSchema) ||
			!sameStrings(got.Capabilities, wanted.Capabilities) {
			return fmt.Errorf("Extension Command %q does not match its external manifest", wanted.Name)
		}
	}
	return nil
}

func sameStrings(first, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	left := append([]string(nil), first...)
	right := append([]string(nil), second...)
	sort.Strings(left)
	sort.Strings(right)
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameJSON(first, second json.RawMessage) bool {
	if len(first) == 0 || len(second) == 0 {
		return len(first) == len(second)
	}
	var left any
	var right any
	if json.Unmarshal(first, &left) != nil || json.Unmarshal(second, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

func (process *Process) call(ctx context.Context, method protocol.Method, params any, result any) error {
	startedAt := time.Now()
	process.debug("extension.rpc.started",
		"extension_id", process.options.ExpectedID,
		"method", method,
	)
	err := process.client.call(ctx, method, params, result)
	if err == nil {
		process.debug("extension.rpc.completed",
			"extension_id", process.options.ExpectedID,
			"method", method,
			"duration_ms", time.Since(startedAt).Milliseconds(),
		)
		return nil
	}
	process.debug("extension.rpc.failed",
		"extension_id", process.options.ExpectedID,
		"method", method,
		"duration_ms", time.Since(startedAt).Milliseconds(),
		"error_type", fmt.Sprintf("%T", err),
	)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-process.waitDone:
		case <-timer.C:
		case <-ctx.Done():
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	select {
	case <-process.waitDone:
		process.waitMu.Lock()
		waitErr := process.waitErr
		process.waitMu.Unlock()
		if waitErr != nil {
			return fmt.Errorf("%w: %v", ErrProcessExited, waitErr)
		}
		return ErrProcessExited
	default:
		return err
	}
}

func (process *Process) exited() bool {
	select {
	case <-process.waitDone:
		return true
	default:
		return false
	}
}

func (process *Process) done() <-chan struct{} {
	return process.waitDone
}

// Manifest returns an isolated copy of the validated Extension manifest
func (process *Process) Manifest() protocol.Manifest {
	return cloneManifest(process.manifest)
}

// Tools returns host-side RPC Tools tagged with one generation
func (process *Process) Tools(generation uint64) []agent.Tool {
	tools := make([]agent.Tool, len(process.manifest.Tools))
	for index := range process.manifest.Tools {
		definition := process.manifest.Tools[index]
		remote := &remoteTool{
			process: process,
			definition: agent.ToolDefinition{
				Name:                definition.Name,
				Description:         definition.Description,
				InputSchema:         append(json.RawMessage(nil), definition.InputSchema...),
				Capabilities:        append([]string(nil), definition.Capabilities...),
				ExtensionID:         process.manifest.ID,
				ExtensionGeneration: generation,
			},
		}
		if definition.DynamicCapabilities {
			tools[index] = &dynamicRemoteTool{remoteTool: remote}
		} else {
			tools[index] = remote
		}
	}
	return tools
}

// Hooks returns host-side RPC Hooks tagged with one generation
func (process *Process) Hooks(generation uint64) []agent.Hook {
	if len(process.manifest.Hooks) == 0 {
		return nil
	}
	eventTypes := make([]agent.EventType, len(process.manifest.Hooks))
	for index, eventType := range process.manifest.Hooks {
		eventTypes[index] = agent.EventType(eventType)
	}
	return []agent.Hook{&remoteHook{
		process: process,
		definition: agent.HookDefinition{
			EventTypes:          eventTypes,
			ExtensionID:         process.manifest.ID,
			ExtensionGeneration: generation,
		},
	}}
}

// Commands returns host-side RPC Commands tagged with one generation
func (process *Process) Commands(generation uint64) []extensionpkg.Command {
	commands := make([]extensionpkg.Command, len(process.manifest.Commands))
	for index, definition := range process.manifest.Commands {
		commands[index] = &remoteCommand{
			process: process,
			definition: extensionpkg.CommandDefinition{
				Name:                definition.Name,
				Description:         definition.Description,
				InputSchema:         append(json.RawMessage(nil), definition.InputSchema...),
				Capabilities:        append([]string(nil), definition.Capabilities...),
				ExtensionID:         process.manifest.ID,
				ExtensionGeneration: generation,
			},
		}
	}
	return commands
}

// Snapshot returns opaque Extension state for a compatible new generation
func (process *Process) Snapshot(ctx context.Context) (json.RawMessage, error) {
	var response protocol.SnapshotResponse
	if err := process.call(ctx, protocol.MethodSnapshot, protocol.Empty{}, &response); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), response.State...), nil
}

// Restore applies opaque state from an older compatible generation
func (process *Process) Restore(ctx context.Context, state json.RawMessage) error {
	return process.call(ctx, protocol.MethodRestore, protocol.RestoreRequest{
		State: append(json.RawMessage(nil), state...),
	}, &protocol.Empty{})
}

// HealthCheck verifies that the Extension remains initialized and ready
func (process *Process) HealthCheck(ctx context.Context) (protocol.HealthCheckResponse, error) {
	var response protocol.HealthCheckResponse
	if err := process.call(ctx, protocol.MethodHealthCheck, protocol.Empty{}, &response); err != nil {
		return protocol.HealthCheckResponse{}, err
	}
	if !response.Initialized || response.Draining || response.Status != "ready" {
		return response, fmt.Errorf("Extension is not ready: %#v", response)
	}
	return response, nil
}

// Drain rejects new Tool calls and waits for accepted calls to finish
func (process *Process) Drain(ctx context.Context) error {
	return process.call(ctx, protocol.MethodDrain, protocol.Empty{}, &protocol.Empty{})
}

// Stderr returns bounded child diagnostics and whether additional bytes were discarded
func (process *Process) Stderr() (string, bool) {
	return process.stderr.snapshot()
}

// Close gracefully shuts down the Extension and forcibly terminates it on timeout
func (process *Process) Close() error {
	process.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), process.options.ShutdownTimeout)
		defer cancel()
		select {
		case <-process.waitDone:
			process.closePipes()
			return
		default:
		}
		shutdownErr := process.call(ctx, protocol.MethodShutdown, protocol.Empty{}, &protocol.Empty{})
		_ = process.stdin.Close()
		select {
		case <-process.waitDone:
		case <-ctx.Done():
			killErr := process.command.Process.Kill()
			<-process.waitDone
			if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				process.closeErr = errors.Join(process.closeErr, fmt.Errorf("kill Extension process: %w", killErr))
			}
		}
		process.closePipes()
		if shutdownErr != nil && !errors.Is(shutdownErr, ErrProcessExited) && !errors.Is(shutdownErr, io.EOF) {
			process.closeErr = errors.Join(process.closeErr, fmt.Errorf("shutdown Extension process: %w", shutdownErr))
		}
	})
	process.debug("extension.process.closed",
		"extension_id", process.options.ExpectedID,
		"error_type", errorType(process.closeErr),
	)
	return process.closeErr
}

func (process *Process) debug(message string, arguments ...any) {
	if process.logger != nil {
		process.logger.Debug(message, append([]any{"component", "extension_host"}, arguments...)...)
	}
}

func errorType(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}

func (process *Process) terminate() {
	process.client.fail(ErrHostClosed)
	_ = process.stdin.Close()
	_ = process.command.Process.Kill()
	<-process.waitDone
	process.closePipes()
}

func (process *Process) wait() {
	err := process.command.Wait()
	process.waitMu.Lock()
	process.waitErr = err
	process.waitMu.Unlock()
	if err != nil {
		process.client.fail(fmt.Errorf("%w: %v", ErrProcessExited, err))
	} else {
		process.client.fail(ErrProcessExited)
	}
	close(process.waitDone)
}

func (process *Process) closePipes() {
	_ = process.stdin.Close()
	_ = process.stdout.Close()
}

type remoteTool struct {
	process    *Process
	definition agent.ToolDefinition
}

func (tool *remoteTool) Definition() agent.ToolDefinition {
	definition := tool.definition
	definition.InputSchema = append(json.RawMessage(nil), definition.InputSchema...)
	definition.Capabilities = append([]string(nil), definition.Capabilities...)
	return definition
}

func (tool *remoteTool) Execute(ctx context.Context, call agent.ToolCall) (agent.ToolResult, error) {
	run := protocol.RunInfo{}
	if info, ok := agent.RunInfoFromContext(ctx); ok {
		run = protocol.RunInfo{
			RunID:       info.RunID,
			ParentRunID: info.ParentRunID,
			AgentID:     info.AgentID,
			SessionID:   info.SessionID,
		}
	}
	var response protocol.InvokeToolResponse
	if err := tool.process.call(ctx, protocol.MethodInvokeTool, protocol.InvokeToolRequest{
		Run:  run,
		Call: toProtocolToolCall(call),
	}, &response); err != nil {
		return agent.ToolResult{}, err
	}
	result := agent.ToolResult{
		CallID:           call.ID,
		Name:             call.Name,
		Output:           response.Result.Output,
		IsError:          response.Result.IsError,
		ContextOperation: toAgentContextOperation(response.Result.ContextOperation),
	}
	if err := agent.ValidateContextOperation(result.ContextOperation); err != nil {
		return agent.ToolResult{}, fmt.Errorf("Tool %q context operation: %w", call.Name, err)
	}
	return result, nil
}

func toAgentContextOperation(operation *protocol.ContextOperation) *agent.ContextOperation {
	if operation == nil {
		return nil
	}
	return &agent.ContextOperation{Kind: agent.ContextOperationKind(operation.Kind)}
}

type dynamicRemoteTool struct {
	*remoteTool
}

type remoteHook struct {
	process    *Process
	definition agent.HookDefinition
}

func (hook *remoteHook) Definition() agent.HookDefinition {
	definition := hook.definition
	definition.EventTypes = append([]agent.EventType(nil), definition.EventTypes...)
	return definition
}

func (hook *remoteHook) Handle(ctx context.Context, event agent.Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode Run Event: %w", err)
	}
	run := protocol.RunInfo{
		RunID:       event.RunID,
		ParentRunID: event.ParentRunID,
		AgentID:     event.AgentID,
		SessionID:   event.SessionID,
	}
	return hook.process.call(ctx, protocol.MethodHandleEvent, protocol.HandleEventRequest{
		Run: run,
		Event: protocol.RunEvent{
			Type:    string(event.Type),
			Payload: payload,
		},
	}, &protocol.Empty{})
}

type remoteCommand struct {
	process    *Process
	definition extensionpkg.CommandDefinition
}

func (command *remoteCommand) Definition() extensionpkg.CommandDefinition {
	definition := command.definition
	definition.InputSchema = append(json.RawMessage(nil), definition.InputSchema...)
	definition.Capabilities = append([]string(nil), definition.Capabilities...)
	return definition
}

func (command *remoteCommand) Execute(ctx context.Context, call extensionpkg.CommandCall) (extensionpkg.CommandResult, error) {
	run := protocol.RunInfo{}
	if info, ok := agent.RunInfoFromContext(ctx); ok {
		run = protocol.RunInfo{
			RunID:       info.RunID,
			ParentRunID: info.ParentRunID,
			AgentID:     info.AgentID,
			SessionID:   info.SessionID,
		}
	}
	var response protocol.InvokeCommandResponse
	if err := command.process.call(ctx, protocol.MethodInvokeCommand, protocol.InvokeCommandRequest{
		Run: run,
		Call: protocol.CommandCall{
			Name:      call.Name,
			Arguments: append(json.RawMessage(nil), call.Arguments...),
		},
	}, &response); err != nil {
		return extensionpkg.CommandResult{}, err
	}
	return extensionpkg.CommandResult{Output: append(json.RawMessage(nil), response.Result.Output...)}, nil
}

func (tool *dynamicRemoteTool) RequiredCapabilities(ctx context.Context, call agent.ToolCall) ([]capability.Name, error) {
	var response protocol.RequiredCapabilitiesResponse
	if err := tool.process.call(ctx, protocol.MethodRequiredCapabilities, protocol.RequiredCapabilitiesRequest{
		Call: toProtocolToolCall(call),
	}, &response); err != nil {
		return nil, err
	}
	result := make([]capability.Name, len(response.Capabilities))
	for index, value := range response.Capabilities {
		result[index] = capability.Name(value)
		if err := capability.ValidateName(result[index]); err != nil {
			return nil, fmt.Errorf("Extension returned invalid dynamic capability: %w", err)
		}
	}
	return result, nil
}

func toProtocolToolCall(call agent.ToolCall) protocol.ToolCall {
	return protocol.ToolCall{
		ID:        call.ID,
		Name:      call.Name,
		Arguments: append(json.RawMessage(nil), call.Arguments...),
	}
}

func withDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= timeout {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func environmentList(environment map[string]string) ([]string, error) {
	names := make([]string, 0, len(environment))
	for name, value := range environment {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("invalid Extension process environment entry %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, len(names))
	for index, name := range names {
		result[index] = name + "=" + environment[name]
	}
	return result, nil
}

func cloneEnvironment(environment map[string]string) map[string]string {
	if environment == nil {
		return map[string]string{}
	}
	cloned := make(map[string]string, len(environment))
	for name, value := range environment {
		cloned[name] = value
	}
	return cloned
}

func cloneInitializeRequest(request protocol.InitializeRequest) protocol.InitializeRequest {
	request.Environment = cloneEnvironment(request.Environment)
	request.Configuration = append(json.RawMessage(nil), request.Configuration...)
	return request
}

func cloneManifest(manifest protocol.Manifest) protocol.Manifest {
	manifest.Capabilities = append([]string(nil), manifest.Capabilities...)
	manifest.Hooks = append([]string(nil), manifest.Hooks...)
	manifest.Tools = append([]protocol.ToolDefinition(nil), manifest.Tools...)
	for index := range manifest.Tools {
		manifest.Tools[index].InputSchema = append(json.RawMessage(nil), manifest.Tools[index].InputSchema...)
		manifest.Tools[index].Capabilities = append([]string(nil), manifest.Tools[index].Capabilities...)
	}
	manifest.Commands = append([]protocol.CommandDefinition(nil), manifest.Commands...)
	for index := range manifest.Commands {
		manifest.Commands[index].InputSchema = append(json.RawMessage(nil), manifest.Commands[index].InputSchema...)
		manifest.Commands[index].Capabilities = append([]string(nil), manifest.Commands[index].Capabilities...)
	}
	return manifest
}

type boundedBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	remaining := buffer.limit - len(buffer.data)
	if remaining > 0 {
		amount := len(data)
		if amount > remaining {
			amount = remaining
		}
		buffer.data = append(buffer.data, data[:amount]...)
	}
	if len(data) > remaining {
		buffer.truncated = true
	}
	return len(data), nil
}

func (buffer *boundedBuffer) snapshot() (string, bool) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return string(append([]byte(nil), buffer.data...)), buffer.truncated
}

func truncationLabel(truncated bool) string {
	if truncated {
		return " (truncated)"
	}
	return ""
}
