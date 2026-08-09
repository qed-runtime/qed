package workspace_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/workspace"
)

func TestWorkspaceConstrainsPathsAndSymlinks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	file := filepath.Join(root, "source.go")
	if err := os.WriteFile(file, []byte("package sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scoped, err := workspace.New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	resolved, err := scoped.ResolveFile("source.go")
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err != nil || resolved != filepath.Join(canonicalRoot, "source.go") {
		t.Fatalf("ResolveFile() = %q, %v", resolved, err)
	}
	for _, path := range []string{"../outside", file} {
		if _, err := scoped.Resolve(path); err == nil {
			t.Errorf("Resolve(%q) error = nil", path)
		}
	}

	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := scoped.ResolveFile("link.txt"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("ResolveFile(symlink) error = %v", err)
	}
}

func TestWorkspaceResolvesNewTargetsThroughExistingParents(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	scoped, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	resolved, exists, err := scoped.ResolveTarget("nested/new.go")
	if err != nil || exists || resolved != filepath.Join(canonicalRoot, "nested", "new.go") {
		t.Fatalf("ResolveTarget() = %q, %t, %v", resolved, exists, err)
	}
	if _, _, err := scoped.ResolveTarget("missing/new.go"); err == nil {
		t.Error("ResolveTarget(missing parent) error = nil")
	}
}

func TestWorkspaceRootOperationsPreventSymlinkEscape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outsideRoot := t.TempDir()
	outsidePath := filepath.Join(outsideRoot, "outside.txt")
	if err := os.WriteFile(outsidePath, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideRoot, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	scoped, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.Open("escape/outside.txt"); err == nil {
		t.Fatal("Open() followed a symlink outside the Workspace")
	}
	if err := scoped.AtomicWrite("escape/outside.txt", []byte("changed"), 0o600); err == nil {
		t.Fatal("AtomicWrite() followed a symlink outside the Workspace")
	}
	content, err := os.ReadFile(outsidePath)
	if err != nil || string(content) != "secret" {
		t.Fatalf("outside file = %q, %v", content, err)
	}
}

func TestWorkspaceAtomicWriteReplacesContainedFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "source.txt")
	if err := os.WriteFile(path, []byte("before"), 0o640); err != nil {
		t.Fatal(err)
	}
	scoped, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := scoped.AtomicWrite("source.txt", []byte("after"), 0o640); err != nil {
		t.Fatalf("AtomicWrite() error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "after" {
		t.Fatalf("file = %q, %v", content, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v", info.Mode())
	}
}
