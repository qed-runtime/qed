package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
)

func TestDefaultCachePlannerBuildsIsolatedExplicitPlan(t *testing.T) {
	t.Parallel()

	request := agent.ModelRequest{
		AgentID:   "coding",
		SessionID: "session-1",
		Messages:  []agent.Message{{Role: agent.RoleUser, Text: strings.Repeat("stable", 1000)}},
	}
	compiled, err := (agent.DefaultContextCompiler{}).Compile(context.Background(), agent.ContextCompileRequest{
		Provider:     "openai/responses:primary",
		Model:        "gpt-5.6-luna",
		ModelRequest: request,
	})
	if err != nil {
		t.Fatal(err)
	}
	pricing := &agent.CachePricing{
		Currency:                      "USD",
		UncachedInputMicrosPerMillion: 2_500_000,
		CacheReadMicrosPerMillion:     250_000,
		CacheWriteMicrosPerMillion:    3_000_000,
		OutputMicrosPerMillion:        10_000_000,
	}
	planRequest := agent.CachePlanRequest{
		RunID:        "run-1",
		Provider:     "openai/responses:primary",
		Model:        "gpt-5.6-luna",
		ModelRequest: compiled.ModelRequest,
		Segments:     compiled.Segments,
		Capabilities: agent.CacheCapabilities{
			ExactPrefix:         true,
			SupportsCacheKey:    true,
			SupportsExplicit:    true,
			SupportsAutomatic:   true,
			MaxWriteBreakpoints: 4,
			MinimumPrefixTokens: 1024,
			SupportedTTLs:       []agent.CacheTTL{agent.CacheTTLThirtyMinutes},
		},
		Policy: agent.CachePolicy{
			Mode:          agent.CacheModeAdaptive,
			TTL:           agent.CacheTTLThirtyMinutes,
			ExpectedReuse: 3,
			IsolationKey:  "tenant-secret",
			Family:        "repository-qed",
			Pricing:       pricing,
		},
	}
	planner := agent.DefaultCachePlanner{}
	plan, err := planner.Plan(context.Background(), planRequest)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != agent.CacheModeExplicit || len(plan.Breakpoints) != 1 ||
		plan.Breakpoints[0].MessageIndex != 0 || plan.TTL != agent.CacheTTLThirtyMinutes {
		t.Fatalf("Cache Plan = %#v", plan)
	}
	if plan.Forecast == nil || plan.Forecast.SavingsMicros <= 0 {
		t.Fatalf("Cost Forecast = %#v", plan.Forecast)
	}
	if strings.Contains(plan.FamilyID, "tenant-secret") || strings.Contains(plan.FamilyID, "repository-qed") {
		t.Fatalf("Cache family exposed raw isolation input: %q", plan.FamilyID)
	}

	planRequest.RunID = "run-2"
	sameSession, err := planner.Plan(context.Background(), planRequest)
	if err != nil {
		t.Fatal(err)
	}
	if sameSession.FamilyID != plan.FamilyID {
		t.Fatalf("same Session families differ: %q / %q", plan.FamilyID, sameSession.FamilyID)
	}
	planRequest.Policy.IsolationKey = "other-tenant"
	isolated, err := planner.Plan(context.Background(), planRequest)
	if err != nil {
		t.Fatal(err)
	}
	if isolated.FamilyID == plan.FamilyID {
		t.Fatal("different isolation keys produced the same Cache family")
	}
}

func TestDefaultCachePlannerFallsBackSafely(t *testing.T) {
	t.Parallel()

	compiled, err := (agent.DefaultContextCompiler{}).Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: []agent.Message{{Role: agent.RoleUser, Text: "short"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := agent.CachePlanRequest{
		RunID:        "run-1",
		Provider:     "automatic-only",
		ModelRequest: compiled.ModelRequest,
		Segments:     compiled.Segments,
		Capabilities: agent.CacheCapabilities{SupportsAutomatic: true, ExactPrefix: true},
		Policy:       agent.CachePolicy{Mode: agent.CacheModeExplicit},
	}
	plan, err := (agent.DefaultCachePlanner{}).Plan(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != agent.CacheModeAutomatic || plan.FallbackReason != "explicit_cache_unsupported" {
		t.Fatalf("fallback Cache Plan = %#v", plan)
	}
	request.Policy.Required = true
	if _, err := (agent.DefaultCachePlanner{}).Plan(context.Background(), request); err == nil {
		t.Fatal("required unsupported explicit cache did not fail")
	}
}

func TestDefaultCachePlannerClearsDisabledFallbackControls(t *testing.T) {
	t.Parallel()

	compiled, err := (agent.DefaultContextCompiler{}).Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: []agent.Message{{Role: agent.RoleUser, Text: "short"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := (agent.DefaultCachePlanner{}).Plan(context.Background(), agent.CachePlanRequest{
		RunID:        "run-1",
		Provider:     "explicit-only",
		ModelRequest: compiled.ModelRequest,
		Segments:     compiled.Segments,
		Capabilities: agent.CacheCapabilities{
			SupportsExplicit:    true,
			MaxWriteBreakpoints: 1,
			MinimumPrefixTokens: 1024,
			SupportedTTLs:       []agent.CacheTTL{agent.CacheTTLFiveMinutes},
		},
		Policy: agent.CachePolicy{
			Mode: agent.CacheModeExplicit,
			TTL:  agent.CacheTTLFiveMinutes,
			Pricing: &agent.CachePricing{
				Currency:                      "USD",
				UncachedInputMicrosPerMillion: 2_000_000,
				CacheReadMicrosPerMillion:     200_000,
				CacheWriteMicrosPerMillion:    2_500_000,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != agent.CacheModeDisabled || plan.FamilyID != "" || plan.TTL != "" ||
		len(plan.Breakpoints) != 0 || plan.Forecast != nil || plan.FallbackReason != "no_eligible_cache_breakpoint" {
		t.Fatalf("disabled fallback Cache Plan = %#v", plan)
	}
}

func TestDefaultCachePolicyDoesNotOptIntoProviderWrites(t *testing.T) {
	t.Parallel()

	compiled, err := (agent.DefaultContextCompiler{}).Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: []agent.Message{{Role: agent.RoleUser, Text: strings.Repeat("x", 5000)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := (agent.DefaultCachePlanner{}).Plan(context.Background(), agent.CachePlanRequest{
		RunID:        "run-default",
		Provider:     "cache-capable",
		ModelRequest: compiled.ModelRequest,
		Segments:     compiled.Segments,
		Capabilities: agent.CacheCapabilities{
			SupportsExplicit:    true,
			SupportsAutomatic:   true,
			MaxWriteBreakpoints: 4,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != agent.CacheModeDisabled || plan.FamilyID != "" || len(plan.Breakpoints) != 0 {
		t.Fatalf("zero policy Cache Plan = %#v", plan)
	}
}

func TestEstimateUsageCostUsesCacheCategories(t *testing.T) {
	t.Parallel()

	cost, err := agent.EstimateUsageCost(agent.CachePricing{
		Currency:                      "USD",
		UncachedInputMicrosPerMillion: 2_000_000,
		CacheReadMicrosPerMillion:     200_000,
		CacheWriteMicrosPerMillion:    2_500_000,
		OutputMicrosPerMillion:        10_000_000,
	}, agent.Usage{
		InputTokens:               3_000_000,
		OutputTokens:              1_000_000,
		TotalTokens:               4_000_000,
		InputTokenDetailsReported: true,
		UncachedInputTokens:       1_000_000,
		CacheReadInputTokens:      1_000_000,
		CacheWriteInputTokens:     1_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cost.InputMicros != 4_700_000 || cost.OutputMicros != 10_000_000 || cost.TotalMicros != 14_700_000 {
		t.Fatalf("Usage Cost = %#v", cost)
	}
}
