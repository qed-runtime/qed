package anthropic_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/provider/anthropic"
	"github.com/qed-runtime/qed/provider/contracttest"
)

func TestProviderContract(t *testing.T) {
	t.Parallel()
	contracttest.Run(t, contracttest.SuiteOptions{
		NewProvider: newAnthropicContractProvider,
		AssertMessage: func(t *testing.T, scenario contracttest.Scenario, message agent.Message) {
			if message.ResponseID != contracttest.FixtureResponseID || message.Model != contracttest.FixtureModel {
				t.Errorf("response identity = %q/%q, want %q/%q", message.ResponseID, message.Model, contracttest.FixtureResponseID, contracttest.FixtureModel)
			}
			wantRawStop := "end_turn"
			if scenario == contracttest.ScenarioToolCall {
				wantRawStop = "tool_use"
			}
			if message.RawStopReason != wantRawStop {
				t.Errorf("RawStopReason = %q, want %q", message.RawStopReason, wantRawStop)
			}
			if message.ProviderState == nil || message.ProviderState.Provider == "" {
				t.Errorf("ProviderState = %#v", message.ProviderState)
			}
		},
		AssertError: func(t *testing.T, scenario contracttest.Scenario, err error) {
			if scenario == contracttest.ScenarioStreamError && !strings.Contains(err.Error(), "Anthropic Messages stream failed") {
				t.Errorf("stream error = %q, want Anthropic stream context", err)
			}
		},
	})
}

func newAnthropicContractProvider(t *testing.T, scenario contracttest.Scenario) agent.Provider {
	t.Helper()
	client := anthropicContractClientFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", request.Header.Get("Accept"))
		}
		if scenario == contracttest.ScenarioCancellation || scenario == contracttest.ScenarioClose {
			return anthropicContractBlockingResponse(request.Context()), nil
		}
		status, contentType, body := anthropicContractResponse(t, scenario)
		response := anthropicContractHTTPResponse(status, contentType, body)
		if scenario == contracttest.ScenarioHTTPError {
			response.Header.Set("request-id", contracttest.FixtureRequestID)
		}
		return response, nil
	})
	modelProvider, err := anthropic.New(anthropic.Config{
		BaseURL:    "https://contract.invalid/v1",
		Model:      contracttest.FixtureModel,
		HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("anthropic.New() error = %v", err)
	}
	return modelProvider
}

func anthropicContractResponse(t *testing.T, scenario contracttest.Scenario) (int, string, string) {
	t.Helper()
	if scenario == contracttest.ScenarioHTTPError {
		return contracttest.FixtureHTTPStatus, "application/json", `{"error":{"type":"rate_limit_error","code":"rate_limit","message":"contract rate limit"}}`
	}
	return http.StatusOK, "text/event-stream", anthropicContractSSE(t, scenario)
}

func anthropicContractSSE(t *testing.T, scenario contracttest.Scenario) string {
	t.Helper()
	switch scenario {
	case contracttest.ScenarioText:
		return joinAnthropicContractSSE(
			`event: message_start
data: {"type":"message_start","message":{"id":"contract-response","role":"assistant","model":"contract-model","content":[],"usage":{}}}`,
			`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
			`event: content_block_stop
data: {"type":"content_block_stop","index":0}`,
			`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{}}`,
			`event: message_stop
data: {"type":"message_stop"}`,
		)
	case contracttest.ScenarioToolCall:
		return joinAnthropicContractSSE(
			`event: message_start
data: {"type":"message_start","message":{"id":"contract-response","role":"assistant","model":"contract-model","content":[],"usage":{}}}`,
			`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"contract-call","name":"contract_tool","input":{}}}`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"value\":\"hello\"}"}}`,
			`event: content_block_stop
data: {"type":"content_block_stop","index":0}`,
			`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{}}`,
			`event: message_stop
data: {"type":"message_stop"}`,
		)
	case contracttest.ScenarioUsage:
		return joinAnthropicContractSSE(
			`event: message_start
data: {"type":"message_start","message":{"id":"contract-response","role":"assistant","model":"contract-model","content":[],"usage":{"input_tokens":4,"cache_creation_input_tokens":1,"cache_read_input_tokens":2}}}`,
			`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			`event: message_stop
data: {"type":"message_stop"}`,
		)
	case contracttest.ScenarioStreamError:
		return joinAnthropicContractSSE(
			`event: error
data: {"type":"error","error":{"type":"contract_stream","message":"contract stream failure"}}`,
		)
	default:
		t.Fatalf("unsupported Anthropic contract scenario %q", scenario)
		return ""
	}
}

func joinAnthropicContractSSE(events ...string) string {
	return strings.Join(events, "\n\n") + "\n\n"
}

type anthropicContractClientFunc func(*http.Request) (*http.Response, error)

func (client anthropicContractClientFunc) Do(request *http.Request) (*http.Response, error) {
	return client(request)
}

func anthropicContractHTTPResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func anthropicContractBlockingResponse(ctx context.Context) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &anthropicContractBlockingBody{
			ctx:    ctx,
			closed: make(chan struct{}),
		},
	}
}

type anthropicContractBlockingBody struct {
	ctx       context.Context
	closed    chan struct{}
	closeOnce sync.Once
}

func (body *anthropicContractBlockingBody) Read([]byte) (int, error) {
	select {
	case <-body.ctx.Done():
		return 0, body.ctx.Err()
	case <-body.closed:
		return 0, io.EOF
	}
}

func (body *anthropicContractBlockingBody) Close() error {
	body.closeOnce.Do(func() {
		close(body.closed)
	})
	return nil
}
