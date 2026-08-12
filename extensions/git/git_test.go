package git_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

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

func TestGitDiffIncludesUntrackedFilesByScopeAndPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	initializeRepository(t, root, "source.txt", "one\n")
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "new name.txt"), []byte("new content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.txt"), []byte("do not include\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "info", "exclude"), []byte("ignored.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tools := newGitTools(t, root)

	worktree := decodeDiffResponse(t, executeGitTool(t, tools[1], `{"scope":"worktree"}`))
	if worktree.Truncated || !strings.Contains(worktree.Patch, "-one") ||
		!strings.Contains(worktree.Patch, "+two") ||
		!strings.Contains(worktree.Patch, "new file mode 100644") ||
		!strings.Contains(worktree.Patch, "+new content") {
		t.Fatalf("worktree diff = %#v", worktree)
	}
	if strings.Contains(worktree.Patch, "do not include") || strings.Contains(worktree.Patch, root) {
		t.Fatalf("worktree diff included ignored or absolute content: %q", worktree.Patch)
	}
	assertDiffDigest(t, worktree)

	filtered := decodeDiffResponse(t, executeGitTool(t, tools[1], `{"scope":"worktree","paths":["source.txt"]}`))
	if !strings.Contains(filtered.Patch, "+two") || strings.Contains(filtered.Patch, "new content") {
		t.Fatalf("filtered diff = %#v", filtered)
	}

	base := decodeDiffResponse(t, executeGitTool(t, tools[1], `{"scope":"base","base":"HEAD","paths":["nested"]}`))
	if base.Base == "" || base.Truncated || !strings.Contains(base.Patch, "+new content") {
		t.Fatalf("base diff = %#v", base)
	}
	assertDiffDigest(t, base)

	staged := decodeDiffResponse(t, executeGitTool(t, tools[1], `{"scope":"staged","paths":["nested"]}`))
	if staged.Patch != "" || staged.Truncated {
		t.Fatalf("staged diff = %#v", staged)
	}
	assertDiffDigest(t, staged)
}

func TestGitDiffBoundsCombinedUntrackedPatch(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	initializeRepository(t, root, "source.txt", "one\n")
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(strings.Repeat("bounded content\n", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	const maximum = 320
	tools := newGitToolsWithOptions(t, root, gitextension.Options{MaxOutputBytes: maximum})
	diff := decodeDiffResponse(t, executeGitTool(t, tools[1], `{"scope":"worktree"}`))
	if !diff.Truncated || len([]byte(diff.Patch)) != maximum ||
		!strings.Contains(diff.Patch, "-one") || !strings.Contains(diff.Patch, "+two") ||
		!strings.Contains(diff.Patch, "+bounded content") {
		t.Fatalf("bounded diff bytes = %d, response = %#v", len([]byte(diff.Patch)), diff)
	}
	assertDiffDigest(t, diff)
}

func TestGitDiffKeepsTruncatedPatchValidUTF8(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	initializeRepository(t, root, "source.txt", "one\n")
	if err := os.WriteFile(filepath.Join(root, "unicode.txt"), []byte(strings.Repeat("あ", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fullTools := newGitTools(t, root)
	full := decodeDiffResponse(t, executeGitTool(t, fullTools[1], `{"scope":"worktree","paths":["unicode.txt"]}`))
	firstRune := strings.Index(full.Patch, "あ")
	if firstRune < 0 {
		t.Fatalf("full diff does not contain Unicode content: %q", full.Patch)
	}

	maximum := firstRune + 1
	boundedTools := newGitToolsWithOptions(t, root, gitextension.Options{MaxOutputBytes: maximum})
	bounded := decodeDiffResponse(t, executeGitTool(t, boundedTools[1], `{"scope":"worktree","paths":["unicode.txt"]}`))
	if !bounded.Truncated || !utf8.ValidString(bounded.Patch) || len([]byte(bounded.Patch)) >= maximum {
		t.Fatalf("bounded Unicode diff bytes = %d, response = %#v", len([]byte(bounded.Patch)), bounded)
	}
	assertDiffDigest(t, bounded)
}

func TestGitDiffTreatsRequestedPathsLiterally(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	initializeRepository(t, root, "source.txt", "one\n")
	if err := os.WriteFile(filepath.Join(root, "*.txt"), []byte("literal path\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("must not match\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tools := newGitTools(t, root)
	diff := decodeDiffResponse(t, executeGitTool(t, tools[1], `{"scope":"worktree","paths":["*.txt"]}`))
	if diff.Truncated || !strings.Contains(diff.Patch, "+literal path") || strings.Contains(diff.Patch, "must not match") {
		t.Fatalf("literal path diff = %#v", diff)
	}
	assertDiffDigest(t, diff)
}

func TestGitDiffDoesNotReadUntrackedSymlinkOrBinaryContent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	initializeRepository(t, root, "source.txt", "one\n")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary.dat"), []byte{'s', 'e', 'c', 'r', 'e', 't', 0, 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	tools := newGitTools(t, root)

	_, err := tools[1].Execute(context.Background(), agent.ToolCall{
		ID: "symlink", Name: gitextension.DiffToolName, Arguments: json.RawMessage(`{"scope":"worktree","paths":["link.txt"]}`),
	})
	if err == nil || strings.Contains(err.Error(), "outside-secret") || strings.Contains(err.Error(), outside) {
		t.Fatalf("explicit symlink error = %v", err)
	}

	binary := decodeDiffResponse(t, executeGitTool(t, tools[1], `{"scope":"worktree","paths":["binary.dat"]}`))
	if binary.Truncated || !strings.Contains(binary.Patch, "Binary files") || strings.Contains(binary.Patch, "secret") {
		t.Fatalf("binary diff = %#v", binary)
	}
	assertDiffDigest(t, binary)

	all := decodeDiffResponse(t, executeGitTool(t, tools[1], `{"scope":"worktree"}`))
	if !all.Truncated || !strings.Contains(all.Patch, "Binary files") ||
		strings.Contains(all.Patch, "outside-secret") || strings.Contains(all.Patch, outside) {
		t.Fatalf("unfiltered diff = %#v", all)
	}
	assertDiffDigest(t, all)
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

func TestGitDiffRejectsNilContext(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	initializeRepository(t, root, "source.txt", "one\n")
	tools := newGitTools(t, root)
	_, err := tools[1].Execute(nil, agent.ToolCall{Name: gitextension.DiffToolName, Arguments: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "context must not be nil") {
		t.Fatalf("Execute(nil) error = %v", err)
	}
}

func newGitTools(t *testing.T, root string) []agent.Tool {
	return newGitToolsWithOptions(t, root, gitextension.Options{})
}

func newGitToolsWithOptions(t *testing.T, root string, options gitextension.Options) []agent.Tool {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable is unavailable")
	}
	scoped, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := gitextension.NewTools(scoped, options)
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

type decodedDiffResponse struct {
	Scope     string `json:"scope"`
	Base      string `json:"base"`
	Patch     string `json:"patch"`
	Digest    string `json:"digest"`
	Truncated bool   `json:"truncated"`
}

func decodeDiffResponse(t *testing.T, result agent.ToolResult) decodedDiffResponse {
	t.Helper()
	var response decodedDiffResponse
	if err := json.Unmarshal([]byte(result.Output), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func assertDiffDigest(t *testing.T, response decodedDiffResponse) {
	t.Helper()
	sum := sha256.Sum256([]byte(response.Patch))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if response.Digest != want {
		t.Fatalf("diff digest = %q, want %q", response.Digest, want)
	}
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
