package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/evidence"
)

func TestCompactingContextCompilerExternalizesScopedEvidence(t *testing.T) {
	t.Parallel()

	objects := evidence.NewMemoryObjectStore()
	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          4096,
		RecentMessages:         2,
		EvidenceThresholdBytes: 1000,
		EvidenceExcerptBytes:   100,
		CheckpointMaxBytes:     2048,
	}, objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	access := contextEvidenceAccess("tenant", "session", "profile")
	toolOutput := strings.Repeat("protected-output", 200)
	compiled, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{SessionID: "session", Messages: []agent.Message{
			{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "command"}}},
			{Role: agent.RoleTool, ToolCallID: "call-1", ToolName: "command", Text: toolOutput},
			{Role: agent.RoleUser, Text: "continue"},
		}},
		EvidenceAccess: &access,
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Compaction == nil || len(compiled.Compaction.Externalized) != 1 ||
		compiled.Compaction.Externalized[0].Scope == nil {
		t.Fatalf("scoped compaction = %#v", compiled.Compaction)
	}
	reference := compiled.Compaction.Externalized[0]
	if _, err := objects.GetObject(context.Background(), reference); !errors.Is(err, agent.ErrEvidenceScopeRequired) {
		t.Fatalf("legacy GetObject() error = %v", err)
	}
	content, err := objects.GetObjectScoped(context.Background(), agent.EvidenceObjectGetRequest{
		Access: access, Reference: reference,
	})
	if err != nil || string(content) != toolOutput {
		t.Fatalf("GetObjectScoped() = %q, %v", content, err)
	}
	other := access
	other.Scope.SessionID = "other-session"
	if _, err := objects.GetObjectScoped(context.Background(), agent.EvidenceObjectGetRequest{
		Access: other, Reference: reference,
	}); !errors.Is(err, agent.ErrEvidenceAccessDenied) {
		t.Fatalf("cross-Session GetObjectScoped() error = %v", err)
	}
}

func TestCompactingContextCompilerRejectsCheckpointFromAnotherScope(t *testing.T) {
	t.Parallel()

	objects := evidence.NewMemoryObjectStore()
	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          3000,
		RecentMessages:         2,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     1800,
	}, objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	messages := make([]agent.Message, 0, 12)
	for index := 0; index < 6; index++ {
		messages = append(messages,
			agent.Message{Role: agent.RoleUser, Text: strings.Repeat(string(rune('a'+index)), 600)},
			agent.Message{Role: agent.RoleAssistant, Text: strings.Repeat(string(rune('A'+index)), 600)},
		)
	}
	access := contextEvidenceAccess("tenant", "session-one", "profile")
	request := agent.ContextCompileRequest{
		ModelRequest:   agent.ModelRequest{SessionID: "session-one", Messages: messages},
		EvidenceAccess: &access,
	}
	compiled, err := compiler.Compile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Checkpoint == nil || len(compiled.Checkpoint.Evidence) == 0 ||
		compiled.Checkpoint.Evidence[0].Scope == nil {
		t.Fatalf("scoped Checkpoint = %#v", compiled.Checkpoint)
	}
	other := access
	other.Scope.SessionID = "session-two"
	request.Checkpoint = compiled.Checkpoint
	request.EvidenceAccess = &other
	if _, err := compiler.Compile(context.Background(), request); !errors.Is(err, agent.ErrEvidenceAccessDenied) {
		t.Fatalf("cross-scope Checkpoint Compile() error = %v", err)
	}
}

func TestCompactingContextCompilerRejectsSecretOnBuiltInStore(t *testing.T) {
	t.Parallel()

	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          4096,
		RecentMessages:         2,
		EvidenceThresholdBytes: 10,
		EvidenceExcerptBytes:   2,
		CheckpointMaxBytes:     1024,
	}, evidence.NewMemoryObjectStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	access := contextEvidenceAccess("tenant", "session", "profile")
	_, err = compiler.Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: []agent.Message{
			{Role: agent.RoleTool, ToolCallID: "call", ToolName: "command", Text: "secret content"},
		}},
		EvidenceAccess:      &access,
		EvidenceSensitivity: agent.EvidenceSensitivitySecret,
	})
	if !errors.Is(err, agent.ErrSecretEvidenceRejected) {
		t.Fatalf("secret Compile() error = %v", err)
	}
}

func contextEvidenceAccess(tenantID, sessionID, profileID string) agent.EvidenceAccess {
	return agent.EvidenceAccess{
		Scope: agent.EvidenceScope{
			TenantID: tenantID, SessionID: sessionID, ProfileID: profileID,
		},
		PrincipalID:  "runtime",
		Capabilities: []string{agent.EvidenceReadCapability, agent.EvidenceWriteCapability},
	}
}
