package agent_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/qed-runtime/qed/agent"
)

func TestCanonicalByteTokenEstimatorAndIsolation(t *testing.T) {
	t.Parallel()

	content := []byte("abcde")
	result, err := agent.EstimateTokenItems(context.Background(), nil, agent.TokenEstimateRequest{
		Purpose: agent.TokenEstimateRetrievalSnippets,
		Items: []agent.TokenEstimateItem{
			{ID: "one", Content: content},
			{ID: "empty"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != agent.CanonicalByteTokenEstimateKind || len(result.Tokens) != 2 ||
		result.Tokens[0] != 2 || result.Tokens[1] != 0 {
		t.Fatalf("canonical Token Estimate = %#v", result)
	}

	estimator := &recordingTokenEstimator{kind: "test_tokenizer", tokensPerItem: 3, mutate: true}
	result, err = agent.EstimateTokenItems(context.Background(), estimator, agent.TokenEstimateRequest{
		Provider: "provider", Model: "model", Purpose: agent.TokenEstimateContextSegments,
		Items: []agent.TokenEstimateItem{{ID: "one", Content: content}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "abcde" || result.Kind != "test_tokenizer" || result.Tokens[0] != 3 {
		t.Fatalf("isolated Token Estimate = %#v, content = %q", result, content)
	}
}

func TestEstimateTokenItemsRejectsInvalidOrSensitiveFailure(t *testing.T) {
	t.Parallel()

	request := agent.TokenEstimateRequest{
		Purpose: agent.TokenEstimateContextSegments,
		Items:   []agent.TokenEstimateItem{{ID: "one", Content: []byte("value")}},
	}
	tests := []struct {
		name      string
		estimator agent.TokenEstimator
		want      string
	}{
		{name: "failure", estimator: &recordingTokenEstimator{err: errors.New("secret remote detail")}, want: agent.ErrTokenEstimatorFailed.Error()},
		{name: "kind", estimator: &recordingTokenEstimator{kind: "Invalid Kind", tokensPerItem: 1}, want: "invalid Kind"},
		{name: "count", estimator: &recordingTokenEstimator{kind: "valid", omitLast: true}, want: "invalid token count"},
		{name: "negative", estimator: &recordingTokenEstimator{kind: "valid", tokensPerItem: -1}, want: "negative estimate"},
		{name: "canonical", estimator: &recordingTokenEstimator{kind: agent.CanonicalByteTokenEstimateKind, tokensPerItem: 1}, want: "invalid canonical estimate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := agent.EstimateTokenItems(context.Background(), test.estimator, request)
			if err == nil || !contains(err.Error(), test.want) || contains(err.Error(), "secret remote detail") {
				t.Fatalf("EstimateTokenItems() error = %v, want %q", err, test.want)
			}
		})
	}

	var typedNil *recordingTokenEstimator
	if _, err := agent.NewRuntime(agent.Options{
		Provider:       &tokenEstimatingProvider{streamMessage: agent.Message{Role: agent.RoleAssistant}},
		TokenEstimator: typedNil,
	}); err == nil || !contains(err.Error(), "typed nil") {
		t.Fatalf("NewRuntime typed nil error = %v", err)
	}

	if _, err := agent.EstimateTokenItems(context.Background(), nil, agent.TokenEstimateRequest{
		Purpose: agent.TokenEstimateContextSegments,
	}); err == nil {
		t.Fatal("EstimateTokenItems accepted an empty batch")
	}
	tooMany := make([]agent.TokenEstimateItem, agent.MaxTokenEstimateItems+1)
	if _, err := agent.EstimateTokenItems(context.Background(), nil, agent.TokenEstimateRequest{
		Purpose: agent.TokenEstimateContextSegments,
		Items:   tooMany,
	}); err == nil {
		t.Fatal("EstimateTokenItems accepted an oversized batch")
	}
}

func TestDefaultCompilerAndCachePlannerUseInjectedTokenEstimator(t *testing.T) {
	t.Parallel()

	estimator := &recordingTokenEstimator{kind: "test_tokenizer", tokensPerItem: 11}
	compiled, err := (agent.DefaultContextCompiler{}).Compile(context.Background(), agent.ContextCompileRequest{
		Provider: "provider", Model: "model", TokenEstimator: estimator,
		ModelRequest: agent.ModelRequest{
			Instructions: "system",
			Messages:     []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Segments) != 3 {
		t.Fatalf("Context Segments = %#v", compiled.Segments)
	}
	for _, segment := range compiled.Segments {
		if segment.TokenEstimate != 11 || segment.TokenEstimateKind != "test_tokenizer" {
			t.Fatalf("Context Segment estimate = %#v", segment)
		}
	}
	plan, err := (agent.DefaultCachePlanner{}).Plan(context.Background(), agent.CachePlanRequest{
		RunID: "run", Provider: "provider", Model: "model",
		ModelRequest: compiled.ModelRequest, Segments: compiled.Segments,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.InputTokenEstimate != 33 || plan.TokenEstimateKind != "test_tokenizer" {
		t.Fatalf("Cache Plan estimate = %#v", plan)
	}
	manifest, err := agent.BuildPrefixManifest(
		agent.PrefixManifestOptions{Provider: "provider", Model: "model"},
		compiled.Segments,
	)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Segments[0].TokenEstimate != 11 || manifest.Segments[0].TokenEstimateKind != "test_tokenizer" {
		t.Fatalf("Prefix Manifest estimate = %#v", manifest.Segments[0])
	}

	mixed := append([]agent.ContextSegment(nil), compiled.Segments...)
	mixed[1].TokenEstimateKind = "other_tokenizer"
	mixed[1].TokenEstimate = 99
	plan, err = (agent.DefaultCachePlanner{}).Plan(context.Background(), agent.CachePlanRequest{
		RunID: "run", Provider: "provider", Model: "model",
		ModelRequest: compiled.ModelRequest, Segments: mixed,
	})
	if err != nil {
		t.Fatal(err)
	}
	var canonicalBytes int64
	for _, segment := range mixed {
		canonicalBytes += segment.Bytes
	}
	canonicalEstimate := (canonicalBytes + 3) / 4
	if plan.InputTokenEstimate != canonicalEstimate ||
		plan.TokenEstimateKind != agent.CanonicalByteTokenEstimateKind {
		t.Fatalf("mixed Token Estimate fallback = %#v", plan)
	}
}

func TestPrefixManifestRejectsMismatchedCanonicalTokenEstimate(t *testing.T) {
	t.Parallel()

	compiled, err := (agent.DefaultContextCompiler{}).Compile(
		context.Background(),
		agent.ContextCompileRequest{
			Provider: "provider",
			ModelRequest: agent.ModelRequest{
				Messages: []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	compiled.Segments[0].TokenEstimate++
	if _, err := agent.BuildPrefixManifest(
		agent.PrefixManifestOptions{Provider: "provider"},
		compiled.Segments,
	); err == nil || !contains(err.Error(), "canonical token estimate") {
		t.Fatalf("BuildPrefixManifest() error = %v", err)
	}
}

func TestRuntimeSelectsProviderEstimatorAndHostOverride(t *testing.T) {
	t.Parallel()

	providerEstimator := &recordingTokenEstimator{kind: "provider_tokenizer", tokensPerItem: 7}
	provider := &tokenEstimatingProvider{
		estimator: providerEstimator,
		streamMessage: agent.Message{
			Role: agent.RoleAssistant,
			Text: "done",
			Usage: &agent.Usage{
				InputTokens: 25, OutputTokens: 2, TotalTokens: 27,
			},
		},
	}
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID: "agent", Input: []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, _, runErr := collectRun(handle)
	if runErr != nil {
		t.Fatal(runErr)
	}
	report, err := agent.BuildTokenUsageReport(context.Background(), events[0].RunID, events)
	if err != nil {
		t.Fatal(err)
	}
	latest, ok := report.Latest()
	if !ok || latest.EstimatedInputTokens != 21 || latest.TokenEstimateKind != "provider_tokenizer" ||
		latest.ProviderInputTokens == nil || *latest.ProviderInputTokens != 25 ||
		latest.DifferenceTokens == nil || *latest.DifferenceTokens != 4 {
		t.Fatalf("Provider Token Usage observation = %#v", latest)
	}

	hostEstimator := &recordingTokenEstimator{kind: "host_tokenizer", tokensPerItem: 5}
	providerEstimator.reset()
	provider.streamMessage = agent.Message{Role: agent.RoleAssistant, Text: "done"}
	runtime, err = agent.NewRuntime(agent.Options{Provider: provider, TokenEstimator: hostEstimator})
	if err != nil {
		t.Fatal(err)
	}
	handle, err = runtime.Run(context.Background(), agent.RunRequest{
		AgentID: "agent", Input: []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, _, runErr = collectRun(handle)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if providerEstimator.callCount() != 0 || hostEstimator.callCount() != 1 {
		t.Fatalf("Token Estimator precedence provider=%d host=%d", providerEstimator.callCount(), hostEstimator.callCount())
	}
	for _, event := range events {
		if event.Type == agent.EventModelRequest &&
			(event.CachePlan == nil || event.CachePlan.TokenEstimateKind != "host_tokenizer") {
			t.Fatalf("host Token Estimate was not used = %#v", event.CachePlan)
		}
	}
}

func TestRuntimeTokenEstimatorFailureStopsBeforeProviderCall(t *testing.T) {
	t.Parallel()

	provider := &tokenEstimatingProvider{
		streamMessage: agent.Message{Role: agent.RoleAssistant, Text: "unused"},
	}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider,
		TokenEstimator: &recordingTokenEstimator{
			err: errors.New("private estimator failure"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID: "agent", Input: []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, result, runErr := collectRun(handle)
	if runErr == nil || !errors.Is(runErr, agent.ErrTokenEstimatorFailed) ||
		contains(runErr.Error(), "private estimator failure") {
		t.Fatalf("Run error = %v", runErr)
	}
	if provider.streamCount() != 0 || result.ProviderCalls != 0 {
		t.Fatalf("Provider calls = %d/%d", provider.streamCount(), result.ProviderCalls)
	}
	for _, event := range events {
		if event.Type == agent.EventModelRequest {
			t.Fatalf("failed estimate emitted model request = %#v", event)
		}
	}
}

func TestRuntimeEstimatesCurrentWorldStateWithResolvedEstimator(t *testing.T) {
	t.Parallel()

	estimator := &recordingTokenEstimator{kind: "world_state_tokenizer", tokensPerItem: 5}
	provider := &tokenEstimatingProvider{
		streamMessage: agent.Message{Role: agent.RoleAssistant, Text: "done"},
	}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:       provider,
		TokenEstimator: estimator,
		CurrentWorldStateSource: worldStateSourceFunc(func(
			context.Context,
			agent.CurrentWorldStateRequest,
		) (agent.CurrentWorldStateSnapshot, error) {
			return worldStateSnapshot(
				"sha256:7692c3ad3540bb803c020b3aee66cd8887123234ea0c6e7143c0add73ff431ed",
				3,
			), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID: "agent", Input: []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, _, runErr := collectRun(handle)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if estimator.callCount() != 2 {
		t.Fatalf("Token Estimator calls = %d, want 2", estimator.callCount())
	}
	found := false
	for _, event := range events {
		if event.Type != agent.EventModelRequest || event.PrefixManifest == nil {
			continue
		}
		for _, segment := range event.PrefixManifest.Segments {
			if segment.Kind == agent.SegmentKindCurrentWorldState {
				found = true
				if segment.TokenEstimate != 5 || segment.TokenEstimateKind != "world_state_tokenizer" {
					t.Fatalf("Current World State Token Estimate = %#v", segment)
				}
			}
		}
	}
	if !found {
		t.Fatal("Current World State Segment was not observed")
	}
}

type recordingTokenEstimator struct {
	mu            sync.Mutex
	kind          string
	tokensPerItem int64
	err           error
	omitLast      bool
	mutate        bool
	calls         int
}

func (estimator *recordingTokenEstimator) EstimateTokens(
	_ context.Context,
	request agent.TokenEstimateRequest,
) (agent.TokenEstimateResult, error) {
	estimator.mu.Lock()
	defer estimator.mu.Unlock()
	estimator.calls++
	if estimator.mutate && len(request.Items) > 0 && len(request.Items[0].Content) > 0 {
		request.Items[0].Content[0] = 'x'
	}
	if estimator.err != nil {
		return agent.TokenEstimateResult{}, estimator.err
	}
	length := len(request.Items)
	if estimator.omitLast && length > 0 {
		length--
	}
	tokens := make([]int64, length)
	for index := range tokens {
		tokens[index] = estimator.tokensPerItem
	}
	return agent.TokenEstimateResult{Kind: estimator.kind, Tokens: tokens}, nil
}

func (estimator *recordingTokenEstimator) callCount() int {
	estimator.mu.Lock()
	defer estimator.mu.Unlock()
	return estimator.calls
}

func (estimator *recordingTokenEstimator) reset() {
	estimator.mu.Lock()
	estimator.calls = 0
	estimator.mu.Unlock()
}

type tokenEstimatingProvider struct {
	mu            sync.Mutex
	estimator     agent.TokenEstimator
	streamMessage agent.Message
	streams       int
}

func (provider *tokenEstimatingProvider) Name() string {
	return "token-provider"
}

func (provider *tokenEstimatingProvider) ModelID() string {
	return "token-model"
}

func (provider *tokenEstimatingProvider) Stream(
	_ context.Context,
	_ agent.ModelRequest,
) (agent.ModelStream, error) {
	provider.mu.Lock()
	provider.streams++
	message := provider.streamMessage
	provider.mu.Unlock()
	return agent.MessageStream(message), nil
}

func (provider *tokenEstimatingProvider) streamCount() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.streams
}

func (provider *tokenEstimatingProvider) EstimateTokens(
	ctx context.Context,
	request agent.TokenEstimateRequest,
) (agent.TokenEstimateResult, error) {
	if provider.estimator == nil {
		return agent.CanonicalByteTokenEstimator{}.EstimateTokens(ctx, request)
	}
	return provider.estimator.EstimateTokens(ctx, request)
}

func contains(value, part string) bool {
	for index := 0; index+len(part) <= len(value); index++ {
		if value[index:index+len(part)] == part {
			return true
		}
	}
	return false
}

var _ agent.TokenEstimator = (*recordingTokenEstimator)(nil)
var _ agent.Provider = (*tokenEstimatingProvider)(nil)
var _ agent.TokenEstimator = (*tokenEstimatingProvider)(nil)
