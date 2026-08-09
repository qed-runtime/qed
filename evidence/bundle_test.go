package evidence_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/evidence"
)

func TestBundleClassifiesRunEvidenceWithoutProviderState(t *testing.T) {
	t.Parallel()

	message := agent.Message{
		Role:  agent.RoleAssistant,
		Text:  "done",
		Model: "test-model",
		ProviderState: &agent.ProviderState{
			Provider: "test",
			Data:     json.RawMessage(`{"private":"opaque-secret"}`),
		},
	}
	bundle, err := evidence.NewBundle(agent.RunResult{
		RunID:   "run-1",
		AgentID: "coding",
		Status:  agent.RunStatusCompleted,
		Messages: []agent.Message{
			{Role: agent.RoleUser, Text: "fix it"},
			message,
		},
		Usage: agent.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5},
	}, evidence.BundleOptions{
		Events: []agent.Event{{Type: agent.EventMessageCompleted, RunID: "run-1", Message: &message}},
		ToolInvocations: []evidence.ToolInvocation{
			{CallID: "command-1", Tool: "run_command", ArgumentsDigest: "sha256:a", OutputDigest: "sha256:b", PolicyOutcome: "allow"},
			{CallID: "change-1", Tool: "apply_patch", ArgumentsDigest: "sha256:c", OutputDigest: "sha256:d", PolicyOutcome: "allow"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Model.Name != "test-model" || len(bundle.Commands) != 1 || len(bundle.Checks) != 1 || len(bundle.Changes) != 1 {
		t.Fatalf("Bundle = %#v", bundle)
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "opaque-secret") {
		t.Fatalf("Bundle exposed ProviderState: %s", encoded)
	}
}

func TestJSONStoreRoundTripListAndPrivatePermissions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := evidence.NewJSONStore(root)
	if err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("Evidence root mode = %o", rootInfo.Mode().Perm())
	}
	bundle := evidence.Bundle{
		Version:   1,
		Run:       evidence.RunDescriptor{ID: "run/with/path", Status: agent.RunStatusCompleted},
		Events:    []agent.Event{},
		CreatedAt: time.Now().UTC(),
	}
	if err := store.Save(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), bundle.Run.ID)
	if err != nil || loaded.Run.ID != bundle.Run.ID {
		t.Fatalf("Load = %#v, %v", loaded, err)
	}
	descriptors, err := store.List(context.Background())
	if err != nil || len(descriptors) != 1 || descriptors[0].ID != bundle.Run.ID {
		t.Fatalf("List = %#v, %v", descriptors, err)
	}
	entries, err := os.ReadDir(store.Root())
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir = %#v, %v", entries, err)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("Bundle mode = %o", info.Mode().Perm())
	}
	if _, err := store.Load(context.Background(), "missing"); !errors.Is(err, evidence.ErrBundleNotFound) {
		t.Fatalf("missing Load error = %v", err)
	}
}
