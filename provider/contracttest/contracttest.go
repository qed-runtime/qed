// Package contracttest provides reusable conformance checks for QED Provider
// adapters
package contracttest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
	providerbase "github.com/qed-runtime/qed/provider"
)

const (
	defaultTimeout      = 5 * time.Second
	blockingProbeWindow = 10 * time.Millisecond
	maximumStreamEvents = 1024
)

// Scenario identifies the scripted behavior requested from a Provider factory
type Scenario string

const (
	// ScenarioText requests a successful two-delta text response
	ScenarioText Scenario = "text"
	// ScenarioToolCall requests one successful Tool Call response
	ScenarioToolCall Scenario = "tool_call"
	// ScenarioUsage requests one successful response with normalized Usage
	ScenarioUsage Scenario = "usage"
	// ScenarioHTTPError requests a non-success HTTP response
	ScenarioHTTPError Scenario = "http_error"
	// ScenarioStreamError requests an error after a stream has started
	ScenarioStreamError Scenario = "stream_error"
	// ScenarioCancellation requests a stream that blocks until its Context is canceled
	ScenarioCancellation Scenario = "cancellation"
	// ScenarioClose requests a stream whose blocked Next call is interrupted by Close
	ScenarioClose Scenario = "close"
)

const (
	// FixtureModel is the model identifier reported by successful fixture responses
	FixtureModel = "contract-model"
	// FixtureResponseID is the response identifier reported by successful fixture responses
	FixtureResponseID = "contract-response"
	// FixtureText is the completed text for ScenarioText
	FixtureText = "hello"
	// FixtureToolCallID is the Tool Call identifier for ScenarioToolCall
	FixtureToolCallID = "contract-call"
	// FixtureToolName is the Tool name used by the request and ScenarioToolCall
	FixtureToolName = "contract_tool"
	// FixtureHTTPStatus is the status code for ScenarioHTTPError
	FixtureHTTPStatus = 429
	// FixtureHTTPErrorType is the Provider error type for ScenarioHTTPError
	FixtureHTTPErrorType = "rate_limit_error"
	// FixtureHTTPErrorCode is the Provider error code for ScenarioHTTPError
	FixtureHTTPErrorCode = "rate_limit"
	// FixtureHTTPErrorMessage is the Provider error message for ScenarioHTTPError
	FixtureHTTPErrorMessage = "contract rate limit"
	// FixtureRetryAfterSeconds is the Retry-After delay for ScenarioHTTPError
	FixtureRetryAfterSeconds = 2
	// FixtureRequestID is the request identifier for ScenarioHTTPError
	FixtureRequestID = "contract-request"
	// FixtureStreamErrorCode is the Provider error code for ScenarioStreamError
	FixtureStreamErrorCode = "contract_stream"
	// FixtureStreamErrorMessage is the Provider error message for ScenarioStreamError
	FixtureStreamErrorMessage = "contract stream failure"
)

// ProviderFactory constructs a fresh Provider backed by one scripted Scenario
//
// A factory used with Run must support every Scenario constant. Successful
// responses must use the exported fixture values. Error and blocking scenarios
// must follow their Scenario documentation without using an external service.
type ProviderFactory func(t *testing.T, scenario Scenario) agent.Provider

// SuiteOptions configures the complete streaming HTTP Provider conformance suite
type SuiteOptions struct {
	// NewProvider constructs a fresh Provider for every Scenario
	NewProvider ProviderFactory
	// Timeout bounds each stream operation
	//
	// A zero value uses five seconds
	Timeout time.Duration
	// AssertMessage checks Provider-specific fields after common assertions
	AssertMessage func(t *testing.T, scenario Scenario, message agent.Message)
	// AssertError checks Provider-specific error details after common assertions
	//
	// It is called for ScenarioHTTPError and ScenarioStreamError
	AssertError func(t *testing.T, scenario Scenario, err error)
}

// TextOptions configures the successful text-stream subset used by Providers
// that do not produce model-generated Tool Calls or Usage
type TextOptions struct {
	// Provider is the Provider under test
	Provider agent.Provider
	// Request is sent to Provider.Stream
	Request agent.ModelRequest
	// ExpectedText is the completed assistant text
	ExpectedText string
	// ExpectedDeltas optionally fixes exact delta boundaries
	//
	// A nil slice accepts any non-empty delta boundaries whose concatenation is
	// ExpectedText. A non-nil slice requires an exact match.
	ExpectedDeltas []string
	// Timeout bounds each stream operation
	//
	// A zero value uses five seconds
	Timeout time.Duration
	// AssertMessage checks additional fields after common assertions
	AssertMessage func(t *testing.T, message agent.Message)
}

// Run executes the complete streaming HTTP Provider conformance suite
func Run(t *testing.T, options SuiteOptions) {
	t.Helper()
	timeout := configuredTimeout(t, options.Timeout)
	if options.NewProvider == nil {
		t.Fatal("Provider contract factory is required")
	}

	t.Run("text_stream", func(t *testing.T) {
		modelProvider := newProvider(t, options, ScenarioText)
		RunText(t, TextOptions{
			Provider:       modelProvider,
			Request:        FixtureRequest(),
			ExpectedText:   FixtureText,
			ExpectedDeltas: FixtureTextDeltas(),
			Timeout:        timeout,
			AssertMessage: func(t *testing.T, message agent.Message) {
				assertMessage(t, options, ScenarioText, message)
			},
		})
	})
	t.Run("tool_call", func(t *testing.T) {
		runToolCall(t, options, timeout)
	})
	t.Run("usage", func(t *testing.T) {
		runUsage(t, options, timeout)
	})
	t.Run("http_error", func(t *testing.T) {
		runHTTPError(t, options, timeout)
	})
	t.Run("stream_error", func(t *testing.T) {
		runStreamError(t, options, timeout)
	})
	t.Run("context_cancellation", func(t *testing.T) {
		runCancellation(t, options, timeout)
	})
	t.Run("close_interrupts_next", func(t *testing.T) {
		runClose(t, options, timeout)
	})
}

// RunText verifies successful stream lifecycle, text deltas, terminal Message,
// stable EOF, and Close behavior
func RunText(t *testing.T, options TextOptions) {
	t.Helper()
	timeout := configuredTimeout(t, options.Timeout)
	if options.Provider == nil {
		t.Fatal("Provider is required")
	}
	validateProviderIdentity(t, options.Provider, "text stream")

	result := collectCompleted(t, options.Provider, options.Request, timeout)
	if result.message.Role != agent.RoleAssistant {
		t.Errorf("completed role = %q, want %q", result.message.Role, agent.RoleAssistant)
	}
	if result.message.Text != options.ExpectedText {
		t.Errorf("completed text = %q, want %q", result.message.Text, options.ExpectedText)
	}
	if result.message.StopReason != agent.StopReasonEndTurn {
		t.Errorf("completed stop reason = %q, want %q", result.message.StopReason, agent.StopReasonEndTurn)
	}
	if len(result.message.ToolCalls) != 0 {
		t.Errorf("completed Tool Calls = %#v, want none", result.message.ToolCalls)
	}
	if strings.Join(result.deltas, "") != options.ExpectedText {
		t.Errorf("joined deltas = %q, want %q", strings.Join(result.deltas, ""), options.ExpectedText)
	}
	if options.ExpectedDeltas != nil && !reflect.DeepEqual(result.deltas, options.ExpectedDeltas) {
		t.Errorf("deltas = %#v, want %#v", result.deltas, options.ExpectedDeltas)
	}
	if options.AssertMessage != nil {
		options.AssertMessage(t, result.message)
	}
}

// FixtureRequest returns a fresh provider-neutral request used by Run
func FixtureRequest() agent.ModelRequest {
	return agent.ModelRequest{
		AgentID:      "contract-agent",
		SessionID:    "contract-session",
		Metadata:     map[string]string{"contract": "provider"},
		Instructions: "Follow the Provider contract fixture",
		Messages: []agent.Message{{
			Role: agent.RoleUser,
			Text: "Run the Provider contract fixture",
		}},
		Tools: []agent.ToolDefinition{{
			Name:        FixtureToolName,
			Description: "Return the supplied value",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`),
		}},
	}
}

// FixtureTextDeltas returns fresh delta boundaries for ScenarioText
func FixtureTextDeltas() []string {
	return []string{"hel", "lo"}
}

// FixtureToolArguments returns fresh Tool Call arguments for ScenarioToolCall
func FixtureToolArguments() json.RawMessage {
	return json.RawMessage(`{"value":"hello"}`)
}

// FixtureUsage returns fresh normalized Usage for ScenarioUsage
func FixtureUsage() agent.Usage {
	return agent.Usage{
		InputTokens:               7,
		OutputTokens:              3,
		TotalTokens:               10,
		InputTokenDetailsReported: true,
		UncachedInputTokens:       4,
		CacheReadInputTokens:      2,
		CacheWriteInputTokens:     1,
	}
}

type streamResult struct {
	deltas  []string
	message agent.Message
}

type nextResult struct {
	event agent.ModelStreamEvent
	err   error
}

func newProvider(t *testing.T, options SuiteOptions, scenario Scenario) agent.Provider {
	t.Helper()
	modelProvider := options.NewProvider(t, scenario)
	if modelProvider == nil {
		t.Fatalf("Provider factory returned nil for %q", scenario)
	}
	validateProviderIdentity(t, modelProvider, string(scenario))
	return modelProvider
}

func validateProviderIdentity(t *testing.T, modelProvider agent.Provider, scope string) {
	t.Helper()
	name := modelProvider.Name()
	if strings.TrimSpace(name) == "" {
		t.Fatalf("Provider name is empty for %q", scope)
	}
	if repeated := modelProvider.Name(); repeated != name {
		t.Fatalf("Provider name changed from %q to %q for %q", name, repeated, scope)
	}
}

func collectCompleted(
	t *testing.T,
	modelProvider agent.Provider,
	request agent.ModelRequest,
	timeout time.Duration,
) streamResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stream, err := modelProvider.Stream(ctx, request)
	if err != nil {
		t.Fatalf("Provider.Stream() error = %v", err)
	}
	if stream == nil {
		t.Fatal("Provider.Stream() returned a nil stream")
	}
	closed := false
	defer func() {
		if !closed {
			_ = stream.Close()
		}
	}()

	var result streamResult
	completed := false
	for eventCount := 0; eventCount < maximumStreamEvents; eventCount++ {
		event, nextErr := nextWithin(t, stream, timeout)
		if errors.Is(nextErr, io.EOF) {
			if !completed {
				t.Fatal("Provider stream reached EOF without a completed Message")
			}
			_, repeatedErr := nextWithin(t, stream, timeout)
			if !errors.Is(repeatedErr, io.EOF) {
				t.Fatalf("Provider stream error after EOF = %v, want io.EOF", repeatedErr)
			}
			if err := stream.Close(); err != nil {
				t.Fatalf("Provider stream Close() error = %v", err)
			}
			closed = true
			return result
		}
		if nextErr != nil {
			t.Fatalf("Provider stream Next() error = %v", nextErr)
		}
		if completed {
			t.Fatalf("Provider stream emitted %q after its completed Message", event.Type)
		}
		switch event.Type {
		case agent.ModelStreamTextDelta:
			if event.TextDelta == "" {
				t.Fatal("Provider stream emitted an empty text delta")
			}
			if event.Message != nil {
				t.Fatal("text delta event contains a Message")
			}
			result.deltas = append(result.deltas, event.TextDelta)
		case agent.ModelStreamMessageComplete:
			if event.Message == nil {
				t.Fatal("completed event has no Message")
			}
			if event.TextDelta != "" {
				t.Fatal("completed event contains a text delta")
			}
			result.message = *event.Message
			completed = true
		default:
			t.Fatalf("Provider stream event type = %q, want a supported type", event.Type)
		}
	}
	t.Fatalf("Provider stream exceeded %d events", maximumStreamEvents)
	return streamResult{}
}

func nextWithin(t *testing.T, stream agent.ModelStream, timeout time.Duration) (agent.ModelStreamEvent, error) {
	t.Helper()
	result := make(chan nextResult, 1)
	go func() {
		event, err := stream.Next()
		result <- nextResult{event: event, err: err}
	}()
	select {
	case received := <-result:
		return received.event, received.err
	case <-time.After(timeout):
		_ = stream.Close()
		t.Fatalf("Provider stream Next() did not return within %s", timeout)
		return agent.ModelStreamEvent{}, context.DeadlineExceeded
	}
}

func runToolCall(t *testing.T, options SuiteOptions, timeout time.Duration) {
	t.Helper()
	result := collectCompleted(t, newProvider(t, options, ScenarioToolCall), FixtureRequest(), timeout)
	message := result.message
	if message.Role != agent.RoleAssistant || message.Text != "" || message.StopReason != agent.StopReasonToolUse {
		t.Errorf("completed Tool Call message = %#v", message)
	}
	if len(result.deltas) != 0 {
		t.Errorf("Tool Call deltas = %#v, want none", result.deltas)
	}
	if len(message.ToolCalls) != 1 {
		t.Fatalf("Tool Call count = %d, want 1", len(message.ToolCalls))
	}
	call := message.ToolCalls[0]
	if call.ID != FixtureToolCallID || call.Name != FixtureToolName {
		t.Errorf("Tool Call identity = %q/%q, want %q/%q", call.ID, call.Name, FixtureToolCallID, FixtureToolName)
	}
	if !jsonEqual(call.Arguments, FixtureToolArguments()) {
		t.Errorf("Tool Call arguments = %s, want %s", call.Arguments, FixtureToolArguments())
	}
	assertMessage(t, options, ScenarioToolCall, message)
}

func runUsage(t *testing.T, options SuiteOptions, timeout time.Duration) {
	t.Helper()
	result := collectCompleted(t, newProvider(t, options, ScenarioUsage), FixtureRequest(), timeout)
	message := result.message
	if message.Role != agent.RoleAssistant || message.Text != "" || message.StopReason != agent.StopReasonEndTurn ||
		len(message.ToolCalls) != 0 {
		t.Errorf("completed Usage message = %#v", message)
	}
	if len(result.deltas) != 0 {
		t.Errorf("Usage deltas = %#v, want none", result.deltas)
	}
	if message.Usage == nil {
		t.Fatal("completed Message has no Usage")
	}
	want := FixtureUsage()
	if !reflect.DeepEqual(*message.Usage, want) {
		t.Errorf("Usage = %#v, want %#v", *message.Usage, want)
	}
	assertMessage(t, options, ScenarioUsage, message)
}

func runHTTPError(t *testing.T, options SuiteOptions, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stream, err := newProvider(t, options, ScenarioHTTPError).Stream(ctx, FixtureRequest())
	if stream != nil {
		_ = stream.Close()
		t.Error("Provider.Stream() returned a stream with an HTTP error")
	}
	if err == nil {
		t.Fatal("Provider.Stream() returned no HTTP error")
	}
	var apiError *providerbase.HTTPError
	if !errors.As(err, &apiError) {
		t.Fatalf("Provider.Stream() error type = %T, want *provider.HTTPError", err)
	}
	if apiError.StatusCode != FixtureHTTPStatus || apiError.Type != FixtureHTTPErrorType ||
		apiError.Code != FixtureHTTPErrorCode || apiError.Message != FixtureHTTPErrorMessage ||
		apiError.RequestID != FixtureRequestID || apiError.RetryAfter != FixtureRetryAfterSeconds*time.Second {
		t.Errorf("HTTP error = %#v", apiError)
	}
	if info := providerbase.ClassifyError(err); info.Code != providerbase.ErrorCodeRateLimited ||
		info.RetryAfter != FixtureRetryAfterSeconds*time.Second {
		t.Errorf("classified HTTP error = %#v", info)
	}
	assertError(t, options, ScenarioHTTPError, err)
}

func runStreamError(t *testing.T, options SuiteOptions, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stream, err := newProvider(t, options, ScenarioStreamError).Stream(ctx, FixtureRequest())
	if err != nil {
		t.Fatalf("Provider.Stream() startup error = %v", err)
	}
	if stream == nil {
		t.Fatal("Provider.Stream() returned a nil stream")
	}
	defer stream.Close()
	_, err = nextWithin(t, stream, timeout)
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("Provider stream error = %v, want a non-EOF error", err)
	}
	if !strings.Contains(err.Error(), FixtureStreamErrorMessage) {
		t.Errorf("Provider stream error = %q, want message %q", err, FixtureStreamErrorMessage)
	}
	if info := providerbase.ClassifyError(err); info.Code != providerbase.ErrorCodeTerminal {
		t.Errorf("classified stream error = %#v, want terminal", info)
	}
	assertError(t, options, ScenarioStreamError, err)
}

func runCancellation(t *testing.T, options SuiteOptions, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := newProvider(t, options, ScenarioCancellation).Stream(ctx, FixtureRequest())
	if err != nil {
		cancel()
		t.Fatalf("Provider.Stream() startup error = %v", err)
	}
	if stream == nil {
		cancel()
		t.Fatal("Provider.Stream() returned a nil stream")
	}
	defer stream.Close()
	result := asyncNext(stream)
	assertBlocked(t, result)
	cancel()
	received := awaitNext(t, result, timeout)
	if !errors.Is(received.err, context.Canceled) {
		t.Fatalf("Provider stream cancellation error = %v, want context.Canceled", received.err)
	}
}

func runClose(t *testing.T, options SuiteOptions, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stream, err := newProvider(t, options, ScenarioClose).Stream(ctx, FixtureRequest())
	if err != nil {
		t.Fatalf("Provider.Stream() startup error = %v", err)
	}
	if stream == nil {
		t.Fatal("Provider.Stream() returned a nil stream")
	}
	result := asyncNext(stream)
	assertBlocked(t, result)
	if err := stream.Close(); err != nil {
		t.Fatalf("Provider stream Close() error = %v", err)
	}
	received := awaitNext(t, result, timeout)
	if received.err == nil {
		t.Fatalf("blocked Next() returned event %#v without an error after Close", received.event)
	}
}

func asyncNext(stream agent.ModelStream) <-chan nextResult {
	result := make(chan nextResult, 1)
	go func() {
		event, err := stream.Next()
		result <- nextResult{event: event, err: err}
	}()
	return result
}

func assertBlocked(t *testing.T, result <-chan nextResult) {
	t.Helper()
	timer := time.NewTimer(blockingProbeWindow)
	defer timer.Stop()
	select {
	case received := <-result:
		t.Fatalf("blocking fixture returned early with event %#v and error %v", received.event, received.err)
	case <-timer.C:
	}
}

func awaitNext(t *testing.T, result <-chan nextResult, timeout time.Duration) nextResult {
	t.Helper()
	select {
	case received := <-result:
		return received
	case <-time.After(timeout):
		t.Fatalf("blocked Provider stream Next() did not return within %s", timeout)
		return nextResult{err: context.DeadlineExceeded}
	}
}

func assertMessage(t *testing.T, options SuiteOptions, scenario Scenario, message agent.Message) {
	t.Helper()
	if options.AssertMessage != nil {
		options.AssertMessage(t, scenario, message)
	}
}

func assertError(t *testing.T, options SuiteOptions, scenario Scenario, err error) {
	t.Helper()
	if options.AssertError != nil {
		options.AssertError(t, scenario, err)
	}
}

func configuredTimeout(t *testing.T, timeout time.Duration) time.Duration {
	t.Helper()
	if timeout < 0 {
		t.Fatal("Provider contract timeout must not be negative")
	}
	if timeout == 0 {
		return defaultTimeout
	}
	return timeout
}

func jsonEqual(left, right json.RawMessage) bool {
	var leftValue any
	var rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil &&
		reflect.DeepEqual(leftValue, rightValue)
}
