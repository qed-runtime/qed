package host_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	extensionpkg "github.com/qed-runtime/qed/extension"
	"github.com/qed-runtime/qed/extension/host"
	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/extension/server"
)

const testExtensionID = "test-extension"

func TestMain(testingMain *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == "__test_extension" {
		os.Exit(serveTestExtension(os.Args[2], os.Args[3]))
	}
	os.Exit(testingMain.Run())
}

func TestProcessLifecycle(t *testing.T) {
	t.Parallel()

	process := startProcess(t, "normal", "v1", nil)
	defer closeProcess(t, process)
	manifest := process.Manifest()
	if manifest.ID != testExtensionID || manifest.Version != "v1" || len(manifest.Tools) != 1 {
		t.Fatalf("Manifest() = %#v", manifest)
	}
	tools := process.Tools(7)
	definition := tools[0].Definition()
	if definition.ExtensionID != testExtensionID || definition.ExtensionGeneration != 7 {
		t.Fatalf("Tool origin = %q/%d", definition.ExtensionID, definition.ExtensionGeneration)
	}
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{
		RunID: "run-1", AgentID: "agent-1", SessionID: "session-1",
	})
	result, err := tools[0].Execute(ctx, agent.ToolCall{ID: "call-1", Name: "identity", Arguments: json.RawMessage(`{}`)})
	if err != nil || result.Output != "v1" {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	state, err := process.Snapshot(context.Background())
	if err != nil || string(state) != `{"value":"v1"}` {
		t.Fatalf("Snapshot() = %s, %v", state, err)
	}
	if err := process.Restore(context.Background(), json.RawMessage(`{"value":"restored"}`)); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	result, err = tools[0].Execute(ctx, agent.ToolCall{ID: "call-2", Name: "identity", Arguments: json.RawMessage(`{}`)})
	if err != nil || result.Output != "v1:restored" {
		t.Fatalf("Execute() after Restore = %#v, %v", result, err)
	}
	if err := process.Drain(context.Background()); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	_, err = tools[0].Execute(ctx, agent.ToolCall{ID: "call-3", Name: "identity", Arguments: json.RawMessage(`{}`)})
	var rpcError *protocol.RPCError
	if !errors.As(err, &rpcError) || rpcError.Code != protocol.ErrorCodeDraining {
		t.Fatalf("Execute() after Drain error = %v, want draining RPC error", err)
	}
}

func TestProcessHooksAndCommandsCrossRPCBoundary(t *testing.T) {
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "hook")
	process := startProcess(t, "components", "v1", map[string]string{"MARKER": marker})
	defer closeProcess(t, process)
	manifest := process.Manifest()
	if len(manifest.Hooks) != 1 || manifest.Hooks[0] != string(agent.EventRunStarted) {
		t.Fatalf("Hooks = %#v", manifest.Hooks)
	}
	if len(manifest.Commands) != 1 || manifest.Commands[0].Name != "inspect_state" {
		t.Fatalf("Commands = %#v", manifest.Commands)
	}
	hooks := process.Hooks(9)
	if len(hooks) != 1 || hooks[0].Definition().ExtensionGeneration != 9 {
		t.Fatalf("host Hooks = %#v", hooks)
	}
	event := agent.Event{Type: agent.EventRunStarted, RunID: "run-hook", AgentID: "agent-hook"}
	if err := hooks[0].Handle(context.Background(), event); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "run-hook" {
		t.Fatalf("Hook marker = %q, %v", data, err)
	}
	commands := process.Commands(9)
	if len(commands) != 1 || commands[0].Definition().ExtensionGeneration != 9 {
		t.Fatalf("host Commands = %#v", commands)
	}
	result, err := commands[0].Execute(context.Background(), extensionpkg.CommandCall{
		Name: "inspect_state", Arguments: json.RawMessage(`{"requested":true}`),
	})
	if err != nil || string(result.Output) != `{"state":"v1"}` {
		t.Fatalf("Command Execute() = %s, %v", result.Output, err)
	}
}

func TestVerboseDiagnosticsReachExtensionWithoutSensitivePayloads(t *testing.T) {
	t.Parallel()

	var diagnostics bytes.Buffer
	options := processOptions(t, "normal", "v1", map[string]string{"SECRET_NAME": "do-not-log-environment"})
	options.Verbose = true
	options.DebugWriter = &diagnostics
	options.Initialize.Configuration = json.RawMessage(`{"secret":"do-not-log-configuration"}`)
	process, err := host.Start(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	tool := process.Tools(1)[0]
	_, err = tool.Execute(context.Background(), agent.ToolCall{
		ID:        "verbose-call",
		Name:      "identity",
		Arguments: json.RawMessage(`{"secret":"do-not-log-arguments"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
	output := diagnostics.String()
	for _, expected := range []string{"extension.process.ready", "extension.initialized", "invoke_tool"} {
		if !strings.Contains(output, expected) {
			t.Errorf("verbose diagnostics do not contain %q: %s", expected, output)
		}
	}
	for _, sensitive := range []string{"do-not-log-environment", "do-not-log-configuration", "do-not-log-arguments"} {
		if strings.Contains(output, sensitive) {
			t.Errorf("verbose diagnostics contain sensitive value %q: %s", sensitive, output)
		}
	}
}

func TestProcessCancellationCrossesRPCBoundary(t *testing.T) {
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "tool-started")
	process := startProcess(t, "blocking", "v1", map[string]string{"MARKER": marker})
	defer closeProcess(t, process)
	tool := process.Tools(1)[0]
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := tool.Execute(ctx, agent.ToolCall{ID: "blocking-call", Name: "identity", Arguments: json.RawMessage(`{}`)})
		result <- err
	}()
	waitForFile(t, marker)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Execute() did not return after cancellation")
	}
	waitForFile(t, marker+".canceled")
}

func TestProcessCrashIsIsolatedFromHost(t *testing.T) {
	t.Parallel()

	process := startProcess(t, "crash", "v1", nil)
	tool := process.Tools(1)[0]
	_, err := tool.Execute(context.Background(), agent.ToolCall{
		ID: "crash-call", Name: "identity", Arguments: json.RawMessage(`{}`),
	})
	if !errors.Is(err, host.ErrProcessExited) {
		t.Fatalf("Execute() error = %v, want ErrProcessExited", err)
	}
	if err := process.Close(); err != nil {
		t.Fatalf("Close() after crash error = %v", err)
	}

	replacement := startProcess(t, "normal", "v2", nil)
	defer closeProcess(t, replacement)
	result, err := replacement.Tools(1)[0].Execute(context.Background(), agent.ToolCall{
		ID: "replacement-call", Name: "identity", Arguments: json.RawMessage(`{}`),
	})
	if err != nil || result.Output != "v2" {
		t.Fatalf("replacement Execute() = %#v, %v", result, err)
	}
}

func TestStartRejectsProtocolAndIdentityMismatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		mode string
		want string
	}{
		{mode: "wrong-protocol", want: "protocol version"},
		{mode: "wrong-identity", want: "does not match expected"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.mode, func(t *testing.T) {
			t.Parallel()
			_, err := host.Start(context.Background(), processOptions(t, test.mode, "v1", nil))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Start() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestStartRejectsExternalManifestMismatch(t *testing.T) {
	t.Parallel()

	options := processOptions(t, "normal", "v1", nil)
	options.ExpectedVersion = "v1"
	options.ExpectedManifest = &protocol.Manifest{
		ID:              testExtensionID,
		Version:         "v1",
		ProtocolVersion: protocol.Version,
		Capabilities:    []string{"different.capability"},
	}
	_, err := host.Start(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "capabilities do not match") {
		t.Fatalf("Start() error = %v", err)
	}
}

func TestManagerReloadPinsRunGenerationAndRollsBack(t *testing.T) {
	t.Parallel()

	policy, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{
		Allow: []capability.Name{"test.execute"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := host.NewManager(context.Background(), host.ManagerOptions{
		Process: processOptions(t, "normal", "v1", nil),
		Policy:  policy,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer func() {
		if err := manager.Close(); err != nil {
			t.Errorf("Manager.Close() error = %v", err)
		}
	}()

	provider := newGenerationProvider()
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider, ToolSource: manager})
	if err != nil {
		t.Fatal(err)
	}
	oldHandle, err := runtime.Run(context.Background(), agent.RunRequest{
		SessionID: "old", Input: []agent.Message{{Role: agent.RoleUser, Text: "identify"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-provider.oldStarted

	generation, err := manager.Reload(context.Background(), processOptions(t, "normal", "v2", nil))
	if err != nil || generation != 2 || manager.CurrentGeneration() != 2 {
		t.Fatalf("Reload() = %d, %v, current = %d", generation, err, manager.CurrentGeneration())
	}
	close(provider.allowOld)
	oldResult, err := waitRun(oldHandle)
	if err != nil || finalText(oldResult) != "v1" {
		t.Fatalf("old Run = %#v, %v", oldResult, err)
	}

	newResult, err := runtimeRun(runtime, "new")
	if err != nil || finalText(newResult) != "v2:v1" {
		t.Fatalf("new Run = %#v, %v", newResult, err)
	}
	if len(newResult.ToolResults) != 1 {
		t.Fatalf("new ToolResults = %#v", newResult.ToolResults)
	}

	_, err = manager.Reload(context.Background(), processOptions(t, "restore-fail", "v3", nil))
	if err == nil || !strings.Contains(err.Error(), "restore generation 3") {
		t.Fatalf("failed Reload() error = %v", err)
	}
	if manager.CurrentGeneration() != 2 {
		t.Fatalf("CurrentGeneration() = %d, want 2", manager.CurrentGeneration())
	}
	rollbackResult, err := runtimeRun(runtime, "rollback")
	if err != nil || finalText(rollbackResult) != "v2:v1" {
		t.Fatalf("Run after rollback = %#v, %v", rollbackResult, err)
	}
}

func TestGenerationSetAcquiresAndReleasesEveryExtension(t *testing.T) {
	t.Parallel()

	first := &fakeManagedExtension{id: "first", generation: 2, tools: []agent.Tool{fakeTool("first_tool")}}
	second := &fakeManagedExtension{id: "second", generation: 7, tools: []agent.Tool{fakeTool("second_tool")}}
	set, err := host.NewGenerationSet([]host.ManagedExtension{second, first})
	if err != nil {
		t.Fatal(err)
	}
	tools, release, err := set.AcquireTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Definition().Name != "first_tool" || tools[1].Definition().Name != "second_tool" {
		t.Fatalf("Tools = %#v", tools)
	}
	release()
	release()
	if first.releases.Load() != 1 || second.releases.Load() != 1 {
		t.Fatalf("release counts = %d/%d", first.releases.Load(), second.releases.Load())
	}
	if generations := set.Generations(); generations["first"] != 2 || generations["second"] != 7 {
		t.Fatalf("Generations = %#v", generations)
	}
	if _, err := set.Reload(context.Background(), "first", host.ProcessOptions{ExpectedID: "first"}); err != nil {
		t.Fatal(err)
	}
	if generations := set.Generations(); generations["first"] != 3 || generations["second"] != 7 {
		t.Fatalf("Generations after Reload = %#v", generations)
	}
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerEnforcesDynamicCapabilityBeforeRemoteExecution(t *testing.T) {
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "executed")
	policy, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{
		Allow: []capability.Name{"test.execute"},
		Deny:  []capability.Name{"test.dynamic"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := host.NewManager(context.Background(), host.ManagerOptions{
		Process: processOptions(t, "dynamic", "v1", map[string]string{"MARKER": marker}),
		Policy:  policy,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer func() {
		if err := manager.Close(); err != nil {
			t.Errorf("Manager.Close() error = %v", err)
		}
	}()
	tools, release, err := manager.AcquireTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, err = tools[0].Execute(context.Background(), agent.ToolCall{
		ID: "dynamic-call", Name: "identity", Arguments: json.RawMessage(`{}`),
	})
	if !errors.Is(err, capability.ErrDenied) {
		t.Fatalf("Execute() error = %v, want capability denial", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote Tool executed before host Policy, stat error = %v", err)
	}
}

func TestManagerRestoresHostOwnedStateAcrossProcessLifetimes(t *testing.T) {
	t.Parallel()

	policy, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{
		Allow: []capability.Name{"test.execute"},
	})
	if err != nil {
		t.Fatal(err)
	}
	stateStore := extensionpkg.NewMemoryStateStore()
	first, err := host.NewManager(context.Background(), host.ManagerOptions{
		Process:    processOptions(t, "normal", "v1", nil),
		Policy:     policy,
		StateStore: stateStore,
		StateScope: "workspace:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := host.NewManager(context.Background(), host.ManagerOptions{
		Process:    processOptions(t, "normal", "v2", nil),
		Policy:     policy,
		StateStore: stateStore,
		StateScope: "workspace:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeProcessManager(t, second)
	tools, release, err := second.AcquireTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	result, err := tools[0].Execute(context.Background(), agent.ToolCall{
		ID: "state-call", Name: "identity", Arguments: json.RawMessage(`{}`),
	})
	if err != nil || result.Output != "v2:v1" {
		t.Fatalf("restored Execute() = %#v, %v", result, err)
	}
}

func closeProcessManager(t *testing.T, manager *host.Manager) {
	t.Helper()
	if err := manager.Close(); err != nil {
		t.Errorf("Manager.Close() error = %v", err)
	}
}

func startProcess(t *testing.T, mode, version string, initializeEnvironment map[string]string) *host.Process {
	t.Helper()
	process, err := host.Start(context.Background(), processOptions(t, mode, version, initializeEnvironment))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return process
}

func processOptions(t *testing.T, mode, version string, initializeEnvironment map[string]string) host.ProcessOptions {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	return host.ProcessOptions{
		Command: host.Command{
			Path: executable,
			Args: []string{"__test_extension", mode, version},
		},
		ExpectedID: testExtensionID,
		Initialize: protocol.InitializeRequest{
			Environment: initializeEnvironment,
		},
	}
}

func closeProcess(t *testing.T, process *host.Process) {
	t.Helper()
	if err := process.Close(); err != nil {
		t.Errorf("Process.Close() error = %v", err)
	}
}

type fakeManagedExtension struct {
	id         string
	generation uint64
	tools      []agent.Tool
	releases   atomic.Int32
	closed     atomic.Bool
}

func (extension *fakeManagedExtension) ExtensionID() string { return extension.id }

func (extension *fakeManagedExtension) CurrentGeneration() uint64 { return extension.generation }

func (extension *fakeManagedExtension) AcquireTools(context.Context) ([]agent.Tool, func(), error) {
	if extension.closed.Load() {
		return nil, nil, host.ErrHostClosed
	}
	return append([]agent.Tool(nil), extension.tools...), func() { extension.releases.Add(1) }, nil
}

func (extension *fakeManagedExtension) AcquireComponents(ctx context.Context) (agent.RunComponents, func(), error) {
	tools, release, err := extension.AcquireTools(ctx)
	return agent.RunComponents{Tools: tools}, release, err
}

func (extension *fakeManagedExtension) AcquireCommands(context.Context) ([]extensionpkg.Command, func(), error) {
	if extension.closed.Load() {
		return nil, nil, host.ErrHostClosed
	}
	return nil, func() { extension.releases.Add(1) }, nil
}

func (extension *fakeManagedExtension) Reload(_ context.Context, options host.ProcessOptions) (uint64, error) {
	if options.ExpectedID != extension.id {
		return 0, errors.New("unexpected Extension ID")
	}
	extension.generation++
	return extension.generation, nil
}

func (extension *fakeManagedExtension) CloseContext(context.Context) error {
	extension.closed.Store(true)
	return nil
}

type fakeTool string

func (tool fakeTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: string(tool), InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (fakeTool) Execute(context.Context, agent.ToolCall) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

func serveTestExtension(mode, version string) int {
	if mode == "wrong-protocol" {
		return serveWrongProtocol()
	}
	identity := testExtensionID
	if mode == "wrong-identity" {
		identity = "wrong-extension"
	}
	service := &testService{mode: mode, version: version, state: version}
	options := server.Options{
		ID:          identity,
		Version:     version,
		DebugWriter: os.Stderr,
		Initialize: func(_ context.Context, request protocol.InitializeRequest) ([]agent.Tool, error) {
			service.marker = request.Environment["MARKER"]
			return []agent.Tool{service}, nil
		},
		Snapshot: func(context.Context) (json.RawMessage, error) {
			service.mu.Lock()
			defer service.mu.Unlock()
			return json.Marshal(map[string]string{"value": service.state})
		},
		Restore: func(_ context.Context, state json.RawMessage) error {
			if mode == "restore-fail" {
				return errors.New("restore rejected for test")
			}
			var restored struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(state, &restored); err != nil {
				return err
			}
			service.mu.Lock()
			service.state = restored.Value
			service.mu.Unlock()
			return nil
		},
	}
	if mode == "components" {
		options.Initialize = nil
		options.InitializeComponents = func(_ context.Context, request protocol.InitializeRequest) (server.Components, error) {
			service.marker = request.Environment["MARKER"]
			return server.Components{
				Tools: []agent.Tool{service},
				Hooks: []string{string(agent.EventRunStarted)},
				HandleEvent: func(_ context.Context, request protocol.HandleEventRequest) error {
					return os.WriteFile(service.marker, []byte(request.Run.RunID), 0o600)
				},
				Commands: []extensionpkg.Command{testCommand{service: service}},
			}, nil
		}
	}
	err := server.Serve(context.Background(), os.Stdin, os.Stdout, options)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

type testCommand struct {
	service *testService
}

func (testCommand) Definition() extensionpkg.CommandDefinition {
	return extensionpkg.CommandDefinition{
		Name:         "inspect_state",
		Description:  "Inspect test state",
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		Capabilities: []string{"test.execute"},
	}
}

func (command testCommand) Execute(ctx context.Context, _ extensionpkg.CommandCall) (extensionpkg.CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return extensionpkg.CommandResult{}, err
	}
	command.service.mu.Lock()
	state := command.service.state
	command.service.mu.Unlock()
	output, err := json.Marshal(map[string]string{"state": state})
	return extensionpkg.CommandResult{Output: output}, err
}

func serveWrongProtocol() int {
	reader := protocol.NewReader(os.Stdin)
	request, err := reader.Read()
	if err != nil {
		return 1
	}
	result, _ := protocol.Marshal(protocol.HandshakeResponse{
		ProtocolVersion:  protocol.Version + 1,
		ExtensionID:      testExtensionID,
		ExtensionVersion: "v1",
	})
	if err := protocol.NewWriter(os.Stdout).Write(protocol.Envelope{
		Version: protocol.Version,
		ID:      request.ID,
		Result:  result,
	}); err != nil {
		return 1
	}
	_, _ = reader.Read()
	return 0
}

type testService struct {
	mu      sync.Mutex
	mode    string
	version string
	state   string
	marker  string
}

func (service *testService) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:         "identity",
		Description:  "Return the test Extension generation",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Capabilities: []string{"test.execute"},
	}
}

func (service *testService) RequiredCapabilities(ctx context.Context, _ agent.ToolCall) ([]capability.Name, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if service.mode == "dynamic" {
		return []capability.Name{"test.dynamic"}, nil
	}
	return nil, nil
}

func (service *testService) Execute(ctx context.Context, _ agent.ToolCall) (agent.ToolResult, error) {
	switch service.mode {
	case "crash":
		os.Exit(23)
	case "blocking":
		if service.marker != "" {
			if err := os.WriteFile(service.marker, []byte("started"), 0o600); err != nil {
				return agent.ToolResult{}, err
			}
		}
		<-ctx.Done()
		if service.marker != "" {
			_ = os.WriteFile(service.marker+".canceled", []byte("canceled"), 0o600)
		}
		return agent.ToolResult{}, ctx.Err()
	case "dynamic":
		if service.marker != "" {
			if err := os.WriteFile(service.marker, []byte("executed"), 0o600); err != nil {
				return agent.ToolResult{}, err
			}
		}
	}
	service.mu.Lock()
	state := service.state
	service.mu.Unlock()
	output := service.version
	if state != service.version {
		output += ":" + state
	}
	return agent.ToolResult{Output: output}, nil
}

type generationProvider struct {
	mu         sync.Mutex
	calls      map[string]int
	oldStarted chan struct{}
	allowOld   chan struct{}
	oldOnce    sync.Once
}

func newGenerationProvider() *generationProvider {
	return &generationProvider{
		calls:      make(map[string]int),
		oldStarted: make(chan struct{}),
		allowOld:   make(chan struct{}),
	}
}

func (provider *generationProvider) Name() string { return "generation-test" }

func (provider *generationProvider) Complete(ctx context.Context, request agent.ModelRequest) (agent.Message, error) {
	provider.mu.Lock()
	call := provider.calls[request.SessionID]
	provider.calls[request.SessionID] = call + 1
	provider.mu.Unlock()
	if call == 0 {
		if request.SessionID == "old" {
			provider.oldOnce.Do(func() { close(provider.oldStarted) })
			select {
			case <-provider.allowOld:
			case <-ctx.Done():
				return agent.Message{}, ctx.Err()
			}
		}
		return agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{
			ID: "identity-call", Name: "identity", Arguments: json.RawMessage(`{}`),
		}}}, nil
	}
	if len(request.Messages) == 0 {
		return agent.Message{}, errors.New("Tool result is missing")
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role != agent.RoleTool || last.ToolIsError {
		return agent.Message{}, fmt.Errorf("unexpected Tool result: %#v", last)
	}
	return agent.Message{Role: agent.RoleAssistant, Text: last.Text}, nil
}

func (provider *generationProvider) Stream(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	message, err := provider.Complete(ctx, request)
	if err != nil {
		return nil, err
	}
	return agent.MessageStream(message), nil
}

func runtimeRun(runtime *agent.Runtime, sessionID string) (agent.RunResult, error) {
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		SessionID: sessionID,
		Input:     []agent.Message{{Role: agent.RoleUser, Text: "identify"}},
	})
	if err != nil {
		return agent.RunResult{}, err
	}
	return waitRun(handle)
}

func waitRun(handle *agent.RunHandle) (agent.RunResult, error) {
	for range handle.Events() {
	}
	return handle.Wait()
}

func finalText(result agent.RunResult) string {
	for index := len(result.Messages) - 1; index >= 0; index-- {
		if result.Messages[index].Role == agent.RoleAssistant && len(result.Messages[index].ToolCalls) == 0 {
			return result.Messages[index].Text
		}
	}
	return ""
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %q was not created", path)
}
