package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/evidence"
)

func TestCompactingContextCompilerCreatesRebuildableCheckpoint(t *testing.T) {
	t.Parallel()

	objects := evidence.NewMemoryObjectStore()
	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          2600,
		RecentMessages:         2,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     1200,
	}, objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	messages := make([]agent.Message, 0, 12)
	for index := 0; index < 6; index++ {
		messages = append(messages,
			agent.Message{Role: agent.RoleUser, Text: strings.Repeat(string(rune('a'+index)), 600)},
			agent.Message{Role: agent.RoleAssistant, Text: strings.Repeat(string(rune('A'+index)), 600)},
		)
	}
	request := agent.ContextCompileRequest{
		SessionRevision: 42,
		ModelRequest: agent.ModelRequest{
			AgentID:   "worker",
			SessionID: "session-1",
			Messages:  messages,
		},
	}
	compiled, err := compiler.Compile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Checkpoint == nil || compiled.Compaction == nil || !compiled.Compaction.Applied {
		t.Fatalf("compiled Context = %#v", compiled)
	}
	checkpoint := compiled.Checkpoint
	if checkpoint.Generation != 1 || checkpoint.SessionRevision != 42 ||
		checkpoint.SourceMessageCount < 1 || checkpoint.SourceMessageCount >= len(messages) {
		t.Fatalf("Checkpoint = %#v", checkpoint)
	}
	if compiled.Compaction.CompiledBytes > 2600 || compiled.Compaction.CompiledBytes >= compiled.Compaction.OriginalBytes {
		t.Fatalf("Compaction report = %#v", compiled.Compaction)
	}
	if len(compiled.ModelRequest.Messages) != len(messages)-checkpoint.SourceMessageCount+1 ||
		!strings.Contains(compiled.ModelRequest.Messages[0].Text, "<qed_context_checkpoint>") {
		t.Fatalf("compiled Messages = %#v", compiled.ModelRequest.Messages)
	}
	if request.ModelRequest.Messages[0].Text != messages[0].Text {
		t.Fatal("Compile mutated the raw request")
	}
	if len(checkpoint.Evidence) == 0 {
		t.Fatal("Checkpoint has no exact source Evidence")
	}
	source, err := objects.GetObject(context.Background(), checkpoint.Evidence[0])
	if err != nil {
		t.Fatal(err)
	}
	var restored []agent.Message
	if err := json.Unmarshal(source, &restored); err != nil {
		t.Fatal(err)
	}
	if len(restored) != checkpoint.SourceMessageCount || restored[0].Text != messages[0].Text {
		t.Fatalf("restored source = %#v", restored)
	}
	if len(compiled.Segments) < 3 || compiled.Segments[2].Kind != agent.SegmentKindCheckpoint {
		t.Fatalf("Context Segments = %#v", compiled.Segments)
	}
}

func TestDeterministicCheckpointStrategyUsesOnlyActiveConstraintFacts(t *testing.T) {
	t.Parallel()

	firstID, err := agent.ConstraintFactID(agent.ContextLedgerEventRef{RunID: "run-first", Sequence: 2})
	if err != nil {
		t.Fatal(err)
	}
	events := []agent.Event{
		{RunID: "run-first", Sequence: 1, Type: agent.EventRunStarted},
		{RunID: "run-first", Sequence: 2, Type: agent.EventUserMessageAdded, Message: &agent.Message{Role: agent.RoleUser, Text: "use sqlite"}},
		{RunID: "run-first", Sequence: 3, Type: agent.EventRunCompleted},
		{RunID: "run-second", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "run-second", Sequence: 2, Type: agent.EventUserMessageAdded,
			Message: &agent.Message{Role: agent.RoleUser, Text: "use postgres"},
			FactDirective: &agent.FactLifecycleDirective{
				Action: agent.FactLifecycleSupersede, Targets: []string{firstID},
			},
		},
		{RunID: "run-second", Sequence: 3, Type: agent.EventRunCompleted},
	}
	ledger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	messages := []agent.Message{
		{Role: agent.RoleUser, Text: "use sqlite"},
		{Role: agent.RoleUser, Text: "use postgres"},
	}
	checkpoint, err := (agent.DeterministicCheckpointStrategy{}).BuildCheckpoint(context.Background(), agent.CheckpointRequest{
		Messages: messages, SourceHash: "source", Ledger: &ledger, MaxBytes: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Goal == nil || checkpoint.Goal.SourceMessage != 1 || len(checkpoint.Facts) != 0 {
		t.Fatalf("Fact-aware Checkpoint = %#v", checkpoint)
	}
}

func TestCompactingContextCompilerFiltersRetiredFactFromReusedCheckpointView(t *testing.T) {
	t.Parallel()

	objects := evidence.NewMemoryObjectStore()
	initialCompiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          1800,
		RecentMessages:         2,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     900,
	}, objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	messages := make([]agent.Message, 8)
	events := []agent.Event{{RunID: "run-old", Sequence: 1, Type: agent.EventRunStarted}}
	for index := range messages {
		messages[index] = agent.Message{
			Role: agent.RoleUser,
			Text: strings.Repeat(string(rune('a'+index)), 500),
		}
		message := messages[index]
		events = append(events, agent.Event{
			RunID: "run-old", Sequence: uint64(index + 2), Type: agent.EventUserMessageAdded,
			Message: &message,
		})
	}
	prefixLedger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := initialCompiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages},
		Ledger:       &prefixLedger,
	})
	if err != nil {
		t.Fatal(err)
	}
	if initial.Checkpoint == nil || initial.Checkpoint.Goal == nil {
		t.Fatalf("initial Checkpoint = %#v", initial.Checkpoint)
	}
	retiredSummary := initial.Checkpoint.Goal.Summary
	retiredSource := initial.Checkpoint.Goal.SourceMessage
	if retiredSource < 0 || retiredSource >= len(prefixLedger.Constraints) {
		t.Fatalf("retired source Message = %d", retiredSource)
	}
	targetID := prefixLedger.Constraints[retiredSource].ID

	checkpointEvent := agent.Event{
		RunID: "run-old", Sequence: uint64(len(events) + 1), Type: agent.EventContextCompacted,
		ContextCheckpoint: initial.Checkpoint,
		ContextCompaction: initial.Compaction,
	}
	events = append(events, checkpointEvent)
	events = append(events, agent.Event{
		RunID: "run-old", Sequence: uint64(len(events) + 1), Type: agent.EventRunCompleted,
	})
	events = append(events,
		agent.Event{RunID: "run-new", Sequence: 1, Type: agent.EventRunStarted},
		agent.Event{
			RunID: "run-new", Sequence: 2, Type: agent.EventUserMessageAdded,
			Message: &agent.Message{Role: agent.RoleUser, Text: "replacement"},
			FactDirective: &agent.FactLifecycleDirective{
				Action: agent.FactLifecycleSupersede, Targets: []string{targetID},
			},
		},
	)
	currentLedger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	messages = append(messages, agent.Message{Role: agent.RoleUser, Text: "replacement"})
	reuseCompiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          1 << 20,
		RecentMessages:         2,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     4096,
	}, objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := reuseCompiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages},
		Checkpoint:   initial.Checkpoint,
		Ledger:       &currentLedger,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reused.Checkpoint == nil || reused.Checkpoint.Goal == nil ||
		reused.Checkpoint.Goal.Summary != retiredSummary {
		t.Fatalf("persisted Checkpoint changed = %#v", reused.Checkpoint)
	}
	if len(reused.ModelRequest.Messages) == 0 ||
		strings.Contains(reused.ModelRequest.Messages[0].Text, retiredSummary) {
		t.Fatalf("reused model view retained retired Fact: %#v", reused.ModelRequest.Messages)
	}
}

func TestCompactingContextCompilerAdvancesCheckpointGeneration(t *testing.T) {
	t.Parallel()

	objects := evidence.NewMemoryObjectStore()
	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          1800,
		RecentMessages:         2,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     900,
	}, objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	var messages []agent.Message
	for index := 0; index < 8; index++ {
		messages = append(messages, agent.Message{
			Role: agent.RoleUser,
			Text: strings.Repeat(string(rune('a'+index)), 500),
		})
	}
	first, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Checkpoint == nil {
		t.Fatal("first compilation did not create a Checkpoint")
	}
	firstCount := first.Checkpoint.SourceMessageCount
	for index := 0; index < 4; index++ {
		messages = append(messages, agent.Message{Role: agent.RoleUser, Text: strings.Repeat("z", 500)})
	}
	second, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		Checkpoint:   first.Checkpoint,
		ModelRequest: agent.ModelRequest{Messages: messages},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Checkpoint == nil || second.Checkpoint.Generation != 2 ||
		second.Checkpoint.SourceMessageCount <= firstCount {
		t.Fatalf("second Checkpoint = %#v", second.Checkpoint)
	}
}

func TestCompactingContextCompilerDoesNotSplitToolTransaction(t *testing.T) {
	t.Parallel()

	objects := evidence.NewMemoryObjectStore()
	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          1400,
		RecentMessages:         1,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     800,
	}, objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	messages := []agent.Message{
		{Role: agent.RoleUser, Text: strings.Repeat("question", 100)},
		{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "lookup"}}},
		{Role: agent.RoleTool, ToolCallID: "call-1", ToolName: "lookup", Text: strings.Repeat("result", 100)},
		{Role: agent.RoleUser, Text: "current request"},
	}
	compiled, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages},
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Checkpoint == nil || compiled.Checkpoint.SourceMessageCount != 3 {
		t.Fatalf("Checkpoint = %#v", compiled.Checkpoint)
	}
	if len(compiled.ModelRequest.Messages) != 2 || compiled.ModelRequest.Messages[1].Text != "current request" {
		t.Fatalf("compiled Messages = %#v", compiled.ModelRequest.Messages)
	}
}

func TestCompactingContextCompilerExternalizesLargeToolOutput(t *testing.T) {
	t.Parallel()

	objects := evidence.NewMemoryObjectStore()
	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          4096,
		RecentMessages:         2,
		EvidenceThresholdBytes: 1000,
		EvidenceExcerptBytes:   100,
		CheckpointMaxBytes:     1024,
	}, objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	toolOutput := strings.Repeat("0123456789", 300)
	compiled, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: []agent.Message{
			{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "command"}}},
			{Role: agent.RoleTool, ToolCallID: "call-1", ToolName: "command", Text: toolOutput},
			{Role: agent.RoleUser, Text: "continue"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Checkpoint != nil || compiled.Compaction == nil || !compiled.Compaction.Applied ||
		len(compiled.Compaction.Externalized) != 1 {
		t.Fatalf("compiled Context = %#v", compiled)
	}
	if !strings.Contains(compiled.ModelRequest.Messages[1].Text, "[QED externalized Tool output]") {
		t.Fatalf("externalized message = %q", compiled.ModelRequest.Messages[1].Text)
	}
	loaded, err := objects.GetObject(context.Background(), compiled.Compaction.Externalized[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded) != toolOutput {
		t.Fatal("externalized Tool output did not round trip")
	}
}

func TestCompactingContextCompilerReportsEvidenceExternalizedBeforeCheckpoint(t *testing.T) {
	t.Parallel()

	objects := evidence.NewMemoryObjectStore()
	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          1600,
		RecentMessages:         1,
		EvidenceThresholdBytes: 1000,
		EvidenceExcerptBytes:   100,
		CheckpointMaxBytes:     900,
	}, objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: []agent.Message{
			{Role: agent.RoleUser, Text: strings.Repeat("old", 700)},
			{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "command"}}},
			{Role: agent.RoleTool, ToolCallID: "call-1", ToolName: "command", Text: strings.Repeat("output", 500)},
			{Role: agent.RoleUser, Text: "continue"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Checkpoint == nil || compiled.Checkpoint.SourceMessageCount != 3 || compiled.Compaction == nil ||
		len(compiled.Compaction.Externalized) != 2 {
		t.Fatalf("compiled Context = %#v", compiled)
	}
	mediaTypes := make(map[string]bool)
	for _, reference := range compiled.Compaction.Externalized {
		mediaTypes[reference.MediaType] = true
		if _, err := objects.GetObject(context.Background(), reference); err != nil {
			t.Fatal(err)
		}
	}
	if !mediaTypes["text/plain; charset=utf-8"] || !mediaTypes["application/vnd.qed.context-messages+json"] {
		t.Fatalf("externalized Evidence = %#v", compiled.Compaction.Externalized)
	}
}

func TestCompactingContextCompilerFallsBackFromInvalidStrategy(t *testing.T) {
	t.Parallel()

	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          1600,
		RecentMessages:         1,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     900,
	}, evidence.NewMemoryObjectStore(), checkpointStrategyFunc(func(context.Context, agent.CheckpointRequest) (agent.ContextCheckpoint, error) {
		return agent.ContextCheckpoint{}, errors.New("unavailable")
	}))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: []agent.Message{
			{Role: agent.RoleUser, Text: strings.Repeat("old", 500)},
			{Role: agent.RoleAssistant, Text: strings.Repeat("answer", 300)},
			{Role: agent.RoleUser, Text: "latest"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Checkpoint == nil || compiled.Compaction == nil || compiled.Compaction.Fallback == "" {
		t.Fatalf("fallback result = %#v", compiled)
	}
}

func TestCompactingContextCompilerRejectsMisclassifiedStrategyFacts(t *testing.T) {
	t.Parallel()

	strategy := checkpointStrategyFunc(func(ctx context.Context, request agent.CheckpointRequest) (agent.ContextCheckpoint, error) {
		checkpoint, err := (agent.DeterministicCheckpointStrategy{}).BuildCheckpoint(ctx, request)
		if err != nil {
			return agent.ContextCheckpoint{}, err
		}
		if len(checkpoint.Decisions) == 0 {
			return agent.ContextCheckpoint{}, errors.New("test requires an assistant decision")
		}
		goal := checkpoint.Decisions[0]
		checkpoint.Goal = &goal
		return checkpoint, nil
	})
	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          2200,
		RecentMessages:         1,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     1600,
	}, evidence.NewMemoryObjectStore(), strategy)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: []agent.Message{
			{Role: agent.RoleUser, Text: "old goal"},
			{Role: agent.RoleAssistant, Text: "old decision"},
			{Role: agent.RoleUser, Text: strings.Repeat("background", 400)},
			{Role: agent.RoleUser, Text: "latest"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Checkpoint == nil || compiled.Checkpoint.Goal == nil ||
		compiled.Checkpoint.Goal.SourceMessage != 2 || compiled.Compaction == nil ||
		compiled.Compaction.Fallback != "checkpoint_strategy_validation_failed" {
		t.Fatalf("Checkpoint = %#v, Compaction = %#v", compiled.Checkpoint, compiled.Compaction)
	}
}

func TestCompactingContextCompilerRejectsOverflowingExcerptPolicy(t *testing.T) {
	t.Parallel()

	maximumInt := int(^uint(0) >> 1)
	if _, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          4096,
		EvidenceThresholdBytes: 3,
		EvidenceExcerptBytes:   maximumInt,
		CheckpointMaxBytes:     512,
	}, evidence.NewMemoryObjectStore(), nil); err == nil {
		t.Fatal("overflowing Evidence excerpt policy was accepted")
	}
}

func TestCompactingContextCompilerRejectsMissingStrategyEvidence(t *testing.T) {
	t.Parallel()

	strategy := checkpointStrategyFunc(func(ctx context.Context, request agent.CheckpointRequest) (agent.ContextCheckpoint, error) {
		checkpoint, err := (agent.DeterministicCheckpointStrategy{}).BuildCheckpoint(ctx, request)
		if err != nil {
			return agent.ContextCheckpoint{}, err
		}
		checkpoint.Evidence = append(checkpoint.Evidence, agent.EvidenceObjectRef{
			Digest:    "sha256:" + strings.Repeat("f", 64),
			Bytes:     10,
			MediaType: "text/plain",
		})
		return checkpoint, nil
	})
	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          1800,
		RecentMessages:         1,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     1400,
	}, evidence.NewMemoryObjectStore(), strategy)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: []agent.Message{
			{Role: agent.RoleUser, Text: strings.Repeat("source", 500)},
			{Role: agent.RoleAssistant, Text: strings.Repeat("decision", 250)},
			{Role: agent.RoleUser, Text: "latest"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Checkpoint == nil || compiled.Compaction == nil ||
		compiled.Compaction.Fallback != "checkpoint_strategy_validation_failed" ||
		len(compiled.Checkpoint.Evidence) != 1 {
		t.Fatalf("Checkpoint fallback = %#v / %#v", compiled.Checkpoint, compiled.Compaction)
	}
}

type checkpointStrategyFunc func(context.Context, agent.CheckpointRequest) (agent.ContextCheckpoint, error)

func (strategy checkpointStrategyFunc) BuildCheckpoint(
	ctx context.Context,
	request agent.CheckpointRequest,
) (agent.ContextCheckpoint, error) {
	return strategy(ctx, request)
}
