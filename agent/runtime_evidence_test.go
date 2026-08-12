package agent_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/session"
)

func TestRuntimeDerivesStableSessionAndEphemeralEvidenceScopes(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, Text: "first"}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "second"}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "third"}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "fourth"}},
	}}
	var mu sync.Mutex
	var observed []agent.EvidenceAccess
	compiler := contextCompilerFunc(func(ctx context.Context, request agent.ContextCompileRequest) (agent.CompiledContext, error) {
		if request.EvidenceAccess == nil {
			t.Fatal("Runtime omitted EvidenceAccess")
		}
		fromContext, ok := agent.EvidenceAccessFromContext(ctx)
		if !ok || fromContext.Scope != request.EvidenceAccess.Scope {
			t.Fatalf("context Evidence access = %#v / %#v", fromContext, request.EvidenceAccess)
		}
		mu.Lock()
		observed = append(observed, *request.EvidenceAccess)
		mu.Unlock()
		return (agent.DefaultContextCompiler{}).Compile(ctx, request)
	})
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:        provider,
		SessionStore:    session.NewMemoryStore(),
		ContextCompiler: compiler,
		EvidenceAccess: &agent.RuntimeEvidenceAccess{
			TenantID:     "tenant",
			ProfileID:    "configured-profile",
			PrincipalID:  "runtime",
			Capabilities: []string{agent.EvidenceReadCapability, agent.EvidenceWriteCapability},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	run := func(sessionID string) agent.RunResult {
		handle, err := runtime.Run(context.Background(), agent.RunRequest{
			AgentID: "request-agent", SessionID: sessionID,
			Input: []agent.Message{{Role: agent.RoleUser, Text: "continue"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, result, err := collectRun(handle)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := run("session")
	second := run("session")
	third := run("")
	fourth := run("")
	mu.Lock()
	accesses := append([]agent.EvidenceAccess(nil), observed...)
	mu.Unlock()
	if len(accesses) != 4 {
		t.Fatalf("observed Evidence access = %#v", accesses)
	}
	if accesses[0].Scope.SessionID != "session" || accesses[1].Scope.SessionID != "session" ||
		accesses[0].Scope.RunID != "" || accesses[0].Scope != accesses[1].Scope {
		t.Fatalf("Session scopes = %#v / %#v", accesses[0].Scope, accesses[1].Scope)
	}
	if accesses[0].Scope.ProfileID != "configured-profile" || accesses[0].Scope.TenantID != "tenant" {
		t.Fatalf("configured scope = %#v", accesses[0].Scope)
	}
	if accesses[2].Scope.RunID != third.RunID || accesses[3].Scope.RunID != fourth.RunID ||
		third.RunID == fourth.RunID || first.RunID == second.RunID {
		t.Fatalf("ephemeral scopes/results = %#v / %#v / %#v / %#v", accesses[2], accesses[3], third, fourth)
	}
}

func TestRuntimeEvidenceAccessConstrainsInheritedTenantAndCapabilities(t *testing.T) {
	t.Parallel()

	base := &agent.RuntimeEvidenceAccess{
		TenantID:     "tenant",
		ProfileID:    "profile",
		PrincipalID:  "runtime",
		Capabilities: []string{agent.EvidenceReadCapability, agent.EvidenceWriteCapability},
	}
	var observed agent.EvidenceAccess
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: &scriptedProvider{responses: []providerResponse{{
			message: agent.Message{Role: agent.RoleAssistant, Text: "done"},
		}}},
		EvidenceAccess: base,
		ContextCompiler: contextCompilerFunc(func(ctx context.Context, request agent.ContextCompileRequest) (agent.CompiledContext, error) {
			observed = *request.EvidenceAccess
			return (agent.DefaultContextCompiler{}).Compile(ctx, request)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	inherited := agent.EvidenceAccess{
		Scope:        agent.EvidenceScope{TenantID: "tenant", RunID: "parent", ProfileID: "parent-profile"},
		PrincipalID:  "parent",
		Capabilities: []string{agent.EvidenceReadCapability},
	}
	ctx := agent.WithEvidenceTenant(context.Background(), "tenant")
	ctx = agent.WithEvidenceAccess(ctx, inherited)
	handle, err := runtime.Run(ctx, agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = collectRun(handle)
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.Capabilities) != 1 || observed.Capabilities[0] != agent.EvidenceReadCapability ||
		observed.PrincipalID != "runtime" || observed.Scope.ProfileID != "profile" || observed.Scope.RunID == "parent" {
		t.Fatalf("constrained Evidence access = %#v", observed)
	}

	wrongTenant := inherited
	wrongTenant.Scope.TenantID = "other-tenant"
	ctx = agent.WithEvidenceTenant(context.Background(), "other-tenant")
	ctx = agent.WithEvidenceAccess(ctx, wrongTenant)
	handle, err = runtime.Run(ctx, agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = collectRun(handle)
	if !errors.Is(err, agent.ErrEvidenceAccessDenied) {
		t.Fatalf("tenant mismatch error = %v", err)
	}
}

func TestRuntimeEvidenceAccessUsesAuthenticatedContextTenant(t *testing.T) {
	t.Parallel()

	var observed agent.EvidenceAccess
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: &scriptedProvider{responses: []providerResponse{{
			message: agent.Message{Role: agent.RoleAssistant, Text: "done"},
		}}},
		EvidenceAccess: &agent.RuntimeEvidenceAccess{
			ProfileID:    "profile",
			PrincipalID:  "runtime",
			Capabilities: []string{agent.EvidenceReadCapability, agent.EvidenceWriteCapability},
		},
		ContextCompiler: contextCompilerFunc(func(ctx context.Context, request agent.ContextCompileRequest) (agent.CompiledContext, error) {
			observed = *request.EvidenceAccess
			return (agent.DefaultContextCompiler{}).Compile(ctx, request)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := agent.WithEvidenceTenant(context.Background(), "authenticated-tenant")
	handle, err := runtime.Run(ctx, agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := collectRun(handle)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Scope.TenantID != "authenticated-tenant" || observed.Scope.RunID != result.RunID ||
		observed.Scope.ProfileID != "profile" {
		t.Fatalf("context tenant Evidence access = %#v", observed)
	}

	handle, err = runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = collectRun(handle)
	if err == nil || !strings.Contains(err.Error(), "tenant ID") {
		t.Fatalf("missing authenticated tenant error = %v", err)
	}
}

func TestRuntimeRejectsInvalidEvidenceAccessConfiguration(t *testing.T) {
	t.Parallel()

	_, err := agent.NewRuntime(agent.Options{
		Provider: &scriptedProvider{},
		EvidenceAccess: &agent.RuntimeEvidenceAccess{
			TenantID: "tenant", ProfileID: "profile", PrincipalID: "runtime",
		},
	})
	if err == nil {
		t.Fatal("NewRuntime accepted Evidence access without capabilities")
	}
}
