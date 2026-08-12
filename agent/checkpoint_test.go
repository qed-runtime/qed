package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
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
		checkpoint.LastRebaseGeneration != 1 ||
		checkpoint.SourceMessageCount < 1 || checkpoint.SourceMessageCount >= len(messages) {
		t.Fatalf("Checkpoint = %#v", checkpoint)
	}
	if !compiled.Compaction.Rebased || compiled.Compaction.RebaseReason != agent.ContextRebaseInitial {
		t.Fatalf("initial Compaction report = %#v", compiled.Compaction)
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

func TestCompactingContextCompilerRendersHierarchyAndReplaysLegacyCheckpoint(t *testing.T) {
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
	compiled, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages},
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := compiled.Checkpoint
	if checkpoint == nil || checkpoint.Version != agent.ContextCheckpointVersion || len(checkpoint.Layers) != 2 ||
		checkpoint.Layers[0].Level != agent.ContextCheckpointLevelTask ||
		checkpoint.Layers[1].Level != agent.ContextCheckpointLevelEpisode ||
		checkpoint.Layers[len(checkpoint.Layers)-1].SourceMessageEnd != checkpoint.SourceMessageCount {
		t.Fatalf("hierarchical Checkpoint = %#v", checkpoint)
	}
	view := decodeCheckpointModelView(t, compiled.ModelRequest.Messages[0].Text)
	levels := checkpointViewLevels(t, view)
	if !reflect.DeepEqual(levels, compiled.Compaction.ModelLevels) || !reflect.DeepEqual(levels, []agent.ContextCheckpointLevel{
		agent.ContextCheckpointLevelEpisode,
	}) {
		t.Fatalf("model levels = %#v, report = %#v", levels, compiled.Compaction.ModelLevels)
	}
	for _, field := range []string{"goal", "facts", "decisions", "executions", "ledger", "source_hash"} {
		if _, exists := view[field]; exists {
			t.Fatalf("hierarchical model view retained top-level %q: %#v", field, view)
		}
	}

	followUpMessages := append([]agent.Message(nil), messages...)
	followUpMessages = append(followUpMessages, agent.Message{Role: agent.RoleUser, Text: "follow up"})
	events := safeCutCompilerEvents(messages, nil)
	events = append(events, agent.Event{
		RunID: "run-safe-cut-compiler", Sequence: uint64(len(events) + 1), Type: agent.EventRunCompleted,
	})
	followUp := followUpMessages[len(followUpMessages)-1]
	events = append(events,
		agent.Event{RunID: "run-follow-up", Sequence: 1, Type: agent.EventRunStarted},
		agent.Event{RunID: "run-follow-up", Sequence: 2, Type: agent.EventUserMessageAdded, Message: &followUp},
	)
	ledger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
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
	followUpCompiled, err := reuseCompiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: followUpMessages},
		Checkpoint:   checkpoint,
		Ledger:       &ledger,
		Events:       events,
	})
	if err != nil {
		t.Fatal(err)
	}
	followUpView := decodeCheckpointModelView(t, followUpCompiled.ModelRequest.Messages[0].Text)
	if got := checkpointViewLevels(t, followUpView); !reflect.DeepEqual(got, []agent.ContextCheckpointLevel{
		agent.ContextCheckpointLevelSessionSynopsis,
	}) || !reflect.DeepEqual(got, followUpCompiled.Compaction.ModelLevels) {
		t.Fatalf("follow-up model levels = %#v / %#v", got, followUpCompiled.Compaction)
	}
	if followUpCompiled.Checkpoint == nil || followUpCompiled.Checkpoint.Layers[0].Level != agent.ContextCheckpointLevelTask {
		t.Fatalf("follow-up mutated stored hierarchy = %#v", followUpCompiled.Checkpoint)
	}

	legacy := *checkpoint
	legacy.Version = 1
	legacy.Layers = nil
	reused, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages},
		Checkpoint:   &legacy,
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyView := decodeCheckpointModelView(t, reused.ModelRequest.Messages[0].Text)
	if version, ok := legacyView["version"].(float64); !ok || version != 1 {
		t.Fatalf("legacy model view version = %#v", legacyView["version"])
	}
	if _, exists := legacyView["layers"]; exists || len(reused.Compaction.ModelLevels) != 0 {
		t.Fatalf("legacy model view or report gained hierarchy = %#v / %#v", legacyView, reused.Compaction)
	}

	tampered := *checkpoint
	tampered.Layers = append([]agent.ContextCheckpointLayer(nil), checkpoint.Layers...)
	tampered.Layers[0].SourceMessageEnd = 0
	_, err = compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages},
		Checkpoint:   &tampered,
	})
	if err == nil || !strings.Contains(err.Error(), "hierarchy") {
		t.Fatalf("tampered hierarchy error = %v", err)
	}
}

func TestCompactingContextCompilerSuppliesRawEventsForInitialRebase(t *testing.T) {
	t.Parallel()

	messages := []agent.Message{
		{Role: agent.RoleUser, Text: strings.Repeat("old", 500)},
		{Role: agent.RoleAssistant, Text: strings.Repeat("answer", 300)},
		{Role: agent.RoleUser, Text: "latest"},
	}
	events := safeCutCompilerEvents(messages, nil)
	ledger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	var observed bool
	strategy := checkpointStrategyFunc(func(ctx context.Context, request agent.CheckpointRequest) (agent.ContextCheckpoint, error) {
		observed = true
		if request.Mode != agent.CheckpointBuildRawRebase || request.RebaseReason != agent.ContextRebaseInitial ||
			request.Generation != 1 || request.Previous != nil || len(request.Events) != len(events) {
			t.Fatalf("initial Checkpoint request = %#v", request)
		}
		request.Events[0].Type = agent.EventRunFailed
		return (agent.DeterministicCheckpointStrategy{}).BuildCheckpoint(ctx, request)
	})
	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          1600,
		RecentMessages:         1,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     900,
	}, evidence.NewMemoryObjectStore(), strategy)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages},
		Ledger:       &ledger,
		Events:       events,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !observed || events[0].Type != agent.EventRunStarted || compiled.Compaction == nil ||
		!compiled.Compaction.Rebased || compiled.Compaction.RebaseReason != agent.ContextRebaseInitial {
		t.Fatalf("initial Raw Event Rebase = %#v", compiled)
	}
}

func TestCompactingContextCompilerReservesCurrentWorldStateBytes(t *testing.T) {
	t.Parallel()

	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          900,
		RecentMessages:         1,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   100,
		CheckpointMaxBytes:     512,
	}, evidence.NewMemoryObjectStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	request := agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: []agent.Message{{Role: agent.RoleUser, Text: "continue"}}},
	}
	if _, err := compiler.Compile(context.Background(), request); err != nil {
		t.Fatalf("Compile() without Current World State error = %v", err)
	}
	state := &agent.CurrentWorldState{
		Version:  1,
		Digest:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Snapshot: agent.CurrentWorldStateSnapshot{FilesAvailable: true},
	}
	for index := 0; index < 12; index++ {
		state.Snapshot.Files = append(state.Snapshot.Files, agent.CurrentWorldFile{
			Path:   strings.Repeat(string(rune('a'+index)), 24) + ".txt",
			Status: agent.CurrentWorldFilePresent,
			Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Bytes:  1,
		})
	}
	request.CurrentWorldState = state
	if _, err := compiler.Compile(context.Background(), request); err == nil || !strings.Contains(err.Error(), "no safe Checkpoint boundary") {
		t.Fatalf("Compile() with oversized Current World State error = %v", err)
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

func TestCompactingContextCompilerRebasesInconsistentCheckpointFact(t *testing.T) {
	t.Parallel()

	objects := evidence.NewMemoryObjectStore()
	initialCompiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          3600,
		RecentMessages:         2,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     2600,
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
	if reused.Checkpoint == nil || reused.Checkpoint.Generation != initial.Checkpoint.Generation+1 ||
		reused.Checkpoint.LastRebaseGeneration != reused.Checkpoint.Generation ||
		reused.Checkpoint.SourceMessageCount != initial.Checkpoint.SourceMessageCount ||
		reused.Checkpoint.Goal == nil || reused.Checkpoint.Goal.Summary == retiredSummary {
		t.Fatalf("rebased Checkpoint = %#v", reused.Checkpoint)
	}
	if reused.Compaction == nil || reused.Compaction.Reason != "raw_event_rebase" ||
		!reused.Compaction.Rebased || reused.Compaction.RebaseReason != agent.ContextRebaseCheckpointInconsistent {
		t.Fatalf("Raw Event Rebase report = %#v", reused.Compaction)
	}
	if len(reused.ModelRequest.Messages) == 0 ||
		strings.Contains(reused.ModelRequest.Messages[0].Text, retiredSummary) {
		t.Fatalf("reused model view retained retired Fact: %#v", reused.ModelRequest.Messages)
	}
}

func TestCompactingContextCompilerRebasesAfterFactLifecycleChange(t *testing.T) {
	t.Parallel()

	objects := evidence.NewMemoryObjectStore()
	initialCompiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          3600,
		RecentMessages:         1,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     2600,
	}, objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	messages := make([]agent.Message, 8)
	for index := range messages {
		messages[index] = agent.Message{Role: agent.RoleUser, Text: strings.Repeat(string(rune('a'+index)), 500)}
	}
	events := safeCutCompilerEvents(messages, nil)
	prefixLedger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := initialCompiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages},
		Ledger:       &prefixLedger,
		Events:       events,
	})
	if err != nil {
		t.Fatal(err)
	}
	if initial.Checkpoint == nil || initial.Compaction == nil {
		t.Fatalf("initial Checkpoint = %#v", initial)
	}
	var target string
	targetSource := -1
	for _, constraint := range prefixLedger.Constraints {
		if constraint.SourceMessage >= initial.Checkpoint.SourceMessageCount {
			target = constraint.ID
			targetSource = constraint.SourceMessage
			break
		}
	}
	if target == "" {
		t.Fatalf("no raw tail Fact after source %d", initial.Checkpoint.SourceMessageCount)
	}
	sequence := uint64(len(events) + 1)
	events = append(events, agent.Event{
		RunID: "run-safe-cut-compiler", Sequence: sequence, Type: agent.EventContextCompacted,
		ContextCheckpoint: initial.Checkpoint, ContextCompaction: initial.Compaction,
	})
	sequence++
	resolution := agent.Message{Role: agent.RoleUser, Text: "the tail requirement is resolved"}
	events = append(events, agent.Event{
		RunID: "run-safe-cut-compiler", Sequence: sequence, Type: agent.EventUserMessageAdded,
		Message: &resolution,
		FactDirective: &agent.FactLifecycleDirective{
			Action: agent.FactLifecycleResolve, Targets: []string{target},
		},
	})
	messages = append(messages, resolution)
	currentLedger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	var requestMode agent.CheckpointBuildMode
	var requestReason agent.ContextRebaseReason
	var requestEventCount int
	rebaseCompiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          1 << 20,
		RecentMessages:         1,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     4096,
	}, objects, checkpointStrategyFunc(func(ctx context.Context, request agent.CheckpointRequest) (agent.ContextCheckpoint, error) {
		requestMode = request.Mode
		requestReason = request.RebaseReason
		requestEventCount = len(request.Events)
		if request.Previous != nil {
			t.Fatal("Raw Event Rebase exposed the previous Checkpoint")
		}
		return (agent.DeterministicCheckpointStrategy{}).BuildCheckpoint(ctx, request)
	}))
	if err != nil {
		t.Fatal(err)
	}
	rebased, err := rebaseCompiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages},
		Checkpoint:   initial.Checkpoint,
		Ledger:       &currentLedger,
		Events:       events,
	})
	if err != nil {
		t.Fatal(err)
	}
	if requestMode != agent.CheckpointBuildRawRebase || requestReason != agent.ContextRebaseFactLifecycleChanged ||
		requestEventCount != len(events) || rebased.Compaction == nil || !rebased.Compaction.Rebased ||
		rebased.Compaction.RebaseReason != agent.ContextRebaseFactLifecycleChanged {
		t.Fatalf("Fact lifecycle Rebase = request %q/%q/%d, compiled %#v", requestMode, requestReason, requestEventCount, rebased)
	}
	if rebased.Checkpoint == nil || rebased.Checkpoint.SourceMessageCount <= initial.Checkpoint.SourceMessageCount ||
		rebased.Checkpoint.SourceMessageCount <= targetSource ||
		len(rebased.ModelRequest.Messages) == 0 ||
		strings.Contains(rebased.ModelRequest.Messages[0].Text, messages[targetSource].Text) {
		t.Fatalf("Fact lifecycle Rebase retained retired raw tail Fact: %#v", rebased)
	}
}

func TestCompactingContextCompilerAdvancesCheckpointGeneration(t *testing.T) {
	t.Parallel()

	objects := evidence.NewMemoryObjectStore()
	type observedBuild struct {
		mode        agent.CheckpointBuildMode
		reason      agent.ContextRebaseReason
		generation  uint64
		hasPrevious bool
	}
	var builds []observedBuild
	strategy := checkpointStrategyFunc(func(ctx context.Context, request agent.CheckpointRequest) (agent.ContextCheckpoint, error) {
		builds = append(builds, observedBuild{
			mode: request.Mode, reason: request.RebaseReason, generation: request.Generation,
			hasPrevious: request.Previous != nil,
		})
		return (agent.DeterministicCheckpointStrategy{}).BuildCheckpoint(ctx, request)
	})
	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:            1800,
		RecentMessages:           2,
		EvidenceThresholdBytes:   4096,
		EvidenceExcerptBytes:     256,
		CheckpointMaxBytes:       900,
		RebaseGenerationInterval: 2,
	}, objects, strategy)
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
		second.Checkpoint.LastRebaseGeneration != 1 || second.Checkpoint.SourceMessageCount <= firstCount {
		t.Fatalf("second Checkpoint = %#v", second.Checkpoint)
	}
	third, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		Checkpoint:   second.Checkpoint,
		ModelRequest: agent.ModelRequest{Messages: messages},
	})
	if err != nil {
		t.Fatal(err)
	}
	if third.Checkpoint == nil || third.Checkpoint.Generation != 3 ||
		third.Checkpoint.LastRebaseGeneration != 3 ||
		third.Checkpoint.SourceMessageCount != second.Checkpoint.SourceMessageCount ||
		third.Compaction == nil || third.Compaction.Reason != "raw_event_rebase" ||
		third.Compaction.RebaseReason != agent.ContextRebaseGenerationInterval {
		t.Fatalf("third Checkpoint = %#v / %#v", third.Checkpoint, third.Compaction)
	}
	wantBuilds := []observedBuild{
		{mode: agent.CheckpointBuildRawRebase, reason: agent.ContextRebaseInitial, generation: 1},
		{mode: agent.CheckpointBuildIncremental, generation: 2, hasPrevious: true},
		{mode: agent.CheckpointBuildRawRebase, reason: agent.ContextRebaseGenerationInterval, generation: 3},
	}
	uniqueBuilds := make([]observedBuild, 0, len(builds))
	for _, build := range builds {
		if len(uniqueBuilds) == 0 || uniqueBuilds[len(uniqueBuilds)-1] != build {
			uniqueBuilds = append(uniqueBuilds, build)
		}
	}
	if len(uniqueBuilds) != len(wantBuilds) {
		t.Fatalf("Checkpoint builds = %#v", builds)
	}
	for index := range wantBuilds {
		if uniqueBuilds[index] != wantBuilds[index] {
			t.Fatalf("Checkpoint build %d = %#v, want %#v", index, uniqueBuilds[index], wantBuilds[index])
		}
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

func TestCompactingContextCompilerUsesEventAwareMutationVerificationBoundary(t *testing.T) {
	t.Parallel()

	newCompiler := func(t *testing.T) *agent.CompactingContextCompiler {
		t.Helper()
		compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
			MaxInputBytes:          2600,
			RecentMessages:         3,
			EvidenceThresholdBytes: 4096,
			EvidenceExcerptBytes:   256,
			CheckpointMaxBytes:     1400,
		}, evidence.NewMemoryObjectStore(), nil)
		if err != nil {
			t.Fatal(err)
		}
		return compiler
	}
	messages := []agent.Message{
		{Role: agent.RoleUser, Text: strings.Repeat("request", 180)},
		{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "edit-1", Name: "edit"}}},
		{Role: agent.RoleTool, ToolCallID: "edit-1", ToolName: "edit", Text: strings.Repeat("changed", 180)},
		{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "read-1", Name: "read"}}},
		{Role: agent.RoleTool, ToolCallID: "read-1", ToolName: "read", Text: strings.Repeat("current", 180)},
		{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "verify-1", Name: "verify"}}},
		{Role: agent.RoleTool, ToolCallID: "verify-1", ToolName: "verify", Text: "passed"},
		{Role: agent.RoleUser, Text: "next request"},
	}
	legacyCompiler := newCompiler(t)
	legacy, err := legacyCompiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages},
	})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Checkpoint == nil || legacy.Checkpoint.SourceMessageCount != 5 {
		t.Fatalf("legacy Checkpoint = %#v", legacy.Checkpoint)
	}

	events := safeCutCompilerEvents(messages, map[string]agent.ContextOperationKind{
		"edit-1":   agent.ContextOperationMutation,
		"verify-1": agent.ContextOperationVerification,
	})
	ledger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := newCompiler(t).Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages},
		Ledger:       &ledger,
		Events:       events,
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Checkpoint == nil || compiled.Checkpoint.SourceMessageCount != 7 {
		t.Fatalf("event-aware Checkpoint = %#v", compiled.Checkpoint)
	}
	_, err = legacyCompiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: messages},
		Checkpoint:   legacy.Checkpoint,
		Ledger:       &ledger,
		Events:       events,
	})
	if err == nil || !strings.Contains(err.Error(), "active Context Checkpoint splits a protected transaction") {
		t.Fatalf("unsafe active Checkpoint error = %v", err)
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

func safeCutCompilerEvents(
	messages []agent.Message,
	operations map[string]agent.ContextOperationKind,
) []agent.Event {
	const runID = "run-safe-cut-compiler"
	sequence := uint64(0)
	providerCall := 0
	events := make([]agent.Event, 0, len(messages)*2)
	calls := make(map[string]agent.ToolCall)
	emit := func(event agent.Event) {
		sequence++
		event.RunID = runID
		event.Sequence = sequence
		events = append(events, event)
	}
	emit(agent.Event{Type: agent.EventRunStarted})
	for index := range messages {
		message := messages[index]
		switch message.Role {
		case agent.RoleUser:
			emit(agent.Event{Type: agent.EventUserMessageAdded, Message: &message})
		case agent.RoleAssistant:
			providerCall++
			emit(agent.Event{
				Type:            agent.EventModelRequest,
				ProviderCall:    providerCall,
				ProviderAttempt: 1,
				PrefixManifest: &agent.PrefixManifest{
					Version: 1, Provider: "safe-cut/provider", Epoch: "safe-cut",
				},
			})
			emit(agent.Event{Type: agent.EventMessageCompleted, Message: &message})
			for _, call := range message.ToolCalls {
				calls[call.ID] = call
			}
		case agent.RoleTool:
			call := calls[message.ToolCallID]
			emit(agent.Event{Type: agent.EventToolStarted, ToolCall: &call})
			result := agent.ToolResult{
				CallID: message.ToolCallID, Name: message.ToolName, Output: message.Text,
			}
			if kind := operations[message.ToolCallID]; kind != "" {
				result.ContextOperation = &agent.ContextOperation{Kind: kind}
			}
			emit(agent.Event{
				Type: agent.EventToolCompleted, Message: &message, ToolCall: &call, ToolResult: &result,
			})
		}
	}
	return events
}

func (strategy checkpointStrategyFunc) BuildCheckpoint(
	ctx context.Context,
	request agent.CheckpointRequest,
) (agent.ContextCheckpoint, error) {
	return strategy(ctx, request)
}

func decodeCheckpointModelView(t *testing.T, rendered string) map[string]any {
	t.Helper()
	start := strings.Index(rendered, "{")
	end := strings.LastIndex(rendered, "\n</qed_context_checkpoint>")
	if start < 0 || end <= start {
		t.Fatalf("rendered Checkpoint envelope = %q", rendered)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(rendered[start:end]), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func checkpointViewLevels(t *testing.T, view map[string]any) []agent.ContextCheckpointLevel {
	t.Helper()
	values, ok := view["layers"].([]any)
	if !ok {
		t.Fatalf("Checkpoint model layers = %#v", view["layers"])
	}
	levels := make([]agent.ContextCheckpointLevel, 0, len(values))
	for _, value := range values {
		layer, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("Checkpoint model layer = %#v", value)
		}
		level, ok := layer["level"].(string)
		if !ok {
			t.Fatalf("Checkpoint model layer level = %#v", layer)
		}
		levels = append(levels, agent.ContextCheckpointLevel(level))
	}
	return levels
}
