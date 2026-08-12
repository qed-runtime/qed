package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// ContextRelevanceScoreVersion identifies the deterministic score schema
	ContextRelevanceScoreVersion uint32 = 1
	// ContextRelevanceSignalMaximum is the inclusive upper bound for every signal
	ContextRelevanceSignalMaximum = 1000
	// MaxContextRelevanceCandidates bounds the newest searchable Event candidate pool
	MaxContextRelevanceCandidates = 512
	// MaxContextRelevanceAnalysisBytes bounds text analyzed for one Event
	MaxContextRelevanceAnalysisBytes = 16 << 10
	// MaxContextRelevanceConstraints bounds active Constraint text used as ranking signals
	MaxContextRelevanceConstraints = 128
	// MaxContextRelevanceReferenceEvents bounds prior search results used as ranking signals
	MaxContextRelevanceReferenceEvents = 64
	// MaxContextRelevanceReferenceOutputBytes bounds one prior search result decoded for references
	MaxContextRelevanceReferenceOutputBytes = 256 << 10
	// ContextRelevanceTokenCostReference is the full-penalty retrieval estimate
	ContextRelevanceTokenCostReference = maximumContextSearchSnippetBytes / 4
	// MaxContextSemanticCandidates bounds one injected semantic scoring request
	MaxContextSemanticCandidates = MaxContextRelevanceCandidates
	// MaxContextSemanticCandidateBytes bounds candidate text sent to a scorer
	MaxContextSemanticCandidateBytes = maximumContextSearchSnippetBytes
	// MaxContextSemanticTaskBytes bounds latest-task text sent to a scorer
	MaxContextSemanticTaskBytes = 4096

	contextRelevanceLexicalWeight          = 240
	contextRelevanceTaskWeight             = 130
	contextRelevanceFileWeight             = 110
	contextRelevanceSymbolWeight           = 90
	contextRelevanceActiveConstraintWeight = 100
	contextRelevanceUnresolvedErrorWeight  = 100
	contextRelevanceRecencyWeight          = 100
	contextRelevanceReferenceWeight        = 30
	contextRelevanceSemanticWeight         = 100
	contextRelevanceTokenCostWeight        = 100
)

// ContextSearchOrder identifies the ordering contract for context_search
type ContextSearchOrder string

// Context search orders
const (
	// ContextSearchOrderRecency preserves exact case-insensitive matching newest first
	ContextSearchOrderRecency ContextSearchOrder = "recency"
	// ContextSearchOrderRelevance ranks a frozen bounded Event prefix
	ContextSearchOrderRelevance ContextSearchOrder = "relevance"
)

// ContextRelevanceFactors contains normalized deterministic and optional signals
//
// Every field is between zero and ContextRelevanceSignalMaximum. TokenCostPenalty
// is subtracted while every other field contributes positively to Total.
type ContextRelevanceFactors struct {
	// Lexical measures normalized query-term overlap and exact phrase matching
	Lexical int `json:"lexical"`
	// Task measures overlap with the latest user task in the frozen prefix
	Task int `json:"task"`
	// File measures referenced workspace path overlap
	File int `json:"file"`
	// Symbol measures identifier-like query and task term overlap
	Symbol int `json:"symbol"`
	// ActiveConstraint measures overlap with active Constraint Facts and their sources
	ActiveConstraint int `json:"active_constraint"`
	// UnresolvedError identifies failed execution and current failed-check sources
	UnresolvedError int `json:"unresolved_error"`
	// Recency measures source position within the frozen Event prefix
	Recency int `json:"recency"`
	// ReferenceFrequency measures earlier successful search references to the source
	ReferenceFrequency int `json:"reference_frequency"`
	// Semantic is the optional host-supplied normalized signal
	Semantic int `json:"semantic"`
	// TokenCostPenalty normalizes the configured or canonical token estimate
	TokenCostPenalty int `json:"token_cost_penalty"`
}

// ContextRelevanceScore explains one bounded context_search ranking decision
type ContextRelevanceScore struct {
	// Version identifies the score and weight contract
	Version uint32 `json:"version"`
	// Total is the deterministic weighted score between zero and 1000
	Total int `json:"total"`
	// Factors contains normalized signals used to calculate Total
	Factors ContextRelevanceFactors `json:"factors"`
	// SemanticApplied reports whether the configured scorer supplied Semantic
	SemanticApplied bool `json:"semantic_applied"`
	// EstimatedInputBytes is the exact UTF-8 size of the returned snippet
	EstimatedInputBytes int `json:"estimated_input_bytes"`
	// EstimatedInputTokens is the configured or canonical fallback estimate
	EstimatedInputTokens int64 `json:"estimated_input_tokens"`
	// TokenEstimateKind identifies the tokenizer or canonical fallback
	TokenEstimateKind string `json:"token_estimate_kind"`
}

// ContextSemanticCandidate is bounded untrusted Session text supplied to a host scorer
type ContextSemanticCandidate struct {
	// Source identifies the immutable source Event
	Source ContextLedgerEventRef `json:"source"`
	// EventType identifies the source Event kind
	EventType EventType `json:"event_type"`
	// Role identifies user, assistant, or Tool text when applicable
	Role Role `json:"role,omitempty"`
	// ToolName identifies Tool text when applicable
	ToolName string `json:"tool_name,omitempty"`
	// Text is a valid UTF-8 excerpt bounded by MaxContextSemanticCandidateBytes
	Text string `json:"text"`
	// ContentBytes is the exact byte size before excerpting
	ContentBytes int `json:"content_bytes"`
	// Truncated reports that Text is only an excerpt
	Truncated bool `json:"truncated"`
}

// ContextSemanticScoreRequest contains isolated bounded data for optional scoring
type ContextSemanticScoreRequest struct {
	// Query is the exact context_search query
	Query string `json:"query"`
	// Task is a bounded copy of the latest user input in the frozen prefix
	Task string `json:"task,omitempty"`
	// TaskContentBytes is the exact byte size before bounding Task
	TaskContentBytes int `json:"task_content_bytes,omitempty"`
	// TaskTruncated reports that Task is only a prefix
	TaskTruncated bool `json:"task_truncated,omitempty"`
	// Candidates are ordered newest first and bounded by MaxContextSemanticCandidates
	Candidates []ContextSemanticCandidate `json:"candidates"`
}

// ContextSemanticScorer optionally augments deterministic Context relevance
//
// Score must return exactly one normalized score for each request Candidate in
// the same order. Implementations must be safe for concurrent use, honor
// cancellation, treat all text as untrusted data, and enforce any external data
// disclosure policy. Runtime calls this interface only for relevance order.
type ContextSemanticScorer interface {
	Score(ctx context.Context, request ContextSemanticScoreRequest) ([]int, error)
}

type contextRelevanceCandidate struct {
	index             int
	event             Event
	text              string
	analysis          string
	role              Role
	toolName          string
	snippet           string
	semantic          int
	semanticUsed      bool
	estimatedTokens   int64
	tokenEstimateKind string
	score             ContextRelevanceScore
	qualifies         bool
}

type contextRelevanceSignals struct {
	queryText                 string
	queryTerms                map[string]struct{}
	taskTerms                 map[string]struct{}
	symbolTerms               map[string]struct{}
	fileTerms                 map[string]struct{}
	activeTerms               map[string]struct{}
	activeSources             map[string]struct{}
	unresolvedSources         map[string]struct{}
	referenceFrequency        map[string]int
	constraintPoolTruncated   bool
	referenceHistoryTruncated bool
	taskText                  string
	taskContentBytes          int
	taskTruncated             bool
}

func normalizeContextSearchOrder(order ContextSearchOrder) (ContextSearchOrder, error) {
	switch order {
	case "", ContextSearchOrderRecency:
		return ContextSearchOrderRecency, nil
	case ContextSearchOrderRelevance:
		return ContextSearchOrderRelevance, nil
	default:
		return "", fmt.Errorf("order must be %q or %q", ContextSearchOrderRecency, ContextSearchOrderRelevance)
	}
}

// ValidateContextRelevanceScore verifies one public score and its snippet size
func ValidateContextRelevanceScore(score ContextRelevanceScore, snippetBytes int) error {
	if score.Version != ContextRelevanceScoreVersion {
		return fmt.Errorf("version = %d, want %d", score.Version, ContextRelevanceScoreVersion)
	}
	if snippetBytes < 0 || score.EstimatedInputBytes != snippetBytes {
		return errors.New("estimated input bytes do not match the returned snippet")
	}
	if score.EstimatedInputTokens < 0 || !validTokenEstimateKind(score.TokenEstimateKind) {
		return errors.New("Context relevance token estimate is invalid")
	}
	if score.TokenEstimateKind == CanonicalByteTokenEstimateKind &&
		score.EstimatedInputTokens != estimateBytes(int64(snippetBytes)) {
		return errors.New("canonical Context relevance token estimate does not match the returned snippet")
	}
	factors := []int{
		score.Factors.Lexical,
		score.Factors.Task,
		score.Factors.File,
		score.Factors.Symbol,
		score.Factors.ActiveConstraint,
		score.Factors.UnresolvedError,
		score.Factors.Recency,
		score.Factors.ReferenceFrequency,
		score.Factors.Semantic,
		score.Factors.TokenCostPenalty,
	}
	for _, factor := range factors {
		if factor < 0 || factor > ContextRelevanceSignalMaximum {
			return errors.New("factor is outside the normalized range")
		}
	}
	if !score.SemanticApplied && score.Factors.Semantic != 0 {
		return errors.New("semantic signal is present without a configured scorer")
	}
	if score.Total != contextRelevanceTotal(score.Factors) {
		return errors.New("total does not match relevance factors")
	}
	return nil
}

func (tool contextRetrievalTool) relevanceSearch(
	ctx context.Context,
	input contextSearchInput,
	events []Event,
	allowance contextRetrievalAllowance,
) (ToolResult, error) {
	queryDigest := contextSearchQueryDigest(input.Query)
	if input.Cursor > 0 && input.SnapshotEventCount == 0 {
		return ToolResult{}, errors.New("snapshot_event_count is required when relevance cursor is nonzero")
	}
	if (input.Cursor > 0 || input.SnapshotEventCount > 0) && input.SnapshotQueryDigest == "" {
		return ToolResult{}, errors.New("snapshot_query_digest is required for an explicit relevance snapshot")
	}
	if input.SnapshotQueryDigest != "" && input.SnapshotQueryDigest != queryDigest {
		return ToolResult{}, errors.New("snapshot_query_digest does not match query")
	}
	if len(events) == 0 {
		return ToolResult{}, errors.New("relevance search requires a nonempty Event prefix")
	}
	snapshotEventCount := input.SnapshotEventCount
	if snapshotEventCount == 0 {
		snapshotEventCount = len(events)
	}
	if snapshotEventCount <= 0 || snapshotEventCount > len(events) {
		return ToolResult{}, fmt.Errorf("snapshot_event_count must be between 1 and %d", len(events))
	}
	prefix := events[:snapshotEventCount]
	signals, err := buildContextRelevanceSignals(ctx, input.Query, prefix)
	if err != nil {
		if ctx.Err() != nil {
			return ToolResult{}, ctx.Err()
		}
		return ToolResult{}, errors.New("build Context relevance state failed")
	}
	candidates, poolTruncated, err := collectContextRelevanceCandidates(ctx, prefix, signals)
	if err != nil {
		return ToolResult{}, err
	}
	if err := tool.estimateRelevanceCandidates(ctx, candidates); err != nil {
		return ToolResult{}, err
	}
	if tool.semanticScorer != nil && len(candidates) > 0 {
		request := contextSemanticRequest(input.Query, signals, candidates)
		scores, scoreErr := tool.semanticScorer.Score(ctx, request)
		if scoreErr != nil {
			if ctx.Err() != nil {
				return ToolResult{}, ctx.Err()
			}
			return ToolResult{}, errors.New("semantic relevance scoring failed; retry with recency order")
		}
		if len(scores) != len(candidates) {
			return ToolResult{}, errors.New("semantic relevance scorer returned an invalid score count")
		}
		for index, score := range scores {
			if score < 0 || score > ContextRelevanceSignalMaximum {
				return ToolResult{}, errors.New("semantic relevance scorer returned a score outside the normalized range")
			}
			candidates[index].semantic = score
			candidates[index].semanticUsed = true
		}
	}

	ranked := candidates[:0]
	for index := range candidates {
		scoreContextRelevanceCandidate(&candidates[index], signals, snapshotEventCount)
		if candidates[index].qualifies {
			ranked = append(ranked, candidates[index])
		}
	}
	sort.SliceStable(ranked, func(left, right int) bool {
		if ranked[left].score.Total != ranked[right].score.Total {
			return ranked[left].score.Total > ranked[right].score.Total
		}
		if ranked[left].index != ranked[right].index {
			return ranked[left].index > ranked[right].index
		}
		if ranked[left].event.RunID != ranked[right].event.RunID {
			return ranked[left].event.RunID < ranked[right].event.RunID
		}
		return ranked[left].event.Sequence < ranked[right].event.Sequence
	})
	if input.Cursor < 0 || input.Cursor > len(ranked) {
		return ToolResult{}, fmt.Errorf("cursor must be between 0 and %d for the frozen relevance result", len(ranked))
	}
	limit := requestedLimit(input.Limit, allowance.maxItems)
	response := contextSearchResponse{
		Version: ContextRetrievalMetadataVersion, Order: ContextSearchOrderRelevance,
		SnapshotEventCount: snapshotEventCount, SnapshotQueryDigest: queryDigest,
		CandidatePoolTruncated:    poolTruncated,
		ConstraintPoolTruncated:   signals.constraintPoolTruncated,
		ReferenceHistoryTruncated: signals.referenceHistoryTruncated,
		Results:                   make([]contextSearchResult, 0, limit), NextCursor: input.Cursor,
	}
	for index := input.Cursor; index < len(ranked) && len(response.Results) < limit; index++ {
		candidate := ranked[index]
		item := contextSearchResult{
			Source: contextSourceRef{
				RunID: candidate.event.RunID, Sequence: candidate.event.Sequence,
				SessionRevision: candidate.event.SessionRevision, EventType: candidate.event.Type,
			},
			Role: candidate.role, ToolName: candidate.toolName, Snippet: candidate.snippet,
			Relevance: &candidate.score, Untrusted: true,
		}
		response.Results = append(response.Results, item)
		response.NextCursor = index + 1
		if !responseFits(response, allowance.maxOutputBytes) {
			response.Results = response.Results[:len(response.Results)-1]
			response.NextCursor = index
			if len(response.Results) == 0 {
				return tool.errorResult(ContextRetrievalLimit, errContextRetrievalByteLimit.Error(), allowance.postCompaction, ""), nil
			}
			response.Truncated = true
			return tool.successResult(response, len(response.Results), true, allowance.postCompaction, "", allowance.maxOutputBytes), nil
		}
	}
	response.Truncated = response.NextCursor < len(ranked)
	return tool.successResult(
		response,
		len(response.Results),
		response.Truncated,
		allowance.postCompaction,
		"",
		allowance.maxOutputBytes,
	), nil
}

func (tool contextRetrievalTool) estimateRelevanceCandidates(
	ctx context.Context,
	candidates []contextRelevanceCandidate,
) error {
	if len(candidates) == 0 {
		return nil
	}
	items := make([]TokenEstimateItem, len(candidates))
	for index := range candidates {
		items[index] = TokenEstimateItem{
			ID:      fmt.Sprintf("candidate/%010d", index),
			Content: []byte(candidates[index].snippet),
		}
	}
	result, err := EstimateTokenItems(ctx, tool.tokenEstimator, TokenEstimateRequest{
		Provider: tool.provider,
		Model:    tool.model,
		Purpose:  TokenEstimateRetrievalSnippets,
		Items:    items,
	})
	if err != nil {
		return err
	}
	for index := range candidates {
		candidates[index].estimatedTokens = result.Tokens[index]
		candidates[index].tokenEstimateKind = result.Kind
	}
	return nil
}

func buildContextRelevanceSignals(
	ctx context.Context,
	query string,
	events []Event,
) (contextRelevanceSignals, error) {
	ledger, err := BuildContextLedger(ctx, events)
	if err != nil {
		return contextRelevanceSignals{}, err
	}
	referenceFrequency, referenceHistoryTruncated := contextReferenceFrequency(events)
	signals := contextRelevanceSignals{
		queryText:                 query,
		queryTerms:                contextRelevanceTerms(query),
		taskTerms:                 make(map[string]struct{}),
		symbolTerms:               contextSymbolTerms(query),
		fileTerms:                 contextPathTerms(query),
		activeTerms:               make(map[string]struct{}),
		activeSources:             make(map[string]struct{}),
		unresolvedSources:         make(map[string]struct{}),
		referenceFrequency:        referenceFrequency,
		referenceHistoryTruncated: referenceHistoryTruncated,
	}
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type == EventUserMessageAdded && event.Message != nil && event.Message.Role == RoleUser {
			task := strings.ToValidUTF8(event.Message.Text, "\uFFFD")
			signals.taskContentBytes = len(task)
			signals.taskText = boundedContextRelevanceText(task, MaxContextSemanticTaskBytes)
			signals.taskTruncated = len(signals.taskText) < signals.taskContentBytes
			break
		}
	}
	signals.taskTerms = contextRelevanceTerms(signals.taskText)
	mergeStringSet(signals.symbolTerms, contextSymbolTerms(signals.taskText))
	mergeStringSet(signals.fileTerms, contextPathTerms(signals.taskText))
	activeConstraintCount := 0
	for index := len(ledger.Constraints) - 1; index >= 0; index-- {
		constraint := ledger.Constraints[index]
		if constraint.State != FactActive {
			continue
		}
		for _, source := range constraint.Sources {
			signals.activeSources[contextEventRefKey(source)] = struct{}{}
		}
		if activeConstraintCount >= MaxContextRelevanceConstraints {
			signals.constraintPoolTruncated = true
			continue
		}
		activeConstraintCount++
		mergeStringSet(signals.activeTerms, contextRelevanceTerms(
			boundedContextRelevanceText(constraint.Text, MaxContextSemanticTaskBytes),
		))
	}

	for _, execution := range ledger.Executions {
		if execution.State != ExecutionLedgerFailed {
			continue
		}
		for _, source := range execution.Sources {
			signals.unresolvedSources[contextEventRefKey(source)] = struct{}{}
		}
	}

	if worldState := latestContextWorldState(events); worldState != nil {
		for _, file := range worldState.Snapshot.Files {
			addRelevantWorldPath(signals.fileTerms, query, signals.taskText, file.Path)
		}
		if worldState.Snapshot.Git != nil {
			for _, change := range worldState.Snapshot.Git.Changes {
				addRelevantWorldPath(signals.fileTerms, query, signals.taskText, change.Path)
				addRelevantWorldPath(signals.fileTerms, query, signals.taskText, change.OriginalPath)
			}
		}
		for _, check := range worldState.Snapshot.Checks {
			if check.Status == CurrentWorldCheckFailed && check.Freshness != CurrentWorldCheckStale {
				signals.unresolvedSources[contextEventRefKey(check.Source)] = struct{}{}
			}
		}
	}
	return signals, nil
}

func collectContextRelevanceCandidates(
	ctx context.Context,
	events []Event,
	signals contextRelevanceSignals,
) ([]contextRelevanceCandidate, bool, error) {
	candidates := make([]contextRelevanceCandidate, 0, min(len(events), MaxContextRelevanceCandidates))
	poolTruncated := false
	for index := len(events) - 1; index >= 0; index-- {
		if index%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
		}
		text, role, toolName, ok := relevanceEventText(events[index])
		if !ok {
			continue
		}
		if len(candidates) >= MaxContextRelevanceCandidates {
			poolTruncated = true
			break
		}
		text = strings.ToValidUTF8(text, "\uFFFD")
		analysis := boundedContextRelevanceText(text, MaxContextRelevanceAnalysisBytes)
		match := contextRelevanceMatch(analysis, signals)
		candidates = append(candidates, contextRelevanceCandidate{
			index: index, event: events[index], text: text, analysis: analysis,
			role: role, toolName: toolName, snippet: contextSearchSnippet(analysis, match),
		})
	}
	return candidates, poolTruncated, nil
}

func scoreContextRelevanceCandidate(
	candidate *contextRelevanceCandidate,
	signals contextRelevanceSignals,
	snapshotEventCount int,
) {
	if candidate.tokenEstimateKind == "" {
		candidate.estimatedTokens = estimateBytes(int64(len(candidate.snippet)))
		candidate.tokenEstimateKind = CanonicalByteTokenEstimateKind
	}
	terms := contextRelevanceTerms(candidate.analysis)
	lexical := contextTermOverlap(terms, signals.queryTerms)
	if query := strings.TrimSpace(signals.queryText); query != "" && caseInsensitiveIndex(candidate.analysis, query) >= 0 {
		lexical = ContextRelevanceSignalMaximum
	}
	if lexical < ContextRelevanceSignalMaximum {
		lexical = lexical * 800 / ContextRelevanceSignalMaximum
	}
	task := contextTermOverlap(terms, signals.taskTerms)
	file := contextTextSetMatch(candidate.analysis, signals.fileTerms)
	symbol := contextTermOverlap(terms, signals.symbolTerms)
	active := contextTermOverlap(terms, signals.activeTerms) / 2
	sourceKey := contextEventKey(candidate.event)
	if _, ok := signals.activeSources[sourceKey]; ok {
		active = ContextRelevanceSignalMaximum
	}
	unresolved := 0
	if _, ok := signals.unresolvedSources[sourceKey]; ok {
		unresolved = ContextRelevanceSignalMaximum
	} else if candidate.event.ToolResult != nil && candidate.event.ToolResult.IsError {
		unresolved = 800
	} else if candidate.event.Type == EventRunFailed {
		unresolved = 800
	}
	recency := 0
	if snapshotEventCount > 0 {
		recency = int(int64(candidate.index+1) * ContextRelevanceSignalMaximum / int64(snapshotEventCount))
	}
	references := min(signals.referenceFrequency[sourceKey], 5) * 200
	penalty := ContextRelevanceSignalMaximum
	if candidate.estimatedTokens < ContextRelevanceTokenCostReference {
		penalty = int(candidate.estimatedTokens * ContextRelevanceSignalMaximum / ContextRelevanceTokenCostReference)
	}
	candidate.score = ContextRelevanceScore{
		Version: ContextRelevanceScoreVersion,
		Factors: ContextRelevanceFactors{
			Lexical: lexical, Task: task, File: file, Symbol: symbol,
			ActiveConstraint: active, UnresolvedError: unresolved, Recency: recency,
			ReferenceFrequency: references, Semantic: candidate.semantic,
			TokenCostPenalty: penalty,
		},
		SemanticApplied: candidate.semanticUsed, EstimatedInputBytes: len(candidate.snippet),
		EstimatedInputTokens: candidate.estimatedTokens, TokenEstimateKind: candidate.tokenEstimateKind,
	}
	candidate.score.Total = contextRelevanceTotal(candidate.score.Factors)
	candidate.qualifies = lexical > 0 || task > 0 || file > 0 || symbol > 0 || active > 0 ||
		unresolved > 0 || references > 0 || candidate.semantic > 0
}

func contextRelevanceTotal(factors ContextRelevanceFactors) int {
	total := weightedContextSignal(factors.Lexical, contextRelevanceLexicalWeight) +
		weightedContextSignal(factors.Task, contextRelevanceTaskWeight) +
		weightedContextSignal(factors.File, contextRelevanceFileWeight) +
		weightedContextSignal(factors.Symbol, contextRelevanceSymbolWeight) +
		weightedContextSignal(factors.ActiveConstraint, contextRelevanceActiveConstraintWeight) +
		weightedContextSignal(factors.UnresolvedError, contextRelevanceUnresolvedErrorWeight) +
		weightedContextSignal(factors.Recency, contextRelevanceRecencyWeight) +
		weightedContextSignal(factors.ReferenceFrequency, contextRelevanceReferenceWeight) +
		weightedContextSignal(factors.Semantic, contextRelevanceSemanticWeight) -
		weightedContextSignal(factors.TokenCostPenalty, contextRelevanceTokenCostWeight)
	return max(0, min(ContextRelevanceSignalMaximum, total))
}

func weightedContextSignal(signal, weight int) int {
	return int(int64(signal) * int64(weight) / ContextRelevanceSignalMaximum)
}

func contextSemanticRequest(
	query string,
	signals contextRelevanceSignals,
	candidates []contextRelevanceCandidate,
) ContextSemanticScoreRequest {
	request := ContextSemanticScoreRequest{
		Query: query, Task: signals.taskText, TaskContentBytes: signals.taskContentBytes,
		TaskTruncated: signals.taskTruncated,
		Candidates:    make([]ContextSemanticCandidate, len(candidates)),
	}
	for index, candidate := range candidates {
		request.Candidates[index] = ContextSemanticCandidate{
			Source: ContextLedgerEventRef{
				RunID: candidate.event.RunID, Sequence: candidate.event.Sequence,
				SessionRevision: candidate.event.SessionRevision,
			},
			EventType: candidate.event.Type, Role: candidate.role, ToolName: candidate.toolName,
			Text: candidate.snippet, ContentBytes: len(candidate.text),
			Truncated: len(candidate.snippet) < len(candidate.text),
		}
	}
	return request
}

func relevanceEventText(event Event) (string, Role, string, bool) {
	if event.Type == EventToolCompleted && event.ToolResult != nil && event.ToolResult.ContextRetrieval != nil {
		return "", "", "", false
	}
	if text, role, toolName, ok := searchableEventText(event); ok {
		return text, role, toolName, true
	}
	if event.Type == EventRunFailed && event.Error != "" {
		return event.Error, RoleTool, "runtime", true
	}
	return "", "", "", false
}

func contextReferenceFrequency(events []Event) (map[string]int, bool) {
	frequency := make(map[string]int)
	matchedEvents := 0
	truncated := false
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != EventToolCompleted || event.ToolResult == nil ||
			event.ToolResult.ContextRetrieval == nil ||
			event.ToolResult.ContextRetrieval.Operation != ContextRetrievalSearch ||
			event.ToolResult.ContextRetrieval.Outcome != ContextRetrievalSucceeded {
			continue
		}
		if matchedEvents >= MaxContextRelevanceReferenceEvents {
			truncated = true
			break
		}
		matchedEvents++
		if len(event.ToolResult.Output) > MaxContextRelevanceReferenceOutputBytes {
			truncated = true
			continue
		}
		var response contextSearchResponse
		if err := jsonUnmarshalContextSearch(event.ToolResult.Output, &response); err != nil {
			continue
		}
		for _, result := range response.Results {
			frequency[contextSourceKey(result.Source)]++
		}
	}
	return frequency, truncated
}

func jsonUnmarshalContextSearch(value string, response *contextSearchResponse) error {
	return json.Unmarshal([]byte(value), response)
}

func latestContextWorldState(events []Event) *CurrentWorldState {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type == EventCurrentWorldStateCaptured && events[index].CurrentWorldState != nil {
			return events[index].CurrentWorldState
		}
	}
	return nil
}

func contextRelevanceTerms(value string) map[string]struct{} {
	terms := make(map[string]struct{})
	var current strings.Builder
	flush := func() {
		term := strings.Map(unicode.ToLower, current.String())
		current.Reset()
		runes := []rune(term)
		if len(runes) < 2 {
			return
		}
		terms[term] = struct{}{}
		if contextDenseScript(runes) {
			for index := 0; index+1 < len(runes); index++ {
				terms[string(runes[index:index+2])] = struct{}{}
			}
		}
	}
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || character == '_' {
			current.WriteRune(character)
			continue
		}
		flush()
	}
	flush()
	return terms
}

func contextDenseScript(value []rune) bool {
	for _, character := range value {
		if unicode.In(character, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			return true
		}
	}
	return false
}

func contextSymbolTerms(value string) map[string]struct{} {
	terms := contextRelevanceTerms(value)
	for term := range terms {
		if _, common := contextRelevanceCommonTerms[term]; common || !contextASCIIIdentifier(term) {
			delete(terms, term)
		}
	}
	return terms
}

func contextASCIIIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character > unicode.MaxASCII ||
			(!unicode.IsLetter(character) && !unicode.IsDigit(character) && character != '_') {
			return false
		}
	}
	return true
}

var contextRelevanceCommonTerms = map[string]struct{}{
	"and": {}, "are": {}, "for": {}, "from": {}, "into": {}, "that": {}, "the": {},
	"this": {}, "with": {}, "を": {}, "が": {}, "に": {}, "の": {}, "は": {},
}

func contextPathTerms(value string) map[string]struct{} {
	terms := make(map[string]struct{})
	for _, token := range strings.FieldsFunc(value, func(character rune) bool {
		return unicode.IsSpace(character) || strings.ContainsRune("\"'`()[]{}<>,:;", character)
	}) {
		token = strings.Trim(token, "!?=*|\\")
		if token == "" || token == "." || token == ".." || (!strings.Contains(token, "/") && !strings.Contains(token, ".")) {
			continue
		}
		terms[strings.Map(unicode.ToLower, token)] = struct{}{}
	}
	return terms
}

func contextTermOverlap(candidate, wanted map[string]struct{}) int {
	if len(wanted) == 0 {
		return 0
	}
	matches := 0
	for term := range wanted {
		if _, ok := candidate[term]; ok {
			matches++
		}
	}
	return int(int64(matches) * ContextRelevanceSignalMaximum / int64(len(wanted)))
}

func contextTextSetMatch(value string, wanted map[string]struct{}) int {
	if len(wanted) == 0 {
		return 0
	}
	lower := strings.Map(unicode.ToLower, value)
	matches := 0
	for term := range wanted {
		if strings.Contains(lower, term) {
			matches++
		}
	}
	return int(int64(matches) * ContextRelevanceSignalMaximum / int64(len(wanted)))
}

func contextRelevanceMatch(value string, signals contextRelevanceSignals) int {
	match := -1
	for _, terms := range []map[string]struct{}{signals.queryTerms, signals.fileTerms} {
		ordered := make([]string, 0, len(terms))
		for term := range terms {
			ordered = append(ordered, term)
		}
		sort.Strings(ordered)
		for _, term := range ordered {
			candidate := caseInsensitiveIndex(value, term)
			if candidate >= 0 && (match < 0 || candidate < match) {
				match = candidate
			}
		}
		if match >= 0 {
			return match
		}
	}
	return 0
}

func boundedContextRelevanceText(value string, maximum int) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) <= maximum {
		return value
	}
	end := maximum
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func latestPathBase(value string) string {
	value = strings.TrimRight(value, "/")
	if index := strings.LastIndex(value, "/"); index >= 0 {
		return value[index+1:]
	}
	return value
}

func addRelevantWorldPath(target map[string]struct{}, query, task, value string) {
	if value == "" {
		return
	}
	lowerValue := strings.Map(unicode.ToLower, value)
	base := latestPathBase(lowerValue)
	haystack := strings.Map(unicode.ToLower, query+"\n"+task)
	if strings.Contains(haystack, lowerValue) || (base != "" && strings.Contains(haystack, base)) {
		target[lowerValue] = struct{}{}
		if base != "" {
			target[base] = struct{}{}
		}
	}
}

func mergeStringSet(target, source map[string]struct{}) {
	for value := range source {
		target[value] = struct{}{}
	}
}

func contextEventKey(event Event) string {
	return contextEventRefKey(ContextLedgerEventRef{
		RunID: event.RunID, Sequence: event.Sequence, SessionRevision: event.SessionRevision,
	})
}

func contextSourceKey(source contextSourceRef) string {
	return contextEventRefKey(ContextLedgerEventRef{
		RunID: source.RunID, Sequence: source.Sequence, SessionRevision: source.SessionRevision,
	})
}

func contextEventRefKey(source ContextLedgerEventRef) string {
	return fmt.Sprintf("%s\x00%d\x00%d", source.RunID, source.Sequence, source.SessionRevision)
}

func contextSearchQueryDigest(query string) string {
	return contextLedgerDigestParts("qed.context.search.query.v1", []byte(query))
}
