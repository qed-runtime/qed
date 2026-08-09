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
	"github.com/qed-runtime/qed/provider/echo"
)

func TestRunEventsReachView(t *testing.T) {
	t.Parallel()

	runtime, err := agent.NewRuntime(agent.Options{Provider: echo.New()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID: "echo",
		Input: []agent.Message{
			{Role: agent.RoleUser, Text: "hello"},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	bridge := newEventBridge(handle)
	defer bridge.Close()
	view := newRunView("hello", bridge.messages, handle.Cancel, handle.Resume)
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
		"Status: completed",
		"Answer: hello",
		"run.completed",
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
	view := newRunView("hello", messages, func() { canceled = true }, nil)
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
	view := newRunView("hello", nil, func() {}, func(value agent.WaitResponse) error {
		response = value
		return nil
	})
	view.Update(message{kind: runEventMessage, event: agent.Event{
		Type: agent.EventRunWaiting,
		WaitRequest: &agent.WaitRequest{
			ID: "approval-1", Kind: agent.WaitKindApproval, Payload: json.RawMessage(`{"tool":"read_file"}`),
		},
	}})
	view.Update(message{kind: approveMessage})
	if response.RequestID != "approval-1" {
		t.Fatalf("WaitResponse = %#v", response)
	}
	var payload struct {
		Approved bool `json:"approved"`
	}
	if err := json.Unmarshal(response.Payload, &payload); err != nil || !payload.Approved {
		t.Fatalf("approval payload = %s, %v", response.Payload, err)
	}
	if view.pendingWait != nil || view.status != "resuming" {
		t.Fatalf("view state = pending %#v status %q", view.pendingWait, view.status)
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
