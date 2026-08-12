package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
	providerbase "github.com/qed-runtime/qed/provider"
)

func TestBuildContextLedgerReconstructsFiveLedgersDeterministically(t *testing.T) {
	t.Parallel()

	events := completeLedgerEvents()
	first, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	second, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || first.Digest != second.Digest {
		t.Fatalf("Ledger is not deterministic:\n%#v\n%#v", first, second)
	}
	if first.Version != agent.ContextLedgerVersion || first.SessionID != "ledger-session" ||
		first.SessionRevision != uint64(len(events)) || first.SourceEventCount != len(events) ||
		len(first.Sources) != len(events) {
		t.Fatalf("Ledger identity = %#v", first)
	}
	if len(first.Artifacts) != 2 || first.Artifacts[0].Kind != agent.ArtifactLedgerToolOutput ||
		first.Artifacts[1].Kind != agent.ArtifactLedgerEvidenceObject {
		t.Fatalf("Artifact Ledger = %#v", first.Artifacts)
	}
	if len(first.Executions) != 4 || first.Executions[0].State != agent.ExecutionLedgerFailed ||
		first.Executions[1].State != agent.ExecutionLedgerSucceeded ||
		first.Executions[2].Kind != agent.ExecutionLedgerToolCall ||
		first.Executions[2].State != agent.ExecutionLedgerSucceeded ||
		first.Executions[3].State != agent.ExecutionLedgerSucceeded {
		t.Fatalf("Execution Ledger = %#v", first.Executions)
	}
	if len(first.Constraints) != 1 || first.Constraints[0].Text != "change the file" ||
		first.Constraints[0].Kind != agent.ConstraintLedgerUserInput {
		t.Fatalf("Constraint Ledger = %#v", first.Constraints)
	}
	if len(first.Policies) != 2 || first.Policies[0].Kind != agent.PolicyLedgerHumanApproval ||
		first.Policies[0].Outcome != agent.PolicyLedgerAllowed ||
		first.Policies[1].Kind != agent.PolicyLedgerToolAuthorization ||
		first.Policies[1].Outcome != agent.PolicyLedgerAllowed {
		t.Fatalf("Policy Ledger = %#v", first.Policies)
	}
	if len(first.Tasks) != 1 || first.Tasks[0].State != agent.TaskLedgerCompleted || first.Tasks[0].InputCount != 1 {
		t.Fatalf("Task Ledger = %#v", first.Tasks)
	}
	if err := agent.ValidateContextLedger(context.Background(), first, events); err != nil {
		t.Fatalf("ValidateContextLedger: %v", err)
	}

	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var decoded agent.ContextLedger
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, first) {
		t.Fatalf("JSON round trip changed Ledger:\n%#v\n%#v", first, decoded)
	}

	tampered := first
	tampered.Tasks = append([]agent.TaskLedgerEntry(nil), first.Tasks...)
	tampered.Tasks[0].State = agent.TaskLedgerFailed
	if err := agent.ValidateContextLedger(context.Background(), tampered, events); err == nil {
		t.Fatal("ValidateContextLedger accepted changed derived state")
	}
	changedEvents := cloneLedgerEvents(events)
	changedEvents[1].Message.Text = "change another file"
	changed, err := agent.BuildContextLedger(context.Background(), changedEvents)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Digest == first.Digest || changed.SourceHash == first.SourceHash {
		t.Fatal("changed source Event did not change Ledger identity")
	}
}

func TestBuildContextLedgerRejectsBrokenEventTransactions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events []agent.Event
		want   string
	}{
		{
			name: "missing run start",
			events: []agent.Event{{
				RunID: "run-1", Sequence: 1, Type: agent.EventUserMessageAdded,
				Message: &agent.Message{Role: agent.RoleUser, Text: "input"},
			}},
			want: "before run.started",
		},
		{
			name: "sequence gap",
			events: []agent.Event{
				{RunID: "run-1", Sequence: 1, Type: agent.EventRunStarted},
				{RunID: "run-1", Sequence: 3, Type: agent.EventRunCompleted},
			},
			want: "Sequence = 3, want 2",
		},
		{
			name: "tool completion without start",
			events: []agent.Event{
				{RunID: "run-1", Sequence: 1, Type: agent.EventRunStarted},
				{
					RunID: "run-1", Sequence: 2, Type: agent.EventToolCompleted,
					ToolCall:   &agent.ToolCall{ID: "call-1", Name: "read"},
					ToolResult: &agent.ToolResult{CallID: "call-1", Name: "read"},
				},
			},
			want: "does not match one pending Tool Call",
		},
		{
			name: "event after terminal",
			events: []agent.Event{
				{RunID: "run-1", Sequence: 1, Type: agent.EventRunStarted},
				{RunID: "run-1", Sequence: 2, Type: agent.EventRunCompleted},
				{RunID: "run-1", Sequence: 3, Type: agent.EventMessageDelta, Delta: "late"},
			},
			want: "after terminal state",
		},
		{
			name: "overlapping provider calls",
			events: []agent.Event{
				{RunID: "run-1", Sequence: 1, Type: agent.EventRunStarted},
				{
					RunID: "run-1", Sequence: 2, Type: agent.EventModelRequest,
					ProviderCall: 1, ProviderAttempt: 1,
					PrefixManifest: &agent.PrefixManifest{Provider: "test/provider"},
				},
				{
					RunID: "run-1", Sequence: 3, Type: agent.EventModelRequest,
					ProviderCall: 2, ProviderAttempt: 1,
					PrefixManifest: &agent.PrefixManifest{Provider: "test/provider"},
				},
			},
			want: "overlaps one pending Provider call",
		},
		{
			name: "completed run with pending execution",
			events: []agent.Event{
				{RunID: "run-1", Sequence: 1, Type: agent.EventRunStarted},
				{
					RunID: "run-1", Sequence: 2, Type: agent.EventModelRequest,
					ProviderCall: 1, ProviderAttempt: 1,
					PrefixManifest: &agent.PrefixManifest{Provider: "test/provider"},
				},
				{RunID: "run-1", Sequence: 3, Type: agent.EventRunCompleted},
			},
			want: "run.completed has pending execution",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := agent.BuildContextLedger(context.Background(), test.events)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("BuildContextLedger() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBuildContextLedgerPreservesInvalidRawMessageByteIdentity(t *testing.T) {
	t.Parallel()

	build := func(arguments string) agent.ContextLedger {
		events := []agent.Event{
			{RunID: "run-invalid", Sequence: 1, Type: agent.EventRunStarted},
			{RunID: "run-invalid", Sequence: 2, Type: agent.EventUserMessageAdded, Message: &agent.Message{Role: agent.RoleUser, Text: "start"}},
			{
				RunID: "run-invalid", Sequence: 3, Type: agent.EventModelRequest,
				ProviderCall: 1, ProviderAttempt: 1,
				PrefixManifest: &agent.PrefixManifest{Provider: "test/provider"},
			},
			{
				RunID: "run-invalid", Sequence: 4, Type: agent.EventMessageCompleted,
				Message: &agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{
					ID: "call-invalid", Name: "invalid", Arguments: json.RawMessage(arguments),
				}}},
			},
			{RunID: "run-invalid", Sequence: 5, Type: agent.EventRunCanceled, Error: "stop"},
		}
		ledger, err := agent.BuildContextLedger(context.Background(), events)
		if err != nil {
			t.Fatal(err)
		}
		return ledger
	}
	first := build("{")
	second := build("[")
	if first.Digest == second.Digest || first.Sources[3].ContentHash == second.Sources[3].ContentHash {
		t.Fatal("invalid raw Tool arguments lost exact byte identity")
	}
}

func TestBuildContextLedgerHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := agent.BuildContextLedger(ctx, nil); err == nil {
		t.Fatal("BuildContextLedger accepted canceled Context")
	}
}

func TestBuildContextLedgerRetainsNonUserRunInputOnlyAsSource(t *testing.T) {
	t.Parallel()

	events := []agent.Event{
		{RunID: "run-history", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "run-history", Sequence: 2, Type: agent.EventUserMessageAdded,
			Message: &agent.Message{Role: agent.RoleUser, Text: "first"},
		},
		{
			RunID: "run-history", Sequence: 3, Type: agent.EventUserMessageAdded,
			Message: &agent.Message{Role: agent.RoleAssistant, Text: "previous response"},
		},
		{
			RunID: "run-history", Sequence: 4, Type: agent.EventUserMessageAdded,
			Message: &agent.Message{Role: agent.RoleUser, Text: "follow up"},
		},
		{RunID: "run-history", Sequence: 5, Type: agent.EventRunCompleted},
	}
	ledger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Sources) != len(events) || len(ledger.Constraints) != 2 {
		t.Fatalf("source/Constraint count = %d/%d", len(ledger.Sources), len(ledger.Constraints))
	}
	if len(ledger.Tasks) != 1 || ledger.Tasks[0].InputCount != 3 {
		t.Fatalf("Task Ledger = %#v", ledger.Tasks)
	}
	if ledger.Constraints[0].Text != "first" || ledger.Constraints[1].Text != "follow up" {
		t.Fatalf("Constraint Ledger = %#v", ledger.Constraints)
	}
}

func TestBuildContextLedgerRejectsNonUserSteering(t *testing.T) {
	t.Parallel()

	_, err := agent.BuildContextLedger(context.Background(), []agent.Event{
		{RunID: "run-steering", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "run-steering", Sequence: 2, Type: agent.EventUserMessageAdded,
			UserMessageOrigin: agent.UserMessageOriginSteering,
			Message:           &agent.Message{Role: agent.RoleAssistant, Text: "not user input"},
		},
	})
	if !errors.Is(err, agent.ErrInvalidSteeringMessage) {
		t.Fatalf("BuildContextLedger() error = %v, want ErrInvalidSteeringMessage", err)
	}
}

func TestBuildContextLedgerKeepsReusedApprovalIDsDistinctAcrossRuns(t *testing.T) {
	t.Parallel()

	approval := func(approved bool) json.RawMessage {
		encoded, err := json.Marshal(struct {
			Approved bool `json:"approved"`
		}{Approved: approved})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	events := []agent.Event{
		{RunID: "run-first", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "run-first", Sequence: 2, Type: agent.EventRunWaiting,
			WaitRequest: &agent.WaitRequest{ID: "reused", Kind: agent.WaitKindApproval},
		},
		{
			RunID: "run-first", Sequence: 3, Type: agent.EventRunResumed,
			WaitResponse: &agent.WaitResponse{RequestID: "reused", Payload: approval(true)},
		},
		{RunID: "run-first", Sequence: 4, Type: agent.EventRunCompleted},
		{RunID: "run-second", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "run-second", Sequence: 2, Type: agent.EventRunWaiting,
			WaitRequest: &agent.WaitRequest{ID: "reused", Kind: agent.WaitKindApproval},
		},
		{
			RunID: "run-second", Sequence: 3, Type: agent.EventRunResumed,
			WaitResponse: &agent.WaitResponse{RequestID: "reused", Payload: approval(false)},
		},
		{RunID: "run-second", Sequence: 4, Type: agent.EventRunCompleted},
	}
	ledger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Tasks) != 2 || len(ledger.Policies) != 2 {
		t.Fatalf("Task/Policy count = %d/%d", len(ledger.Tasks), len(ledger.Policies))
	}
	if ledger.Policies[0].ID == ledger.Policies[1].ID ||
		ledger.Policies[0].Outcome != agent.PolicyLedgerAllowed ||
		ledger.Policies[1].Outcome != agent.PolicyLedgerDenied {
		t.Fatalf("Policy Ledger = %#v", ledger.Policies)
	}
}

func TestBuildContextLedgerResolvesPersistedApprovalInNewRun(t *testing.T) {
	t.Parallel()

	events := []agent.Event{
		{RunID: "run-waiting", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "run-waiting", Sequence: 2, Type: agent.EventRunWaiting,
			WaitRequest: &agent.WaitRequest{ID: "approval-resume", Kind: agent.WaitKindApproval},
		},
		{RunID: "run-resume", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "run-resume", Sequence: 2, Type: agent.EventRunResumed,
			WaitResponse: &agent.WaitResponse{
				RequestID: "approval-resume", Payload: json.RawMessage(`{"approved":true}`),
			},
		},
		{RunID: "run-resume", Sequence: 3, Type: agent.EventRunCompleted},
	}
	ledger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Policies) != 1 || ledger.Policies[0].Outcome != agent.PolicyLedgerAllowed ||
		len(ledger.Policies[0].Sources) != 2 || ledger.Policies[0].Sources[1].RunID != "run-resume" {
		t.Fatalf("Policy Ledger = %#v", ledger.Policies)
	}
}

func TestBuildContextLedgerResolvesLegacyApproval(t *testing.T) {
	t.Parallel()

	events := []agent.Event{
		{
			SessionID: "legacy", SessionRevision: 1, Type: agent.EventRunWaiting,
			WaitRequest: &agent.WaitRequest{ID: "legacy-approval", Kind: agent.WaitKindApproval},
		},
		{
			SessionID: "legacy", SessionRevision: 2, Type: agent.EventRunResumed,
			WaitResponse: &agent.WaitResponse{
				RequestID: "legacy-approval", Payload: json.RawMessage(`{"approved":true}`),
			},
		},
	}
	ledger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Policies) != 1 || ledger.Policies[0].Outcome != agent.PolicyLedgerAllowed ||
		len(ledger.Policies[0].Sources) != 2 {
		t.Fatalf("legacy Policy Ledger = %#v", ledger.Policies)
	}
}

func completeLedgerEvents() []agent.Event {
	const runID = "run-ledger"
	messageWithTool := agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{
		ID: "call-1", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"note.txt"}`),
	}}}
	call := messageWithTool.ToolCalls[0]
	manifest := func() *agent.PrefixManifest {
		return &agent.PrefixManifest{Version: 1, Provider: "test/provider", Model: "test-model", Epoch: "epoch"}
	}
	evidenceDigest := "sha256:" + strings.Repeat("e", 64)
	reasonDigest := "sha256:" + strings.Repeat("a", 64)
	events := []agent.Event{
		{Type: agent.EventRunStarted},
		{Type: agent.EventUserMessageAdded, Message: &agent.Message{Role: agent.RoleUser, Text: "change the file"}},
		{Type: agent.EventModelRequest, ProviderCall: 1, ProviderAttempt: 1, PrefixManifest: manifest()},
		{
			Type: agent.EventProviderRetry, ProviderCall: 1, ProviderAttempt: 1,
			ProviderRetry: &agent.ProviderRetryInfo{
				Error:       agent.ProviderErrorInfo{Code: providerbase.ErrorCodeRetryable, Attempt: 1},
				NextAttempt: 2,
			},
		},
		{Type: agent.EventModelRequest, ProviderCall: 2, ProviderAttempt: 2, PrefixManifest: manifest()},
		{Type: agent.EventMessageCompleted, Message: &messageWithTool},
		{Type: agent.EventToolStarted, ToolCall: &call},
		{
			Type: agent.EventRunWaiting,
			WaitRequest: &agent.WaitRequest{
				ID: "approval-1", Kind: agent.WaitKindApproval,
				Payload: json.RawMessage(`{"tool":"apply_patch","capabilities":["filesystem.write"]}`),
			},
		},
		{Type: agent.EventRunResumed, WaitResponse: &agent.WaitResponse{RequestID: "approval-1", Payload: json.RawMessage(`{"approved":true}`)}},
		{
			Type: agent.EventToolCompleted, ToolCall: &call,
			ToolResult: &agent.ToolResult{
				CallID: "call-1", Name: "apply_patch", Output: `{"changed":true}`,
				Policy: &agent.ToolPolicyDecision{
					Outcome: "allow", Capabilities: []string{"filesystem.write"}, ReasonDigest: reasonDigest,
				},
			},
		},
		{
			Type: agent.EventContextCompacted,
			ContextCompaction: &agent.ContextCompactionReport{Externalized: []agent.EvidenceObjectRef{{
				Digest: evidenceDigest, Bytes: 128, MediaType: "application/json",
			}}},
		},
		{Type: agent.EventModelRequest, ProviderCall: 3, ProviderAttempt: 1, PrefixManifest: manifest()},
		{Type: agent.EventMessageCompleted, Message: &agent.Message{Role: agent.RoleAssistant, Text: "done"}},
		{Type: agent.EventRunCompleted},
	}
	baseTime := time.Date(2026, time.August, 12, 0, 0, 0, 0, time.UTC)
	for index := range events {
		events[index].RunID = runID
		events[index].AgentID = "coding"
		events[index].SessionID = "ledger-session"
		events[index].Sequence = uint64(index + 1)
		events[index].SessionRevision = uint64(index + 1)
		events[index].Time = baseTime.Add(time.Duration(index) * time.Second)
	}
	return events
}

func cloneLedgerEvents(events []agent.Event) []agent.Event {
	encoded, _ := json.Marshal(events)
	var cloned []agent.Event
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}
