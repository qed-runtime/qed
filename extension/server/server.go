// Package server adapts Go Extension Tools to the QED Extension Protocol
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/extension"
	"github.com/qed-runtime/qed/extension/protocol"
)

// Initializer constructs one process-local Tool registry from host resources
type Initializer func(ctx context.Context, request protocol.InitializeRequest) ([]agent.Tool, error)

// HookHandler handles one Run Event registered by an Extension
type HookHandler func(ctx context.Context, request protocol.HandleEventRequest) error

// Components contains all components registered during Initialize
type Components struct {
	Tools       []agent.Tool
	Hooks       []string
	HandleEvent HookHandler
	Commands    []extension.Command
}

// ComponentInitializer constructs all process-local Extension components
type ComponentInitializer func(ctx context.Context, request protocol.InitializeRequest) (Components, error)

// Options configures one Extension protocol server
type Options struct {
	// ID is the stable Extension identifier expected by the Host
	ID string
	// Version identifies the Extension implementation for diagnostics and reloads
	Version string
	// Initialize constructs Tools exactly once after a successful Handshake
	Initialize Initializer
	// InitializeComponents constructs Tools, Hooks, and Commands exactly once
	//
	// Initialize and InitializeComponents are mutually exclusive
	InitializeComponents ComponentInitializer
	// ToolInputValidator compiles schemas enforced at the Extension RPC boundary
	//
	// A nil Validator uses agent.JSONSchemaSubsetValidator
	ToolInputValidator agent.ToolInputValidator
	// Snapshot returns opaque process-local state, or nil for an empty state object
	Snapshot func(ctx context.Context) (json.RawMessage, error)
	// Restore accepts state produced by a compatible older generation
	Restore func(ctx context.Context, state json.RawMessage) error
	// Shutdown releases process-local resources after all Tool calls drain
	Shutdown func(ctx context.Context) error
	// DebugWriter receives safe JSON diagnostics after a verbose Initialize
	// request. Tool arguments, output, environment values, configuration, and
	// Extension state are never logged by the protocol server
	DebugWriter io.Writer
}

// Serve runs one concurrent Extension Protocol server until Shutdown or EOF
func Serve(ctx context.Context, input io.Reader, output io.Writer, options Options) error {
	if ctx == nil {
		return errors.New("Extension Server context must not be nil")
	}
	if input == nil || output == nil {
		return errors.New("Extension Server input and output are required")
	}
	if err := validateOptions(options); err != nil {
		return err
	}

	serverContext, cancelServer := context.WithCancel(ctx)
	defer cancelServer()
	state := newState(serverContext, options, protocol.NewWriter(output))
	reader := protocol.NewReader(input)
	for {
		envelope, err := reader.Read()
		if err != nil {
			state.cancelRequests()
			state.workers.Wait()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if envelope.Method == protocol.MethodShutdown {
			if err := state.serveShutdown(envelope); err != nil {
				return err
			}
			state.workers.Wait()
			return nil
		}
		if envelope.Method == protocol.MethodCancel {
			if err := state.serveCancel(envelope); err != nil {
				return err
			}
			continue
		}
		if err := state.dispatch(envelope); err != nil {
			return err
		}
	}
}

type state struct {
	ctx     context.Context
	options Options
	writer  *protocol.Writer

	mu             sync.Mutex
	handshaken     bool
	initializing   bool
	initialized    bool
	draining       bool
	tools          map[string]agent.Tool
	toolValidators map[string]agent.CompiledToolInputValidator
	hooks          map[string]struct{}
	hookHandler    HookHandler
	commands       map[string]extension.Command
	manifest       protocol.Manifest
	active         int
	idle           chan struct{}

	requestsMu sync.Mutex
	requests   map[string]context.CancelFunc
	workers    sync.WaitGroup

	loggerMu sync.RWMutex
	logger   *slog.Logger
}

func newState(ctx context.Context, options Options, writer *protocol.Writer) *state {
	idle := make(chan struct{})
	close(idle)
	return &state{
		ctx:            ctx,
		options:        options,
		writer:         writer,
		tools:          make(map[string]agent.Tool),
		toolValidators: make(map[string]agent.CompiledToolInputValidator),
		hooks:          make(map[string]struct{}),
		commands:       make(map[string]extension.Command),
		idle:           idle,
		requests:       make(map[string]context.CancelFunc),
	}
}

func validateOptions(options Options) error {
	if strings.TrimSpace(options.ID) != options.ID || options.ID == "" {
		return errors.New("Extension Server ID is required and must not have surrounding whitespace")
	}
	if strings.TrimSpace(options.Version) != options.Version || options.Version == "" {
		return errors.New("Extension Server version is required and must not have surrounding whitespace")
	}
	if options.Initialize == nil && options.InitializeComponents == nil {
		return errors.New("Extension Server initializer is required")
	}
	if options.Initialize != nil && options.InitializeComponents != nil {
		return errors.New("Extension Server Initialize and InitializeComponents are mutually exclusive")
	}
	return nil
}

func (state *state) dispatch(envelope protocol.Envelope) error {
	requestContext, finish, rpcError := state.beginRequest(envelope)
	if rpcError != nil {
		return state.writeError(envelope.ID, rpcError)
	}
	state.workers.Add(1)
	go func() {
		defer state.workers.Done()
		defer finish()
		startedAt := time.Now()
		state.debug("extension.rpc.started", "method", envelope.Method)

		result, callError := state.handle(requestContext, envelope)
		if callError != nil {
			state.debug("extension.rpc.failed",
				"method", envelope.Method,
				"duration_ms", time.Since(startedAt).Milliseconds(),
				"error_code", callError.Code,
			)
			_ = state.writeError(envelope.ID, callError)
			return
		}
		state.debug("extension.rpc.completed",
			"method", envelope.Method,
			"duration_ms", time.Since(startedAt).Milliseconds(),
		)
		_ = state.writeResult(envelope.ID, result)
	}()
	return nil
}

func (state *state) beginRequest(envelope protocol.Envelope) (context.Context, func(), *protocol.RPCError) {
	if envelope.ID == "" || envelope.Method == "" || envelope.Error != nil || len(envelope.Result) > 0 {
		return nil, nil, rpcError(protocol.ErrorCodeInvalidRequest, "request envelope is invalid")
	}
	if envelope.Version != protocol.Version {
		return nil, nil, rpcError(
			protocol.ErrorCodeProtocolMismatch,
			fmt.Sprintf("protocol version %d is unsupported, want %d", envelope.Version, protocol.Version),
		)
	}
	requestContext, cancel := context.WithCancel(state.ctx)
	state.requestsMu.Lock()
	if _, duplicate := state.requests[envelope.ID]; duplicate {
		state.requestsMu.Unlock()
		cancel()
		return nil, nil, rpcError(protocol.ErrorCodeInvalidRequest, "request ID is already in flight")
	}
	state.requests[envelope.ID] = cancel
	state.requestsMu.Unlock()
	finish := func() {
		state.requestsMu.Lock()
		delete(state.requests, envelope.ID)
		state.requestsMu.Unlock()
		cancel()
	}
	return requestContext, finish, nil
}

func (state *state) serveCancel(envelope protocol.Envelope) error {
	if envelope.Version != protocol.Version || envelope.ID == "" || envelope.Error != nil || len(envelope.Result) > 0 {
		return state.writeError(envelope.ID, rpcError(protocol.ErrorCodeInvalidRequest, "cancel request is invalid"))
	}
	var request protocol.CancelRequest
	if err := protocol.Unmarshal(envelope.Params, &request); err != nil || request.RequestID == "" {
		return state.writeError(envelope.ID, rpcError(protocol.ErrorCodeInvalidParams, "cancel request_id is required"))
	}
	state.requestsMu.Lock()
	cancel := state.requests[request.RequestID]
	state.requestsMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return state.writeResult(envelope.ID, protocol.Empty{})
}

func (state *state) serveShutdown(envelope protocol.Envelope) error {
	requestContext, finish, rpcError := state.beginRequest(envelope)
	if rpcError != nil {
		return state.writeError(envelope.ID, rpcError)
	}
	defer finish()
	if err := decodeEmpty(envelope.Params); err != nil {
		return state.writeError(envelope.ID, rpcErrorFrom(protocol.ErrorCodeInvalidParams, err))
	}
	if err := state.drain(requestContext); err != nil {
		return state.writeError(envelope.ID, mapCallError(err))
	}
	if state.options.Shutdown != nil {
		if err := state.options.Shutdown(requestContext); err != nil {
			return state.writeError(envelope.ID, rpcErrorFrom(protocol.ErrorCodeExtensionRejected, err))
		}
	}
	return state.writeResult(envelope.ID, protocol.Empty{})
}

func (state *state) handle(ctx context.Context, envelope protocol.Envelope) (any, *protocol.RPCError) {
	switch envelope.Method {
	case protocol.MethodHandshake:
		return state.handshake(envelope.Params)
	case protocol.MethodInitialize:
		return state.initialize(ctx, envelope.Params)
	case protocol.MethodDescribe:
		return state.describe(envelope.Params)
	case protocol.MethodRequiredCapabilities:
		return state.requiredCapabilities(ctx, envelope.Params)
	case protocol.MethodApprovalPreview:
		return state.approvalPreview(ctx, envelope.Params)
	case protocol.MethodInvokeTool:
		return state.invokeTool(ctx, envelope.Params)
	case protocol.MethodHandleEvent:
		return state.handleEvent(ctx, envelope.Params)
	case protocol.MethodInvokeCommand:
		return state.invokeCommand(ctx, envelope.Params)
	case protocol.MethodHealthCheck:
		return state.healthCheck(envelope.Params)
	case protocol.MethodSnapshot:
		return state.snapshot(ctx, envelope.Params)
	case protocol.MethodRestore:
		return state.restore(ctx, envelope.Params)
	case protocol.MethodDrain:
		if err := decodeEmpty(envelope.Params); err != nil {
			return nil, rpcErrorFrom(protocol.ErrorCodeInvalidParams, err)
		}
		if err := state.drain(ctx); err != nil {
			return nil, mapCallError(err)
		}
		return protocol.Empty{}, nil
	default:
		return nil, rpcError(protocol.ErrorCodeMethodNotFound, fmt.Sprintf("method %q is not supported", envelope.Method))
	}
}

func (state *state) handshake(params json.RawMessage) (any, *protocol.RPCError) {
	var request protocol.HandshakeRequest
	if err := protocol.Unmarshal(params, &request); err != nil {
		return nil, rpcErrorFrom(protocol.ErrorCodeInvalidParams, err)
	}
	if request.ProtocolVersion != protocol.Version {
		return nil, rpcError(
			protocol.ErrorCodeProtocolMismatch,
			fmt.Sprintf("protocol version %d is unsupported, want %d", request.ProtocolVersion, protocol.Version),
		)
	}
	state.mu.Lock()
	state.handshaken = true
	state.mu.Unlock()
	return protocol.HandshakeResponse{
		ProtocolVersion:  protocol.Version,
		ExtensionID:      state.options.ID,
		ExtensionVersion: state.options.Version,
	}, nil
}

func (state *state) initialize(ctx context.Context, params json.RawMessage) (any, *protocol.RPCError) {
	var request protocol.InitializeRequest
	if err := protocol.Unmarshal(params, &request); err != nil {
		return nil, rpcErrorFrom(protocol.ErrorCodeInvalidParams, err)
	}
	state.enableDebug(request.Verbose)
	state.mu.Lock()
	if !state.handshaken {
		state.mu.Unlock()
		return nil, rpcError(protocol.ErrorCodeInvalidRequest, "Handshake must succeed before Initialize")
	}
	if state.initialized || state.initializing {
		state.mu.Unlock()
		return nil, rpcError(protocol.ErrorCodeInvalidRequest, "Extension is already initialized")
	}
	state.initializing = true
	state.mu.Unlock()

	components := Components{}
	var err error
	if state.options.InitializeComponents != nil {
		components, err = state.options.InitializeComponents(ctx, cloneInitializeRequest(request))
	} else {
		components.Tools, err = state.options.Initialize(ctx, cloneInitializeRequest(request))
	}
	if err != nil {
		state.mu.Lock()
		state.initializing = false
		state.mu.Unlock()
		return nil, mapCallError(err)
	}
	toolMap, toolValidators, manifestTools, toolCapabilities, err := validateTools(
		components.Tools,
		state.options.ToolInputValidator,
	)
	if err != nil {
		state.mu.Lock()
		state.initializing = false
		state.mu.Unlock()
		return nil, rpcErrorFrom(protocol.ErrorCodeExtensionRejected, err)
	}
	hooks, err := validateHooks(components.Hooks, components.HandleEvent)
	if err != nil {
		state.mu.Lock()
		state.initializing = false
		state.mu.Unlock()
		return nil, rpcErrorFrom(protocol.ErrorCodeExtensionRejected, err)
	}
	commands, manifestCommands, commandCapabilities, err := validateCommands(components.Commands)
	if err != nil {
		state.mu.Lock()
		state.initializing = false
		state.mu.Unlock()
		return nil, rpcErrorFrom(protocol.ErrorCodeExtensionRejected, err)
	}
	capabilities := mergeCapabilities(toolCapabilities, commandCapabilities)

	state.mu.Lock()
	state.initializing = false
	state.initialized = true
	state.tools = toolMap
	state.toolValidators = toolValidators
	state.hooks = hooks
	state.hookHandler = components.HandleEvent
	state.commands = commands
	state.manifest = protocol.Manifest{
		ID:              state.options.ID,
		Version:         state.options.Version,
		ProtocolVersion: protocol.Version,
		Capabilities:    capabilities,
		Tools:           manifestTools,
		Hooks:           append([]string(nil), components.Hooks...),
		Commands:        manifestCommands,
	}
	state.mu.Unlock()
	state.debug("extension.initialized",
		"tool_count", len(manifestTools),
		"hook_count", len(components.Hooks),
		"command_count", len(manifestCommands),
		"verbose", request.Verbose,
	)
	return protocol.Empty{}, nil
}

func (state *state) enableDebug(verbose bool) {
	if !verbose || state.options.DebugWriter == nil {
		return
	}
	logger := slog.New(slog.NewJSONHandler(state.options.DebugWriter, &slog.HandlerOptions{Level: slog.LevelDebug})).With(
		"qed_safe_debug", true,
		"component", "extension_server",
		"extension_id", state.options.ID,
	)
	state.loggerMu.Lock()
	state.logger = logger
	state.loggerMu.Unlock()
}

func (state *state) debug(message string, arguments ...any) {
	state.loggerMu.RLock()
	logger := state.logger
	state.loggerMu.RUnlock()
	if logger != nil {
		logger.Debug(message, arguments...)
	}
}

func (state *state) describe(params json.RawMessage) (any, *protocol.RPCError) {
	if err := decodeEmpty(params); err != nil {
		return nil, rpcErrorFrom(protocol.ErrorCodeInvalidParams, err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.initialized {
		return nil, rpcError(protocol.ErrorCodeNotInitialized, "Extension is not initialized")
	}
	return protocol.DescribeResponse{Manifest: cloneManifest(state.manifest)}, nil
}

func (state *state) requiredCapabilities(ctx context.Context, params json.RawMessage) (any, *protocol.RPCError) {
	var request protocol.RequiredCapabilitiesRequest
	if err := protocol.Unmarshal(params, &request); err != nil {
		return nil, rpcErrorFrom(protocol.ErrorCodeInvalidParams, err)
	}
	tool, call, finish, rpcFailure := state.beginToolCall(request.Call)
	if rpcFailure != nil {
		return nil, rpcFailure
	}
	defer finish()
	if dynamic, ok := tool.(extension.DynamicCapabilities); ok {
		capabilities, err := dynamic.RequiredCapabilities(ctx, call)
		if err != nil {
			return nil, mapCallError(err)
		}
		result := make([]string, len(capabilities))
		for index, name := range capabilities {
			if err := capability.ValidateName(name); err != nil {
				return nil, rpcErrorFrom(protocol.ErrorCodeExtensionRejected, err)
			}
			result[index] = string(name)
		}
		if err := ctx.Err(); err != nil {
			return nil, mapCallError(err)
		}
		return protocol.RequiredCapabilitiesResponse{Capabilities: result}, nil
	}
	return protocol.RequiredCapabilitiesResponse{}, nil
}

func (state *state) approvalPreview(ctx context.Context, params json.RawMessage) (any, *protocol.RPCError) {
	var request protocol.ApprovalPreviewRequest
	if err := protocol.Unmarshal(params, &request); err != nil {
		return nil, rpcErrorFrom(protocol.ErrorCodeInvalidParams, err)
	}
	tool, call, finish, rpcFailure := state.beginToolCall(request.Call)
	if rpcFailure != nil {
		return nil, rpcFailure
	}
	defer finish()
	previewer, ok := tool.(extension.ApprovalPreviewer)
	if !ok {
		return protocol.ApprovalPreviewResponse{}, nil
	}
	preview, err := previewer.ApprovalPreview(ctx, call)
	if err != nil {
		return nil, mapCallError(err)
	}
	if err := capability.ValidateApprovalPreview(preview); err != nil {
		return nil, rpcErrorFrom(protocol.ErrorCodeExtensionRejected, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, mapCallError(err)
	}
	return protocol.ApprovalPreviewResponse{Preview: toProtocolApprovalPreview(preview)}, nil
}

func toProtocolApprovalPreview(preview *capability.ApprovalPreview) *protocol.ApprovalPreview {
	if preview == nil {
		return nil
	}
	result := &protocol.ApprovalPreview{
		Summary: preview.Summary,
		Details: make([]protocol.ApprovalPreviewDetail, len(preview.Details)),
	}
	for index, detail := range preview.Details {
		result.Details[index] = protocol.ApprovalPreviewDetail{Label: detail.Label, Value: detail.Value}
	}
	return result
}

func (state *state) invokeTool(ctx context.Context, params json.RawMessage) (any, *protocol.RPCError) {
	var request protocol.InvokeToolRequest
	if err := protocol.Unmarshal(params, &request); err != nil {
		return nil, rpcErrorFrom(protocol.ErrorCodeInvalidParams, err)
	}
	tool, call, finish, rpcFailure := state.beginToolCall(request.Call)
	if rpcFailure != nil {
		return nil, rpcFailure
	}
	defer finish()
	ctx = agent.WithRunInfo(ctx, agent.RunInfo{
		RunID:       request.Run.RunID,
		ParentRunID: request.Run.ParentRunID,
		AgentID:     request.Run.AgentID,
		SessionID:   request.Run.SessionID,
	})
	result, err := tool.Execute(ctx, call)
	if err != nil {
		return nil, mapCallError(err)
	}
	if err := agent.ValidateContextOperation(result.ContextOperation); err != nil {
		return nil, rpcErrorFrom(protocol.ErrorCodeExtensionRejected, fmt.Errorf("Tool context operation: %w", err))
	}
	return protocol.InvokeToolResponse{Result: protocol.ToolResult{
		CallID:           request.Call.ID,
		Name:             request.Call.Name,
		Output:           result.Output,
		IsError:          result.IsError,
		ContextOperation: toProtocolContextOperation(result.ContextOperation),
	}}, nil
}

func toProtocolContextOperation(operation *agent.ContextOperation) *protocol.ContextOperation {
	if operation == nil {
		return nil
	}
	return &protocol.ContextOperation{Kind: string(operation.Kind)}
}

func (state *state) handleEvent(ctx context.Context, params json.RawMessage) (any, *protocol.RPCError) {
	var request protocol.HandleEventRequest
	if err := protocol.Unmarshal(params, &request); err != nil {
		return nil, rpcErrorFrom(protocol.ErrorCodeInvalidParams, err)
	}
	if request.Event.Type == "" || strings.TrimSpace(request.Event.Type) != request.Event.Type {
		return nil, rpcError(protocol.ErrorCodeInvalidParams, "Event type is required and must not have surrounding whitespace")
	}
	if len(request.Event.Payload) == 0 || !json.Valid(request.Event.Payload) {
		return nil, rpcError(protocol.ErrorCodeInvalidParams, "Event payload must contain valid JSON")
	}
	var identity struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(request.Event.Payload, &identity); err != nil || identity.Type != request.Event.Type {
		return nil, rpcError(protocol.ErrorCodeInvalidParams, "Event payload type does not match Event type")
	}
	finish, handler, failure := state.beginHook(request.Event.Type)
	if failure != nil {
		return nil, failure
	}
	defer finish()
	ctx = withRunInfo(ctx, request.Run)
	if err := handler(ctx, cloneHandleEventRequest(request)); err != nil {
		return nil, mapCallError(err)
	}
	return protocol.Empty{}, nil
}

func (state *state) invokeCommand(ctx context.Context, params json.RawMessage) (any, *protocol.RPCError) {
	var request protocol.InvokeCommandRequest
	if err := protocol.Unmarshal(params, &request); err != nil {
		return nil, rpcErrorFrom(protocol.ErrorCodeInvalidParams, err)
	}
	if request.Call.Name == "" || strings.TrimSpace(request.Call.Name) != request.Call.Name {
		return nil, rpcError(protocol.ErrorCodeInvalidParams, "Command name is required and must not have surrounding whitespace")
	}
	arguments := request.Call.Arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if !json.Valid(arguments) {
		return nil, rpcError(protocol.ErrorCodeInvalidParams, "Command arguments must contain valid JSON")
	}
	command, finish, failure := state.beginCommand(request.Call.Name)
	if failure != nil {
		return nil, failure
	}
	defer finish()
	ctx = withRunInfo(ctx, request.Run)
	result, err := command.Execute(ctx, extension.CommandCall{
		Name:      request.Call.Name,
		Arguments: append(json.RawMessage(nil), arguments...),
	})
	if err != nil {
		return nil, mapCallError(err)
	}
	if len(result.Output) == 0 {
		result.Output = json.RawMessage(`null`)
	}
	if !json.Valid(result.Output) {
		return nil, rpcError(protocol.ErrorCodeExtensionRejected, "Extension Command returned invalid JSON")
	}
	return protocol.InvokeCommandResponse{Result: protocol.CommandResult{
		Output: append(json.RawMessage(nil), result.Output...),
	}}, nil
}

func withRunInfo(ctx context.Context, run protocol.RunInfo) context.Context {
	return agent.WithRunInfo(ctx, agent.RunInfo{
		RunID:       run.RunID,
		ParentRunID: run.ParentRunID,
		AgentID:     run.AgentID,
		SessionID:   run.SessionID,
	})
}

func (state *state) beginHook(eventType string) (func(), HookHandler, *protocol.RPCError) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if failure := state.beginOperationLocked(); failure != nil {
		return nil, nil, failure
	}
	if _, ok := state.hooks[eventType]; !ok || state.hookHandler == nil {
		state.endOperationLocked()
		return nil, nil, rpcError(protocol.ErrorCodeInvalidParams, fmt.Sprintf("Event Hook %q is not registered", eventType))
	}
	return state.endOperation, state.hookHandler, nil
}

func (state *state) beginCommand(name string) (extension.Command, func(), *protocol.RPCError) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if failure := state.beginOperationLocked(); failure != nil {
		return nil, nil, failure
	}
	command, ok := state.commands[name]
	if !ok {
		state.endOperationLocked()
		return nil, nil, rpcError(protocol.ErrorCodeInvalidParams, fmt.Sprintf("Command %q is not registered", name))
	}
	return command, state.endOperation, nil
}

func (state *state) beginToolCall(
	call protocol.ToolCall,
) (agent.Tool, agent.ToolCall, func(), *protocol.RPCError) {
	if call.ID == "" || call.Name == "" {
		return nil, agent.ToolCall{}, nil, rpcError(protocol.ErrorCodeInvalidParams, "Tool call ID and name are required")
	}
	state.mu.Lock()
	if failure := state.beginOperationLocked(); failure != nil {
		state.mu.Unlock()
		return nil, agent.ToolCall{}, nil, failure
	}
	tool, ok := state.tools[call.Name]
	if !ok {
		state.endOperationLocked()
		state.mu.Unlock()
		return nil, agent.ToolCall{}, nil, rpcError(protocol.ErrorCodeInvalidParams, fmt.Sprintf("Tool %q is not registered", call.Name))
	}
	validator := state.toolValidators[call.Name]
	state.mu.Unlock()
	agentCall := toAgentToolCall(call)
	if err := agent.ValidateToolInput(validator, agentCall.Arguments); err != nil {
		state.endOperation()
		return nil, agent.ToolCall{}, nil, rpcErrorFrom(protocol.ErrorCodeInvalidParams, err)
	}
	return tool, agentCall, state.endOperation, nil
}

func (state *state) beginOperationLocked() *protocol.RPCError {
	if !state.initialized {
		return rpcError(protocol.ErrorCodeNotInitialized, "Extension is not initialized")
	}
	if state.draining {
		return rpcError(protocol.ErrorCodeDraining, "Extension is draining")
	}
	if state.active == 0 {
		state.idle = make(chan struct{})
	}
	state.active++
	return nil
}

func (state *state) endOperation() {
	state.mu.Lock()
	state.endOperationLocked()
	state.mu.Unlock()
}

func (state *state) endOperationLocked() {
	state.active--
	if state.active == 0 {
		close(state.idle)
	}
}

func (state *state) healthCheck(params json.RawMessage) (any, *protocol.RPCError) {
	if err := decodeEmpty(params); err != nil {
		return nil, rpcErrorFrom(protocol.ErrorCodeInvalidParams, err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	status := "ready"
	if !state.initialized {
		status = "starting"
	}
	if state.draining {
		status = "draining"
	}
	return protocol.HealthCheckResponse{
		Status:      status,
		Initialized: state.initialized,
		Draining:    state.draining,
	}, nil
}

func (state *state) snapshot(ctx context.Context, params json.RawMessage) (any, *protocol.RPCError) {
	if err := decodeEmpty(params); err != nil {
		return nil, rpcErrorFrom(protocol.ErrorCodeInvalidParams, err)
	}
	state.mu.Lock()
	initialized := state.initialized
	state.mu.Unlock()
	if !initialized {
		return nil, rpcError(protocol.ErrorCodeNotInitialized, "Extension is not initialized")
	}
	data := json.RawMessage(`{}`)
	if state.options.Snapshot != nil {
		var err error
		data, err = state.options.Snapshot(ctx)
		if err != nil {
			return nil, mapCallError(err)
		}
		if len(data) == 0 {
			data = json.RawMessage(`{}`)
		}
	}
	if !json.Valid(data) {
		return nil, rpcError(protocol.ErrorCodeExtensionRejected, "Extension Snapshot returned invalid JSON")
	}
	return protocol.SnapshotResponse{State: append(json.RawMessage(nil), data...)}, nil
}

func (state *state) restore(ctx context.Context, params json.RawMessage) (any, *protocol.RPCError) {
	var request protocol.RestoreRequest
	if err := protocol.Unmarshal(params, &request); err != nil {
		return nil, rpcErrorFrom(protocol.ErrorCodeInvalidParams, err)
	}
	if len(request.State) == 0 || !json.Valid(request.State) {
		return nil, rpcError(protocol.ErrorCodeInvalidParams, "Restore state must contain valid JSON")
	}
	state.mu.Lock()
	initialized := state.initialized
	state.mu.Unlock()
	if !initialized {
		return nil, rpcError(protocol.ErrorCodeNotInitialized, "Extension is not initialized")
	}
	if state.options.Restore != nil {
		if err := state.options.Restore(ctx, append(json.RawMessage(nil), request.State...)); err != nil {
			return nil, mapCallError(err)
		}
	}
	return protocol.Empty{}, nil
}

func (state *state) drain(ctx context.Context) error {
	state.mu.Lock()
	state.draining = true
	idle := state.idle
	state.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (state *state) writeResult(id string, value any) error {
	result, err := protocol.Marshal(value)
	if err != nil {
		return err
	}
	return state.writer.Write(protocol.Envelope{
		Version: protocol.Version,
		ID:      id,
		Result:  result,
	})
}

func (state *state) writeError(id string, rpcError *protocol.RPCError) error {
	return state.writer.Write(protocol.Envelope{
		Version: protocol.Version,
		ID:      id,
		Error:   rpcError,
	})
}

func (state *state) cancelRequests() {
	state.requestsMu.Lock()
	for _, cancel := range state.requests {
		cancel()
	}
	state.requestsMu.Unlock()
}

func validateTools(
	tools []agent.Tool,
	validator agent.ToolInputValidator,
) (
	map[string]agent.Tool,
	map[string]agent.CompiledToolInputValidator,
	[]protocol.ToolDefinition,
	[]string,
	error,
) {
	toolMap := make(map[string]agent.Tool, len(tools))
	validators := make(map[string]agent.CompiledToolInputValidator, len(tools))
	definitions := make([]protocol.ToolDefinition, 0, len(tools))
	capabilitySet := make(map[string]struct{})
	for _, tool := range tools {
		if tool == nil {
			return nil, nil, nil, nil, errors.New("Extension Tool must not be nil")
		}
		definition := tool.Definition()
		if strings.TrimSpace(definition.Name) != definition.Name || definition.Name == "" {
			return nil, nil, nil, nil, errors.New("Extension Tool name is required")
		}
		if _, duplicate := toolMap[definition.Name]; duplicate {
			return nil, nil, nil, nil, fmt.Errorf("Extension Tool %q is registered more than once", definition.Name)
		}
		compiled, err := agent.CompileToolInputSchema(validator, definition.InputSchema)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("Extension Tool %q input schema: %w", definition.Name, err)
		}
		capabilities := append([]string(nil), definition.Capabilities...)
		for _, value := range capabilities {
			if err := capability.ValidateName(capability.Name(value)); err != nil {
				return nil, nil, nil, nil, fmt.Errorf("Extension Tool %q: %w", definition.Name, err)
			}
			capabilitySet[value] = struct{}{}
		}
		toolMap[definition.Name] = tool
		validators[definition.Name] = compiled
		_, dynamic := tool.(extension.DynamicCapabilities)
		definitions = append(definitions, protocol.ToolDefinition{
			Name:                definition.Name,
			Description:         definition.Description,
			InputSchema:         append(json.RawMessage(nil), definition.InputSchema...),
			Capabilities:        capabilities,
			DynamicCapabilities: dynamic,
		})
	}
	capabilities := make([]string, 0, len(capabilitySet))
	for name := range capabilitySet {
		capabilities = append(capabilities, name)
	}
	sort.Strings(capabilities)
	return toolMap, validators, definitions, capabilities, nil
}

func validateHooks(hooks []string, handler HookHandler) (map[string]struct{}, error) {
	if len(hooks) > 0 && handler == nil {
		return nil, errors.New("Extension Hooks require an Event handler")
	}
	if len(hooks) == 0 && handler != nil {
		return nil, errors.New("Extension Event handler requires at least one Hook")
	}
	result := make(map[string]struct{}, len(hooks))
	for _, eventType := range hooks {
		if eventType == "" || strings.TrimSpace(eventType) != eventType {
			return nil, errors.New("Extension Hook Event type is required and must not have surrounding whitespace")
		}
		if _, duplicate := result[eventType]; duplicate {
			return nil, fmt.Errorf("Extension Hook %q is registered more than once", eventType)
		}
		result[eventType] = struct{}{}
	}
	return result, nil
}

func validateCommands(commands []extension.Command) (map[string]extension.Command, []protocol.CommandDefinition, []string, error) {
	commandMap := make(map[string]extension.Command, len(commands))
	definitions := make([]protocol.CommandDefinition, 0, len(commands))
	capabilitySet := make(map[string]struct{})
	for _, command := range commands {
		if command == nil {
			return nil, nil, nil, errors.New("Extension Command must not be nil")
		}
		definition := command.Definition()
		if definition.Name == "" || strings.TrimSpace(definition.Name) != definition.Name {
			return nil, nil, nil, errors.New("Extension Command name is required and must not have surrounding whitespace")
		}
		if _, duplicate := commandMap[definition.Name]; duplicate {
			return nil, nil, nil, fmt.Errorf("Extension Command %q is registered more than once", definition.Name)
		}
		if len(definition.InputSchema) > 0 && !json.Valid(definition.InputSchema) {
			return nil, nil, nil, fmt.Errorf("Extension Command %q has invalid input schema", definition.Name)
		}
		seen := make(map[string]struct{}, len(definition.Capabilities))
		capabilities := append([]string(nil), definition.Capabilities...)
		for _, value := range capabilities {
			if err := capability.ValidateName(capability.Name(value)); err != nil {
				return nil, nil, nil, fmt.Errorf("Extension Command %q: %w", definition.Name, err)
			}
			if _, duplicate := seen[value]; duplicate {
				return nil, nil, nil, fmt.Errorf("Extension Command %q capability %q is registered more than once", definition.Name, value)
			}
			seen[value] = struct{}{}
			capabilitySet[value] = struct{}{}
		}
		commandMap[definition.Name] = command
		definitions = append(definitions, protocol.CommandDefinition{
			Name:         definition.Name,
			Description:  definition.Description,
			InputSchema:  append(json.RawMessage(nil), definition.InputSchema...),
			Capabilities: capabilities,
		})
	}
	capabilities := make([]string, 0, len(capabilitySet))
	for name := range capabilitySet {
		capabilities = append(capabilities, name)
	}
	sort.Strings(capabilities)
	return commandMap, definitions, capabilities, nil
}

func mergeCapabilities(groups ...[]string) []string {
	set := make(map[string]struct{})
	for _, group := range groups {
		for _, name := range group {
			set[name] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for name := range set {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func cloneInitializeRequest(request protocol.InitializeRequest) protocol.InitializeRequest {
	request.Configuration = append(json.RawMessage(nil), request.Configuration...)
	if request.Environment != nil {
		cloned := make(map[string]string, len(request.Environment))
		for name, value := range request.Environment {
			cloned[name] = value
		}
		request.Environment = cloned
	}
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

func cloneHandleEventRequest(request protocol.HandleEventRequest) protocol.HandleEventRequest {
	request.Event.Payload = append(json.RawMessage(nil), request.Event.Payload...)
	return request
}

func toAgentToolCall(call protocol.ToolCall) agent.ToolCall {
	arguments := append(json.RawMessage(nil), call.Arguments...)
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	return agent.ToolCall{ID: call.ID, Name: call.Name, Arguments: arguments}
}

func decodeEmpty(data json.RawMessage) error {
	var empty protocol.Empty
	return protocol.Unmarshal(data, &empty)
}

func rpcError(code protocol.ErrorCode, message string) *protocol.RPCError {
	return &protocol.RPCError{Code: code, Message: message}
}

func rpcErrorFrom(code protocol.ErrorCode, err error) *protocol.RPCError {
	return rpcError(code, err.Error())
}

func mapCallError(err error) *protocol.RPCError {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return rpcErrorFrom(protocol.ErrorCodeRequestCanceled, err)
	}
	return rpcErrorFrom(protocol.ErrorCodeExtensionRejected, err)
}
