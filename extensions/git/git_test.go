package git_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	gitextension "github.com/qed-runtime/qed/extensions/git"
	"github.com/qed-runtime/qed/workspace"
)

func TestGitStatusAndDiffReturnStructuredReadOnlyResults(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	initializeRepository(t, root, "source.txt", "one\n")
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tools := newGitTools(t, root)

	status := executeGitTool(t, tools[0], `{}`)
	var statusResponse struct {
		Branch struct {
			Head string `json:"head"`
		} `json:"branch"`
		Entries []struct {
			Path           string `json:"path"`
			Kind           string `json:"kind"`
			WorktreeStatus string `json:"worktree_status"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(status.Output), &statusResponse); err != nil {
		t.Fatal(err)
	}
	if statusResponse.Branch.Head != "main" || len(statusResponse.Entries) != 2 {
		t.Fatalf("status = %#v", statusResponse)
	}
	if statusResponse.Entries[0].Path != "source.txt" || statusResponse.Entries[0].WorktreeStatus != "M" ||
		statusResponse.Entries[1].Path != "new.txt" || statusResponse.Entries[1].Kind != "untracked" {
		t.Fatalf("status entries = %#v", statusResponse.Entries)
	}

	diff := executeGitTool(t, tools[1], `{"scope":"worktree","paths":["source.txt"]}`)
	var diffResponse struct {
		Scope     string `json:"scope"`
		Patch     string `json:"patch"`
		Digest    string `json:"digest"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(diff.Output), &diffResponse); err != nil {
		t.Fatal(err)
	}
	if diffResponse.Scope != "worktree" || diffResponse.Truncated || diffResponse.Digest == "" ||
		!strings.Contains(diffResponse.Patch, "-one") || !strings.Contains(diffResponse.Patch, "+two") {
		t.Fatalf("diff = %#v", diffResponse)
	}
	if strings.Contains(status.Output, root) || strings.Contains(diff.Output, root) {
		t.Error("Git Tool output exposed the absolute workspace path")
	}
}

func TestGitToolsRejectWorkspaceBelowRepositoryRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	initializeRepository(t, root, "nested/source.txt", "one\n")
	tools := newGitTools(t, filepath.Join(root, "nested"))
	_, err := tools[0].Execute(context.Background(), agent.ToolCall{Name: gitextension.StatusToolName, Arguments: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "must equal the Git repository root") {
		t.Fatalf("Execute() error = %v", err)
	}
}

func newGitTools(t *testing.T, root string) []agent.Tool {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable is unavailable")
	}
	scoped, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := gitextension.NewTools(scoped, gitextension.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

func executeGitTool(t *testing.T, tool agent.Tool, arguments string) agent.ToolResult {
	t.Helper()
	result, err := tool.Execute(context.Background(), agent.ToolCall{ID: "call-1", Name: tool.Definition().Name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatalf("Execute(%s) error = %v", tool.Definition().Name, err)
	}
	return result
}

func initializeRepository(t *testing.T, root, path, content string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable is unavailable")
	}
	runGit(t, root, "init", "-q", "--initial-branch=main")
	absolute := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	blob := strings.TrimSpace(runGit(t, root, "hash-object", "-w", "--", path))
	runGit(t, root, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+path)
	tree := strings.TrimSpace(runGit(t, root, "write-tree"))
	commit := strings.TrimSpace(runGit(t, root, "commit-tree", tree, "-m", "baseline"))
	runGit(t, root, "update-ref", "refs/heads/main", commit)
	runGit(t, root, "symbolic-ref", "HEAD", "refs/heads/main")
}

func runGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=QED Test",
		"GIT_AUTHOR_EMAIL=qed@example.invalid",
		"GIT_COMMITTER_NAME=QED Test",
		"GIT_COMMITTER_EMAIL=qed@example.invalid",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return string(output)
}
