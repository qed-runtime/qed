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
	// ContextValidationVersion is the deterministic validation report schema
	ContextValidationVersion uint32 = 1
)

// ContextValidationFailure identifies one preservation invariant that failed
type ContextValidationFailure string

// Deterministic Context validation failure codes
const (
	// ContextValidationActiveConstraints reports missing active Constraint Facts
	ContextValidationActiveConstraints ContextValidationFailure = "active_constraints"
	// ContextValidationModifiedArtifacts reports missing current modified artifacts
	ContextValidationModifiedArtifacts ContextValidationFailure = "modified_artifacts"
	// ContextValidationFailingChecks reports missing current failing checks
	ContextValidationFailingChecks ContextValidationFailure = "failing_checks"
	// ContextValidationPendingTools reports missing pending Tool transactions
	ContextValidationPendingTools ContextValidationFailure = "pending_tools"
	// ContextValidationEvidence reports unavailable required Evidence Objects
	ContextValidationEvidence ContextValidationFailure = "evidence"
)

// ContextValidationRollback identifies the safe view selected after candidate failure
type ContextValidationRollback string

// Context validation rollback targets
const (
	// ContextValidationRollbackPrevious keeps the last validated Checkpoint and raw tail
	ContextValidationRollbackPrevious ContextValidationRollback = "previous_checkpoint"
	// ContextValidationRollbackRaw uses the complete raw model context without a Checkpoint
	ContextValidationRollbackRaw ContextValidationRollback = "raw_context"
)

// ContextPreservationCount records deterministic required and preserved totals
type ContextPreservationCount struct {
	// Required is the number of items that must remain reconstructable
	Required int `json:"required"`
	// Preserved is the number of required items present in the candidate Context Program
	Preserved int `json:"preserved"`
	// RequiredBytes is the exact required byte total when the subject has a byte identity
	RequiredBytes int64 `json:"required_bytes,omitempty"`
	// PreservedBytes is the exact preserved byte total when the subject has a byte identity
	PreservedBytes int64 `json:"preserved_bytes,omitempty"`
}

// ContextValidationReport describes deterministic Checkpoint preservation checks
//
// Counts contain no prompt text, paths, commands, or object content. Passed
// describes the candidate before Rollback; a non-empty Rollback means Runtime
// safely selected an earlier validated view after candidate failure.
type ContextValidationReport struct {
	// Version identifies the report schema
	Version uint32 `json:"version"`
	// CandidateGeneration identifies the candidate Checkpoint generation
	CandidateGeneration uint64 `json:"candidate_generation,omitempty"`
	// CandidateSourceMessageCount identifies the candidate compacted raw prefix
	CandidateSourceMessageCount int `json:"candidate_source_message_count,omitempty"`
	// Passed reports whether every deterministic preservation invariant succeeded
	Passed bool `json:"passed"`
	// ActiveConstraints counts active Constraint Facts preserved by Checkpoint or raw tail
	ActiveConstraints ContextPreservationCount `json:"active_constraints"`
	// ModifiedArtifacts counts canonical current Git changes retained in Current World State
	ModifiedArtifacts ContextPreservationCount `json:"modified_artifacts"`
	// FailingChecks counts failed observations retained in Current World State
	FailingChecks ContextPreservationCount `json:"failing_checks"`
	// PendingTools counts pending Tool transactions retained in the raw tail
	PendingTools ContextPreservationCount `json:"pending_tools"`
	// Evidence counts required content-addressed objects that remain available
	Evidence ContextPreservationCount `json:"evidence"`
	// Failures contains stable content-free failed invariant codes
	Failures []ContextValidationFailure `json:"failures,omitempty"`
	// Rollback identifies the safe effective view selected after candidate failure
	Rollback ContextValidationRollback `json:"rollback,omitempty"`
}

func (compiler *CompactingContextCompiler) validateContextCandidate(
	ctx context.Context,
	access *EvidenceAccess,
	checkpoint *ContextCheckpoint,
	messages []Message,
	ledger *ContextLedger,
	worldState *CurrentWorldState,
	requiredEvidence []EvidenceObjectRef,
	preservedEvidence []EvidenceObjectRef,
) (ContextValidationReport, error) {
	if ctx == nil {
		return ContextValidationReport{}, errors.New("Context validation context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return ContextValidationReport{}, err
	}
	report := ContextValidationReport{Version: ContextValidationVersion}
	if checkpoint != nil {
		report.CandidateGeneration = checkpoint.Generation
		report.CandidateSourceMessageCount = checkpoint.SourceMessageCount
	}
	report.ActiveConstraints = validateActiveConstraintPreservation(checkpoint, messages, ledger)
	report.ModifiedArtifacts, report.FailingChecks = validateCurrentWorldPreservation(worldState)
	report.PendingTools = validatePendingToolPreservation(checkpoint, messages, ledger)
	var err error
	report.Evidence, err = compiler.validateEvidencePreservation(
		ctx,
		access,
		requiredEvidence,
		preservedEvidence,
	)
	if err != nil {
		return ContextValidationReport{}, err
	}
	checks := []struct {
		failure ContextValidationFailure
		count   ContextPreservationCount
	}{
		{ContextValidationActiveConstraints, report.ActiveConstraints},
		{ContextValidationModifiedArtifacts, report.ModifiedArtifacts},
		{ContextValidationFailingChecks, report.FailingChecks},
		{ContextValidationPendingTools, report.PendingTools},
		{ContextValidationEvidence, report.Evidence},
	}
	for _, check := range checks {
		if check.count.Preserved != check.count.Required ||
			check.count.PreservedBytes != check.count.RequiredBytes {
			report.Failures = append(report.Failures, check.failure)
		}
	}
	report.Passed = len(report.Failures) == 0
	return report, nil
}

func validateActiveConstraintPreservation(
	checkpoint *ContextCheckpoint,
	messages []Message,
	ledger *ContextLedger,
) ContextPreservationCount {
	checkpointSources := make(map[int]string)
	if checkpoint != nil {
		if checkpoint.Goal != nil {
			checkpointSources[checkpoint.Goal.SourceMessage] = checkpoint.Goal.ContentHash
		}
		for _, fact := range checkpoint.Facts {
			checkpointSources[fact.SourceMessage] = fact.ContentHash
		}
	}
	sourceCount := checkpointSourceCount(checkpoint)
	result := ContextPreservationCount{}
	if ledger == nil {
		return result
	}
	for _, constraint := range ledger.Constraints {
		if constraint.State != FactActive {
			continue
		}
		result.Required++
		if constraint.SourceMessage >= sourceCount {
			if messageMatchesConstraint(messages, constraint) {
				result.Preserved++
			}
			continue
		}
		if checkpointSources[constraint.SourceMessage] == constraint.ContentHash {
			result.Preserved++
		}
	}
	return result
}

func messageMatchesConstraint(messages []Message, constraint ConstraintLedgerEntry) bool {
	if constraint.SourceMessage < 0 || constraint.SourceMessage >= len(messages) {
		return false
	}
	message := messages[constraint.SourceMessage]
	if message.Role != RoleUser {
		return false
	}
	digest, err := checkpointMessageHash(message)
	return err == nil && digest == constraint.ContentHash
}

func validateCurrentWorldPreservation(
	worldState *CurrentWorldState,
) (ContextPreservationCount, ContextPreservationCount) {
	modified := ContextPreservationCount{}
	failing := ContextPreservationCount{}
	if worldState == nil {
		return modified, failing
	}
	if git := worldState.Snapshot.Git; git != nil && git.Available {
		modified.Required = len(git.Changes)
		modified.Preserved = len(git.Changes)
	}
	for _, check := range worldState.Snapshot.Checks {
		if check.Status == CurrentWorldCheckFailed {
			failing.Required++
			failing.Preserved++
		}
	}
	return modified, failing
}

func validatePendingToolPreservation(
	checkpoint *ContextCheckpoint,
	messages []Message,
	ledger *ContextLedger,
) ContextPreservationCount {
	result := ContextPreservationCount{}
	if ledger == nil {
		pending := pendingToolCalls(messages)
		result.Required = len(pending)
		result.Preserved = len(pending)
		return result
	}
	sourceCount := checkpointSourceCount(checkpoint)
	for _, execution := range ledger.Executions {
		if execution.Kind != ExecutionLedgerToolCall || execution.State != ExecutionLedgerPending {
			continue
		}
		result.Required++
		if rawTailContainsToolCall(messages[sourceCount:], execution.CallID, execution.Name) {
			result.Preserved++
		}
	}
	return result
}

func pendingToolCalls(messages []Message) map[string]string {
	pending := make(map[string]string)
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			pending[call.ID] = call.Name
		}
		if message.Role == RoleTool {
			delete(pending, message.ToolCallID)
		}
	}
	return pending
}

func rawTailContainsToolCall(messages []Message, callID, name string) bool {
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if call.ID == callID && call.Name == name {
				return true
			}
		}
	}
	return false
}

func (compiler *CompactingContextCompiler) validateEvidencePreservation(
	ctx context.Context,
	access *EvidenceAccess,
	required []EvidenceObjectRef,
	preserved []EvidenceObjectRef,
) (ContextPreservationCount, error) {
	result := ContextPreservationCount{}
	preservedByIdentity := make(map[string]EvidenceObjectRef, len(preserved))
	for _, reference := range uniqueEvidenceRefs(preserved) {
		preservedByIdentity[reference.Identity()] = reference
	}
	for _, reference := range uniqueEvidenceRefs(required) {
		if err := ctx.Err(); err != nil {
			return ContextPreservationCount{}, err
		}
		if reference.Bytes < 0 || result.RequiredBytes > math.MaxInt64-reference.Bytes {
			return ContextPreservationCount{}, errors.New("Context validation Evidence byte total overflows")
		}
		result.Required++
		result.RequiredBytes += reference.Bytes
		candidate, exists := preservedByIdentity[reference.Identity()]
		if !exists || !evidenceObjectRefsEqual(candidate, reference) {
			continue
		}
		stored, err := compiler.getEvidenceObject(ctx, access, candidate)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ContextPreservationCount{}, ctxErr
			}
			continue
		}
		if !strings.EqualFold(reference.Digest, sha256Digest(stored)) ||
			reference.Bytes != int64(len(stored)) {
			continue
		}
		result.Preserved++
		result.PreservedBytes += reference.Bytes
	}
	return result, nil
}

func validateContextValidationReport(report ContextValidationReport) error {
	if report.Version != ContextValidationVersion {
		return fmt.Errorf("Context validation version = %d, want %d", report.Version, ContextValidationVersion)
	}
	counts := []ContextPreservationCount{
		report.ActiveConstraints,
		report.ModifiedArtifacts,
		report.FailingChecks,
		report.PendingTools,
		report.Evidence,
	}
	for _, count := range counts {
		if count.Required < 0 || count.Preserved < 0 || count.Preserved > count.Required ||
			count.RequiredBytes < 0 || count.PreservedBytes < 0 || count.PreservedBytes > count.RequiredBytes {
			return errors.New("Context validation contains invalid preservation counts")
		}
	}
	for _, count := range counts[:len(counts)-1] {
		if count.RequiredBytes != 0 || count.PreservedBytes != 0 {
			return errors.New("Context validation contains byte totals for a non-Evidence subject")
		}
	}
	knownFailures := map[ContextValidationFailure]ContextPreservationCount{
		ContextValidationActiveConstraints: report.ActiveConstraints,
		ContextValidationModifiedArtifacts: report.ModifiedArtifacts,
		ContextValidationFailingChecks:     report.FailingChecks,
		ContextValidationPendingTools:      report.PendingTools,
		ContextValidationEvidence:          report.Evidence,
	}
	expectedFailures := make([]ContextValidationFailure, 0, len(counts))
	for failure, count := range knownFailures {
		if count.Preserved != count.Required || count.PreservedBytes != count.RequiredBytes {
			expectedFailures = append(expectedFailures, failure)
		}
	}
	sort.Slice(expectedFailures, func(first, second int) bool {
		return expectedFailures[first] < expectedFailures[second]
	})
	failures := append([]ContextValidationFailure(nil), report.Failures...)
	sort.Slice(failures, func(first, second int) bool { return failures[first] < failures[second] })
	for index, failure := range failures {
		if index > 0 && failure == failures[index-1] {
			return errors.New("Context validation contains duplicate failure codes")
		}
		if _, supported := knownFailures[failure]; !supported {
			return fmt.Errorf("Context validation contains unsupported failure %q", failure)
		}
	}
	if len(failures) != len(expectedFailures) {
		return errors.New("Context validation failure codes do not match preservation counts")
	}
	for index := range failures {
		if failures[index] != expectedFailures[index] {
			return errors.New("Context validation failure codes do not match preservation counts")
		}
	}
	wantPassed := len(expectedFailures) == 0
	if report.Passed != wantPassed {
		return errors.New("Context validation outcome does not match preservation counts")
	}
	if report.Passed && report.Rollback != "" {
		return errors.New("passed Context validation contains rollback")
	}
	switch report.Rollback {
	case "", ContextValidationRollbackPrevious, ContextValidationRollbackRaw:
	default:
		return fmt.Errorf("Context validation contains unsupported rollback %q", report.Rollback)
	}
	return nil
}

func validateCompiledContextValidation(
	report *ContextCompactionReport,
	checkpoint *ContextCheckpoint,
	previous *ContextCheckpoint,
) error {
	if report == nil || report.Validation == nil {
		return nil
	}
	validation := report.Validation
	if err := validateContextValidationReport(*validation); err != nil {
		return err
	}
	if validation.CandidateGeneration == 0 || validation.CandidateSourceMessageCount <= 0 {
		return errors.New("Context validation requires a Checkpoint candidate identity")
	}
	if validation.Passed {
		if !report.Applied || checkpoint == nil {
			return errors.New("passed Context validation requires an applied Checkpoint")
		}
		if checkpoint.Generation != validation.CandidateGeneration ||
			checkpoint.SourceMessageCount != validation.CandidateSourceMessageCount ||
			report.SourceMessageCount != validation.CandidateSourceMessageCount {
			return errors.New("Context validation candidate identity does not match its Checkpoint")
		}
		if previous != nil && contextCheckpointsEqual(previous, checkpoint) {
			return errors.New("passed Context validation did not produce a new Checkpoint")
		}
		wantGeneration := uint64(1)
		if previous != nil {
			if previous.Generation == ^uint64(0) {
				return errors.New("passed Context validation follows an exhausted Checkpoint generation")
			}
			wantGeneration = previous.Generation + 1
		}
		if validation.CandidateGeneration != wantGeneration {
			return errors.New("Context validation candidate does not follow the active Checkpoint generation")
		}
		return nil
	}
	if validation.Rollback == "" || report.Reason != "validation_rollback" {
		return errors.New("failed Context validation requires an explicit rollback")
	}
	if report.Rebased || report.RebaseReason != "" {
		return errors.New("Context validation rollback must not publish a Raw Event Rebase")
	}
	switch validation.Rollback {
	case ContextValidationRollbackPrevious:
		if previous == nil || !contextCheckpointsEqual(previous, checkpoint) {
			return errors.New("Context validation previous rollback does not retain the active Checkpoint")
		}
		if report.SourceMessageCount != previous.SourceMessageCount || previous.Generation == ^uint64(0) ||
			validation.CandidateGeneration != previous.Generation+1 {
			return errors.New("Context validation previous rollback does not match its candidate transition")
		}
	case ContextValidationRollbackRaw:
		if previous != nil || checkpoint != nil || report.SourceMessageCount != 0 ||
			validation.CandidateGeneration != 1 {
			return errors.New("Context validation raw rollback must not retain a Checkpoint")
		}
	default:
		return fmt.Errorf("Context validation contains unsupported rollback %q", validation.Rollback)
	}
	return nil
}

func validateContextValidationEvent(event Event) error {
	report := event.ContextCompaction
	if report == nil || report.Validation == nil {
		return nil
	}
	validation := report.Validation
	if err := validateContextValidationReport(*validation); err != nil {
		return err
	}
	if validation.CandidateGeneration == 0 || validation.CandidateSourceMessageCount <= 0 {
		return errors.New("context.compacted validation requires a Checkpoint candidate identity")
	}
	if validation.Passed {
		if !report.Applied || event.ContextCheckpoint == nil {
			return errors.New("context.compacted passed validation requires an applied Checkpoint")
		}
		if event.ContextCheckpoint.Generation != validation.CandidateGeneration ||
			event.ContextCheckpoint.SourceMessageCount != validation.CandidateSourceMessageCount ||
			report.SourceMessageCount != validation.CandidateSourceMessageCount {
			return errors.New("context.compacted validation candidate identity does not match its Checkpoint")
		}
		return nil
	}
	if validation.Rollback == "" || report.Reason != "validation_rollback" {
		return errors.New("context.compacted failed validation requires an explicit rollback")
	}
	if event.ContextCheckpoint != nil {
		return errors.New("context.compacted validation rollback must not publish its failed Checkpoint")
	}
	if report.Rebased || report.RebaseReason != "" {
		return errors.New("context.compacted validation rollback must not publish a Raw Event Rebase")
	}
	return nil
}

func validateContextValidationTransition(event Event, active *ContextCheckpoint) error {
	if err := validateContextValidationEvent(event); err != nil {
		return err
	}
	report := event.ContextCompaction
	if report == nil || report.Validation == nil {
		return nil
	}
	validation := report.Validation
	if validation.Passed {
		wantGeneration := uint64(1)
		if active != nil {
			if active.Generation == ^uint64(0) {
				return errors.New("context.compacted validation follows an exhausted Checkpoint generation")
			}
			wantGeneration = active.Generation + 1
		}
		if validation.CandidateGeneration != wantGeneration {
			return errors.New("context.compacted validation candidate does not follow the active Checkpoint generation")
		}
		return nil
	}
	switch validation.Rollback {
	case ContextValidationRollbackPrevious:
		if active == nil || report.SourceMessageCount != active.SourceMessageCount {
			return errors.New("context.compacted previous rollback has no matching active Checkpoint")
		}
		if active.Generation == ^uint64(0) || validation.CandidateGeneration != active.Generation+1 {
			return errors.New("context.compacted rollback candidate does not follow the active Checkpoint generation")
		}
	case ContextValidationRollbackRaw:
		if active != nil || report.SourceMessageCount != 0 || validation.CandidateGeneration != 1 {
			return errors.New("context.compacted raw rollback does not match an uncheckpointed context")
		}
	}
	return nil
}

func validateContextPreparationTransition(event Event, active *ContextCheckpoint) error {
	if event.Type != EventContextCompactionPrepared || event.ContextCheckpoint == nil ||
		event.ContextCompaction == nil || event.ContextCompaction.Validation == nil {
		return errors.New("context.compaction.prepared requires a validated Checkpoint candidate")
	}
	if !event.ContextCompaction.Validation.Passed || event.ContextCompaction.Validation.Rollback != "" {
		return errors.New("context.compaction.prepared requires passed validation without rollback")
	}
	return validateCompiledContextValidation(
		event.ContextCompaction,
		event.ContextCheckpoint,
		active,
	)
}

func cloneContextValidationReport(report *ContextValidationReport) *ContextValidationReport {
	if report == nil {
		return nil
	}
	cloned := *report
	cloned.Failures = append([]ContextValidationFailure(nil), report.Failures...)
	return &cloned
}

func contextValidationFailureError(report ContextValidationReport) error {
	values := make([]string, len(report.Failures))
	for index, failure := range report.Failures {
		values[index] = string(failure)
	}
	return fmt.Errorf("Context Checkpoint failed deterministic validation: %s", strings.Join(values, ","))
}
