package tuiapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mayahiro/nagi-go/vt"
	tui "github.com/mayahiro/nagitui-go"
	"github.com/mayahiro/nagitui-go/surface"
	"github.com/mayahiro/nagitui-go/tuitest"

	"github.com/qed-runtime/qed/agent"
	providerbase "github.com/qed-runtime/qed/provider"
	"github.com/qed-runtime/qed/provider/echo"
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
		"Answer: hello",
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
		{Sequence: 4, Type: agent.EventModelRequest},
		{
			Sequence: 5, Type: agent.EventProviderRetry,
			ProviderRetry: &agent.ProviderRetryInfo{
				Error:             agent.ProviderErrorInfo{Code: providerbase.ErrorCodeRateLimited, Attempt: 1},
				NextAttempt:       2,
				DelayMilliseconds: 1000,
			},
		},
		{Sequence: 6, Type: agent.EventMessageStarted},
		{Sequence: 7, Type: agent.EventMessageDelta, Delta: "assistant-visible-content"},
		{
			Sequence: 8, Type: agent.EventToolStarted,
			ToolCall: &agent.ToolCall{
				ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"secret-input"}`),
			},
		},
		{
			Sequence: 9, Type: agent.EventToolCompleted,
			ToolCall:   &agent.ToolCall{ID: "call-1", Name: "read_file"},
			ToolResult: &agent.ToolResult{CallID: "call-1", Name: "read_file", Output: "secret-output"},
		},
		{Sequence: 10, Type: agent.EventRunFailed, Error: "secret-error"},
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
		"Answer: assistant-visible-content",
		"Waiting for model capacity (limit 2) [waiting]",
		"Model rate limit cooldown 750ms [waiting]",
		"Model retry 2 in 1000ms (rate_limited) [waiting]",
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
