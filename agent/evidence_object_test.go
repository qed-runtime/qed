package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
)

func TestEvidenceObjectScopeBindsTenantSessionProfileAndCapability(t *testing.T) {
	t.Parallel()

	access := scopedEvidenceAccess("tenant-a", "session-a", "profile-a")
	reference, err := agent.BindEvidenceObjectReference(
		agent.EvidenceObjectRef{
			Digest: "sha256:" + strings.Repeat("a", 64), Bytes: 7, MediaType: "text/plain",
		},
		access,
		[]string{agent.EvidenceReadCapability},
		agent.EvidenceSensitivityPrivate,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reference.Scope == nil || reference.Identity() != reference.Scope.BindingDigest {
		t.Fatalf("scoped reference = %#v", reference)
	}
	if err := agent.ValidateEvidenceObjectRef(reference); err != nil {
		t.Fatal(err)
	}
	if err := agent.AuthorizeEvidenceObjectAccess(reference, access); err != nil {
		t.Fatalf("authorized access error = %v", err)
	}

	encoded, err := json.Marshal(reference)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"tenant-a", "session-a", "profile-a", "principal-a"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("scoped reference exposed %q: %s", private, encoded)
		}
	}

	tests := map[string]agent.EvidenceAccess{
		"tenant":     scopedEvidenceAccess("tenant-b", "session-a", "profile-a"),
		"Session":    scopedEvidenceAccess("tenant-a", "session-b", "profile-a"),
		"Profile":    scopedEvidenceAccess("tenant-a", "session-a", "profile-b"),
		"capability": {Scope: access.Scope, PrincipalID: "principal-a"},
	}
	for name, denied := range tests {
		denied := denied
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := agent.AuthorizeEvidenceObjectAccess(reference, denied); !errors.Is(err, agent.ErrEvidenceAccessDenied) {
				t.Fatalf("AuthorizeEvidenceObjectAccess() error = %v", err)
			}
		})
	}
}

func TestEvidenceObjectScopeUsesRunOnlyForEphemeralEvidence(t *testing.T) {
	t.Parallel()

	access := agent.EvidenceAccess{
		Scope:        agent.EvidenceScope{TenantID: "tenant", RunID: "run-one", ProfileID: "profile"},
		PrincipalID:  "principal",
		Capabilities: []string{agent.EvidenceReadCapability, agent.EvidenceWriteCapability},
	}
	reference, err := agent.BindEvidenceObjectReference(
		agent.EvidenceObjectRef{
			Digest: "sha256:" + strings.Repeat("b", 64), Bytes: 4, MediaType: "text/plain",
		},
		access,
		[]string{agent.EvidenceReadCapability},
		agent.EvidenceSensitivityPrivate,
	)
	if err != nil {
		t.Fatal(err)
	}
	other := access
	other.Scope.RunID = "run-two"
	if err := agent.AuthorizeEvidenceObjectAccess(reference, other); !errors.Is(err, agent.ErrEvidenceAccessDenied) {
		t.Fatalf("ephemeral cross-Run access error = %v", err)
	}
}

func TestEvidenceObjectScopeRejectsTamperedBindingMetadata(t *testing.T) {
	t.Parallel()

	access := scopedEvidenceAccess("tenant", "session", "profile")
	reference, err := agent.BindEvidenceObjectReference(
		agent.EvidenceObjectRef{
			Digest: "sha256:" + strings.Repeat("c", 64), Bytes: 4, MediaType: "text/plain",
		},
		access,
		[]string{agent.EvidenceReadCapability},
		agent.EvidenceSensitivityPrivate,
	)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*agent.EvidenceObjectRef){
		"size":       func(value *agent.EvidenceObjectRef) { value.Bytes++ },
		"media type": func(value *agent.EvidenceObjectRef) { value.MediaType = "application/json" },
		"scope": func(value *agent.EvidenceObjectRef) {
			value.Scope.ScopeDigest = "sha256:" + strings.Repeat("d", 64)
		},
		"capability": func(value *agent.EvidenceObjectRef) {
			value.Scope.RequiredCapabilities = []string{agent.EvidenceWriteCapability}
		},
		"sensitivity": func(value *agent.EvidenceObjectRef) {
			value.Scope.Sensitivity = agent.EvidenceSensitivitySecret
		},
	}
	for name, tamper := range tests {
		tamper := tamper
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			changed := reference
			scope := *reference.Scope
			scope.RequiredCapabilities = append([]string(nil), reference.Scope.RequiredCapabilities...)
			changed.Scope = &scope
			tamper(&changed)
			if err := agent.ValidateEvidenceObjectRef(changed); err == nil {
				t.Fatalf("ValidateEvidenceObjectRef() accepted %#v", changed)
			}
		})
	}
}

func TestEvidenceAccessContextAndAuditRecordDoNotAliasOrExposeIdentity(t *testing.T) {
	t.Parallel()

	access := scopedEvidenceAccess("tenant-private", "session-private", "profile-private")
	ctx := agent.WithEvidenceAccess(context.Background(), access)
	access.Capabilities[0] = "changed"
	loaded, ok := agent.EvidenceAccessFromContext(ctx)
	if !ok || loaded.Capabilities[0] != agent.EvidenceReadCapability {
		t.Fatalf("EvidenceAccessFromContext() = %#v / %t", loaded, ok)
	}
	loaded.Capabilities[0] = "caller-mutated"
	again, ok := agent.EvidenceAccessFromContext(ctx)
	if !ok || again.Capabilities[0] != agent.EvidenceReadCapability {
		t.Fatal("Evidence access context aliases caller memory")
	}

	reference, err := agent.BindEvidenceObjectReference(
		agent.EvidenceObjectRef{
			Digest: "sha256:" + strings.Repeat("e", 64), Bytes: 5, MediaType: "text/plain",
		},
		again,
		[]string{agent.EvidenceReadCapability},
		agent.EvidenceSensitivityPrivate,
	)
	if err != nil {
		t.Fatal(err)
	}
	record, err := agent.NewEvidenceObjectAccessRecord(
		time.Unix(123, 0),
		agent.EvidenceObjectAccessGet,
		agent.EvidenceObjectAccessAllowed,
		reference,
		again,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.ValidateEvidenceObjectAccessRecord(record); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"tenant-private", "session-private", "profile-private", "principal-a"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("audit record exposed %q: %s", private, encoded)
		}
	}
}

func scopedEvidenceAccess(tenantID, sessionID, profileID string) agent.EvidenceAccess {
	return agent.EvidenceAccess{
		Scope: agent.EvidenceScope{
			TenantID: tenantID, SessionID: sessionID, ProfileID: profileID,
		},
		PrincipalID:  "principal-a",
		Capabilities: []string{agent.EvidenceReadCapability, agent.EvidenceWriteCapability},
	}
}
