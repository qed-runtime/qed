package agent

import (
	"context"
	"strings"
	"testing"
)

func TestContextLedgerRejectsCurrentWorldStateDuringPendingWork(t *testing.T) {
	t.Parallel()

	call := ToolCall{ID: "call-1", Name: "write"}
	manifest, err := BuildPrefixManifest(PrefixManifestOptions{Provider: "scripted"}, []ContextSegment{
		newContextSegment("instructions", SegmentKindInstructions, StabilityProject, contextInstructionsPriority, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]Event{
		"Provider": {
			Type: EventModelRequest, ProviderCall: 1, ProviderAttempt: 1, PrefixManifest: &manifest,
		},
		"Tool": {Type: EventToolStarted, ToolCall: &call},
	}
	for name, pending := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pending.RunID = "run-1"
			pending.Sequence = 2
			events := []Event{
				{RunID: "run-1", Sequence: 1, Type: EventRunStarted},
				pending,
			}
			prefix, err := BuildContextLedger(context.Background(), events)
			if err != nil {
				t.Fatal(err)
			}
			state, err := buildCurrentWorldState(
				prefix.Reference(),
				CurrentWorldStateSnapshot{FilesAvailable: true},
				events,
			)
			if err != nil {
				t.Fatal(err)
			}
			events = append(events, Event{
				RunID: "run-1", Sequence: 3, Type: EventCurrentWorldStateCaptured, CurrentWorldState: &state,
			})
			if _, err := BuildContextLedger(context.Background(), events); err == nil ||
				!strings.Contains(err.Error(), "pending "+name+" call") {
				t.Fatalf("BuildContextLedger() error = %v", err)
			}
		})
	}
}

func TestValidateCurrentWorldStateRejectsInvalidProvenance(t *testing.T) {
	t.Parallel()

	call := ToolCall{ID: "call-1", Name: "run_command"}
	result := ToolResult{CallID: call.ID, Name: call.Name, Output: "structured output"}
	events := []Event{
		{RunID: "run-1", Sequence: 1, Type: EventRunStarted},
		{RunID: "run-1", Sequence: 2, Type: EventToolStarted, ToolCall: &call},
		{RunID: "run-1", Sequence: 3, Type: EventToolCompleted, ToolCall: &call, ToolResult: &result},
	}
	ledger, err := BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := CurrentWorldStateSnapshot{
		FilesAvailable: true,
		Checks: []CurrentWorldCheck{{
			Argv: []string{"check"}, CWD: ".", Status: CurrentWorldCheckPassed,
			Freshness: CurrentWorldCheckCurrent, OutputDigest: sha256Digest([]byte(result.Output)),
			Source: ContextLedgerEventRef{RunID: "run-1", Sequence: 3},
		}},
	}
	state, err := buildCurrentWorldState(ledger.Reference(), snapshot, events)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentWorldState(context.Background(), state, events); err != nil {
		t.Fatalf("ValidateCurrentWorldState() error = %v", err)
	}

	wrongSource := state
	wrongSource.Source.SourceEventCount--
	if err := ValidateCurrentWorldState(context.Background(), wrongSource, events); err == nil {
		t.Fatal("ValidateCurrentWorldState() accepted a mismatched source prefix")
	}

	snapshot.Checks[0].Source.Sequence = 99
	if _, err := buildCurrentWorldState(ledger.Reference(), snapshot, events); err == nil {
		t.Fatal("buildCurrentWorldState() accepted a missing Tool reference")
	}
	snapshot.Checks[0].Source.Sequence = 3
	snapshot.Checks[0].OutputDigest = sha256Digest([]byte("changed"))
	if _, err := buildCurrentWorldState(ledger.Reference(), snapshot, events); err == nil {
		t.Fatal("buildCurrentWorldState() accepted a mismatched Tool output")
	}
}

func TestBuildCurrentWorldStateRejectsInconsistentOrInvalidSnapshot(t *testing.T) {
	t.Parallel()

	tests := map[string]CurrentWorldStateSnapshot{
		"unavailable files": {
			Files: []CurrentWorldFile{{Path: "note.txt", Status: CurrentWorldFileAbsent}},
		},
		"invalid Git status": {
			FilesAvailable: true,
			Git: &CurrentWorldGitState{
				Available:  true,
				DiffDigest: sha256Digest(nil),
				Changes: []CurrentWorldGitChange{{
					Path: "note.txt", Kind: "ordinary", IndexStatus: string([]byte{0xff}), WorktreeStatus: ".",
				}},
			},
		},
	}
	for name, snapshot := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := buildCurrentWorldState(ContextLedgerReference{}, snapshot, nil); err == nil {
				t.Fatal("buildCurrentWorldState() accepted an invalid snapshot")
			}
		})
	}
}
