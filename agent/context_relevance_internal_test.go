package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestContextRelevanceSearchRanksAndPaginatesFrozenPrefix(t *testing.T) {
	t.Parallel()

	events := []Event{
		{RunID: "old", Sequence: 1, Type: EventRunStarted},
		{
			RunID: "old", Sequence: 2, Type: EventUserMessageAdded,
			Message: &Message{Role: RoleUser, Text: "Fix ParseManifest in agent/context_retrieval.go after parser timeout"},
		},
		{RunID: "old", Sequence: 3, Type: EventRunCompleted},
		{RunID: "recent", Sequence: 1, Type: EventRunStarted},
		{
			RunID: "recent", Sequence: 2, Type: EventUserMessageAdded,
			Message: &Message{Role: RoleUser, Text: "continue the current work"},
		},
		{RunID: "recent", Sequence: 3, Type: EventRunCompleted},
	}
	tool := contextRetrievalTool{operation: ContextRetrievalSearch}
	allowance := contextRetrievalAllowance{maxItems: 8, maxOutputBytes: 64 << 10}
	input := contextSearchInput{
		Query: "ParseManifest agent/context_retrieval.go", Order: ContextSearchOrderRelevance, Limit: 1,
	}
	first, err := tool.relevanceSearch(context.Background(), input, events, allowance)
	if err != nil {
		t.Fatal(err)
	}
	second, err := tool.relevanceSearch(context.Background(), input, events, allowance)
	if err != nil {
		t.Fatal(err)
	}
	if first.Output != second.Output {
		t.Fatalf("repeated relevance output changed:\n%s\n%s", first.Output, second.Output)
	}
	var page contextSearchResponse
	if err := json.Unmarshal([]byte(first.Output), &page); err != nil {
		t.Fatal(err)
	}
	if page.Order != ContextSearchOrderRelevance || page.SnapshotEventCount != len(events) ||
		!validSHA256Digest(page.SnapshotQueryDigest) ||
		len(page.Results) != 1 || page.Results[0].Source.RunID != "old" ||
		page.Results[0].Relevance == nil || page.Results[0].Relevance.Factors.Lexical == 0 ||
		page.Results[0].Relevance.Factors.File == 0 || page.Results[0].Relevance.Factors.Symbol == 0 {
		t.Fatalf("ranked page = %#v", page)
	}
	if err := ValidateContextRelevanceScore(*page.Results[0].Relevance, len(page.Results[0].Snippet)); err != nil {
		t.Fatal(err)
	}

	events = append(events,
		Event{RunID: "future", Sequence: 1, Type: EventRunStarted},
		Event{
			RunID: "future", Sequence: 2, Type: EventUserMessageAdded,
			Message: &Message{Role: RoleUser, Text: "ParseManifest agent/context_retrieval.go future exact result"},
		},
		Event{RunID: "future", Sequence: 3, Type: EventRunCompleted},
	)
	nextInput := input
	nextInput.SnapshotEventCount = page.SnapshotEventCount
	nextInput.SnapshotQueryDigest = page.SnapshotQueryDigest
	nextInput.Cursor = page.NextCursor
	next, err := tool.relevanceSearch(context.Background(), nextInput, events, allowance)
	if err != nil {
		t.Fatal(err)
	}
	var nextPage contextSearchResponse
	if err := json.Unmarshal([]byte(next.Output), &nextPage); err != nil {
		t.Fatal(err)
	}
	for _, result := range nextPage.Results {
		if result.Source.RunID == "future" {
			t.Fatalf("frozen snapshot included a later Event: %#v", nextPage)
		}
	}
	invalid := input
	invalid.Cursor = 1
	if _, err := tool.relevanceSearch(context.Background(), invalid, events, allowance); err == nil ||
		!strings.Contains(err.Error(), "snapshot_event_count") {
		t.Fatalf("missing snapshot error = %v", err)
	}
	invalid.SnapshotEventCount = page.SnapshotEventCount
	invalid.SnapshotQueryDigest = "sha256:" + strings.Repeat("0", 64)
	if _, err := tool.relevanceSearch(context.Background(), invalid, events, allowance); err == nil ||
		!strings.Contains(err.Error(), "does not match query") {
		t.Fatalf("changed query snapshot error = %v", err)
	}
}

func TestContextRelevanceSignalsIncludeLifecycleErrorAndReferences(t *testing.T) {
	t.Parallel()

	searchOutput := `{"version":1,"results":[{"source":{"run_id":"task","sequence":2,"event_type":"user.message.added"},"snippet":"old","untrusted":true}],"next_cursor":0,"truncated":false}`
	events := []Event{
		{RunID: "task", Sequence: 1, Type: EventRunStarted},
		{
			RunID: "task", Sequence: 2, Type: EventUserMessageAdded,
			Message: &Message{Role: RoleUser, Text: "inspect ParseManifest in agent/context_retrieval.go"},
		},
		{RunID: "task", Sequence: 3, Type: EventRunCompleted},
		{RunID: "search", Sequence: 1, Type: EventRunStarted},
		{
			RunID: "search", Sequence: 2, Type: EventToolStarted,
			ToolCall: &ToolCall{ID: "lookup", Name: ContextSearchToolName},
		},
		{
			RunID: "search", Sequence: 3, Type: EventToolCompleted,
			ToolCall: &ToolCall{ID: "lookup", Name: ContextSearchToolName},
			ToolResult: &ToolResult{
				CallID: "lookup", Name: ContextSearchToolName, Output: searchOutput,
				ContextRetrieval: &ContextRetrievalMetadata{
					Version: ContextRetrievalMetadataVersion, Operation: ContextRetrievalSearch,
					Outcome: ContextRetrievalSucceeded, ItemCount: 1, OutputBytes: int64(len(searchOutput)),
				},
			},
		},
		{RunID: "search", Sequence: 4, Type: EventRunCompleted},
		{RunID: "failed", Sequence: 1, Type: EventRunStarted},
		{RunID: "failed", Sequence: 2, Type: EventRunFailed, Error: "ParseManifest parser timeout"},
	}
	signals, err := buildContextRelevanceSignals(
		context.Background(),
		"ParseManifest agent/context_retrieval.go",
		events,
	)
	if err != nil {
		t.Fatal(err)
	}
	candidates, _, err := collectContextRelevanceCandidates(context.Background(), events, signals)
	if err != nil {
		t.Fatal(err)
	}
	byRun := make(map[string]ContextRelevanceScore)
	for index := range candidates {
		scoreContextRelevanceCandidate(&candidates[index], signals, len(events))
		byRun[candidates[index].event.RunID] = candidates[index].score
	}
	task := byRun["task"]
	if task.Factors.ActiveConstraint == 0 || task.Factors.ReferenceFrequency == 0 ||
		task.Factors.File == 0 || task.Factors.Symbol == 0 {
		t.Fatalf("task relevance factors = %#v", task)
	}
	failed := byRun["failed"]
	if failed.Factors.UnresolvedError == 0 {
		t.Fatalf("failed relevance factors = %#v", failed)
	}
}

func TestContextSemanticScorerIsOptionalBoundedAndValidated(t *testing.T) {
	t.Parallel()

	events := []Event{
		{RunID: "alpha", Sequence: 1, Type: EventRunStarted},
		{RunID: "alpha", Sequence: 2, Type: EventUserMessageAdded, Message: &Message{Role: RoleUser, Text: strings.Repeat("shared alpha ", 100)}},
		{RunID: "alpha", Sequence: 3, Type: EventRunCompleted},
		{RunID: "beta", Sequence: 1, Type: EventRunStarted},
		{RunID: "beta", Sequence: 2, Type: EventUserMessageAdded, Message: &Message{Role: RoleUser, Text: strings.Repeat("shared beta ", 100)}},
		{RunID: "beta", Sequence: 3, Type: EventRunCompleted},
		{RunID: "task", Sequence: 1, Type: EventRunStarted},
		{RunID: "task", Sequence: 2, Type: EventUserMessageAdded, Message: &Message{Role: RoleUser, Text: "continue"}},
	}
	scorer := &recordingContextSemanticScorer{}
	tool := contextRetrievalTool{operation: ContextRetrievalSearch, semanticScorer: scorer}
	result, err := tool.relevanceSearch(context.Background(), contextSearchInput{
		Query: "shared", Order: ContextSearchOrderRelevance, Limit: 8,
	}, events, contextRetrievalAllowance{maxItems: 8, maxOutputBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	var response contextSearchResponse
	if err := json.Unmarshal([]byte(result.Output), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) < 2 || response.Results[0].Source.RunID != "alpha" ||
		response.Results[0].Relevance == nil || !response.Results[0].Relevance.SemanticApplied ||
		response.Results[0].Relevance.Factors.Semantic != ContextRelevanceSignalMaximum {
		t.Fatalf("semantic ranking = %#v", response)
	}
	request := scorer.Request()
	if len(request.Candidates) == 0 || len(request.Candidates) > MaxContextSemanticCandidates {
		t.Fatalf("semantic candidates = %d", len(request.Candidates))
	}
	for _, candidate := range request.Candidates {
		if len(candidate.Text) > MaxContextSemanticCandidateBytes || !utf8.ValidString(candidate.Text) {
			t.Fatalf("unbounded semantic candidate = %#v", candidate)
		}
	}

	tool.semanticScorer = fixedContextSemanticScorer{err: errors.New("sensitive upstream detail")}
	call := ToolCall{
		ID: "rank", Name: ContextSearchToolName,
		Arguments: json.RawMessage(`{"query":"shared","order":"relevance"}`),
	}
	failed := tool.search(
		context.Background(),
		call,
		events,
		contextRetrievalAllowance{maxItems: 8, maxOutputBytes: 64 << 10},
	)
	if !failed.IsError || failed.ContextRetrieval == nil ||
		failed.ContextRetrieval.Outcome != ContextRetrievalFailed ||
		!strings.Contains(failed.Output, "retry with recency") ||
		strings.Contains(failed.Output, "sensitive upstream detail") {
		t.Fatalf("semantic failure = %#v", failed)
	}

	exactCall := ToolCall{
		ID: "exact", Name: ContextSearchToolName,
		Arguments: json.RawMessage(`{"query":"shared"}`),
	}
	exact := tool.search(
		context.Background(),
		exactCall,
		events,
		contextRetrievalAllowance{maxItems: 8, maxOutputBytes: 64 << 10},
	)
	if exact.IsError {
		t.Fatalf("exact search invoked failing semantic scorer: %#v", exact)
	}
}

func TestContextSemanticScorerRejectsInvalidOutput(t *testing.T) {
	t.Parallel()

	events := []Event{
		{RunID: "run", Sequence: 1, Type: EventRunStarted},
		{RunID: "run", Sequence: 2, Type: EventUserMessageAdded, Message: &Message{Role: RoleUser, Text: "shared"}},
	}
	tests := []struct {
		name   string
		scores []int
		want   string
	}{
		{name: "count", scores: nil, want: "score count"},
		{name: "range", scores: []int{ContextRelevanceSignalMaximum + 1}, want: "normalized range"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tool := contextRetrievalTool{
				operation:      ContextRetrievalSearch,
				semanticScorer: fixedContextSemanticScorer{scores: test.scores},
			}
			_, err := tool.relevanceSearch(context.Background(), contextSearchInput{
				Query: "shared", Order: ContextSearchOrderRelevance,
			}, events, contextRetrievalAllowance{maxItems: 8, maxOutputBytes: 64 << 10})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("relevanceSearch() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestContextRelevanceUsesInjectedTokenEstimator(t *testing.T) {
	t.Parallel()

	events := []Event{
		{RunID: "run", Sequence: 1, Type: EventRunStarted},
		{RunID: "run", Sequence: 2, Type: EventUserMessageAdded, Message: &Message{Role: RoleUser, Text: "shared value"}},
	}
	estimator := &fixedRelevanceTokenEstimator{kind: "test_tokenizer", tokens: 64}
	tool := contextRetrievalTool{
		operation: ContextRetrievalSearch, tokenEstimator: estimator,
		provider: "provider", model: "model",
	}
	result, err := tool.relevanceSearch(context.Background(), contextSearchInput{
		Query: "shared", Order: ContextSearchOrderRelevance,
	}, events, contextRetrievalAllowance{maxItems: 8, maxOutputBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	var response contextSearchResponse
	if err := json.Unmarshal([]byte(result.Output), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].Relevance == nil {
		t.Fatalf("relevance response = %#v", response)
	}
	score := response.Results[0].Relevance
	if score.EstimatedInputTokens != 64 || score.TokenEstimateKind != "test_tokenizer" ||
		score.Factors.TokenCostPenalty != ContextRelevanceSignalMaximum/2 {
		t.Fatalf("relevance Token Estimate = %#v", score)
	}
	if err := ValidateContextRelevanceScore(*score, len(response.Results[0].Snippet)); err != nil {
		t.Fatal(err)
	}

	estimator.err = errors.New("sensitive tokenizer detail")
	failed := tool.search(context.Background(), ToolCall{
		ID: "rank", Name: ContextSearchToolName,
		Arguments: json.RawMessage(`{"query":"shared","order":"relevance"}`),
	}, events, contextRetrievalAllowance{maxItems: 8, maxOutputBytes: 64 << 10})
	if !failed.IsError || strings.Contains(failed.Output, "sensitive tokenizer detail") ||
		!strings.Contains(failed.Output, ErrTokenEstimatorFailed.Error()) {
		t.Fatalf("Token Estimator failure = %#v", failed)
	}
}

func TestContextRetrievalRejectsTypedNilSemanticScorer(t *testing.T) {
	t.Parallel()

	var scorer *recordingContextSemanticScorer
	_, err := normalizeContextRetrievalOptions(&ContextRetrievalOptions{SemanticScorer: scorer})
	if err == nil || !strings.Contains(err.Error(), "typed nil") {
		t.Fatalf("normalizeContextRetrievalOptions() error = %v", err)
	}
}

func TestValidateContextRelevanceScoreRejectsTampering(t *testing.T) {
	t.Parallel()

	score := ContextRelevanceScore{
		Version:              ContextRelevanceScoreVersion,
		Factors:              ContextRelevanceFactors{Lexical: ContextRelevanceSignalMaximum},
		EstimatedInputBytes:  3,
		EstimatedInputTokens: 1,
		TokenEstimateKind:    CanonicalByteTokenEstimateKind,
	}
	score.Total = contextRelevanceTotal(score.Factors)
	if err := ValidateContextRelevanceScore(score, 3); err != nil {
		t.Fatal(err)
	}
	tampered := score
	tampered.Total++
	if err := ValidateContextRelevanceScore(tampered, 3); err == nil {
		t.Fatal("ValidateContextRelevanceScore accepted a changed total")
	}
	tampered = score
	tampered.Factors.Semantic = 1
	if err := ValidateContextRelevanceScore(tampered, 3); err == nil {
		t.Fatal("ValidateContextRelevanceScore accepted semantic data without a scorer")
	}
}

func TestContextRelevanceTermsMatchJapaneseWithoutSpaces(t *testing.T) {
	t.Parallel()

	query := contextRelevanceTerms("設定ファイルの原因を確認")
	candidate := contextRelevanceTerms("設定ファイルを調査して原因を確定する")
	if overlap := contextTermOverlap(candidate, query); overlap == 0 {
		t.Fatal("Japanese relevance terms did not overlap")
	}
}

func TestContextRelevanceBoundsCandidatePool(t *testing.T) {
	t.Parallel()

	events := make([]Event, 0, (MaxContextRelevanceCandidates+1)*3)
	for index := 0; index <= MaxContextRelevanceCandidates; index++ {
		runID := fmt.Sprintf("run-%04d", index)
		events = append(events,
			Event{RunID: runID, Sequence: 1, Type: EventRunStarted},
			Event{
				RunID: runID, Sequence: 2, Type: EventUserMessageAdded,
				Message: &Message{Role: RoleUser, Text: fmt.Sprintf("shared candidate %04d", index)},
			},
			Event{RunID: runID, Sequence: 3, Type: EventRunCompleted},
		)
	}
	scorer := &recordingContextSemanticScorer{}
	tool := contextRetrievalTool{operation: ContextRetrievalSearch, semanticScorer: scorer}
	result, err := tool.relevanceSearch(context.Background(), contextSearchInput{
		Query: "shared", Order: ContextSearchOrderRelevance, Limit: 1,
	}, events, contextRetrievalAllowance{maxItems: 1, maxOutputBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	var response contextSearchResponse
	if err := json.Unmarshal([]byte(result.Output), &response); err != nil {
		t.Fatal(err)
	}
	if !response.CandidatePoolTruncated || !response.ConstraintPoolTruncated ||
		len(scorer.Request().Candidates) != MaxContextSemanticCandidates {
		t.Fatalf("bounded relevance response = %#v, semantic candidates = %d", response, len(scorer.Request().Candidates))
	}
}

type recordingContextSemanticScorer struct {
	request ContextSemanticScoreRequest
}

func (scorer *recordingContextSemanticScorer) Score(
	_ context.Context,
	request ContextSemanticScoreRequest,
) ([]int, error) {
	scorer.request = request
	scores := make([]int, len(request.Candidates))
	for index, candidate := range request.Candidates {
		if strings.Contains(candidate.Text, "alpha") {
			scores[index] = ContextRelevanceSignalMaximum
		}
	}
	return scores, nil
}

func (scorer *recordingContextSemanticScorer) Request() ContextSemanticScoreRequest {
	return ContextSemanticScoreRequest{
		Query:            scorer.request.Query,
		Task:             scorer.request.Task,
		TaskContentBytes: scorer.request.TaskContentBytes,
		TaskTruncated:    scorer.request.TaskTruncated,
		Candidates:       append([]ContextSemanticCandidate(nil), scorer.request.Candidates...),
	}
}

type fixedContextSemanticScorer struct {
	scores []int
	err    error
}

type fixedRelevanceTokenEstimator struct {
	kind   string
	tokens int64
	err    error
}

func (estimator *fixedRelevanceTokenEstimator) EstimateTokens(
	_ context.Context,
	request TokenEstimateRequest,
) (TokenEstimateResult, error) {
	if estimator.err != nil {
		return TokenEstimateResult{}, estimator.err
	}
	tokens := make([]int64, len(request.Items))
	for index := range tokens {
		tokens[index] = estimator.tokens
	}
	return TokenEstimateResult{Kind: estimator.kind, Tokens: tokens}, nil
}

func (scorer fixedContextSemanticScorer) Score(
	context.Context,
	ContextSemanticScoreRequest,
) ([]int, error) {
	return append([]int(nil), scorer.scores...), scorer.err
}

var _ ContextSemanticScorer = (*recordingContextSemanticScorer)(nil)
var _ ContextSemanticScorer = fixedContextSemanticScorer{}
