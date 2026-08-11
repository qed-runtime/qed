package process_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
	processextension "github.com/qed-runtime/qed/extensions/process"
	"github.com/qed-runtime/qed/workspace"
)

func TestRunCommandReturnsSeparatedBoundedOutput(t *testing.T) {
	t.Parallel()

	tool := newTool(t, t.TempDir(), processextension.Options{MaxOutputBytes: 4})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	arguments, _ := json.Marshal(map[string]any{
		"argv": []string{executable, "-test.run=TestProcessHelper", "--", "output"},
	})
	result, err := tool.Execute(context.Background(), agent.ToolCall{ID: "call-1", Name: processextension.RunCommandToolName, Arguments: arguments})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var response struct {
		ExitCode        int    `json:"exit_code"`
		Success         bool   `json:"success"`
		Stdout          string `json:"stdout"`
		Stderr          string `json:"stderr"`
		StdoutTruncated bool   `json:"stdout_truncated"`
		StderrTruncated bool   `json:"stderr_truncated"`
	}
	if err := json.Unmarshal([]byte(result.Output), &response); err != nil {
		t.Fatal(err)
	}
	if !result.IsError || response.ExitCode != 3 || response.Success || response.Stdout != "stdo" || response.Stderr != "stde" ||
		!response.StdoutTruncated || !response.StderrTruncated {
		t.Fatalf("result/response = %#v / %#v", result, response)
	}
}

func TestRunCommandTimesOutAndRejectsEscapingCWD(t *testing.T) {
	t.Parallel()

	tool := newTool(t, t.TempDir(), processextension.Options{DefaultTimeout: 50 * time.Millisecond, MaximumTimeout: time.Second})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	arguments, _ := json.Marshal(map[string]any{
		"argv": []string{executable, "-test.run=TestProcessHelper", "--", "sleep"},
	})
	result, err := tool.Execute(context.Background(), agent.ToolCall{Name: processextension.RunCommandToolName, Arguments: arguments})
	if err != nil {
		t.Fatalf("Execute(timeout) error = %v", err)
	}
	var response struct {
		TimedOut bool `json:"timed_out"`
		Success  bool `json:"success"`
	}
	if err := json.Unmarshal([]byte(result.Output), &response); err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !response.TimedOut || response.Success {
		t.Fatalf("timeout result/response = %#v / %#v", result, response)
	}

	escapeArguments, _ := json.Marshal(map[string]any{"argv": []string{executable}, "cwd": ".."})
	if _, err := tool.Execute(context.Background(), agent.ToolCall{Name: processextension.RunCommandToolName, Arguments: escapeArguments}); err == nil {
		t.Error("Execute(escaping cwd) error = nil")
	}
}

func TestProcessHelper(t *testing.T) {
	if os.Getenv("QED_PROCESS_TEST_HELPER") != "1" {
		return
	}
	mode := ""
	for index, argument := range os.Args {
		if argument == "--" && index+1 < len(os.Args) {
			mode = os.Args[index+1]
			break
		}
	}
	switch mode {
	case "output":
		fmt.Fprint(os.Stdout, "stdout-value")
		fmt.Fprint(os.Stderr, "stderr-value")
		os.Exit(3)
	case "sleep":
		time.Sleep(5 * time.Second)
		os.Exit(0)
	default:
		os.Exit(4)
	}
}

func newTool(t *testing.T, root string, options processextension.Options) agent.Tool {
	t.Helper()
	scoped, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	options.Environment = map[string]string{"QED_PROCESS_TEST_HELPER": "1"}
	tool, err := processextension.NewTool(scoped, options)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func TestRunCommandUsesWorkspaceCWD(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	tool := newTool(t, root, processextension.Options{})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	arguments, _ := json.Marshal(map[string]any{
		"argv": []string{executable, "-test.run=TestProcessHelper", "--", "output"},
		"cwd":  "nested",
	})
	if _, err := tool.Execute(context.Background(), agent.ToolCall{Name: processextension.RunCommandToolName, Arguments: arguments}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
}
