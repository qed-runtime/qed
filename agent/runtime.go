package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxProviderCalls = 16
	defaultMaxToolCalls     = 64
)

// ErrProviderCallLimit indicates that a Run exhausted its Provider call budget
var ErrProviderCallLimit = errors.New("provider call limit reached")

// ErrToolCallLimit indicates that a Run exhausted its Tool call budget
var ErrToolCallLimit = errors.New("tool call limit reached")

// Options configures a Runtime
type Options struct {
	// Provider is required and is shared by every Run
	Provider Provider
	// Tools contains the fixed Tool registry shared by every Run
	Tools []Tool
	// ToolSource supplies one Tool generation that remains pinned for a Run
	//
	// Source Tools are ordered before fixed Tools when both are configured
	ToolSource ToolSource
	// ComponentSource supplies one atomic Tool and Hook generation set for a Run
	//
	// ComponentSource and ToolSource are mutually exclusive
	ComponentSource ComponentSource
	// Hooks contains fixed Run Event Hooks shared by every Run
	Hooks []Hook
	// MaxProviderCalls bounds one Run and defaults to 16 when zero
	MaxProviderCalls int
	// MaxToolCalls bounds one Run and defaults to 64 when zero
	MaxToolCalls int
	// SessionStore persists Run Events and reconstructs Session context
	//
	// A nil Store keeps Runs ephemeral
	SessionStore SessionStore
	// Logger receives safe structured debug diagnostics without message content,
	// Tool arguments, Tool output, metadata values, or Provider-private state
	Logger *slog.Logger
}

// Runtime executes Agent Runs with one Provider and a Run-scoped Tool registry
//
// Runtime is safe for concurrent use after construction
type Runtime struct {
	provider         Provider
	staticTools      runtimeToolSet
	toolSource       ToolSource
	componentSource  ComponentSource
	staticHooks      []runtimeHook
	maxProviderCalls int
	maxToolCalls     int
	sessionStore     SessionStore
	logger           *slog.Logger
	sessionMu        sync.Mutex
	sessionLocks     map[string]*runtimeSessionLock
}

type runtimeSessionLock struct {
	token chan struct{}
	refs  int
}

type runtimeToolSet struct {
	tools       map[string]Tool
	definitions []ToolDefinition
}

type runtimeHook struct {
	hook       Hook
	eventTypes map[EventType]struct{}
	definition HookDefinition
}

// NewRuntime validates options and constructs an immutable Runtime
func NewRuntime(options Options) (*Runtime, error) {
	if options.Provider == nil {
		return nil, errors.New("provider is required")
	}
	maxProviderCalls := options.MaxProviderCalls
	if maxProviderCalls == 0 {
		maxProviderCalls = defaultMaxProviderCalls
	}
	if maxProviderCalls < 0 {
		return nil, errors.New("max provider calls must be positive")
	}

	maxToolCalls := options.MaxToolCalls
	if maxToolCalls == 0 {
		maxToolCalls = defaultMaxToolCalls
	}
	if maxToolCalls < 0 {
		return nil, errors.New("max tool calls must be positive")
	}

	staticTools, err := newRuntimeToolSet(options.Tools)
	if err != nil {
		return nil, err
	}
	if options.ToolSource != nil && options.ComponentSource != nil {
		return nil, errors.New("ToolSource and ComponentSource are mutually exclusive")
	}
	staticHooks, err := newRuntimeHooks(options.Hooks)
	if err != nil {
		return nil, err
	}

	return &Runtime{
		provider:         options.Provider,
		staticTools:      staticTools,
		toolSource:       options.ToolSource,
		componentSource:  options.ComponentSource,
		staticHooks:      staticHooks,
		maxProviderCalls: maxProviderCalls,
		maxToolCalls:     maxToolCalls,
		sessionStore:     options.SessionStore,
		logger:           options.Logger,
		sessionLocks:     make(map[string]*runtimeSessionLock),
	}, nil
}

func newRuntimeHooks(configured []Hook) ([]runtimeHook, error) {
	hooks := make([]runtimeHook, 0, len(configured))
	for _, hook := range configured {
		if hook == nil {
			return nil, errors.New("Hook must not be nil")
		}
		definition := hook.Definition()
		definition.EventTypes = append([]EventType(nil), definition.EventTypes...)
		if len(definition.EventTypes) == 0 {
			return nil, errors.New("Hook must observe at least one Event type")
		}
		eventTypes := make(map[EventType]struct{}, len(definition.EventTypes))
		for _, eventType := range definition.EventTypes {
			if eventType == "" || strings.TrimSpace(string(eventType)) != string(eventType) {
				return nil, errors.New("Hook Event type is required and must not have surrounding whitespace")
			}
			if _, duplicate := eventTypes[eventType]; duplicate {
				return nil, fmt.Errorf("Hook Event type %q is registered more than once", eventType)
			}
			eventTypes[eventType] = struct{}{}
		}
		hooks = append(hooks, runtimeHook{hook: hook, eventTypes: eventTypes, definition: definition})
	}
	return hooks, nil
}

func newRuntimeToolSet(configured []Tool) (runtimeToolSet, error) {
	tools := make(map[string]Tool, len(configured))
	definitions := make([]ToolDefinition, 0, len(configured))
	for _, tool := range configured {
		if tool == nil {
			return runtimeToolSet{}, errors.New("tool must not be nil")
		}

		definition := cloneToolDefinition(tool.Definition())
		if strings.TrimSpace(definition.Name) != definition.Name || definition.Name == "" {
			return runtimeToolSet{}, errors.New("tool name is required")
		}
		if len(definition.InputSchema) > 0 && !json.Valid(definition.InputSchema) {
			return runtimeToolSet{}, fmt.Errorf("tool %q has an invalid input schema", definition.Name)
		}
		if _, exists := tools[definition.Name]; exists {
			return runtimeToolSet{}, fmt.Errorf("tool %q is registered more than once", definition.Name)
		}
		capabilities := make(map[string]struct{}, len(definition.Capabilities))
		for _, capability := range definition.Capabilities {
			if capability == "" || strings.TrimSpace(capability) != capability {
				return runtimeToolSet{}, fmt.Errorf("tool %q capability is required and must not have surrounding whitespace", definition.Name)
			}
			if _, duplicate := capabilities[capability]; duplicate {
				return runtimeToolSet{}, fmt.Errorf("tool %q capability %q is registered more than once", definition.Name, capability)
			}
			capabilities[capability] = struct{}{}
		}

		tools[definition.Name] = tool
		definitions = append(definitions, definition)
	}
	return runtimeToolSet{tools: tools, definitions: definitions}, nil
}

// Run starts an Agent Run and returns immediately with a handle
//
// The caller should consume Events until the channel closes and then call Wait
// to obtain the terminal result
func (runtime *Runtime) Run(ctx context.Context, request RunRequest) (*RunHandle, error) {
	if ctx == nil {
		return nil, errors.New("context must not be nil")
	}
	if request.Resume == nil && len(request.Input) == 0 {
		return nil, errors.New("run input is required")
	}
	if request.Resume != nil {
		if request.SessionID == "" {
			return nil, errors.New("Session ID is required to resume a Run")
		}
		if len(request.Input) != 0 {
			return nil, errors.New("run input must be empty when resuming")
		}
		if runtime.sessionStore == nil {
			return nil, errors.New("Session Store is required to resume a Run")
		}
		if request.Resume.RequestID == "" {
			return nil, errors.New("resume request ID is required")
		}
	}
	if !request.Deadline.IsZero() && !request.Deadline.After(time.Now()) {
		return nil, context.DeadlineExceeded
	}

	runContext, cancel := runtime.runContext(ctx, request)

	toolSet, hooks, releaseTools, err := runtime.acquireComponents(runContext)
	if err != nil {
		cancel()
		return nil, err
	}

	runID, err := newRunID()
	if err != nil {
		releaseTools()
		cancel()
		return nil, fmt.Errorf("create run ID: %w", err)
	}

	handle := &RunHandle{
		events: make(chan Event, runtime.eventBufferSize()),
		done:   make(chan struct{}),
		cancel: cancel,
	}

	request.Input = cloneMessages(request.Input)
	request.Metadata = cloneMetadata(request.Metadata)
	request.Capabilities = append([]string(nil), request.Capabilities...)
	if request.Resume != nil {
		resume := cloneWaitResponse(*request.Resume)
		request.Resume = &resume
	}
	go runtime.execute(runContext, runID, request, handle, toolSet, hooks, releaseTools)

	return handle, nil
}

func (runtime *Runtime) runContext(ctx context.Context, request RunRequest) (context.Context, context.CancelFunc) {
	deadline := request.Deadline
	if budgetDeadline, ok := request.Budget.Deadline(); ok && (deadline.IsZero() || budgetDeadline.Before(deadline)) {
		deadline = budgetDeadline
	}
	if deadline.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline)
}

func (runtime *Runtime) acquireComponents(ctx context.Context) (runtimeToolSet, []runtimeHook, func(), error) {
	var tools []Tool
	var hooks []Hook
	var release func()
	var err error
	switch {
	case runtime.componentSource != nil:
		var components RunComponents
		components, release, err = runtime.componentSource.AcquireComponents(ctx)
		tools = components.Tools
		hooks = components.Hooks
	case runtime.toolSource != nil:
		tools, release, err = runtime.toolSource.AcquireTools(ctx)
	default:
		return runtime.staticTools, append([]runtimeHook(nil), runtime.staticHooks...), func() {}, nil
	}
	if err != nil {
		return runtimeToolSet{}, nil, nil, fmt.Errorf("acquire Run components: %w", err)
	}
	if release == nil {
		release = func() {}
	}
	combined := make([]Tool, 0, len(tools)+len(runtime.staticTools.definitions))
	combined = append(combined, tools...)
	for _, definition := range runtime.staticTools.definitions {
		combined = append(combined, runtime.staticTools.tools[definition.Name])
	}
	toolSet, err := newRuntimeToolSet(combined)
	if err != nil {
		release()
		return runtimeToolSet{}, nil, nil, fmt.Errorf("validate acquired Run tools: %w", err)
	}
	combinedHooks := make([]Hook, 0, len(hooks)+len(runtime.staticHooks))
	combinedHooks = append(combinedHooks, hooks...)
	for _, configured := range runtime.staticHooks {
		combinedHooks = append(combinedHooks, configured.hook)
	}
	runtimeHooks, err := newRuntimeHooks(combinedHooks)
	if err != nil {
		release()
		return runtimeToolSet{}, nil, nil, fmt.Errorf("validate acquired Run Hooks: %w", err)
	}
	return toolSet, runtimeHooks, release, nil
}

func (runtime *Runtime) eventBufferSize() int {
	return 16 + 4*runtime.maxProviderCalls + 4*runtime.maxToolCalls
}

func (runtime *Runtime) execute(
	ctx context.Context,
	runID string,
	request RunRequest,
	handle *RunHandle,
	toolSet runtimeToolSet,
	hooks []runtimeHook,
	releaseTools func(),
) {
	runStartedAt := time.Now()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(releaseTools) }
	defer release()
	defer handle.cancel()
	loaded, releaseSession, err := runtime.loadSession(ctx, request)
	if err != nil {
		runtime.debug("run.execution.finished",
			"run_id", runID,
			"status", RunStatusFailed,
			"provider_calls", 0,
			"tool_calls", 0,
			"duration_ms", time.Since(runStartedAt).Milliseconds(),
			"error_type", errorType(err),
		)
		handle.complete(RunResult{
			RunID:     runID,
			AgentID:   request.AgentID,
			SessionID: request.SessionID,
			Status:    RunStatusFailed,
		}, err)
		return
	}
	defer releaseSession()
	messages := loaded.messages
	sessionRevision := loaded.revision

	ctx = WithRunInfo(ctx, RunInfo{
		RunID:        runID,
		ParentRunID:  request.ParentRunID,
		AgentID:      request.AgentID,
		SessionID:    request.SessionID,
		Capabilities: append([]string(nil), request.Capabilities...),
	})
	runtime.debug("run.execution.started",
		"run_id", runID,
		"agent_id", request.AgentID,
		"session_configured", request.SessionID != "",
		"provider", runtime.provider.Name(),
		"tool_count", len(toolSet.definitions),
		"hook_count", len(hooks),
	)

	toolResults := make([]ToolResult, 0)
	providerCalls := 0
	toolCalls := 0
	sequence := uint64(0)
	usage := Usage{}

	emit := func(event Event) error {
		event.Sequence = sequence + 1
		event.RunID = runID
		event.ParentRunID = request.ParentRunID
		event.AgentID = request.AgentID
		event.SessionID = request.SessionID
		event.Time = time.Now().UTC()
		for _, configured := range hooks {
			if _, observes := configured.eventTypes[event.Type]; !observes {
				continue
			}
			hookStartedAt := time.Now()
			if err := configured.hook.Handle(ctx, cloneEvent(event)); err != nil {
				runtime.debug("run.hook.failed",
					"run_id", runID,
					"event_type", event.Type,
					"extension_id", configured.definition.ExtensionID,
					"extension_generation", configured.definition.ExtensionGeneration,
					"duration_ms", time.Since(hookStartedAt).Milliseconds(),
					"error_type", fmt.Sprintf("%T", err),
				)
				identity := configured.definition.ExtensionID
				if identity == "" {
					identity = "host"
				}
				return fmt.Errorf("handle Event %q with Hook %q generation %d: %w", event.Type, identity, configured.definition.ExtensionGeneration, err)
			}
			runtime.debug("run.hook.completed",
				"run_id", runID,
				"event_type", event.Type,
				"extension_id", configured.definition.ExtensionID,
				"extension_generation", configured.definition.ExtensionGeneration,
				"duration_ms", time.Since(hookStartedAt).Milliseconds(),
			)
		}
		if runtime.sessionStore != nil && request.SessionID != "" {
			persistContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			revision, appendErr := runtime.sessionStore.Append(
				persistContext,
				request.SessionID,
				sessionRevision,
				[]Event{cloneEvent(event)},
			)
			cancel()
			if appendErr != nil {
				return fmt.Errorf("append Session event %q: %w", event.Type, appendErr)
			}
			sessionRevision = revision
			event.SessionRevision = revision
		}
		sequence = event.Sequence
		handle.events <- cloneEvent(event)
		runtime.debug("run.event.emitted",
			"run_id", runID,
			"event_type", event.Type,
			"sequence", event.Sequence,
			"session_revision", event.SessionRevision,
		)
		return nil
	}
	var waiter *waitBroker
	if request.Resume != nil && loaded.pendingWait != nil {
		waiter = newResumingWaitBroker(emit, *loaded.pendingWait, *request.Resume)
	} else {
		waiter = newWaitBroker(emit)
	}
	handle.setWaiter(waiter)
	defer waiter.close()
	ctx = context.WithValue(ctx, runWaiterContextKey{}, runWaiter(waiter))

	finish := func(status RunStatus, eventType EventType, runErr error) {
		event := Event{Type: eventType}
		if runErr != nil {
			event.Error = runErr.Error()
		}
		emitErr := emit(event)
		if emitErr != nil {
			runErr = errors.Join(runErr, emitErr)
			if status == RunStatusCompleted {
				status = RunStatusFailed
				event = Event{Type: EventRunFailed, Error: runErr.Error()}
				emitErr = emit(event)
				if emitErr != nil {
					runErr = errors.Join(runErr, emitErr)
				}
			}
		}
		if emitErr != nil {
			sequence++
			event.Sequence = sequence
			event.RunID = runID
			event.ParentRunID = request.ParentRunID
			event.AgentID = request.AgentID
			event.SessionID = request.SessionID
			event.Time = time.Now().UTC()
			event.Error = runErr.Error()
			handle.events <- cloneEvent(event)
		}
		release()
		var budgetSnapshot *BudgetSnapshot
		if request.Budget != nil {
			snapshot := request.Budget.Snapshot()
			budgetSnapshot = &snapshot
		}

		runtime.debug("run.execution.finished",
			"run_id", runID,
			"status", status,
			"provider_calls", providerCalls,
			"tool_calls", toolCalls,
			"duration_ms", time.Since(runStartedAt).Milliseconds(),
			"error_type", errorType(runErr),
		)
		handle.complete(RunResult{
			RunID:           runID,
			ParentRunID:     request.ParentRunID,
			AgentID:         request.AgentID,
			SessionID:       request.SessionID,
			Status:          status,
			Messages:        messages,
			ToolResults:     toolResults,
			ProviderCalls:   providerCalls,
			ToolCalls:       toolCalls,
			Usage:           usage,
			Budget:          budgetSnapshot,
			SessionRevision: sessionRevision,
		}, runErr)
	}

	fail := func(runErr error) {
		if ctx.Err() != nil || errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			finish(RunStatusCanceled, EventRunCanceled, runErr)
			return
		}
		finish(RunStatusFailed, EventRunFailed, runErr)
	}

	if err := emit(Event{Type: EventRunStarted}); err != nil {
		fail(err)
		return
	}
	for index := range request.Input {
		input := cloneMessage(request.Input[index])
		if err := emit(Event{Type: EventUserMessageAdded, Message: &input}); err != nil {
			fail(err)
			return
		}
	}
	if request.Resume != nil {
		if loaded.pendingTool == nil {
			fail(errors.New("persisted Run wait is not associated with a resumable Tool call"))
			return
		}
		if err := request.Budget.consumeToolCalls(1); err != nil {
			fail(err)
			return
		}
		call := cloneToolCall(*loaded.pendingTool)
		toolCalls++
		if err := emit(Event{Type: EventToolStarted, ToolCall: &call}); err != nil {
			fail(err)
			return
		}
		result := runtime.executeToolWithDebug(ctx, runID, toolSet, call)
		if ctx.Err() != nil {
			fail(ctx.Err())
			return
		}
		toolResults = append(toolResults, result)
		messages = append(messages, Message{
			Role:        RoleTool,
			Text:        result.Output,
			ToolCallID:  result.CallID,
			ToolName:    result.Name,
			ToolIsError: result.IsError,
		})
		toolMessage := cloneMessage(messages[len(messages)-1])
		if err := emit(Event{Type: EventToolCompleted, Message: &toolMessage, ToolCall: &call, ToolResult: &result}); err != nil {
			fail(err)
			return
		}
	}

	for providerCalls < runtime.maxProviderCalls {
		if err := ctx.Err(); err != nil {
			fail(err)
			return
		}

		if err := request.Budget.consumeProviderCall(); err != nil {
			fail(err)
			return
		}

		providerCalls++
		providerStartedAt := time.Now()
		runtime.debug("provider.stream.started",
			"run_id", runID,
			"provider", runtime.provider.Name(),
			"call", providerCalls,
		)
		if err := emit(Event{Type: EventModelRequest}); err != nil {
			fail(err)
			return
		}
		stream, err := runtime.provider.Stream(ctx, ModelRequest{
			AgentID:      request.AgentID,
			SessionID:    request.SessionID,
			Metadata:     cloneMetadata(request.Metadata),
			Instructions: request.Instructions,
			Messages:     cloneMessages(messages),
			Tools:        cloneToolDefinitions(toolSet.definitions),
		})
		if err != nil {
			runtime.debug("provider.stream.failed",
				"run_id", runID,
				"provider", runtime.provider.Name(),
				"call", providerCalls,
				"duration_ms", time.Since(providerStartedAt).Milliseconds(),
				"error_type", fmt.Sprintf("%T", err),
			)
			fail(fmt.Errorf("provider %q failed: %w", runtime.provider.Name(), err))
			return
		}
		if err := emit(Event{Type: EventMessageStarted}); err != nil {
			_ = stream.Close()
			fail(err)
			return
		}
		message, err := consumeModelStream(ctx, stream, func(delta string) error {
			return emit(Event{Type: EventMessageDelta, Delta: delta})
		})
		if err != nil {
			runtime.debug("provider.stream.failed",
				"run_id", runID,
				"provider", runtime.provider.Name(),
				"call", providerCalls,
				"duration_ms", time.Since(providerStartedAt).Milliseconds(),
				"error_type", fmt.Sprintf("%T", err),
			)
			fail(fmt.Errorf("provider %q stream failed: %w", runtime.provider.Name(), err))
			return
		}
		runtime.debug("provider.stream.completed",
			"run_id", runID,
			"provider", runtime.provider.Name(),
			"call", providerCalls,
			"duration_ms", time.Since(providerStartedAt).Milliseconds(),
			"tool_call_count", len(message.ToolCalls),
		)
		if message.Role != RoleAssistant {
			fail(fmt.Errorf("provider %q returned message role %q, want %q", runtime.provider.Name(), message.Role, RoleAssistant))
			return
		}
		if message.Usage != nil {
			usage.InputTokens += message.Usage.InputTokens
			usage.OutputTokens += message.Usage.OutputTokens
			usage.TotalTokens += message.Usage.TotalTokens
			usage.CostMicros += message.Usage.CostMicros
			if err := request.Budget.recordUsage(message.Usage); err != nil {
				fail(err)
				return
			}
		}

		message = cloneMessage(message)
		for index := range message.ToolCalls {
			if message.ToolCalls[index].ID == "" {
				message.ToolCalls[index].ID = fmt.Sprintf("call-%d", toolCalls+index+1)
			}
		}
		messages = append(messages, message)
		if err := emit(Event{Type: EventMessageCompleted, Message: &message}); err != nil {
			fail(err)
			return
		}

		if len(message.ToolCalls) == 0 {
			finish(RunStatusCompleted, EventRunCompleted, nil)
			return
		}
		if toolCalls+len(message.ToolCalls) > runtime.maxToolCalls {
			fail(ErrToolCallLimit)
			return
		}
		if err := request.Budget.consumeToolCalls(len(message.ToolCalls)); err != nil {
			fail(err)
			return
		}

		for index := range message.ToolCalls {
			if err := ctx.Err(); err != nil {
				fail(err)
				return
			}

			call := cloneToolCall(message.ToolCalls[index])
			toolCalls++
			if err := emit(Event{Type: EventToolStarted, ToolCall: &call}); err != nil {
				fail(err)
				return
			}

			result := runtime.executeToolWithDebug(ctx, runID, toolSet, call)
			if ctx.Err() != nil {
				fail(ctx.Err())
				return
			}
			toolResults = append(toolResults, result)
			messages = append(messages, Message{
				Role:        RoleTool,
				Text:        result.Output,
				ToolCallID:  result.CallID,
				ToolName:    result.Name,
				ToolIsError: result.IsError,
			})
			toolMessage := cloneMessage(messages[len(messages)-1])
			if err := emit(Event{Type: EventToolCompleted, Message: &toolMessage, ToolCall: &call, ToolResult: &result}); err != nil {
				fail(err)
				return
			}
		}
	}

	fail(ErrProviderCallLimit)
}

func (runtime *Runtime) debug(message string, arguments ...any) {
	if runtime.logger != nil {
		runtime.logger.Debug(message, arguments...)
	}
}

func errorType(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}

type loadedSession struct {
	messages    []Message
	revision    uint64
	pendingWait *WaitRequest
	pendingTool *ToolCall
}

func (runtime *Runtime) loadSession(ctx context.Context, request RunRequest) (loadedSession, func(), error) {
	if runtime.sessionStore == nil || request.SessionID == "" {
		return loadedSession{messages: cloneMessages(request.Input)}, func() {}, nil
	}
	release, err := runtime.acquireSession(ctx, request.SessionID)
	if err != nil {
		return loadedSession{}, func() {}, err
	}
	snapshot, err := runtime.sessionStore.Load(ctx, request.SessionID)
	if errors.Is(err, ErrSessionNotFound) {
		snapshot = SessionSnapshot{ID: request.SessionID}
		err = nil
	}
	if err != nil {
		release()
		return loadedSession{}, func() {}, fmt.Errorf("load Session %q: %w", request.SessionID, err)
	}
	if request.Resume != nil {
		if snapshot.PendingWait == nil {
			release()
			return loadedSession{}, func() {}, fmt.Errorf("Session %q has no pending wait", request.SessionID)
		}
		if snapshot.PendingWait.ID != request.Resume.RequestID {
			release()
			return loadedSession{}, func() {}, fmt.Errorf("%w: Session %q is waiting for %q", ErrWaitResponseMismatch, request.SessionID, snapshot.PendingWait.ID)
		}
		if snapshot.PendingTool == nil {
			release()
			return loadedSession{}, func() {}, fmt.Errorf("Session %q pending wait is not resumable", request.SessionID)
		}
		wait := cloneWaitRequest(*snapshot.PendingWait)
		tool := cloneToolCall(*snapshot.PendingTool)
		return loadedSession{
			messages:    cloneMessages(snapshot.Messages),
			revision:    snapshot.Revision,
			pendingWait: &wait,
			pendingTool: &tool,
		}, release, nil
	}
	if snapshot.PendingWait != nil || snapshot.PendingTool != nil {
		release()
		return loadedSession{}, func() {}, fmt.Errorf("Session %q has unfinished work and must be resumed", request.SessionID)
	}
	messages := cloneMessages(snapshot.Messages)
	messages = append(messages, cloneMessages(request.Input)...)
	return loadedSession{messages: messages, revision: snapshot.Revision}, release, nil
}

func (runtime *Runtime) acquireSession(ctx context.Context, sessionID string) (func(), error) {
	runtime.sessionMu.Lock()
	lock := runtime.sessionLocks[sessionID]
	if lock == nil {
		lock = &runtimeSessionLock{token: make(chan struct{}, 1)}
		lock.token <- struct{}{}
		runtime.sessionLocks[sessionID] = lock
	}
	lock.refs++
	runtime.sessionMu.Unlock()

	select {
	case <-lock.token:
		return func() {
			lock.token <- struct{}{}
			runtime.sessionMu.Lock()
			lock.refs--
			if lock.refs == 0 {
				delete(runtime.sessionLocks, sessionID)
			}
			runtime.sessionMu.Unlock()
		}, nil
	case <-ctx.Done():
		runtime.sessionMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(runtime.sessionLocks, sessionID)
		}
		runtime.sessionMu.Unlock()
		return nil, ctx.Err()
	}
}

func (runtime *Runtime) executeTool(ctx context.Context, toolSet runtimeToolSet, call ToolCall) ToolResult {
	result := ToolResult{CallID: call.ID, Name: call.Name}
	tool, ok := toolSet.tools[call.Name]
	if !ok {
		result.Output = fmt.Sprintf("tool %q is not registered", call.Name)
		result.IsError = true
		return result
	}

	arguments := cloneRawMessage(call.Arguments)
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if !json.Valid(arguments) {
		result.Output = "tool arguments are not valid JSON"
		result.IsError = true
		return result
	}

	call.Arguments = arguments
	toolResult, err := tool.Execute(ctx, call)
	if err != nil {
		result.Output = err.Error()
		result.IsError = true
		return result
	}

	result.Output = toolResult.Output
	result.IsError = toolResult.IsError
	return result
}

func (runtime *Runtime) executeToolWithDebug(ctx context.Context, runID string, toolSet runtimeToolSet, call ToolCall) ToolResult {
	startedAt := time.Now()
	tool := toolSet.tools[call.Name]
	var definition ToolDefinition
	if tool != nil {
		definition = tool.Definition()
	}
	runtime.debug("tool.execution.started",
		"run_id", runID,
		"tool", call.Name,
		"extension_id", definition.ExtensionID,
		"extension_generation", definition.ExtensionGeneration,
	)
	result := runtime.executeTool(ctx, toolSet, call)
	runtime.debug("tool.execution.completed",
		"run_id", runID,
		"tool", call.Name,
		"extension_id", definition.ExtensionID,
		"extension_generation", definition.ExtensionGeneration,
		"duration_ms", time.Since(startedAt).Milliseconds(),
		"is_error", result.IsError,
	)
	return result
}

// RunHandle provides events, cancellation, and the terminal result of a Run
type RunHandle struct {
	events chan Event
	done   chan struct{}
	cancel context.CancelFunc

	mu     sync.Mutex
	result RunResult
	err    error
	waiter *waitBroker
}

// Events returns the ordered event stream for the Run
//
// The channel closes after the terminal event has been emitted
func (handle *RunHandle) Events() <-chan Event {
	return handle.events
}

// Wait blocks until the Run reaches a terminal state
//
// Wait is safe to call more than once
func (handle *RunHandle) Wait() (RunResult, error) {
	<-handle.done

	handle.mu.Lock()
	defer handle.mu.Unlock()

	return cloneRunResult(handle.result), handle.err
}

// Cancel requests cancellation of the Run
func (handle *RunHandle) Cancel() {
	handle.cancel()
}

// Resume supplies external input to the Run's current waiting request
func (handle *RunHandle) Resume(response WaitResponse) error {
	handle.mu.Lock()
	waiter := handle.waiter
	handle.mu.Unlock()
	if waiter == nil {
		return ErrRunNotWaiting
	}
	return waiter.resume(response)
}

// PendingWait returns an isolated copy of the current waiting request
func (handle *RunHandle) PendingWait() (WaitRequest, bool) {
	handle.mu.Lock()
	waiter := handle.waiter
	handle.mu.Unlock()
	if waiter == nil {
		return WaitRequest{}, false
	}
	return waiter.pendingRequest()
}

func (handle *RunHandle) setWaiter(waiter *waitBroker) {
	handle.mu.Lock()
	handle.waiter = waiter
	handle.mu.Unlock()
}

func (handle *RunHandle) complete(result RunResult, err error) {
	handle.mu.Lock()
	handle.result = cloneRunResult(result)
	handle.err = err
	handle.mu.Unlock()

	close(handle.events)
	close(handle.done)
}

func newRunID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "run_" + hex.EncodeToString(value[:]), nil
}

func cloneMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return nil
	}

	cloned := make(map[string]string, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func cloneRawMessage(value json.RawMessage) json.RawMessage {
	if value == nil {
		return nil
	}
	return append(json.RawMessage(nil), value...)
}

func cloneToolCall(call ToolCall) ToolCall {
	call.Arguments = cloneRawMessage(call.Arguments)
	return call
}

func cloneToolCalls(calls []ToolCall) []ToolCall {
	if calls == nil {
		return nil
	}

	cloned := make([]ToolCall, len(calls))
	for index := range calls {
		cloned[index] = cloneToolCall(calls[index])
	}
	return cloned
}

func cloneMessage(message Message) Message {
	message.ToolCalls = cloneToolCalls(message.ToolCalls)
	if message.Usage != nil {
		usage := *message.Usage
		message.Usage = &usage
	}
	if message.ProviderState != nil {
		state := *message.ProviderState
		state.Data = cloneRawMessage(state.Data)
		message.ProviderState = &state
	}
	return message
}

func cloneMessages(messages []Message) []Message {
	if messages == nil {
		return nil
	}

	cloned := make([]Message, len(messages))
	for index := range messages {
		cloned[index] = cloneMessage(messages[index])
	}
	return cloned
}

func cloneToolDefinition(definition ToolDefinition) ToolDefinition {
	definition.InputSchema = cloneRawMessage(definition.InputSchema)
	definition.Capabilities = append([]string(nil), definition.Capabilities...)
	return definition
}

func cloneToolDefinitions(definitions []ToolDefinition) []ToolDefinition {
	if definitions == nil {
		return nil
	}

	cloned := make([]ToolDefinition, len(definitions))
	for index := range definitions {
		cloned[index] = cloneToolDefinition(definitions[index])
	}
	return cloned
}

func cloneToolResults(results []ToolResult) []ToolResult {
	if results == nil {
		return nil
	}
	return append([]ToolResult(nil), results...)
}

func cloneEvent(event Event) Event {
	if event.Message != nil {
		message := cloneMessage(*event.Message)
		event.Message = &message
	}
	if event.ToolCall != nil {
		call := cloneToolCall(*event.ToolCall)
		event.ToolCall = &call
	}
	if event.ToolResult != nil {
		result := *event.ToolResult
		event.ToolResult = &result
	}
	if event.WaitRequest != nil {
		request := cloneWaitRequest(*event.WaitRequest)
		event.WaitRequest = &request
	}
	if event.WaitResponse != nil {
		response := *event.WaitResponse
		response.Payload = cloneRawMessage(response.Payload)
		event.WaitResponse = &response
	}
	return event
}

func cloneRunResult(result RunResult) RunResult {
	result.Messages = cloneMessages(result.Messages)
	result.ToolResults = cloneToolResults(result.ToolResults)
	if result.Budget != nil {
		budget := *result.Budget
		result.Budget = &budget
	}
	return result
}
