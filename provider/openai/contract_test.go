package openai_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/provider/contracttest"
	"github.com/qed-runtime/qed/provider/openai"
)

func TestProviderContract(t *testing.T) {
	tests := []struct {
		name string
		api  openai.API
	}{
		{name: "responses", api: openai.APIResponses},
		{name: "chat_completions", api: openai.APIChatCompletions},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			contracttest.Run(t, contracttest.SuiteOptions{
				NewProvider: func(t *testing.T, scenario contracttest.Scenario) agent.Provider {
					return newOpenAIContractProvider(t, test.api, scenario)
				},
				AssertMessage: func(t *testing.T, scenario contracttest.Scenario, message agent.Message) {
					assertOpenAIContractMessage(t, test.api, scenario, message)
				},
				AssertError: func(t *testing.T, scenario contracttest.Scenario, err error) {
					if scenario == contracttest.ScenarioStreamError {
						want := "OpenAI Responses stream failed"
						if test.api == openai.APIChatCompletions {
							want = "OpenAI Chat Completions stream failed"
						}
						if !strings.Contains(err.Error(), want) {
							t.Errorf("stream error = %q, want %q", err, want)
						}
					}
				},
			})
		})
	}
}

func newOpenAIContractProvider(t *testing.T, api openai.API, scenario contracttest.Scenario) agent.Provider {
	t.Helper()
	client := openAIContractClientFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", request.Header.Get("Accept"))
		}
		if scenario == contracttest.ScenarioCancellation || scenario == contracttest.ScenarioClose {
			return openAIContractBlockingResponse(request.Context()), nil
		}
		status, contentType, body := openAIContractResponse(t, api, scenario)
		response := openAIContractHTTPResponse(status, contentType, body)
		if scenario == contracttest.ScenarioHTTPError {
			response.Header.Set("x-request-id", contracttest.FixtureRequestID)
			response.Header.Set("retry-after", "2")
		}
		return response, nil
	})
	modelProvider, err := openai.New(openai.Config{
		API:        api,
		BaseURL:    "https://contract.invalid/v1",
		Model:      contracttest.FixtureModel,
		HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("openai.New() error = %v", err)
	}
	return modelProvider
}

func openAIContractResponse(
	t *testing.T,
	api openai.API,
	scenario contracttest.Scenario,
) (int, string, string) {
	t.Helper()
	if scenario == contracttest.ScenarioHTTPError {
		return contracttest.FixtureHTTPStatus, "application/json", `{"error":{"type":"rate_limit_error","code":"rate_limit","message":"contract rate limit"}}`
	}
	if api == openai.APIChatCompletions {
		return http.StatusOK, "text/event-stream", openAIChatContractSSE(t, scenario)
	}
	return http.StatusOK, "text/event-stream", openAIResponsesContractSSE(t, scenario)
}

func openAIResponsesContractSSE(t *testing.T, scenario contracttest.Scenario) string {
	t.Helper()
	switch scenario {
	case contracttest.ScenarioText:
		return openAIContractSSE(
			`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"hel"}`,
			`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"lo"}`,
			`event: response.completed
data: {"type":"response.completed","response":{"id":"contract-response","model":"contract-model","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}]}}`,
		)
	case contracttest.ScenarioToolCall:
		return openAIContractSSE(
			`event: response.completed
data: {"type":"response.completed","response":{"id":"contract-response","model":"contract-model","status":"completed","output":[{"type":"function_call","call_id":"contract-call","name":"contract_tool","arguments":"{\"value\":\"hello\"}"}]}}`,
		)
	case contracttest.ScenarioUsage:
		return openAIContractSSE(
			`event: response.completed
data: {"type":"response.completed","response":{"id":"contract-response","model":"contract-model","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":1}}}}`,
		)
	case contracttest.ScenarioStreamError:
		return openAIContractSSE(
			`event: error
data: {"type":"error","code":"contract_stream","message":"contract stream failure"}`,
		)
	default:
		t.Fatalf("unsupported OpenAI Responses contract scenario %q", scenario)
		return ""
	}
}

func openAIChatContractSSE(t *testing.T, scenario contracttest.Scenario) string {
	t.Helper()
	switch scenario {
	case contracttest.ScenarioText:
		return openAIContractSSE(
			`data: {"id":"contract-response","model":"contract-model","choices":[{"index":0,"delta":{"content":"hel"}}]}`,
			`data: {"id":"contract-response","model":"contract-model","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
			`data: {"id":"contract-response","model":"contract-model","choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`,
			`data: [DONE]`,
		)
	case contracttest.ScenarioToolCall:
		return openAIContractSSE(
			`data: {"id":"contract-response","model":"contract-model","choices":[{"index":0,"finish_reason":"tool_calls","delta":{"tool_calls":[{"index":0,"id":"contract-call","type":"function","function":{"name":"contract_tool","arguments":"{\"value\":\"hello\"}"}}]}}]}`,
			`data: [DONE]`,
		)
	case contracttest.ScenarioUsage:
		return openAIContractSSE(
			`data: {"id":"contract-response","model":"contract-model","choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10,"prompt_tokens_details":{"cached_tokens":2,"cache_write_tokens":1}}}`,
			`data: [DONE]`,
		)
	case contracttest.ScenarioStreamError:
		return openAIContractSSE(
			`data: {"error":{"code":"contract_stream","message":"contract stream failure"}}`,
		)
	default:
		t.Fatalf("unsupported OpenAI Chat contract scenario %q", scenario)
		return ""
	}
}

func assertOpenAIContractMessage(
	t *testing.T,
	api openai.API,
	scenario contracttest.Scenario,
	message agent.Message,
) {
	t.Helper()
	if message.ResponseID != contracttest.FixtureResponseID || message.Model != contracttest.FixtureModel {
		t.Errorf("response identity = %q/%q, want %q/%q", message.ResponseID, message.Model, contracttest.FixtureResponseID, contracttest.FixtureModel)
	}
	wantRawStop := "completed"
	if api == openai.APIChatCompletions {
		wantRawStop = "stop"
	}
	if scenario == contracttest.ScenarioToolCall {
		wantRawStop = "tool_use"
		if api == openai.APIChatCompletions {
			wantRawStop = "tool_calls"
		}
	}
	if message.RawStopReason != wantRawStop {
		t.Errorf("RawStopReason = %q, want %q", message.RawStopReason, wantRawStop)
	}
	if api == openai.APIResponses {
		if message.ProviderState == nil || message.ProviderState.Provider == "" {
			t.Errorf("Responses ProviderState = %#v", message.ProviderState)
		}
	} else if message.ProviderState != nil {
		t.Errorf("Chat Completions ProviderState = %#v, want nil", message.ProviderState)
	}
}

func openAIContractSSE(events ...string) string {
	return strings.Join(events, "\n\n") + "\n\n"
}

type openAIContractClientFunc func(*http.Request) (*http.Response, error)

func (client openAIContractClientFunc) Do(request *http.Request) (*http.Response, error) {
	return client(request)
}

func openAIContractHTTPResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func openAIContractBlockingResponse(ctx context.Context) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &openAIContractBlockingBody{
			ctx:    ctx,
			closed: make(chan struct{}),
		},
	}
}

type openAIContractBlockingBody struct {
	ctx       context.Context
	closed    chan struct{}
	closeOnce sync.Once
}

func (body *openAIContractBlockingBody) Read([]byte) (int, error) {
	select {
	case <-body.ctx.Done():
		return 0, body.ctx.Err()
	case <-body.closed:
		return 0, io.EOF
	}
}

func (body *openAIContractBlockingBody) Close() error {
	body.closeOnce.Do(func() {
		close(body.closed)
	})
	return nil
}
