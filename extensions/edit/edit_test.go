package edit_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/extension"
	"github.com/qed-runtime/qed/extensions/edit"
	"github.com/qed-runtime/qed/workspace"
)

func TestApplyPatchDefinitionExplainsFormatAndPreconditions(t *testing.T) {
	t.Parallel()

	definition := newTool(t, t.TempDir()).Definition()
	for _, expected := range []string{"--- a/path", "+++ b/path", "@@", "*** Begin Patch", "sha256:", "absent:true"} {
		if !strings.Contains(definition.Description, expected) {
			t.Errorf("Description %q does not contain %q", definition.Description, expected)
		}
	}
	schema := string(definition.InputSchema)
	for _, expected := range []string{
		"Unified diff text with --- a/path, +++ b/path, and @@ hunk headers",
		"Full sha256:... digest returned by read_file",
		"Set true only when adding a path that does not exist",
	} {
		if !strings.Contains(schema, expected) {
			t.Errorf("InputSchema does not contain %q", expected)
		}
	}
}

func TestApplyPatchUpdatesFileWithDigestPrecondition(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "source.txt")
	before := "one\ntwo\nthree\n"
	if err := os.WriteFile(path, []byte(before), 0o640); err != nil {
		t.Fatal(err)
	}
	tool := newTool(t, root)
	request := map[string]any{
		"patch":         "--- a/source.txt\n+++ b/source.txt\n@@ -1,3 +1,3 @@\n one\n-two\n+TWO\n three\n",
		"preconditions": []map[string]any{{"path": "source.txt", "sha256": digest(before)}},
	}
	result := execute(t, tool, request)
	if result.IsError {
		t.Fatalf("result = %#v", result)
	}
	if result.ContextOperation == nil || result.ContextOperation.Kind != agent.ContextOperationMutation {
		t.Fatalf("Context operation = %#v", result.ContextOperation)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "one\nTWO\nthree\n" {
		t.Fatalf("file = %q", after)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}
	var response struct {
		Changes []struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		} `json:"changes"`
	}
	if err := json.Unmarshal([]byte(result.Output), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Changes) != 1 || response.Changes[0].Path != "source.txt" || response.Changes[0].Kind != "update" {
		t.Fatalf("response = %#v", response)
	}
}

func TestApplyPatchApprovalPreviewValidatesWithoutMutation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "source.txt")
	before := "one\ntwo\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := newTool(t, root)
	previewer, ok := raw.(extension.ApprovalPreviewer)
	if !ok {
		t.Fatal("apply_patch does not expose ApprovalPreviewer")
	}
	arguments, err := json.Marshal(map[string]any{
		"patch":         "--- a/source.txt\n+++ b/source.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+TWO\n",
		"preconditions": []map[string]any{{"path": "source.txt", "sha256": digest(before)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := previewer.ApprovalPreview(context.Background(), agent.ToolCall{Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	if preview == nil || preview.Summary != "Apply a validated patch to 1 file(s) (+1 -1)" ||
		len(preview.Details) != 1 || preview.Details[0].Label != "update" || preview.Details[0].Value != "source.txt" {
		t.Fatalf("ApprovalPreview() = %#v", preview)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != before {
		t.Fatalf("preview changed file = %q, %v", content, err)
	}

	var request map[string]any
	if err := json.Unmarshal(arguments, &request); err != nil {
		t.Fatal(err)
	}
	request["preconditions"] = []map[string]any{{"path": "source.txt", "sha256": digest("stale\n")}}
	staleArguments, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := previewer.ApprovalPreview(context.Background(), agent.ToolCall{Arguments: staleArguments}); err == nil {
		t.Fatal("ApprovalPreview accepted stale precondition")
	}
}

func TestApplyPatchAcceptsWorkspaceRelativeHeaders(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "source.txt")
	before := "before\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := newTool(t, root)
	result := execute(t, tool, map[string]any{
		"patch":         "--- source.txt\n+++ source.txt\n@@ -1 +1 @@\n-before\n+after\n",
		"preconditions": []map[string]any{{"path": "source.txt", "sha256": digest(before)}},
	})
	if result.IsError {
		t.Fatalf("result = %#v", result)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "after\n" {
		t.Fatalf("file = %q, %v", content, err)
	}
}

func TestApplyPatchAddsAndDeletesWithDynamicCapability(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	oldPath := filepath.Join(root, "old.txt")
	if err := os.WriteFile(oldPath, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := newTool(t, root)
	request := map[string]any{
		"patch": "--- a/old.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-old\n--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1 @@\n+new\n",
		"preconditions": []map[string]any{
			{"path": "old.txt", "sha256": digest("old\n")},
			{"path": "new.txt", "absent": true},
		},
	}
	arguments, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	dynamic, ok := raw.(extension.DynamicCapabilities)
	if !ok {
		t.Fatal("apply_patch does not expose dynamic capabilities")
	}
	capabilities, err := dynamic.RequiredCapabilities(context.Background(), agent.ToolCall{Arguments: arguments})
	if err != nil || len(capabilities) != 1 || capabilities[0] != capability.FilesystemDelete {
		t.Fatalf("RequiredCapabilities() = %#v, %v", capabilities, err)
	}

	policy, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{Allow: []capability.Name{capability.FilesystemWrite}})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := extension.NewTool(extension.ToolOptions{Tool: raw, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	_, err = proxy.Execute(context.Background(), agent.ToolCall{ID: "call-1", Name: edit.ApplyPatchToolName, Arguments: arguments})
	if !errors.Is(err, capability.ErrDenied) {
		t.Fatalf("Execute(without delete capability) error = %v", err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Errorf("denied patch changed old file: %v", err)
	}

	policy, err = capability.NewStaticPolicy(capability.StaticPolicyOptions{Allow: []capability.Name{capability.FilesystemWrite, capability.FilesystemDelete}})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err = extension.NewTool(extension.ToolOptions{Tool: raw, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proxy.Execute(context.Background(), agent.ToolCall{ID: "call-2", Name: edit.ApplyPatchToolName, Arguments: arguments}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old file still exists: %v", err)
	}
	newContent, err := os.ReadFile(filepath.Join(root, "new.txt"))
	if err != nil || string(newContent) != "new\n" {
		t.Errorf("new file = %q, %v", newContent, err)
	}
}

func TestApplyPatchRejectsStalePreconditionWithoutMutation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "source.txt")
	if err := os.WriteFile(path, []byte("current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := newTool(t, root)
	request := map[string]any{
		"patch":         "--- a/source.txt\n+++ b/source.txt\n@@ -1 +1 @@\n-current\n+changed\n",
		"preconditions": []map[string]any{{"path": "source.txt", "sha256": digest("stale\n")}},
	}
	arguments, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.Execute(context.Background(), agent.ToolCall{Name: edit.ApplyPatchToolName, Arguments: arguments})
	if err == nil {
		t.Fatal("Execute() error = nil")
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil || string(content) != "current\n" {
		t.Fatalf("file changed after rejection = %q, %v", content, readErr)
	}
}

func newTool(t *testing.T, root string) agent.Tool {
	t.Helper()
	scoped, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	tool, err := edit.NewTool(scoped, edit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func execute(t *testing.T, tool agent.Tool, request any) agent.ToolResult {
	t.Helper()
	arguments, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(context.Background(), agent.ToolCall{ID: "call-1", Name: tool.Definition().Name, Arguments: arguments})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	return result
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}
