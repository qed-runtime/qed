package orcarouter_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	providerbase "github.com/qed-runtime/qed/provider"
	"github.com/qed-runtime/qed/provider/orcarouter"
)

func TestProviderAddsStableOpaqueAffinityAndNormalizesRoutingMetadata(t *testing.T) {
	tests := []struct {
		name string
		api  orcarouter.API
		path string
		body string
	}{
		{
			name: "responses",
			api:  orcarouter.APIResponses,
			path: "/v1/responses",
			body: `{"id":"response-id","model":"orcarouter/auto","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3,"cost_usd":0.0012345}}`,
		},
		{
			name: "chat_completions",
			api:  orcarouter.APIChatCompletions,
			path: "/v1/chat/completions",
			body: `{"id":"response-id","model":"orcarouter/auto","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":""}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3,"cost_usd":0.0012345}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var affinities []string
			client := contractClientFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != test.path {
					t.Errorf("path = %q, want %q", request.URL.Path, test.path)
				}
				if request.Header.Get("Authorization") != "Bearer secret" {
					t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
				}
				if request.Header.Get("X-OrcaRouter-Include-Cost") != "true" {
					t.Errorf("cost header = %q", request.Header.Get("X-OrcaRouter-Include-Cost"))
				}
				affinities = append(affinities, request.Header.Get("X-OrcaRouter-Session-Id"))
				response := httpResponse(http.StatusOK, "application/json", test.body)
				response.Header.Set("X-Orca-Request-Id", "request-id")
				response.Header.Set("X-Orca-Resolved-Model", "resolved-model")
				return response, nil
			})
			modelProvider, err := orcarouter.New(orcarouter.Config{
				ProfileID:  "primary",
				API:        test.api,
				APIKey:     "secret",
				BaseURL:    "https://router.invalid/v1",
				Model:      "orcarouter/auto",
				HTTPClient: client,
			})
			if err != nil {
				t.Fatal(err)
			}

			requests := []agent.ModelRequest{
				{RunID: "run-one", AgentID: "coding", SessionID: "private-session", Messages: userMessages()},
				{RunID: "run-two", AgentID: "coding", SessionID: "private-session", Messages: userMessages()},
				{RunID: "run-one", AgentID: "coding", Messages: userMessages()},
				{RunID: "run-two", AgentID: "coding", Messages: userMessages()},
			}
			for _, request := range requests {
				message := streamMessage(t, modelProvider, request)
				if message.RequestID != "request-id" || message.ResponseID != "response-id" ||
					message.Model != "resolved-model" {
					t.Errorf("message identity = %#v", message)
				}
				if message.Usage == nil || message.Usage.CostMicros != 1235 {
					t.Errorf("Usage = %#v, want 1235 cost micros", message.Usage)
				}
			}
			if len(affinities) != 4 || affinities[0] == "" || affinities[0] != affinities[1] ||
				affinities[0] == affinities[2] || affinities[2] == affinities[3] {
				t.Fatalf("affinities = %#v", affinities)
			}
			for _, affinity := range affinities {
				if !strings.HasPrefix(affinity, "qed-") || strings.Contains(affinity, "private-session") ||
					strings.Contains(affinity, "run-one") || strings.Contains(affinity, "run-two") {
					t.Errorf("unsafe affinity = %q", affinity)
				}
			}
		})
	}
}

func TestResponsesContinuationStateRemainsOwnedByOrcaRouter(t *testing.T) {
	requestCount := 0
	client := contractClientFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		var body struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if requestCount == 2 {
			var foundReasoning bool
			for _, item := range body.Input {
				var kind struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(item, &kind) == nil && kind.Type == "reasoning" {
					foundReasoning = true
				}
			}
			if !foundReasoning {
				t.Errorf("second input did not preserve Responses continuation state: %s", body.Input)
			}
		}
		return httpResponse(http.StatusOK, "application/json", `{"id":"response-id","model":"model","status":"completed","output":[{"type":"reasoning","id":"reasoning-id","summary":[]}]}`), nil
	})
	modelProvider, err := orcarouter.New(orcarouter.Config{
		API: orcarouter.APIResponses, BaseURL: "https://router.invalid/v1", Model: "model", HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	first := streamMessage(t, modelProvider, agent.ModelRequest{RunID: "run", Messages: userMessages()})
	if first.ProviderState == nil || first.ProviderState.Provider != modelProvider.Name() {
		t.Fatalf("ProviderState = %#v, want owner %q", first.ProviderState, modelProvider.Name())
	}
	streamMessage(t, modelProvider, agent.ModelRequest{
		RunID: "run",
		Messages: []agent.Message{
			{Role: agent.RoleUser, Text: "first"},
			first,
			{Role: agent.RoleUser, Text: "continue"},
		},
	})
}

func TestProviderNormalizesStreamingCost(t *testing.T) {
	tests := []struct {
		name string
		api  orcarouter.API
		body string
	}{
		{
			name: "responses",
			api:  orcarouter.APIResponses,
			body: contractSSE(eventData("response.completed", `{"type":"response.completed","response":{"id":"response-id","model":"model","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3,"cost_usd":0.000002}}}`)),
		},
		{
			name: "chat_completions",
			api:  orcarouter.APIChatCompletions,
			body: contractSSE(
				eventData("", `{"id":"response-id","model":"model","choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`),
				eventData("", `{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3,"cost_usd":0.000002}}`),
				eventData("", `[DONE]`),
			),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := contractClientFunc(func(*http.Request) (*http.Response, error) {
				return httpResponse(http.StatusOK, "text/event-stream", test.body), nil
			})
			modelProvider, err := orcarouter.New(orcarouter.Config{
				API: test.api, BaseURL: "https://router.invalid/v1", Model: "model", HTTPClient: client,
			})
			if err != nil {
				t.Fatal(err)
			}
			message := streamMessage(t, modelProvider, agent.ModelRequest{RunID: "run", Messages: userMessages()})
			if message.Usage == nil || message.Usage.CostMicros != 2 {
				t.Fatalf("Usage = %#v, want 2 cost micros", message.Usage)
			}
		})
	}
}

func TestHTTPErrorUsesOrcaRequestID(t *testing.T) {
	client := contractClientFunc(func(*http.Request) (*http.Response, error) {
		response := httpResponse(http.StatusTooManyRequests, "application/json", `{"error":{"type":"rate_limit_error","code":"rate_limit","message":"slow down"}}`)
		response.Header.Set("X-Orca-Request-Id", "orca-error-request")
		return response, nil
	})
	modelProvider, err := orcarouter.New(orcarouter.Config{
		BaseURL: "https://router.invalid/v1", Model: "model", HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := modelProvider.Stream(context.Background(), agent.ModelRequest{RunID: "run", Messages: userMessages()})
	if stream != nil {
		_ = stream.Close()
		t.Fatal("Stream returned a stream for an HTTP error")
	}
	var httpError *providerbase.HTTPError
	if !errors.As(err, &httpError) || httpError.RequestID != "orca-error-request" {
		t.Fatalf("error = %#v, want OrcaRouter request ID", err)
	}
}

func TestCacheCapabilitiesAreConservativeUnlessDeclared(t *testing.T) {
	configured := agent.CacheCapabilities{
		ExactPrefix: true, SupportsAutomatic: true, SupportsCacheKey: true, MinimumPrefixTokens: 1024,
	}
	modelProvider, err := orcarouter.New(orcarouter.Config{
		Model: "orcarouter/auto", CacheCapabilities: &configured,
	})
	if err != nil {
		t.Fatal(err)
	}
	configured.MinimumPrefixTokens = 1
	want := agent.CacheCapabilities{
		ExactPrefix: true, SupportsAutomatic: true, SupportsCacheKey: true, MinimumPrefixTokens: 1024,
	}
	if got := modelProvider.CacheCapabilities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("CacheCapabilities() = %#v, want %#v", got, want)
	}
	conservative, err := orcarouter.New(orcarouter.Config{
		BaseURL: "https://api.openai.com/v1", Model: "gpt-5.6-luna",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := conservative.CacheCapabilities(); !reflect.DeepEqual(got, agent.CacheCapabilities{}) {
		t.Fatalf("default CacheCapabilities() = %#v, want zero value", got)
	}
}

func streamMessage(t *testing.T, modelProvider agent.Provider, request agent.ModelRequest) agent.Message {
	t.Helper()
	stream, err := modelProvider.Stream(context.Background(), request)
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()
	for {
		event, nextErr := stream.Next()
		if errors.Is(nextErr, io.EOF) {
			t.Fatal("stream ended without a completed Message")
		}
		if nextErr != nil {
			t.Fatalf("Next() error = %v", nextErr)
		}
		if event.Type == agent.ModelStreamMessageComplete && event.Message != nil {
			return *event.Message
		}
	}
}

func userMessages() []agent.Message {
	return []agent.Message{{Role: agent.RoleUser, Text: "hello"}}
}
