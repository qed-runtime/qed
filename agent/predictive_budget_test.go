package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/evidence"
	"github.com/qed-runtime/qed/session"
)

func TestBuildPredictiveBudgetPlanThresholds(t *testing.T) {
	t.Parallel()

	policy := agent.PredictiveBudgetPolicy{
		ContextWindowTokens:       100,
		OutputReserveTokens:       10,
		SafetyMarginTokens:        15,
		PredictedToolOutputTokens: 5,
		SoftThresholdTokens:       80,
	}
	tests := []struct {
		name  string
		input int64
		want  agent.PredictiveBudgetLevel
	}{
		{name: "within", input: 59, want: agent.PredictiveBudgetWithin},
		{name: "soft boundary", input: 60, want: agent.PredictiveBudgetSoft},
		{name: "hard boundary fits", input: 80, want: agent.PredictiveBudgetSoft},
		{name: "hard exceeded", input: 81, want: agent.PredictiveBudgetHard},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			plan, err := agent.BuildPredictiveBudgetPlan(policy, test.input, "test_tokenizer")
			if err != nil {
				t.Fatal(err)
			}
			if plan.Level != test.want || plan.RequiredReserveTokens != 15 ||
				plan.MaxInputTokens != 80 || plan.PredictedTotalTokens != test.input+20 ||
				plan.CandidatePredictedTotalTokens != plan.PredictedTotalTokens ||
				plan.ProviderPredictedTotalTokens != plan.PredictedTotalTokens {
				t.Fatalf("plan = %#v", plan)
			}
		})
	}
}

func TestPredictiveBudgetPolicyValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		policy agent.PredictiveBudgetPolicy
		want   string
	}{
		{name: "window", policy: agent.PredictiveBudgetPolicy{}, want: "context window"},
		{name: "output", policy: agent.PredictiveBudgetPolicy{ContextWindowTokens: 100}, want: "output reserve"},
		{name: "negative margin", policy: agent.PredictiveBudgetPolicy{
			ContextWindowTokens: 100, OutputReserveTokens: 10, SafetyMarginTokens: -1, SoftThresholdTokens: 80,
		}, want: "must not be negative"},
		{name: "soft", policy: agent.PredictiveBudgetPolicy{
			ContextWindowTokens: 100, OutputReserveTokens: 10, SoftThresholdTokens: 100,
		}, want: "soft threshold"},
		{name: "reserves", policy: agent.PredictiveBudgetPolicy{
			ContextWindowTokens: 100, OutputReserveTokens: 90, PredictedToolOutputTokens: 10,
			SoftThresholdTokens: 95,
		}, want: "leave no model input"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := agent.BuildPredictiveBudgetPlan(test.policy, 1, "test"); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("BuildPredictiveBudgetPlan() error = %v, want %q", err, test.want)
			}
		})
	}
	provider := &scriptedProvider{}
	if _, err := agent.NewRuntime(agent.Options{
		Provider: provider,
		PredictiveBudget: &agent.PredictiveBudgetPolicy{
			ContextWindowTokens: 100, OutputReserveTokens: 10, SoftThresholdTokens: 80,
		},
	}); err == nil || !strings.Contains(err.Error(), "Predictive Context Compiler") {
		t.Fatalf("NewRuntime() error = %v", err)
	}
}

func TestRuntimePredictiveBudgetSoftFailureContinuesAndHardFailureStops(t *testing.T) {
	t.Parallel()

	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          1 << 20,
		RecentMessages:         1,
		EvidenceThresholdBytes: 1 << 18,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     4096,
	}, evidence.NewMemoryObjectStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	newRuntime := func(window int64, provider *scriptedProvider) *agent.Runtime {
		runtime, err := agent.NewRuntime(agent.Options{
			Provider:        provider,
			ContextCompiler: compiler,
			TokenEstimator:  &recordingTokenEstimator{kind: "test_tokenizer", tokensPerItem: 11},
			PredictiveBudget: &agent.PredictiveBudgetPolicy{
				ContextWindowTokens: window, OutputReserveTokens: 15,
				PredictedToolOutputTokens: 5, SoftThresholdTokens: 40,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return runtime
	}

	softProvider := &scriptedProvider{responses: []providerResponse{{
		message: agent.Message{Role: agent.RoleAssistant, Text: "safe"},
	}}}
	softHandle, err := newRuntime(60, softProvider).Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "only message"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	softEvents, softResult, err := collectRun(softHandle)
	if err != nil {
		t.Fatal(err)
	}
	if len(softProvider.Requests()) != 1 || softResult.PredictiveBudget == nil ||
		softResult.PredictiveBudget.Level != agent.PredictiveBudgetSoft ||
		softResult.PredictiveBudget.Action != agent.PredictiveBudgetActionNone ||
		eventOfType(softEvents, agent.EventContextCompactionPrepared) != nil {
		t.Fatalf("soft failure result/events = %#v/%#v", softResult.PredictiveBudget, softEvents)
	}

	hardProvider := &scriptedProvider{responses: []providerResponse{{
		message: agent.Message{Role: agent.RoleAssistant, Text: "must not run"},
	}}}
	hardHandle, err := newRuntime(50, hardProvider).Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "only message"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, hardResult, err := collectRun(hardHandle)
	if err == nil || !strings.Contains(err.Error(), "no safe Checkpoint boundary") ||
		len(hardProvider.Requests()) != 0 || hardResult.PredictiveBudget == nil ||
		hardResult.PredictiveBudget.Level != agent.PredictiveBudgetHard {
		t.Fatalf("hard failure = result %#v, requests %d, error %v", hardResult, len(hardProvider.Requests()), err)
	}
}

func TestRuntimePredictiveBudgetReestimatesCompilerOutput(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{{
		message: agent.Message{Role: agent.RoleAssistant, Text: "must not run"},
	}}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:        provider,
		ContextCompiler: underestimatingPredictiveCompiler{},
		TokenEstimator:  &recordingTokenEstimator{kind: "test_tokenizer", tokensPerItem: 11},
		PredictiveBudget: &agent.PredictiveBudgetPolicy{
			ContextWindowTokens:       40,
			OutputReserveTokens:       10,
			PredictedToolOutputTokens: 5,
			SoftThresholdTokens:       30,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "request"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := collectRun(handle)
	if err == nil || !strings.Contains(err.Error(), "Predictive Context Compiler returned") ||
		len(provider.Requests()) != 0 || result.PredictiveBudget == nil ||
		result.PredictiveBudget.Level != agent.PredictiveBudgetHard {
		t.Fatalf("forged estimate result = %#v, requests %d, error %v", result, len(provider.Requests()), err)
	}
}

func TestRuntimePreparesThenAdoptsPredictiveBudgetCandidate(t *testing.T) {
	t.Parallel()

	store := session.NewMemoryStore()
	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          1 << 20,
		RecentMessages:         2,
		EvidenceThresholdBytes: 1 << 18,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     4096,
	}, evidence.NewMemoryObjectStore(), nil)
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
		TokenEstimator:  &recordingTokenEstimator{kind: "test_tokenizer", tokensPerItem: 11},
		PredictiveBudget: &agent.PredictiveBudgetPolicy{
			ContextWindowTokens:       120,
			OutputReserveTokens:       15,
			SafetyMarginTokens:        10,
			PredictedToolOutputTokens: 5,
			SoftThresholdTokens:       100,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	firstInput := make([]agent.Message, 6)
	for index := range firstInput {
		firstInput[index] = agent.Message{Role: agent.RoleUser, Text: strings.Repeat(string(rune('a'+index)), 64)}
	}
	firstHandle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID: "worker", SessionID: "predictive-session", Input: firstInput,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstEvents, firstResult, err := collectRun(firstHandle)
	if err != nil {
		t.Fatal(err)
	}
	prepared := eventOfType(firstEvents, agent.EventContextCompactionPrepared)
	if prepared == nil || prepared.PredictiveBudget == nil ||
		prepared.PredictiveBudget.Action != agent.PredictiveBudgetActionPrepare ||
		prepared.PredictiveBudget.ProviderInputTokenEstimate != prepared.PredictiveBudget.InputTokenEstimate ||
		prepared.PredictiveBudget.CandidateInputTokenEstimate >= prepared.PredictiveBudget.InputTokenEstimate ||
		prepared.ContextCheckpoint == nil || prepared.ContextCompaction == nil ||
		prepared.ContextCompaction.Validation == nil || !prepared.ContextCompaction.Validation.Passed {
		t.Fatalf("prepared Event = %#v", prepared)
	}
	if firstResult.ContextCheckpoint != nil || firstResult.PredictiveBudget == nil ||
		firstResult.PredictiveBudget.Action != agent.PredictiveBudgetActionPrepare {
		t.Fatalf("first result = %#v", firstResult)
	}
	firstSnapshot, err := store.Load(context.Background(), "predictive-session")
	if err != nil {
		t.Fatal(err)
	}
	if firstSnapshot.Checkpoint != nil || firstSnapshot.PreparedContext == nil ||
		firstSnapshot.PreparedContext.Checkpoint.Generation != prepared.ContextCheckpoint.Generation {
		t.Fatalf("first Session snapshot = %#v", firstSnapshot)
	}
	firstSnapshot.PreparedContext.Checkpoint.Evidence[0].Digest = "caller-mutated"
	firstSnapshot.PreparedContext.Compaction.Externalized[0].Digest = "caller-mutated"
	reloadedSnapshot, err := store.Load(context.Background(), "predictive-session")
	if err != nil {
		t.Fatal(err)
	}
	if reloadedSnapshot.PreparedContext.Checkpoint.Evidence[0].Digest == "caller-mutated" ||
		reloadedSnapshot.PreparedContext.Compaction.Externalized[0].Digest == "caller-mutated" {
		t.Fatal("Memory Session Store shares Prepared Context memory with the caller")
	}
	if _, err := agent.BuildContextLedger(context.Background(), firstEvents); err != nil {
		t.Fatalf("build prepared Context Ledger: %v", err)
	}
	tampered := append([]agent.Event(nil), firstEvents...)
	for index := range tampered {
		if tampered[index].Type == agent.EventContextCompactionPrepared {
			plan := *tampered[index].PredictiveBudget
			plan.CandidatePredictedTotalTokens++
			tampered[index].PredictiveBudget = &plan
			break
		}
	}
	if _, err := agent.BuildContextLedger(context.Background(), tampered); err == nil ||
		!strings.Contains(err.Error(), "candidate or Provider total") {
		t.Fatalf("tampered Predictive Budget Ledger error = %v", err)
	}
	tampered = append([]agent.Event(nil), firstEvents...)
	for index := range tampered {
		if tampered[index].Type == agent.EventModelRequest {
			plan := *tampered[index].CachePlan
			plan.InputTokenEstimate++
			tampered[index].CachePlan = &plan
			break
		}
	}
	if _, err := agent.BuildContextLedger(context.Background(), tampered); err == nil ||
		!strings.Contains(err.Error(), "does not match its Cache Plan") {
		t.Fatalf("tampered Predictive Budget Cache Plan error = %v", err)
	}
	jsonStore, err := session.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jsonStore.Append(context.Background(), "predictive-session", 0, firstEvents); err != nil {
		t.Fatal(err)
	}
	jsonSnapshot, err := jsonStore.Load(context.Background(), "predictive-session")
	if err != nil {
		t.Fatal(err)
	}
	if jsonSnapshot.PreparedContext == nil ||
		jsonSnapshot.PreparedContext.Checkpoint.Generation != prepared.ContextCheckpoint.Generation ||
		jsonSnapshot.PreparedContext.Budget.Action != agent.PredictiveBudgetActionPrepare {
		t.Fatalf("JSONL prepared snapshot = %#v", jsonSnapshot.PreparedContext)
	}

	secondInput := []agent.Message{
		{Role: agent.RoleUser, Text: "follow one"},
		{Role: agent.RoleUser, Text: "follow two"},
		{Role: agent.RoleUser, Text: "follow three"},
	}
	secondHandle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID: "worker", SessionID: "predictive-session", Input: secondInput,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondEvents, secondResult, err := collectRun(secondHandle)
	if err != nil {
		t.Fatal(err)
	}
	adopted := eventOfType(secondEvents, agent.EventContextCompacted)
	if adopted == nil || adopted.PredictiveBudget == nil ||
		adopted.PredictiveBudget.Action != agent.PredictiveBudgetActionAdopt ||
		adopted.ContextCheckpoint == nil ||
		adopted.ContextCheckpoint.Generation != prepared.ContextCheckpoint.Generation {
		t.Fatalf("adopted Event = %#v", adopted)
	}
	if adopted.ContextCompaction == nil || adopted.ContextCompaction.Validation == nil ||
		adopted.ContextCompaction.Validation.ActiveConstraints.Required <=
			prepared.ContextCompaction.Validation.ActiveConstraints.Required {
		t.Fatalf("adoption did not revalidate the current tail = prepared %#v, adopted %#v",
			prepared.ContextCompaction.Validation, adopted.ContextCompaction.Validation)
	}
	if secondResult.ContextCheckpoint == nil || secondResult.PredictiveBudget == nil ||
		secondResult.PredictiveBudget.Action != agent.PredictiveBudgetActionAdopt ||
		secondResult.PredictiveBudget.ProviderInputTokenEstimate != secondResult.PredictiveBudget.CandidateInputTokenEstimate ||
		secondResult.PredictiveBudget.ProviderInputTokenEstimate > secondResult.PredictiveBudget.MaxInputTokens {
		t.Fatalf("second result = %#v", secondResult)
	}
	contextReport, err := agent.BuildContextReport(context.Background(), secondResult.RunID, secondEvents)
	if err != nil {
		t.Fatal(err)
	}
	if len(contextReport.Snapshots) != 1 ||
		contextReport.Snapshots[0].Reason != "predictive_budget_adopt" {
		t.Fatalf("Predictive Budget Context report = %#v", contextReport)
	}
	secondSnapshot, err := store.Load(context.Background(), "predictive-session")
	if err != nil {
		t.Fatal(err)
	}
	if secondSnapshot.Checkpoint == nil || secondSnapshot.PreparedContext != nil ||
		secondSnapshot.Checkpoint.Generation != prepared.ContextCheckpoint.Generation {
		t.Fatalf("second Session snapshot = %#v", secondSnapshot)
	}
	requests := provider.Requests()
	if len(requests) != 2 || len(requests[0].Messages) != len(firstInput) ||
		len(requests[1].Messages) == 0 ||
		!strings.Contains(requests[1].Messages[0].Text, "<qed_context_checkpoint>") {
		t.Fatalf("Provider requests = %#v", requests)
	}
}

func eventOfType(events []agent.Event, eventType agent.EventType) *agent.Event {
	for index := range events {
		if events[index].Type == eventType {
			event := events[index]
			return &event
		}
	}
	return nil
}

type underestimatingPredictiveCompiler struct{}

func (underestimatingPredictiveCompiler) Compile(
	ctx context.Context,
	request agent.ContextCompileRequest,
) (agent.CompiledContext, error) {
	return underestimatingContext(ctx, request)
}

func (underestimatingPredictiveCompiler) CompileToTokenLimit(
	ctx context.Context,
	request agent.ContextCompileRequest,
	_ int64,
) (agent.CompiledContext, error) {
	return underestimatingContext(ctx, request)
}

func underestimatingContext(
	ctx context.Context,
	request agent.ContextCompileRequest,
) (agent.CompiledContext, error) {
	compiled, err := (agent.DefaultContextCompiler{}).Compile(ctx, request)
	if err != nil {
		return agent.CompiledContext{}, err
	}
	for index := range compiled.Segments {
		compiled.Segments[index].TokenEstimate = 0
		compiled.Segments[index].TokenEstimateKind = "forged"
	}
	return compiled, nil
}
