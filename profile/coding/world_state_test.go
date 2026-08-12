package coding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/extensions/edit"
	"github.com/qed-runtime/qed/extensions/filesystem"
	gitextension "github.com/qed-runtime/qed/extensions/git"
	processextension "github.com/qed-runtime/qed/extensions/process"
	"github.com/qed-runtime/qed/workspace"
)

func TestCodingCurrentWorldStateReconstructsCanonicalFilesGitAndChecks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runWorldGit(t, root, "init", "-q")
	stalePath := filepath.Join(root, "stale.go")
	calcPath := filepath.Join(root, "calc.go")
	if err := os.WriteFile(stalePath, []byte("package sample\n\nconst Value = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(calcPath, []byte("package sample\n\nfunc Add(a, b int) int { return a + b }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldStale := worldTestDigest([]byte("package sample\n\nconst Value = 0\n"))
	calcDigest := worldTestDigest([]byte("package sample\n\nfunc Add(a, b int) int { return a + b }\n"))

	policy, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{Allow: []capability.Name{
		capability.FilesystemRead, capability.GitRead,
	}})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newCurrentWorldStateSource(
		scoped, policy, gitextension.Options{}, map[string]string{"PATH": os.Getenv("PATH")}, CurrentWorldStateOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}

	readOutput, _ := json.Marshal(struct {
		Path   string `json:"path"`
		Digest string `json:"digest"`
	}{Path: "stale.go", Digest: oldStale})
	commandArguments := json.RawMessage(`{"argv":["go","test","./..."],"cwd":"."}`)
	commandOutput := `{"argv":["go","test","./..."],"cwd":".","exit_code":0,"success":true,"stdout":"ok","stderr":"","stdout_truncated":false,"stderr_truncated":false,"timed_out":false,"duration_ms":10}`
	patchOutput, _ := json.Marshal(struct {
		Changes []struct {
			Path        string `json:"path"`
			Kind        string `json:"kind"`
			AfterDigest string `json:"after_digest"`
		} `json:"changes"`
	}{Changes: []struct {
		Path        string `json:"path"`
		Kind        string `json:"kind"`
		AfterDigest string `json:"after_digest"`
	}{{Path: "calc.go", Kind: "update", AfterDigest: calcDigest}}})
	events := []agent.Event{
		worldToolEvent(1, filesystem.ReadFileToolName, json.RawMessage(`{"path":"stale.go"}`), string(readOutput), false),
		worldToolEvent(2, processextension.RunCommandToolName, commandArguments, commandOutput, false),
		worldToolEvent(3, edit.ApplyPatchToolName, json.RawMessage(`{}`), string(patchOutput), false),
	}
	snapshot, err := source.Snapshot(context.Background(), agent.CurrentWorldStateRequest{
		Run: agent.RunInfo{RunID: "run-1"}, Events: events,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Git == nil || !snapshot.Git.Available || snapshot.Git.DiffDigest == "" || snapshot.Git.DiffBytes == 0 {
		t.Fatalf("Git state = %#v", snapshot.Git)
	}
	files := make(map[string]agent.CurrentWorldFile)
	for _, file := range snapshot.Files {
		files[file.Path] = file
	}
	if files["stale.go"].Digest == oldStale || files["stale.go"].Observation == nil || files["stale.go"].Observation.Matches {
		t.Fatalf("stale.go state = %#v", files["stale.go"])
	}
	if files["calc.go"].Digest != calcDigest || files["calc.go"].Observation == nil || !files["calc.go"].Observation.Matches {
		t.Fatalf("calc.go state = %#v", files["calc.go"])
	}
	if len(snapshot.Checks) != 1 || snapshot.Checks[0].Status != agent.CurrentWorldCheckPassed ||
		snapshot.Checks[0].Freshness != agent.CurrentWorldCheckStale ||
		snapshot.Checks[0].OutputDigest != worldTestDigest([]byte(commandOutput)) {
		t.Fatalf("check state = %#v", snapshot.Checks)
	}
}

func TestCodingCurrentWorldStateRespectsRunCapabilities(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runWorldGit(t, root, "init", "-q")
	policy, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{Allow: []capability.Name{
		capability.FilesystemRead, capability.GitRead,
	}})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newCurrentWorldStateSource(
		scoped, policy, gitextension.Options{}, map[string]string{"PATH": os.Getenv("PATH")}, CurrentWorldStateOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.Snapshot(context.Background(), agent.CurrentWorldStateRequest{
		Run: agent.RunInfo{Capabilities: []string{string(capability.FilesystemRead)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Git == nil || snapshot.Git.Available {
		t.Fatalf("Git state outside Run capabilities = %#v", snapshot.Git)
	}
}

func TestCodingCurrentWorldStateMarksUnknownMutationUnverified(t *testing.T) {
	t.Parallel()

	commandOutput := `{"argv":["go","test","./..."],"cwd":".","exit_code":0,"success":true,"stdout":"ok","stderr":"","stdout_truncated":false,"stderr_truncated":false,"timed_out":false,"duration_ms":10}`
	events := []agent.Event{
		worldToolEvent(
			1,
			processextension.RunCommandToolName,
			json.RawMessage(`{"argv":["go","test","./..."],"cwd":"."}`),
			commandOutput,
			false,
		),
		worldToolEvent(2, "third_party_operation", json.RawMessage(`{}`), `{}`, false),
	}
	source := &currentWorldStateSource{maxChecks: 4}
	_, _, _, checks, truncated, err := source.reduceEvents(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("checks were unexpectedly truncated")
	}
	current, err := source.snapshotChecks(context.Background(), events, checks)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || current[0].Freshness != agent.CurrentWorldCheckUnverified {
		t.Fatalf("check state = %#v", current)
	}
}

func TestBoundCurrentWorldStateSnapshotRetainsBoundedChecks(t *testing.T) {
	t.Parallel()

	snapshot := agent.CurrentWorldStateSnapshot{FilesAvailable: true}
	for index := 0; index < agent.MaxCurrentWorldStateChecks; index++ {
		snapshot.Checks = append(snapshot.Checks, agent.CurrentWorldCheck{
			Argv:         []string{"check", fmt.Sprintf("%04d-%s", index, strings.Repeat("x", 2048))},
			CWD:          ".",
			Status:       agent.CurrentWorldCheckPassed,
			Freshness:    agent.CurrentWorldCheckCurrent,
			OutputDigest: worldTestDigest([]byte(fmt.Sprint(index))),
		})
	}
	bounded := boundCurrentWorldStateSnapshot(snapshot)
	encoded, err := json.Marshal(bounded)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > agent.MaxCurrentWorldStateBytes || !bounded.ChecksTruncated ||
		len(bounded.Checks) == 0 || len(bounded.Checks) >= len(snapshot.Checks) {
		t.Fatalf("bounded checks = %d bytes / %d checks / truncated %t", len(encoded), len(bounded.Checks), bounded.ChecksTruncated)
	}
}

func TestCodingCurrentWorldStateReductionHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := &currentWorldStateSource{maxChecks: 1}
	if _, _, _, _, _, err := source.reduceEvents(ctx, []agent.Event{{}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("reduceEvents() error = %v", err)
	}
}

func worldToolEvent(sequence uint64, name string, arguments json.RawMessage, output string, isError bool) agent.Event {
	call := agent.ToolCall{ID: fmt.Sprintf("call-%d", sequence), Name: name, Arguments: arguments}
	result := agent.ToolResult{CallID: call.ID, Name: name, Output: output, IsError: isError}
	return agent.Event{
		RunID: "run-1", Sequence: sequence, Type: agent.EventToolCompleted,
		ToolCall: &call, ToolResult: &result,
	}
}

func runWorldGit(t *testing.T, root string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

func worldTestDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}
