package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
)

func TestBuildContextReportAggregatesValidatedTimeline(t *testing.T) {
	t.Parallel()

	report, err := agent.BuildContextReport(context.Background(), "run-context", contextReportEvents())
	if err != nil {
		t.Fatal(err)
	}
	if report.Version != agent.ContextReportVersion || report.RunID != "run-context" || len(report.Snapshots) != 3 {
		t.Fatalf("Context report identity = %#v", report)
	}
	metrics := report.Metrics
	if metrics.CompactionCount != 3 || metrics.CheckpointGenerationCount != 2 ||
		metrics.LatestCheckpointGeneration == nil || *metrics.LatestCheckpointGeneration != 2 ||
		metrics.FullRebaseCount != 1 ||
		metrics.RollbackCount != 1 || metrics.CustomFallbackCount != 1 ||
		metrics.ValidationCount != 3 || metrics.ValidationFailureCount != 1 ||
		metrics.UnreportedValidationCount != 0 {
		t.Fatalf("Context metrics counts = %#v", metrics)
	}
	if metrics.OriginalBytes != 380 || metrics.CompiledBytes != 154 ||
		metrics.ExternalizedObjects != 2 || metrics.ExternalizedBytes != 30 {
		t.Fatalf("Context metrics sizes = %#v", metrics)
	}
	if metrics.CompressionRatio == nil || math.Abs(*metrics.CompressionRatio-(154.0/380.0)) > 1e-12 {
		t.Fatalf("Context compression ratio = %#v", metrics.CompressionRatio)
	}
	if metrics.Preservation.Overall.Required != 11 || metrics.Preservation.Overall.Preserved != 10 ||
		metrics.Preservation.Overall.Rate == nil ||
		math.Abs(*metrics.Preservation.Overall.Rate-(10.0/11.0)) > 1e-12 {
		t.Fatalf("Context preservation = %#v", metrics.Preservation)
	}
	if metrics.Preservation.EvidenceBytes.Required != 10 ||
		metrics.Preservation.EvidenceBytes.Preserved != 10 {
		t.Fatalf("Evidence byte preservation = %#v", metrics.Preservation.EvidenceBytes)
	}
	if metrics.PostCompactionRereadCount != nil {
		t.Fatalf("post-compaction reread count = %#v, want unavailable", metrics.PostCompactionRereadCount)
	}

	wantEffective := []uint64{1, 1, 2}
	wantCandidates := []uint64{1, 2, 2}
	for index, snapshot := range report.Snapshots {
		if snapshot.Version != agent.ContextReportVersion ||
			snapshot.CheckpointGeneration == nil || *snapshot.CheckpointGeneration != wantEffective[index] ||
			snapshot.CandidateGeneration == nil || *snapshot.CandidateGeneration != wantCandidates[index] {
			t.Fatalf("snapshot[%d] generations = %#v", index, snapshot)
		}
	}
	if report.Snapshots[1].Validation == nil ||
		report.Snapshots[1].Validation.Rollback != agent.ContextValidationRollbackPrevious {
		t.Fatalf("rollback snapshot = %#v", report.Snapshots[1])
	}
	if got := report.Snapshots[2].ModelLevels; len(got) != 2 ||
		got[0] != agent.ContextCheckpointLevelTask || got[1] != agent.ContextCheckpointLevelEpisode {
		t.Fatalf("hierarchical model levels = %#v", got)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret narrative", strings.Repeat("a", 64)} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("Context report exposed %q: %s", forbidden, encoded)
		}
	}
}

func TestBuildContextReportSupportsLegacyAndFollowUpGenerations(t *testing.T) {
	t.Parallel()

	legacy, err := agent.BuildContextReport(context.Background(), "run-legacy", []agent.Event{
		{RunID: "run-legacy", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "run-legacy", Sequence: 2, Type: agent.EventContextCompacted,
			ContextCheckpoint: &agent.ContextCheckpoint{Generation: 9, SourceMessageCount: 4},
			ContextCompaction: &agent.ContextCompactionReport{
				Applied: true, Reason: "legacy", OriginalBytes: 80, CompiledBytes: 40,
				SourceMessageCount: 4, RecentMessageCount: 1,
			},
		},
		{RunID: "run-legacy", Sequence: 3, Type: agent.EventRunCompleted},
	})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Metrics.UnreportedValidationCount != 1 || legacy.Metrics.CheckpointGenerationCount != 1 ||
		legacy.Metrics.LatestCheckpointGeneration == nil || *legacy.Metrics.LatestCheckpointGeneration != 9 {
		t.Fatalf("legacy metrics = %#v", legacy.Metrics)
	}

	inherited, err := agent.BuildContextReport(context.Background(), "run-inherited", []agent.Event{
		{RunID: "run-inherited", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "run-inherited", Sequence: 2, Type: agent.EventContextCompacted,
			ContextCompaction: &agent.ContextCompactionReport{
				Applied: true, Reason: "externalize_evidence", OriginalBytes: 80, CompiledBytes: 40,
				SourceMessageCount: 4, RecentMessageCount: 2,
			},
		},
		{RunID: "run-inherited", Sequence: 3, Type: agent.EventRunCompleted},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inherited.Metrics.LatestCheckpointGeneration != nil ||
		inherited.Snapshots[0].CheckpointGeneration != nil ||
		inherited.Snapshots[0].CandidateGeneration != nil {
		t.Fatalf("inherited generation should be unavailable = %#v", inherited)
	}

	followUp, err := agent.BuildContextReport(context.Background(), "run-follow-up", []agent.Event{
		{RunID: "run-follow-up", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "run-follow-up", Sequence: 2, Type: agent.EventContextCompacted,
			ContextCheckpoint: &agent.ContextCheckpoint{
				Generation: 7, LastRebaseGeneration: 4, SourceMessageCount: 12,
			},
			ContextCompaction: &agent.ContextCompactionReport{
				Applied: true, Reason: "input_limit", OriginalBytes: 200, CompiledBytes: 90,
				SourceMessageCount: 12, RecentMessageCount: 2,
				Validation: passedContextValidation(7, 12, 1),
			},
		},
		{RunID: "run-follow-up", Sequence: 3, Type: agent.EventRunCompleted},
	})
	if err != nil {
		t.Fatal(err)
	}
	if followUp.Metrics.LatestCheckpointGeneration == nil || *followUp.Metrics.LatestCheckpointGeneration != 7 ||
		followUp.Snapshots[0].CheckpointGeneration == nil || *followUp.Snapshots[0].CheckpointGeneration != 7 {
		t.Fatalf("follow-up report = %#v", followUp)
	}
}

func TestContextReportSnapshotAndDiff(t *testing.T) {
	t.Parallel()

	report, err := agent.BuildContextReport(context.Background(), "run-context", contextReportEvents())
	if err != nil {
		t.Fatal(err)
	}
	latest, err := report.Snapshot(0)
	if err != nil || latest.EventSequence != 4 {
		t.Fatalf("latest Snapshot = %#v, %v", latest, err)
	}
	before, err := report.Snapshot(2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := report.Snapshot(99); !errors.Is(err, agent.ErrContextSnapshotNotFound) {
		t.Fatalf("missing Snapshot error = %v", err)
	}

	diff := agent.DiffContextSnapshots(before, latest)
	if diff.Before.EventSequence != 2 || diff.After.EventSequence != 4 ||
		diff.Delta.OriginalBytes.Delta != 60 || diff.Delta.CompiledBytes.Delta != 24 ||
		diff.Delta.SourceMessageCount.Delta != 2 ||
		diff.Delta.Preservation.ActiveConstraints.Required != 1 ||
		diff.Delta.Preservation.ActiveConstraints.Preserved != 1 {
		t.Fatalf("Context diff = %#v", diff)
	}
	if diff.Delta.CompressionRatio.Delta == nil ||
		math.Abs(*diff.Delta.CompressionRatio.Delta) > 1e-12 {
		t.Fatalf("compression ratio delta = %#v", diff.Delta.CompressionRatio)
	}
}

func TestBuildContextReportNormalizesUnrecognizedLabels(t *testing.T) {
	t.Parallel()

	report, err := agent.BuildContextReport(context.Background(), "run-labels", []agent.Event{
		{RunID: "run-labels", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "run-labels", Sequence: 2, Type: agent.EventContextCompacted,
			ContextCompaction: &agent.ContextCompactionReport{
				Applied: true, Reason: "SECRET_REASON_CONTENT", Fallback: "SECRET_FALLBACK_CONTENT",
				OriginalBytes: 10, CompiledBytes: 5,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Snapshots[0].Reason != agent.ContextReportUnrecognizedLabel ||
		report.Snapshots[0].Fallback != agent.ContextReportUnrecognizedLabel {
		t.Fatalf("normalized labels = %#v", report.Snapshots[0])
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "SECRET_") {
		t.Fatalf("Context report exposed an unrecognized label: %s", encoded)
	}
}

func TestBuildContextReportRejectsMalformedEvents(t *testing.T) {
	t.Parallel()

	valid := contextReportEvents()[1]
	tests := map[string][]agent.Event{
		"sequence gap": {
			{RunID: "run-bad", Sequence: 1, Type: agent.EventRunStarted},
			contextEventForRun(valid, "run-bad", 3),
		},
		"missing report": {
			{RunID: "run-bad", Sequence: 1, Type: agent.EventRunStarted},
			{RunID: "run-bad", Sequence: 2, Type: agent.EventContextCompacted},
		},
		"negative size": {
			{RunID: "run-bad", Sequence: 1, Type: agent.EventRunStarted},
			contextEventWithReport("run-bad", 2, &agent.ContextCompactionReport{
				Reason: "bad", OriginalBytes: -1,
			}),
		},
		"invalid validation": {
			{RunID: "run-bad", Sequence: 1, Type: agent.EventRunStarted},
			contextEventWithReport("run-bad", 2, &agent.ContextCompactionReport{
				Applied: true, Reason: "bad", OriginalBytes: 10, CompiledBytes: 5,
				SourceMessageCount: 2,
				Validation: &agent.ContextValidationReport{
					Version: agent.ContextValidationVersion, CandidateGeneration: 1,
					CandidateSourceMessageCount: 2, Passed: true,
					ActiveConstraints: agent.ContextPreservationCount{Required: 1},
				},
			}),
		},
		"previous rollback without generation": {
			{RunID: "run-bad", Sequence: 1, Type: agent.EventRunStarted},
			contextEventWithReport("run-bad", 2, &agent.ContextCompactionReport{
				Reason: "validation_rollback", OriginalBytes: 10, CompiledBytes: 5,
				Validation: &agent.ContextValidationReport{
					Version: agent.ContextValidationVersion, CandidateGeneration: 1,
					CandidateSourceMessageCount: 2,
					ActiveConstraints:           agent.ContextPreservationCount{Required: 1},
					Failures:                    []agent.ContextValidationFailure{agent.ContextValidationActiveConstraints},
					Rollback:                    agent.ContextValidationRollbackPrevious,
				},
			}),
		},
		"invalid evidence": {
			{RunID: "run-bad", Sequence: 1, Type: agent.EventRunStarted},
			contextEventWithReport("run-bad", 2, &agent.ContextCompactionReport{
				Reason: "bad", OriginalBytes: 10, CompiledBytes: 5,
				Externalized: []agent.EvidenceObjectRef{{Digest: "not-sha256", MediaType: "application/json"}},
			}),
		},
		"duplicate evidence": {
			{RunID: "run-bad", Sequence: 1, Type: agent.EventRunStarted},
			contextEventWithReport("run-bad", 2, &agent.ContextCompactionReport{
				Reason: "input_limit", OriginalBytes: 10, CompiledBytes: 5,
				Externalized: []agent.EvidenceObjectRef{
					{Digest: "sha256:" + strings.Repeat("c", 64), MediaType: "application/json"},
					{Digest: "sha256:" + strings.Repeat("c", 64), MediaType: "application/json"},
				},
			}),
		},
		"unordered model levels": {
			{RunID: "run-bad", Sequence: 1, Type: agent.EventRunStarted},
			contextEventWithReport("run-bad", 2, &agent.ContextCompactionReport{
				Reason: "input_limit", OriginalBytes: 10, CompiledBytes: 5,
				ModelLevels: []agent.ContextCheckpointLevel{
					agent.ContextCheckpointLevelEpisode,
					agent.ContextCheckpointLevelTask,
				},
			}),
		},
	}
	for name, events := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := agent.BuildContextReport(context.Background(), "run-bad", events); err == nil {
				t.Fatal("BuildContextReport error = nil")
			}
		})
	}
}

func contextReportEvents() []agent.Event {
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	return []agent.Event{
		{RunID: "run-context", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "run-context", Sequence: 2, Type: agent.EventContextCompacted,
			ContextCheckpoint: &agent.ContextCheckpoint{
				Version: agent.ContextCheckpointVersion, Generation: 1,
				LastRebaseGeneration: 1, SourceMessageCount: 2,
				Layers:    []agent.ContextCheckpointLayer{{Level: agent.ContextCheckpointLevelEpisode, SourceMessageEnd: 2}},
				Goal:      &agent.CheckpointFact{SourceMessage: 1},
				Narrative: "secret narrative",
			},
			ContextCompaction: &agent.ContextCompactionReport{
				Applied: true, Reason: "raw_event_rebase", OriginalBytes: 100, CompiledBytes: 40,
				SourceMessageCount: 2, RecentMessageCount: 2,
				ModelLevels: []agent.ContextCheckpointLevel{agent.ContextCheckpointLevelEpisode},
				Externalized: []agent.EvidenceObjectRef{{
					Digest: digestA, MediaType: "application/json", Bytes: 10,
				}},
				Rebased: true, RebaseReason: agent.ContextRebaseInitial,
				Validation: &agent.ContextValidationReport{
					Version: agent.ContextValidationVersion, CandidateGeneration: 1,
					CandidateSourceMessageCount: 2, Passed: true,
					ActiveConstraints: agent.ContextPreservationCount{Required: 2, Preserved: 2},
					ModifiedArtifacts: agent.ContextPreservationCount{Required: 1, Preserved: 1},
					FailingChecks:     agent.ContextPreservationCount{Required: 1, Preserved: 1},
					PendingTools:      agent.ContextPreservationCount{Required: 1, Preserved: 1},
					Evidence: agent.ContextPreservationCount{
						Required: 1, Preserved: 1, RequiredBytes: 10, PreservedBytes: 10,
					},
				},
			},
		},
		{
			RunID: "run-context", Sequence: 3, Type: agent.EventContextCompacted,
			ContextCompaction: &agent.ContextCompactionReport{
				Reason: "validation_rollback", OriginalBytes: 120, CompiledBytes: 50,
				SourceMessageCount: 2, RecentMessageCount: 3,
				ModelLevels: []agent.ContextCheckpointLevel{agent.ContextCheckpointLevelEpisode},
				Fallback:    "checkpoint_strategy_validation_failed",
				Validation: &agent.ContextValidationReport{
					Version: agent.ContextValidationVersion, CandidateGeneration: 2,
					CandidateSourceMessageCount: 4,
					ActiveConstraints:           agent.ContextPreservationCount{Required: 2, Preserved: 1},
					Failures:                    []agent.ContextValidationFailure{agent.ContextValidationActiveConstraints},
					Rollback:                    agent.ContextValidationRollbackPrevious,
				},
			},
		},
		{
			RunID: "run-context", Sequence: 4, Type: agent.EventContextCompacted,
			ContextCheckpoint: &agent.ContextCheckpoint{
				Version: agent.ContextCheckpointVersion, Generation: 2,
				LastRebaseGeneration: 1, SourceMessageCount: 4,
				Layers: []agent.ContextCheckpointLayer{
					{Level: agent.ContextCheckpointLevelTask, SourceMessageEnd: 2},
					{Level: agent.ContextCheckpointLevelEpisode, SourceMessageEnd: 4},
				},
				Facts: []agent.CheckpointFact{{SourceMessage: 1}},
				Goal:  &agent.CheckpointFact{SourceMessage: 3},
			},
			ContextCompaction: &agent.ContextCompactionReport{
				Applied: true, Reason: "input_limit", OriginalBytes: 160, CompiledBytes: 64,
				SourceMessageCount: 4, RecentMessageCount: 2,
				ModelLevels: []agent.ContextCheckpointLevel{
					agent.ContextCheckpointLevelTask,
					agent.ContextCheckpointLevelEpisode,
				},
				Externalized: []agent.EvidenceObjectRef{{
					Digest: digestB, MediaType: "application/json", Bytes: 20,
				}},
				Validation: passedContextValidation(2, 4, 3),
			},
		},
		{RunID: "run-context", Sequence: 5, Type: agent.EventRunCompleted},
	}
}

func passedContextValidation(generation uint64, sourceMessages, constraints int) *agent.ContextValidationReport {
	return &agent.ContextValidationReport{
		Version: agent.ContextValidationVersion, CandidateGeneration: generation,
		CandidateSourceMessageCount: sourceMessages, Passed: true,
		ActiveConstraints: agent.ContextPreservationCount{Required: constraints, Preserved: constraints},
	}
}

func contextEventForRun(event agent.Event, runID string, sequence uint64) agent.Event {
	event.RunID = runID
	event.Sequence = sequence
	return event
}

func contextEventWithReport(runID string, sequence uint64, report *agent.ContextCompactionReport) agent.Event {
	return agent.Event{
		RunID: runID, Sequence: sequence, Type: agent.EventContextCompacted,
		ContextCompaction: report,
	}
}
