package agent_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/evidence"
)

func TestCompactingContextCompilerReportsDeterministicPreservation(t *testing.T) {
	t.Parallel()

	messages := []agent.Message{
		{Role: agent.RoleUser, Text: strings.Repeat("constraint-a ", 120)},
		{Role: agent.RoleAssistant, Text: strings.Repeat("decision-a ", 120)},
		{Role: agent.RoleUser, Text: strings.Repeat("constraint-b ", 120)},
		{Role: agent.RoleAssistant, Text: strings.Repeat("decision-b ", 120)},
		{Role: agent.RoleUser, Text: "latest constraint"},
	}
	events := safeCutCompilerEvents(messages, nil)
	ledger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	worldState := &agent.CurrentWorldState{Snapshot: agent.CurrentWorldStateSnapshot{
		Git: &agent.CurrentWorldGitState{Available: true, Changes: []agent.CurrentWorldGitChange{
			{Path: "first.go", Kind: "modified", WorktreeStatus: "M"},
			{Path: "second.go", Kind: "untracked", WorktreeStatus: "?"},
		}},
		Checks: []agent.CurrentWorldCheck{
			{Status: agent.CurrentWorldCheckFailed},
			{Status: agent.CurrentWorldCheckPassed},
		},
	}}
	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          4300,
		RecentMessages:         1,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     2600,
	}, evidence.NewMemoryObjectStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest:      agent.ModelRequest{Messages: messages},
		Ledger:            &ledger,
		Events:            events,
		CurrentWorldState: worldState,
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Compaction == nil || compiled.Compaction.Validation == nil {
		t.Fatalf("Context validation report = %#v", compiled.Compaction)
	}
	report := compiled.Compaction.Validation
	if !report.Passed || len(report.Failures) != 0 || report.Rollback != "" {
		t.Fatalf("Context validation outcome = %#v", report)
	}
	if report.ActiveConstraints != (agent.ContextPreservationCount{Required: 3, Preserved: 3}) ||
		report.ModifiedArtifacts != (agent.ContextPreservationCount{Required: 2, Preserved: 2}) ||
		report.FailingChecks != (agent.ContextPreservationCount{Required: 1, Preserved: 1}) ||
		report.PendingTools != (agent.ContextPreservationCount{}) ||
		report.Evidence.Required == 0 || report.Evidence != (agent.ContextPreservationCount{
		Required:       report.Evidence.Required,
		Preserved:      report.Evidence.Required,
		RequiredBytes:  report.Evidence.RequiredBytes,
		PreservedBytes: report.Evidence.RequiredBytes,
	}) {
		t.Fatalf("Context preservation counts = %#v", report)
	}
}

func TestCompactingContextCompilerFallsBackWhenStrategyDropsEvidence(t *testing.T) {
	t.Parallel()

	objects := evidence.NewMemoryObjectStore()
	strategy := checkpointStrategyFunc(func(ctx context.Context, request agent.CheckpointRequest) (agent.ContextCheckpoint, error) {
		checkpoint, err := (agent.DeterministicCheckpointStrategy{}).BuildCheckpoint(ctx, request)
		if err != nil {
			return agent.ContextCheckpoint{}, err
		}
		if len(checkpoint.Evidence) < 2 {
			t.Fatalf("test Checkpoint Evidence = %#v", checkpoint.Evidence)
		}
		checkpoint.Evidence = checkpoint.Evidence[:1]
		return checkpoint, nil
	})
	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          1800,
		RecentMessages:         1,
		EvidenceThresholdBytes: 500,
		EvidenceExcerptBytes:   100,
		CheckpointMaxBytes:     1500,
	}, objects, strategy)
	if err != nil {
		t.Fatal(err)
	}
	messages := []agent.Message{
		{Role: agent.RoleUser, Text: strings.Repeat("old ", 500)},
		{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "command"}}},
		{Role: agent.RoleTool, ToolCallID: "call-1", ToolName: "command", Text: strings.Repeat("output ", 500)},
		{Role: agent.RoleUser, Text: "continue"},
	}
	compiled, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages},
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Compaction == nil || compiled.Compaction.Validation == nil ||
		compiled.Compaction.Fallback != "checkpoint_strategy_validation_failed" ||
		!compiled.Compaction.Validation.Passed || compiled.Compaction.Validation.Evidence.Required != 2 ||
		compiled.Compaction.Validation.Evidence.Preserved != 2 || len(compiled.Checkpoint.Evidence) != 2 {
		t.Fatalf("Evidence validation fallback = %#v / %#v", compiled.Checkpoint, compiled.Compaction)
	}
}

func TestCompactingContextCompilerRollsBackToPreviousCheckpoint(t *testing.T) {
	t.Parallel()

	objects := evidence.NewMemoryObjectStore()
	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:            7500,
		RecentMessages:           1,
		EvidenceThresholdBytes:   4096,
		EvidenceExcerptBytes:     256,
		CheckpointMaxBytes:       5200,
		RebaseGenerationInterval: 1,
	}, objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	messages := validationConstraintMessages(18)
	events := safeCutCompilerEvents(messages, nil)
	ledger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	first, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages}, Ledger: &ledger, Events: events,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Checkpoint == nil || first.Checkpoint.SourceMessageCount != 16 ||
		first.Compaction == nil || first.Compaction.Validation == nil || !first.Compaction.Validation.Passed {
		t.Fatalf("initial validated Checkpoint = %#v", first)
	}
	second, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages}, Checkpoint: first.Checkpoint,
		Ledger: &ledger, Events: events,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second.Checkpoint, first.Checkpoint) || second.Compaction == nil ||
		second.Compaction.Reason != "validation_rollback" || second.Compaction.Validation == nil ||
		second.Compaction.Validation.Passed ||
		second.Compaction.Validation.Rollback != agent.ContextValidationRollbackPrevious ||
		second.Compaction.Validation.ActiveConstraints != (agent.ContextPreservationCount{Required: 18, Preserved: 17}) ||
		!reflect.DeepEqual(second.Compaction.Validation.Failures, []agent.ContextValidationFailure{
			agent.ContextValidationActiveConstraints,
		}) {
		t.Fatalf("Context validation rollback = %#v", second)
	}
}

func TestCompactingContextCompilerStopsWhenNoValidatedViewFits(t *testing.T) {
	t.Parallel()

	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          7500,
		RecentMessages:         1,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     5200,
	}, evidence.NewMemoryObjectStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	messages := validationConstraintMessages(25)
	events := safeCutCompilerEvents(messages, nil)
	ledger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	_, err = compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages}, Ledger: &ledger, Events: events,
	})
	if err == nil || !strings.Contains(err.Error(), string(agent.ContextValidationActiveConstraints)) {
		t.Fatalf("Compile() error = %v", err)
	}
}

func validationConstraintMessages(count int) []agent.Message {
	messages := make([]agent.Message, count)
	for index := range messages {
		messages[index] = agent.Message{
			Role: agent.RoleUser,
			Text: strings.Repeat(string(rune('a'+index%26)), 600),
		}
	}
	return messages
}
