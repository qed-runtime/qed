package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/evidence"
	"github.com/qed-runtime/qed/session"
)

func TestRuntimeCompletesWithoutTools(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{
		responses: []providerResponse{
			{message: agent.Message{Role: agent.RoleAssistant, Text: "done"}},
		},
	}
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}

	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	events, result, runErr := collectRun(handle)
	if runErr != nil {
		t.Fatalf("Wait() error = %v", runErr)
	}
	if result.Status != agent.RunStatusCompleted {
		t.Fatalf("Status = %q, want %q", result.Status, agent.RunStatusCompleted)
	}
	if result.ProviderCalls != 1 {
		t.Errorf("ProviderCalls = %d, want 1", result.ProviderCalls)
	}
	if got := result.Messages[len(result.Messages)-1].Text; got != "done" {
		t.Errorf("last message = %q, want %q", got, "done")
	}
	secondResult, secondErr := handle.Wait()
	if secondErr != nil || secondResult.RunID != result.RunID {
		t.Errorf("second Wait() = (%#v, %v), want RunID %q", secondResult, secondErr, result.RunID)
	}

	wantEvents := []agent.EventType{
		agent.EventRunStarted,
		agent.EventUserMessageAdded,
		agent.EventModelRequest,
		agent.EventMessageStarted,
		agent.EventMessageDelta,
		agent.EventMessageCompleted,
		agent.EventRunCompleted,
	}
	assertEventTypes(t, events, wantEvents)
}

func TestRuntimePinsAndInvokesRunComponentHooks(t *testing.T) {
	t.Parallel()

	hook := &recordingHook{}
	source := &componentSource{hook: hook}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: &scriptedProvider{responses: []providerResponse{{
			message: agent.Message{Role: agent.RoleAssistant, Text: "done"},
		}}},
		ComponentSource: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := collectRun(handle)
	if err != nil || result.Status != agent.RunStatusCompleted {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	if got := hook.types(); len(got) != 2 || got[0] != agent.EventRunStarted || got[1] != agent.EventRunCompleted {
		t.Fatalf("Hook Events = %#v", got)
	}
	if source.releases != 1 {
		t.Fatalf("component releases = %d, want 1", source.releases)
	}
}

func TestRuntimePersistsFailedTerminalEventWhenCompletedHookFails(t *testing.T) {
	t.Parallel()

	store := session.NewMemoryStore()
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: &scriptedProvider{responses: []providerResponse{{
			message: agent.Message{Role: agent.RoleAssistant, Text: "done"},
		}}},
		SessionStore: store,
		Hooks:        []agent.Hook{failingCompletedHook{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		SessionID: "hook-failure",
		Input:     []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, result, runErr := collectRun(handle)
	if runErr == nil || result.Status != agent.RunStatusFailed {
		t.Fatalf("Run = %#v, %v", result, runErr)
	}
	if got := events[len(events)-1].Type; got != agent.EventRunFailed {
		t.Fatalf("last published Event = %q, want %q", got, agent.EventRunFailed)
	}
	for _, event := range events {
		if event.Type == agent.EventRunCompleted {
			t.Fatalf("published Events contain rejected terminal Event: %#v", events)
		}
	}
	snapshot, err := store.Snapshot(context.Background(), "hook-failure")
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Events[len(snapshot.Events)-1].Type; got != agent.EventRunFailed {
		t.Fatalf("last persisted Event = %q, want %q", got, agent.EventRunFailed)
	}
	for _, event := range snapshot.Events {
		if event.Type == agent.EventRunCompleted {
			t.Fatalf("persisted Events contain rejected terminal Event: %#v", snapshot.Events)
		}
	}
}

func TestRuntimeDebugDiagnosticsExcludeMessageContentAndMetadataValues(t *testing.T) {
	t.Parallel()

	var diagnostics bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&diagnostics, &slog.HandlerOptions{Level: slog.LevelDebug}))
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: &scriptedProvider{responses: []providerResponse{{
			message: agent.Message{Role: agent.RoleAssistant, Text: "do-not-log-output"},
		}}},
		Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Metadata: map[string]string{"secret": "do-not-log-metadata"},
		Input:    []agent.Message{{Role: agent.RoleUser, Text: "do-not-log-input"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, runErr := collectRun(handle)
	if runErr != nil {
		t.Fatal(runErr)
	}
	output := diagnostics.String()
	for _, expected := range []string{"run.execution.started", "provider.stream.completed", "run.execution.finished"} {
		if !strings.Contains(output, expected) {
			t.Errorf("diagnostics do not contain %q: %s", expected, output)
		}
	}
	for _, sensitive := range []string{"do-not-log-input", "do-not-log-output", "do-not-log-metadata"} {
		if strings.Contains(output, sensitive) {
			t.Errorf("diagnostics contain sensitive value %q: %s", sensitive, output)
		}
	}
}

func TestRuntimeExecutesToolAndContinues(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{
		responses: []providerResponse{
			{
				message: agent.Message{
					Role: agent.RoleAssistant,
					ToolCalls: []agent.ToolCall{
						{Name: "uppercase", Arguments: json.RawMessage(`{"text":"hello"}`)},
					},
				},
			},
			{message: agent.Message{Role: agent.RoleAssistant, Text: "HELLO"}},
		},
	}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider,
		Tools:    []agent.Tool{uppercaseTool{}},
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}

	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID:      "coding",
		SessionID:    "session-1",
		Metadata:     map[string]string{"source": "test"},
		Instructions: "Use registered tools",
		Input:        []agent.Message{{Role: agent.RoleUser, Text: "uppercase hello"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	events, result, runErr := collectRun(handle)
	if runErr != nil {
		t.Fatalf("Wait() error = %v", runErr)
	}
	if result.Status != agent.RunStatusCompleted {
		t.Fatalf("Status = %q, want %q", result.Status, agent.RunStatusCompleted)
	}
	if result.AgentID != "coding" || result.SessionID != "session-1" {
		t.Errorf("result identity = %q/%q", result.AgentID, result.SessionID)
	}
	if result.ProviderCalls != 2 {
		t.Errorf("ProviderCalls = %d, want 2", result.ProviderCalls)
	}
	if result.ToolCalls != 1 {
		t.Errorf("ToolCalls = %d, want 1", result.ToolCalls)
	}
	if len(result.ToolResults) != 1 || result.ToolResults[0].Output != "HELLO" {
		t.Fatalf("ToolResults = %#v, want one HELLO result", result.ToolResults)
	}

	requests := provider.Requests()
	if len(requests) != 2 {
		t.Fatalf("provider request count = %d, want 2", len(requests))
	}
	if requests[0].AgentID != "coding" || requests[0].SessionID != "session-1" ||
		requests[0].Metadata["source"] != "test" || requests[0].Instructions != "Use registered tools" {
		t.Errorf("provider request identity or metadata = %#v", requests[0])
	}
	secondMessages := requests[1].Messages
	if len(secondMessages) != 3 {
		t.Fatalf("second request message count = %d, want 3", len(secondMessages))
	}
	toolMessage := secondMessages[2]
	if toolMessage.Role != agent.RoleTool || toolMessage.Text != "HELLO" || toolMessage.ToolCallID != "call-1" {
		t.Errorf("tool message = %#v", toolMessage)
	}

	wantEvents := []agent.EventType{
		agent.EventRunStarted,
		agent.EventUserMessageAdded,
		agent.EventModelRequest,
		agent.EventMessageStarted,
		agent.EventMessageCompleted,
		agent.EventToolStarted,
		agent.EventToolCompleted,
		agent.EventModelRequest,
		agent.EventMessageStarted,
		agent.EventMessageDelta,
		agent.EventMessageCompleted,
		agent.EventRunCompleted,
	}
	assertEventTypes(t, events, wantEvents)
}

func TestRuntimeEmitsAppendOnlyPrefixManifests(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{
		model: "test-model",
		responses: []providerResponse{
			{message: agent.Message{
				Role:      agent.RoleAssistant,
				ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "missing", Arguments: json.RawMessage(`{}`)}},
			}},
			{message: agent.Message{Role: agent.RoleAssistant, Text: "done"}},
		},
	}
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, _, err := collectRun(handle)
	if err != nil {
		t.Fatal(err)
	}
	var manifests []agent.PrefixManifest
	for _, event := range events {
		if event.Type == agent.EventModelRequest {
			if event.PrefixManifest == nil {
				t.Fatal("model.request.started is missing Prefix Manifest")
			}
			manifests = append(manifests, *event.PrefixManifest)
		}
	}
	if len(manifests) != 2 {
		t.Fatalf("Prefix Manifest count = %d, want 2", len(manifests))
	}
	if manifests[0].Provider != "scripted" || manifests[0].Model != "test-model" || manifests[0].Epoch == "" {
		t.Fatalf("first Prefix Manifest = %#v", manifests[0])
	}
	if len(manifests[0].Segments) != 3 || len(manifests[1].Segments) != 5 {
		t.Fatalf("Prefix Segment counts = %d/%d, want 3/5", len(manifests[0].Segments), len(manifests[1].Segments))
	}
	for index := range manifests[0].Segments {
		if manifests[0].Segments[index] != manifests[1].Segments[index] {
			t.Fatalf("append-only Prefix diverged at Segment %d: %#v / %#v", index, manifests[0], manifests[1])
		}
	}
}

func TestRuntimeAggregatesCompleteInputTokenDetails(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{
			Role:      agent.RoleAssistant,
			ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "missing"}},
			Usage: &agent.Usage{
				InputTokens:               10,
				OutputTokens:              2,
				TotalTokens:               12,
				InputTokenDetailsReported: true,
				UncachedInputTokens:       6,
				CacheReadInputTokens:      3,
				CacheWriteInputTokens:     1,
			},
		}},
		{message: agent.Message{
			Role: agent.RoleAssistant,
			Text: "done",
			Usage: &agent.Usage{
				InputTokens:               20,
				OutputTokens:              4,
				TotalTokens:               24,
				InputTokenDetailsReported: true,
				UncachedInputTokens:       12,
				CacheReadInputTokens:      5,
				CacheWriteInputTokens:     3,
			},
		}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := collectRun(handle)
	if err != nil {
		t.Fatal(err)
	}
	usage := result.Usage
	if usage.InputTokens != 30 || usage.OutputTokens != 6 || usage.TotalTokens != 36 ||
		!usage.InputTokenDetailsReported || usage.UncachedInputTokens != 18 ||
		usage.CacheReadInputTokens != 8 || usage.CacheWriteInputTokens != 4 {
		t.Fatalf("aggregated Usage = %#v", usage)
	}
}

func TestRuntimeDoesNotPublishPartialInputTokenDetails(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{
			Role:      agent.RoleAssistant,
			ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "missing"}},
			Usage: &agent.Usage{
				InputTokens:               10,
				OutputTokens:              2,
				TotalTokens:               12,
				InputTokenDetailsReported: true,
				UncachedInputTokens:       6,
				CacheReadInputTokens:      3,
				CacheWriteInputTokens:     1,
			},
		}},
		{message: agent.Message{
			Role:  agent.RoleAssistant,
			Text:  "done",
			Usage: &agent.Usage{InputTokens: 20, OutputTokens: 4, TotalTokens: 24},
		}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, result, err := collectRun(handle)
	if err != nil {
		t.Fatal(err)
	}
	usage := result.Usage
	if usage.InputTokens != 30 || usage.OutputTokens != 6 || usage.TotalTokens != 36 ||
		usage.InputTokenDetailsReported || usage.UncachedInputTokens != 0 ||
		usage.CacheReadInputTokens != 0 || usage.CacheWriteInputTokens != 0 {
		t.Fatalf("aggregated Usage = %#v", usage)
	}
	var firstMessageUsage *agent.Usage
	for _, event := range events {
		if event.Type == agent.EventMessageCompleted && event.Message != nil && event.Message.Usage != nil {
			firstMessageUsage = event.Message.Usage
			break
		}
	}
	if firstMessageUsage == nil || !firstMessageUsage.InputTokenDetailsReported ||
		firstMessageUsage.CacheReadInputTokens != 3 {
		t.Fatalf("first message Usage = %#v", firstMessageUsage)
	}
}

func TestRuntimeRejectsInconsistentInputTokenDetails(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{{message: agent.Message{
		Role: agent.RoleAssistant,
		Text: "invalid",
		Usage: &agent.Usage{
			InputTokens:               10,
			OutputTokens:              2,
			TotalTokens:               12,
			InputTokenDetailsReported: true,
			UncachedInputTokens:       5,
			CacheReadInputTokens:      4,
			CacheWriteInputTokens:     2,
		},
	}}}}
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, runErr := collectRun(handle)
	if runErr == nil || !strings.Contains(runErr.Error(), "input token categories total 11, want 10") {
		t.Fatalf("Run error = %v", runErr)
	}
}

func TestRuntimeRejectsUsageAggregationOverflow(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{
			Role:      agent.RoleAssistant,
			ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "missing"}},
			Usage:     &agent.Usage{InputTokens: math.MaxInt64, TotalTokens: math.MaxInt64},
		}},
		{message: agent.Message{
			Role:  agent.RoleAssistant,
			Text:  "done",
			Usage: &agent.Usage{InputTokens: 1, TotalTokens: 1},
		}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, runErr := collectRun(handle)
	if runErr == nil || result.Status != agent.RunStatusFailed || !strings.Contains(runErr.Error(), "usage value overflow") {
		t.Fatalf("Run = %#v, error = %v", result, runErr)
	}
}

func TestRuntimeFailsBeforeProviderCallWhenContextCompilationFails(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{{message: agent.Message{Role: agent.RoleAssistant, Text: "unused"}}}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider,
		ContextCompiler: contextCompilerFunc(func(context.Context, agent.ContextCompileRequest) (agent.CompiledContext, error) {
			return agent.CompiledContext{}, errors.New("compiler unavailable")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, result, runErr := collectRun(handle)
	if runErr == nil || !strings.Contains(runErr.Error(), "compiler unavailable") {
		t.Fatalf("Run error = %v", runErr)
	}
	if result.ProviderCalls != 0 || len(provider.Requests()) != 0 {
		t.Fatalf("Provider calls = %d/%d", result.ProviderCalls, len(provider.Requests()))
	}
	if len(events) != 3 || events[2].Type != agent.EventRunFailed {
		t.Fatalf("Events = %#v", events)
	}
}

func TestRuntimeReturnsToolErrorsToProvider(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{
		responses: []providerResponse{
			{
				message: agent.Message{
					Role:      agent.RoleAssistant,
					ToolCalls: []agent.ToolCall{{ID: "missing-1", Name: "missing"}},
				},
			},
			{message: agent.Message{Role: agent.RoleAssistant, Text: "recovered"}},
		},
	}
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}

	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "call missing"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	_, result, runErr := collectRun(handle)
	if runErr != nil {
		t.Fatalf("Wait() error = %v", runErr)
	}
	if len(result.ToolResults) != 1 || !result.ToolResults[0].IsError {
		t.Fatalf("ToolResults = %#v, want one error result", result.ToolResults)
	}
	if !strings.Contains(result.ToolResults[0].Output, "not registered") {
		t.Errorf("tool error = %q, want not registered", result.ToolResults[0].Output)
	}

	requests := provider.Requests()
	if got := requests[1].Messages[2].Text; !strings.Contains(got, "not registered") {
		t.Errorf("tool result message = %q, want not registered", got)
	}
	if !requests[1].Messages[2].ToolIsError {
		t.Error("tool result message did not preserve the error flag")
	}
}

func TestRuntimeStopsAtProviderCallLimit(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{
		responses: []providerResponse{
			{
				message: agent.Message{
					Role:      agent.RoleAssistant,
					ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "missing"}},
				},
			},
		},
	}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:         provider,
		MaxProviderCalls: 1,
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}

	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	events, result, runErr := collectRun(handle)
	if !errors.Is(runErr, agent.ErrProviderCallLimit) {
		t.Fatalf("Wait() error = %v, want ErrProviderCallLimit", runErr)
	}
	if result.Status != agent.RunStatusFailed {
		t.Errorf("Status = %q, want %q", result.Status, agent.RunStatusFailed)
	}
	if got := events[len(events)-1].Type; got != agent.EventRunFailed {
		t.Errorf("last event = %q, want %q", got, agent.EventRunFailed)
	}
}

func TestRuntimeCancellation(t *testing.T) {
	t.Parallel()

	runtime, err := agent.NewRuntime(agent.Options{Provider: blockingProvider{}})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "wait"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	handle.Cancel()
	events, result, runErr := collectRun(handle)
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", runErr)
	}
	if result.Status != agent.RunStatusCanceled {
		t.Errorf("Status = %q, want %q", result.Status, agent.RunStatusCanceled)
	}
	if got := events[len(events)-1].Type; got != agent.EventRunCanceled {
		t.Errorf("last event = %q, want %q", got, agent.EventRunCanceled)
	}
}

func TestRuntimePinsToolSourceForRunAndReleasesLease(t *testing.T) {
	t.Parallel()

	released := make(chan struct{})
	source := toolSourceFunc(func(ctx context.Context) ([]agent.Tool, func(), error) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		return []agent.Tool{namedTool{name: "sourced"}}, func() { close(released) }, nil
	})
	provider := &scriptedProvider{responses: []providerResponse{{
		message: agent.Message{Role: agent.RoleAssistant, Text: "done"},
	}}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:   provider,
		ToolSource: source,
		Tools:      []agent.Tool{namedTool{name: "fixed"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := collectRun(handle)
	if err != nil || result.Status != agent.RunStatusCompleted {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("ToolSource lease was not released")
	}
	requests := provider.Requests()
	if len(requests) != 1 || len(requests[0].Tools) != 2 ||
		requests[0].Tools[0].Name != "fixed" || requests[0].Tools[1].Name != "sourced" {
		t.Fatalf("Provider Tools = %#v", requests)
	}
}

func TestRuntimeReleasesInvalidToolSourceLease(t *testing.T) {
	t.Parallel()

	released := false
	source := toolSourceFunc(func(context.Context) ([]agent.Tool, func(), error) {
		return []agent.Tool{namedTool{name: "duplicate"}}, func() { released = true }, nil
	})
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:   &scriptedProvider{},
		ToolSource: source,
		Tools:      []agent.Tool{namedTool{name: "duplicate"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err == nil || !strings.Contains(err.Error(), "registered more than once") {
		t.Fatalf("Run() error = %v, want duplicate Tool", err)
	}
	if !released {
		t.Fatal("invalid ToolSource lease was not released")
	}
}

func TestRuntimeEmitsProviderTextDeltas(t *testing.T) {
	t.Parallel()

	runtime, err := agent.NewRuntime(agent.Options{Provider: deltaProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "stream"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, result, err := collectRun(handle)
	if err != nil || result.Messages[len(result.Messages)-1].Text != "hello" {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	var deltas []string
	for _, event := range events {
		if event.Type == agent.EventMessageDelta {
			deltas = append(deltas, event.Delta)
		}
	}
	if strings.Join(deltas, "") != "hello" || len(deltas) != 2 {
		t.Fatalf("deltas = %#v", deltas)
	}
}

func TestRuntimePersistsAndContinuesSession(t *testing.T) {
	t.Parallel()

	store := session.NewMemoryStore()
	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, Text: "one"}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "two"}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider, SessionStore: store})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"first", "second"} {
		handle, runErr := runtime.Run(context.Background(), agent.RunRequest{
			SessionID: "session-1",
			Input:     []agent.Message{{Role: agent.RoleUser, Text: input}},
		})
		if runErr != nil {
			t.Fatal(runErr)
		}
		if _, _, runErr = collectRun(handle); runErr != nil {
			t.Fatal(runErr)
		}
	}
	requests := provider.Requests()
	if len(requests) != 2 || len(requests[1].Messages) != 3 {
		t.Fatalf("Provider requests = %#v", requests)
	}
	if requests[1].Messages[0].Text != "first" || requests[1].Messages[1].Text != "one" || requests[1].Messages[2].Text != "second" {
		t.Fatalf("continued messages = %#v", requests[1].Messages)
	}
	snapshot, err := store.Snapshot(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 14 || len(snapshot.Messages) != 4 {
		t.Fatalf("Session Snapshot = revision %d, messages %#v", snapshot.Revision, snapshot.Messages)
	}
}

func TestRuntimePublishesAndReusesContextCheckpoint(t *testing.T) {
	t.Parallel()

	store := session.NewMemoryStore()
	objects := evidence.NewMemoryObjectStore()
	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          2200,
		RecentMessages:         1,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     1000,
	}, objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, Text: "first done"}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "second done"}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:        provider,
		SessionStore:    store,
		ContextCompiler: compiler,
	})
	if err != nil {
		t.Fatal(err)
	}
	inputs := make([]agent.Message, 10)
	for index := range inputs {
		inputs[index] = agent.Message{Role: agent.RoleUser, Text: strings.Repeat(string(rune('a'+index)), 500)}
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{SessionID: "compacted", Input: inputs})
	if err != nil {
		t.Fatal(err)
	}
	events, result, err := collectRun(handle)
	if err != nil {
		t.Fatal(err)
	}
	contextIndex := -1
	modelIndex := -1
	for index, event := range events {
		switch event.Type {
		case agent.EventContextCompacted:
			contextIndex = index
			if event.ContextCheckpoint == nil || event.ContextCompaction == nil {
				t.Fatalf("context.compacted Event = %#v", event)
			}
		case agent.EventModelRequest:
			if modelIndex == -1 {
				modelIndex = index
			}
		}
	}
	if contextIndex < 0 || modelIndex < 0 || contextIndex >= modelIndex || result.ContextCheckpoint == nil {
		t.Fatalf("Context event/model/result = %d/%d/%#v", contextIndex, modelIndex, result.ContextCheckpoint)
	}

	handle, err = runtime.Run(context.Background(), agent.RunRequest{
		SessionID: "compacted",
		Input:     []agent.Message{{Role: agent.RoleUser, Text: "continue"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, secondResult, err := collectRun(handle)
	if err != nil {
		t.Fatal(err)
	}
	if secondResult.ContextCompaction == nil || secondResult.ContextCompaction.Reason != "reuse_checkpoint" {
		t.Fatalf("second Context Compaction = %#v", secondResult.ContextCompaction)
	}
	requests := provider.Requests()
	if len(requests) != 2 || len(requests[1].Messages) == 0 ||
		!strings.Contains(requests[1].Messages[0].Text, "<qed_context_checkpoint>") {
		t.Fatalf("second Provider request = %#v", requests)
	}
	snapshot, err := store.Load(context.Background(), "compacted")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Checkpoint == nil || len(snapshot.EvidenceObjects) == 0 || len(snapshot.Messages) != 13 {
		t.Fatalf("compacted Session Snapshot = %#v", snapshot)
	}
}

func TestRuntimeWaitsAndResumesThroughRunHandle(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{
			ID: "approval-call", Name: "approval_tool", Arguments: json.RawMessage(`{}`),
		}}}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "continued"}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider, Tools: []agent.Tool{approvalTool{}}})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "ask"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var events []agent.Event
	for event := range handle.Events() {
		events = append(events, event)
		if event.Type == agent.EventRunWaiting {
			if event.WaitRequest == nil || event.WaitRequest.Kind != agent.WaitKindApproval {
				t.Fatalf("waiting Event = %#v", event)
			}
			pending, ok := handle.PendingWait()
			if !ok || pending.ID != event.WaitRequest.ID {
				t.Fatalf("PendingWait() = %#v, %v", pending, ok)
			}
			if err := handle.Resume(agent.WaitResponse{
				RequestID: pending.ID,
				Payload:   json.RawMessage(`{"approved":true}`),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	result, err := handle.Wait()
	if err != nil || result.Status != agent.RunStatusCompleted || len(result.ToolResults) != 1 || result.ToolResults[0].Output != "approved" {
		t.Fatalf("Wait() = %#v, %v", result, err)
	}
	var waiting, resumed bool
	for _, event := range events {
		waiting = waiting || event.Type == agent.EventRunWaiting
		resumed = resumed || event.Type == agent.EventRunResumed
	}
	if !waiting || !resumed {
		t.Fatalf("waiting lifecycle Events = %#v", events)
	}
}

func TestRuntimeEnforcesSharedBudget(t *testing.T) {
	t.Parallel()

	budget, err := agent.NewBudget(agent.BudgetLimits{MaxProviderCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, Text: "first"}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "unexpected"}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	first, err := runtime.Run(context.Background(), agent.RunRequest{
		Budget: budget,
		Input:  []agent.Message{{Role: agent.RoleUser, Text: "first"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := collectRun(first); err != nil {
		t.Fatal(err)
	}
	second, err := runtime.Run(context.Background(), agent.RunRequest{
		Budget: budget,
		Input:  []agent.Message{{Role: agent.RoleUser, Text: "second"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := collectRun(second)
	if !errors.Is(err, agent.ErrBudgetProviderCalls) || result.Status != agent.RunStatusFailed {
		t.Fatalf("second Run = %#v, %v", result, err)
	}
	if snapshot := budget.Snapshot(); snapshot.ProviderCalls != 1 {
		t.Fatalf("Budget Snapshot = %#v", snapshot)
	}
}

func TestRuntimeResumesPersistedPendingToolWithoutRepeatingProviderCall(t *testing.T) {
	t.Parallel()

	store := session.NewMemoryStore()
	assistant := agent.Message{
		Role: agent.RoleAssistant,
		ToolCalls: []agent.ToolCall{{
			ID:        "tool-1",
			Name:      "resume_wait",
			Arguments: json.RawMessage(`{}`),
		}},
	}
	wait := agent.WaitRequest{
		ID:      "approval-fixed",
		Kind:    agent.WaitKindApproval,
		Payload: json.RawMessage(`{"tool":"resume_wait","capabilities":[]}`),
	}
	call := assistant.ToolCalls[0]
	_, err := store.Append(context.Background(), "resume-session", 0, []agent.Event{
		{Type: agent.EventUserMessageAdded, Message: &agent.Message{Role: agent.RoleUser, Text: "start"}},
		{Type: agent.EventMessageCompleted, Message: &assistant},
		{Type: agent.EventToolStarted, ToolCall: &call},
		{Type: agent.EventRunWaiting, WaitRequest: &wait},
	})
	if err != nil {
		t.Fatalf("seed Session: %v", err)
	}

	provider := &scriptedProvider{responses: []providerResponse{{
		message: agent.Message{Role: agent.RoleAssistant, Text: "resumed"},
	}}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:     provider,
		Tools:        []agent.Tool{resumeWaitTool{}},
		SessionStore: store,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		SessionID: "resume-session",
		Resume: &agent.WaitResponse{
			RequestID: "approval-fixed",
			Payload:   json.RawMessage(`{"approved":true}`),
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, result, err := collectRun(handle)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.Status != agent.RunStatusCompleted || len(result.ToolResults) != 1 || result.ToolResults[0].Output != "approved" {
		t.Fatalf("result = %#v", result)
	}
	requests := provider.Requests()
	if len(requests) != 1 || len(requests[0].Messages) != 3 || requests[0].Messages[2].Role != agent.RoleTool {
		t.Fatalf("Provider requests = %#v", requests)
	}
	var resumed bool
	for _, event := range events {
		if event.Type == agent.EventRunResumed && event.WaitResponse != nil && event.WaitResponse.RequestID == "approval-fixed" {
			resumed = true
		}
	}
	if !resumed {
		t.Fatalf("events do not contain persisted resume: %#v", events)
	}
	snapshot, err := store.Load(context.Background(), "resume-session")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if snapshot.PendingWait != nil || snapshot.PendingTool != nil {
		t.Fatalf("pending state was not cleared: %#v", snapshot)
	}
}

type providerResponse struct {
	message agent.Message
	err     error
}

type scriptedProvider struct {
	mu        sync.Mutex
	model     string
	responses []providerResponse
	requests  []agent.ModelRequest
}

func (provider *scriptedProvider) Name() string {
	return "scripted"
}

func (provider *scriptedProvider) ModelID() string {
	return provider.model
}

func (provider *scriptedProvider) Complete(_ context.Context, request agent.ModelRequest) (agent.Message, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()

	provider.requests = append(provider.requests, request)
	index := len(provider.requests) - 1
	if index >= len(provider.responses) {
		return agent.Message{}, errors.New("scripted provider exhausted")
	}
	response := provider.responses[index]
	return response.message, response.err
}

func (provider *scriptedProvider) Stream(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	message, err := provider.Complete(ctx, request)
	if err != nil {
		return nil, err
	}
	return agent.MessageStream(message), nil
}

func (provider *scriptedProvider) Requests() []agent.ModelRequest {
	provider.mu.Lock()
	defer provider.mu.Unlock()

	return append([]agent.ModelRequest(nil), provider.requests...)
}

type contextCompilerFunc func(context.Context, agent.ContextCompileRequest) (agent.CompiledContext, error)

func (compiler contextCompilerFunc) Compile(
	ctx context.Context,
	request agent.ContextCompileRequest,
) (agent.CompiledContext, error) {
	return compiler(ctx, request)
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

type blockingProvider struct{}

func (blockingProvider) Name() string {
	return "blocking"
}

type toolSourceFunc func(context.Context) ([]agent.Tool, func(), error)

func (source toolSourceFunc) AcquireTools(ctx context.Context) ([]agent.Tool, func(), error) {
	return source(ctx)
}

type namedTool struct {
	name string
}

type deltaProvider struct{}

func (deltaProvider) Name() string { return "delta" }

func (deltaProvider) Stream(context.Context, agent.ModelRequest) (agent.ModelStream, error) {
	events := []agent.ModelStreamEvent{
		{Type: agent.ModelStreamTextDelta, TextDelta: "hel"},
		{Type: agent.ModelStreamTextDelta, TextDelta: "lo"},
		{Type: agent.ModelStreamMessageComplete, Message: &agent.Message{Role: agent.RoleAssistant, Text: "hello"}},
	}
	index := 0
	return &agent.ModelStreamFunc{NextFunc: func() (agent.ModelStreamEvent, error) {
		if index >= len(events) {
			return agent.ModelStreamEvent{}, io.EOF
		}
		event := events[index]
		index++
		return event, nil
	}}, nil
}

type recordingHook struct {
	mu     sync.Mutex
	events []agent.EventType
}

type failingCompletedHook struct{}

func (failingCompletedHook) Definition() agent.HookDefinition {
	return agent.HookDefinition{EventTypes: []agent.EventType{agent.EventRunCompleted}}
}

func (failingCompletedHook) Handle(context.Context, agent.Event) error {
	return errors.New("terminal Hook failed")
}

func (hook *recordingHook) Definition() agent.HookDefinition {
	return agent.HookDefinition{
		EventTypes:          []agent.EventType{agent.EventRunStarted, agent.EventRunCompleted},
		ExtensionID:         "test-hook",
		ExtensionGeneration: 3,
	}
}

func (hook *recordingHook) Handle(_ context.Context, event agent.Event) error {
	hook.mu.Lock()
	hook.events = append(hook.events, event.Type)
	hook.mu.Unlock()
	return nil
}

func (hook *recordingHook) types() []agent.EventType {
	hook.mu.Lock()
	defer hook.mu.Unlock()
	return append([]agent.EventType(nil), hook.events...)
}

type componentSource struct {
	hook     agent.Hook
	releases int
}

func (source *componentSource) AcquireComponents(context.Context) (agent.RunComponents, func(), error) {
	return agent.RunComponents{Hooks: []agent.Hook{source.hook}}, func() { source.releases++ }, nil
}

type approvalTool struct{}

func (approvalTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "approval_tool", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (approvalTool) Execute(ctx context.Context, _ agent.ToolCall) (agent.ToolResult, error) {
	response, err := agent.WaitForInput(ctx, agent.WaitRequest{
		Kind:   agent.WaitKindApproval,
		Prompt: "Approve approval_tool",
	})
	if err != nil {
		return agent.ToolResult{}, err
	}
	var payload struct {
		Approved bool `json:"approved"`
	}
	if err := json.Unmarshal(response.Payload, &payload); err != nil {
		return agent.ToolResult{}, err
	}
	if !payload.Approved {
		return agent.ToolResult{Output: "denied", IsError: true}, nil
	}
	return agent.ToolResult{Output: "approved"}, nil
}

type resumeWaitTool struct{}

func (resumeWaitTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "resume_wait", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (resumeWaitTool) Execute(ctx context.Context, _ agent.ToolCall) (agent.ToolResult, error) {
	response, err := agent.WaitForInput(ctx, agent.WaitRequest{
		ID:   "approval-fixed",
		Kind: agent.WaitKindApproval,
	})
	if err != nil {
		return agent.ToolResult{}, err
	}
	var payload struct {
		Approved bool `json:"approved"`
	}
	if err := json.Unmarshal(response.Payload, &payload); err != nil {
		return agent.ToolResult{}, err
	}
	if !payload.Approved {
		return agent.ToolResult{Output: "denied", IsError: true}, nil
	}
	return agent.ToolResult{Output: "approved"}, nil
}

func (tool namedTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: tool.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (tool namedTool) Execute(context.Context, agent.ToolCall) (agent.ToolResult, error) {
	return agent.ToolResult{Output: tool.name}, nil
}

func (blockingProvider) Complete(ctx context.Context, _ agent.ModelRequest) (agent.Message, error) {
	<-ctx.Done()
	return agent.Message{}, ctx.Err()
}

func (provider blockingProvider) Stream(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	message, err := provider.Complete(ctx, request)
	if err != nil {
		return nil, err
	}
	return agent.MessageStream(message), nil
}

func collectRun(handle *agent.RunHandle) ([]agent.Event, agent.RunResult, error) {
	var events []agent.Event
	for event := range handle.Events() {
		events = append(events, event)
	}
	result, err := handle.Wait()
	return events, result, err
}

func assertEventTypes(t *testing.T, events []agent.Event, want []agent.EventType) {
	t.Helper()

	if len(events) != len(want) {
		t.Fatalf("event count = %d, want %d: %#v", len(events), len(want), events)
	}
	for index := range want {
		if events[index].Type != want[index] {
			t.Errorf("event[%d] = %q, want %q", index, events[index].Type, want[index])
		}
		if events[index].Sequence != uint64(index+1) {
			t.Errorf("event[%d].Sequence = %d, want %d", index, events[index].Sequence, index+1)
		}
	}
}
