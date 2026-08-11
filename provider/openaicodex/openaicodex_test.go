package openaicodex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/qed-runtime/qed/agent"
)

func TestStreamUsesCodexContractAndPreservesState(t *testing.T) {
	t.Parallel()

	client := httpClientFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/responses" {
			t.Errorf("path = %q", request.URL.Path)
		}
		for name, want := range map[string]string{
			"Authorization":      "Bearer access-token",
			"ChatGPT-Account-ID": "account-1",
			"OpenAI-Beta":        "responses=experimental",
			"originator":         "qed-test",
			"version":            "1.2.3",
			"X-OpenAI-Fedramp":   "true",
			"Accept":             "text/event-stream",
		} {
			if got := request.Header.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		var body struct {
			Model             string            `json:"model"`
			Instructions      string            `json:"instructions"`
			Input             []json.RawMessage `json:"input"`
			Tools             []responsesTool   `json:"tools"`
			ToolChoice        string            `json:"tool_choice"`
			ParallelToolCalls bool              `json:"parallel_tool_calls"`
			Store             bool              `json:"store"`
			Stream            bool              `json:"stream"`
			Include           []string          `json:"include"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			return nil, fmt.Errorf("decode request: %w", err)
		}
		if body.Model != "gpt-test" || body.Instructions != defaultInstructions || !body.Stream || body.Store ||
			body.ToolChoice != "auto" || !body.ParallelToolCalls {
			t.Errorf("request body = %#v", body)
		}
		if len(body.Include) != 1 || body.Include[0] != "reasoning.encrypted_content" {
			t.Errorf("include = %#v", body.Include)
		}
		if len(body.Input) != 1 || !strings.Contains(string(body.Input[0]), `"type":"input_text"`) {
			t.Errorf("input = %s", body.Input)
		}
		if len(body.Tools) != 1 || body.Tools[0].Name != "read" || body.Tools[0].Strict {
			t.Errorf("tools = %#v", body.Tools)
		}

		bodyText := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\n" +
			"event: response.done\ndata: {\"type\":\"response.done\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-test\",\"status\":\"completed\",\"output\":[{\"type\":\"reasoning\",\"encrypted_content\":\"opaque\"},{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":1,\"cache_write_tokens\":1}}}}\n\n"
		return httpResponse(http.StatusOK, "text/event-stream", bodyText), nil
	})

	modelProvider, err := New(Config{
		ProfileID: "primary",
		AuthorizationSource: AuthorizationSourceFunc(func(context.Context) (Authorization, error) {
			return Authorization{AccessToken: "access-token", AccountID: "account-1", FedRAMP: true}, nil
		}),
		Model:      "gpt-test",
		Originator: "qed-test",
		Version:    "1.2.3",
		HTTPClient: client,
		endpoint:   "https://chatgpt.test/responses",
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := modelProvider.Stream(context.Background(), agent.ModelRequest{
		Messages: []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
		Tools: []agent.ToolDefinition{{
			Name:        "read",
			Description: "Read a file",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	deltas, message := collectStream(t, stream)
	if deltas != "done" || message.Text != "done" || message.ResponseID != "resp_1" {
		t.Fatalf("deltas/message = %q/%#v", deltas, message)
	}
	if message.ProviderState == nil || message.ProviderState.Provider != "openai-codex/responses:primary" ||
		!strings.Contains(string(message.ProviderState.Data), "opaque") {
		t.Fatalf("ProviderState = %#v", message.ProviderState)
	}
	if message.Usage == nil || message.Usage.TotalTokens != 5 ||
		!message.Usage.InputTokenDetailsReported || message.Usage.UncachedInputTokens != 1 ||
		message.Usage.CacheReadInputTokens != 1 || message.Usage.CacheWriteInputTokens != 1 {
		t.Fatalf("usage = %#v", message.Usage)
	}
}

func TestStreamReconstructsTextWhenTerminalOutputIsOmitted(t *testing.T) {
	t.Parallel()

	client := httpClientFunc(func(*http.Request) (*http.Response, error) {
		bodyText := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Q\"}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ED ChatGPT connection OK\"}\n\n" +
			"event: response.done\ndata: {\"type\":\"response.done\",\"response\":{\"id\":\"resp_live\",\"model\":\"gpt-5.6-luna\",\"status\":\"completed\",\"usage\":{\"input_tokens\":26,\"output_tokens\":10,\"total_tokens\":36}}}\n\n"
		return httpResponse(http.StatusOK, "text/event-stream", bodyText), nil
	})

	modelProvider, err := New(Config{
		AuthorizationSource: staticAuthorization(),
		Model:               "gpt-5.6-luna",
		HTTPClient:          client,
		endpoint:            "https://chatgpt.test/responses",
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := modelProvider.Stream(context.Background(), agent.ModelRequest{
		Messages: []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	deltas, message := collectStream(t, stream)
	if deltas != "QED ChatGPT connection OK" || message.Text != deltas {
		t.Fatalf("deltas/message text = %q/%q", deltas, message.Text)
	}
	if message.ResponseID != "resp_live" || message.Model != "gpt-5.6-luna" ||
		message.Usage == nil || message.Usage.TotalTokens != 36 {
		t.Fatalf("message = %#v", message)
	}
	if message.ProviderState == nil || !strings.Contains(string(message.ProviderState.Data), deltas) {
		t.Fatalf("ProviderState = %#v", message.ProviderState)
	}
	continuation, err := modelProvider.responsesRequest(agent.ModelRequest{Messages: []agent.Message{message}})
	if err != nil {
		t.Fatal(err)
	}
	if len(continuation.Input) != 1 || !strings.Contains(string(continuation.Input[0]), deltas) {
		t.Fatalf("continuation input = %s", continuation.Input)
	}
}

func TestStreamReconstructsFinalizedTextWithoutDeltas(t *testing.T) {
	t.Parallel()

	client := httpClientFunc(func(*http.Request) (*http.Response, error) {
		bodyText := "event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"text\":\"final text\"}\n\n" +
			"event: response.done\ndata: {\"type\":\"response.done\",\"response\":{\"id\":\"resp_final\",\"model\":\"gpt-5.6-luna\",\"status\":\"completed\"}}\n\n"
		return httpResponse(http.StatusOK, "text/event-stream", bodyText), nil
	})

	modelProvider, err := New(Config{
		AuthorizationSource: staticAuthorization(),
		Model:               "gpt-5.6-luna",
		HTTPClient:          client,
		endpoint:            "https://chatgpt.test/responses",
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := modelProvider.Stream(context.Background(), agent.ModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	deltas, message := collectStream(t, stream)
	if deltas != "" || message.Text != "final text" {
		t.Fatalf("deltas/message text = %q/%q", deltas, message.Text)
	}
}

func TestStreamReconstructsDoneOutputItemsWhenTerminalOutputIsOmitted(t *testing.T) {
	t.Parallel()

	client := httpClientFunc(func(*http.Request) (*http.Response, error) {
		bodyText := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"opaque\"}}\n\n" +
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"note.txt\\\"}\"}}\n\n" +
			"event: response.done\ndata: {\"type\":\"response.done\",\"response\":{\"id\":\"resp_tool\",\"model\":\"gpt-5.6-luna\",\"status\":\"completed\"}}\n\n"
		return httpResponse(http.StatusOK, "text/event-stream", bodyText), nil
	})

	modelProvider, err := New(Config{
		AuthorizationSource: staticAuthorization(),
		Model:               "gpt-5.6-luna",
		HTTPClient:          client,
		endpoint:            "https://chatgpt.test/responses",
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := modelProvider.Stream(context.Background(), agent.ModelRequest{
		Messages: []agent.Message{{Role: agent.RoleUser, Text: "read the note"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, message := collectStream(t, stream)
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != "call_1" ||
		message.ToolCalls[0].Name != "read" || string(message.ToolCalls[0].Arguments) != `{"path":"note.txt"}` {
		t.Fatalf("ToolCalls = %#v", message.ToolCalls)
	}
	if message.ProviderState == nil || !strings.Contains(string(message.ProviderState.Data), "opaque") ||
		!strings.Contains(string(message.ProviderState.Data), "call_1") {
		t.Fatalf("ProviderState = %#v", message.ProviderState)
	}
}

func TestResponsesMessagePreservesMalformedToolArgumentsForRuntimeValidation(t *testing.T) {
	t.Parallel()

	message, err := (&Provider{}).messageFromResponsesResponse(responsesResponse{
		Status: "completed",
		Output: []json.RawMessage{
			json.RawMessage(`{"type":"function_call","call_id":"call-1","name":"read","arguments":"{"}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(message.ToolCalls) != 1 || string(message.ToolCalls[0].Arguments) != `{` {
		t.Fatalf("Tool calls = %#v", message.ToolCalls)
	}
}

func TestStreamReplaysOpaqueContinuationState(t *testing.T) {
	t.Parallel()

	state := json.RawMessage(`[{"type":"reasoning","encrypted_content":"secret-state"},{"type":"function_call","call_id":"call_1","name":"read","arguments":"{}"}]`)
	client := httpClientFunc(func(request *http.Request) (*http.Response, error) {
		var body struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			return nil, err
		}
		if len(body.Input) != 3 || !strings.Contains(string(body.Input[0]), "secret-state") ||
			!strings.Contains(string(body.Input[2]), "function_call_output") {
			t.Errorf("input = %s", body.Input)
		}
		return httpResponse(http.StatusOK, "application/json", `{"id":"resp_2","model":"gpt-test","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`), nil
	})

	modelProvider, err := New(Config{
		ProfileID:           "primary",
		AuthorizationSource: staticAuthorization(),
		Model:               "gpt-test",
		HTTPClient:          client,
		endpoint:            "https://chatgpt.test/responses",
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := modelProvider.Stream(context.Background(), agent.ModelRequest{Messages: []agent.Message{
		{Role: agent.RoleAssistant, ProviderState: &agent.ProviderState{Provider: modelProvider.Name(), Data: state}},
		{Role: agent.RoleTool, ToolCallID: "call_1", Text: "result"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, message := collectStream(t, stream)
	if message.Text != "ok" {
		t.Fatalf("message = %#v", message)
	}
}

func TestStreamRefreshesOnceAfterUnauthorized(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	client := httpClientFunc(func(request *http.Request) (*http.Response, error) {
		count := requests.Add(1)
		if count == 1 {
			if request.Header.Get("Authorization") != "Bearer stale" {
				t.Errorf("first Authorization = %q", request.Header.Get("Authorization"))
			}
			return httpResponse(http.StatusUnauthorized, "application/json", `{"error":{"code":"expired","message":"expired"}}`), nil
		}
		if request.Header.Get("Authorization") != "Bearer fresh" {
			t.Errorf("second Authorization = %q", request.Header.Get("Authorization"))
		}
		return httpResponse(http.StatusOK, "application/json", `{"id":"resp_1","model":"gpt-test","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`), nil
	})

	source := &recoveringSource{current: Authorization{AccessToken: "stale", AccountID: "account-1"}}
	modelProvider, err := New(Config{
		AuthorizationSource: source,
		Model:               "gpt-test",
		HTTPClient:          client,
		endpoint:            "https://chatgpt.test/responses",
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := modelProvider.Stream(context.Background(), agent.ModelRequest{Messages: []agent.Message{{Role: agent.RoleUser, Text: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, message := collectStream(t, stream)
	if message.Text != "ok" || source.recoveries.Load() != 1 || requests.Load() != 2 {
		t.Fatalf("message/recoveries/requests = %#v/%d/%d", message, source.recoveries.Load(), requests.Load())
	}
}

func TestNewValidatesRequiredConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config Config
		want   string
	}{
		{name: "authorization", config: Config{Model: "gpt-test"}, want: "authorization source is required"},
		{name: "model", config: Config{AuthorizationSource: staticAuthorization()}, want: "model is required"},
		{name: "model whitespace", config: Config{AuthorizationSource: staticAuthorization(), Model: " gpt-test"}, want: "surrounding whitespace"},
		{name: "profile", config: Config{ProfileID: " bad-profile", AuthorizationSource: staticAuthorization(), Model: "gpt-test"}, want: "profile"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(test.config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAuthorizationErrorsDoNotIncludeCredentials(t *testing.T) {
	t.Parallel()

	modelProvider, err := New(Config{
		AuthorizationSource: AuthorizationSourceFunc(func(context.Context) (Authorization, error) {
			return Authorization{AccessToken: "do-not-expose", AccountID: "account-1"}, errors.New("credential unavailable")
		}),
		Model: "gpt-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = modelProvider.Stream(context.Background(), agent.ModelRequest{})
	if err == nil || !strings.Contains(err.Error(), "credential unavailable") || strings.Contains(err.Error(), "do-not-expose") {
		t.Fatalf("Stream() error = %v", err)
	}
}

type recoveringSource struct {
	mu         sync.Mutex
	current    Authorization
	recoveries atomic.Int32
}

func (source *recoveringSource) Authorization(context.Context) (Authorization, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.current, nil
}

func (source *recoveringSource) RecoverUnauthorized(_ context.Context, _ Authorization) (Authorization, error) {
	source.recoveries.Add(1)
	source.mu.Lock()
	defer source.mu.Unlock()
	source.current.AccessToken = "fresh"
	return source.current, nil
}

func staticAuthorization() AuthorizationSource {
	return AuthorizationSourceFunc(func(context.Context) (Authorization, error) {
		return Authorization{AccessToken: "token", AccountID: "account-1"}, nil
	})
}

func collectStream(t *testing.T, stream agent.ModelStream) (string, agent.Message) {
	t.Helper()
	defer stream.Close()
	var text strings.Builder
	var message agent.Message
	for {
		event, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next() error = %v", err)
		}
		switch event.Type {
		case agent.ModelStreamTextDelta:
			text.WriteString(event.TextDelta)
		case agent.ModelStreamMessageComplete:
			if event.Message == nil {
				t.Fatal("completed event has no message")
			}
			message = *event.Message
		}
	}
	return text.String(), message
}

type httpClientFunc func(*http.Request) (*http.Response, error)

func (client httpClientFunc) Do(request *http.Request) (*http.Response, error) {
	return client(request)
}

func httpResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
