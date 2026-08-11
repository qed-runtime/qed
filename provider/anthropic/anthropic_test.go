package anthropic_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qed-runtime/qed/agent"
	providerbase "github.com/qed-runtime/qed/provider"
	"github.com/qed-runtime/qed/provider/anthropic"
)

func TestMessagesToolLoopPreservesBlocksAndGroupsResults(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/messages" {
			t.Errorf("path = %q, want /v1/messages", request.URL.Path)
		}
		if got := request.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := request.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q", got)
		}

		var body struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			System    string `json:"system"`
			Messages  []struct {
				Role    string            `json:"role"`
				Content []json.RawMessage `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"input_schema"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")

		switch requestCount.Add(1) {
		case 1:
			if body.Model != "test-model" || body.MaxTokens != 256 || body.System != "Use tools" {
				t.Errorf("model request = %#v", body)
			}
			if len(body.Messages) != 1 || body.Messages[0].Role != "user" || len(body.Tools) != 1 || body.Tools[0].Name != "uppercase" {
				t.Errorf("messages/tools = %#v/%#v", body.Messages, body.Tools)
			}
			_, _ = writer.Write([]byte(`{
                    "id":"msg_1",
                    "type":"message",
                    "role":"assistant",
                    "model":"test-model",
                    "content":[
                        {"type":"thinking","thinking":"opaque reasoning","signature":"signature_1"},
                        {"type":"tool_use","id":"tool_1","name":"uppercase","input":{"text":"hello"}},
                        {"type":"tool_use","id":"tool_2","name":"missing","input":{}}
                    ],
                    "stop_reason":"tool_use",
                    "usage":{"input_tokens":5,"output_tokens":4}
                }`))
		case 2:
			if len(body.Messages) != 3 {
				t.Errorf("message count = %d, want 3", len(body.Messages))
			} else {
				assistantMessage := body.Messages[1]
				if assistantMessage.Role != "assistant" || len(assistantMessage.Content) != 3 {
					t.Errorf("assistant continuation = %#v", assistantMessage)
				} else {
					var thinking struct {
						Type      string `json:"type"`
						Signature string `json:"signature"`
					}
					if err := json.Unmarshal(assistantMessage.Content[0], &thinking); err != nil {
						t.Errorf("decode thinking: %v", err)
					}
					if thinking.Type != "thinking" || thinking.Signature != "signature_1" {
						t.Errorf("thinking = %#v", thinking)
					}
				}

				toolResults := body.Messages[2]
				if toolResults.Role != "user" || len(toolResults.Content) != 2 {
					t.Errorf("tool results = %#v", toolResults)
				} else {
					var first struct {
						Type      string `json:"type"`
						ToolUseID string `json:"tool_use_id"`
						Content   string `json:"content"`
						IsError   bool   `json:"is_error"`
					}
					var second = first
					_ = json.Unmarshal(toolResults.Content[0], &first)
					_ = json.Unmarshal(toolResults.Content[1], &second)
					if first.ToolUseID != "tool_1" || first.Content != "HELLO" || first.IsError {
						t.Errorf("first tool result = %#v", first)
					}
					if second.ToolUseID != "tool_2" || !second.IsError || !strings.Contains(second.Content, "not registered") {
						t.Errorf("second tool result = %#v", second)
					}
				}
			}
			_, _ = writer.Write([]byte(`{
                    "id":"msg_2",
                    "type":"message",
                    "role":"assistant",
                    "model":"test-model",
                    "content":[{"type":"text","text":"HELLO"}],
                    "stop_reason":"end_turn",
                    "usage":{
                        "input_tokens":3,
                        "cache_creation_input_tokens":2,
                        "cache_read_input_tokens":5,
                        "output_tokens":4
                    }
                }`))
		default:
			t.Errorf("unexpected request count")
			writer.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	modelProvider, err := anthropic.New(anthropic.Config{
		APIKey:          "test-key",
		BaseURL:         server.URL + "/v1",
		Model:           "test-model",
		MaxOutputTokens: 256,
		HTTPClient:      server.Client(),
	})
	if err != nil {
		t.Fatalf("anthropic.New: %v", err)
	}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: modelProvider,
		Tools:    []agent.Tool{uppercaseTool{}},
	})
	if err != nil {
		t.Fatalf("agent.NewRuntime: %v", err)
	}

	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Instructions: "Use tools",
		Input:        []agent.Message{{Role: agent.RoleUser, Text: "uppercase hello"}},
	})
	if err != nil {
		t.Fatalf("Runtime.Run: %v", err)
	}
	for range handle.Events() {
	}
	result, err := handle.Wait()
	if err != nil {
		t.Fatalf("RunHandle.Wait: %v", err)
	}
	if requestCount.Load() != 2 {
		t.Errorf("request count = %d, want 2", requestCount.Load())
	}
	if len(result.ToolResults) != 2 || !result.ToolResults[1].IsError {
		t.Errorf("tool results = %#v", result.ToolResults)
	}
	last := result.Messages[len(result.Messages)-1]
	if last.Text != "HELLO" || last.StopReason != agent.StopReasonEndTurn {
		t.Errorf("last message = %#v", last)
	}
	if last.Usage == nil || last.Usage.InputTokens != 10 || last.Usage.TotalTokens != 14 ||
		!last.Usage.InputTokenDetailsReported || last.Usage.UncachedInputTokens != 3 ||
		last.Usage.CacheReadInputTokens != 5 || last.Usage.CacheWriteInputTokens != 2 {
		t.Errorf("usage = %#v", last.Usage)
	}

	serialized, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal result: %v", err)
	}
	if strings.Contains(string(serialized), "opaque reasoning") || strings.Contains(string(serialized), "signature_1") {
		t.Errorf("serialized result exposed ProviderState: %s", serialized)
	}
}

func TestProfileIDAndCredentialSource(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		count := requestCount.Add(1)
		want := "first-token"
		if count == 2 {
			want = "second-token"
		}
		if got := request.Header.Get("x-api-key"); got != want {
			t.Errorf("request %d x-api-key = %q, want %q", count, got, want)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"id":"message_1",
			"type":"message",
			"role":"assistant",
			"model":"test-model",
			"content":[{"type":"text","text":"done"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":1,"output_tokens":1}
		}`))
	}))
	defer server.Close()

	var sourceCalls atomic.Int32
	modelProvider, err := anthropic.New(anthropic.Config{
		ProfileID: "reviewer",
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
		t.Fatalf("anthropic.New: %v", err)
	}
	if modelProvider.Name() != "anthropic/messages:reviewer" {
		t.Errorf("Name() = %q", modelProvider.Name())
	}
	request := agent.ModelRequest{Messages: []agent.Message{{Role: agent.RoleUser, Text: "hello"}}}
	for range 2 {
		response, err := modelProvider.Complete(context.Background(), request)
		if err != nil {
			t.Fatalf("Complete() error = %v", err)
		}
		if response.ProviderState == nil || response.ProviderState.Provider != "anthropic/messages:reviewer" {
			t.Errorf("ProviderState = %#v", response.ProviderState)
		}
	}
	if sourceCalls.Load() != 2 || requestCount.Load() != 2 {
		t.Errorf("source/request calls = %d/%d", sourceCalls.Load(), requestCount.Load())
	}
}

func TestNewRejectsAPIKeyWithCredentialSource(t *testing.T) {
	t.Parallel()

	_, err := anthropic.New(anthropic.Config{
		APIKey: "static-key",
		CredentialSource: providerbase.CredentialSourceFunc(func(context.Context) (string, error) {
			return "dynamic-key", nil
		}),
		Model: "test-model",
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("anthropic.New() error = %v", err)
	}
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
