package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qed-runtime/qed/agent"
	providerbase "github.com/qed-runtime/qed/provider"
	"github.com/qed-runtime/qed/provider/openai"
)

func TestChatCompletionsToolLoop(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}

		var body struct {
			Model               string `json:"model"`
			MaxCompletionTokens int    `json:"max_completion_tokens"`
			Messages            []struct {
				Role       string         `json:"role"`
				Content    *string        `json:"content"`
				ToolCallID string         `json:"tool_call_id"`
				ToolCalls  []chatToolCall `json:"tool_calls"`
			} `json:"messages"`
			Tools []struct {
				Type     string `json:"type"`
				Function struct {
					Name       string          `json:"name"`
					Parameters json.RawMessage `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Model != "test-model" || body.MaxCompletionTokens != 128 {
			t.Errorf("model/max tokens = %q/%d", body.Model, body.MaxCompletionTokens)
		}
		if len(body.Tools) != 1 || body.Tools[0].Type != "function" || body.Tools[0].Function.Name != "uppercase" {
			t.Errorf("tools = %#v", body.Tools)
		}

		writer.Header().Set("Content-Type", "application/json")
		switch requestCount.Add(1) {
		case 1:
			if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Role != "user" {
				t.Errorf("first messages = %#v", body.Messages)
			}
			_, _ = writer.Write([]byte(`{
                    "id":"chat_1",
                    "model":"test-model",
                    "choices":[{
                        "index":0,
                        "finish_reason":"tool_calls",
                        "message":{
                            "role":"assistant",
                            "content":null,
                            "tool_calls":[{
                                "id":"call_1",
                                "type":"function",
                                "function":{"name":"uppercase","arguments":"{\"text\":\"hello\"}"}
                            }]
                        }
                    }],
                    "usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}
                }`))
		case 2:
			last := body.Messages[len(body.Messages)-1]
			if last.Role != "tool" || last.ToolCallID != "call_1" || last.Content == nil || *last.Content != "HELLO" {
				t.Errorf("tool result message = %#v", last)
			}
			_, _ = writer.Write([]byte(`{
                    "id":"chat_2",
                    "model":"test-model",
                    "choices":[{
                        "index":0,
                        "finish_reason":"stop",
                        "message":{"role":"assistant","content":"HELLO"}
                    }],
                    "usage":{"prompt_tokens":20,"completion_tokens":2,"total_tokens":22}
                }`))
		default:
			t.Errorf("unexpected request count")
			writer.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	modelProvider, err := openai.New(openai.Config{
		API:             openai.APIChatCompletions,
		APIKey:          "test-key",
		BaseURL:         server.URL + "/v1",
		Model:           "test-model",
		MaxOutputTokens: 128,
		HTTPClient:      server.Client(),
	})
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: modelProvider,
		Tools:    []agent.Tool{uppercaseTool{}},
	})
	if err != nil {
		t.Fatalf("agent.NewRuntime: %v", err)
	}

	result := runAgent(t, runtime, agent.RunRequest{
		Instructions: "Use tools when requested",
		Input:        []agent.Message{{Role: agent.RoleUser, Text: "uppercase hello"}},
	})
	if requestCount.Load() != 2 {
		t.Errorf("request count = %d, want 2", requestCount.Load())
	}
	last := result.Messages[len(result.Messages)-1]
	if last.Text != "HELLO" || last.StopReason != agent.StopReasonEndTurn {
		t.Errorf("last message = %#v", last)
	}
	if last.Usage == nil || last.Usage.TotalTokens != 22 {
		t.Errorf("usage = %#v", last.Usage)
	}
}

func TestResponsesPreservesOutputItemsForToolContinuation(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" {
			t.Errorf("path = %q, want /v1/responses", request.URL.Path)
		}
		var body struct {
			Model        string            `json:"model"`
			Instructions string            `json:"instructions"`
			Input        []json.RawMessage `json:"input"`
			Tools        []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")

		switch requestCount.Add(1) {
		case 1:
			if body.Model != "test-model" || body.Instructions != "Use tools" || len(body.Input) != 1 || len(body.Tools) != 1 {
				t.Errorf("first request = %#v", body)
			}
			_, _ = writer.Write([]byte(`{
                    "id":"resp_1",
                    "model":"test-model",
                    "status":"completed",
                    "output":[
                        {"type":"reasoning","id":"rs_1","summary":[]},
                        {"type":"function_call","id":"fc_1","call_id":"call_1","name":"uppercase","arguments":"{\"text\":\"hello\"}","status":"completed"}
                    ],
                    "usage":{"input_tokens":8,"output_tokens":4,"total_tokens":12}
                }`))
		case 2:
			var types []string
			for _, rawItem := range body.Input {
				var item struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
					Output string `json:"output"`
				}
				if err := json.Unmarshal(rawItem, &item); err != nil {
					t.Errorf("decode input item: %v", err)
				}
				types = append(types, item.Type)
				if item.Type == "function_call_output" && (item.CallID != "call_1" || item.Output != "HELLO") {
					t.Errorf("function output = %#v", item)
				}
			}
			wantTypes := []string{"message", "reasoning", "function_call", "function_call_output"}
			if strings.Join(types, ",") != strings.Join(wantTypes, ",") {
				t.Errorf("input types = %v, want %v", types, wantTypes)
			}
			_, _ = writer.Write([]byte(`{
                    "id":"resp_2",
                    "model":"test-model",
                    "status":"completed",
                    "output":[{
                        "type":"message",
                        "id":"msg_1",
                        "role":"assistant",
                        "status":"completed",
                        "content":[{"type":"output_text","text":"HELLO","annotations":[]}]
                    }],
                    "usage":{"input_tokens":14,"output_tokens":2,"total_tokens":16}
                }`))
		default:
			t.Errorf("unexpected request count")
			writer.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	modelProvider, err := openai.New(openai.Config{
		API:        openai.APIResponses,
		APIKey:     "test-key",
		BaseURL:    server.URL + "/v1",
		Model:      "test-model",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: modelProvider,
		Tools:    []agent.Tool{uppercaseTool{}},
	})
	if err != nil {
		t.Fatalf("agent.NewRuntime: %v", err)
	}

	result := runAgent(t, runtime, agent.RunRequest{
		Instructions: "Use tools",
		Input:        []agent.Message{{Role: agent.RoleUser, Text: "uppercase hello"}},
	})
	if requestCount.Load() != 2 {
		t.Errorf("request count = %d, want 2", requestCount.Load())
	}
	firstAssistant := result.Messages[1]
	if firstAssistant.StopReason != agent.StopReasonToolUse || firstAssistant.ProviderState == nil {
		t.Errorf("first assistant = %#v", firstAssistant)
	}
	last := result.Messages[len(result.Messages)-1]
	if last.Text != "HELLO" || last.ResponseID != "resp_2" {
		t.Errorf("last message = %#v", last)
	}
}

func TestHTTPErrorIsInspectable(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("x-request-id", "request_123")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"error":{"type":"rate_limit_error","code":"rate_limit","message":"slow down"}}`))
	}))
	defer server.Close()

	modelProvider, err := openai.New(openai.Config{
		API:        openai.APIChatCompletions,
		BaseURL:    server.URL,
		Model:      "test-model",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	_, err = modelProvider.Complete(context.Background(), agent.ModelRequest{
		Messages: []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
	})
	var httpError *providerbase.HTTPError
	if !errors.As(err, &httpError) {
		t.Fatalf("error = %v, want *provider.HTTPError", err)
	}
	if httpError.StatusCode != http.StatusTooManyRequests || httpError.RequestID != "request_123" || httpError.Code != "rate_limit" {
		t.Errorf("HTTPError = %#v", httpError)
	}
}

func TestNewRejectsCredentialInBaseURL(t *testing.T) {
	t.Parallel()

	_, err := openai.New(openai.Config{
		BaseURL: "https://user:password@example.com/v1",
		Model:   "test-model",
	})
	if err == nil || !strings.Contains(err.Error(), "must not contain credentials") {
		t.Fatalf("error = %v", err)
	}
}

func TestProfileIDDistinguishesProviderInstances(t *testing.T) {
	t.Parallel()

	first, err := openai.New(openai.Config{ProfileID: "primary", Model: "test-model"})
	if err != nil {
		t.Fatalf("openai.New(first): %v", err)
	}
	second, err := openai.New(openai.Config{ProfileID: "reviewer", Model: "test-model"})
	if err != nil {
		t.Fatalf("openai.New(second): %v", err)
	}
	legacy, err := openai.New(openai.Config{Model: "test-model"})
	if err != nil {
		t.Fatalf("openai.New(legacy): %v", err)
	}
	if first.Name() != "openai/responses:primary" || second.Name() != "openai/responses:reviewer" {
		t.Errorf("profile names = %q/%q", first.Name(), second.Name())
	}
	if legacy.Name() != "openai/responses" {
		t.Errorf("legacy name = %q", legacy.Name())
	}
}

func TestCredentialSourceIsResolvedForEveryRequest(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		count := requestCount.Add(1)
		want := "Bearer first-token"
		if count == 2 {
			want = "Bearer second-token"
		}
		if got := request.Header.Get("Authorization"); got != want {
			t.Errorf("request %d Authorization = %q, want %q", count, got, want)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"id":"response_1",
			"model":"test-model",
			"status":"completed",
			"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],
			"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.Close()

	var sourceCalls atomic.Int32
	modelProvider, err := openai.New(openai.Config{
		ProfileID: "primary",
		CredentialSource: providerbase.CredentialSourceFunc(func(context.Context) (string, error) {
			if sourceCalls.Add(1) == 1 {
				return "first-token", nil
			}
			return "second-token", nil
		}),
		BaseURL:    server.URL,
		Model:      "test-model",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	request := agent.ModelRequest{Messages: []agent.Message{{Role: agent.RoleUser, Text: "hello"}}}
	for range 2 {
		response, err := modelProvider.Complete(context.Background(), request)
		if err != nil {
			t.Fatalf("Complete() error = %v", err)
		}
		if response.ProviderState == nil || response.ProviderState.Provider != "openai/responses:primary" {
			t.Errorf("ProviderState = %#v", response.ProviderState)
		}
	}
	if sourceCalls.Load() != 2 || requestCount.Load() != 2 {
		t.Errorf("source/request calls = %d/%d", sourceCalls.Load(), requestCount.Load())
	}
}

func TestNewRejectsAPIKeyWithCredentialSource(t *testing.T) {
	t.Parallel()

	_, err := openai.New(openai.Config{
		APIKey: "static-key",
		CredentialSource: providerbase.CredentialSourceFunc(func(context.Context) (string, error) {
			return "dynamic-key", nil
		}),
		Model: "test-model",
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("openai.New() error = %v", err)
	}
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type uppercaseTool struct{}

func (uppercaseTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        "uppercase",
		Description: "Convert text to uppercase",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
	}
}

func (uppercaseTool) Execute(_ context.Context, call agent.ToolCall) (agent.ToolResult, error) {
	var input struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(call.Arguments, &input); err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Output: strings.ToUpper(input.Text)}, nil
}

func runAgent(t *testing.T, runtime *agent.Runtime, request agent.RunRequest) agent.RunResult {
	t.Helper()

	handle, err := runtime.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("Runtime.Run: %v", err)
	}
	for range handle.Events() {
	}
	result, err := handle.Wait()
	if err != nil {
		t.Fatalf("RunHandle.Wait: %v", err)
	}
	return result
}
