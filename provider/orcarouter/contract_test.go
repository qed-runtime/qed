package orcarouter_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/provider/contracttest"
	"github.com/qed-runtime/qed/provider/orcarouter"
)

const contractResolvedModel = "resolved-contract-model"

func TestProviderContract(t *testing.T) {
	tests := []struct {
		name string
		api  orcarouter.API
	}{
		{name: "responses", api: orcarouter.APIResponses},
		{name: "chat_completions", api: orcarouter.APIChatCompletions},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			contracttest.Run(t, contracttest.SuiteOptions{
				NewProvider: func(t *testing.T, scenario contracttest.Scenario) agent.Provider {
					return newContractProvider(t, test.api, scenario)
				},
				AssertMessage: func(t *testing.T, _ contracttest.Scenario, message agent.Message) {
					if message.ResponseID != contracttest.FixtureResponseID ||
						message.RequestID != contracttest.FixtureRequestID || message.Model != contractResolvedModel {
						t.Errorf("response identity = %#v", message)
					}
					if test.api == orcarouter.APIResponses {
						if message.ProviderState == nil || !strings.HasPrefix(message.ProviderState.Provider, "orcarouter/responses") {
							t.Errorf("Responses ProviderState = %#v", message.ProviderState)
						}
					} else if message.ProviderState != nil {
						t.Errorf("Chat Completions ProviderState = %#v, want nil", message.ProviderState)
					}
				},
			})
		})
	}
}

func newContractProvider(t *testing.T, api orcarouter.API, scenario contracttest.Scenario) agent.Provider {
	t.Helper()
	client := contractClientFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", request.Header.Get("Accept"))
		}
		if request.Header.Get("X-OrcaRouter-Include-Cost") != "true" {
			t.Errorf("cost header = %q, want true", request.Header.Get("X-OrcaRouter-Include-Cost"))
		}
		affinity := request.Header.Get("X-OrcaRouter-Session-Id")
		if !strings.HasPrefix(affinity, "qed-") || strings.Contains(affinity, "contract-session") {
			t.Errorf("affinity header = %q", affinity)
		}
		if scenario == contracttest.ScenarioCancellation || scenario == contracttest.ScenarioClose {
			return blockingResponse(request.Context()), nil
		}
		status, body := contractResponse(t, api, scenario)
		response := httpResponse(status, "text/event-stream", body)
		response.Header.Set("X-Orca-Request-Id", contracttest.FixtureRequestID)
		response.Header.Set("X-Orca-Resolved-Model", contractResolvedModel)
		if scenario == contracttest.ScenarioHTTPError {
			response.Header.Set("Retry-After", "2")
		}
		return response, nil
	})
	modelProvider, err := orcarouter.New(orcarouter.Config{
		ProfileID:  "contract",
		API:        api,
		APIKey:     "contract-key",
		BaseURL:    "https://contract.invalid/v1",
		Model:      contracttest.FixtureModel,
		HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("orcarouter.New() error = %v", err)
	}
	return modelProvider
}

func contractResponse(t *testing.T, api orcarouter.API, scenario contracttest.Scenario) (int, string) {
	t.Helper()
	if scenario == contracttest.ScenarioHTTPError {
		return contracttest.FixtureHTTPStatus, `{"error":{"type":"rate_limit_error","code":"rate_limit","message":"contract rate limit"}}`
	}
	if api == orcarouter.APIChatCompletions {
		return http.StatusOK, chatContractSSE(t, scenario)
	}
	return http.StatusOK, responsesContractSSE(t, scenario)
}

func responsesContractSSE(t *testing.T, scenario contracttest.Scenario) string {
	t.Helper()
	switch scenario {
	case contracttest.ScenarioText:
		return contractSSE(
			eventData("response.output_text.delta", `{"type":"response.output_text.delta","delta":"hel"}`),
			eventData("response.output_text.delta", `{"type":"response.output_text.delta","delta":"lo"}`),
			eventData("response.completed", `{"type":"response.completed","response":{"id":"contract-response","model":"contract-model","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}]}}`),
		)
	case contracttest.ScenarioToolCall:
		return contractSSE(eventData("response.completed", `{"type":"response.completed","response":{"id":"contract-response","model":"contract-model","status":"completed","output":[{"type":"function_call","call_id":"contract-call","name":"contract_tool","arguments":"{\"value\":\"hello\"}"}]}}`))
	case contracttest.ScenarioUsage:
		return contractSSE(eventData("response.completed", `{"type":"response.completed","response":{"id":"contract-response","model":"contract-model","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":1}}}}`))
	case contracttest.ScenarioStreamError:
		return contractSSE(eventData("error", `{"type":"error","code":"contract_stream","message":"contract stream failure"}`))
	default:
		t.Fatalf("unsupported Responses contract scenario %q", scenario)
		return ""
	}
}

func chatContractSSE(t *testing.T, scenario contracttest.Scenario) string {
	t.Helper()
	switch scenario {
	case contracttest.ScenarioText:
		return contractSSE(
			eventData("", `{"id":"contract-response","model":"contract-model","choices":[{"index":0,"delta":{"content":"hel"}}]}`),
			eventData("", `{"id":"contract-response","model":"contract-model","choices":[{"index":0,"delta":{"content":"lo"}}]}`),
			eventData("", `{"id":"contract-response","model":"contract-model","choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`),
			eventData("", `[DONE]`),
		)
	case contracttest.ScenarioToolCall:
		return contractSSE(
			eventData("", `{"id":"contract-response","model":"contract-model","choices":[{"index":0,"finish_reason":"tool_calls","delta":{"tool_calls":[{"index":0,"id":"contract-call","type":"function","function":{"name":"contract_tool","arguments":"{\"value\":\"hello\"}"}}]}}]}`),
			eventData("", `[DONE]`),
		)
	case contracttest.ScenarioUsage:
		return contractSSE(
			eventData("", `{"id":"contract-response","model":"contract-model","choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`),
			eventData("", `{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10,"prompt_tokens_details":{"cached_tokens":2,"cache_write_tokens":1}}}`),
			eventData("", `[DONE]`),
		)
	case contracttest.ScenarioStreamError:
		return contractSSE(eventData("", `{"error":{"code":"contract_stream","message":"contract stream failure"}}`))
	default:
		t.Fatalf("unsupported Chat contract scenario %q", scenario)
		return ""
	}
}

func eventData(event, data string) string {
	if event == "" {
		return "data: " + data
	}
	return "event: " + event + "\n" + "data: " + data
}

func contractSSE(events ...string) string {
	return strings.Join(events, "\n\n") + "\n\n"
}

type contractClientFunc func(*http.Request) (*http.Response, error)

func (client contractClientFunc) Do(request *http.Request) (*http.Response, error) {
	return client(request)
}

func httpResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func blockingResponse(ctx context.Context) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &blockingBody{ctx: ctx, closed: make(chan struct{})},
	}
}

type blockingBody struct {
	ctx       context.Context
	closed    chan struct{}
	closeOnce sync.Once
}

func (body *blockingBody) Read([]byte) (int, error) {
	select {
	case <-body.ctx.Done():
		return 0, body.ctx.Err()
	case <-body.closed:
		return 0, io.EOF
	}
}

func (body *blockingBody) Close() error {
	body.closeOnce.Do(func() { close(body.closed) })
	return nil
}
