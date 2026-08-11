package openaicodex

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/provider/contracttest"
)

func TestProviderContract(t *testing.T) {
	t.Parallel()
	contracttest.Run(t, contracttest.SuiteOptions{
		NewProvider: newCodexContractProvider,
		AssertMessage: func(t *testing.T, scenario contracttest.Scenario, message agent.Message) {
			if message.ResponseID != contracttest.FixtureResponseID || message.Model != contracttest.FixtureModel {
				t.Errorf("response identity = %q/%q, want %q/%q", message.ResponseID, message.Model, contracttest.FixtureResponseID, contracttest.FixtureModel)
			}
			wantRawStop := "completed"
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
			if scenario == contracttest.ScenarioStreamError && !strings.Contains(err.Error(), "OpenAI Codex Responses stream failed") {
				t.Errorf("stream error = %q, want Codex stream context", err)
			}
		},
	})
}

func newCodexContractProvider(t *testing.T, scenario contracttest.Scenario) agent.Provider {
	t.Helper()
	client := httpClientFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", request.Header.Get("Accept"))
		}
		if scenario == contracttest.ScenarioCancellation || scenario == contracttest.ScenarioClose {
			return codexContractBlockingResponse(request.Context()), nil
		}
		status, contentType, body := codexContractResponse(t, scenario)
		response := httpResponse(status, contentType, body)
		if scenario == contracttest.ScenarioHTTPError {
			response.Header.Set("x-request-id", contracttest.FixtureRequestID)
			response.Header.Set("retry-after", "2")
		}
		return response, nil
	})
	modelProvider, err := New(Config{
		AuthorizationSource: staticAuthorization(),
		Model:               contracttest.FixtureModel,
		HTTPClient:          client,
		endpoint:            "https://contract.invalid/responses",
	})
	if err != nil {
		t.Fatalf("openaicodex.New() error = %v", err)
	}
	return modelProvider
}

func codexContractResponse(t *testing.T, scenario contracttest.Scenario) (int, string, string) {
	t.Helper()
	if scenario == contracttest.ScenarioHTTPError {
		return contracttest.FixtureHTTPStatus, "application/json", `{"error":{"type":"rate_limit_error","code":"rate_limit","message":"contract rate limit"}}`
	}
	return http.StatusOK, "text/event-stream", codexContractSSE(t, scenario)
}

func codexContractSSE(t *testing.T, scenario contracttest.Scenario) string {
	t.Helper()
	switch scenario {
	case contracttest.ScenarioText:
		return joinCodexContractSSE(
			`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"hel"}`,
			`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"lo"}`,
			`event: response.done
data: {"type":"response.done","response":{"id":"contract-response","model":"contract-model","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}]}}`,
		)
	case contracttest.ScenarioToolCall:
		return joinCodexContractSSE(
			`event: response.done
data: {"type":"response.done","response":{"id":"contract-response","model":"contract-model","status":"completed","output":[{"type":"function_call","call_id":"contract-call","name":"contract_tool","arguments":"{\"value\":\"hello\"}"}]}}`,
		)
	case contracttest.ScenarioUsage:
		return joinCodexContractSSE(
			`event: response.done
data: {"type":"response.done","response":{"id":"contract-response","model":"contract-model","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":1}}}}`,
		)
	case contracttest.ScenarioStreamError:
		return joinCodexContractSSE(
			`event: error
data: {"type":"error","code":"contract_stream","message":"contract stream failure"}`,
		)
	default:
		t.Fatalf("unsupported OpenAI Codex contract scenario %q", scenario)
		return ""
	}
}

func joinCodexContractSSE(events ...string) string {
	return strings.Join(events, "\n\n") + "\n\n"
}

func codexContractBlockingResponse(ctx context.Context) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &codexContractBlockingBody{
			ctx:    ctx,
			closed: make(chan struct{}),
		},
	}
}

type codexContractBlockingBody struct {
	ctx       context.Context
	closed    chan struct{}
	closeOnce sync.Once
}

func (body *codexContractBlockingBody) Read([]byte) (int, error) {
	select {
	case <-body.ctx.Done():
		return 0, body.ctx.Err()
	case <-body.closed:
		return 0, io.EOF
	}
}

func (body *codexContractBlockingBody) Close() error {
	body.closeOnce.Do(func() {
		close(body.closed)
	})
	return nil
}
