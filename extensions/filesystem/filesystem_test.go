package filesystem_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	filesystemextension "github.com/qed-runtime/qed/extensions/filesystem"
	"github.com/qed-runtime/qed/workspace"
)

func TestSearchTextReturnsStableBoundedMatches(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.go"), "first\nNeedle here\nneedle again\n")
	writeFile(t, filepath.Join(root, "nested", "b.go"), "Needle nested\n")
	writeFile(t, filepath.Join(root, ".git", "hidden"), "Needle hidden\n")
	tools := newTools(t, root)
	result := execute(t, tools[0], `{"query":"needle","case_sensitive":false,"max_results":2}`)

	var response struct {
		Matches []struct {
			Path   string `json:"path"`
			Line   int    `json:"line"`
			Column int    `json:"column"`
		} `json:"matches"`
		SearchedFiles int  `json:"searched_files"`
		Truncated     bool `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(result.Output), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Matches) != 2 || response.Matches[0].Path != "a.go" || response.Matches[0].Line != 2 ||
		response.Matches[1].Path != "a.go" || response.Matches[1].Line != 3 || !response.Truncated {
		t.Fatalf("search response = %#v", response)
	}
	if strings.Contains(result.Output, ".git") || strings.Contains(result.Output, root) {
		t.Errorf("search output exposed ignored or absolute path: %s", result.Output)
	}
}

func TestReadFileReturnsRangeAndDigest(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	content := "one\ntwo\nthree\n"
	writeFile(t, filepath.Join(root, "source.txt"), content)
	tools := newTools(t, root)
	result := execute(t, tools[1], `{"path":"source.txt","start_line":2,"end_line":2}`)

	var response struct {
		Path       string `json:"path"`
		Digest     string `json:"digest"`
		Content    string `json:"content"`
		TotalLines int    `json:"total_lines"`
		Truncated  bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(result.Output), &response); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])
	if response.Path != "source.txt" || response.Digest != wantDigest || response.Content != "two\n" ||
		response.TotalLines != 3 || !response.Truncated {
		t.Fatalf("read response = %#v", response)
	}
}

func TestFilesystemToolsRejectTraversalAndAmbiguousJSON(t *testing.T) {
	t.Parallel()

	tools := newTools(t, t.TempDir())
	for _, arguments := range []string{
		`{"path":"../outside"}`,
		`{"path":"missing","path":"duplicate"}`,
		`{"path":"missing","unknown":true}`,
	} {
		_, err := tools[1].Execute(context.Background(), agent.ToolCall{Name: filesystemextension.ReadFileToolName, Arguments: json.RawMessage(arguments)})
		if err == nil {
			t.Errorf("Execute(%s) error = nil", arguments)
		}
	}
}

func TestReadFileBoundsEncodedOutput(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "control.txt"), strings.Repeat("\x01", 40))
	scoped, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := filesystemextension.NewTools(scoped, filesystemextension.Options{MaxOutputBytes: 200})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tools[1].Execute(context.Background(), agent.ToolCall{
		Name:      filesystemextension.ReadFileToolName,
		Arguments: json.RawMessage(`{"path":"control.txt"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "result exceeds the output limit") {
		t.Fatalf("Execute() error = %v", err)
	}
}

func newTools(t *testing.T, root string) []agent.Tool {
	t.Helper()
	scoped, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := filesystemextension.NewTools(scoped, filesystemextension.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

func execute(t *testing.T, tool agent.Tool, arguments string) agent.ToolResult {
	t.Helper()
	result, err := tool.Execute(context.Background(), agent.ToolCall{ID: "call-1", Name: tool.Definition().Name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	return result
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
