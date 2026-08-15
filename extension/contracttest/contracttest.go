// Package contracttest provides a reusable Extension Protocol conformance suite
// and its process-isolated reference fixture
package contracttest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	extensionpkg "github.com/qed-runtime/qed/extension"
	"github.com/qed-runtime/qed/extension/host"
	"github.com/qed-runtime/qed/extension/manifest"
	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/extension/server"
)

const (
	fixtureID      = "qed.contracttest"
	fixtureVersion = "1.0.0"

	defaultTimeout = 5 * time.Second
	fixtureNonce   = "qed-contract-nonce"
	environmentKey = "QED_CONTRACT_NONCE"

	executeCapability = "contract.execute"
	inspectCapability = "contract.inspect"
	dynamicCapability = "contract.dynamic"

	echoToolName     = "contract_echo"
	blockingToolName = "contract_block"
	crashToolName    = "contract_crash"
	commandName      = "contract_inspect"

	initializedMarker = "initialized.json"
	hookMarker        = "hook.json"
	blockingMarker    = "blocking"
	canceledMarker    = "canceled"
	shutdownMarker    = "shutdown"
)

const (
	// ExternalChildArgument is the conventional private argument used by tests
	// to dispatch the reference fixture as an external executable
	ExternalChildArgument = "__qed_extension_contract_test"
)

// SuiteOptions configures one run of the Extension Protocol contract suite
type SuiteOptions struct {
	// Command starts a child mode that serves ServerOptions over stdin and stdout
	Command host.Command
	// Timeout bounds startup and each lifecycle assertion
	//
	// A zero value uses five seconds
	Timeout time.Duration
}

// LifecycleOptions configures lifecycle conformance checks for an actual
// Extension executable
type LifecycleOptions struct {
	// Command starts a fresh Extension process on every invocation
	Command host.Command
	// Declaration is the transport-independent manifest the process must match
	Declaration manifest.Declaration
	// Initialize contains the host resources required by the Extension
	Initialize protocol.InitializeRequest
	// Timeout bounds startup and each lifecycle assertion
	//
	// A zero value uses five seconds
	Timeout time.Duration
}

// Run executes the same Extension Protocol contract suite against Command
//
// The command must serve a fresh ServerOptions fixture on every invocation
// because the suite verifies independent lifecycle, cancellation, and crash
// process generations
func Run(t *testing.T, options SuiteOptions) {
	t.Helper()
	timeout := configuredTimeout(t, options.Timeout)

	t.Run("lifecycle_and_components", func(t *testing.T) {
		runLifecycleAndComponents(t, options.Command, timeout)
	})
	t.Run("cancellation", func(t *testing.T) {
		runCancellation(t, options.Command, timeout)
	})
	t.Run("process_crash", func(t *testing.T) {
		runProcessCrash(t, options.Command, timeout)
	})
}

// RunLifecycle verifies startup, declaration, state, drain, and shutdown
// behavior for an actual Extension process
//
// Tool, Hook, Command, cancellation, and crash semantics are specific to an
// Extension. Use Run with the reference fixture to verify those common RPC
// paths, then add behavior tests for the actual Extension components
func RunLifecycle(t *testing.T, options LifecycleOptions) {
	t.Helper()
	timeout := configuredTimeout(t, options.Timeout)
	if err := manifest.ValidateDeclaration(options.Declaration); err != nil {
		t.Fatalf("validate Extension declaration: %v", err)
	}
	declaration := options.Declaration
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	process, err := host.Start(ctx, host.ProcessOptions{
		Command:          options.Command,
		ExpectedID:       declaration.ID,
		ExpectedVersion:  declaration.Version,
		ExpectedManifest: pointerTo(declaration.ProtocolManifest()),
		Initialize:       options.Initialize,
		StartupTimeout:   timeout,
		ShutdownTimeout:  timeout,
	})
	cancel()
	if err != nil {
		t.Fatalf("start Extension lifecycle fixture: %v", err)
	}
	t.Cleanup(func() {
		if process != nil {
			_ = process.Close()
		}
	})

	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	health, err := process.HealthCheck(ctx)
	cancel()
	if err != nil || health.Status != "ready" || !health.Initialized || health.Draining {
		t.Fatalf("HealthCheck() = %#v, %v", health, err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	state, err := process.Snapshot(ctx)
	cancel()
	if err != nil || len(state) == 0 || !json.Valid(state) {
		t.Fatalf("Snapshot() = %s, %v", state, err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	err = process.Restore(ctx, state)
	cancel()
	if err != nil {
		t.Fatalf("Restore(Snapshot()) error = %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	err = process.Drain(ctx)
	cancel()
	if err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	health, err = process.HealthCheck(ctx)
	cancel()
	if err == nil || health.Status != "draining" || !health.Initialized || !health.Draining {
		t.Fatalf("HealthCheck() after Drain = %#v, %v", health, err)
	}
	if err := process.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	process = nil
}

// Declaration returns the transport-independent declaration of the reference
// fixture used by Run
func Declaration() manifest.Declaration {
	command := commandDefinition()
	return manifest.Declaration{
		ID:              fixtureID,
		Version:         fixtureVersion,
		ProtocolVersion: protocol.Version,
		Capabilities:    []string{executeCapability, inspectCapability},
		Hooks:           []string{string(agent.EventRunStarted)},
		Commands: []protocol.CommandDefinition{{
			Name:         command.Name,
			Description:  command.Description,
			InputSchema:  append(json.RawMessage(nil), command.InputSchema...),
			Capabilities: append([]string(nil), command.Capabilities...),
		}},
	}
}

// ServerOptions constructs a fresh reference fixture for an external process
// or self-exec catalog child
//
// The fixture exposes a crash probe that calls os.Exit. It must only be served
// from a dedicated test child process
func ServerOptions() server.Options {
	service := &fixtureService{state: "initial"}
	return server.Options{
		ID:                   fixtureID,
		Version:              fixtureVersion,
		InitializeComponents: service.initialize,
		Snapshot:             service.snapshot,
		Restore:              service.restore,
		Shutdown:             service.shutdown,
	}
}

type startedFixture struct {
	process   *host.Process
	workspace string
}

func startFixture(t *testing.T, command host.Command, timeout time.Duration) startedFixture {
	t.Helper()
	workspace := t.TempDir()
	configuration, err := json.Marshal(initializeConfiguration{Nonce: fixtureNonce})
	if err != nil {
		t.Fatal(err)
	}
	declaration := Declaration()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	process, err := host.Start(ctx, host.ProcessOptions{
		Command:          command,
		ExpectedID:       declaration.ID,
		ExpectedVersion:  declaration.Version,
		ExpectedManifest: pointerTo(declaration.ProtocolManifest()),
		Initialize: protocol.InitializeRequest{
			WorkspaceRoot: workspace,
			Environment:   map[string]string{environmentKey: fixtureNonce},
			Configuration: configuration,
			Verbose:       true,
		},
		StartupTimeout:  timeout,
		ShutdownTimeout: timeout,
	})
	if err != nil {
		t.Fatalf("start contract fixture: %v", err)
	}
	return startedFixture{process: process, workspace: workspace}
}

func runLifecycleAndComponents(t *testing.T, command host.Command, timeout time.Duration) {
	t.Helper()
	fixture := startFixture(t, command, timeout)
	process := fixture.process
	t.Cleanup(func() {
		if process != nil {
			_ = process.Close()
		}
	})

	var initialized initializeRecord
	readJSONMarker(t, filepath.Join(fixture.workspace, initializedMarker), timeout, &initialized)
	if initialized.WorkspaceRoot != fixture.workspace || initialized.EnvironmentNonce != fixtureNonce ||
		initialized.ConfigurationNonce != fixtureNonce || !initialized.Verbose {
		t.Fatalf("Initialize record = %#v", initialized)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	health, err := process.HealthCheck(ctx)
	cancel()
	if err != nil || health.Status != "ready" || !health.Initialized || health.Draining {
		t.Fatalf("HealthCheck() = %#v, %v", health, err)
	}

	if actual, expected := process.Manifest(), expectedManifest(); !reflect.DeepEqual(actual, expected) {
		t.Fatalf("Manifest() = %#v, want %#v", actual, expected)
	}

	const generation = uint64(41)
	tools := toolsByName(t, process.Tools(generation))
	echo := tools[echoToolName]
	definition := echo.Definition()
	if definition.ExtensionID != fixtureID || definition.ExtensionGeneration != generation {
		t.Fatalf("echo Tool origin = %q/%d", definition.ExtensionID, definition.ExtensionGeneration)
	}
	resolver, ok := echo.(extensionpkg.DynamicCapabilities)
	if !ok {
		t.Fatal("echo Tool does not expose DynamicCapabilities")
	}
	invalidCall := agent.ToolCall{
		ID:        "contract-echo-invalid",
		Name:      echoToolName,
		Arguments: json.RawMessage(`{"value":1}`),
	}
	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	_, err = resolver.RequiredCapabilities(ctx, invalidCall)
	cancel()
	var invalidParams *protocol.RPCError
	if !errors.As(err, &invalidParams) || invalidParams.Code != protocol.ErrorCodeInvalidParams {
		t.Fatalf("RequiredCapabilities(invalid) error = %v, want invalid params RPC error", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	_, err = echo.Execute(ctx, invalidCall)
	cancel()
	invalidParams = nil
	if !errors.As(err, &invalidParams) || invalidParams.Code != protocol.ErrorCodeInvalidParams {
		t.Fatalf("Execute(invalid) error = %v, want invalid params RPC error", err)
	}
	call := agent.ToolCall{
		ID:        "contract-echo-1",
		Name:      echoToolName,
		Arguments: json.RawMessage(`{"value":"hello"}`),
	}
	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	capabilities, err := resolver.RequiredCapabilities(ctx, call)
	cancel()
	if err != nil || !reflect.DeepEqual(capabilities, []capability.Name{dynamicCapability}) {
		t.Fatalf("RequiredCapabilities() = %v, %v", capabilities, err)
	}
	previewer, ok := echo.(extensionpkg.ApprovalPreviewer)
	if !ok {
		t.Fatal("echo Tool does not expose ApprovalPreviewer")
	}
	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	preview, err := previewer.ApprovalPreview(ctx, call)
	cancel()
	if err != nil || preview == nil || preview.Summary != "Echo one value" || len(preview.Details) != 1 ||
		preview.Details[0].Label != "value" || preview.Details[0].Value != "hello" {
		t.Fatalf("ApprovalPreview() = %#v, %v", preview, err)
	}

	runInfo := agent.RunInfo{
		RunID:       "contract-run",
		ParentRunID: "contract-parent",
		AgentID:     "contract-agent",
		SessionID:   "contract-session",
	}
	output := executeEcho(t, echo, call, runInfo, timeout)
	if output.Value != "hello" || output.State != "initial" || !reflect.DeepEqual(output.RunInfo, runInfo) {
		t.Fatalf("echo output = %#v", output)
	}

	hooks := process.Hooks(generation)
	if len(hooks) != 1 {
		t.Fatalf("Hooks() count = %d, want 1", len(hooks))
	}
	hookDefinition := hooks[0].Definition()
	if hookDefinition.ExtensionID != fixtureID || hookDefinition.ExtensionGeneration != generation ||
		!reflect.DeepEqual(hookDefinition.EventTypes, []agent.EventType{agent.EventRunStarted}) {
		t.Fatalf("Hook definition = %#v", hookDefinition)
	}
	event := agent.Event{
		Sequence:    1,
		Type:        agent.EventRunStarted,
		RunID:       runInfo.RunID,
		ParentRunID: runInfo.ParentRunID,
		AgentID:     runInfo.AgentID,
		SessionID:   runInfo.SessionID,
	}
	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	err = hooks[0].Handle(ctx, event)
	cancel()
	if err != nil {
		t.Fatalf("Hook Handle() error = %v", err)
	}
	var hook hookRecord
	readJSONMarker(t, filepath.Join(fixture.workspace, hookMarker), timeout, &hook)
	if !reflect.DeepEqual(hook.RunInfo, runInfo) || hook.EventType != string(event.Type) || hook.PayloadRunID != event.RunID {
		t.Fatalf("Hook record = %#v", hook)
	}

	commands := process.Commands(generation)
	if len(commands) != 1 {
		t.Fatalf("Commands() count = %d, want 1", len(commands))
	}
	commandDefinition := commands[0].Definition()
	if commandDefinition.ExtensionID != fixtureID || commandDefinition.ExtensionGeneration != generation ||
		commandDefinition.Name != commandName {
		t.Fatalf("Command definition = %#v", commandDefinition)
	}
	ctx, cancel = context.WithTimeout(agent.WithRunInfo(context.Background(), runInfo), timeout)
	commandResult, err := commands[0].Execute(ctx, extensionpkg.CommandCall{
		Name:      commandName,
		Arguments: json.RawMessage(`{}`),
	})
	cancel()
	if err != nil {
		t.Fatalf("Command Execute() error = %v", err)
	}
	var commandOutput inspectOutput
	if err := json.Unmarshal(commandResult.Output, &commandOutput); err != nil {
		t.Fatalf("decode Command output: %v", err)
	}
	if commandOutput.State != "initial" || !reflect.DeepEqual(commandOutput.RunInfo, runInfo) {
		t.Fatalf("Command output = %#v", commandOutput)
	}

	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	snapshot, err := process.Snapshot(ctx)
	cancel()
	if err != nil || !sameJSON(snapshot, json.RawMessage(`{"value":"initial"}`)) {
		t.Fatalf("Snapshot() = %s, %v", snapshot, err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	err = process.Restore(ctx, json.RawMessage(`{"value":"restored"}`))
	cancel()
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	call.ID = "contract-echo-2"
	output = executeEcho(t, echo, call, runInfo, timeout)
	if output.State != "restored" {
		t.Fatalf("echo state after Restore = %q, want restored", output.State)
	}

	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	err = process.Drain(ctx)
	cancel()
	if err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	health, err = process.HealthCheck(ctx)
	cancel()
	if err == nil || health.Status != "draining" || !health.Initialized || !health.Draining {
		t.Fatalf("HealthCheck() after Drain = %#v, %v", health, err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	_, err = echo.Execute(ctx, call)
	cancel()
	var rpcError *protocol.RPCError
	if !errors.As(err, &rpcError) || rpcError.Code != protocol.ErrorCodeDraining {
		t.Fatalf("Tool Execute() after Drain error = %v, want draining RPC error", err)
	}

	if err := process.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	process = nil
	waitForFile(t, filepath.Join(fixture.workspace, shutdownMarker), timeout)
}

func runCancellation(t *testing.T, command host.Command, timeout time.Duration) {
	t.Helper()
	fixture := startFixture(t, command, timeout)
	process := fixture.process
	t.Cleanup(func() {
		if process != nil {
			_ = process.Close()
		}
	})
	blocking := toolsByName(t, process.Tools(1))[blockingToolName]
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := blocking.Execute(ctx, agent.ToolCall{
			ID:        "contract-blocking",
			Name:      blockingToolName,
			Arguments: json.RawMessage(`{}`),
		})
		result <- err
	}()
	waitForFile(t, filepath.Join(fixture.workspace, blockingMarker), timeout)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocking Tool error = %v, want context.Canceled", err)
		}
	case <-time.After(timeout):
		t.Fatal("blocking Tool did not return after cancellation")
	}
	waitForFile(t, filepath.Join(fixture.workspace, canceledMarker), timeout)
	if err := process.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	process = nil
}

func runProcessCrash(t *testing.T, command host.Command, timeout time.Duration) {
	t.Helper()
	fixture := startFixture(t, command, timeout)
	process := fixture.process
	t.Cleanup(func() {
		if process != nil {
			_ = process.Close()
		}
	})
	crash := toolsByName(t, process.Tools(1))[crashToolName]
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	_, err := crash.Execute(ctx, agent.ToolCall{
		ID:        "contract-crash",
		Name:      crashToolName,
		Arguments: json.RawMessage(`{}`),
	})
	cancel()
	if !errors.Is(err, host.ErrProcessExited) {
		t.Fatalf("crash Tool error = %v, want host.ErrProcessExited", err)
	}
	if err := process.Close(); err != nil {
		t.Fatalf("Close() after process crash error = %v", err)
	}
	process = nil
}

func toolsByName(t *testing.T, tools []agent.Tool) map[string]agent.Tool {
	t.Helper()
	if len(tools) != 3 {
		t.Fatalf("Tools() count = %d, want 3", len(tools))
	}
	result := make(map[string]agent.Tool, len(tools))
	for _, tool := range tools {
		definition := tool.Definition()
		result[definition.Name] = tool
	}
	for _, name := range []string{echoToolName, blockingToolName, crashToolName} {
		if result[name] == nil {
			t.Fatalf("Tools() does not contain %q", name)
		}
	}
	return result
}

func executeEcho(
	t *testing.T,
	tool agent.Tool,
	call agent.ToolCall,
	runInfo agent.RunInfo,
	timeout time.Duration,
) echoOutput {
	t.Helper()
	ctx, cancel := context.WithTimeout(agent.WithRunInfo(context.Background(), runInfo), timeout)
	defer cancel()
	result, err := tool.Execute(ctx, call)
	if err != nil {
		t.Fatalf("echo Tool Execute() error = %v", err)
	}
	if result.CallID != call.ID || result.Name != call.Name || result.IsError {
		t.Fatalf("echo Tool result identity = %#v", result)
	}
	if result.ContextOperation == nil || result.ContextOperation.Kind != agent.ContextOperationVerification {
		t.Fatalf("echo Tool Context operation = %#v", result.ContextOperation)
	}
	var output echoOutput
	if err := json.Unmarshal([]byte(result.Output), &output); err != nil {
		t.Fatalf("decode echo Tool output: %v", err)
	}
	return output
}

func expectedManifest() protocol.Manifest {
	declaration := Declaration()
	tools := []agent.Tool{
		&echoTool{},
		&blockingTool{},
		&crashTool{},
	}
	result := declaration.ProtocolManifest()
	result.Tools = make([]protocol.ToolDefinition, len(tools))
	for index, tool := range tools {
		definition := tool.Definition()
		_, dynamic := tool.(extensionpkg.DynamicCapabilities)
		result.Tools[index] = protocol.ToolDefinition{
			Name:                definition.Name,
			Description:         definition.Description,
			InputSchema:         append(json.RawMessage(nil), definition.InputSchema...),
			Capabilities:        append([]string(nil), definition.Capabilities...),
			DynamicCapabilities: dynamic,
		}
	}
	return result
}

func readJSONMarker(t *testing.T, path string, timeout time.Duration, target any) {
	t.Helper()
	waitForFile(t, path, timeout)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read contract marker %q: %v", filepath.Base(path), err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode contract marker %q: %v", filepath.Base(path), err)
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("contract marker %q was not created", filepath.Base(path))
}

func pointerTo[T any](value T) *T {
	return &value
}

func configuredTimeout(t *testing.T, timeout time.Duration) time.Duration {
	t.Helper()
	if timeout == 0 {
		return defaultTimeout
	}
	if timeout < 0 {
		t.Fatal("contract test timeout must not be negative")
	}
	return timeout
}

func sameJSON(first, second json.RawMessage) bool {
	var left any
	var right any
	if json.Unmarshal(first, &left) != nil || json.Unmarshal(second, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

type initializeConfiguration struct {
	Nonce string `json:"nonce"`
}

type initializeRecord struct {
	WorkspaceRoot      string `json:"workspace_root"`
	EnvironmentNonce   string `json:"environment_nonce"`
	ConfigurationNonce string `json:"configuration_nonce"`
	Verbose            bool   `json:"verbose"`
}

type echoArguments struct {
	Value string `json:"value"`
}

type echoOutput struct {
	Value   string        `json:"value"`
	State   string        `json:"state"`
	RunInfo agent.RunInfo `json:"run_info"`
}

type inspectOutput struct {
	State   string        `json:"state"`
	RunInfo agent.RunInfo `json:"run_info"`
}

type hookRecord struct {
	RunInfo      agent.RunInfo `json:"run_info"`
	EventType    string        `json:"event_type"`
	PayloadRunID string        `json:"payload_run_id"`
}

type snapshotState struct {
	Value string `json:"value"`
}

type fixtureService struct {
	mu        sync.RWMutex
	workspace string
	state     string
}

func (service *fixtureService) initialize(
	ctx context.Context,
	request protocol.InitializeRequest,
) (server.Components, error) {
	if err := ctx.Err(); err != nil {
		return server.Components{}, err
	}
	if !filepath.IsAbs(request.WorkspaceRoot) {
		return server.Components{}, errors.New("contract fixture workspace root must be absolute")
	}
	info, err := os.Stat(request.WorkspaceRoot)
	if err != nil || !info.IsDir() {
		return server.Components{}, errors.New("contract fixture workspace root must be an existing directory")
	}
	var configuration initializeConfiguration
	if err := protocol.Unmarshal(request.Configuration, &configuration); err != nil {
		return server.Components{}, fmt.Errorf("decode contract fixture configuration: %w", err)
	}
	if request.Environment[environmentKey] != fixtureNonce || configuration.Nonce != fixtureNonce || !request.Verbose {
		return server.Components{}, errors.New("contract fixture Initialize values do not match the suite")
	}
	service.mu.Lock()
	service.workspace = request.WorkspaceRoot
	service.state = "initial"
	service.mu.Unlock()
	if err := service.writeJSON(initializedMarker, initializeRecord{
		WorkspaceRoot:      request.WorkspaceRoot,
		EnvironmentNonce:   request.Environment[environmentKey],
		ConfigurationNonce: configuration.Nonce,
		Verbose:            request.Verbose,
	}); err != nil {
		return server.Components{}, err
	}
	return server.Components{
		Tools: []agent.Tool{
			&echoTool{service: service},
			&blockingTool{service: service},
			&crashTool{},
		},
		Hooks:       []string{string(agent.EventRunStarted)},
		HandleEvent: service.handleEvent,
		Commands:    []extensionpkg.Command{&inspectCommand{service: service}},
	}, nil
}

func (service *fixtureService) snapshot(ctx context.Context) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(snapshotState{Value: service.currentState()})
}

func (service *fixtureService) restore(ctx context.Context, state json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var restored snapshotState
	if err := protocol.Unmarshal(state, &restored); err != nil {
		return err
	}
	if restored.Value == "" {
		return errors.New("contract fixture state value is required")
	}
	service.mu.Lock()
	service.state = restored.Value
	service.mu.Unlock()
	return nil
}

func (service *fixtureService) shutdown(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(service.workspacePath(), shutdownMarker), []byte("shutdown"), 0o600)
}

func (service *fixtureService) handleEvent(
	ctx context.Context,
	request protocol.HandleEventRequest,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var event agent.Event
	if err := json.Unmarshal(request.Event.Payload, &event); err != nil {
		return err
	}
	return service.writeJSON(hookMarker, hookRecord{
		RunInfo: agent.RunInfo{
			RunID:       request.Run.RunID,
			ParentRunID: request.Run.ParentRunID,
			AgentID:     request.Run.AgentID,
			SessionID:   request.Run.SessionID,
		},
		EventType:    request.Event.Type,
		PayloadRunID: event.RunID,
	})
}

func (service *fixtureService) writeJSON(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(service.workspacePath(), name), data, 0o600)
}

func (service *fixtureService) workspacePath() string {
	service.mu.RLock()
	defer service.mu.RUnlock()
	return service.workspace
}

func (service *fixtureService) currentState() string {
	service.mu.RLock()
	defer service.mu.RUnlock()
	return service.state
}

type echoTool struct {
	service *fixtureService
}

func (*echoTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:         echoToolName,
		Description:  "Echo one value and observable fixture state",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`),
		Capabilities: []string{executeCapability},
	}
}

func (*echoTool) RequiredCapabilities(ctx context.Context, call agent.ToolCall) ([]capability.Name, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var arguments echoArguments
	if err := protocol.Unmarshal(call.Arguments, &arguments); err != nil {
		return nil, err
	}
	return []capability.Name{dynamicCapability}, nil
}

func (*echoTool) ApprovalPreview(ctx context.Context, call agent.ToolCall) (*capability.ApprovalPreview, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var arguments echoArguments
	if err := protocol.Unmarshal(call.Arguments, &arguments); err != nil {
		return nil, err
	}
	return &capability.ApprovalPreview{
		Summary: "Echo one value",
		Details: []capability.ApprovalPreviewDetail{{Label: "value", Value: arguments.Value}},
	}, nil
}

func (tool *echoTool) Execute(ctx context.Context, call agent.ToolCall) (agent.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return agent.ToolResult{}, err
	}
	var arguments echoArguments
	if err := protocol.Unmarshal(call.Arguments, &arguments); err != nil {
		return agent.ToolResult{}, err
	}
	runInfo, _ := agent.RunInfoFromContext(ctx)
	output, err := json.Marshal(echoOutput{Value: arguments.Value, State: tool.service.currentState(), RunInfo: runInfo})
	if err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{
		CallID: call.ID, Name: call.Name, Output: string(output),
		ContextOperation: &agent.ContextOperation{Kind: agent.ContextOperationVerification},
	}, nil
}

type blockingTool struct {
	service *fixtureService
}

func (*blockingTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:         blockingToolName,
		Description:  "Block until the request context is canceled",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Capabilities: []string{executeCapability},
	}
}

func (tool *blockingTool) Execute(ctx context.Context, _ agent.ToolCall) (agent.ToolResult, error) {
	workspace := tool.service.workspacePath()
	if err := os.WriteFile(filepath.Join(workspace, blockingMarker), []byte("started"), 0o600); err != nil {
		return agent.ToolResult{}, err
	}
	<-ctx.Done()
	if err := os.WriteFile(filepath.Join(workspace, canceledMarker), []byte("canceled"), 0o600); err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{}, ctx.Err()
}

type crashTool struct{}

func (*crashTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:         crashToolName,
		Description:  "Terminate the reference fixture process",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Capabilities: []string{executeCapability},
	}
}

func (*crashTool) Execute(context.Context, agent.ToolCall) (agent.ToolResult, error) {
	os.Exit(23)
	return agent.ToolResult{}, errors.New("contract fixture process did not exit")
}

type inspectCommand struct {
	service *fixtureService
}

func commandDefinition() extensionpkg.CommandDefinition {
	return extensionpkg.CommandDefinition{
		Name:         commandName,
		Description:  "Return observable fixture state",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Capabilities: []string{inspectCapability},
	}
}

func (*inspectCommand) Definition() extensionpkg.CommandDefinition {
	return commandDefinition()
}

func (command *inspectCommand) Execute(
	ctx context.Context,
	_ extensionpkg.CommandCall,
) (extensionpkg.CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return extensionpkg.CommandResult{}, err
	}
	runInfo, _ := agent.RunInfoFromContext(ctx)
	output, err := json.Marshal(inspectOutput{State: command.service.currentState(), RunInfo: runInfo})
	return extensionpkg.CommandResult{Output: output}, err
}
