package agent

import (
	"context"
	"testing"
)

func TestValidateContextCandidatePreservesPendingToolOnlyInRawTail(t *testing.T) {
	t.Parallel()

	messages := []Message{
		{Role: RoleUser, Text: "constraint"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "pending-call", Name: "command"}}},
	}
	ledger := &ContextLedger{Executions: []ExecutionLedgerEntry{{
		Kind: ExecutionLedgerToolCall, State: ExecutionLedgerPending,
		CallID: "pending-call", Name: "command",
	}}}
	compiler := &CompactingContextCompiler{}
	report, err := compiler.validateContextCandidate(
		context.Background(),
		&ContextCheckpoint{Generation: 1, SourceMessageCount: 1},
		messages,
		ledger,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.PendingTools != (ContextPreservationCount{Required: 1, Preserved: 1}) {
		t.Fatalf("raw-tail pending Tool report = %#v", report)
	}
	report, err = compiler.validateContextCandidate(
		context.Background(),
		&ContextCheckpoint{Generation: 1, SourceMessageCount: 2},
		messages,
		ledger,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.PendingTools != (ContextPreservationCount{Required: 1}) ||
		len(report.Failures) != 1 || report.Failures[0] != ContextValidationPendingTools {
		t.Fatalf("compacted pending Tool report = %#v", report)
	}
}

func TestValidateContextValidationEvent(t *testing.T) {
	t.Parallel()

	passed := ContextValidationReport{
		Version: ContextValidationVersion, CandidateGeneration: 1, CandidateSourceMessageCount: 1,
		Passed: true,
		Evidence: ContextPreservationCount{
			Required: 1, Preserved: 1, RequiredBytes: 10, PreservedBytes: 10,
		},
	}
	failed := ContextValidationReport{
		Version: ContextValidationVersion, CandidateGeneration: 2, CandidateSourceMessageCount: 2,
		ActiveConstraints: ContextPreservationCount{Required: 2, Preserved: 1},
		Failures:          []ContextValidationFailure{ContextValidationActiveConstraints},
		Rollback:          ContextValidationRollbackPrevious,
	}
	tests := map[string]struct {
		event   Event
		wantErr bool
	}{
		"passed candidate": {
			event: Event{
				ContextCheckpoint: &ContextCheckpoint{Generation: 1, SourceMessageCount: 1},
				ContextCompaction: &ContextCompactionReport{
					Applied: true, SourceMessageCount: 1, Validation: cloneContextValidationReport(&passed),
				},
			},
		},
		"failed candidate rollback": {
			event: Event{ContextCompaction: &ContextCompactionReport{
				Reason: "validation_rollback", Validation: cloneContextValidationReport(&failed),
			}},
		},
		"count and failure mismatch": {
			event: Event{ContextCompaction: &ContextCompactionReport{
				Reason: "validation_rollback",
				Validation: &ContextValidationReport{
					Version: ContextValidationVersion, CandidateGeneration: 2, CandidateSourceMessageCount: 2,
					ActiveConstraints: ContextPreservationCount{Required: 2, Preserved: 1},
					Rollback:          ContextValidationRollbackPrevious,
				},
			}},
			wantErr: true,
		},
		"failed candidate without rollback": {
			event: Event{ContextCompaction: &ContextCompactionReport{
				Reason: "validation_rollback",
				Validation: func() *ContextValidationReport {
					report := failed
					report.Rollback = ""
					return &report
				}(),
			}},
			wantErr: true,
		},
		"failed candidate is published": {
			event: Event{
				ContextCheckpoint: &ContextCheckpoint{Generation: 2, SourceMessageCount: 2},
				ContextCompaction: &ContextCompactionReport{
					Reason: "validation_rollback", Validation: cloneContextValidationReport(&failed),
				},
			},
			wantErr: true,
		},
		"passed candidate identity mismatch": {
			event: Event{
				ContextCheckpoint: &ContextCheckpoint{Generation: 2, SourceMessageCount: 1},
				ContextCompaction: &ContextCompactionReport{
					Applied: true, SourceMessageCount: 1, Validation: cloneContextValidationReport(&passed),
				},
			},
			wantErr: true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := validateContextValidationEvent(test.event)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateContextValidationEvent() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestValidateContextValidationTransition(t *testing.T) {
	t.Parallel()

	previous := &ContextCheckpoint{Generation: 1, SourceMessageCount: 3}
	failed := ContextValidationReport{
		Version: ContextValidationVersion, CandidateGeneration: 2, CandidateSourceMessageCount: 4,
		ActiveConstraints: ContextPreservationCount{Required: 2, Preserved: 1},
		Failures:          []ContextValidationFailure{ContextValidationActiveConstraints},
		Rollback:          ContextValidationRollbackPrevious,
	}
	previousRollback := Event{ContextCompaction: &ContextCompactionReport{
		Reason: "validation_rollback", SourceMessageCount: 3, Validation: &failed,
	}}
	if err := validateContextValidationTransition(previousRollback, previous); err != nil {
		t.Fatalf("previous rollback transition error = %v", err)
	}
	if err := validateContextValidationTransition(previousRollback, nil); err == nil {
		t.Fatal("previous rollback without an active Checkpoint succeeded")
	}
	raw := failed
	raw.CandidateGeneration = 1
	raw.Rollback = ContextValidationRollbackRaw
	rawRollback := Event{ContextCompaction: &ContextCompactionReport{
		Reason: "validation_rollback", Validation: &raw,
	}}
	if err := validateContextValidationTransition(rawRollback, nil); err != nil {
		t.Fatalf("raw rollback transition error = %v", err)
	}
	if err := validateContextValidationTransition(rawRollback, previous); err == nil {
		t.Fatal("raw rollback with an active Checkpoint succeeded")
	}
}
