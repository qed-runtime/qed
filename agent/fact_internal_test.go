package agent

import (
	"strings"
	"testing"
)

func TestValidateConstraintFactsRejectsSupersedesCycle(t *testing.T) {
	t.Parallel()

	firstSource := ContextLedgerEventRef{RunID: "run-cycle", Sequence: 1}
	secondSource := ContextLedgerEventRef{RunID: "run-cycle", Sequence: 2}
	firstTransition := ContextLedgerEventRef{RunID: "run-cycle", Sequence: 3}
	secondTransition := ContextLedgerEventRef{RunID: "run-cycle", Sequence: 4}
	firstID, err := ConstraintFactID(firstSource)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := ConstraintFactID(secondSource)
	if err != nil {
		t.Fatal(err)
	}
	entries := []ConstraintLedgerEntry{
		{
			ID: firstID, Kind: ConstraintLedgerUserInput, ContentHash: "sha256:" + strings.Repeat("a", 64),
			SourceMessage: 0, State: FactSuperseded, StateSource: firstTransition, Supersedes: []string{secondID},
			SupersededBy: secondID, Sources: []ContextLedgerEventRef{firstSource, firstTransition},
		},
		{
			ID: secondID, Kind: ConstraintLedgerUserInput, ContentHash: "sha256:" + strings.Repeat("b", 64),
			SourceMessage: 1, State: FactSuperseded, StateSource: secondTransition, Supersedes: []string{firstID},
			SupersededBy: firstID, Sources: []ContextLedgerEventRef{secondSource, secondTransition},
		},
	}
	if err := validateConstraintFacts(entries); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("validateConstraintFacts() error = %v", err)
	}
}

func TestCheckpointLifecycleViewOmitsRetiredFactsWithoutMutatingCheckpoint(t *testing.T) {
	t.Parallel()

	checkpoint := ContextCheckpoint{
		Version:            1,
		SourceMessageCount: 2,
		Goal:               &CheckpointFact{Kind: "user_input", Summary: "replacement", SourceMessage: 1},
		Facts: []CheckpointFact{
			{Kind: "user_input", Summary: "retired", SourceMessage: 0},
			{Kind: "user_input", Summary: "replacement", SourceMessage: 1},
		},
		Narrative: "original",
	}
	firstSource := ContextLedgerEventRef{RunID: "run-view", Sequence: 1}
	secondSource := ContextLedgerEventRef{RunID: "run-view", Sequence: 2}
	ledger := ContextLedger{Constraints: []ConstraintLedgerEntry{
		{SourceMessage: 0, State: FactSuperseded, Sources: []ContextLedgerEventRef{firstSource}},
		{SourceMessage: 1, State: FactActive, Sources: []ContextLedgerEventRef{secondSource}},
	}}
	view, err := checkpointLifecycleView(checkpoint, &ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Facts) != 1 || view.Facts[0].SourceMessage != 1 ||
		view.Goal == nil || view.Goal.SourceMessage != 1 {
		t.Fatalf("Checkpoint lifecycle view = %#v", view)
	}
	if len(checkpoint.Facts) != 2 || checkpoint.Narrative != "original" {
		t.Fatalf("checkpoint was mutated = %#v", checkpoint)
	}
}

func TestCloneContextLedgerIsolatesFactLifecycleSlices(t *testing.T) {
	t.Parallel()

	original := ContextLedger{Constraints: []ConstraintLedgerEntry{{
		ID:         "constraint_" + strings.Repeat("a", 64),
		Supersedes: []string{"constraint_" + strings.Repeat("b", 64)},
		Sources:    []ContextLedgerEventRef{{RunID: "run", Sequence: 1}},
	}}}
	cloned := cloneContextLedgerPointer(&original)
	cloned.Constraints[0].Supersedes[0] = "changed"
	cloned.Constraints[0].Sources[0].RunID = "changed"
	if original.Constraints[0].Supersedes[0] == "changed" || original.Constraints[0].Sources[0].RunID == "changed" {
		t.Fatalf("cloned Fact lifecycle aliases original = %#v", original.Constraints[0])
	}
}
