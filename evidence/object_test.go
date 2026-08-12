package evidence_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/evidence"
)

func TestObjectStoresRoundTripContentByDigest(t *testing.T) {
	t.Parallel()

	stores := map[string]func(*testing.T) agent.EvidenceObjectStore{
		"memory": func(*testing.T) agent.EvidenceObjectStore {
			return evidence.NewMemoryObjectStore()
		},
		"json": func(t *testing.T) agent.EvidenceObjectStore {
			store, err := evidence.NewJSONStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return store
		},
	}
	for name, construct := range stores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := construct(t)
			content := []byte("complete tool output\n")
			first, err := store.PutObject(context.Background(), "text/plain; charset=utf-8", content)
			if err != nil {
				t.Fatal(err)
			}
			second, err := store.PutObject(context.Background(), "text/plain; charset=utf-8", content)
			if err != nil {
				t.Fatal(err)
			}
			if first != second || first.Bytes != int64(len(content)) {
				t.Fatalf("Object refs = %#v / %#v", first, second)
			}
			loaded, err := store.GetObject(context.Background(), first)
			if err != nil {
				t.Fatal(err)
			}
			loaded[0] = 'X'
			again, err := store.GetObject(context.Background(), first)
			if err != nil {
				t.Fatal(err)
			}
			if string(again) != string(content) {
				t.Fatalf("loaded Object = %q", again)
			}
		})
	}
}

func TestJSONStoreRejectsCorruptObject(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := evidence.NewJSONStore(root)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := store.PutObject(context.Background(), "text/plain", []byte("original"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("Object entries = %d, want 1", len(entries))
	}
	if err := os.WriteFile(filepath.Join(root, "objects", entries[0].Name()), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = store.GetObject(context.Background(), reference)
	if !errors.Is(err, evidence.ErrObjectCorrupt) {
		t.Fatalf("GetObject error = %v", err)
	}
}

func TestObjectStoreRejectsInvalidMediaType(t *testing.T) {
	t.Parallel()

	store := evidence.NewMemoryObjectStore()
	if _, err := store.PutObject(context.Background(), "text/plain\ninjected: value", []byte("content")); err == nil {
		t.Fatal("PutObject accepted an invalid media type")
	}
}

func TestScopedObjectStoresEnforceIsolationCapabilitiesAndAudit(t *testing.T) {
	t.Parallel()

	stores := map[string]func(*testing.T) agent.ScopedEvidenceObjectStore{
		"memory": func(*testing.T) agent.ScopedEvidenceObjectStore {
			return evidence.NewMemoryObjectStore()
		},
		"json": func(t *testing.T) agent.ScopedEvidenceObjectStore {
			store, err := evidence.NewJSONStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return store
		},
	}
	for name, construct := range stores {
		name, construct := name, construct
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := construct(t)
			access := scopedStoreAccess("tenant-a", "session-a", "profile-a")
			content := []byte("protected tool output\n")
			reference, err := store.PutObjectScoped(context.Background(), agent.EvidenceObjectPutRequest{
				Access:               access,
				MediaType:            "text/plain; charset=utf-8",
				Content:              content,
				RequiredCapabilities: []string{agent.EvidenceReadCapability},
				Sensitivity:          agent.EvidenceSensitivityPrivate,
			})
			if err != nil {
				t.Fatal(err)
			}
			if reference.Scope == nil || reference.Identity() == reference.Digest {
				t.Fatalf("scoped reference = %#v", reference)
			}

			loaded, err := store.GetObjectScoped(context.Background(), agent.EvidenceObjectGetRequest{
				Access: access, Reference: reference,
			})
			if err != nil || string(loaded) != string(content) {
				t.Fatalf("GetObjectScoped() = %q, %v", loaded, err)
			}
			if _, err := store.GetObject(context.Background(), reference); !errors.Is(err, agent.ErrEvidenceScopeRequired) {
				t.Fatalf("legacy GetObject() error = %v", err)
			}

			wrongTenant := access
			wrongTenant.Scope.TenantID = "tenant-b"
			if _, err := store.GetObjectScoped(context.Background(), agent.EvidenceObjectGetRequest{
				Access: wrongTenant, Reference: reference,
			}); !errors.Is(err, agent.ErrEvidenceAccessDenied) {
				t.Fatalf("cross-tenant GetObjectScoped() error = %v", err)
			}
			withoutRead := access
			withoutRead.Capabilities = []string{agent.EvidenceWriteCapability}
			if _, err := store.GetObjectScoped(context.Background(), agent.EvidenceObjectGetRequest{
				Access: withoutRead, Reference: reference,
			}); !errors.Is(err, agent.ErrEvidenceAccessDenied) {
				t.Fatalf("capability GetObjectScoped() error = %v", err)
			}

			log := store.(agent.EvidenceObjectAccessLog)
			records, err := log.EvidenceObjectAccessRecords(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 4 {
				t.Fatalf("access records = %#v", records)
			}
			wantOutcomes := []agent.EvidenceObjectAccessOutcome{
				agent.EvidenceObjectAccessAllowed,
				agent.EvidenceObjectAccessAllowed,
				agent.EvidenceObjectAccessDenied,
				agent.EvidenceObjectAccessDenied,
			}
			for index, want := range wantOutcomes {
				if records[index].Outcome != want || records[index].BindingDigest != reference.Identity() {
					t.Fatalf("access record %d = %#v, want outcome %q", index, records[index], want)
				}
			}
			if records[2].ScopeDigest == records[2].AccessScopeDigest {
				t.Fatalf("cross-tenant audit did not preserve both scope digests: %#v", records[2])
			}
			if records[3].ScopeDigest != records[3].AccessScopeDigest {
				t.Fatalf("capability audit changed scope digest: %#v", records[3])
			}
		})
	}
}

func TestScopedObjectStoresSeparateScopeAndRejectSecretContent(t *testing.T) {
	t.Parallel()

	stores := map[string]agent.ScopedEvidenceObjectStore{
		"memory": evidence.NewMemoryObjectStore(),
	}
	jsonStore, err := evidence.NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stores["json"] = jsonStore
	for name, store := range stores {
		name, store := name, store
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			firstAccess := scopedStoreAccess("tenant", "session-one", "profile")
			secondAccess := scopedStoreAccess("tenant", "session-two", "profile")
			put := func(access agent.EvidenceAccess, sensitivity agent.EvidenceSensitivity) (agent.EvidenceObjectRef, error) {
				return store.PutObjectScoped(context.Background(), agent.EvidenceObjectPutRequest{
					Access:               access,
					MediaType:            "text/plain",
					Content:              []byte("same content"),
					RequiredCapabilities: []string{agent.EvidenceReadCapability},
					Sensitivity:          sensitivity,
				})
			}
			first, err := put(firstAccess, agent.EvidenceSensitivityPrivate)
			if err != nil {
				t.Fatal(err)
			}
			second, err := put(secondAccess, agent.EvidenceSensitivityPrivate)
			if err != nil {
				t.Fatal(err)
			}
			if first.Digest != second.Digest || first.Identity() == second.Identity() {
				t.Fatalf("scope identities = %#v / %#v", first, second)
			}
			if _, err := put(firstAccess, agent.EvidenceSensitivitySecret); !errors.Is(err, agent.ErrSecretEvidenceRejected) {
				t.Fatalf("secret PutObjectScoped() error = %v", err)
			}

			withoutWrite := firstAccess
			withoutWrite.Capabilities = []string{agent.EvidenceReadCapability}
			if _, err := put(withoutWrite, agent.EvidenceSensitivityPrivate); !errors.Is(err, agent.ErrEvidenceAccessDenied) {
				t.Fatalf("write capability error = %v", err)
			}
		})
	}
}

func TestJSONScopedObjectAdminReadIsAuditedWithoutRawIdentity(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := evidence.NewJSONStore(root)
	if err != nil {
		t.Fatal(err)
	}
	access := scopedStoreAccess("private-tenant", "private-session", "private-profile")
	reference, err := store.PutObjectScoped(context.Background(), agent.EvidenceObjectPutRequest{
		Access:               access,
		MediaType:            "application/json",
		Content:              []byte(`{"result":"ok"}`),
		RequiredCapabilities: []string{agent.EvidenceReadCapability},
		Sensitivity:          agent.EvidenceSensitivityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	content, err := store.GetObjectAdmin(context.Background(), reference, "private-operator")
	if err != nil || string(content) != `{"result":"ok"}` {
		t.Fatalf("GetObjectAdmin() = %q, %v", content, err)
	}
	records, err := store.EvidenceObjectAccessRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[1].Operation != agent.EvidenceObjectAccessAdminGet ||
		records[1].Outcome != agent.EvidenceObjectAccessAllowed {
		t.Fatalf("access records = %#v", records)
	}
	encoded, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-tenant", "private-session", "private-profile", "private-operator"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("audit records exposed %q: %s", private, encoded)
		}
	}
	info, err := os.Stat(filepath.Join(root, "object-access.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("audit log mode = %o", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Join(root, "scoped-objects"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || strings.Contains(entries[0].Name(), strings.TrimPrefix(reference.Digest, "sha256:")) {
		t.Fatalf("scoped object entries = %#v", entries)
	}
}

func TestJSONScopedRetrievalFailsClosedWhenAuditCannotBeWritten(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := evidence.NewJSONStore(root)
	if err != nil {
		t.Fatal(err)
	}
	access := scopedStoreAccess("tenant", "session", "profile")
	reference, err := store.PutObjectScoped(context.Background(), agent.EvidenceObjectPutRequest{
		Access:               access,
		MediaType:            "text/plain",
		Content:              []byte("must not escape without audit"),
		RequiredCapabilities: []string{agent.EvidenceReadCapability},
		Sensitivity:          agent.EvidenceSensitivityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "object-access.jsonl")
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(logPath, 0o700); err != nil {
		t.Fatal(err)
	}
	content, err := store.GetObjectScoped(context.Background(), agent.EvidenceObjectGetRequest{
		Access: access, Reference: reference,
	})
	if err == nil || content != nil || !strings.Contains(err.Error(), "record Evidence Object access") {
		t.Fatalf("fail-closed GetObjectScoped() = %q, %v", content, err)
	}
}

func TestJSONScopedAccessRejectsSymlinkAuditLog(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := evidence.NewJSONStore(root)
	if err != nil {
		t.Fatal(err)
	}
	targetName := "audit-target"
	targetPath := filepath.Join(root, targetName)
	if err := os.WriteFile(targetPath, []byte("protected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetName, filepath.Join(root, "object-access.jsonl")); err != nil {
		t.Skipf("create audit symlink: %v", err)
	}
	access := scopedStoreAccess("tenant", "session", "profile")
	_, err = store.PutObjectScoped(context.Background(), agent.EvidenceObjectPutRequest{
		Access:               access,
		MediaType:            "text/plain",
		Content:              []byte("content"),
		RequiredCapabilities: []string{agent.EvidenceReadCapability},
		Sensitivity:          agent.EvidenceSensitivityPrivate,
	})
	if err == nil || !strings.Contains(err.Error(), "real regular file") {
		t.Fatalf("symlink audit PutObjectScoped() error = %v", err)
	}
	if _, err := store.EvidenceObjectAccessRecords(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "real regular file") {
		t.Fatalf("symlink audit EvidenceObjectAccessRecords() error = %v", err)
	}
	content, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "protected" {
		t.Fatalf("audit symlink target = %q", content)
	}
}

func scopedStoreAccess(tenantID, sessionID, profileID string) agent.EvidenceAccess {
	return agent.EvidenceAccess{
		Scope: agent.EvidenceScope{
			TenantID: tenantID, SessionID: sessionID, ProfileID: profileID,
		},
		PrincipalID:  "runtime-principal",
		Capabilities: []string{agent.EvidenceReadCapability, agent.EvidenceWriteCapability},
	}
}
