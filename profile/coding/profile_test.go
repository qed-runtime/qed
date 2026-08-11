package coding_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/extension/host"
	"github.com/qed-runtime/qed/extension/selfexec"
	gitextension "github.com/qed-runtime/qed/extensions/git"
	processextension "github.com/qed-runtime/qed/extensions/process"
	workspaceextension "github.com/qed-runtime/qed/extensions/workspace"
	"github.com/qed-runtime/qed/internal/extensionregistry"
	"github.com/qed-runtime/qed/profile/coding"
)

func TestMain(testingMain *testing.M) {
	if len(os.Args) >= 2 && os.Args[1] == selfexec.ChildArgument {
		handled, err := extensionregistry.Catalog.Dispatch(context.Background(), selfexec.DispatchOptions{
			Arguments: os.Args[1:],
			Input:     os.Stdin,
			Output:    os.Stdout,
		})
		if !handled || err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(testingMain.Run())
}

func TestCodingProfileRunsCompleteSixToolLoop(t *testing.T) {
	t.Parallel()

	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go executable is unavailable")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable is unavailable")
	}

	root := t.TempDir()
	writeFile(t, root, "go.mod", "module example.com/codingtest\n\ngo 1.25.0\n")
	writeFile(t, root, "calc.go", "package codingtest\n\nfunc Add(first, second int) int {\n\treturn first - second\n}\n")
	writeFile(t, root, "calc_test.go", "package codingtest\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"unexpected sum\")\n\t}\n}\n")
	writeFile(t, root, "AGENTS.md", "# Test instructions\n\nRun the Go test after editing\n")
	initializeRepository(t, root, []string{"AGENTS.md", "calc.go", "calc_test.go", "go.mod"})

	policy, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{Allow: []capability.Name{
		capability.FilesystemRead,
		capability.FilesystemWrite,
		capability.ProcessExecute,
		capability.GitRead,
	}})
	if err != nil {
		t.Fatal(err)
	}
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	testExecutable, err = filepath.Abs(testExecutable)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := coding.New(context.Background(), coding.Options{
		Root:   root,
		Policy: policy,
		Extensions: []coding.ExtensionOptions{
			{
				ID: workspaceextension.ID,
				Command: host.Command{
					Path: testExecutable,
					Args: []string{selfexec.ChildArgument, workspaceextension.ID},
				},
			},
			{
				ID: processextension.ID,
				Command: host.Command{
					Path: testExecutable,
					Args: []string{selfexec.ChildArgument, processextension.ID},
				},
			},
			{
				ID: gitextension.ID,
				Command: host.Command{
					Path: testExecutable,
					Args: []string{selfexec.ChildArgument, gitextension.ID},
				},
			},
		},
		CommandEnvironment: map[string]string{
			"GOCACHE":     t.TempDir(),
			"GOENV":       "off",
			"GOTOOLCHAIN": "local",
			"HOME":        t.TempDir(),
			"PATH":        os.Getenv("PATH"),
		},
	})
	if err != nil {
		t.Fatalf("coding.New() error = %v", err)
	}
	defer func() {
		if err := profile.Close(); err != nil {
			t.Errorf("Profile.Close() error = %v", err)
		}
	}()
	if !strings.Contains(profile.Instructions(), "Run the Go test after editing") {
		t.Fatal("Coding Profile did not load AGENTS.md")
	}
	if strings.Contains(profile.Instructions(), root) {
		t.Fatal("Coding Profile instructions exposed the absolute workspace path")
	}

	provider := &codingLoopProvider{goExecutable: goExecutable}
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider, ToolSource: profile.ToolSource()})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID:      "coding",
		SessionID:    "session-1",
		Instructions: profile.Instructions(),
		Input:        []agent.Message{{Role: agent.RoleUser, Text: "Fix Add and verify the change"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range handle.Events() {
	}
	result, err := handle.Wait()
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != agent.RunStatusCompleted || result.ToolCalls != 6 || result.ProviderCalls != 7 {
		t.Fatalf("Run result = %#v", result)
	}
	if got := result.Messages[len(result.Messages)-1].Text; got != "Fixed Add and verified go test, status, and diff" {
		t.Fatalf("final message = %q", got)
	}
	content, err := os.ReadFile(filepath.Join(root, "calc.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "return first + second") {
		t.Fatalf("calc.go = %q", content)
	}

	wantTools := []string{"search_text", "read_file", "apply_patch", "run_command", "git_status", "git_diff"}
	wantExtensionIDs := []string{
		workspaceextension.ID,
		workspaceextension.ID,
		workspaceextension.ID,
		processextension.ID,
		gitextension.ID,
		gitextension.ID,
	}
	invocations := profile.ToolInvocations()
	if len(invocations) != len(wantTools) {
		t.Fatalf("Evidence count = %d, want %d", len(invocations), len(wantTools))
	}
	for index, invocation := range invocations {
		if invocation.Tool != wantTools[index] || invocation.RunID != result.RunID || invocation.AgentID != "coding" ||
			invocation.SessionID != "session-1" || invocation.CallID == "" || invocation.ArgumentsDigest == "" ||
			invocation.OutputDigest == "" || invocation.PolicyOutcome != string(capability.OutcomeAllow) || invocation.IsError ||
			invocation.ExtensionID != wantExtensionIDs[index] || invocation.ExtensionGeneration != 1 {
			t.Errorf("Evidence[%d] = %#v", index, invocation)
		}
	}
}

type codingLoopProvider struct {
	mu           sync.Mutex
	step         int
	goExecutable string
}

func (provider *codingLoopProvider) Name() string { return "coding-loop-test" }

func (provider *codingLoopProvider) Complete(ctx context.Context, request agent.ModelRequest) (agent.Message, error) {
	if err := ctx.Err(); err != nil {
		return agent.Message{}, err
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()

	step := provider.step
	provider.step++
	call := func(name, arguments string) (agent.Message, error) {
		return agent.Message{
			Role:       agent.RoleAssistant,
			StopReason: agent.StopReasonToolUse,
			ToolCalls: []agent.ToolCall{{
				ID:        fmt.Sprintf("call-%d", step+1),
				Name:      name,
				Arguments: json.RawMessage(arguments),
			}},
		}, nil
	}

	switch step {
	case 0:
		want := []string{"apply_patch", "git_diff", "git_status", "read_file", "run_command", "search_text"}
		if len(request.Tools) != len(want) {
			return agent.Message{}, fmt.Errorf("Tool count = %d, want %d", len(request.Tools), len(want))
		}
		for index := range want {
			if request.Tools[index].Name != want[index] {
				return agent.Message{}, fmt.Errorf("Tool[%d] = %q, want %q", index, request.Tools[index].Name, want[index])
			}
		}
		if !strings.Contains(request.Instructions, "Run the Go test after editing") {
			return agent.Message{}, errors.New("project instructions are missing")
		}
		return call("search_text", `{"query":"return first - second","paths":["calc.go"]}`)
	case 1:
		if _, err := successfulToolOutput(request, "search_text"); err != nil {
			return agent.Message{}, err
		}
		return call("read_file", `{"path":"calc.go"}`)
	case 2:
		output, err := successfulToolOutput(request, "read_file")
		if err != nil {
			return agent.Message{}, err
		}
		var readResult struct {
			Digest string `json:"digest"`
		}
		if err := json.Unmarshal([]byte(output), &readResult); err != nil || readResult.Digest == "" {
			return agent.Message{}, fmt.Errorf("decode read_file output: %w", err)
		}
		arguments, err := json.Marshal(map[string]any{
			"patch":         "--- a/calc.go\n+++ b/calc.go\n@@ -2,4 +2,4 @@\n \n func Add(first, second int) int {\n-\treturn first - second\n+\treturn first + second\n }\n",
			"preconditions": []map[string]string{{"path": "calc.go", "sha256": readResult.Digest}},
		})
		if err != nil {
			return agent.Message{}, err
		}
		return call("apply_patch", string(arguments))
	case 3:
		if _, err := successfulToolOutput(request, "apply_patch"); err != nil {
			return agent.Message{}, err
		}
		arguments, err := json.Marshal(map[string]any{"argv": []string{provider.goExecutable, "test", "./..."}})
		if err != nil {
			return agent.Message{}, err
		}
		return call("run_command", string(arguments))
	case 4:
		output, err := successfulToolOutput(request, "run_command")
		if err != nil {
			return agent.Message{}, err
		}
		var commandResult struct {
			Success bool `json:"success"`
		}
		if err := json.Unmarshal([]byte(output), &commandResult); err != nil || !commandResult.Success {
			return agent.Message{}, fmt.Errorf("go test did not succeed: %s", output)
		}
		return call("git_status", `{}`)
	case 5:
		if _, err := successfulToolOutput(request, "git_status"); err != nil {
			return agent.Message{}, err
		}
		return call("git_diff", `{"scope":"worktree","paths":["calc.go"]}`)
	case 6:
		output, err := successfulToolOutput(request, "git_diff")
		if err != nil {
			return agent.Message{}, err
		}
		if !strings.Contains(output, "return first + second") {
			return agent.Message{}, errors.New("git_diff does not contain the applied change")
		}
		return agent.Message{Role: agent.RoleAssistant, Text: "Fixed Add and verified go test, status, and diff", StopReason: agent.StopReasonEndTurn}, nil
	default:
		return agent.Message{}, errors.New("unexpected Provider call")
	}
}

func (provider *codingLoopProvider) Stream(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	message, err := provider.Complete(ctx, request)
	if err != nil {
		return nil, err
	}
	return agent.MessageStream(message), nil
}

func successfulToolOutput(request agent.ModelRequest, name string) (string, error) {
	if len(request.Messages) == 0 {
		return "", errors.New("Tool result message is missing")
	}
	message := request.Messages[len(request.Messages)-1]
	if message.Role != agent.RoleTool || message.ToolName != name || message.ToolIsError {
		return "", fmt.Errorf("last message = %#v, want successful %s result", message, name)
	}
	return message.Text, nil
}

func writeFile(t *testing.T, root, path, content string) {
	t.Helper()
	absolute := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func initializeRepository(t *testing.T, root string, paths []string) {
	t.Helper()
	runGit(t, root, "init", "-q", "--initial-branch=main")
	for _, path := range paths {
		blob := strings.TrimSpace(runGit(t, root, "hash-object", "-w", "--", path))
		runGit(t, root, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+path)
	}
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
