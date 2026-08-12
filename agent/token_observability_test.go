package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/qed-runtime/qed/agent"
)

func TestBuildTokenUsageReportComparesReportedAndMissingUsage(t *testing.T) {
	t.Parallel()

	events := []agent.Event{
		{RunID: "run", Sequence: 1, Type: agent.EventRunStarted},
		tokenModelRequestEvent("run", 2, 1, 1, 10),
		{
			RunID: "run", Sequence: 3, Type: agent.EventMessageCompleted,
			Message: &agent.Message{Role: agent.RoleAssistant, Usage: &agent.Usage{
				InputTokens: 12, OutputTokens: 2, TotalTokens: 14,
			}},
		},
		tokenModelRequestEvent("run", 4, 2, 1, 20),
		{
			RunID: "run", Sequence: 5, Type: agent.EventProviderRetry,
			ProviderCall: 2, ProviderAttempt: 1,
			ProviderRetry: &agent.ProviderRetryInfo{
				NextAttempt: 2,
			},
		},
		tokenModelRequestEvent("run", 6, 3, 2, 20),
		{RunID: "run", Sequence: 7, Type: agent.EventRunFailed},
	}
	report, err := agent.BuildTokenUsageReport(context.Background(), "run", events)
	if err != nil {
		t.Fatal(err)
	}
	if report.Version != agent.TokenUsageReportVersion || len(report.Observations) != 3 ||
		report.Observations[0].Outcome != agent.TokenUsageCompleted ||
		report.Observations[1].Outcome != agent.TokenUsageRetry ||
		report.Observations[2].Outcome != agent.TokenUsageFailed {
		t.Fatalf("Token Usage observations = %#v", report.Observations)
	}
	metrics := report.Metrics
	if metrics.RequestCount != 3 || metrics.ProviderUsageReportedCount != 1 ||
		metrics.ProviderUsageMissingCount != 2 || metrics.ComparableEstimatedInputTokens != 10 ||
		metrics.ProviderInputTokens != 12 || metrics.DifferenceTokens != 2 {
		t.Fatalf("Token Usage metrics = %#v", metrics)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"prompt text", "tool output", "metadata value"} {
		if contains(string(encoded), forbidden) {
			t.Fatalf("Token Usage report exposed %q: %s", forbidden, encoded)
		}
	}
}

func TestBuildTokenUsageReportRejectsMalformedPairing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events []agent.Event
	}{
		{
			name: "sequence",
			events: []agent.Event{
				{RunID: "run", Sequence: 1, Type: agent.EventRunStarted},
				tokenModelRequestEvent("run", 3, 1, 1, 10),
			},
		},
		{
			name: "pending",
			events: []agent.Event{
				{RunID: "run", Sequence: 1, Type: agent.EventRunStarted},
				tokenModelRequestEvent("run", 2, 1, 1, 10),
			},
		},
		{
			name: "completion",
			events: []agent.Event{
				{RunID: "run", Sequence: 1, Type: agent.EventRunStarted},
				{RunID: "run", Sequence: 2, Type: agent.EventMessageCompleted, Message: &agent.Message{Role: agent.RoleAssistant}},
			},
		},
		{
			name: "retry metadata",
			events: []agent.Event{
				{RunID: "run", Sequence: 1, Type: agent.EventRunStarted},
				tokenModelRequestEvent("run", 2, 1, 1, 10),
				{
					RunID: "run", Sequence: 3, Type: agent.EventProviderRetry,
					ProviderCall: 1, ProviderAttempt: 1,
				},
			},
		},
		{
			name: "retry attempt",
			events: []agent.Event{
				{RunID: "run", Sequence: 1, Type: agent.EventRunStarted},
				tokenModelRequestEvent("run", 2, 1, 1, 10),
				{
					RunID: "run", Sequence: 3, Type: agent.EventProviderRetry,
					ProviderCall: 1, ProviderAttempt: 2,
					ProviderRetry: &agent.ProviderRetryInfo{NextAttempt: 2},
				},
			},
		},
		{
			name: "retry progression",
			events: []agent.Event{
				{RunID: "run", Sequence: 1, Type: agent.EventRunStarted},
				tokenModelRequestEvent("run", 2, 1, 1, 10),
				{
					RunID: "run", Sequence: 3, Type: agent.EventProviderRetry,
					ProviderCall: 1, ProviderAttempt: 1,
					ProviderRetry: &agent.ProviderRetryInfo{NextAttempt: 2},
				},
				tokenModelRequestEvent("run", 4, 2, 1, 10),
			},
		},
		{
			name: "after terminal",
			events: []agent.Event{
				{RunID: "run", Sequence: 1, Type: agent.EventRunStarted},
				{RunID: "run", Sequence: 2, Type: agent.EventRunCompleted},
				{RunID: "run", Sequence: 3, Type: agent.EventUserMessageAdded},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := agent.BuildTokenUsageReport(context.Background(), "run", test.events); err == nil {
				t.Fatal("BuildTokenUsageReport() error = nil")
			}
		})
	}
}

func TestBuildTokenUsageReportAcceptsLegacyEventsWithoutEstimates(t *testing.T) {
	t.Parallel()

	events := []agent.Event{
		{RunID: "run", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "run", Sequence: 2, Type: agent.EventModelRequest,
			ProviderCall: 1, ProviderAttempt: 1,
			PrefixManifest: &agent.PrefixManifest{Provider: "provider"},
		},
		{
			RunID: "run", Sequence: 3, Type: agent.EventMessageCompleted,
			Message: &agent.Message{Role: agent.RoleAssistant},
		},
		{RunID: "run", Sequence: 4, Type: agent.EventRunCompleted},
	}
	report, err := agent.BuildTokenUsageReport(context.Background(), "run", events)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Observations) != 0 || report.Metrics.RequestCount != 0 {
		t.Fatalf("legacy Token Usage report = %#v", report)
	}
	if _, ok := report.Latest(); ok {
		t.Fatal("legacy Token Usage report unexpectedly has a latest observation")
	}
	if _, err := agent.BuildTokenUsageReport(context.Background(), "missing", events); err == nil {
		t.Fatal("BuildTokenUsageReport accepted a missing Run")
	}
}

func tokenModelRequestEvent(runID string, sequence uint64, call, attempt int, estimate int64) agent.Event {
	return agent.Event{
		RunID: runID, Sequence: sequence, Type: agent.EventModelRequest,
		ProviderCall: call, ProviderAttempt: attempt,
		PrefixManifest: &agent.PrefixManifest{Provider: "provider", Model: "model"},
		CachePlan: &agent.CachePlan{
			InputTokenEstimate: estimate,
			TokenEstimateKind:  agent.CanonicalByteTokenEstimateKind,
		},
	}
}
