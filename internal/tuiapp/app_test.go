package tuiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayahiro/nagi-go/vt"
	tui "github.com/mayahiro/nagitui-go"
	"github.com/mayahiro/nagitui-go/surface"
	"github.com/mayahiro/nagitui-go/tuitest"
	"github.com/mayahiro/nagitui-go/widget"

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

func TestChatTerminalOptionsEnableMouseInput(t *testing.T) {
	t.Parallel()

	options := chatTerminalOptions()
	if options.MouseTracking == nil || *options.MouseTracking != vt.MouseTrackingPress {
		t.Fatalf("MouseTracking = %v, want press tracking", options.MouseTracking)
	}
}

func TestFunctionKeysMapLongChatNavigation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  vt.KeyEvent
		want messageKind
	}{
		{name: "context", key: vt.KeyEvent{Code: vt.KeyFunction, Function: 2}, want: toggleContextMessage},
		{name: "older", key: vt.KeyEvent{Code: vt.KeyFunction, Function: 6}, want: browseOlderSessionMessage},
		{
			name: "newer",
			key:  vt.KeyEvent{Code: vt.KeyFunction, Function: 6, Modifiers: vt.Modifiers{Shift: true}},
			want: browseNewerSessionMessage,
		},
		{name: "current", key: vt.KeyEvent{Code: vt.KeyFunction, Function: 7}, want: returnCurrentSessionMessage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			action := mapEvent(vt.Event{Kind: vt.EventKey, Key: test.key})
			value, ok := action.Message()
			if action.Kind() != tui.EventMessage || !ok || value.kind != test.want {
				t.Fatalf("action/message = %+v/%+v, want %d", action, value, test.want)
			}
		})
	}
}

func TestSessionHistoryExcludesLiveAndDuplicateDescriptors(t *testing.T) {
	t.Parallel()

	descriptors := excludeSession([]session.SessionDescriptor{
		{ID: "current"},
		{ID: "older"},
		{ID: "older"},
		{ID: "oldest"},
		{},
	}, "current")
	if len(descriptors) != 2 || descriptors[0].ID != "older" || descriptors[1].ID != "oldest" {
		t.Fatalf("filtered Session descriptors = %#v", descriptors)
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
		if activity.label == "Tool read_file" {
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

func TestAdapterScopesToolAndApprovalActivitiesByRun(t *testing.T) {
	t.Parallel()

	presentation := newRunPresentation("", runIdentity{})
	for _, runID := range []string{"run-first", "run-second"} {
		for _, event := range []agent.Event{
			{
				RunID: runID, Sequence: 1, Type: agent.EventToolStarted,
				ToolCall: &agent.ToolCall{ID: "shared-call", Name: "read_file"},
			},
			{
				RunID: runID, Sequence: 2, Type: agent.EventToolCompleted,
				ToolCall:   &agent.ToolCall{ID: "shared-call", Name: "read_file"},
				ToolResult: &agent.ToolResult{CallID: "shared-call", Name: "read_file"},
			},
		} {
			presentation.apply(adaptRunEvent(event))
		}
	}
	if len(presentation.activities) != 2 {
		t.Fatalf("Tool activities = %#v", presentation.activities)
	}
	for _, activity := range presentation.activities {
		if activity.state != activityStateCompleted {
			t.Fatalf("Tool activity = %#v", activity)
		}
	}

	wait := &agent.WaitRequest{
		ID:      "shared-approval",
		Kind:    agent.WaitKindApproval,
		Payload: json.RawMessage(`{"tool":"read_file","capabilities":["workspace.read"]}`),
	}
	for index, runID := range []string{"run-first", "run-second"} {
		presentation.apply(adaptRunEvent(agent.Event{
			RunID: runID, Sequence: 3, Type: agent.EventRunWaiting, WaitRequest: wait,
		}))
		if _, ok := presentation.resolveApproval(index == 0); !ok {
			t.Fatalf("resolve approval for %s = false", runID)
		}
	}
	if len(presentation.activities) != 4 {
		t.Fatalf("All activities = %#v", presentation.activities)
	}
	if presentation.activities[2].state != activityStateApproved ||
		presentation.activities[3].state != activityStateDenied {
		t.Fatalf("Approval activities = %#v", presentation.activities[2:])
	}
	if presentation.activities[0].key == presentation.activities[1].key ||
		presentation.activities[2].key == presentation.activities[3].key {
		t.Fatalf("Activity keys are not Run-scoped = %#v", presentation.activities)
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

func TestIdleChatStartsFirstRunOnSubmit(t *testing.T) {
	t.Parallel()

	runtime, err := agent.NewRuntime(agent.Options{Provider: echo.New()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	starts := 0
	start := func(ctx context.Context, request agent.RunRequest) (*agent.RunHandle, error) {
		starts++
		return runtime.Run(ctx, request)
	}
	view := newIdleChatView(context.Background(), start, agent.RunRequest{AgentID: "echo"})
	harness, err := tuitest.New(view, tui.Size{Width: 100, Height: 30}, mapEvent)
	if err != nil {
		t.Fatalf("tuitest.New: %v", err)
	}
	defer closeChatHarness(view, harness)

	if starts != 0 || view.runNumber != 0 || !view.finished {
		t.Fatalf("idle state = starts %d run %d finished %t", starts, view.runNumber, view.finished)
	}
	rendered := surfaceText(harness.LatestSurface())
	if !strings.Contains(rendered, "Status: ready") || !strings.Contains(rendered, "Enter start") {
		t.Fatalf("idle chat is not rendered as ready:\n%s", rendered)
	}
	if _, err := harness.RequestFocus(composerInputID); err != nil {
		t.Fatalf("focus composer: %v", err)
	}
	if err := harness.Input([]byte("first message\r")); err != nil {
		t.Fatalf("submit first message: %v", err)
	}
	if starts != 1 || view.runNumber != 1 {
		t.Fatalf("started state = starts %d run %d", starts, view.runNumber)
	}
	waitForChat(t, harness, func() bool { return view.finished }, "first Run")

	outcome := view.Outcome()
	if len(outcome.Runs) != 1 || len(outcome.Result.Messages) != 2 ||
		outcome.Result.Messages[0].Text != "first message" ||
		outcome.Result.Messages[1].Text != "first message" {
		t.Fatalf("idle chat Outcome = %#v", outcome)
	}
	rendered = surfaceText(harness.LatestSurface())
	for _, expected := range []string{"You: first message", "Assistant: first message", "Enter follow-up"} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("rendered surface does not contain %q:\n%s", expected, rendered)
		}
	}
}

func TestComposerRendersDraftBeforeSubmit(t *testing.T) {
	t.Parallel()

	view := newIdleChatView(
		context.Background(),
		func(context.Context, agent.RunRequest) (*agent.RunHandle, error) {
			t.Fatal("Composer draft unexpectedly started a Run")
			return nil, nil
		},
		agent.RunRequest{AgentID: "echo"},
	)
	harness, err := tuitest.New(view, tui.Size{Width: 100, Height: 30}, mapEvent)
	if err != nil {
		t.Fatalf("tuitest.New: %v", err)
	}
	defer harness.Close()

	if _, err := harness.RequestFocus(composerInputID); err != nil {
		t.Fatalf("focus composer: %v", err)
	}
	if rendered := surfaceText(harness.LatestSurface()); !strings.Contains(rendered, "Send a message") {
		t.Fatalf("rendered surface does not contain Composer placeholder:\n%s", rendered)
	}
	const draft = "ascii 日本語"
	if err := harness.Input([]byte(draft)); err != nil {
		t.Fatalf("type Composer draft: %v", err)
	}
	if view.draftText() != draft {
		t.Fatalf("Composer draft = %q, want %q", view.draftText(), draft)
	}
	rendered := surfaceText(harness.LatestSurface())
	if !strings.Contains(rendered, draft) {
		t.Fatalf("rendered surface does not contain Composer draft %q:\n%s", draft, rendered)
	}
}

func TestChatAndComposerClickSwitchFocusForHistoryNavigation(t *testing.T) {
	t.Parallel()

	view := newIdleChatView(
		context.Background(),
		func(context.Context, agent.RunRequest) (*agent.RunHandle, error) {
			t.Fatal("history navigation unexpectedly started a Run")
			return nil, nil
		},
		agent.RunRequest{AgentID: "echo"},
	)
	messages := make([]agent.Message, 40)
	for index := range messages {
		messages[index] = agent.Message{
			Role: agent.RoleUser,
			Text: fmt.Sprintf("history-%02d", index),
		}
	}
	view.presentation.reconcileMessages(messages)

	harness, err := tuitest.New(view, tui.Size{Width: 80, Height: 20}, mapEvent)
	if err != nil {
		t.Fatalf("tuitest.New: %v", err)
	}
	defer harness.Close()

	if focused, ok := harness.Interaction().Focused(); !ok || focused != composerInputID {
		t.Fatalf("initial focus = %q, present = %t", focused, ok)
	}
	initial, ok := harness.ScrollState(chatViewportID)
	if !ok || !initial.AtEnd || initial.AtStart {
		t.Fatalf("initial chat ScrollState = %+v, present = %t", initial, ok)
	}

	if err := harness.Input([]byte("\x1b[<0;5;8M")); err != nil {
		t.Fatalf("click chat history: %v", err)
	}
	if focused, ok := harness.Interaction().Focused(); !ok || focused != chatViewportID {
		t.Fatalf("chat focus = %q, present = %t", focused, ok)
	}
	if err := harness.Input([]byte("\x1b[5~")); err != nil {
		t.Fatalf("PageUp chat history: %v", err)
	}
	afterPageUp, ok := harness.ScrollState(chatViewportID)
	if !ok || afterPageUp.Offset.Y >= initial.Offset.Y || afterPageUp.AtEnd {
		t.Fatalf("chat ScrollState after PageUp = %+v, initial = %+v", afterPageUp, initial)
	}
	if err := harness.Input([]byte("\x1b[6~")); err != nil {
		t.Fatalf("PageDown chat history: %v", err)
	}
	afterPageDown, ok := harness.ScrollState(chatViewportID)
	if !ok || afterPageDown.Offset.Y <= afterPageUp.Offset.Y {
		t.Fatalf("chat ScrollState after PageDown = %+v, PageUp = %+v", afterPageDown, afterPageUp)
	}
	if err := harness.Input([]byte("\x1b[5~")); err != nil {
		t.Fatalf("second PageUp chat history: %v", err)
	}
	afterPageUp, ok = harness.ScrollState(chatViewportID)
	if !ok || afterPageUp.Offset.Y >= afterPageDown.Offset.Y {
		t.Fatalf("chat ScrollState after second PageUp = %+v, PageDown = %+v", afterPageUp, afterPageDown)
	}

	if err := harness.Input([]byte("\x1b[<64;5;8M")); err != nil {
		t.Fatalf("wheel chat history: %v", err)
	}
	afterWheel, ok := harness.ScrollState(chatViewportID)
	if !ok || afterWheel.Offset.Y >= afterPageUp.Offset.Y {
		t.Fatalf("chat ScrollState after wheel = %+v, PageUp = %+v", afterWheel, afterPageUp)
	}

	if err := harness.Input([]byte("\x1b[<0;5;16M")); err != nil {
		t.Fatalf("click Composer: %v", err)
	}
	if focused, ok := harness.Interaction().Focused(); !ok || focused != composerInputID {
		t.Fatalf("Composer focus = %q, present = %t", focused, ok)
	}
	if err := harness.Input([]byte("draft")); err != nil {
		t.Fatalf("type Composer draft: %v", err)
	}
	if view.draftText() != "draft" {
		t.Fatalf("Composer draft = %q, want draft", view.draftText())
	}
}

func TestIdleChatRejectsSessionWithPendingInput(t *testing.T) {
	t.Parallel()

	store := session.NewMemoryStore()
	wait := agent.WaitRequest{ID: "pending-input", Kind: agent.WaitKindApproval}
	if _, err := store.Append(context.Background(), "waiting-session", 0, []agent.Event{{
		Type:        agent.EventRunWaiting,
		WaitRequest: &wait,
	}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	starts := 0
	start := func(context.Context, agent.RunRequest) (*agent.RunHandle, error) {
		starts++
		return nil, errors.New("unexpected start")
	}
	_, err := RunWithStarterOptions(
		context.Background(),
		start,
		agent.RunRequest{AgentID: "echo", SessionID: "waiting-session"},
		"",
		ChatOptions{SessionStore: store},
	)
	if err == nil || !strings.Contains(err.Error(), "resume it before starting a new Run") {
		t.Fatalf("RunWithStarterOptions error = %v", err)
	}
	if starts != 0 {
		t.Fatalf("Run starts = %d, want 0", starts)
	}
}

func TestIdleChatContinuesExistingSession(t *testing.T) {
	t.Parallel()

	store := session.NewMemoryStore()
	runtime, err := agent.NewRuntime(agent.Options{Provider: echo.New(), SessionStore: store})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	request := agent.RunRequest{
		AgentID:   "echo",
		SessionID: "idle-existing-session",
		Input:     []agent.Message{{Role: agent.RoleUser, Text: "first"}},
	}
	handle, err := runtime.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range handle.Events() {
	}
	if result, waitErr := handle.Wait(); waitErr != nil || result.Status != agent.RunStatusCompleted {
		t.Fatalf("Wait = %#v, %v", result, waitErr)
	}
	snapshot, err := store.Snapshot(context.Background(), request.SessionID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	idleRequest := agent.RunRequest{AgentID: "echo", SessionID: request.SessionID}
	view := newIdleChatView(context.Background(), runtime.Run, idleRequest)
	view.seedIdleSession(snapshot, idleRequest)
	harness, err := tuitest.New(view, tui.Size{Width: 100, Height: 30}, mapEvent)
	if err != nil {
		t.Fatalf("tuitest.New: %v", err)
	}
	defer closeChatHarness(view, harness)
	rendered := surfaceText(harness.LatestSurface())
	if !strings.Contains(rendered, "You: first") || !strings.Contains(rendered, "Status: ready") {
		t.Fatalf("existing idle Session is not seeded:\n%s", rendered)
	}
	if _, err := harness.RequestFocus(composerInputID); err != nil {
		t.Fatalf("focus composer: %v", err)
	}
	if err := harness.Input([]byte("second\r")); err != nil {
		t.Fatalf("submit continuation: %v", err)
	}
	waitForChat(t, harness, func() bool { return view.finished }, "continued Run")

	snapshot, err = store.Snapshot(context.Background(), request.SessionID)
	if err != nil {
		t.Fatalf("continued Snapshot: %v", err)
	}
	wantMessages := []agent.Message{
		{Role: agent.RoleUser, Text: "first"},
		{Role: agent.RoleAssistant, Text: "first", StopReason: agent.StopReasonEndTurn},
		{Role: agent.RoleUser, Text: "second"},
		{Role: agent.RoleAssistant, Text: "second", StopReason: agent.StopReasonEndTurn},
	}
	if !equalChatMessages(snapshot.Messages, wantMessages) {
		t.Fatalf("continued Session Messages = %#v", snapshot.Messages)
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
	if view.draftText() != "" || view.inputNotice != "Steering queued" {
		t.Fatalf("steering draft/notice = %q/%q", view.draftText(), view.inputNotice)
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

func TestLongChatPresentationIsBoundedAndKeepsStableTailIDs(t *testing.T) {
	t.Parallel()

	presentation := newRunPresentation("", runIdentity{})
	messages := make([]agent.Message, maximumTranscriptHistory+100)
	for index := range messages {
		messages[index] = agent.Message{Role: agent.RoleUser, Text: fmt.Sprintf("message-%04d", index)}
	}
	presentation.reconcileMessages(messages)
	if len(presentation.transcript) != maximumTranscriptHistory {
		t.Fatalf("transcript length = %d", len(presentation.transcript))
	}
	if presentation.transcript[0].text != "message-0100" {
		t.Fatalf("transcript head = %q", presentation.transcript[0].text)
	}
	firstTailID := presentation.transcript[len(presentation.transcript)-1].id
	entries, items := buildFeedEntries(&presentation)
	if len(entries) != maximumTranscriptHistory+2 || len(items) != len(entries) {
		t.Fatalf("feed entries/items = %d/%d", len(entries), len(items))
	}

	presentation.reconcileMessages(messages)
	if got := presentation.transcript[len(presentation.transcript)-1].id; got != firstTailID {
		t.Fatalf("stable tail ID = %d, want %d", got, firstTailID)
	}
	if _, err := tui.NewVirtualFlowItems(items); err != nil {
		t.Fatalf("VirtualFeed item identities: %v", err)
	}

	other := newRunPresentation("", runIdentity{sessionID: "other-session"})
	other.reconcileMessages(messages)
	otherEntries, _ := buildFeedEntries(&other)
	if entries[0].id == otherEntries[0].id {
		t.Fatalf("historical Session item IDs collide = %q", entries[0].id)
	}

	streaming := newRunPresentation("", runIdentity{})
	streaming.apply(presentationUpdate{assistantStarted: true})
	previousRevision := streaming.revision
	streaming.apply(presentationUpdate{answerDelta: "delta"})
	update := virtualFlowUpdate(&streaming)
	previous, ok := update.PreviousRevision()
	start, end := update.ChangedRange()
	if update.Revision() != previousRevision+1 || !ok || previous != previousRevision || start != 0 || end != 1 {
		t.Fatalf(
			"streaming VirtualFeed update = revision %d previous %d/%t range %d:%d",
			update.Revision(),
			previous,
			ok,
			start,
			end,
		)
	}
}

func TestCurrentSessionSnapshotSeedsBoundedChatAndComposerHistory(t *testing.T) {
	t.Parallel()

	messages := make([]agent.Message, 0, 2*(maximumComposerHistory+10))
	for index := 0; index < maximumComposerHistory+10; index++ {
		messages = append(messages,
			agent.Message{Role: agent.RoleUser, Text: fmt.Sprintf("prior-%03d", index)},
			agent.Message{Role: agent.RoleAssistant, Text: fmt.Sprintf("answer-%03d", index)},
		)
	}
	snapshot := agent.SessionSnapshot{
		ID:       "seeded-session",
		Revision: 3,
		Messages: messages,
		Events: []agent.Event{
			{RunID: "prior-run", AgentID: "prior-agent", SessionID: "seeded-session", Sequence: 1, Type: agent.EventRunStarted},
			{
				RunID: "prior-run", AgentID: "prior-agent", SessionID: "seeded-session", Sequence: 2,
				Type: agent.EventContextCompacted,
				ContextCompaction: &agent.ContextCompactionReport{
					Applied: true, Reason: "externalize_evidence", OriginalBytes: 100, CompiledBytes: 60,
					Externalized: []agent.EvidenceObjectRef{{Bytes: 40}},
				},
			},
			{RunID: "prior-run", AgentID: "prior-agent", SessionID: "seeded-session", Sequence: 3, Type: agent.EventRunCompleted},
		},
	}
	view := newRunView("current", runIdentity{}, nil, func() {}, nil)
	view.seedCurrentSession(snapshot, agent.RunRequest{
		AgentID: "current-agent", SessionID: "seeded-session",
	}, "current")

	if len(view.presentation.transcript) != len(messages)+1 ||
		view.presentation.transcript[len(view.presentation.transcript)-1].text != "current" ||
		view.presentation.transcript[len(view.presentation.transcript)-1].state != transcriptStateQueued {
		t.Fatalf("seeded transcript = %#v", view.presentation.transcript)
	}
	if view.presentation.context.compactions != 1 || view.presentation.context.externalized != 1 ||
		view.presentation.context.externalizedBytes != 40 {
		t.Fatalf("seeded Context = %#v", view.presentation.context)
	}
	if view.presentation.identity.agentID != "current-agent" || view.presentation.identity.runID != "" ||
		view.presentation.status != "starting" {
		t.Fatalf("seeded identity/status = %#v/%q", view.presentation.identity, view.presentation.status)
	}
	history := view.composerHistory.Entries()
	if len(history) != maximumComposerHistory || history[0].Value() != "prior-011" ||
		history[len(history)-1].Value() != "current" {
		t.Fatalf(
			"seeded Composer history = %d/%q/%q",
			len(history),
			history[0].Value(),
			history[len(history)-1].Value(),
		)
	}
}

func TestComposerAcceptsMultilineSteeringAndBoundsRecallHistory(t *testing.T) {
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
	multiline := "inspect first\nthen verify"
	if err := harness.Send(message{
		kind:     composerChangedMessage,
		composer: widget.NewComposerStateAtEnd(multiline),
	}); err != nil {
		t.Fatalf("set Composer state: %v", err)
	}
	if err := harness.Send(message{kind: submitMessage}); err != nil {
		t.Fatalf("submit Composer: %v", err)
	}
	close(provider.release)
	waitForChat(t, harness, func() bool { return view.finished }, "multiline steering")

	requests := provider.Requests()
	if len(requests) != 2 || requests[1].Messages[len(requests[1].Messages)-1].Text != multiline {
		t.Fatalf("Provider requests = %#v", requests)
	}
	if view.draftText() != "" {
		t.Fatalf("Composer draft = %q", view.draftText())
	}
	for index := 0; index < maximumComposerHistory+20; index++ {
		view.recordComposerHistory(fmt.Sprintf("history-%03d", index))
	}
	entries := view.composerHistory.Entries()
	if len(entries) != maximumComposerHistory || entries[0].Value() != "history-020" {
		t.Fatalf("Composer history length/head = %d/%q", len(entries), entries[0].Value())
	}
	view.recordComposerHistory(strings.Repeat("x", maximumComposerBytes+1))
	if got := len(view.composerHistory.Entries()); got != maximumComposerHistory {
		t.Fatalf("Composer accepted oversized history entry, length = %d", got)
	}
}

func TestContextCompactionPreservesLatestPredictiveBudget(t *testing.T) {
	t.Parallel()

	presentation := newRunPresentation("", runIdentity{})
	presentation.apply(presentationUpdate{context: &contextObservation{
		hasCompaction:   true,
		generation:      7,
		generationKnown: true,
	}})
	presentation.apply(presentationUpdate{context: &contextObservation{
		hasPredictive:    true,
		predictiveLevel:  agent.PredictiveBudgetSoft,
		predictiveAction: agent.PredictiveBudgetActionPrepare,
		predictedInput:   80,
		maximumInput:     100,
	}})
	presentation.apply(presentationUpdate{context: &contextObservation{
		hasCompaction:     true,
		compacted:         true,
		reason:            "input_limit",
		externalized:      math.MaxInt,
		externalizedBytes: math.MaxInt64,
	}})
	presentation.apply(presentationUpdate{context: &contextObservation{
		hasCompaction:     true,
		externalized:      1,
		externalizedBytes: 1,
	}})
	last := presentation.context.last
	if !last.hasPredictive || last.predictiveLevel != agent.PredictiveBudgetSoft ||
		last.predictiveAction != agent.PredictiveBudgetActionPrepare ||
		last.predictedInput != 80 || last.maximumInput != 100 ||
		!last.generationKnown || last.generation != 7 {
		t.Fatalf("predictive Context state = %#v", last)
	}
	if presentation.context.externalized != math.MaxInt ||
		presentation.context.externalizedBytes != math.MaxInt64 {
		t.Fatalf("saturated Context totals = %#v", presentation.context)
	}
}

func TestContextAndCacheStatusRemainContentFree(t *testing.T) {
	t.Parallel()

	view := newRunView("hello", runIdentity{}, nil, func() {}, nil)
	view.Update(message{kind: runEventMessage, update: adaptRunEvent(agent.Event{
		Sequence: 1,
		Type:     agent.EventContextCompacted,
		ContextCheckpoint: &agent.ContextCheckpoint{
			Generation: 3,
		},
		ContextCompaction: &agent.ContextCompactionReport{
			Applied:            true,
			Reason:             "secret custom reason",
			OriginalBytes:      1000,
			CompiledBytes:      400,
			SourceMessageCount: 8,
			RecentMessageCount: 2,
			Externalized: []agent.EvidenceObjectRef{{
				Digest: "secret-evidence-digest", Bytes: 4096, MediaType: "text/plain",
			}},
		},
	})})
	view.Update(message{kind: runEventMessage, update: adaptRunEvent(agent.Event{
		Sequence: 2,
		Type:     agent.EventModelRequest,
		CachePlan: &agent.CachePlan{
			Mode:               agent.CacheModeExplicit,
			TTL:                agent.CacheTTLFiveMinutes,
			Breakpoints:        []agent.CacheBreakpoint{{AfterSegmentID: "secret-segment"}},
			ExpectedReuse:      2,
			InputTokenEstimate: 80,
			TokenEstimateKind:  agent.CanonicalByteTokenEstimateKind,
			FallbackReason:     "secret cache fallback",
			FamilyID:           "secret-cache-family",
		},
		PredictiveBudget: &agent.PredictiveBudgetPlan{
			Level:                      agent.PredictiveBudgetSoft,
			Action:                     agent.PredictiveBudgetActionPrepare,
			ProviderInputTokenEstimate: 80,
			MaxInputTokens:             100,
		},
	})})
	usage := agent.Usage{
		InputTokens: 70, InputTokenDetailsReported: true,
		CacheReadInputTokens: 50, CacheWriteInputTokens: 10,
	}
	view.Update(message{kind: runEventMessage, update: adaptRunEvent(agent.Event{
		Sequence: 3,
		Type:     agent.EventMessageCompleted,
		Message:  &agent.Message{Role: agent.RoleAssistant, Text: "done", Usage: &usage},
	})})
	view.Update(message{kind: toggleContextMessage})

	harness, err := tuitest.New(view, tui.Size{Width: 120, Height: 28}, mapEvent)
	if err != nil {
		t.Fatalf("tuitest.New: %v", err)
	}
	defer harness.Close()
	rendered := surfaceText(harness.LatestSurface())
	for _, expected := range []string{
		"Context: compactions=1 generation=3 budget=soft input=80/100",
		"Cache: explicit breakpoints=1 estimate=80 actual=70",
		"evidence=1 objects/4096 bytes",
		"Last compaction: reason=unrecognized applied=true bytes=1000->400 messages=8+2 recent",
		"Cache detail: ttl=5m reuse=2 estimator=canonical_bytes_div_4 read=50 write=10 fallback=unrecognized",
		"Evidence retrieval: ask the agent to use context_search or context_fetch within its scoped access",
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("rendered surface does not contain %q:\n%s", expected, rendered)
		}
	}
	for _, protected := range []string{
		"secret custom reason", "secret-evidence-digest", "secret-segment",
		"secret cache fallback", "secret-cache-family",
	} {
		if strings.Contains(rendered, protected) {
			t.Errorf("rendered surface contains protected value %q:\n%s", protected, rendered)
		}
	}
}

func TestChatNavigatesRecentSessionAndReturnsToCurrentChat(t *testing.T) {
	t.Parallel()

	store := session.NewMemoryStore()
	runtime, err := agent.NewRuntime(agent.Options{Provider: echo.New(), SessionStore: store})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	completeRun := func(sessionID, prompt string) {
		t.Helper()
		handle, runErr := runtime.Run(context.Background(), agent.RunRequest{
			AgentID: "echo", SessionID: sessionID,
			Input: []agent.Message{{Role: agent.RoleUser, Text: prompt}},
		})
		if runErr != nil {
			t.Fatalf("Run %q: %v", sessionID, runErr)
		}
		for range handle.Events() {
		}
		if result, waitErr := handle.Wait(); waitErr != nil || result.Status != agent.RunStatusCompleted {
			t.Fatalf("Wait %q = %#v, %v", sessionID, result, waitErr)
		}
	}
	completeRun("older-session", "older message")

	request := agent.RunRequest{
		AgentID:   "echo",
		SessionID: "current-session",
		Input:     []agent.Message{{Role: agent.RoleUser, Text: "current message"}},
	}
	view, harness := startChatHarness(t, runtime.Run, request, "current message")
	view.options.SessionStore = store
	defer closeChatHarness(view, harness)
	waitForChat(t, harness, func() bool { return view.finished }, "current Session")

	if err := harness.Send(message{kind: browseOlderSessionMessage}); err != nil {
		t.Fatalf("browse older Session: %v", err)
	}
	waitForChat(t, harness, func() bool { return view.historyView != nil && !view.historyLoading }, "older Session")
	rendered := surfaceText(harness.LatestSurface())
	for _, expected := range []string{"Session: older-session", "You: older message", "Status: history revision"} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("history surface does not contain %q:\n%s", expected, rendered)
		}
	}
	if strings.Contains(rendered, "Message│") {
		t.Fatalf("history view rendered an editable Composer:\n%s", rendered)
	}

	if err := harness.Send(message{kind: returnCurrentSessionMessage}); err != nil {
		t.Fatalf("return to current Session: %v", err)
	}
	rendered = surfaceText(harness.LatestSurface())
	for _, expected := range []string{"Session: current-session", "You: current message", "Enter follow-up"} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("current surface does not contain %q:\n%s", expected, rendered)
		}
	}
}

func TestStaleSessionHistoryResultCannotReplaceCurrentChat(t *testing.T) {
	t.Parallel()

	view := newRunView("current", runIdentity{sessionID: "current"}, nil, func() {}, nil)
	view.historyRequest = 2
	view.Update(message{
		kind:           sessionHistoryLoadedMessage,
		historyRequest: 1,
		sessionDescriptor: session.SessionDescriptor{
			ID: "stale", Revision: 1,
		},
		sessionSnapshot: agent.SessionSnapshot{ID: "stale"},
	})
	if view.historyView != nil || view.historySession.ID != "" {
		t.Fatalf("stale Session history was applied = %#v/%#v", view.historyView, view.historySession)
	}
}

func TestSessionHistoryLoadingKeepsCurrentRunReadOnly(t *testing.T) {
	t.Parallel()

	canceled := 0
	view := newRunView("current", runIdentity{sessionID: "current"}, nil, func() { canceled++ }, nil)
	view.composerEnabled = true
	view.historyLoading = true
	if view.composerVisible() {
		t.Fatal("Composer is visible while Session history is loading")
	}
	view.Update(message{kind: cancelMessage})
	if canceled != 0 || view.inputNotice != "Press F7 to return before canceling the current Run" {
		t.Fatalf("history loading cancel state = %d/%q", canceled, view.inputNotice)
	}
	view.Update(message{kind: returnCurrentSessionMessage})
	if view.historyLoading || !view.composerVisible() {
		t.Fatalf("current Session state = loading %t composer %t", view.historyLoading, view.composerVisible())
	}
}

func TestApprovalWaitReturnsFromSessionHistoryToCurrentRun(t *testing.T) {
	t.Parallel()

	view := newRunView("current", runIdentity{sessionID: "current"}, nil, func() {}, nil)
	history := newRunPresentation("", runIdentity{sessionID: "older"})
	view.historyView = &history
	view.historySession = session.SessionDescriptor{ID: "older"}
	view.Update(message{kind: runEventMessage, update: adaptRunEvent(agent.Event{
		Type: agent.EventRunWaiting,
		WaitRequest: &agent.WaitRequest{
			ID: "approval-current", Kind: agent.WaitKindApproval,
			Payload: json.RawMessage(`{"tool":"read_file","capabilities":["workspace.read"]}`),
		},
	})})
	if view.historyView != nil || view.historySession.ID != "" ||
		view.presentation.pendingApproval == nil || view.inputNotice != "Current Run requires input" {
		t.Fatalf(
			"approval/history state = history %#v descriptor %#v approval %#v notice %q",
			view.historyView,
			view.historySession,
			view.presentation.pendingApproval,
			view.inputNotice,
		)
	}
}

func TestApprovalWaitCancelsPendingSessionHistoryLoad(t *testing.T) {
	t.Parallel()

	view := newRunView("current", runIdentity{sessionID: "current"}, nil, func() {}, nil)
	view.historyLoading = true
	view.historyRequest = 4
	view.Update(message{kind: runEventMessage, update: adaptRunEvent(agent.Event{
		Type: agent.EventRunWaiting,
		WaitRequest: &agent.WaitRequest{
			ID: "approval-current", Kind: agent.WaitKindApproval,
			Payload: json.RawMessage(`{"tool":"read_file","capabilities":["workspace.read"]}`),
		},
	})})
	if view.historyLoading || view.historyRequest != 5 || view.presentation.pendingApproval == nil ||
		view.inputNotice != "Current Run requires input" {
		t.Fatalf(
			"approval/history load state = loading %t request %d approval %#v notice %q",
			view.historyLoading,
			view.historyRequest,
			view.presentation.pendingApproval,
			view.inputNotice,
		)
	}
}

func TestRuntimeFailuresDistinguishHistoryFromRunStream(t *testing.T) {
	t.Parallel()

	historyView := newRunView("current", runIdentity{sessionID: "current"}, nil, func() {}, nil)
	historyView.historyLoading = true
	historyView.historyRequest = 2
	historyView.Update(message{kind: runtimeFailureMessage, err: errSessionHistoryTask})
	if historyView.historyLoading || historyView.historyRequest != 3 || historyView.exiting ||
		historyView.inputNotice != "Session history is unavailable" {
		t.Fatalf(
			"history failure state = loading %t request %d exiting %t notice %q",
			historyView.historyLoading,
			historyView.historyRequest,
			historyView.exiting,
			historyView.inputNotice,
		)
	}

	canceled := 0
	runView := newRunView("current", runIdentity{}, nil, func() { canceled++ }, nil)
	runView.Update(message{kind: runtimeFailureMessage, err: errRunEventStream})
	if canceled != 1 || !runView.exiting || !errors.Is(runView.runErr, errRunEventStream) ||
		runView.presentation.status != "TUI event stream failed" {
		t.Fatalf(
			"Run stream failure state = canceled %d exiting %t error %v status %q",
			canceled,
			runView.exiting,
			runView.runErr,
			runView.presentation.status,
		)
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
