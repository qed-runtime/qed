package orchestration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/orchestration"
	"github.com/qed-runtime/qed/provider/anthropic"
	"github.com/qed-runtime/qed/provider/openai"
)

func TestOpenAIParentDelegatesToAnthropicChild(t *testing.T) {
	t.Parallel()

	var anthropicRequests atomic.Int32
	anthropicServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		anthropicRequests.Add(1)
		if request.URL.Path != "/v1/messages" {
			t.Errorf("Anthropic path = %q, want /v1/messages", request.URL.Path)
		}
		if got := request.Header.Get("x-api-key"); got != "anthropic-test-key" {
			t.Errorf("Anthropic x-api-key = %q", got)
		}

		var body struct {
			Model    string `json:"model"`
			System   string `json:"system"`
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode Anthropic request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Model != "anthropic-test-model" || body.System != "Analyze the delegated request\n\nBe concise" {
			t.Errorf("Anthropic model/system = %q/%q", body.Model, body.System)
		}
		if len(body.Messages) != 1 || body.Messages[0].Role != "user" ||
			len(body.Messages[0].Content) != 1 || body.Messages[0].Content[0].Text != "Review protocol boundary" {
			t.Errorf("Anthropic messages = %#v", body.Messages)
		}

		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"id":"anthropic_child_1",
			"type":"message",
			"role":"assistant",
			"model":"anthropic-test-model",
			"content":[{"type":"text","text":"Anthropic specialist answer"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":7,"output_tokens":3}
		}`))
	}))
	defer anthropicServer.Close()

	var openAIRequests atomic.Int32
	openAIServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" {
			t.Errorf("OpenAI path = %q, want /v1/responses", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer openai-test-key" {
			t.Errorf("OpenAI Authorization = %q", got)
		}

		var body struct {
			Model        string            `json:"model"`
			Instructions string            `json:"instructions"`
			Input        []json.RawMessage `json:"input"`
			Tools        []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode OpenAI request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Model != "openai-test-model" || body.Instructions != "Coordinate with the specialist" {
			t.Errorf("OpenAI model/instructions = %q/%q", body.Model, body.Instructions)
		}
		if len(body.Tools) != 1 || body.Tools[0].Name != "consult_anthropic" {
			t.Errorf("OpenAI tools = %#v", body.Tools)
		}

		writer.Header().Set("Content-Type", "application/json")
		switch openAIRequests.Add(1) {
		case 1:
			_, _ = writer.Write([]byte(`{
				"id":"openai_parent_1",
				"model":"openai-test-model",
				"status":"completed",
				"output":[{
					"type":"function_call",
					"id":"function_1",
					"call_id":"call_1",
					"name":"consult_anthropic",
					"arguments":"{\"prompt\":\"Review protocol boundary\"}",
					"status":"completed"
				}],
				"usage":{"input_tokens":8,"output_tokens":4,"total_tokens":12}
			}`))
		case 2:
			var toolOutput string
			for _, rawItem := range body.Input {
				var item struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
					Output string `json:"output"`
				}
				if err := json.Unmarshal(rawItem, &item); err != nil {
					t.Errorf("decode OpenAI input item: %v", err)
					continue
				}
				if item.Type == "function_call_output" && item.CallID == "call_1" {
					toolOutput = item.Output
				}
			}
			if !strings.Contains(toolOutput, `"agent_id":"anthropic-specialist"`) ||
				!strings.Contains(toolOutput, `"output":"Anthropic specialist answer"`) {
				t.Errorf("OpenAI function output = %q", toolOutput)
			}
			_, _ = writer.Write([]byte(`{
				"id":"openai_parent_2",
				"model":"openai-test-model",
				"status":"completed",
				"output":[{
					"type":"message",
					"id":"message_1",
					"role":"assistant",
					"status":"completed",
					"content":[{"type":"output_text","text":"OpenAI final answer","annotations":[]}]
				}],
				"usage":{"input_tokens":20,"output_tokens":3,"total_tokens":23}
			}`))
		default:
			t.Errorf("unexpected OpenAI request count")
			writer.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer openAIServer.Close()

	anthropicProvider, err := anthropic.New(anthropic.Config{
		APIKey:          "anthropic-test-key",
		BaseURL:         anthropicServer.URL + "/v1",
		Model:           "anthropic-test-model",
		MaxOutputTokens: 128,
		HTTPClient:      anthropicServer.Client(),
	})
	if err != nil {
		t.Fatalf("anthropic.New() error = %v", err)
	}
	anthropicRuntime := newTestRuntime(t, anthropicProvider, nil)
	registry := newTestRegistry(t, orchestration.AgentRegistryOptions{
		Agents: []orchestration.AgentDefinition{
			{ID: "anthropic-specialist", Runtime: anthropicRuntime, Instructions: "Analyze the delegated request"},
		},
	})
	tool, err := orchestration.NewSubagentTool(orchestration.SubagentToolOptions{
		Name:         "consult_anthropic",
		Registry:     registry,
		Strategy:     orchestration.TeamStrategyDelegate,
		AgentIDs:     []string{"anthropic-specialist"},
		Instructions: "Be concise",
	})
	if err != nil {
		t.Fatalf("NewSubagentTool() error = %v", err)
	}

	openAIProvider, err := openai.New(openai.Config{
		API:        openai.APIResponses,
		APIKey:     "openai-test-key",
		BaseURL:    openAIServer.URL + "/v1",
		Model:      "openai-test-model",
		HTTPClient: openAIServer.Client(),
	})
	if err != nil {
		t.Fatalf("openai.New() error = %v", err)
	}
	openAIRuntime := newTestRuntime(t, openAIProvider, []agent.Tool{tool})
	if err := registry.Register(orchestration.AgentDefinition{
		ID:           "openai-coordinator",
		Runtime:      openAIRuntime,
		Instructions: "Coordinate with the specialist",
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	result, err := registry.Run(context.Background(), agent.RunRequest{
		AgentID: "openai-coordinator",
		Input:   []agent.Message{{Role: agent.RoleUser, Text: "Evaluate the boundary"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := result.Messages[len(result.Messages)-1].Text; got != "OpenAI final answer" {
		t.Errorf("final output = %q", got)
	}
	if openAIRequests.Load() != 2 || anthropicRequests.Load() != 1 {
		t.Errorf("request counts OpenAI/Anthropic = %d/%d, want 2/1", openAIRequests.Load(), anthropicRequests.Load())
	}
}
