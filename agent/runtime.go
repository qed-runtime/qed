package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	providerbase "github.com/qed-runtime/qed/provider"
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
	// Source Tools are combined with fixed Tools before the Context Compiler
	// canonicalizes their order for Provider calls
	ToolSource ToolSource
	// ComponentSource supplies one atomic Tool and Hook generation set for a Run
	//
	// ComponentSource and ToolSource are mutually exclusive
	ComponentSource ComponentSource
	// ToolInputValidator compiles and validates Tool input schemas
	//
	// A nil Validator uses JSONSchemaSubsetValidator
	ToolInputValidator ToolInputValidator
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
	// ContextCompiler prepares each Provider call and produces Context Segments
	//
	// A nil Compiler uses DefaultContextCompiler
	ContextCompiler ContextCompiler
	// CurrentWorldStateSource reads canonical host state before each logical Provider request
	//
	// A nil Source disables Current World State capture
	CurrentWorldStateSource CurrentWorldStateSource
	// CachePlanner creates Provider-neutral cache routing and breakpoint decisions
	//
	// A nil Planner uses DefaultCachePlanner
	CachePlanner CachePlanner
	// CachePolicy supplies host cache intent, isolation, and optional pricing
	CachePolicy CachePolicy
	// ProviderRetry controls bounded retries before Provider output becomes observable
	ProviderRetry ProviderRetryPolicy
	// ProviderRateLimiter bounds active streams and coordinates rate-limit cooldowns
	//
	// Share one limiter between Runtimes backed by the same Provider rate-limit pool
	ProviderRateLimiter ProviderRateLimitController
	// Logger receives safe structured debug diagnostics without message content,
	// Tool arguments, Tool output, metadata values, or Provider-private state
	Logger *slog.Logger
}

// Runtime executes Agent Runs with one Provider and a Run-scoped Tool registry
//
// Runtime is safe for concurrent use after construction
type Runtime struct {
	provider                Provider
	staticTools             runtimeToolSet
	toolValidator           ToolInputValidator
	toolSource              ToolSource
	componentSource         ComponentSource
	staticHooks             []runtimeHook
	maxProviderCalls        int
	maxToolCalls            int
	sessionStore            SessionStore
	contextCompiler         ContextCompiler
	currentWorldStateSource CurrentWorldStateSource
	cachePlanner            CachePlanner
	cachePolicy             CachePolicy
	providerRetry           ProviderRetryPolicy
	providerLimiter         ProviderRateLimitController
	logger                  *slog.Logger
	sessionMu               sync.Mutex
	sessionLocks            map[string]*runtimeSessionLock
}

type runtimeSessionLock struct {
	token chan struct{}
	refs  int
}

type runtimeToolSet struct {
	tools       map[string]Tool
	validators  map[string]CompiledToolInputValidator
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

	staticTools, err := newRuntimeToolSet(options.Tools, options.ToolInputValidator)
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
	contextCompiler := options.ContextCompiler
	if contextCompiler == nil {
		contextCompiler = DefaultContextCompiler{}
	}
	cachePlanner := options.CachePlanner
	if cachePlanner == nil {
		cachePlanner = DefaultCachePlanner{}
	}
	cachePolicy, err := normalizeCachePolicy(options.CachePolicy)
	if err != nil {
		return nil, fmt.Errorf("configure Cache Policy: %w", err)
	}
	providerRetry, err := normalizeProviderRetryPolicy(options.ProviderRetry)
	if err != nil {
		return nil, fmt.Errorf("configure Provider retry: %w", err)
	}
	providerLimiter := options.ProviderRateLimiter
	if providerLimiter == nil {
		providerLimiter = &ProviderRateLimiter{}
	}
	providerMaxConcurrency := providerLimiter.MaxConcurrency()
	if providerMaxConcurrency <= 0 || providerMaxConcurrency > maximumProviderConcurrency {
		return nil, fmt.Errorf(
			"Provider rate limiter max concurrency must be between 1 and %d",
			maximumProviderConcurrency,
		)
	}

	return &Runtime{
		provider:                options.Provider,
		staticTools:             staticTools,
		toolValidator:           options.ToolInputValidator,
		toolSource:              options.ToolSource,
		componentSource:         options.ComponentSource,
		staticHooks:             staticHooks,
		maxProviderCalls:        maxProviderCalls,
		maxToolCalls:            maxToolCalls,
		sessionStore:            options.SessionStore,
		contextCompiler:         contextCompiler,
		currentWorldStateSource: options.CurrentWorldStateSource,
		cachePlanner:            cachePlanner,
		cachePolicy:             cachePolicy,
		providerRetry:           providerRetry,
		providerLimiter:         providerLimiter,
		logger:                  options.Logger,
		sessionLocks:            make(map[string]*runtimeSessionLock),
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

func newRuntimeToolSet(configured []Tool, validator ToolInputValidator) (runtimeToolSet, error) {
	tools := make(map[string]Tool, len(configured))
	validators := make(map[string]CompiledToolInputValidator, len(configured))
	definitions := make([]ToolDefinition, 0, len(configured))
	for _, tool := range configured {
		if tool == nil {
			return runtimeToolSet{}, errors.New("tool must not be nil")
		}

		definition := cloneToolDefinition(tool.Definition())
		if strings.TrimSpace(definition.Name) != definition.Name || definition.Name == "" {
			return runtimeToolSet{}, errors.New("tool name is required")
		}
		compiled, err := CompileToolInputSchema(validator, definition.InputSchema)
		if err != nil {
			return runtimeToolSet{}, fmt.Errorf("tool %q input schema: %w", definition.Name, err)
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
		validators[definition.Name] = compiled
		definitions = append(definitions, definition)
	}
	return runtimeToolSet{tools: tools, validators: validators, definitions: definitions}, nil
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
	for index := range request.Input {
		if err := validateFactDirectiveMessage(request.Input[index]); err != nil {
			return nil, fmt.Errorf("run input Message %d: %w", index, err)
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
		events:       make(chan Event, runtime.eventBufferSize()),
		done:         make(chan struct{}),
		cancel:       cancel,
		steeringOpen: true,
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
	toolSet, err := newRuntimeToolSet(combined, runtime.toolValidator)
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
	ledgerEvents := cloneEvents(loaded.events)
	activeCheckpoint := cloneContextCheckpointPointer(loaded.checkpoint)
	knownEvidence := make(map[string]struct{}, len(loaded.evidenceObjects))
	for _, reference := range loaded.evidenceObjects {
		knownEvidence[reference.Digest] = struct{}{}
	}
	var latestCompaction *ContextCompactionReport
	var latestCachePlan *CachePlan
	var latestWorldState *CurrentWorldState

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
	inputTokenDetailsComplete := true

	emit := func(event Event) error {
		event.Sequence = sequence + 1
		event.RunID = runID
		event.ParentRunID = request.ParentRunID
		event.AgentID = request.AgentID
		event.SessionID = request.SessionID
		event.Time = time.Now().UTC()
		if event.FactDirective != nil {
			preview := cloneEvent(event)
			if runtime.sessionStore != nil && request.SessionID != "" {
				preview.SessionRevision = sessionRevision + 1
			}
			previewEvents := append(cloneEvents(ledgerEvents), preview)
			if _, err := BuildContextLedger(ctx, previewEvents); err != nil {
				return fmt.Errorf("validate Fact lifecycle transition: %w", err)
			}
		}
		if event.Type == EventCurrentWorldStateCaptured && event.CurrentWorldState == nil {
			return errors.New("current_world_state.captured requires Current World State")
		}
		if event.Type != EventCurrentWorldStateCaptured && event.CurrentWorldState != nil {
			return fmt.Errorf("Event %q must not contain Current World State", event.Type)
		}
		if event.CurrentWorldState != nil {
			prefix, err := BuildContextLedger(ctx, ledgerEvents)
			if err != nil {
				return fmt.Errorf("build Current World State prefix: %w", err)
			}
			if err := validateCurrentWorldStateAgainstPrefix(*event.CurrentWorldState, prefix, ledgerEvents); err != nil {
				return fmt.Errorf("validate Current World State: %w", err)
			}
		}
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
		ledgerEvents = append(ledgerEvents, cloneEvent(event))
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
	applySteering := func(pending []Message) error {
		for index := range pending {
			if err := ctx.Err(); err != nil {
				return err
			}
			message := cloneMessage(pending[index])
			directive := cloneFactLifecycleDirective(message.FactDirective)
			message.FactDirective = nil
			if err := emit(Event{
				Type:              EventUserMessageAdded,
				Message:           &message,
				UserMessageOrigin: UserMessageOriginSteering,
				FactDirective:     directive,
			}); err != nil {
				return err
			}
			messages = append(messages, message)
		}
		if len(pending) != 0 {
			runtime.debug("run.steering.applied",
				"run_id", runID,
				"message_count", len(pending),
			)
		}
		return nil
	}

	finish := func(status RunStatus, eventType EventType, runErr error) {
		handle.discardSteering()
		event := Event{Type: eventType}
		if runErr != nil {
			event.Error = runErr.Error()
			var providerError *runtimeProviderError
			if errors.As(runErr, &providerError) {
				info := providerError.eventInfo()
				event.ProviderError = &info
			}
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
		finalLedger, ledgerErr := BuildContextLedger(context.WithoutCancel(ctx), ledgerEvents)
		var terminalLedger *ContextLedger
		if ledgerErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("build terminal Context Ledger: %w", ledgerErr))
			if status == RunStatusCompleted {
				status = RunStatusFailed
			}
		} else {
			terminalLedger = &finalLedger
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
			RunID:             runID,
			ParentRunID:       request.ParentRunID,
			AgentID:           request.AgentID,
			SessionID:         request.SessionID,
			Status:            status,
			Messages:          messages,
			ToolResults:       toolResults,
			ProviderCalls:     providerCalls,
			ToolCalls:         toolCalls,
			Usage:             usage,
			Budget:            budgetSnapshot,
			SessionRevision:   sessionRevision,
			ContextCheckpoint: cloneContextCheckpointPointer(activeCheckpoint),
			ContextLedger:     cloneContextLedgerPointer(terminalLedger),
			CurrentWorldState: cloneCurrentWorldStatePointer(latestWorldState),
			ContextCompaction: cloneContextCompactionReport(latestCompaction),
			CachePlan:         cloneCachePlanPointer(latestCachePlan),
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
		directive := cloneFactLifecycleDirective(input.FactDirective)
		input.FactDirective = nil
		if err := emit(Event{
			Type:              EventUserMessageAdded,
			Message:           &input,
			UserMessageOrigin: UserMessageOriginRunInput,
			FactDirective:     directive,
		}); err != nil {
			fail(err)
			return
		}
		messages = append(messages, input)
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
		pendingSteering, canceled := handle.takeSteering()
		if canceled {
			fail(context.Canceled)
			return
		}
		if err := ctx.Err(); err != nil {
			fail(err)
			return
		}
		if err := applySteering(pendingSteering); err != nil {
			fail(fmt.Errorf("apply steering input: %w", err))
			return
		}

		modelRequest := ModelRequest{
			AgentID:      request.AgentID,
			SessionID:    request.SessionID,
			Metadata:     cloneMetadata(request.Metadata),
			Instructions: request.Instructions,
			Messages:     cloneMessages(messages),
			Tools:        cloneToolDefinitions(toolSet.definitions),
		}
		compileStartedAt := time.Now()
		activeLedger, err := BuildContextLedger(ctx, ledgerEvents)
		if err != nil {
			fail(fmt.Errorf("build Context Ledger: %w", err))
			return
		}
		if runtime.currentWorldStateSource != nil {
			captureStartedAt := time.Now()
			runtime.debug("current_world_state.capture.started",
				"run_id", runID,
				"provider", runtime.provider.Name(),
			)
			snapshot, snapshotErr := runtime.currentWorldStateSource.Snapshot(ctx, CurrentWorldStateRequest{
				Run: RunInfo{
					RunID: runID, ParentRunID: request.ParentRunID, AgentID: request.AgentID,
					SessionID: request.SessionID, Capabilities: append([]string(nil), request.Capabilities...),
				},
				Events: cloneEvents(ledgerEvents),
				Ledger: *cloneContextLedgerPointer(&activeLedger),
			})
			if snapshotErr != nil {
				runtime.debug("current_world_state.capture.failed",
					"run_id", runID,
					"provider", runtime.provider.Name(),
					"duration_ms", time.Since(captureStartedAt).Milliseconds(),
					"error_type", fmt.Sprintf("%T", snapshotErr),
				)
				fail(fmt.Errorf("capture Current World State: %w", snapshotErr))
				return
			}
			worldState, stateErr := buildCurrentWorldState(activeLedger.Reference(), snapshot, ledgerEvents)
			if stateErr != nil {
				runtime.debug("current_world_state.capture.failed",
					"run_id", runID,
					"provider", runtime.provider.Name(),
					"duration_ms", time.Since(captureStartedAt).Milliseconds(),
					"error_type", fmt.Sprintf("%T", stateErr),
				)
				fail(fmt.Errorf("build Current World State: %w", stateErr))
				return
			}
			if err := emit(Event{Type: EventCurrentWorldStateCaptured, CurrentWorldState: &worldState}); err != nil {
				fail(err)
				return
			}
			latestWorldState = cloneCurrentWorldStatePointer(&worldState)
			activeLedger, err = BuildContextLedger(ctx, ledgerEvents)
			if err != nil {
				fail(fmt.Errorf("rebuild Context Ledger after Current World State: %w", err))
				return
			}
			gitChanges := 0
			gitAvailable := false
			if worldState.Snapshot.Git != nil {
				gitAvailable = worldState.Snapshot.Git.Available
				gitChanges = len(worldState.Snapshot.Git.Changes)
			}
			runtime.debug("current_world_state.capture.completed",
				"run_id", runID,
				"provider", runtime.provider.Name(),
				"duration_ms", time.Since(captureStartedAt).Milliseconds(),
				"files_available", worldState.Snapshot.FilesAvailable,
				"file_count", len(worldState.Snapshot.Files),
				"files_truncated", worldState.Snapshot.FilesTruncated,
				"git_available", gitAvailable,
				"git_change_count", gitChanges,
				"check_count", len(worldState.Snapshot.Checks),
				"checks_truncated", worldState.Snapshot.ChecksTruncated,
			)
		}
		compiled, manifest, err := runtime.compileContext(
			ctx,
			runID,
			modelRequest,
			sessionRevision,
			activeCheckpoint,
			&activeLedger,
			ledgerEvents,
			latestWorldState,
		)
		if err != nil {
			runtime.debug("context.compile.failed",
				"run_id", runID,
				"provider", runtime.provider.Name(),
				"duration_ms", time.Since(compileStartedAt).Milliseconds(),
				"error_type", fmt.Sprintf("%T", err),
			)
			fail(fmt.Errorf("compile Provider context: %w", err))
			return
		}
		runtime.debug("context.compile.completed",
			"run_id", runID,
			"provider", runtime.provider.Name(),
			"duration_ms", time.Since(compileStartedAt).Milliseconds(),
			"segment_count", len(manifest.Segments),
			"prefix_epoch", manifest.Epoch,
		)
		checkpointChanged := compiled.Checkpoint != nil && !contextCheckpointsEqual(activeCheckpoint, compiled.Checkpoint)
		var newEvidence []EvidenceObjectRef
		if compiled.Compaction != nil {
			for _, reference := range compiled.Compaction.Externalized {
				if _, exists := knownEvidence[reference.Digest]; exists {
					continue
				}
				newEvidence = append(newEvidence, reference)
			}
		}
		if checkpointChanged || len(newEvidence) > 0 {
			report := cloneContextCompactionReport(compiled.Compaction)
			if report == nil {
				report = &ContextCompactionReport{Applied: true, Reason: "checkpoint"}
			}
			report.Externalized = append([]EvidenceObjectRef(nil), newEvidence...)
			event := Event{Type: EventContextCompacted, ContextCompaction: report}
			if checkpointChanged {
				event.ContextCheckpoint = cloneContextCheckpointPointer(compiled.Checkpoint)
			}
			if err := emit(event); err != nil {
				fail(err)
				return
			}
			if checkpointChanged {
				activeCheckpoint = cloneContextCheckpointPointer(compiled.Checkpoint)
			}
			for _, reference := range newEvidence {
				knownEvidence[reference.Digest] = struct{}{}
			}
			latestCompaction = cloneContextCompactionReport(report)
		}
		if compiled.Compaction != nil {
			latestCompaction = cloneContextCompactionReport(compiled.Compaction)
		}

		var message Message
		providerAttempt := 0
		for {
			if providerCalls >= runtime.maxProviderCalls {
				fail(ErrProviderCallLimit)
				return
			}
			releaseProvider, waitDuration, err := runtime.providerLimiter.Acquire(
				ctx,
				func(wait ProviderRateLimitWaitInfo) error {
					if err := validateProviderRateLimitWait(
						wait,
						runtime.providerLimiter.MaxConcurrency(),
					); err != nil {
						return err
					}
					runtime.debug("provider.rate_limit.waiting",
						"run_id", runID,
						"provider", runtime.provider.Name(),
						"attempt", providerAttempt+1,
						"reason", wait.Reason,
						"max_concurrency", wait.MaxConcurrency,
						"retry_after_ms", wait.RetryAfterMilliseconds,
					)
					return emit(Event{
						Type:                  EventProviderRateLimitWait,
						ProviderAttempt:       providerAttempt + 1,
						ProviderRateLimitWait: &wait,
					})
				},
			)
			if err != nil {
				fail(err)
				return
			}
			if releaseProvider == nil {
				fail(errors.New("Provider rate limiter returned a nil release function"))
				return
			}
			if err := ctx.Err(); err != nil {
				releaseProvider()
				fail(err)
				return
			}
			if waitDuration > 0 {
				runtime.debug("provider.rate_limit.acquired",
					"run_id", runID,
					"provider", runtime.provider.Name(),
					"attempt", providerAttempt+1,
					"wait_ms", waitDuration.Milliseconds(),
				)
			}
			if err := request.Budget.consumeProviderCall(); err != nil {
				releaseProvider()
				fail(err)
				return
			}

			providerCalls++
			providerAttempt++
			providerStartedAt := time.Now()
			runtime.debug("provider.stream.started",
				"run_id", runID,
				"provider", runtime.provider.Name(),
				"call", providerCalls,
				"attempt", providerAttempt,
			)
			manifestEvent := clonePrefixManifest(manifest)
			cachePlanEvent := cloneCachePlanPointer(compiled.ModelRequest.CachePlan)
			if err := emit(Event{
				Type:            EventModelRequest,
				PrefixManifest:  &manifestEvent,
				CachePlan:       cachePlanEvent,
				ProviderCall:    providerCalls,
				ProviderAttempt: providerAttempt,
			}); err != nil {
				releaseProvider()
				fail(err)
				return
			}
			latestCachePlan = cloneCachePlanPointer(cachePlanEvent)

			phase := "failed"
			stream, providerErr := runtime.provider.Stream(ctx, compiled.ModelRequest)
			outputObserved := false
			providerFailure := true
			if providerErr == nil {
				phase = "stream failed"
				message, outputObserved, providerFailure, providerErr = consumeModelStream(
					ctx,
					stream,
					func() error { return emit(Event{Type: EventMessageStarted}) },
					func(delta string) error { return emit(Event{Type: EventMessageDelta, Delta: delta}) },
				)
			}
			var errorInfo providerbase.ErrorInfo
			var retryDelay time.Duration
			if providerErr != nil && providerFailure {
				errorInfo = providerbase.ClassifyError(providerErr)
				retryDelay = providerRetryDelayWithJitter(
					runtime.providerRetry,
					providerAttempt,
					errorInfo.RetryAfter,
					runID,
				)
				if errorInfo.Code == providerbase.ErrorCodeRateLimited {
					runtime.providerLimiter.ObserveRateLimit(retryDelay)
					runtime.debug("provider.rate_limit.updated",
						"run_id", runID,
						"provider", runtime.provider.Name(),
						"attempt", providerAttempt,
						"cooldown_ms", retryDelay.Milliseconds(),
					)
				}
			}
			releaseProvider()
			if providerErr == nil {
				runtime.debug("provider.stream.completed",
					"run_id", runID,
					"provider", runtime.provider.Name(),
					"call", providerCalls,
					"attempt", providerAttempt,
					"duration_ms", time.Since(providerStartedAt).Milliseconds(),
					"tool_call_count", len(message.ToolCalls),
				)
				break
			}
			if !providerFailure {
				fail(providerErr)
				return
			}

			classifiedError := &runtimeProviderError{
				providerName: runtime.provider.Name(),
				phase:        phase,
				info:         errorInfo,
				attempt:      providerAttempt,
				err:          providerErr,
			}
			runtime.debug("provider.stream.failed",
				"run_id", runID,
				"provider", runtime.provider.Name(),
				"call", providerCalls,
				"attempt", providerAttempt,
				"duration_ms", time.Since(providerStartedAt).Milliseconds(),
				"error_type", fmt.Sprintf("%T", providerErr),
				"error_code", errorInfo.Code,
				"output_observed", outputObserved,
			)
			if !errorInfo.Retryable() || outputObserved || providerAttempt >= runtime.providerRetry.MaxAttempts {
				fail(classifiedError)
				return
			}
			if providerCalls >= runtime.maxProviderCalls {
				fail(errors.Join(ErrProviderCallLimit, classifiedError))
				return
			}

			retry := ProviderRetryInfo{
				Error:             classifiedError.eventInfo(),
				NextAttempt:       providerAttempt + 1,
				DelayMilliseconds: retryDelay.Milliseconds(),
			}
			if err := emit(Event{
				Type:            EventProviderRetry,
				ProviderCall:    providerCalls,
				ProviderAttempt: providerAttempt,
				ProviderRetry:   &retry,
			}); err != nil {
				fail(err)
				return
			}
			runtime.debug("provider.retry.scheduled",
				"run_id", runID,
				"provider", runtime.provider.Name(),
				"call", providerCalls,
				"attempt", providerAttempt,
				"next_attempt", providerAttempt+1,
				"delay_ms", retryDelay.Milliseconds(),
				"error_code", errorInfo.Code,
			)
			if err := waitForProviderRetry(ctx, retryDelay); err != nil {
				fail(err)
				return
			}
		}
		if message.Role != RoleAssistant {
			fail(fmt.Errorf("provider %q returned message role %q, want %q", runtime.provider.Name(), message.Role, RoleAssistant))
			return
		}
		if message.FactDirective != nil {
			fail(fmt.Errorf("provider %q returned host-only Fact lifecycle state", runtime.provider.Name()))
			return
		}
		if err := validateUsage(message.Usage); err != nil {
			fail(fmt.Errorf("provider %q returned invalid Usage: %w", runtime.provider.Name(), err))
			return
		}
		if err := accumulateUsage(&usage, message.Usage, &inputTokenDetailsComplete); err != nil {
			fail(fmt.Errorf("aggregate provider %q Usage: %w", runtime.provider.Name(), err))
			return
		}
		if message.Usage != nil {
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
			if err := ctx.Err(); err != nil {
				fail(err)
				return
			}
			pendingSteering, boundary := handle.resolveEndTurn()
			switch boundary {
			case steeringBoundaryCanceled:
				fail(context.Canceled)
				return
			case steeringBoundaryComplete:
				finish(RunStatusCompleted, EventRunCompleted, nil)
				return
			case steeringBoundaryContinue:
				if err := ctx.Err(); err != nil {
					fail(err)
					return
				}
				if err := applySteering(pendingSteering); err != nil {
					fail(fmt.Errorf("apply steering input: %w", err))
					return
				}
				continue
			default:
				fail(errors.New("invalid steering boundary"))
				return
			}
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

func (runtime *Runtime) compileContext(
	ctx context.Context,
	runID string,
	request ModelRequest,
	sessionRevision uint64,
	checkpoint *ContextCheckpoint,
	ledger *ContextLedger,
	events []Event,
	worldState *CurrentWorldState,
) (CompiledContext, PrefixManifest, error) {
	model := ""
	if provider, ok := runtime.provider.(ModelIDProvider); ok {
		model = provider.ModelID()
	}
	compiled, err := runtime.contextCompiler.Compile(ctx, ContextCompileRequest{
		Provider:          runtime.provider.Name(),
		Model:             model,
		ModelRequest:      cloneModelRequest(request),
		SessionRevision:   sessionRevision,
		Checkpoint:        cloneContextCheckpointPointer(checkpoint),
		Ledger:            cloneContextLedgerPointer(ledger),
		Events:            cloneEvents(events),
		CurrentWorldState: cloneCurrentWorldStatePointer(worldState),
	})
	if err != nil {
		return CompiledContext{}, PrefixManifest{}, err
	}
	compiled.ModelRequest = cloneModelRequest(compiled.ModelRequest)
	compiled.Segments = cloneContextSegments(compiled.Segments)
	compiled.Checkpoint = cloneContextCheckpointPointer(compiled.Checkpoint)
	compiled.Compaction = cloneContextCompactionReport(compiled.Compaction)
	if compiled.ModelRequest.AgentID != request.AgentID {
		return CompiledContext{}, PrefixManifest{}, errors.New("Context Compiler changed Agent ID")
	}
	if compiled.ModelRequest.SessionID != request.SessionID {
		return CompiledContext{}, PrefixManifest{}, errors.New("Context Compiler changed Session ID")
	}
	if !metadataEqual(compiled.ModelRequest.Metadata, request.Metadata) {
		return CompiledContext{}, PrefixManifest{}, errors.New("Context Compiler changed request metadata")
	}
	for index := range compiled.ModelRequest.Messages {
		if compiled.ModelRequest.Messages[index].FactDirective != nil {
			return CompiledContext{}, PrefixManifest{}, fmt.Errorf(
				"Context Compiler returned host-only Fact lifecycle state in Message %d",
				index,
			)
		}
	}
	if len(compiled.ModelRequest.Messages) == 0 && compiled.ModelRequest.Instructions == "" {
		return CompiledContext{}, PrefixManifest{}, errors.New("Context Compiler returned an empty model request")
	}
	if err := appendCurrentWorldState(&compiled, worldState); err != nil {
		return CompiledContext{}, PrefixManifest{}, err
	}
	toolNames := make(map[string]struct{}, len(compiled.ModelRequest.Tools))
	for _, definition := range compiled.ModelRequest.Tools {
		if strings.TrimSpace(definition.Name) != definition.Name || definition.Name == "" {
			return CompiledContext{}, PrefixManifest{}, errors.New("Context Compiler returned a Tool without a valid name")
		}
		if _, duplicate := toolNames[definition.Name]; duplicate {
			return CompiledContext{}, PrefixManifest{}, fmt.Errorf("Context Compiler returned duplicate Tool %q", definition.Name)
		}
		toolNames[definition.Name] = struct{}{}
		if len(definition.InputSchema) > 0 && !json.Valid(definition.InputSchema) {
			return CompiledContext{}, PrefixManifest{}, fmt.Errorf("Context Compiler returned Tool %q with an invalid input schema", definition.Name)
		}
	}
	capabilities := CacheCapabilities{}
	if provider, ok := runtime.provider.(CacheCapabilityProvider); ok {
		capabilities = provider.CacheCapabilities()
	}
	plan, err := runtime.cachePlanner.Plan(ctx, CachePlanRequest{
		RunID:        runID,
		Provider:     runtime.provider.Name(),
		Model:        model,
		ModelRequest: cloneModelRequest(compiled.ModelRequest),
		Segments:     cloneContextSegments(compiled.Segments),
		Capabilities: capabilities,
		Policy:       runtime.cachePolicy,
	})
	if err != nil {
		return CompiledContext{}, PrefixManifest{}, fmt.Errorf("plan Provider cache: %w", err)
	}
	if err := validateCachePlan(plan, capabilities, compiled.ModelRequest, compiled.Segments); err != nil {
		return CompiledContext{}, PrefixManifest{}, fmt.Errorf("validate Provider Cache Plan: %w", err)
	}
	compiled.ModelRequest.CachePlan = cloneCachePlanPointer(&plan)
	manifest, err := BuildPrefixManifest(PrefixManifestOptions{
		Provider:    runtime.provider.Name(),
		Model:       model,
		CacheFamily: plan.FamilyID,
	}, compiled.Segments)
	if err != nil {
		return CompiledContext{}, PrefixManifest{}, err
	}
	return compiled, manifest, nil
}

func metadataEqual(first, second map[string]string) bool {
	if len(first) != len(second) {
		return false
	}
	for key, value := range first {
		if second[key] != value {
			return false
		}
	}
	return true
}

func validateUsage(usage *Usage) error {
	if usage == nil {
		return nil
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.TotalTokens < 0 || usage.CostMicros < 0 ||
		usage.UncachedInputTokens < 0 || usage.CacheReadInputTokens < 0 || usage.CacheWriteInputTokens < 0 {
		return errors.New("token and cost values must not be negative")
	}
	classified, err := addUsageValue(usage.UncachedInputTokens, usage.CacheReadInputTokens)
	if err != nil {
		return err
	}
	classified, err = addUsageValue(classified, usage.CacheWriteInputTokens)
	if err != nil {
		return err
	}
	if usage.InputTokenDetailsReported {
		if classified != usage.InputTokens {
			return fmt.Errorf("input token categories total %d, want %d", classified, usage.InputTokens)
		}
		return nil
	}
	if classified != 0 {
		return errors.New("input token categories require InputTokenDetailsReported")
	}
	return nil
}

func accumulateUsage(total *Usage, next *Usage, inputDetailsComplete *bool) error {
	if next == nil {
		*inputDetailsComplete = false
		total.InputTokenDetailsReported = false
		total.UncachedInputTokens = 0
		total.CacheReadInputTokens = 0
		total.CacheWriteInputTokens = 0
		return nil
	}
	candidate := *total
	fields := []struct {
		name   string
		stored *int64
		value  int64
	}{
		{name: "input tokens", stored: &candidate.InputTokens, value: next.InputTokens},
		{name: "output tokens", stored: &candidate.OutputTokens, value: next.OutputTokens},
		{name: "total tokens", stored: &candidate.TotalTokens, value: next.TotalTokens},
		{name: "cost", stored: &candidate.CostMicros, value: next.CostMicros},
	}
	for _, field := range fields {
		value, err := addUsageValue(*field.stored, field.value)
		if err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
		*field.stored = value
	}
	if *inputDetailsComplete && next.InputTokenDetailsReported {
		candidate.InputTokenDetailsReported = true
		detailFields := []struct {
			name   string
			stored *int64
			value  int64
		}{
			{name: "uncached input tokens", stored: &candidate.UncachedInputTokens, value: next.UncachedInputTokens},
			{name: "cache read input tokens", stored: &candidate.CacheReadInputTokens, value: next.CacheReadInputTokens},
			{name: "cache write input tokens", stored: &candidate.CacheWriteInputTokens, value: next.CacheWriteInputTokens},
		}
		for _, field := range detailFields {
			value, err := addUsageValue(*field.stored, field.value)
			if err != nil {
				return fmt.Errorf("%s: %w", field.name, err)
			}
			*field.stored = value
		}
	} else {
		*inputDetailsComplete = false
		candidate.InputTokenDetailsReported = false
		candidate.UncachedInputTokens = 0
		candidate.CacheReadInputTokens = 0
		candidate.CacheWriteInputTokens = 0
	}
	*total = candidate
	return nil
}

func addUsageValue(first, second int64) (int64, error) {
	if first < 0 || second < 0 {
		return 0, errors.New("usage values must not be negative")
	}
	if second > math.MaxInt64-first {
		return 0, errors.New("usage value overflow")
	}
	return first + second, nil
}

func errorType(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}

type loadedSession struct {
	messages        []Message
	events          []Event
	revision        uint64
	checkpoint      *ContextCheckpoint
	evidenceObjects []EvidenceObjectRef
	pendingWait     *WaitRequest
	pendingTool     *ToolCall
}

func (runtime *Runtime) loadSession(ctx context.Context, request RunRequest) (loadedSession, func(), error) {
	if runtime.sessionStore == nil || request.SessionID == "" {
		return loadedSession{}, func() {}, nil
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
			messages:        cloneMessagesWithoutFactDirectives(snapshot.Messages),
			events:          cloneEvents(snapshot.Events),
			revision:        snapshot.Revision,
			checkpoint:      cloneContextCheckpointPointer(snapshot.Checkpoint),
			evidenceObjects: append([]EvidenceObjectRef(nil), snapshot.EvidenceObjects...),
			pendingWait:     &wait,
			pendingTool:     &tool,
		}, release, nil
	}
	if snapshot.PendingWait != nil || snapshot.PendingTool != nil {
		release()
		return loadedSession{}, func() {}, fmt.Errorf("Session %q has unfinished work and must be resumed", request.SessionID)
	}
	return loadedSession{
		messages:        cloneMessagesWithoutFactDirectives(snapshot.Messages),
		events:          cloneEvents(snapshot.Events),
		revision:        snapshot.Revision,
		checkpoint:      cloneContextCheckpointPointer(snapshot.Checkpoint),
		evidenceObjects: append([]EvidenceObjectRef(nil), snapshot.EvidenceObjects...),
	}, release, nil
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
	if err := ValidateToolInput(toolSet.validators[call.Name], arguments); err != nil {
		result.Output = fmt.Sprintf("tool %q input validation: %v", call.Name, err)
		result.IsError = true
		return result
	}

	call.Arguments = arguments
	toolResult, err := tool.Execute(ctx, call)
	result.Policy = cloneToolPolicyDecision(toolResult.Policy)
	if err != nil {
		result.Output = err.Error()
		result.IsError = true
		return result
	}
	if err := ValidateContextOperation(toolResult.ContextOperation); err != nil {
		result.Output = fmt.Sprintf("tool %q returned invalid context operation: %v", call.Name, err)
		result.IsError = true
		return result
	}

	result.Output = toolResult.Output
	result.IsError = toolResult.IsError
	result.ContextOperation = cloneContextOperation(toolResult.ContextOperation)
	return result
}

func (runtime *Runtime) executeToolWithDebug(
	ctx context.Context,
	runID string,
	toolSet runtimeToolSet,
	call ToolCall,
) ToolResult {
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

// RunHandle provides Events, steering, wait resumption, cancellation, and the
// terminal result of a Run
type RunHandle struct {
	events chan Event
	done   chan struct{}
	cancel context.CancelFunc

	mu              sync.Mutex
	result          RunResult
	err             error
	waiter          *waitBroker
	steering        []Message
	steeringOpen    bool
	cancelRequested bool
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

// Cancel requests cancellation while the Run is active
//
// It has no effect after the terminal transition has begun
func (handle *RunHandle) Cancel() {
	if handle != nil && handle.requestCancel() {
		handle.cancel()
	}
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
	handle.steeringOpen = false
	handle.steering = nil
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
	message.FactDirective = cloneFactLifecycleDirective(message.FactDirective)
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

func cloneMessagesWithoutFactDirectives(messages []Message) []Message {
	cloned := cloneMessages(messages)
	for index := range cloned {
		cloned[index].FactDirective = nil
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
	cloned := make([]ToolResult, len(results))
	for index := range results {
		cloned[index] = results[index]
		cloned[index].Policy = cloneToolPolicyDecision(results[index].Policy)
		cloned[index].ContextOperation = cloneContextOperation(results[index].ContextOperation)
	}
	return cloned
}

func cloneToolPolicyDecision(decision *ToolPolicyDecision) *ToolPolicyDecision {
	if decision == nil {
		return nil
	}
	cloned := *decision
	cloned.Capabilities = append([]string(nil), decision.Capabilities...)
	return &cloned
}

func cloneEvent(event Event) Event {
	if event.PrefixManifest != nil {
		manifest := clonePrefixManifest(*event.PrefixManifest)
		event.PrefixManifest = &manifest
	}
	event.CachePlan = cloneCachePlanPointer(event.CachePlan)
	if event.ProviderError != nil {
		providerError := *event.ProviderError
		event.ProviderError = &providerError
	}
	if event.ProviderRetry != nil {
		providerRetry := *event.ProviderRetry
		event.ProviderRetry = &providerRetry
	}
	if event.ProviderRateLimitWait != nil {
		providerWait := *event.ProviderRateLimitWait
		event.ProviderRateLimitWait = &providerWait
	}
	event.ContextCheckpoint = cloneContextCheckpointPointer(event.ContextCheckpoint)
	event.ContextCompaction = cloneContextCompactionReport(event.ContextCompaction)
	event.FactDirective = cloneFactLifecycleDirective(event.FactDirective)
	event.CurrentWorldState = cloneCurrentWorldStatePointer(event.CurrentWorldState)
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
		result.Policy = cloneToolPolicyDecision(event.ToolResult.Policy)
		result.ContextOperation = cloneContextOperation(event.ToolResult.ContextOperation)
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

func cloneEvents(events []Event) []Event {
	if events == nil {
		return nil
	}
	cloned := make([]Event, len(events))
	for index := range events {
		cloned[index] = cloneEvent(events[index])
	}
	return cloned
}

func cloneRunResult(result RunResult) RunResult {
	result.Messages = cloneMessages(result.Messages)
	result.ToolResults = cloneToolResults(result.ToolResults)
	if result.Budget != nil {
		budget := *result.Budget
		result.Budget = &budget
	}
	result.ContextCheckpoint = cloneContextCheckpointPointer(result.ContextCheckpoint)
	result.ContextLedger = cloneContextLedgerPointer(result.ContextLedger)
	result.CurrentWorldState = cloneCurrentWorldStatePointer(result.CurrentWorldState)
	result.ContextCompaction = cloneContextCompactionReport(result.ContextCompaction)
	result.CachePlan = cloneCachePlanPointer(result.CachePlan)
	return result
}

func contextCheckpointsEqual(first, second *ContextCheckpoint) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}
	return first.Generation == second.Generation &&
		first.SourceMessageCount == second.SourceMessageCount &&
		first.SourceHash == second.SourceHash
}
