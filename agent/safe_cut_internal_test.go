package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestContextSafeCutPlanProtectsApprovalAndAnnotatedToolTransactions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		operation *ContextOperation
		approval  bool
	}{
		{name: "approval", approval: true},
		{name: "subagent", operation: &ContextOperation{Kind: ContextOperationSubagent}},
		{name: "commit", operation: &ContextOperation{Kind: ContextOperationCommit}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newSafeCutFixture()
			fixture.user("change")
			call := ToolCall{ID: "call-1", Name: test.name}
			fixture.assistant("", call)
			fixture.tool(call, "done", test.operation, test.approval)
			ledger := fixture.ledger(t)

			plan, err := buildContextSafeCutPlan(context.Background(), fixture.messages, fixture.events, &ledger)
			if err != nil {
				t.Fatal(err)
			}
			if !plan.safe(1) || plan.safe(2) {
				t.Fatalf("safe cuts before/inside transaction = %t/%t", plan.safe(1), plan.safe(2))
			}
		})
	}
}

func TestContextSafeCutPlanProtectsCompleteToolBatch(t *testing.T) {
	t.Parallel()

	fixture := newSafeCutFixture()
	fixture.user("inspect")
	first := ToolCall{ID: "call-1", Name: "first"}
	second := ToolCall{ID: "call-2", Name: "second"}
	fixture.assistant("", first, second)
	fixture.tool(first, "one", nil, false)
	fixture.tool(second, "two", nil, false)
	ledger := fixture.ledger(t)

	plan, err := buildContextSafeCutPlan(context.Background(), fixture.messages, fixture.events, &ledger)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.safe(1) || plan.safe(2) || plan.safe(3) {
		t.Fatalf(
			"multi-Tool batch safe cuts = before:%t first:%t second:%t",
			plan.safe(1),
			plan.safe(2),
			plan.safe(3),
		)
	}
}

func TestContextSafeCutPlanProtectsPersistedApprovalAcrossRuns(t *testing.T) {
	t.Parallel()

	call := ToolCall{ID: "call-1", Name: "edit"}
	user := Message{Role: RoleUser, Text: "change"}
	assistant := Message{Role: RoleAssistant, ToolCalls: []ToolCall{call}}
	toolMessage := Message{Role: RoleTool, Text: "changed", ToolCallID: call.ID, ToolName: call.Name}
	request := WaitRequest{ID: "approval-1", Kind: WaitKindApproval}
	response := WaitResponse{RequestID: request.ID, Payload: json.RawMessage(`{"approved":true}`)}
	result := ToolResult{
		CallID: call.ID, Name: call.Name, Output: toolMessage.Text,
		ContextOperation: &ContextOperation{Kind: ContextOperationMutation},
	}
	events := []Event{
		{RunID: "run-first", Sequence: 1, Type: EventRunStarted},
		{RunID: "run-first", Sequence: 2, Type: EventUserMessageAdded, Message: &user},
		{
			RunID: "run-first", Sequence: 3, Type: EventModelRequest,
			ProviderCall: 1, ProviderAttempt: 1,
			PrefixManifest: &PrefixManifest{Version: 1, Provider: "safe-cut/provider", Epoch: "safe-cut"},
		},
		{RunID: "run-first", Sequence: 4, Type: EventMessageCompleted, Message: &assistant},
		{RunID: "run-first", Sequence: 5, Type: EventToolStarted, ToolCall: &call},
		{RunID: "run-first", Sequence: 6, Type: EventRunWaiting, WaitRequest: &request},
		{RunID: "run-resume", Sequence: 1, Type: EventRunStarted},
		{RunID: "run-resume", Sequence: 2, Type: EventToolStarted, ToolCall: &call},
		{RunID: "run-resume", Sequence: 3, Type: EventRunResumed, WaitResponse: &response},
		{
			RunID: "run-resume", Sequence: 4, Type: EventToolCompleted,
			Message: &toolMessage, ToolCall: &call, ToolResult: &result,
		},
	}
	messages := []Message{user, assistant, toolMessage}
	ledger, err := BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildContextSafeCutPlan(context.Background(), messages, events, &ledger)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.safe(1) || plan.safe(2) {
		t.Fatalf("persisted approval safe cuts = before:%t inside:%t", plan.safe(1), plan.safe(2))
	}
}

func TestContextSafeCutPlanKeepsMutationThroughTerminalOperation(t *testing.T) {
	t.Parallel()

	for _, operation := range []ContextOperationKind{ContextOperationVerification, ContextOperationCommit} {
		operation := operation
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			fixture := newSafeCutFixture()
			fixture.user("change")
			editCall := ToolCall{ID: "edit-1", Name: "edit"}
			fixture.assistant("", editCall)
			fixture.tool(editCall, "changed", &ContextOperation{Kind: ContextOperationMutation}, false)
			readCall := ToolCall{ID: "read-1", Name: "read"}
			fixture.assistant("", readCall)
			fixture.tool(readCall, "current", nil, false)
			terminalCall := ToolCall{ID: "terminal-1", Name: string(operation)}
			fixture.assistant("", terminalCall)
			fixture.tool(terminalCall, "done", &ContextOperation{Kind: operation}, false)
			fixture.user("next")
			ledger := fixture.ledger(t)

			plan, err := buildContextSafeCutPlan(context.Background(), fixture.messages, fixture.events, &ledger)
			if err != nil {
				t.Fatal(err)
			}
			if !plan.safe(1) || !plan.safe(7) {
				t.Fatalf("safe outer cuts = before:%t after:%t", plan.safe(1), plan.safe(7))
			}
			for cut := 2; cut <= 6; cut++ {
				if plan.safe(cut) {
					t.Errorf("cut %d splits edit-%s transaction", cut, operation)
				}
			}
		})
	}
}

func TestContextSafeCutPlanKeepsOpenMutationUntilUserBoundary(t *testing.T) {
	t.Parallel()

	fixture := newSafeCutFixture()
	fixture.user("change")
	editCall := ToolCall{ID: "edit-1", Name: "edit"}
	fixture.assistant("", editCall)
	fixture.tool(editCall, "changed", &ContextOperation{Kind: ContextOperationMutation}, false)
	readCall := ToolCall{ID: "read-1", Name: "read"}
	fixture.assistant("", readCall)
	fixture.tool(readCall, "current", nil, false)
	ledger := fixture.ledger(t)

	open, err := buildContextSafeCutPlan(context.Background(), fixture.messages, fixture.events, &ledger)
	if err != nil {
		t.Fatal(err)
	}
	for cut := 2; cut < len(fixture.messages); cut++ {
		if open.safe(cut) {
			t.Errorf("open mutation allowed cut %d", cut)
		}
	}

	fixture.user("accept unverified state")
	ledger = fixture.ledger(t)
	closed, err := buildContextSafeCutPlan(context.Background(), fixture.messages, fixture.events, &ledger)
	if err != nil {
		t.Fatal(err)
	}
	if !closed.safe(5) {
		t.Fatal("new user boundary did not close the prior mutation transaction")
	}
}

func TestContextSafeCutPlanRejectsChangedEventPrefixAndOperation(t *testing.T) {
	t.Parallel()

	fixture := newSafeCutFixture()
	fixture.user("change")
	call := ToolCall{ID: "call-1", Name: "edit"}
	fixture.assistant("", call)
	fixture.tool(call, "changed", &ContextOperation{Kind: ContextOperationMutation}, false)
	ledger := fixture.ledger(t)

	tampered := cloneEvents(fixture.events)
	for index := range tampered {
		if tampered[index].Type == EventToolCompleted {
			tampered[index].Message.Text = "different"
		}
	}
	if _, err := buildContextSafeCutPlan(context.Background(), fixture.messages, tampered, &ledger); err == nil ||
		!strings.Contains(err.Error(), "Context Ledger") {
		t.Fatalf("changed Event error = %v", err)
	}

	invalid := cloneEvents(fixture.events)
	for index := range invalid {
		if invalid[index].Type == EventToolCompleted {
			invalid[index].ToolResult.ContextOperation.Kind = "invented"
		}
	}
	invalidLedger, err := BuildContextLedger(context.Background(), invalid)
	if err == nil || !strings.Contains(err.Error(), "unsupported Context operation") {
		t.Fatalf("BuildContextLedger(invalid operation) error = %v", err)
	}
	if invalidLedger.Version != 0 {
		t.Fatalf("invalid Ledger = %#v", invalidLedger)
	}
}

type safeCutFixture struct {
	runID        string
	sequence     uint64
	providerCall int
	events       []Event
	messages     []Message
}

func newSafeCutFixture() *safeCutFixture {
	fixture := &safeCutFixture{runID: "run-safe-cut"}
	fixture.emit(Event{Type: EventRunStarted})
	return fixture
}

func (fixture *safeCutFixture) emit(event Event) {
	fixture.sequence++
	event.RunID = fixture.runID
	event.Sequence = fixture.sequence
	fixture.events = append(fixture.events, event)
}

func (fixture *safeCutFixture) user(text string) {
	message := Message{Role: RoleUser, Text: text}
	fixture.messages = append(fixture.messages, message)
	fixture.emit(Event{Type: EventUserMessageAdded, Message: &message})
}

func (fixture *safeCutFixture) assistant(text string, calls ...ToolCall) {
	fixture.providerCall++
	fixture.emit(Event{
		Type:            EventModelRequest,
		ProviderCall:    fixture.providerCall,
		ProviderAttempt: 1,
		PrefixManifest: &PrefixManifest{
			Version: 1, Provider: "safe-cut/provider", Epoch: "safe-cut",
		},
	})
	message := Message{Role: RoleAssistant, Text: text, ToolCalls: append([]ToolCall(nil), calls...)}
	fixture.messages = append(fixture.messages, message)
	fixture.emit(Event{Type: EventMessageCompleted, Message: &message})
}

func (fixture *safeCutFixture) tool(
	call ToolCall,
	output string,
	operation *ContextOperation,
	approval bool,
) {
	fixture.emit(Event{Type: EventToolStarted, ToolCall: &call})
	if approval {
		request := WaitRequest{ID: "approval-" + call.ID, Kind: WaitKindApproval}
		fixture.emit(Event{Type: EventRunWaiting, WaitRequest: &request})
		response := WaitResponse{RequestID: request.ID, Payload: json.RawMessage(`{"approved":true}`)}
		fixture.emit(Event{Type: EventRunResumed, WaitResponse: &response})
	}
	message := Message{Role: RoleTool, Text: output, ToolCallID: call.ID, ToolName: call.Name}
	result := ToolResult{
		CallID: call.ID, Name: call.Name, Output: output,
		ContextOperation: cloneContextOperation(operation),
	}
	fixture.messages = append(fixture.messages, message)
	fixture.emit(Event{Type: EventToolCompleted, Message: &message, ToolCall: &call, ToolResult: &result})
}

func (fixture *safeCutFixture) ledger(t *testing.T) ContextLedger {
	t.Helper()
	ledger, err := BuildContextLedger(context.Background(), fixture.events)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}
