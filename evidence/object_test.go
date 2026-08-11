package evidence_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
