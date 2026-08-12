package coding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/orchestration"
)

const (
	// ResultStateVersion is the Coding Profile state schema embedded in Result Packets
	ResultStateVersion uint32 = 1
	// ResultFactGitChange identifies one canonical Coding Profile Git change
	ResultFactGitChange = "coding.git_change"
	// ResultArtifactFile identifies one canonical changed-file snapshot
	ResultArtifactFile = "coding.file"
	// ResultArtifactGitDiff identifies one canonical bounded Git diff
	ResultArtifactGitDiff = "coding.git_diff"
	// ResultExecutionCheck identifies one canonical Coding Profile command check
	ResultExecutionCheck = "coding.check"
)

// ResultState contains Coding Profile state without adding coding fields to Runtime Core
//
// CurrentWorldState is content-bearing untrusted data. It contains only
// workspace-relative paths and bounded state already supplied to the child Run.
type ResultState struct {
	// Version identifies the Coding Profile result state schema
	Version uint32 `json:"version"`
	// CurrentWorldState is the latest canonical state observed by the child Run
	CurrentWorldState *agent.CurrentWorldState `json:"current_world_state,omitempty"`
}

// ResultPacketReducer adds Coding Profile facts, artifacts, checks, and state
// to the provider-neutral Ledger reduction
type ResultPacketReducer struct{}

// ReduceResult projects canonical Coding Profile state from one child Run
func (ResultPacketReducer) ReduceResult(
	ctx context.Context,
	request orchestration.ResultReductionRequest,
) (orchestration.ResultReduction, error) {
	if ctx == nil {
		return orchestration.ResultReduction{}, errors.New("Coding Result reducer context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return orchestration.ResultReduction{}, err
	}
	reduction, err := (orchestration.LedgerResultReducer{}).ReduceResult(ctx, request)
	if err != nil {
		return orchestration.ResultReduction{}, err
	}
	world := request.Result.CurrentWorldState
	if world == nil {
		return reduction, nil
	}
	captureSource, err := codingResultWorldSource(request.Result, *world)
	if err != nil {
		return orchestration.ResultReduction{}, err
	}
	state, err := json.Marshal(ResultState{
		Version:           ResultStateVersion,
		CurrentWorldState: world,
	})
	if err != nil {
		return orchestration.ResultReduction{}, err
	}
	reduction.ProfileState = state

	changedPaths := make(map[string]struct{})
	if world.Snapshot.Git != nil {
		for _, change := range world.Snapshot.Git.Changes {
			value, err := json.Marshal(change)
			if err != nil {
				return orchestration.ResultReduction{}, err
			}
			reduction.Facts = append(reduction.Facts, orchestration.ResultFact{
				Kind:    ResultFactGitChange,
				Value:   value,
				Sources: []agent.ContextLedgerEventRef{captureSource},
			})
			changedPaths[change.Path] = struct{}{}
		}
		if world.Snapshot.Git.DiffDigest != "" {
			reduction.Artifacts = append(reduction.Artifacts, orchestration.ResultArtifact{
				Kind:      ResultArtifactGitDiff,
				Name:      "worktree.diff",
				Digest:    world.Snapshot.Git.DiffDigest,
				Bytes:     world.Snapshot.Git.DiffBytes,
				MediaType: "text/x-diff; charset=utf-8",
				Sources:   []agent.ContextLedgerEventRef{captureSource},
			})
		}
	}
	for _, file := range world.Snapshot.Files {
		if _, changed := changedPaths[file.Path]; !changed || file.Status != agent.CurrentWorldFilePresent {
			continue
		}
		reduction.Artifacts = append(reduction.Artifacts, orchestration.ResultArtifact{
			Kind:      ResultArtifactFile,
			Name:      file.Path,
			Digest:    file.Digest,
			Bytes:     file.Bytes,
			MediaType: "application/octet-stream",
			Sources:   []agent.ContextLedgerEventRef{captureSource},
		})
	}
	for _, check := range world.Snapshot.Checks {
		if check.Source.RunID != request.Result.RunID {
			continue
		}
		state := orchestration.ResultExecutionFailed
		if check.Status == agent.CurrentWorldCheckPassed {
			state = orchestration.ResultExecutionSucceeded
		}
		name := "command"
		if len(check.Argv) > 0 {
			name = check.Argv[0]
		}
		reduction.Executions = append(reduction.Executions, orchestration.ResultExecution{
			Kind:            ResultExecutionCheck,
			Name:            name,
			State:           state,
			RunID:           check.Source.RunID,
			ArgumentsDigest: codingResultCheckDigest(check),
			OutputDigest:    check.OutputDigest,
			Sources:         []agent.ContextLedgerEventRef{check.Source},
		})
	}
	return reduction, nil
}

func codingResultWorldSource(
	result agent.RunResult,
	world agent.CurrentWorldState,
) (agent.ContextLedgerEventRef, error) {
	if result.ContextLedger == nil || world.Source.SourceEventCount < 0 ||
		world.Source.SourceEventCount >= len(result.ContextLedger.Sources) {
		return agent.ContextLedgerEventRef{}, errors.New("Coding Result state has no capture Event")
	}
	source := result.ContextLedger.Sources[world.Source.SourceEventCount]
	if source.Type != agent.EventCurrentWorldStateCaptured {
		return agent.ContextLedgerEventRef{}, errors.New("Coding Result state source is not followed by a capture Event")
	}
	return source.ContextLedgerEventRef, nil
}

func codingResultCheckDigest(check agent.CurrentWorldCheck) string {
	encoded, _ := json.Marshal(struct {
		Argv []string `json:"argv"`
		CWD  string   `json:"cwd"`
	}{Argv: check.Argv, CWD: check.CWD})
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}
