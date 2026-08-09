package extension_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qed-runtime/qed/extension"
)

func TestMemoryStateStoreIsolatesNamespacesAndValues(t *testing.T) {
	t.Parallel()

	store := extension.NewMemoryStateStore()
	value := []byte("first")
	if err := store.Set(context.Background(), "extension-a", "session-a", "key", value); err != nil {
		t.Fatal(err)
	}
	value[0] = 'X'
	actual, err := store.Get(context.Background(), "extension-a", "session-a", "key")
	if err != nil || string(actual) != "first" {
		t.Fatalf("Get() = %q, %v", actual, err)
	}
	actual[0] = 'Y'
	again, err := store.Get(context.Background(), "extension-a", "session-a", "key")
	if err != nil || string(again) != "first" {
		t.Fatalf("second Get() = %q, %v", again, err)
	}
	if _, err := store.Get(context.Background(), "extension-b", "session-a", "key"); !errors.Is(err, extension.ErrStateNotFound) {
		t.Fatalf("other namespace error = %v", err)
	}
}

func TestJSONStateStoreRoundTripAndPrivatePermissions(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "state")
	store, err := extension.NewJSONStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), "extension-a", "workspace:one", "snapshot", []byte(`{"value":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), "extension-a", "workspace:one", "snapshot", []byte(`{"value":2}`)); err != nil {
		t.Fatal(err)
	}
	actual, err := store.Get(context.Background(), "extension-a", "workspace:one", "snapshot")
	if err != nil || string(actual) != `{"value":2}` {
		t.Fatalf("Get() = %s, %v", actual, err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("root permissions = %v", rootInfo.Mode().Perm())
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("state files = %d", len(entries))
	}
	fileInfo, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("file permissions = %v", fileInfo.Mode().Perm())
	}
}
