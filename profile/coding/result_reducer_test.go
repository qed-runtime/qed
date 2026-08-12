package coding

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/orchestration"
)

func TestCodingResultReducerProjectsDomainStateOutsideRuntimeCore(t *testing.T) {
	t.Parallel()

	runID := "run-coding-result"
	priorCheckRef := agent.ContextLedgerEventRef{RunID: "run-prior", Sequence: 7}
	checkRef := agent.ContextLedgerEventRef{RunID: runID, Sequence: 2}
	captureRef := agent.ContextLedgerEventRef{RunID: runID, Sequence: 3}
	fileDigest := worldTestDigest([]byte("package sample\n"))
	diffDigest := worldTestDigest([]byte("diff --git a/main.go b/main.go\n"))
	outputDigest := worldTestDigest([]byte("ok"))
	ledger := &agent.ContextLedger{
		Version:          agent.ContextLedgerVersion,
		SourceEventCount: 4,
		Digest:           worldTestDigest([]byte("ledger")),
		SourceHash:       worldTestDigest([]byte("sources")),
		Sources: []agent.ContextLedgerSource{
			{ContextLedgerEventRef: priorCheckRef, Type: agent.EventToolCompleted},
			{ContextLedgerEventRef: agent.ContextLedgerEventRef{RunID: runID, Sequence: 1}, Type: agent.EventRunStarted},
			{ContextLedgerEventRef: checkRef, Type: agent.EventToolCompleted},
			{ContextLedgerEventRef: captureRef, Type: agent.EventCurrentWorldStateCaptured},
		},
	}
	world := &agent.CurrentWorldState{
		Version: agent.CurrentWorldStateVersion,
		Source: agent.ContextLedgerReference{
			Version:          agent.ContextLedgerVersion,
			SourceEventCount: 3,
		},
		Snapshot: agent.CurrentWorldStateSnapshot{
			FilesAvailable: true,
			Files: []agent.CurrentWorldFile{{
				Path: "main.go", Status: agent.CurrentWorldFilePresent, Digest: fileDigest, Bytes: 15,
			}},
			Git: &agent.CurrentWorldGitState{
				Available: true,
				Changes: []agent.CurrentWorldGitChange{{
					Path: "main.go", Kind: "ordinary", IndexStatus: " ", WorktreeStatus: "M",
				}},
				DiffDigest: diffDigest,
				DiffBytes:  35,
			},
			Checks: []agent.CurrentWorldCheck{{
				Argv: []string{"go", "test", "./..."}, CWD: ".",
				Status: agent.CurrentWorldCheckPassed, Freshness: agent.CurrentWorldCheckCurrent,
				OutputDigest: outputDigest, Source: checkRef,
			}, {
				Argv: []string{"go", "vet", "./..."}, CWD: ".",
				Status: agent.CurrentWorldCheckPassed, Freshness: agent.CurrentWorldCheckStale,
				OutputDigest: outputDigest, Source: priorCheckRef,
			}},
		},
	}
	request := orchestration.ResultReductionRequest{
		AgentID: "coding-worker",
		Result: agent.RunResult{
			RunID: runID, AgentID: "coding-worker", Status: agent.RunStatusCompleted,
			ContextLedger: ledger, CurrentWorldState: world,
		},
	}
	reduction, err := (ResultPacketReducer{}).ReduceResult(context.Background(), request)
	if err != nil {
		t.Fatalf("ReduceResult() error = %v", err)
	}
	if len(reduction.Facts) != 1 || reduction.Facts[0].Kind != ResultFactGitChange ||
		len(reduction.Facts[0].Sources) != 1 || reduction.Facts[0].Sources[0] != captureRef {
		t.Errorf("Coding result Facts = %#v", reduction.Facts)
	}
	if len(reduction.Artifacts) != 2 || reduction.Artifacts[0].Kind != ResultArtifactGitDiff ||
		reduction.Artifacts[1].Kind != ResultArtifactFile || reduction.Artifacts[1].Name != "main.go" {
		t.Errorf("Coding result Artifacts = %#v", reduction.Artifacts)
	}
	if len(reduction.Executions) != 1 || reduction.Executions[0].Kind != ResultExecutionCheck ||
		reduction.Executions[0].State != orchestration.ResultExecutionSucceeded ||
		reduction.Executions[0].RunID != runID || reduction.Executions[0].Sources[0] != checkRef {
		t.Errorf("Coding result Executions = %#v", reduction.Executions)
	}
	var state ResultState
	if err := json.Unmarshal(reduction.ProfileState, &state); err != nil {
		t.Fatalf("decode Coding ResultState: %v", err)
	}
	if state.Version != ResultStateVersion || state.CurrentWorldState == nil ||
		state.CurrentWorldState.Snapshot.Git == nil ||
		state.CurrentWorldState.Snapshot.Git.DiffDigest != diffDigest {
		t.Errorf("Coding ResultState = %#v", state)
	}

	var profile Profile
	if _, ok := profile.ResultReducer().(ResultPacketReducer); !ok {
		t.Fatalf("Profile.ResultReducer() = %T", profile.ResultReducer())
	}
}
