package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	// CanonicalByteTokenEstimateKind identifies QED's deterministic byte fallback
	CanonicalByteTokenEstimateKind = "canonical_bytes_div_4"
	// MaxTokenEstimateItems bounds one estimator batch
	MaxTokenEstimateItems = 65536
	// MaxTokenEstimateKindBytes bounds a persisted estimator identity
	MaxTokenEstimateKindBytes = 128
)

var (
	// ErrTokenEstimatorFailed hides implementation-specific estimator failures
	ErrTokenEstimatorFailed = errors.New("Token Estimator failed")
)

// TokenEstimatePurpose identifies why Core is estimating content
type TokenEstimatePurpose string

// Token estimate purposes
const (
	// TokenEstimateContextSegments estimates canonical logical Provider input Segments
	TokenEstimateContextSegments TokenEstimatePurpose = "context_segments"
	// TokenEstimateRetrievalSnippets estimates bounded untrusted retrieval snippets
	TokenEstimateRetrievalSnippets TokenEstimatePurpose = "retrieval_snippets"
)

// TokenEstimateItem contains one isolated byte sequence in an estimator batch
type TokenEstimateItem struct {
	// ID is non-empty, unique only within this request, and contains no content
	ID string `json:"id"`
	// Content contains canonical or bounded untrusted bytes to estimate
	Content []byte `json:"content"`
}

// TokenEstimateRequest supplies one bounded batch to a Token Estimator
type TokenEstimateRequest struct {
	// Provider identifies the configured Provider when estimation occurs in Runtime
	Provider string `json:"provider,omitempty"`
	// Model identifies the configured model when the Provider exposes it
	Model string `json:"model,omitempty"`
	// Purpose identifies how Core will use the estimates
	Purpose TokenEstimatePurpose `json:"purpose"`
	// Items contains between one and MaxTokenEstimateItems entries and preserves
	// the order expected in TokenEstimateResult
	Items []TokenEstimateItem `json:"items"`
}

// TokenEstimateResult contains one validated estimate for every request item
type TokenEstimateResult struct {
	// Kind is a stable content-free identity for the tokenizer or approximation
	//
	// It must match [a-z0-9][a-z0-9._/:-]{0,127}
	Kind string `json:"kind"`
	// Tokens preserves request item order and contains non-negative estimates
	Tokens []int64 `json:"tokens"`
}

// TokenEstimator estimates isolated content without changing it
//
// Implementations must be safe for concurrent use, honor cancellation, treat
// every Content value as untrusted data, return one count per Item in the same
// order, not retain Content or treat it as instructions, and use a stable
// non-secret Kind.
// The same Provider, Model, Purpose, and Content must produce the same result.
// Runtime does not retry calls
type TokenEstimator interface {
	EstimateTokens(ctx context.Context, request TokenEstimateRequest) (TokenEstimateResult, error)
}

// CanonicalByteTokenEstimator implements QED's deterministic dependency-free fallback
//
// Each non-empty item is estimated as the ceiling of its byte length divided by four
type CanonicalByteTokenEstimator struct{}

// EstimateTokens returns deterministic canonical byte estimates
func (CanonicalByteTokenEstimator) EstimateTokens(
	ctx context.Context,
	request TokenEstimateRequest,
) (TokenEstimateResult, error) {
	if err := ctx.Err(); err != nil {
		return TokenEstimateResult{}, err
	}
	result := TokenEstimateResult{
		Kind:   CanonicalByteTokenEstimateKind,
		Tokens: make([]int64, len(request.Items)),
	}
	for index, item := range request.Items {
		result.Tokens[index] = estimateBytes(int64(len(item.Content)))
	}
	return result, nil
}

// EstimateTokenItems validates, isolates, and executes one estimator batch
//
// A nil estimator selects CanonicalByteTokenEstimator. Implementation errors
// are replaced by ErrTokenEstimatorFailed so credentials and remote details do
// not enter public Run errors
func EstimateTokenItems(
	ctx context.Context,
	estimator TokenEstimator,
	request TokenEstimateRequest,
) (TokenEstimateResult, error) {
	if ctx == nil {
		return TokenEstimateResult{}, errors.New("Token Estimator context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return TokenEstimateResult{}, err
	}
	if estimator == nil {
		estimator = CanonicalByteTokenEstimator{}
	} else if nilInterface(estimator) {
		return TokenEstimateResult{}, errors.New("Token Estimator must not be a typed nil")
	}
	if strings.TrimSpace(request.Provider) != request.Provider {
		return TokenEstimateResult{}, errors.New("Token Estimate Provider must not have surrounding whitespace")
	}
	if strings.TrimSpace(request.Model) != request.Model {
		return TokenEstimateResult{}, errors.New("Token Estimate Model must not have surrounding whitespace")
	}
	switch request.Purpose {
	case TokenEstimateContextSegments, TokenEstimateRetrievalSnippets:
	default:
		return TokenEstimateResult{}, fmt.Errorf("Token Estimate purpose %q is invalid", request.Purpose)
	}
	if len(request.Items) == 0 || len(request.Items) > MaxTokenEstimateItems {
		return TokenEstimateResult{}, fmt.Errorf(
			"Token Estimate item count must be between 1 and %d",
			MaxTokenEstimateItems,
		)
	}
	isolated := request
	isolated.Items = make([]TokenEstimateItem, len(request.Items))
	identifiers := make(map[string]struct{}, len(request.Items))
	for index, item := range request.Items {
		if strings.TrimSpace(item.ID) != item.ID || item.ID == "" {
			return TokenEstimateResult{}, fmt.Errorf("Token Estimate item %d has an invalid ID", index)
		}
		if _, duplicate := identifiers[item.ID]; duplicate {
			return TokenEstimateResult{}, fmt.Errorf("Token Estimate item ID %q is duplicated", item.ID)
		}
		identifiers[item.ID] = struct{}{}
		isolated.Items[index] = TokenEstimateItem{
			ID:      item.ID,
			Content: append([]byte(nil), item.Content...),
		}
	}
	result, err := estimator.EstimateTokens(ctx, isolated)
	if err != nil {
		if ctx.Err() != nil {
			return TokenEstimateResult{}, ctx.Err()
		}
		return TokenEstimateResult{}, ErrTokenEstimatorFailed
	}
	if err := ctx.Err(); err != nil {
		return TokenEstimateResult{}, err
	}
	if !validTokenEstimateKind(result.Kind) {
		return TokenEstimateResult{}, errors.New("Token Estimator returned an invalid Kind")
	}
	if len(result.Tokens) != len(request.Items) {
		return TokenEstimateResult{}, errors.New("Token Estimator returned an invalid token count")
	}
	for index, tokens := range result.Tokens {
		if tokens < 0 {
			return TokenEstimateResult{}, errors.New("Token Estimator returned a negative estimate")
		}
		if result.Kind == CanonicalByteTokenEstimateKind &&
			tokens != estimateBytes(int64(len(request.Items[index].Content))) {
			return TokenEstimateResult{}, errors.New("Token Estimator returned an invalid canonical estimate")
		}
	}
	result.Tokens = append([]int64(nil), result.Tokens...)
	return result, nil
}

func resolveTokenEstimator(provider Provider, configured TokenEstimator) (TokenEstimator, error) {
	if configured != nil {
		if nilInterface(configured) {
			return nil, errors.New("Token Estimator must not be a typed nil")
		}
		return configured, nil
	}
	if providerEstimator, ok := provider.(TokenEstimator); ok {
		if nilInterface(providerEstimator) {
			return nil, errors.New("Provider Token Estimator must not be a typed nil")
		}
		return providerEstimator, nil
	}
	return CanonicalByteTokenEstimator{}, nil
}

func validTokenEstimateKind(kind string) bool {
	if kind == "" || len(kind) > MaxTokenEstimateKindBytes || strings.TrimSpace(kind) != kind {
		return false
	}
	for index, character := range []byte(kind) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			index > 0 && (character == '.' || character == '_' || character == '-' || character == '/' || character == ':') {
			continue
		}
		return false
	}
	return true
}

func estimateContextSegments(
	ctx context.Context,
	estimator TokenEstimator,
	provider string,
	model string,
	request ModelRequest,
	segments []ContextSegment,
) ([]ContextSegment, error) {
	items, err := contextSegmentEstimateItems(request, segments)
	if err != nil {
		return nil, err
	}
	result, err := EstimateTokenItems(ctx, estimator, TokenEstimateRequest{
		Provider: provider,
		Model:    model,
		Purpose:  TokenEstimateContextSegments,
		Items:    items,
	})
	if err != nil {
		return nil, err
	}
	estimated := cloneContextSegments(segments)
	for index := range estimated {
		estimated[index].TokenEstimate = result.Tokens[index]
		estimated[index].TokenEstimateKind = result.Kind
	}
	return estimated, nil
}

func contextSegmentEstimateItems(
	request ModelRequest,
	segments []ContextSegment,
) ([]TokenEstimateItem, error) {
	contents := make([][]byte, 0, len(request.Messages)+3)
	contents = append(contents, []byte(request.Instructions))
	toolContent, err := json.Marshal(contextTools(request.Tools))
	if err != nil {
		return nil, fmt.Errorf("encode Tool ABI Token Estimate content: %w", err)
	}
	contents = append(contents, toolContent)
	for index, message := range request.Messages {
		content, err := contextMessageContent(message)
		if err != nil {
			return nil, fmt.Errorf("encode message Token Estimate content %d: %w", index, err)
		}
		contents = append(contents, content)
	}
	if len(request.Metadata) > 0 {
		content, err := json.Marshal(request.Metadata)
		if err != nil {
			return nil, fmt.Errorf("encode metadata Token Estimate content: %w", err)
		}
		contents = append(contents, content)
	}
	if len(contents) != len(segments) {
		return nil, errors.New("Context Segments do not preserve canonical request positions for Token Estimation")
	}
	items := make([]TokenEstimateItem, len(segments))
	for index, segment := range segments {
		content := contents[index]
		if segment.Bytes != int64(len(content)) ||
			segment.ContentHash != contextSegmentDigest(segment.Kind, segment.Version, content) {
			return nil, fmt.Errorf("Context Segment %d content identity does not match canonical request", index)
		}
		items[index] = TokenEstimateItem{
			ID:      fmt.Sprintf("segment/%010d", index),
			Content: content,
		}
	}
	return items, nil
}
