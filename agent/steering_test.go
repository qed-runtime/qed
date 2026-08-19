package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/session"
)

func TestRuntimeSteersDuringProviderCallAtNextSafePoint(t *testing.T) {
	t.Parallel()

	provider := newSteeringGateProvider(
		agent.Message{Role: agent.RoleAssistant, Text: "first", StopReason: agent.StopReasonEndTurn},
		agent.Message{Role: agent.RoleAssistant, Text: "second", StopReason: agent.StopReasonEndTurn},
	)
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handle, err := runtime.Run(ctx, agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "initial"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	awaitSteeringSignal(t, provider.entered, "first Provider call")
	if err := steerWithin(t, handle, agent.Message{Role: agent.RoleUser, Text: "steer"}); err != nil {
		t.Fatalf("Steer() error = %v", err)
	}
	close(provider.release)

	events, result, runErr := collectRun(handle)
	if runErr != nil || result.Status != agent.RunStatusCompleted || result.ProviderCalls != 2 {
		t.Fatalf("Run = %#v, %v", result, runErr)
	}
	requests := provider.Requests()
	if len(requests) != 2 || len(requests[1].Messages) != 3 {
		t.Fatalf("Provider requests = %#v", requests)
	}
	wantMessages := []agent.Message{
		{Role: agent.RoleUser, Text: "initial"},
		{Role: agent.RoleAssistant, Text: "first", StopReason: agent.StopReasonEndTurn},
		{Role: agent.RoleUser, Text: "steer"},
	}
	for index, want := range wantMessages {
		got := requests[1].Messages[index]
		if got.Role != want.Role || got.Text != want.Text {
			t.Errorf("second request Message[%d] = %#v, want role %q text %q", index, got, want.Role, want.Text)
		}
	}

	completed := steeringEventIndex(events, func(event agent.Event) bool {
		return event.Type == agent.EventMessageCompleted && event.Message != nil && event.Message.Text == "first"
	})
	steered := steeringEventIndex(events, func(event agent.Event) bool {
		return event.Type == agent.EventUserMessageAdded &&
			event.UserMessageOrigin == agent.UserMessageOriginSteering
	})
	secondRequest := steeringEventIndex(events, func(event agent.Event) bool {
		return event.Type == agent.EventModelRequest && event.ProviderCall == 2
	})
	if completed < 0 || steered <= completed || secondRequest <= steered {
		t.Fatalf("first completed/steered/second request indexes = %d/%d/%d: %#v", completed, steered, secondRequest, events)
	}
	if events[steered].Message == nil || events[steered].Message.Text != "steer" ||
		events[steered].RunID != result.RunID {
		t.Fatalf("steering Event = %#v", events[steered])
	}
}

func TestRuntimeSteersAfterCompleteToolBatch(t *testing.T) {
	t.Parallel()

	toolEntered := make(chan struct{})
	toolRelease := make(chan struct{})
	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{
			{ID: "tool-1", Name: "first_gate", Arguments: json.RawMessage(`{}`)},
			{ID: "tool-2", Name: "second_tool", Arguments: json.RawMessage(`{}`)},
		}}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "done", StopReason: agent.StopReasonEndTurn}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider,
		Tools: []agent.Tool{
			&steeringGateTool{name: "first_gate", entered: toolEntered, release: toolRelease},
			namedTool{name: "second_tool"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handle, err := runtime.Run(ctx, agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "initial"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	awaitSteeringSignal(t, toolEntered, "first Tool execution")
	if err := steerWithin(t, handle, agent.Message{Role: agent.RoleUser, Text: "after tools"}); err != nil {
		t.Fatalf("Steer() error = %v", err)
	}
	close(toolRelease)

	events, result, runErr := collectRun(handle)
	if runErr != nil || result.Status != agent.RunStatusCompleted || result.ToolCalls != 2 {
		t.Fatalf("Run = %#v, %v", result, runErr)
	}
	lastToolCompleted := -1
	for index, event := range events {
		if event.Type == agent.EventToolCompleted {
			lastToolCompleted = index
		}
	}
	steered := steeringEventIndex(events, func(event agent.Event) bool {
		return event.Type == agent.EventUserMessageAdded &&
			event.UserMessageOrigin == agent.UserMessageOriginSteering
	})
	secondRequest := steeringEventIndex(events, func(event agent.Event) bool {
		return event.Type == agent.EventModelRequest && event.ProviderCall == 2
	})
	if lastToolCompleted < 0 || steered <= lastToolCompleted || secondRequest <= steered {
		t.Fatalf("last Tool completed/steered/second request indexes = %d/%d/%d: %#v", lastToolCompleted, steered, secondRequest, events)
	}
	requests := provider.Requests()
	if len(requests) != 2 || len(requests[1].Messages) != 5 {
		t.Fatalf("Provider requests = %#v", requests)
	}
	if got := requests[1].Messages[4]; got.Role != agent.RoleUser || got.Text != "after tools" {
		t.Fatalf("Message after Tool batch = %#v", got)
	}
}

func TestRuntimeAppliesPreWaitSteeringAfterResumeAndRejectsWaitingSteering(t *testing.T) {
	t.Parallel()

	toolEntered := make(chan struct{})
	allowWait := make(chan struct{})
	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{
			ID: "approval-call", Name: "staged_approval", Arguments: json.RawMessage(`{}`),
		}}}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "continued", StopReason: agent.StopReasonEndTurn}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider,
		Tools: []agent.Tool{&steeringApprovalTool{
			entered: toolEntered,
			proceed: allowWait,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handle, err := runtime.Run(ctx, agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "initial"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	awaitSteeringSignal(t, toolEntered, "approval Tool execution")
	if err := steerWithin(t, handle, agent.Message{Role: agent.RoleUser, Text: "queued before wait"}); err != nil {
		t.Fatalf("Steer() before wait error = %v", err)
	}
	close(allowWait)

	var events []agent.Event
	waitingObserved := false
	for !waitingObserved {
		select {
		case event, ok := <-handle.Events():
			if !ok {
				t.Fatal("Run terminated before approval wait")
			}
			events = append(events, event)
			if event.Type != agent.EventRunWaiting {
				continue
			}
			waitingObserved = true
			if err := steerWithin(t, handle, agent.Message{Role: agent.RoleUser, Text: "rejected while waiting"}); !errors.Is(err, agent.ErrRunWaiting) {
				t.Fatalf("Steer() while waiting error = %v, want ErrRunWaiting", err)
			}
			pending, ok := handle.PendingWait()
			if !ok || pending.ID == "" {
				t.Fatalf("PendingWait() = %#v, %t", pending, ok)
			}
			if err := handle.Resume(agent.WaitResponse{RequestID: pending.ID}); err != nil {
				t.Fatalf("Resume() error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for approval Event")
		}
	}
	for event := range handle.Events() {
		events = append(events, event)
	}
	result, runErr := handle.Wait()
	if runErr != nil || result.Status != agent.RunStatusCompleted {
		t.Fatalf("Run = %#v, %v", result, runErr)
	}

	var steeredMessages []string
	for _, event := range events {
		if event.Type == agent.EventUserMessageAdded && event.UserMessageOrigin == agent.UserMessageOriginSteering && event.Message != nil {
			steeredMessages = append(steeredMessages, event.Message.Text)
		}
	}
	if len(steeredMessages) != 1 || steeredMessages[0] != "queued before wait" {
		t.Fatalf("steering messages = %#v", steeredMessages)
	}
	resumed := steeringEventIndex(events, func(event agent.Event) bool { return event.Type == agent.EventRunResumed })
	toolCompleted := steeringEventIndex(events, func(event agent.Event) bool { return event.Type == agent.EventToolCompleted })
	steered := steeringEventIndex(events, func(event agent.Event) bool {
		return event.Type == agent.EventUserMessageAdded && event.UserMessageOrigin == agent.UserMessageOriginSteering
	})
	secondRequest := steeringEventIndex(events, func(event agent.Event) bool {
		return event.Type == agent.EventModelRequest && event.ProviderCall == 2
	})
	if resumed < 0 || toolCompleted <= resumed || steered <= toolCompleted || secondRequest <= steered {
		t.Fatalf("resumed/Tool completed/steered/second request indexes = %d/%d/%d/%d: %#v", resumed, toolCompleted, steered, secondRequest, events)
	}
}

func TestRuntimePreservesSteeringFIFO(t *testing.T) {
	t.Parallel()

	provider := newSteeringGateProvider(
		agent.Message{Role: agent.RoleAssistant, Text: "response-1", StopReason: agent.StopReasonEndTurn},
		agent.Message{Role: agent.RoleAssistant, Text: "response-2", StopReason: agent.StopReasonEndTurn},
		agent.Message{Role: agent.RoleAssistant, Text: "response-3", StopReason: agent.StopReasonEndTurn},
		agent.Message{Role: agent.RoleAssistant, Text: "response-4", StopReason: agent.StopReasonEndTurn},
	)
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handle, err := runtime.Run(ctx, agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "initial"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	awaitSteeringSignal(t, provider.entered, "first Provider call")
	want := []string{"one", "two", "three"}
	for _, text := range want {
		if err := steerWithin(t, handle, agent.Message{Role: agent.RoleUser, Text: text}); err != nil {
			t.Fatalf("Steer(%q) error = %v", text, err)
		}
	}
	close(provider.release)

	events, result, runErr := collectRun(handle)
	if runErr != nil || result.Status != agent.RunStatusCompleted {
		t.Fatalf("Run = %#v, %v", result, runErr)
	}
	var got []string
	for _, event := range events {
		if event.Type == agent.EventUserMessageAdded && event.UserMessageOrigin == agent.UserMessageOriginSteering && event.Message != nil {
			got = append(got, event.Message.Text)
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("steering Event order = %#v, want %#v", got, want)
	}
	requests := provider.Requests()
	if len(requests) < 2 {
		t.Fatalf("Provider requests = %#v", requests)
	}
	var lastUserMessages []string
	for _, message := range requests[len(requests)-1].Messages {
		if message.Role == agent.RoleUser {
			lastUserMessages = append(lastUserMessages, message.Text)
		}
	}
	if fmt.Sprint(lastUserMessages) != fmt.Sprint([]string{"initial", "one", "two", "three"}) {
		t.Fatalf("last Provider user messages = %#v", lastUserMessages)
	}
}

func TestRuntimeBoundsAndValidatesPendingSteering(t *testing.T) {
	t.Parallel()

	provider := newSteeringGateProvider(agent.Message{
		Role: agent.RoleAssistant, Text: "unused", StopReason: agent.StopReasonEndTurn,
	})
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handle, err := runtime.Run(ctx, agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "initial"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitSteeringSignal(t, provider.entered, "first Provider call")

	invalid := []agent.Message{
		{},
		{Role: agent.RoleAssistant, Text: "assistant"},
		{Role: agent.RoleUser},
		{Role: agent.RoleUser, Text: "user", RequestID: "provider-request"},
	}
	for _, message := range invalid {
		if err := steerWithin(t, handle, message); !errors.Is(err, agent.ErrInvalidSteeringMessage) {
			t.Errorf("Steer(%#v) error = %v, want ErrInvalidSteeringMessage", message, err)
		}
	}
	if agent.MaxPendingSteeringMessages <= 0 {
		t.Fatalf("MaxPendingSteeringMessages = %d, want positive", agent.MaxPendingSteeringMessages)
	}
	for index := range agent.MaxPendingSteeringMessages {
		message := agent.Message{Role: agent.RoleUser, Text: fmt.Sprintf("queued-%d", index)}
		if err := steerWithin(t, handle, message); err != nil {
			t.Fatalf("Steer() queue entry %d error = %v", index, err)
		}
	}
	if err := steerWithin(t, handle, agent.Message{Role: agent.RoleUser, Text: "overflow"}); !errors.Is(err, agent.ErrSteeringQueueFull) {
		t.Fatalf("Steer() overflow error = %v, want ErrSteeringQueueFull", err)
	}

	handle.Cancel()
	events, result, runErr := collectRun(handle)
	if !errors.Is(runErr, context.Canceled) || result.Status != agent.RunStatusCanceled {
		t.Fatalf("canceled Run = %#v, %v", result, runErr)
	}
	for _, event := range events {
		if event.Type == agent.EventUserMessageAdded && event.UserMessageOrigin == agent.UserMessageOriginSteering {
			t.Fatalf("canceled Run applied pending steering Event: %#v", event)
		}
	}
	if err := steerWithin(t, handle, agent.Message{Role: agent.RoleUser, Text: "after terminal"}); !errors.Is(err, agent.ErrRunClosed) {
		t.Fatalf("Steer() after terminal error = %v, want ErrRunClosed", err)
	}
}

func TestRuntimeFollowUpUsesNewRunInSameSession(t *testing.T) {
	t.Parallel()

	store := session.NewMemoryStore()
	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, Text: "first answer", StopReason: agent.StopReasonEndTurn}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "follow-up answer", StopReason: agent.StopReasonEndTurn}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider, SessionStore: store})
	if err != nil {
		t.Fatal(err)
	}
	firstHandle, err := runtime.Run(context.Background(), agent.RunRequest{
		SessionID: "steering-follow-up",
		Input:     []agent.Message{{Role: agent.RoleUser, Text: "initial"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstEvents, firstResult, err := collectRun(firstHandle)
	if err != nil {
		t.Fatal(err)
	}
	secondHandle, err := runtime.Run(context.Background(), agent.RunRequest{
		SessionID: "steering-follow-up",
		Input:     []agent.Message{{Role: agent.RoleUser, Text: "follow up"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	secondEvents, secondResult, err := collectRun(secondHandle)
	if err != nil {
		t.Fatal(err)
	}
	if firstResult.RunID == "" || secondResult.RunID == "" || firstResult.RunID == secondResult.RunID {
		t.Fatalf("Run IDs = %q/%q", firstResult.RunID, secondResult.RunID)
	}
	for index, event := range firstEvents {
		if event.RunID != firstResult.RunID || event.Sequence != uint64(index+1) {
			t.Fatalf("first Event[%d] identity = %q/%d", index, event.RunID, event.Sequence)
		}
	}
	for index, event := range secondEvents {
		if event.RunID != secondResult.RunID || event.Sequence != uint64(index+1) {
			t.Fatalf("second Event[%d] identity = %q/%d", index, event.RunID, event.Sequence)
		}
	}
	if len(secondEvents) == 0 || secondEvents[0].SessionRevision != firstResult.SessionRevision+1 {
		t.Fatalf("follow-up first Session revision = %d, previous terminal = %d", secondEvents[0].SessionRevision, firstResult.SessionRevision)
	}
	requests := provider.Requests()
	if len(requests) != 2 || len(requests[1].Messages) != 3 {
		t.Fatalf("Provider requests = %#v", requests)
	}
	if requests[1].Messages[0].Text != "initial" || requests[1].Messages[1].Text != "first answer" ||
		requests[1].Messages[2].Text != "follow up" {
		t.Fatalf("follow-up Provider Messages = %#v", requests[1].Messages)
	}
	snapshot, err := store.Load(context.Background(), "steering-follow-up")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != secondResult.SessionRevision || len(snapshot.Messages) != 4 {
		t.Fatalf("Session Snapshot = revision %d messages %#v", snapshot.Revision, snapshot.Messages)
	}
}

func TestRuntimeSteeringConsumesSameRunBudget(t *testing.T) {
	t.Parallel()

	provider := newSteeringGateProvider(agent.Message{
		Role: agent.RoleAssistant, Text: "first", StopReason: agent.StopReasonEndTurn,
	})
	budget, err := agent.NewBudget(agent.BudgetLimits{MaxProviderCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handle, err := runtime.Run(ctx, agent.RunRequest{
		Budget: budget,
		Input:  []agent.Message{{Role: agent.RoleUser, Text: "initial"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitSteeringSignal(t, provider.entered, "first Provider call")
	if err := steerWithin(t, handle, agent.Message{Role: agent.RoleUser, Text: "requires another call"}); err != nil {
		t.Fatalf("Steer() error = %v", err)
	}
	close(provider.release)

	events, result, runErr := collectRun(handle)
	if !errors.Is(runErr, agent.ErrBudgetProviderCalls) || result.Status != agent.RunStatusFailed {
		t.Fatalf("Run = %#v, %v, want ErrBudgetProviderCalls", result, runErr)
	}
	if result.ProviderCalls != 1 || len(provider.Requests()) != 1 || budget.Snapshot().ProviderCalls != 1 {
		t.Fatalf("Provider calls = result %d requests %d budget %d", result.ProviderCalls, len(provider.Requests()), budget.Snapshot().ProviderCalls)
	}
	if steeringEventIndex(events, func(event agent.Event) bool {
		return event.Type == agent.EventUserMessageAdded && event.UserMessageOrigin == agent.UserMessageOriginSteering
	}) < 0 {
		t.Fatalf("Events do not contain accepted steering: %#v", events)
	}
}

func TestRuntimeTerminalGateWinsConcurrentSteerAndCancel(t *testing.T) {
	t.Parallel()

	provider := newSteeringGateProvider(agent.Message{
		Role: agent.RoleAssistant, Text: "done", StopReason: agent.StopReasonEndTurn,
	})
	hook := &terminalSteeringHook{}
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider, Hooks: []agent.Hook{hook}})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "initial"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitSteeringSignal(t, provider.entered, "terminal Provider call")
	hook.setHandle(handle)
	close(provider.release)

	_, result, runErr := collectRun(handle)
	if runErr != nil || result.Status != agent.RunStatusCompleted {
		t.Fatalf("Run = %#v, %v", result, runErr)
	}
	steerErr, contextErr := hook.result()
	if !errors.Is(steerErr, agent.ErrRunClosed) {
		t.Fatalf("Steer() from terminal Hook error = %v, want ErrRunClosed", steerErr)
	}
	if contextErr != nil {
		t.Fatalf("terminal Hook Context error after losing Cancel race = %v", contextErr)
	}
}

func TestRuntimeCancelStopsUnappliedSteeringBatchTail(t *testing.T) {
	t.Parallel()

	store := session.NewMemoryStore()
	provider := newSteeringGateProvider(
		agent.Message{Role: agent.RoleAssistant, Text: "first", StopReason: agent.StopReasonEndTurn},
		agent.Message{Role: agent.RoleAssistant, Text: "unused", StopReason: agent.StopReasonEndTurn},
	)
	hook := newSteeringBarrierHook()
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:     provider,
		Hooks:        []agent.Hook{hook},
		SessionStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		SessionID: "cancel-steering-tail",
		Input:     []agent.Message{{Role: agent.RoleUser, Text: "initial"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitSteeringSignal(t, provider.entered, "first Provider call")
	for _, text := range []string{"one", "two", "three"} {
		if err := handle.Steer(agent.Message{Role: agent.RoleUser, Text: text}); err != nil {
			t.Fatalf("Steer(%q) error = %v", text, err)
		}
	}
	close(provider.release)
	awaitSteeringSignal(t, hook.entered, "first steering Hook")
	handle.Cancel()
	close(hook.release)

	events, result, runErr := collectRun(handle)
	if !errors.Is(runErr, context.Canceled) || result.Status != agent.RunStatusCanceled {
		t.Fatalf("Run = %#v, %v, want canceled", result, runErr)
	}
	var applied []string
	for _, event := range events {
		if event.Type == agent.EventUserMessageAdded &&
			event.UserMessageOrigin == agent.UserMessageOriginSteering && event.Message != nil {
			applied = append(applied, event.Message.Text)
		}
	}
	if fmt.Sprint(applied) != fmt.Sprint([]string{"one"}) {
		t.Fatalf("applied steering = %#v, want only first claimed Message", applied)
	}
	snapshot, err := store.Load(context.Background(), "cancel-steering-tail")
	if err != nil {
		t.Fatal(err)
	}
	var persisted []string
	for _, event := range snapshot.Events {
		if event.Type == agent.EventUserMessageAdded &&
			event.UserMessageOrigin == agent.UserMessageOriginSteering && event.Message != nil {
			persisted = append(persisted, event.Message.Text)
		}
	}
	if fmt.Sprint(persisted) != fmt.Sprint(applied) {
		t.Fatalf("persisted steering = %#v, applied %#v", persisted, applied)
	}
}

func TestRuntimeAcceptsConcurrentSteeringExactlyOnce(t *testing.T) {
	t.Parallel()

	provider := newSteeringGateProvider(
		agent.Message{Role: agent.RoleAssistant, Text: "first", StopReason: agent.StopReasonEndTurn},
		agent.Message{Role: agent.RoleAssistant, Text: "second", StopReason: agent.StopReasonEndTurn},
	)
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "initial"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitSteeringSignal(t, provider.entered, "first Provider call")

	const submissions = 32
	errorsBySubmission := make(chan error, submissions)
	var submitted sync.WaitGroup
	for index := range submissions {
		submitted.Add(1)
		go func() {
			defer submitted.Done()
			errorsBySubmission <- handle.Steer(agent.Message{
				Role: agent.RoleUser,
				Text: fmt.Sprintf("concurrent-%d", index),
			})
		}()
	}
	submitted.Wait()
	close(errorsBySubmission)
	for err := range errorsBySubmission {
		if err != nil {
			t.Fatalf("concurrent Steer() error = %v", err)
		}
	}
	close(provider.release)

	events, result, runErr := collectRun(handle)
	if runErr != nil || result.Status != agent.RunStatusCompleted {
		t.Fatalf("Run = %#v, %v", result, runErr)
	}
	seen := make(map[string]int, submissions)
	for _, event := range events {
		if event.Type == agent.EventUserMessageAdded &&
			event.UserMessageOrigin == agent.UserMessageOriginSteering && event.Message != nil {
			seen[event.Message.Text]++
		}
	}
	if len(seen) != submissions {
		t.Fatalf("distinct steering Event count = %d, want %d", len(seen), submissions)
	}
	for index := range submissions {
		text := fmt.Sprintf("concurrent-%d", index)
		if seen[text] != 1 {
			t.Fatalf("steering Event %q count = %d, want 1", text, seen[text])
		}
	}
}

type steeringGateProvider struct {
	mu        sync.Mutex
	responses []agent.Message
	requests  []agent.ModelRequest
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
}

func newSteeringGateProvider(responses ...agent.Message) *steeringGateProvider {
	return &steeringGateProvider{
		responses: append([]agent.Message(nil), responses...),
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
}

func (*steeringGateProvider) Name() string {
	return "steering-gate"
}

func (provider *steeringGateProvider) Stream(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	provider.mu.Lock()
	index := len(provider.requests)
	provider.requests = append(provider.requests, request)
	if index >= len(provider.responses) {
		provider.mu.Unlock()
		return nil, fmt.Errorf("steering Provider exhausted at call %d", index+1)
	}
	response := provider.responses[index]
	provider.mu.Unlock()

	if index == 0 {
		provider.enterOnce.Do(func() { close(provider.entered) })
		select {
		case <-provider.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return agent.MessageStream(response), nil
}

func (provider *steeringGateProvider) Requests() []agent.ModelRequest {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]agent.ModelRequest(nil), provider.requests...)
}

type steeringGateTool struct {
	name    string
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (tool *steeringGateTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        tool.name,
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}
}

func (tool *steeringGateTool) Execute(ctx context.Context, _ agent.ToolCall) (agent.ToolResult, error) {
	tool.once.Do(func() { close(tool.entered) })
	select {
	case <-tool.release:
		return agent.ToolResult{Output: "released"}, nil
	case <-ctx.Done():
		return agent.ToolResult{}, ctx.Err()
	}
}

type steeringApprovalTool struct {
	entered chan struct{}
	proceed <-chan struct{}
	once    sync.Once
}

func (tool *steeringApprovalTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        "staged_approval",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}
}

func (tool *steeringApprovalTool) Execute(ctx context.Context, _ agent.ToolCall) (agent.ToolResult, error) {
	tool.once.Do(func() { close(tool.entered) })
	select {
	case <-tool.proceed:
	case <-ctx.Done():
		return agent.ToolResult{}, ctx.Err()
	}
	if _, err := agent.WaitForInput(ctx, agent.WaitRequest{
		ID:   "steering-approval",
		Kind: agent.WaitKindApproval,
	}); err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Output: "resumed"}, nil
}

func steerWithin(t *testing.T, handle *agent.RunHandle, message agent.Message) error {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		result <- handle.Steer(message)
	}()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("Steer() blocked")
		return nil
	}
}

func awaitSteeringSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func steeringEventIndex(events []agent.Event, match func(agent.Event) bool) int {
	for index, event := range events {
		if match(event) {
			return index
		}
	}
	return -1
}

type terminalSteeringHook struct {
	mu         sync.Mutex
	handle     *agent.RunHandle
	steerErr   error
	contextErr error
}

func (*terminalSteeringHook) Definition() agent.HookDefinition {
	return agent.HookDefinition{EventTypes: []agent.EventType{agent.EventRunCompleted}}
}

func (hook *terminalSteeringHook) Handle(ctx context.Context, _ agent.Event) error {
	hook.mu.Lock()
	handle := hook.handle
	hook.mu.Unlock()
	if handle == nil {
		return errors.New("terminal Hook has no Run handle")
	}
	steerErr := handle.Steer(agent.Message{Role: agent.RoleUser, Text: "too late"})
	handle.Cancel()
	contextErr := ctx.Err()
	hook.mu.Lock()
	hook.steerErr = steerErr
	hook.contextErr = contextErr
	hook.mu.Unlock()
	return contextErr
}

func (hook *terminalSteeringHook) setHandle(handle *agent.RunHandle) {
	hook.mu.Lock()
	hook.handle = handle
	hook.mu.Unlock()
}

func (hook *terminalSteeringHook) result() (error, error) {
	hook.mu.Lock()
	defer hook.mu.Unlock()
	return hook.steerErr, hook.contextErr
}

type steeringBarrierHook struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newSteeringBarrierHook() *steeringBarrierHook {
	return &steeringBarrierHook{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (*steeringBarrierHook) Definition() agent.HookDefinition {
	return agent.HookDefinition{EventTypes: []agent.EventType{agent.EventUserMessageAdded}}
}

func (hook *steeringBarrierHook) Handle(_ context.Context, event agent.Event) error {
	if event.UserMessageOrigin != agent.UserMessageOriginSteering {
		return nil
	}
	blocked := false
	hook.once.Do(func() {
		blocked = true
		close(hook.entered)
	})
	if blocked {
		<-hook.release
	}
	return nil
}
