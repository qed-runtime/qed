package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/evidence"
)

func TestContextCommandsProvideContentFreeTextAndJSON(t *testing.T) {
	t.Parallel()

	storeRoot := t.TempDir()
	store, err := evidence.NewJSONStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	bundle := contextCommandBundle("run-context-cli")
	if err := store.Save(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	followUpBundle := contextCommandBundle("run-context-cli-follow-up")
	if err := store.Save(context.Background(), followUpBundle); err != nil {
		t.Fatal(err)
	}

	assertCommand := func(arguments []string) string {
		t.Helper()
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := run(context.Background(), arguments, &stdout, &stderr)
		if exitCode != 0 {
			t.Fatalf("command %q exit/stderr = %d/%q", arguments, exitCode, stderr.String())
		}
		for _, forbidden := range []string{"SECRET_CHECKPOINT_CONTENT", strings.Repeat("a", 64)} {
			if strings.Contains(stdout.String(), forbidden) {
				t.Fatalf("command %q exposed %q: %s", arguments, forbidden, stdout.String())
			}
		}
		return stdout.String()
	}

	inspectText := assertCommand([]string{
		"context", "inspect", bundle.Run.ID, "--store", storeRoot,
	})
	for _, want := range []string{
		"Context events: 2",
		"Checkpoint generations: 1",
		"Rollbacks: 1",
		"Post-compaction rereads: unavailable",
		"sequence=3 effective_generation=1 candidate_generation=2",
	} {
		if !strings.Contains(inspectText, want) {
			t.Fatalf("context inspect output %q does not contain %q", inspectText, want)
		}
	}

	inspectJSON := assertCommand([]string{
		"context", "inspect", bundle.Run.ID, "--store", storeRoot, "--output", "json",
	})
	var report agent.ContextReport
	if err := json.Unmarshal([]byte(inspectJSON), &report); err != nil {
		t.Fatal(err)
	}
	if report.RunID != bundle.Run.ID || len(report.Snapshots) != 2 ||
		report.Metrics.PostCompactionRereadCount != nil {
		t.Fatalf("Context report JSON = %#v", report)
	}

	explainText := assertCommand([]string{
		"context", "explain", bundle.Run.ID, "--store", storeRoot,
	})
	for _, want := range []string{
		"Event sequence: 3",
		"Reason: validation_rollback",
		"Rollback: previous_checkpoint",
		"Fallback: checkpoint_strategy_validation_failed",
	} {
		if !strings.Contains(explainText, want) {
			t.Fatalf("context explain output %q does not contain %q", explainText, want)
		}
	}

	explainJSON := assertCommand([]string{
		"context", "explain", bundle.Run.ID + "@2", "--store", storeRoot, "--output", "json",
	})
	var snapshot agent.ContextSnapshot
	if err := json.Unmarshal([]byte(explainJSON), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != agent.ContextReportVersion || snapshot.EventSequence != 2 || !snapshot.PublishedCheckpoint ||
		snapshot.CheckpointGeneration == nil || *snapshot.CheckpointGeneration != 1 {
		t.Fatalf("Context snapshot JSON = %#v", snapshot)
	}

	diffText := assertCommand([]string{
		"context", "diff",
		"--before", bundle.Run.ID + "@2",
		"--after", bundle.Run.ID + "@3",
		"--store", storeRoot,
	})
	for _, want := range []string{
		"Before: run-context-cli@2",
		"After: run-context-cli@3",
		"Original bytes: before=100 after=120 delta=+20",
		"Preservation active_constraints: required_delta=+0 preserved_delta=-1",
	} {
		if !strings.Contains(diffText, want) {
			t.Fatalf("context diff output %q does not contain %q", diffText, want)
		}
	}

	diffJSON := assertCommand([]string{
		"context", "diff",
		"--before", bundle.Run.ID + "@2",
		"--after", bundle.Run.ID + "@3",
		"--store", storeRoot,
		"--output", "json",
	})
	var diff agent.ContextSnapshotDiff
	if err := json.Unmarshal([]byte(diffJSON), &diff); err != nil {
		t.Fatal(err)
	}
	if diff.Before.EventSequence != 2 || diff.After.EventSequence != 3 ||
		diff.Delta.CompiledBytes.Delta != 10 {
		t.Fatalf("Context diff JSON = %#v", diff)
	}

	crossRunDiff := assertCommand([]string{
		"context", "diff",
		"--before", bundle.Run.ID + "@2",
		"--after", followUpBundle.Run.ID + "@3",
		"--store", storeRoot,
	})
	if !strings.Contains(crossRunDiff, "After: run-context-cli-follow-up@3") {
		t.Fatalf("cross-Run context diff output = %q", crossRunDiff)
	}
}

func TestContextCommandsRejectMalformedSelectorAndBundle(t *testing.T) {
	t.Parallel()

	storeRoot := t.TempDir()
	store, err := evidence.NewJSONStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	bundle := contextCommandBundle("run-context-bad")
	bundle.Events[1].Sequence = 3
	if err := store.Save(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		arguments []string
		want      string
	}{
		{
			arguments: []string{"context", "explain", "run-context-bad@zero", "--store", storeRoot},
			want:      "Context selector must be",
		},
		{
			arguments: []string{"context", "inspect", bundle.Run.ID, "--store", storeRoot},
			want:      "Event sequence = 3, want 2",
		},
	}
	for _, test := range tests {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if exitCode := run(context.Background(), test.arguments, &stdout, &stderr); exitCode == 0 {
			t.Fatalf("command %q exit = 0, stdout = %q", test.arguments, stdout.String())
		}
		if !strings.Contains(stderr.String(), test.want) {
			t.Fatalf("command %q stderr %q does not contain %q", test.arguments, stderr.String(), test.want)
		}
	}
}

func TestContextInspectSupportsUnreportedValidationEvent(t *testing.T) {
	t.Parallel()

	storeRoot := t.TempDir()
	store, err := evidence.NewJSONStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	bundle := evidence.Bundle{
		Version: 1,
		Run: evidence.RunDescriptor{
			ID: "run-context-legacy", Status: agent.RunStatusCompleted,
		},
		Events: []agent.Event{
			{RunID: "run-context-legacy", Sequence: 1, Type: agent.EventRunStarted},
			{
				RunID: "run-context-legacy", Sequence: 2, Type: agent.EventContextCompacted,
				ContextCheckpoint: &agent.ContextCheckpoint{Generation: 4, SourceMessageCount: 2},
				ContextCompaction: &agent.ContextCompactionReport{
					Applied: true, Reason: "legacy", OriginalBytes: 60, CompiledBytes: 30,
					SourceMessageCount: 2, RecentMessageCount: 1,
				},
			},
			{RunID: "run-context-legacy", Sequence: 3, Type: agent.EventRunCompleted},
		},
	}
	if err := store.Save(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{
		"context", "inspect", bundle.Run.ID, "--store", storeRoot,
	}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exit/stderr = %d/%q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Validation: reports=0 failures=0 unreported=1") {
		t.Fatalf("legacy context inspect output = %q", stdout.String())
	}
}

func contextCommandBundle(runID string) evidence.Bundle {
	digest := "sha256:" + strings.Repeat("a", 64)
	return evidence.Bundle{
		Version: 1,
		Run: evidence.RunDescriptor{
			ID: runID, Status: agent.RunStatusCompleted,
		},
		Events: []agent.Event{
			{RunID: runID, Sequence: 1, Type: agent.EventRunStarted},
			{
				RunID: runID, Sequence: 2, Type: agent.EventContextCompacted,
				ContextCheckpoint: &agent.ContextCheckpoint{
					Generation: 1, LastRebaseGeneration: 1, SourceMessageCount: 2,
					Narrative: "SECRET_CHECKPOINT_CONTENT",
				},
				ContextCompaction: &agent.ContextCompactionReport{
					Applied: true, Reason: "raw_event_rebase", OriginalBytes: 100, CompiledBytes: 50,
					SourceMessageCount: 2, RecentMessageCount: 2,
					Externalized: []agent.EvidenceObjectRef{{
						Digest: digest, MediaType: "application/json", Bytes: 10,
					}},
					Rebased: true, RebaseReason: agent.ContextRebaseInitial,
					Validation: &agent.ContextValidationReport{
						Version: agent.ContextValidationVersion, CandidateGeneration: 1,
						CandidateSourceMessageCount: 2, Passed: true,
						ActiveConstraints: agent.ContextPreservationCount{Required: 2, Preserved: 2},
					},
				},
			},
			{
				RunID: runID, Sequence: 3, Type: agent.EventContextCompacted,
				ContextCompaction: &agent.ContextCompactionReport{
					Reason: "validation_rollback", OriginalBytes: 120, CompiledBytes: 60,
					SourceMessageCount: 2, RecentMessageCount: 3,
					Fallback: "checkpoint_strategy_validation_failed",
					Validation: &agent.ContextValidationReport{
						Version: agent.ContextValidationVersion, CandidateGeneration: 2,
						CandidateSourceMessageCount: 4,
						ActiveConstraints:           agent.ContextPreservationCount{Required: 2, Preserved: 1},
						Failures:                    []agent.ContextValidationFailure{agent.ContextValidationActiveConstraints},
						Rollback:                    agent.ContextValidationRollbackPrevious,
					},
				},
			},
			{RunID: runID, Sequence: 4, Type: agent.EventRunCompleted},
		},
	}
}
