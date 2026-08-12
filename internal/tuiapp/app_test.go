package tuiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayahiro/nagi-go/vt"
	tui "github.com/mayahiro/nagitui-go"
	"github.com/mayahiro/nagitui-go/surface"
	"github.com/mayahiro/nagitui-go/tuitest"

	"github.com/qed-runtime/qed/agent"
	providerbase "github.com/qed-runtime/qed/provider"
	"github.com/qed-runtime/qed/provider/echo"
	"github.com/qed-runtime/qed/session"
)

func TestRunEventsReachView(t *testing.T) {
	t.Parallel()

	runtime, err := agent.NewRuntime(agent.Options{Provider: echo.New()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID:   "echo",
		SessionID: "session-tui",
		Input: []agent.Message{
			{Role: agent.RoleUser, Text: "hello"},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	bridge := newEventBridge(handle)
	defer bridge.Close()
	view := newRunView(
		"hello",
		runIdentity{agentID: "echo", sessionID: "session-tui"},
		bridge.messages,
		handle.Cancel,
		func(requestID string, approved bool) error {
			response, responseErr := approvalWaitResponse(requestID, approved)
			if responseErr != nil {
				return responseErr
			}
			return handle.Resume(response)
		},
	)
	harness, err := tuitest.New(view, tui.Size{Width: 80, Height: 20}, mapEvent)
	if err != nil {
		t.Fatalf("tuitest.New: %v", err)
	}
	defer harness.Close()

	select {
	case <-view.streamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Run Event stream")
	}
	if err := harness.Step(); err != nil {
		t.Fatalf("Step: %v", err)
	}
	outcome := bridge.Outcome()
	if outcome.Result.Status != agent.RunStatusCompleted || len(outcome.Events) == 0 {
		t.Fatalf("bridge Outcome = %#v", outcome)
	}

	rendered := surfaceText(harness.LatestSurface())
	for _, expected := range []string{
		"QED Runtime",
		"Agent: echo  Session: session-tui",
		"Status: completed",
		"You: hello",
		"Assistant: hello",
		"Run completed [completed]",
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("rendered surface does not contain %q:\n%s", expected, rendered)
		}
	}
}

func TestQuitRequestsExitAndCancelsRun(t *testing.T) {
	t.Parallel()

	messages := make(chan message)
	canceled := false
	view := newRunView("hello", runIdentity{}, messages, func() { canceled = true }, nil)
	harness, err := tuitest.New(view, tui.Size{Width: 60, Height: 12}, mapEvent)
	if err != nil {
		t.Fatalf("tuitest.New: %v", err)
	}

	if err := harness.Input([]byte("q")); err != nil {
		harness.Close()
		t.Fatalf("Input: %v", err)
	}
	if !harness.ExitRequested() {
		t.Error("quit did not request exit")
	}
	if !canceled {
		t.Error("quit did not cancel the Agent Run")
	}

	harness.Close()
	select {
	case <-view.streamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for subscription shutdown")
	}
}

func TestControlCProducesCancelMessage(t *testing.T) {
	t.Parallel()

	action := mapEvent(vt.Event{
		Kind: vt.EventKey,
		Key: vt.KeyEvent{
			Code:      vt.KeyCharacter,
			Character: 'c',
			Modifiers: vt.Modifiers{Control: true},
		},
	})
	value, ok := action.Message()
	if action.Kind() != tui.EventMessage || !ok || value.kind != cancelMessage {
		t.Fatalf("action = %+v, message = %+v, %t, want cancel message", action, value, ok)
	}
}

func TestApprovalKeysResumePendingRunWithoutExposingPayload(t *testing.T) {
	t.Parallel()

	var response agent.WaitResponse
	view := newRunView("hello", runIdentity{}, nil, func() {}, func(requestID string, approved bool) error {
		value, err := approvalWaitResponse(requestID, approved)
		if err != nil {
			return err
		}
		response = value
		return nil
	})
	view.Update(message{kind: runEventMessage, update: adaptRunEvent(agent.Event{
		Type: agent.EventRunWaiting,
		WaitRequest: &agent.WaitRequest{
			ID: "approval-1", Kind: agent.WaitKindApproval,
			Prompt:  "payload prompt must not be rendered",
			Payload: json.RawMessage(`{"tool":"read_file","capabilities":["workspace.read"]}`),
		},
	})})
	if view.presentation.pendingApproval == nil ||
		view.presentation.pendingApproval.tool != "read_file" ||
		len(view.presentation.pendingApproval.capabilities) != 1 ||
		view.presentation.pendingApproval.capabilities[0] != "workspace.read" {
		t.Fatalf("pending approval = %#v", view.presentation.pendingApproval)
	}
	harness, err := tuitest.New(view, tui.Size{Width: 90, Height: 14}, mapEvent)
	if err != nil {
		t.Fatalf("tuitest.New: %v", err)
	}
	rendered := surfaceText(harness.LatestSurface())
	if !strings.Contains(rendered, "Approval: Tool read_file [workspace.read]") ||
		strings.Contains(rendered, "payload prompt must not be rendered") ||
		strings.Contains(rendered, "approval-1") {
		t.Fatalf("approval surface violates display contract:\n%s", rendered)
	}
	if err := harness.Input([]byte("y")); err != nil {
		harness.Close()
		t.Fatalf("Input: %v", err)
	}
	harness.Close()
	if response.RequestID != "approval-1" {
		t.Fatalf("WaitResponse = %#v", response)
	}
	var payload struct {
		Approved bool `json:"approved"`
	}
	if err := json.Unmarshal(response.Payload, &payload); err != nil || !payload.Approved {
		t.Fatalf("approval payload = %s, %v", response.Payload, err)
	}
	if view.presentation.pendingApproval != nil || view.presentation.status != "resuming" {
		t.Fatalf("view state = pending %#v status %q", view.presentation.pendingApproval, view.presentation.status)
	}
	if len(view.presentation.activities) != 1 || view.presentation.activities[0].state != activityStateApproved {
		t.Fatalf("approval activity = %#v", view.presentation.activities)
	}
}

func TestAdapterMapsContentAndContentFreeDiagnostics(t *testing.T) {
	t.Parallel()

	view := newRunView(
		"inspect the workspace",
		runIdentity{agentID: "configured-agent", sessionID: "configured-session"},
		nil,
		func() {},
		nil,
	)
	events := []agent.Event{
		{
			Sequence: 1, Type: agent.EventRunStarted,
			RunID: "run-adapter", AgentID: "agent-event", SessionID: "session-event",
		},
		{
			Sequence: 2, Type: agent.EventProviderRateLimitWait,
			ProviderRateLimitWait: &agent.ProviderRateLimitWaitInfo{
				Reason: agent.ProviderRateLimitWaitConcurrency, MaxConcurrency: 2,
			},
		},
		{
			Sequence: 3, Type: agent.EventProviderRateLimitWait,
			ProviderRateLimitWait: &agent.ProviderRateLimitWaitInfo{
				Reason:                 agent.ProviderRateLimitWaitCooldown,
				MaxConcurrency:         2,
				RetryAfterMilliseconds: 750,
			},
		},
		{Sequence: 4, Type: agent.EventCurrentWorldStateCaptured},
		{Sequence: 5, Type: agent.EventContextCompactionPrepared},
		{Sequence: 6, Type: agent.EventModelRequest},
		{
			Sequence: 7, Type: agent.EventProviderRetry,
			ProviderRetry: &agent.ProviderRetryInfo{
				Error:             agent.ProviderErrorInfo{Code: providerbase.ErrorCodeRateLimited, Attempt: 1},
				NextAttempt:       2,
				DelayMilliseconds: 1000,
			},
		},
		{Sequence: 8, Type: agent.EventMessageStarted},
		{Sequence: 9, Type: agent.EventMessageDelta, Delta: "assistant-visible-content"},
		{
			Sequence: 10, Type: agent.EventToolStarted,
			ToolCall: &agent.ToolCall{
				ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"secret-input"}`),
			},
		},
		{
			Sequence: 11, Type: agent.EventToolCompleted,
			ToolCall:   &agent.ToolCall{ID: "call-1", Name: "read_file"},
			ToolResult: &agent.ToolResult{CallID: "call-1", Name: "read_file", Output: "secret-output"},
		},
		{Sequence: 12, Type: agent.EventRunFailed, Error: "secret-error"},
	}
	for _, event := range events {
		view.Update(message{kind: runEventMessage, update: adaptRunEvent(event)})
	}

	if view.presentation.identity != (runIdentity{
		runID: "run-adapter", agentID: "agent-event", sessionID: "session-event",
	}) {
		t.Fatalf("identity = %#v", view.presentation.identity)
	}
	if view.presentation.answer != "assistant-visible-content" || view.presentation.status != "failed" {
		t.Fatalf("presentation answer/status = %q / %q", view.presentation.answer, view.presentation.status)
	}
	var toolActivities []runActivity
	for _, activity := range view.presentation.activities {
		if activity.key == "tool:call-1" {
			toolActivities = append(toolActivities, activity)
		}
	}
	if len(toolActivities) != 1 || toolActivities[0].label != "Tool read_file" ||
		toolActivities[0].state != activityStateCompleted {
		t.Fatalf("Tool activities = %#v", toolActivities)
	}

	harness, err := tuitest.New(view, tui.Size{Width: 100, Height: 20}, mapEvent)
	if err != nil {
		t.Fatalf("tuitest.New: %v", err)
	}
	defer harness.Close()
	rendered := surfaceText(harness.LatestSurface())
	for _, expected := range []string{
		"Agent: agent-event  Session: session-event  Run: run-adapter",
		"Assistant: assistant-visible-content",
		"Waiting for model capacity (limit 2) [waiting]",
		"Model rate limit cooldown 750ms [waiting]",
		"Model retry 2 in 1000ms (rate_limited) [waiting]",
		"Current state captured",
		"Context candidate prepared",
		"Tool read_file [completed]",
		"Run failed [failed]",
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("rendered surface does not contain %q:\n%s", expected, rendered)
		}
	}
	for _, excluded := range []string{"secret-input", "secret-output", "secret-error"} {
		if strings.Contains(rendered, excluded) {
			t.Errorf("rendered surface contains protected value %q:\n%s", excluded, rendered)
		}
	}
}

func TestAdapterDistinguishesSteeringFromInitialRequest(t *testing.T) {
	t.Parallel()

	initial := adaptRunEvent(agent.Event{
		Sequence: 1,
		Type:     agent.EventUserMessageAdded,
		Message:  &agent.Message{Role: agent.RoleUser, Text: "initial private request"},
	})
	steering := adaptRunEvent(agent.Event{
		Sequence:          2,
		Type:              agent.EventUserMessageAdded,
		Message:           &agent.Message{Role: agent.RoleUser, Text: "private steering request"},
		UserMessageOrigin: agent.UserMessageOriginSteering,
	})

	if initial.activity == nil || initial.activity.label != "Request added" {
		t.Fatalf("initial activity = %#v", initial.activity)
	}
	if steering.activity == nil || steering.activity.label != "Steering added" {
		t.Fatalf("steering activity = %#v", steering.activity)
	}
	for _, update := range []presentationUpdate{initial, steering} {
		if update.activity == nil {
			continue
		}
		if strings.Contains(update.activity.label, "private") {
			t.Fatalf("activity label contains Message content: %q", update.activity.label)
		}
	}
}

func TestMalformedApprovalCannotBeAcceptedOrRendered(t *testing.T) {
	t.Parallel()

	resolved := false
	view := newRunView(
		"hello",
		runIdentity{},
		nil,
		func() {},
		func(string, bool) error {
			resolved = true
			return nil
		},
	)
	view.Update(message{kind: runEventMessage, update: adaptRunEvent(agent.Event{
		Sequence: 8,
		Type:     agent.EventRunWaiting,
		WaitRequest: &agent.WaitRequest{
			ID:      "approval-malformed",
			Kind:    agent.WaitKindApproval,
			Payload: json.RawMessage(`{"tool":"read_file","arguments":"secret-argument"}`),
		},
	})})
	view.Update(message{kind: approveMessage})
	if resolved || view.presentation.pendingApproval != nil || !view.presentation.waitingUnsupported {
		t.Fatalf(
			"malformed approval state = resolved %t, pending %#v, unsupported %t",
			resolved,
			view.presentation.pendingApproval,
			view.presentation.waitingUnsupported,
		)
	}

	harness, err := tuitest.New(view, tui.Size{Width: 80, Height: 14}, mapEvent)
	if err != nil {
		t.Fatalf("tuitest.New: %v", err)
	}
	defer harness.Close()
	rendered := surfaceText(harness.LatestSurface())
	if !strings.Contains(rendered, "Approval request unavailable [failed]") ||
		!strings.Contains(rendered, "Input cannot be handled here") {
		t.Fatalf("malformed approval diagnostic is missing:\n%s", rendered)
	}
	for _, excluded := range []string{"read_file", "secret-argument", "approval-malformed"} {
		if strings.Contains(rendered, excluded) {
			t.Errorf("rendered surface contains rejected approval value %q:\n%s", excluded, rendered)
		}
	}
}

func TestChatFollowUpUsesPersistentSession(t *testing.T) {
	t.Parallel()

	store := session.NewMemoryStore()
	runtime, err := agent.NewRuntime(agent.Options{Provider: echo.New(), SessionStore: store})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	request := agent.RunRequest{
		AgentID:   "echo",
		SessionID: "chat-session",
		Input:     []agent.Message{{Role: agent.RoleUser, Text: "first"}},
	}
	view, harness := startChatHarness(t, runtime.Run, request, "first")
	defer closeChatHarness(view, harness)

	waitForChat(t, harness, func() bool { return view.finished }, "initial Run")
	if _, err := harness.RequestFocus(composerInputID); err != nil {
		t.Fatalf("focus composer: %v", err)
	}
	if err := harness.Input([]byte("second\r")); err != nil {
		t.Fatalf("submit follow-up: %v", err)
	}
	if view.runNumber != 2 || view.finished {
		t.Fatalf("follow-up state = run %d finished %t", view.runNumber, view.finished)
	}
	waitForChat(t, harness, func() bool { return view.finished }, "follow-up Run")

	outcome := view.Outcome()
	if len(outcome.Runs) != 2 || outcome.Result.Status != agent.RunStatusCompleted {
		t.Fatalf("chat Outcome = %#v", outcome)
	}
	snapshot, err := store.Snapshot(context.Background(), "chat-session")
	if err != nil {
		t.Fatalf("Session Snapshot: %v", err)
	}
	wantMessages := []agent.Message{
		{Role: agent.RoleUser, Text: "first"},
		{Role: agent.RoleAssistant, Text: "first", StopReason: agent.StopReasonEndTurn},
		{Role: agent.RoleUser, Text: "second"},
		{Role: agent.RoleAssistant, Text: "second", StopReason: agent.StopReasonEndTurn},
	}
	if !equalChatMessages(snapshot.Messages, wantMessages) {
		t.Fatalf("Session Messages = %#v", snapshot.Messages)
	}
	rendered := surfaceText(harness.LatestSurface())
	for _, expected := range []string{"You: first", "Assistant: first", "You: second", "Assistant: second", "Enter follow-up"} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("rendered surface does not contain %q:\n%s", expected, rendered)
		}
	}
}

func TestChatFollowUpWithoutSessionCarriesHistory(t *testing.T) {
	t.Parallel()

	runtime, err := agent.NewRuntime(agent.Options{Provider: echo.New()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	request := agent.RunRequest{
		AgentID: "echo",
		Input:   []agent.Message{{Role: agent.RoleUser, Text: "first"}},
	}
	view, harness := startChatHarness(t, runtime.Run, request, "first")
	defer closeChatHarness(view, harness)

	waitForChat(t, harness, func() bool { return view.finished }, "initial Run")
	if _, err := harness.RequestFocus(composerInputID); err != nil {
		t.Fatalf("focus composer: %v", err)
	}
	if err := harness.Input([]byte("second\r")); err != nil {
		t.Fatalf("submit follow-up: %v", err)
	}
	waitForChat(t, harness, func() bool { return view.finished }, "follow-up Run")

	if len(view.currentResult.Messages) != 4 || view.currentResult.Messages[2].Text != "second" {
		t.Fatalf("follow-up Messages = %#v", view.currentResult.Messages)
	}
	requestActivities := 0
	for _, activity := range view.presentation.activities {
		if activity.label == "Request added" {
			requestActivities++
		}
	}
	if requestActivities != 2 {
		t.Fatalf("Request activity count = %d, want 2", requestActivities)
	}
}

func TestChatSteersActiveRunAtNextProviderBoundary(t *testing.T) {
	t.Parallel()

	provider := newScriptedTUIProvider([]agent.Message{
		{Role: agent.RoleAssistant, Text: "first answer", StopReason: agent.StopReasonEndTurn},
		{Role: agent.RoleAssistant, Text: "steered answer", StopReason: agent.StopReasonEndTurn},
	}, true)
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	request := agent.RunRequest{
		AgentID: "scripted",
		Input:   []agent.Message{{Role: agent.RoleUser, Text: "first"}},
	}
	view, harness := startChatHarness(t, runtime.Run, request, "first")
	defer closeChatHarness(view, harness)

	select {
	case <-provider.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Provider call")
	}
	if _, err := harness.RequestFocus(composerInputID); err != nil {
		t.Fatalf("focus composer: %v", err)
	}
	if err := harness.Input([]byte("steer now\r")); err != nil {
		t.Fatalf("submit steering: %v", err)
	}
	if view.draft != "" || view.inputNotice != "Steering queued" {
		t.Fatalf("steering draft/notice = %q/%q", view.draft, view.inputNotice)
	}
	close(provider.release)
	waitForChat(t, harness, func() bool { return view.finished }, "steered Run")

	requests := provider.Requests()
	if len(requests) != 2 {
		t.Fatalf("Provider requests = %d, want 2", len(requests))
	}
	if got := requests[1].Messages[len(requests[1].Messages)-1]; got.Role != agent.RoleUser || got.Text != "steer now" {
		t.Fatalf("second Provider request tail = %#v", got)
	}
	steeringEvents := 0
	for _, event := range view.Outcome().Runs[0].Events {
		if event.Type == agent.EventUserMessageAdded && event.UserMessageOrigin == agent.UserMessageOriginSteering {
			steeringEvents++
		}
	}
	if steeringEvents != 1 {
		t.Fatalf("steering Events = %d, want 1", steeringEvents)
	}
	rendered := surfaceText(harness.LatestSurface())
	for _, expected := range []string{"You: steer now", "Assistant: steered answer", "Steering added"} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("rendered surface does not contain %q:\n%s", expected, rendered)
		}
	}
}

func TestChatApprovesToolAndResumesRun(t *testing.T) {
	t.Parallel()

	provider := newScriptedTUIProvider([]agent.Message{
		{
			Role:       agent.RoleAssistant,
			StopReason: agent.StopReasonToolUse,
			ToolCalls: []agent.ToolCall{{
				ID: "approval-call", Name: "approval_tool", Arguments: json.RawMessage(`{}`),
			}},
		},
		{Role: agent.RoleAssistant, Text: "approved", StopReason: agent.StopReasonEndTurn},
	}, false)
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider, Tools: []agent.Tool{tuiApprovalTool{}}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	request := agent.RunRequest{
		AgentID: "scripted",
		Input:   []agent.Message{{Role: agent.RoleUser, Text: "use the tool"}},
	}
	view, harness := startChatHarness(t, runtime.Run, request, "use the tool")
	defer closeChatHarness(view, harness)

	waitForChat(t, harness, func() bool { return view.presentation.pendingApproval != nil }, "approval wait")
	rendered := surfaceText(harness.LatestSurface())
	if !strings.Contains(rendered, "Approval: Tool approval_tool [workspace.read]") {
		t.Fatalf("approval is not rendered:\n%s", rendered)
	}
	if err := harness.Input([]byte("y")); err != nil {
		t.Fatalf("approve Tool: %v", err)
	}
	waitForChat(t, harness, func() bool { return view.finished }, "resumed Run")

	eventTypes := make(map[agent.EventType]bool)
	for _, event := range view.Outcome().Runs[0].Events {
		eventTypes[event.Type] = true
	}
	for _, eventType := range []agent.EventType{
		agent.EventToolStarted,
		agent.EventRunWaiting,
		agent.EventRunResumed,
		agent.EventToolCompleted,
		agent.EventRunCompleted,
	} {
		if !eventTypes[eventType] {
			t.Errorf("missing Event %q", eventType)
		}
	}
	rendered = surfaceText(harness.LatestSurface())
	for _, expected := range []string{"Assistant: approved", "Tool approval_tool [completed]", "Run resumed"} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("rendered surface does not contain %q:\n%s", expected, rendered)
		}
	}
}

func TestChatControlCCancelsCurrentRunWithoutExiting(t *testing.T) {
	t.Parallel()

	provider := newScriptedTUIProvider([]agent.Message{
		{Role: agent.RoleAssistant, Text: "unreachable", StopReason: agent.StopReasonEndTurn},
	}, true)
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	request := agent.RunRequest{
		AgentID: "scripted",
		Input:   []agent.Message{{Role: agent.RoleUser, Text: "wait"}},
	}
	view, harness := startChatHarness(t, runtime.Run, request, "wait")
	defer closeChatHarness(view, harness)

	select {
	case <-provider.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Provider call")
	}
	if err := harness.Input([]byte{0x03}); err != nil {
		t.Fatalf("send Ctrl-C: %v", err)
	}
	waitForChat(t, harness, func() bool { return view.finished }, "canceled Run")
	if harness.ExitRequested() {
		t.Fatal("Ctrl-C exited the chat")
	}
	if view.currentResult.Status != agent.RunStatusCanceled || view.runErr != nil {
		t.Fatalf("cancel result/error = %#v / %v", view.currentResult, view.runErr)
	}
	if !strings.Contains(surfaceText(harness.LatestSurface()), "Status: canceled") {
		t.Fatalf("canceled status is not rendered:\n%s", surfaceText(harness.LatestSurface()))
	}
}

type scriptedTUIProvider struct {
	mu        sync.Mutex
	responses []agent.Message
	requests  []agent.ModelRequest
	entered   chan struct{}
	release   chan struct{}
	gateOnce  sync.Once
	gateFirst bool
}

func newScriptedTUIProvider(responses []agent.Message, gateFirst bool) *scriptedTUIProvider {
	return &scriptedTUIProvider{
		responses: append([]agent.Message(nil), responses...),
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		gateFirst: gateFirst,
	}
}

func (*scriptedTUIProvider) Name() string {
	return "scripted-tui"
}

func (provider *scriptedTUIProvider) Stream(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	provider.mu.Lock()
	index := len(provider.requests)
	provider.requests = append(provider.requests, request)
	if index >= len(provider.responses) {
		provider.mu.Unlock()
		return nil, fmt.Errorf("scripted TUI Provider exhausted at call %d", index+1)
	}
	response := provider.responses[index]
	provider.mu.Unlock()

	if provider.gateFirst && index == 0 {
		provider.gateOnce.Do(func() { close(provider.entered) })
		select {
		case <-provider.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return agent.MessageStream(response), nil
}

func (provider *scriptedTUIProvider) Requests() []agent.ModelRequest {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]agent.ModelRequest(nil), provider.requests...)
}

type tuiApprovalTool struct{}

func (tuiApprovalTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:         "approval_tool",
		InputSchema:  json.RawMessage(`{"type":"object","additionalProperties":false}`),
		Capabilities: []string{"workspace.read"},
	}
}

func (tuiApprovalTool) Execute(ctx context.Context, _ agent.ToolCall) (agent.ToolResult, error) {
	response, err := agent.WaitForInput(ctx, agent.WaitRequest{
		ID:     "approval-tui",
		Kind:   agent.WaitKindApproval,
		Prompt: "content not rendered",
		Payload: json.RawMessage(
			`{"tool":"approval_tool","capabilities":["workspace.read"]}`,
		),
	})
	if err != nil {
		return agent.ToolResult{}, err
	}
	var payload struct {
		Approved bool `json:"approved"`
	}
	if err := json.Unmarshal(response.Payload, &payload); err != nil {
		return agent.ToolResult{}, err
	}
	if !payload.Approved {
		return agent.ToolResult{Output: "denied", IsError: true}, nil
	}
	return agent.ToolResult{Output: "approved"}, nil
}

func startChatHarness(
	t *testing.T,
	start StartFunc,
	request agent.RunRequest,
	prompt string,
) (*runView, *tuitest.Harness[message]) {
	t.Helper()
	handle, err := start(context.Background(), request)
	if err != nil {
		t.Fatalf("start Run: %v", err)
	}
	bridge := newEventBridgeForRun(handle, 1, 0)
	view := newChatView(context.Background(), start, request, prompt, handle, bridge)
	harness, err := tuitest.New(view, tui.Size{Width: 100, Height: 30}, mapEvent)
	if err != nil {
		view.closeRuns()
		t.Fatalf("tuitest.New: %v", err)
	}
	return view, harness
}

func closeChatHarness(view *runView, harness *tuitest.Harness[message]) {
	view.closeRuns()
	harness.Close()
}

func waitForChat(
	t *testing.T,
	harness *tuitest.Harness[message],
	ready func() bool,
	description string,
) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ready() {
		if err := harness.Step(); err != nil {
			t.Fatalf("step while waiting for %s: %v", description, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
	if err := harness.Step(); err != nil {
		t.Fatalf("final step for %s: %v", description, err)
	}
}

func equalChatMessages(left, right []agent.Message) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Role != right[index].Role ||
			left[index].Text != right[index].Text ||
			left[index].StopReason != right[index].StopReason {
			return false
		}
	}
	return true
}

func surfaceText(rendered *surface.Surface) string {
	var output strings.Builder
	for y := range rendered.Height() {
		for x := range rendered.Width() {
			cell, ok := rendered.Cell(int32(x), int32(y))
			if !ok || cell.Continuation() {
				continue
			}
			output.WriteString(cell.Content())
		}
		output.WriteByte('\n')
	}
	return output.String()
}
