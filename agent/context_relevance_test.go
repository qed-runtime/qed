package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/session"
)

func TestRuntimeContextSearchUsesInjectedSemanticScorer(t *testing.T) {
	t.Parallel()

	const sessionID = "context-relevance-runtime"
	store := session.NewMemoryStore()
	seed := []agent.Event{
		{RunID: "old", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "old", Sequence: 2, Type: agent.EventUserMessageAdded,
			Message: &agent.Message{Role: agent.RoleUser, Text: "shared semantic target"},
		},
		{RunID: "old", Sequence: 3, Type: agent.EventRunCompleted},
		{RunID: "recent", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "recent", Sequence: 2, Type: agent.EventUserMessageAdded,
			Message: &agent.Message{Role: agent.RoleUser, Text: "shared distractor item"},
		},
		{RunID: "recent", Sequence: 3, Type: agent.EventRunCompleted},
	}
	if _, err := store.Append(context.Background(), sessionID, 0, seed); err != nil {
		t.Fatal(err)
	}
	scorer := &semanticRelevanceRecorder{}
	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{
			ID: "rank", Name: agent.ContextSearchToolName,
			Arguments: json.RawMessage(`{"query":"shared","order":"relevance","limit":2}`),
		}}}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "done"}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider, SessionStore: store,
		ContextRetrieval: &agent.ContextRetrievalOptions{SemanticScorer: scorer},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID: "agent", SessionID: sessionID,
		Input: []agent.Message{{Role: agent.RoleUser, Text: "continue"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, runErr := collectRun(handle)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if len(result.ToolResults) != 1 || result.ToolResults[0].IsError ||
		result.ToolResults[0].ContextRetrieval == nil ||
		result.ToolResults[0].ContextRetrieval.Outcome != agent.ContextRetrievalSucceeded {
		t.Fatalf("relevance Tool result = %#v", result.ToolResults)
	}
	var response struct {
		Order               agent.ContextSearchOrder `json:"order"`
		SnapshotEventCount  int                      `json:"snapshot_event_count"`
		SnapshotQueryDigest string                   `json:"snapshot_query_digest"`
		Results             []struct {
			Source struct {
				RunID string `json:"run_id"`
			} `json:"source"`
			Snippet   string                       `json:"snippet"`
			Relevance *agent.ContextRelevanceScore `json:"relevance"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(result.ToolResults[0].Output), &response); err != nil {
		t.Fatal(err)
	}
	if response.Order != agent.ContextSearchOrderRelevance || response.SnapshotEventCount == 0 ||
		!strings.HasPrefix(response.SnapshotQueryDigest, "sha256:") ||
		len(response.Results) != 2 || response.Results[0].Source.RunID != "old" ||
		response.Results[0].Relevance == nil || !response.Results[0].Relevance.SemanticApplied {
		t.Fatalf("relevance response = %#v", response)
	}
	if err := agent.ValidateContextRelevanceScore(
		*response.Results[0].Relevance,
		len(response.Results[0].Snippet),
	); err != nil {
		t.Fatal(err)
	}
	request := scorer.Request()
	if request.Query != "shared" || len(request.Candidates) < 2 {
		t.Fatalf("semantic request = %#v", request)
	}
	snapshot, err := store.Snapshot(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.BuildContextLedger(context.Background(), snapshot.Events); err != nil {
		t.Fatalf("persisted relevance result did not replay: %v", err)
	}
}

type semanticRelevanceRecorder struct {
	mu      sync.Mutex
	request agent.ContextSemanticScoreRequest
}

func (scorer *semanticRelevanceRecorder) Score(
	_ context.Context,
	request agent.ContextSemanticScoreRequest,
) ([]int, error) {
	scorer.mu.Lock()
	scorer.request = request
	scorer.mu.Unlock()
	scores := make([]int, len(request.Candidates))
	for index, candidate := range request.Candidates {
		if strings.Contains(candidate.Text, "semantic target") {
			scores[index] = agent.ContextRelevanceSignalMaximum
		}
	}
	return scores, nil
}

func (scorer *semanticRelevanceRecorder) Request() agent.ContextSemanticScoreRequest {
	scorer.mu.Lock()
	defer scorer.mu.Unlock()
	return agent.ContextSemanticScoreRequest{
		Query:            scorer.request.Query,
		Task:             scorer.request.Task,
		TaskContentBytes: scorer.request.TaskContentBytes,
		TaskTruncated:    scorer.request.TaskTruncated,
		Candidates: append(
			[]agent.ContextSemanticCandidate(nil),
			scorer.request.Candidates...,
		),
	}
}

var _ agent.ContextSemanticScorer = (*semanticRelevanceRecorder)(nil)
