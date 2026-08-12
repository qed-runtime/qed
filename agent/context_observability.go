package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

const (
	// ContextReportVersion is the content-free Context observability schema
	ContextReportVersion uint32 = 1
	// ContextReportUnrecognizedLabel replaces an untrusted compaction label
	ContextReportUnrecognizedLabel = "unrecognized"
)

// ErrContextSnapshotNotFound indicates that a Run has no matching Context compaction Event
var ErrContextSnapshotNotFound = errors.New("Context snapshot not found")

// ContextMetricRate records one aggregate preservation numerator and denominator
type ContextMetricRate struct {
	// Required is the number of items that validation required
	Required int64 `json:"required"`
	// Preserved is the number of required items that validation preserved
	Preserved int64 `json:"preserved"`
	// Rate is Preserved divided by Required, or nil when Required is zero
	Rate *float64 `json:"rate"`
}

// ContextPreservationMetrics aggregates deterministic validation categories
type ContextPreservationMetrics struct {
	// Overall combines every item-count category, including Evidence object count
	Overall ContextMetricRate `json:"overall"`
	// ActiveConstraints aggregates active Constraint Fact preservation
	ActiveConstraints ContextMetricRate `json:"active_constraints"`
	// ModifiedArtifacts aggregates canonical modified artifact preservation
	ModifiedArtifacts ContextMetricRate `json:"modified_artifacts"`
	// FailingChecks aggregates failing check preservation
	FailingChecks ContextMetricRate `json:"failing_checks"`
	// PendingTools aggregates pending Tool transaction preservation
	PendingTools ContextMetricRate `json:"pending_tools"`
	// EvidenceObjects aggregates required Evidence object preservation
	EvidenceObjects ContextMetricRate `json:"evidence_objects"`
	// EvidenceBytes aggregates required Evidence byte preservation
	EvidenceBytes ContextMetricRate `json:"evidence_bytes"`
}

// ContextMetrics aggregates content-free Context Compiler observations for one Run
type ContextMetrics struct {
	// CompactionCount is the number of context.compacted Events
	CompactionCount int64 `json:"compaction_count"`
	// CheckpointGenerationCount is the number of published Checkpoint generations
	CheckpointGenerationCount int64 `json:"checkpoint_generation_count"`
	// LatestCheckpointGeneration is the effective generation at the last Context Event
	//
	// Nil means the selected Run Bundle does not establish its inherited generation
	LatestCheckpointGeneration *uint64 `json:"latest_checkpoint_generation"`
	// FullRebaseCount is the number of published Raw Event Rebase Checkpoints
	FullRebaseCount int64 `json:"full_rebase_count"`
	// RollbackCount is the number of failed candidates that selected a safe earlier view
	RollbackCount int64 `json:"rollback_count"`
	// CustomFallbackCount is the number of deterministic fallbacks from a custom strategy
	CustomFallbackCount int64 `json:"custom_fallback_count"`
	// ValidationCount is the number of Events with deterministic validation reports
	ValidationCount int64 `json:"validation_count"`
	// ValidationFailureCount is the number of candidate validation failures
	ValidationFailureCount int64 `json:"validation_failure_count"`
	// UnreportedValidationCount is the number of Context Events without candidate validation
	//
	// It includes older Events and current Evidence-only compaction Events
	UnreportedValidationCount int64 `json:"unreported_validation_count"`
	// OriginalBytes is the aggregate canonical size before each observed reduction
	OriginalBytes int64 `json:"original_bytes"`
	// CompiledBytes is the aggregate canonical size after each observed reduction
	CompiledBytes int64 `json:"compiled_bytes"`
	// CompressionRatio is CompiledBytes divided by OriginalBytes, or nil when unavailable
	CompressionRatio *float64 `json:"compression_ratio"`
	// ExternalizedObjects is the number of newly published Evidence object references
	ExternalizedObjects int64 `json:"externalized_objects"`
	// ExternalizedBytes is the byte total of newly published Evidence objects
	ExternalizedBytes int64 `json:"externalized_bytes"`
	// Preservation aggregates deterministic validation counts
	Preservation ContextPreservationMetrics `json:"preservation"`
	// PostCompactionRereadCount is nil until retrieval is represented by a public Event
	//
	// A nil value must not be interpreted as zero rereads
	PostCompactionRereadCount *int64 `json:"post_compaction_reread_count"`
}

// ContextSnapshot is one content-free context.compacted Event projection
type ContextSnapshot struct {
	// Version identifies the snapshot schema
	Version uint32 `json:"version"`
	// RunID identifies the Run that emitted the Event
	RunID string `json:"run_id"`
	// EventSequence identifies the Event within its Run
	EventSequence uint64 `json:"event_sequence"`
	// CheckpointGeneration is the effective validated generation after this Event
	//
	// Nil means this Run Bundle does not establish its inherited generation. A
	// pointer to zero means validation established that no Checkpoint is active.
	CheckpointGeneration *uint64 `json:"checkpoint_generation"`
	// CandidateGeneration is the generation evaluated by this Event when known
	CandidateGeneration *uint64 `json:"candidate_generation"`
	// PublishedCheckpoint reports whether this Event committed a new Checkpoint
	PublishedCheckpoint bool `json:"published_checkpoint"`
	// Applied reports whether the compiler produced a new Checkpoint or externalized view
	Applied bool `json:"applied"`
	// Reason identifies the deterministic compaction trigger
	Reason string `json:"reason"`
	// OriginalBytes is the canonical logical size before reduction
	OriginalBytes int64 `json:"original_bytes"`
	// CompiledBytes is the canonical logical size of the active compiled model view
	CompiledBytes int64 `json:"compiled_bytes"`
	// CompressionRatio is CompiledBytes divided by OriginalBytes, or nil when unavailable
	CompressionRatio *float64 `json:"compression_ratio"`
	// SourceMessageCount is the number of raw messages represented by the effective Checkpoint
	SourceMessageCount int64 `json:"source_message_count"`
	// RecentMessageCount is the number of raw messages retained after the Checkpoint
	RecentMessageCount int64 `json:"recent_message_count"`
	// ExternalizedObjects is the number of newly published Evidence object references
	ExternalizedObjects int64 `json:"externalized_objects"`
	// ExternalizedBytes is the byte total of newly published Evidence objects
	ExternalizedBytes int64 `json:"externalized_bytes"`
	// Fallback identifies a failed custom strategy without including its input or output
	Fallback string `json:"fallback,omitempty"`
	// Rebased reports that the Checkpoint was rebuilt without its previous semantic view
	Rebased bool `json:"rebased"`
	// RebaseReason identifies the deterministic Raw Event Rebase trigger
	RebaseReason ContextRebaseReason `json:"rebase_reason,omitempty"`
	// Validation contains deterministic content-free preservation counts
	Validation *ContextValidationReport `json:"validation,omitempty"`
}

// ContextReport contains one Run Context timeline and aggregate content-free metrics
type ContextReport struct {
	// Version identifies the report schema
	Version uint32 `json:"version"`
	// RunID identifies the selected Run
	RunID string `json:"run_id"`
	// Snapshots preserve context.compacted Event order
	Snapshots []ContextSnapshot `json:"snapshots"`
	// Metrics aggregate all Snapshots
	Metrics ContextMetrics `json:"metrics"`
}

// ContextValueDelta records one signed before-to-after numeric change
type ContextValueDelta struct {
	// Before is the earlier value
	Before int64 `json:"before"`
	// After is the later value
	After int64 `json:"after"`
	// Delta is After minus Before
	Delta int64 `json:"delta"`
}

// ContextRatioDelta records one before-to-after ratio change
type ContextRatioDelta struct {
	// Before is the earlier ratio, or nil when unavailable
	Before *float64 `json:"before"`
	// After is the later ratio, or nil when unavailable
	After *float64 `json:"after"`
	// Delta is After minus Before when both ratios are available
	Delta *float64 `json:"delta"`
}

// ContextPreservationDelta records signed required and preserved count changes
type ContextPreservationDelta struct {
	// Required is the required-count change
	Required int64 `json:"required"`
	// Preserved is the preserved-count change
	Preserved int64 `json:"preserved"`
}

// ContextPreservationDiff compares deterministic validation categories
type ContextPreservationDiff struct {
	// ActiveConstraints compares active Constraint Fact counts
	ActiveConstraints ContextPreservationDelta `json:"active_constraints"`
	// ModifiedArtifacts compares modified artifact counts
	ModifiedArtifacts ContextPreservationDelta `json:"modified_artifacts"`
	// FailingChecks compares failing check counts
	FailingChecks ContextPreservationDelta `json:"failing_checks"`
	// PendingTools compares pending Tool transaction counts
	PendingTools ContextPreservationDelta `json:"pending_tools"`
	// EvidenceObjects compares Evidence object counts
	EvidenceObjects ContextPreservationDelta `json:"evidence_objects"`
	// EvidenceBytes compares Evidence byte counts
	EvidenceBytes ContextPreservationDelta `json:"evidence_bytes"`
}

// ContextSnapshotDelta contains content-free changes between two snapshots
type ContextSnapshotDelta struct {
	// OriginalBytes compares canonical pre-reduction sizes
	OriginalBytes ContextValueDelta `json:"original_bytes"`
	// CompiledBytes compares canonical compiled sizes
	CompiledBytes ContextValueDelta `json:"compiled_bytes"`
	// CompressionRatio compares compiled-to-original ratios
	CompressionRatio ContextRatioDelta `json:"compression_ratio"`
	// SourceMessageCount compares represented raw message counts
	SourceMessageCount ContextValueDelta `json:"source_message_count"`
	// RecentMessageCount compares retained raw-tail counts
	RecentMessageCount ContextValueDelta `json:"recent_message_count"`
	// ExternalizedObjects compares newly published Evidence object counts
	ExternalizedObjects ContextValueDelta `json:"externalized_objects"`
	// ExternalizedBytes compares newly published Evidence byte totals
	ExternalizedBytes ContextValueDelta `json:"externalized_bytes"`
	// Preservation compares deterministic validation counts
	Preservation ContextPreservationDiff `json:"preservation"`
}

// ContextSnapshotDiff compares two content-free Context snapshots
type ContextSnapshotDiff struct {
	// Version identifies the diff schema
	Version uint32 `json:"version"`
	// Before is the earlier selected snapshot
	Before ContextSnapshot `json:"before"`
	// After is the later selected snapshot
	After ContextSnapshot `json:"after"`
	// Delta contains signed After-minus-Before changes
	Delta ContextSnapshotDelta `json:"delta"`
}

// BuildContextReport validates Context observations and projects one Run's
// complete public Event stream
//
// The returned report contains counts, stable reason codes, and ratios only. It
// does not copy prompts, messages, paths, commands, object digests, or object
// content from the Event stream. Events from other Runs are ignored. The
// selected Run's Events must retain their complete contiguous Run sequence.
func BuildContextReport(ctx context.Context, runID string, events []Event) (ContextReport, error) {
	if ctx == nil {
		return ContextReport{}, errors.New("Context report context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return ContextReport{}, err
	}
	if strings.TrimSpace(runID) == "" {
		return ContextReport{}, errors.New("Context report Run ID is required")
	}

	report := ContextReport{
		Version:   ContextReportVersion,
		RunID:     runID,
		Snapshots: make([]ContextSnapshot, 0),
	}
	var expectedSequence uint64
	var activeCheckpoint *ContextCheckpoint
	activeKnown := false
	for index := range events {
		if err := ctx.Err(); err != nil {
			return ContextReport{}, err
		}
		event := events[index]
		if event.RunID != runID {
			continue
		}
		if expectedSequence == ^uint64(0) || event.Sequence != expectedSequence+1 {
			return ContextReport{}, fmt.Errorf(
				"Context report Run %q Event sequence = %d, want %d",
				runID,
				event.Sequence,
				expectedSequence+1,
			)
		}
		expectedSequence = event.Sequence
		if event.ToolResult != nil && event.ToolResult.ContextRetrieval != nil {
			if event.Type != EventToolCompleted {
				return ContextReport{}, fmt.Errorf("Context report Event %d: Context retrieval metadata requires tool.completed", event.Sequence)
			}
			if err := ValidateContextRetrievalMetadata(
				event.ToolResult.Name,
				event.ToolResult.Output,
				event.ToolResult.IsError,
				event.ToolResult.ContextRetrieval,
			); err != nil {
				return ContextReport{}, fmt.Errorf("Context report Event %d: %w", event.Sequence, err)
			}
			if report.Metrics.PostCompactionRereadCount == nil {
				zero := int64(0)
				report.Metrics.PostCompactionRereadCount = &zero
			}
			metadata := event.ToolResult.ContextRetrieval
			if metadata.Operation == ContextRetrievalFetch &&
				metadata.Outcome == ContextRetrievalSucceeded && metadata.PostCompaction {
				count, err := addContextMetric(*report.Metrics.PostCompactionRereadCount, 1)
				if err != nil {
					return ContextReport{}, fmt.Errorf("Context report Event %d: %w", event.Sequence, err)
				}
				*report.Metrics.PostCompactionRereadCount = count
			}
		}
		if event.Type != EventContextCompacted {
			continue
		}
		if event.ContextCompaction == nil {
			return ContextReport{}, fmt.Errorf("Context report Event %d has no compaction report", event.Sequence)
		}
		if err := validateContextMetricEvent(event); err != nil {
			return ContextReport{}, fmt.Errorf("Context report Event %d: %w", event.Sequence, err)
		}
		if activeKnown {
			if err := validateContextValidationTransition(event, activeCheckpoint); err != nil {
				return ContextReport{}, fmt.Errorf("Context report Event %d: %w", event.Sequence, err)
			}
		}

		validation := event.ContextCompaction.Validation
		if !activeKnown && validation != nil && !validation.Passed {
			switch validation.Rollback {
			case ContextValidationRollbackPrevious:
				if validation.CandidateGeneration <= 1 {
					return ContextReport{}, fmt.Errorf(
						"Context report Event %d previous rollback has no preceding generation",
						event.Sequence,
					)
				}
				activeCheckpoint = &ContextCheckpoint{
					Generation:         validation.CandidateGeneration - 1,
					SourceMessageCount: event.ContextCompaction.SourceMessageCount,
				}
				activeKnown = true
			case ContextValidationRollbackRaw:
				activeCheckpoint = nil
				activeKnown = true
			}
		}
		if event.ContextCheckpoint != nil {
			activeCheckpoint = cloneContextCheckpointPointer(event.ContextCheckpoint)
			activeKnown = true
		}

		snapshot, err := contextSnapshotFromEvent(event, activeCheckpoint, activeKnown)
		if err != nil {
			return ContextReport{}, fmt.Errorf("Context report Event %d: %w", event.Sequence, err)
		}
		report.Snapshots = append(report.Snapshots, snapshot)
		if err := addContextSnapshotMetrics(&report.Metrics, snapshot); err != nil {
			return ContextReport{}, fmt.Errorf("Context report Event %d: %w", event.Sequence, err)
		}
	}
	report.Metrics.CompressionRatio = contextRatio(report.Metrics.CompiledBytes, report.Metrics.OriginalBytes)
	finalizeContextPreservationMetrics(&report.Metrics.Preservation)
	return report, nil
}

// Snapshot returns the latest snapshot when eventSequence is zero or the exact
// matching context.compacted Event otherwise
func (report ContextReport) Snapshot(eventSequence uint64) (ContextSnapshot, error) {
	if eventSequence == 0 {
		if len(report.Snapshots) == 0 {
			return ContextSnapshot{}, fmt.Errorf("%w for Run %q", ErrContextSnapshotNotFound, report.RunID)
		}
		return cloneContextSnapshot(report.Snapshots[len(report.Snapshots)-1]), nil
	}
	for _, snapshot := range report.Snapshots {
		if snapshot.EventSequence == eventSequence {
			return cloneContextSnapshot(snapshot), nil
		}
	}
	return ContextSnapshot{}, fmt.Errorf(
		"%w for Run %q Event sequence %d",
		ErrContextSnapshotNotFound,
		report.RunID,
		eventSequence,
	)
}

// DiffContextSnapshots returns signed content-free changes between two snapshots
func DiffContextSnapshots(before, after ContextSnapshot) ContextSnapshotDiff {
	diff := ContextSnapshotDiff{
		Version: ContextReportVersion,
		Before:  cloneContextSnapshot(before),
		After:   cloneContextSnapshot(after),
		Delta: ContextSnapshotDelta{
			OriginalBytes:       contextValueDelta(before.OriginalBytes, after.OriginalBytes),
			CompiledBytes:       contextValueDelta(before.CompiledBytes, after.CompiledBytes),
			CompressionRatio:    contextRatioDelta(before.CompressionRatio, after.CompressionRatio),
			SourceMessageCount:  contextValueDelta(before.SourceMessageCount, after.SourceMessageCount),
			RecentMessageCount:  contextValueDelta(before.RecentMessageCount, after.RecentMessageCount),
			ExternalizedObjects: contextValueDelta(before.ExternalizedObjects, after.ExternalizedObjects),
			ExternalizedBytes:   contextValueDelta(before.ExternalizedBytes, after.ExternalizedBytes),
		},
	}
	diff.Delta.Preservation = contextPreservationDiff(before.Validation, after.Validation)
	return diff
}

func validateContextMetricEvent(event Event) error {
	report := event.ContextCompaction
	if report == nil {
		return errors.New("context.compacted requires a compaction report")
	}
	if strings.TrimSpace(report.Reason) == "" {
		return errors.New("context.compacted reason is required")
	}
	if report.OriginalBytes < 0 || report.CompiledBytes < 0 {
		return errors.New("context.compacted byte counts must not be negative")
	}
	if report.SourceMessageCount < 0 || report.RecentMessageCount < 0 {
		return errors.New("context.compacted message counts must not be negative")
	}
	seenEvidence := make(map[string]struct{}, len(report.Externalized))
	for _, reference := range report.Externalized {
		if err := ValidateEvidenceObjectRef(reference); err != nil || strings.TrimSpace(reference.MediaType) == "" {
			return errors.New("context.compacted contains an invalid Evidence Object reference")
		}
		if _, exists := seenEvidence[reference.Identity()]; exists {
			return errors.New("context.compacted contains a duplicate Evidence Object reference")
		}
		seenEvidence[reference.Identity()] = struct{}{}
	}
	return validateContextRebaseEvent(event)
}

func contextSnapshotFromEvent(event Event, active *ContextCheckpoint, activeKnown bool) (ContextSnapshot, error) {
	report := event.ContextCompaction
	externalizedBytes := int64(0)
	for _, reference := range report.Externalized {
		var err error
		externalizedBytes, err = addContextMetric(externalizedBytes, reference.Bytes)
		if err != nil {
			return ContextSnapshot{}, fmt.Errorf("externalized Evidence bytes: %w", err)
		}
	}
	var candidateGeneration *uint64
	if report.Validation != nil {
		value := report.Validation.CandidateGeneration
		candidateGeneration = &value
	} else if event.ContextCheckpoint != nil {
		value := event.ContextCheckpoint.Generation
		candidateGeneration = &value
	}
	var checkpointGeneration *uint64
	if activeKnown {
		value := uint64(0)
		if active != nil {
			value = active.Generation
		}
		checkpointGeneration = &value
	}
	return ContextSnapshot{
		Version:              ContextReportVersion,
		RunID:                event.RunID,
		EventSequence:        event.Sequence,
		CheckpointGeneration: checkpointGeneration,
		CandidateGeneration:  candidateGeneration,
		PublishedCheckpoint:  event.ContextCheckpoint != nil,
		Applied:              report.Applied,
		Reason:               contextReportReason(report.Reason),
		OriginalBytes:        report.OriginalBytes,
		CompiledBytes:        report.CompiledBytes,
		CompressionRatio:     contextRatio(report.CompiledBytes, report.OriginalBytes),
		SourceMessageCount:   int64(report.SourceMessageCount),
		RecentMessageCount:   int64(report.RecentMessageCount),
		ExternalizedObjects:  int64(len(report.Externalized)),
		ExternalizedBytes:    externalizedBytes,
		Fallback:             contextReportFallback(report.Fallback),
		Rebased:              report.Rebased,
		RebaseReason:         report.RebaseReason,
		Validation:           contextReportValidation(report.Validation),
	}, nil
}

func contextReportReason(value string) string {
	switch value {
	case "checkpoint", "externalize_evidence", "input_limit", "raw_event_rebase",
		"reuse_checkpoint", "validation_rollback", "predictive_budget_adopt":
		return value
	default:
		return ContextReportUnrecognizedLabel
	}
}

func contextReportFallback(value string) string {
	switch value {
	case "":
		return ""
	case "checkpoint_strategy_build_failed", "checkpoint_strategy_validation_failed":
		return value
	default:
		return ContextReportUnrecognizedLabel
	}
}

func contextReportValidation(report *ContextValidationReport) *ContextValidationReport {
	cloned := cloneContextValidationReport(report)
	if cloned == nil {
		return nil
	}
	sort.Slice(cloned.Failures, func(first, second int) bool {
		return cloned.Failures[first] < cloned.Failures[second]
	})
	return cloned
}

func addContextSnapshotMetrics(metrics *ContextMetrics, snapshot ContextSnapshot) error {
	var err error
	if metrics.CompactionCount, err = addContextMetric(metrics.CompactionCount, 1); err != nil {
		return err
	}
	if snapshot.Validation == nil {
		if metrics.UnreportedValidationCount, err = addContextMetric(metrics.UnreportedValidationCount, 1); err != nil {
			return err
		}
	} else {
		if metrics.ValidationCount, err = addContextMetric(metrics.ValidationCount, 1); err != nil {
			return err
		}
		if !snapshot.Validation.Passed {
			if metrics.ValidationFailureCount, err = addContextMetric(metrics.ValidationFailureCount, 1); err != nil {
				return err
			}
		}
		if snapshot.Validation.Rollback != "" {
			if metrics.RollbackCount, err = addContextMetric(metrics.RollbackCount, 1); err != nil {
				return err
			}
		}
		if err := addContextPreservationMetrics(&metrics.Preservation, *snapshot.Validation); err != nil {
			return err
		}
	}
	if snapshot.PublishedCheckpoint {
		if metrics.CheckpointGenerationCount, err = addContextMetric(metrics.CheckpointGenerationCount, 1); err != nil {
			return err
		}
	}
	metrics.LatestCheckpointGeneration = cloneUint64Pointer(snapshot.CheckpointGeneration)
	if snapshot.Rebased {
		if metrics.FullRebaseCount, err = addContextMetric(metrics.FullRebaseCount, 1); err != nil {
			return err
		}
	}
	if snapshot.Fallback != "" {
		if metrics.CustomFallbackCount, err = addContextMetric(metrics.CustomFallbackCount, 1); err != nil {
			return err
		}
	}
	if metrics.OriginalBytes, err = addContextMetric(metrics.OriginalBytes, snapshot.OriginalBytes); err != nil {
		return err
	}
	if metrics.CompiledBytes, err = addContextMetric(metrics.CompiledBytes, snapshot.CompiledBytes); err != nil {
		return err
	}
	if metrics.ExternalizedObjects, err = addContextMetric(metrics.ExternalizedObjects, snapshot.ExternalizedObjects); err != nil {
		return err
	}
	metrics.ExternalizedBytes, err = addContextMetric(metrics.ExternalizedBytes, snapshot.ExternalizedBytes)
	return err
}

func addContextPreservationMetrics(metrics *ContextPreservationMetrics, report ContextValidationReport) error {
	values := []struct {
		target *ContextMetricRate
		count  ContextPreservationCount
	}{
		{&metrics.ActiveConstraints, report.ActiveConstraints},
		{&metrics.ModifiedArtifacts, report.ModifiedArtifacts},
		{&metrics.FailingChecks, report.FailingChecks},
		{&metrics.PendingTools, report.PendingTools},
		{&metrics.EvidenceObjects, report.Evidence},
	}
	for _, value := range values {
		if err := addContextRate(value.target, int64(value.count.Required), int64(value.count.Preserved)); err != nil {
			return err
		}
		if err := addContextRate(&metrics.Overall, int64(value.count.Required), int64(value.count.Preserved)); err != nil {
			return err
		}
	}
	return addContextRate(&metrics.EvidenceBytes, report.Evidence.RequiredBytes, report.Evidence.PreservedBytes)
}

func addContextRate(rate *ContextMetricRate, required, preserved int64) error {
	var err error
	if rate.Required, err = addContextMetric(rate.Required, required); err != nil {
		return err
	}
	rate.Preserved, err = addContextMetric(rate.Preserved, preserved)
	return err
}

func finalizeContextPreservationMetrics(metrics *ContextPreservationMetrics) {
	values := []*ContextMetricRate{
		&metrics.Overall,
		&metrics.ActiveConstraints,
		&metrics.ModifiedArtifacts,
		&metrics.FailingChecks,
		&metrics.PendingTools,
		&metrics.EvidenceObjects,
		&metrics.EvidenceBytes,
	}
	for _, value := range values {
		value.Rate = contextRatio(value.Preserved, value.Required)
	}
}

func addContextMetric(current, value int64) (int64, error) {
	if value < 0 {
		return 0, errors.New("Context metric value must not be negative")
	}
	if current > math.MaxInt64-value {
		return 0, errors.New("Context metric total overflows")
	}
	return current + value, nil
}

func contextRatio(numerator, denominator int64) *float64 {
	if denominator == 0 {
		return nil
	}
	value := float64(numerator) / float64(denominator)
	return &value
}

func cloneContextSnapshot(snapshot ContextSnapshot) ContextSnapshot {
	cloned := snapshot
	cloned.Reason = contextReportReason(snapshot.Reason)
	cloned.Fallback = contextReportFallback(snapshot.Fallback)
	cloned.CheckpointGeneration = cloneUint64Pointer(snapshot.CheckpointGeneration)
	cloned.CandidateGeneration = cloneUint64Pointer(snapshot.CandidateGeneration)
	if snapshot.CompressionRatio != nil {
		ratio := *snapshot.CompressionRatio
		cloned.CompressionRatio = &ratio
	}
	cloned.Validation = contextReportValidation(snapshot.Validation)
	return cloned
}

func cloneUint64Pointer(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func contextValueDelta(before, after int64) ContextValueDelta {
	return ContextValueDelta{Before: before, After: after, Delta: after - before}
}

func contextRatioDelta(before, after *float64) ContextRatioDelta {
	result := ContextRatioDelta{}
	if before != nil {
		value := *before
		result.Before = &value
	}
	if after != nil {
		value := *after
		result.After = &value
	}
	if before != nil && after != nil {
		value := *after - *before
		result.Delta = &value
	}
	return result
}

func contextPreservationDiff(before, after *ContextValidationReport) ContextPreservationDiff {
	beforeReport := ContextValidationReport{}
	afterReport := ContextValidationReport{}
	if before != nil {
		beforeReport = *before
	}
	if after != nil {
		afterReport = *after
	}
	return ContextPreservationDiff{
		ActiveConstraints: contextPreservationCountDelta(
			beforeReport.ActiveConstraints,
			afterReport.ActiveConstraints,
			false,
		),
		ModifiedArtifacts: contextPreservationCountDelta(
			beforeReport.ModifiedArtifacts,
			afterReport.ModifiedArtifacts,
			false,
		),
		FailingChecks: contextPreservationCountDelta(
			beforeReport.FailingChecks,
			afterReport.FailingChecks,
			false,
		),
		PendingTools: contextPreservationCountDelta(
			beforeReport.PendingTools,
			afterReport.PendingTools,
			false,
		),
		EvidenceObjects: contextPreservationCountDelta(
			beforeReport.Evidence,
			afterReport.Evidence,
			false,
		),
		EvidenceBytes: contextPreservationCountDelta(
			beforeReport.Evidence,
			afterReport.Evidence,
			true,
		),
	}
}

func contextPreservationCountDelta(
	before ContextPreservationCount,
	after ContextPreservationCount,
	bytes bool,
) ContextPreservationDelta {
	if bytes {
		return ContextPreservationDelta{
			Required:  after.RequiredBytes - before.RequiredBytes,
			Preserved: after.PreservedBytes - before.PreservedBytes,
		}
	}
	return ContextPreservationDelta{
		Required:  int64(after.Required) - int64(before.Required),
		Preserved: int64(after.Preserved) - int64(before.Preserved),
	}
}
