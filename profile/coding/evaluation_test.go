package coding_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/evidence"
	"github.com/qed-runtime/qed/internal/codingeval"
	"github.com/qed-runtime/qed/profile/coding"
	"github.com/qed-runtime/qed/session"
)

const (
	codingEvaluationREADMEBefore = "# Evaluation Fixture\n"
	codingEvaluationREADMEAfter  = "# QED Evaluation Fixture\n"
)

func TestCodingEvaluationBaseline(t *testing.T) {
	tasks := codingeval.BaselineTasks()
	reports := make([]codingeval.TaskReport, 0, len(tasks))
	for _, task := range tasks {
		task := task
		t.Run(task.ID, func(t *testing.T) {
			var report codingeval.TaskReport
			switch task.Kind {
			case codingeval.TaskInvestigation:
				report = runCodingInvestigationEvaluation(t, task)
			case codingeval.TaskSmallEdit:
				report = runCodingSmallEditEvaluation(t, task)
			case codingeval.TaskTestRepair:
				report = runCodingTestRepairEvaluation(t, task)
			case codingeval.TaskSessionResume:
				report = runCodingSessionResumeEvaluation(t, task)
			default:
				t.Fatalf("unsupported Coding evaluation task kind %q", task.Kind)
			}
			reports = append(reports, report)
		})
	}

	baseline, err := codingeval.NewBaselineReport(reports...)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Summary.Tasks != 4 || baseline.Summary.SuccessfulTasks != 4 ||
		baseline.Summary.AcceptedDiffs != 3 || baseline.Summary.UnchangedTasks != 1 ||
		baseline.Summary.RejectedDiffs != 0 || baseline.Summary.PassedTestTasks != 1 ||
		baseline.Summary.FailedTestTasks != 0 || baseline.Summary.TestsNotRunTasks != 3 ||
		baseline.Summary.UnexpectedChanges != 0 {
		t.Fatalf("Coding evaluation summary = %#v", baseline.Summary)
	}
	if baseline.Metrics.Runs != 5 || baseline.Metrics.CanceledRuns != 1 ||
		baseline.Metrics.FailedRuns != 0 || baseline.Metrics.ApprovalRequests != 1 ||
		baseline.Metrics.Resumes != 1 || baseline.Metrics.PolicyDenials != 0 ||
		baseline.Metrics.SuccessfulChanges != 3 || baseline.Metrics.SuccessfulChecks != 1 ||
		baseline.Metrics.Checks != 2 || baseline.Metrics.GitDiffs != 3 ||
		baseline.Metrics.EvidenceRecords != baseline.Metrics.ToolCalls ||
		baseline.Metrics.Usage.InputTokens != 40 || baseline.Metrics.Usage.OutputTokens != 8 ||
		baseline.Metrics.Usage.TotalTokens != 48 || baseline.Metrics.Usage.CostMicros != 12 ||
		baseline.BaselineDigest == "" {
		t.Fatalf("Coding evaluation metrics = %#v", baseline.Metrics)
	}
	encoded, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if strings.Contains(string(encoded), task.Prompt) {
			t.Fatalf("Coding evaluation report exposed task content: %s", encoded)
		}
	}
	if strings.Contains(string(encoded), "return first - second") {
		t.Fatalf("Coding evaluation report exposed task or workspace content: %s", encoded)
	}
	t.Logf("coding_evaluation_baseline=%s", encoded)
}

func runCodingInvestigationEvaluation(t *testing.T, task codingeval.Task) codingeval.TaskReport {
	t.Helper()
	root, _ := newCodingEvaluationWorkspace(t)
	baselineCommit := codingEvaluationHead(t, root)
	profile := newCodingE2EProfile(t, root, codingE2EProfileOptions{
		policy: allowCodingEvaluationPolicy(t),
	})
	store := newCodingEvaluationSessionStore(t)
	provider := newFixedCodingProvider(
		func(agent.ModelRequest) (agent.Message, error) {
			return codingE2EToolCall("investigate-calc", "read_file", mustJSON(t, map[string]string{"path": "calc.go"})), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			output, err := codingE2EToolOutput(request, "read_file", false)
			if err != nil {
				return agent.Message{}, err
			}
			if !strings.Contains(output, "return first - second") {
				return agent.Message{}, errors.New("calc.go investigation did not observe the defect")
			}
			return codingE2EToolCall("investigate-test", "read_file", mustJSON(t, map[string]string{"path": "calc_test.go"})), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			output, err := codingE2EToolOutput(request, "read_file", false)
			if err != nil {
				return agent.Message{}, err
			}
			if !strings.Contains(output, "Add(2, 3) != 5") {
				return agent.Message{}, errors.New("calc_test.go investigation did not observe the expectation")
			}
			return codingE2EToolCall("investigate-status", "git_status", json.RawMessage(`{}`)), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			if _, err := codingE2EToolOutput(request, "git_status", false); err != nil {
				return agent.Message{}, err
			}
			return codingEvaluationFinalMessage("TestAdd expects addition, but Add subtracts the second operand"), nil
		},
	)
	observation := runCompletedCodingEvaluation(t, task, profile, store, provider)
	if status := runGit(t, root, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("investigation changed workspace: %q", status)
	}
	assertCodingEvaluationHead(t, root, baselineCommit)
	return newCodingEvaluationTaskReport(t, task, codingeval.Assessment{
		Succeeded: true,
		Tests:     codingeval.TestsNotRun,
		Diff:      codingeval.DiffUnchanged,
	}, observation)
}

func runCodingSmallEditEvaluation(t *testing.T, task codingeval.Task) codingeval.TaskReport {
	t.Helper()
	root, _ := newCodingEvaluationWorkspace(t)
	baselineCommit := codingEvaluationHead(t, root)
	profile := newCodingE2EProfile(t, root, codingE2EProfileOptions{
		policy: allowCodingEvaluationPolicy(t),
	})
	store := newCodingEvaluationSessionStore(t)
	provider := newFixedCodingProvider(
		func(agent.ModelRequest) (agent.Message, error) {
			return codingE2EToolCall("small-edit-read", "read_file", mustJSON(t, map[string]string{"path": "README.md"})), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			digest, err := codingEvaluationReadDigest(request, "README.md")
			if err != nil {
				return agent.Message{}, err
			}
			arguments := mustJSON(t, map[string]any{
				"patch":         "--- a/README.md\n+++ b/README.md\n@@ -1 +1 @@\n-# Evaluation Fixture\n+# QED Evaluation Fixture\n",
				"preconditions": []map[string]string{{"path": "README.md", "sha256": digest}},
			})
			return codingE2EToolCall("small-edit-patch", "apply_patch", arguments), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			if _, err := codingE2EToolOutput(request, "apply_patch", false); err != nil {
				return agent.Message{}, err
			}
			return codingE2EToolCall("small-edit-status", "git_status", json.RawMessage(`{}`)), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			if _, err := codingE2EToolOutput(request, "git_status", false); err != nil {
				return agent.Message{}, err
			}
			return codingE2EToolCall(
				"small-edit-diff",
				"git_diff",
				json.RawMessage(`{"scope":"worktree","paths":["README.md"]}`),
			), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			output, err := codingE2EToolOutput(request, "git_diff", false)
			if err != nil {
				return agent.Message{}, err
			}
			if !strings.Contains(output, "QED Evaluation Fixture") {
				return agent.Message{}, errors.New("README diff does not contain the requested title")
			}
			return codingEvaluationFinalMessage("Updated the README title and inspected the Git change"), nil
		},
	)
	observation := runCompletedCodingEvaluation(t, task, profile, store, provider)
	if got := readCodingE2EFile(t, root, "README.md"); got != codingEvaluationREADMEAfter {
		t.Fatalf("README.md = %q", got)
	}
	if status := runGit(t, root, "status", "--porcelain", "--untracked-files=all"); status != " M README.md\n" {
		t.Fatalf("small edit Git status = %q", status)
	}
	assertCodingEvaluationHead(t, root, baselineCommit)
	return newCodingEvaluationTaskReport(t, task, codingeval.Assessment{
		Succeeded: true,
		Tests:     codingeval.TestsNotRun,
		Diff:      codingeval.DiffAccepted,
	}, observation)
}

func runCodingTestRepairEvaluation(t *testing.T, task codingeval.Task) codingeval.TaskReport {
	t.Helper()
	root, goExecutable := newCodingEvaluationWorkspace(t)
	baselineCommit := codingEvaluationHead(t, root)
	profile := newCodingE2EProfile(t, root, codingE2EProfileOptions{
		policy: allowCodingEvaluationPolicy(t),
	})
	store := newCodingEvaluationSessionStore(t)
	testArguments := mustJSON(t, map[string]any{"argv": []string{goExecutable, "test", "./..."}})
	provider := newFixedCodingProvider(
		func(agent.ModelRequest) (agent.Message, error) {
			return codingE2EToolCall("repair-failing-test", "run_command", testArguments), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			output, err := codingE2EToolOutput(request, "run_command", true)
			if err != nil {
				return agent.Message{}, err
			}
			if err := requireCodingEvaluationCommandSuccess(output, false); err != nil {
				return agent.Message{}, err
			}
			return codingE2EToolCall("repair-read", "read_file", mustJSON(t, map[string]string{"path": "calc.go"})), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			digest, err := codingEvaluationReadDigest(request, "calc.go")
			if err != nil {
				return agent.Message{}, err
			}
			arguments := codingE2EPatchArguments(t, codingE2ECalcBefore)
			var patch map[string]any
			if err := json.Unmarshal(arguments, &patch); err != nil {
				return agent.Message{}, err
			}
			patch["preconditions"] = []map[string]string{{"path": "calc.go", "sha256": digest}}
			return codingE2EToolCall("repair-patch", "apply_patch", mustJSON(t, patch)), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			if _, err := codingE2EToolOutput(request, "apply_patch", false); err != nil {
				return agent.Message{}, err
			}
			return codingE2EToolCall("repair-passing-test", "run_command", testArguments), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			output, err := codingE2EToolOutput(request, "run_command", false)
			if err != nil {
				return agent.Message{}, err
			}
			if err := requireCodingEvaluationCommandSuccess(output, true); err != nil {
				return agent.Message{}, err
			}
			return codingE2EToolCall("repair-status", "git_status", json.RawMessage(`{}`)), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			if _, err := codingE2EToolOutput(request, "git_status", false); err != nil {
				return agent.Message{}, err
			}
			return codingE2EToolCall(
				"repair-diff",
				"git_diff",
				json.RawMessage(`{"scope":"worktree","paths":["calc.go"]}`),
			), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			output, err := codingE2EToolOutput(request, "git_diff", false)
			if err != nil {
				return agent.Message{}, err
			}
			if !strings.Contains(output, "return first + second") {
				return agent.Message{}, errors.New("repair diff does not contain the fixed implementation")
			}
			return codingEvaluationFinalMessage("Observed the failure, repaired Add, and verified the tests and diff"), nil
		},
	)
	observation := runCompletedCodingEvaluation(t, task, profile, store, provider)
	if got := readCodingE2EFile(t, root, "calc.go"); !strings.Contains(got, "return first + second") {
		t.Fatalf("calc.go = %q", got)
	}
	if status := runGit(t, root, "status", "--porcelain", "--untracked-files=all"); status != " M calc.go\n" {
		t.Fatalf("repair Git status = %q", status)
	}
	assertCodingEvaluationHead(t, root, baselineCommit)
	return newCodingEvaluationTaskReport(t, task, codingeval.Assessment{
		Succeeded: true,
		Tests:     codingeval.TestsPassed,
		Diff:      codingeval.DiffAccepted,
	}, observation)
}

func runCodingSessionResumeEvaluation(t *testing.T, task codingeval.Task) codingeval.TaskReport {
	t.Helper()
	root, _ := newCodingEvaluationWorkspace(t)
	baselineCommit := codingEvaluationHead(t, root)
	policyOptions := capability.StaticPolicyOptions{
		Allow: []capability.Name{capability.FilesystemRead, capability.ProcessExecute, capability.GitRead},
		Ask:   []capability.Name{capability.FilesystemWrite},
	}
	firstSessionRoot := filepath.Join(t.TempDir(), "sessions-before-restart")
	firstStore, err := session.NewJSONLStore(firstSessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	firstProfile := newCodingE2EProfile(t, root, codingE2EProfileOptions{
		policy:   newCodingE2EPolicy(t, policyOptions),
		approver: capability.WaitApprover{},
	})
	firstProvider := newFixedCodingProvider(func(agent.ModelRequest) (agent.Message, error) {
		return codingE2EToolCall("evaluation-resumed-edit", "apply_patch", codingE2EPatchArguments(t, codingE2ECalcBefore)), nil
	})
	firstRuntime, err := agent.NewRuntime(agent.Options{
		Provider:     firstProvider,
		ToolSource:   firstProfile.ToolSource(),
		SessionStore: firstStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstStarted := time.Now()
	firstHandle, err := firstRuntime.Run(context.Background(), agent.RunRequest{
		AgentID:      "coding-evaluation",
		SessionID:    task.ID,
		Instructions: firstProfile.Instructions(),
		Input:        []agent.Message{{Role: agent.RoleUser, Text: task.Prompt}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var waitRequest *agent.WaitRequest
	var firstEvents []agent.Event
	for event := range firstHandle.Events() {
		firstEvents = append(firstEvents, event)
		if event.Type == agent.EventRunWaiting && event.WaitRequest != nil {
			request := *event.WaitRequest
			request.Payload = append(json.RawMessage(nil), event.WaitRequest.Payload...)
			waitRequest = &request
			break
		}
	}
	if waitRequest == nil {
		firstHandle.Cancel()
		for range firstHandle.Events() {
		}
		result, runErr := firstHandle.Wait()
		t.Fatalf("evaluation Session did not wait: %#v / %v / %#v", result, runErr, firstEvents)
	}
	restartedSessionRoot := filepath.Join(t.TempDir(), "sessions-after-restart")
	if err := copyCodingE2ESessionLog(firstSessionRoot, restartedSessionRoot); err != nil {
		firstHandle.Cancel()
		for range firstHandle.Events() {
		}
		_, _ = firstHandle.Wait()
		t.Fatal(err)
	}
	firstHandle.Cancel()
	for event := range firstHandle.Events() {
		firstEvents = append(firstEvents, event)
	}
	firstResult, firstRunErr := firstHandle.Wait()
	firstDuration := time.Since(firstStarted)
	if !errors.Is(firstRunErr, context.Canceled) || firstResult.Status != agent.RunStatusCanceled || firstProvider.Calls() != 1 {
		t.Fatalf("evaluation interrupted Run = %#v / %v / %#v", firstResult, firstRunErr, firstEvents)
	}
	firstBundle := roundTripCodingEvidence(t, firstResult, firstEvents, firstProfile.ToolInvocations())
	if err := firstProfile.Close(); err != nil {
		t.Fatalf("close first evaluation Profile: %v", err)
	}

	restartedStore, err := session.NewJSONLStore(restartedSessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := restartedStore.Load(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.PendingWait == nil || pending.PendingWait.ID != waitRequest.ID || pending.PendingTool == nil ||
		pending.PendingTool.ID != "evaluation-resumed-edit" {
		t.Fatalf("evaluation persisted Session = %#v", pending)
	}
	restartedProfile := newCodingE2EProfile(t, root, codingE2EProfileOptions{
		policy:   newCodingE2EPolicy(t, policyOptions),
		approver: capability.WaitApprover{},
	})
	restartedProvider := newFixedCodingProvider(
		func(request agent.ModelRequest) (agent.Message, error) {
			if _, err := codingE2EToolOutput(request, "apply_patch", false); err != nil {
				return agent.Message{}, err
			}
			return codingE2EToolCall("resume-status", "git_status", json.RawMessage(`{}`)), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			if _, err := codingE2EToolOutput(request, "git_status", false); err != nil {
				return agent.Message{}, err
			}
			return codingE2EToolCall(
				"resume-diff",
				"git_diff",
				json.RawMessage(`{"scope":"worktree","paths":["calc.go"]}`),
			), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			output, err := codingE2EToolOutput(request, "git_diff", false)
			if err != nil {
				return agent.Message{}, err
			}
			if !strings.Contains(output, "return first + second") {
				return agent.Message{}, errors.New("resumed diff does not contain the approved change")
			}
			return codingEvaluationFinalMessage("Resumed the Session, applied the approved edit, and inspected the diff"), nil
		},
	)
	restartedRuntime, err := agent.NewRuntime(agent.Options{
		Provider:     restartedProvider,
		ToolSource:   restartedProfile.ToolSource(),
		SessionStore: restartedStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	restartedStarted := time.Now()
	restartedHandle, err := restartedRuntime.Run(context.Background(), agent.RunRequest{
		AgentID:      "coding-evaluation",
		SessionID:    task.ID,
		Instructions: restartedProfile.Instructions(),
		Resume: &agent.WaitResponse{
			RequestID: waitRequest.ID,
			Payload:   json.RawMessage(`{"approved":true}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	restartedEvents, restartedResult, restartedRunErr := collectCodingE2ERun(restartedHandle)
	restartedDuration := time.Since(restartedStarted)
	if restartedRunErr != nil || restartedResult.Status != agent.RunStatusCompleted ||
		restartedResult.ProviderCalls != 3 || restartedResult.ToolCalls != 3 || restartedProvider.Calls() != 3 {
		t.Fatalf("evaluation resumed Run = %#v / %v / %#v", restartedResult, restartedRunErr, restartedEvents)
	}
	if !hasCodingE2EEvent(restartedEvents, agent.EventRunResumed) || hasCodingE2EEvent(restartedEvents, agent.EventRunWaiting) {
		t.Fatalf("evaluation resumed Events = %#v", restartedEvents)
	}
	restartedBundle := roundTripCodingEvidence(t, restartedResult, restartedEvents, restartedProfile.ToolInvocations())
	assertSessionFinished(t, restartedStore, task.ID)
	if status := runGit(t, root, "status", "--porcelain", "--untracked-files=all"); status != " M calc.go\n" {
		t.Fatalf("resumed evaluation Git status = %q", status)
	}
	assertCodingEvaluationHead(t, root, baselineCommit)
	return newCodingEvaluationTaskReport(
		t,
		task,
		codingeval.Assessment{
			Succeeded: true,
			Tests:     codingeval.TestsNotRun,
			Diff:      codingeval.DiffAccepted,
		},
		codingeval.RunObservation{
			Provider: firstProvider.Name(),
			Result:   firstResult,
			Events:   firstEvents,
			Evidence: firstBundle,
			Duration: firstDuration,
		},
		codingeval.RunObservation{
			Provider: restartedProvider.Name(),
			Result:   restartedResult,
			Events:   restartedEvents,
			Evidence: restartedBundle,
			Duration: restartedDuration,
		},
	)
}

func newCodingEvaluationWorkspace(t *testing.T) (string, string) {
	t.Helper()
	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go executable is unavailable")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable is unavailable")
	}
	root := t.TempDir()
	writeFile(t, root, "go.mod", codingE2EGoModContent)
	writeFile(t, root, "calc.go", codingE2ECalcBefore)
	writeFile(t, root, "calc_test.go", codingE2ECalcTest)
	writeFile(t, root, "README.md", codingEvaluationREADMEBefore)
	writeFile(t, root, "AGENTS.md", codingE2EInstructions)
	initializeRepository(t, root, []string{"AGENTS.md", "README.md", "calc.go", "calc_test.go", "go.mod"})
	return root, goExecutable
}

func allowCodingEvaluationPolicy(t *testing.T) capability.Policy {
	t.Helper()
	return newCodingE2EPolicy(t, capability.StaticPolicyOptions{Allow: []capability.Name{
		capability.FilesystemRead,
		capability.FilesystemWrite,
		capability.ProcessExecute,
		capability.GitRead,
	}})
}

func newCodingEvaluationSessionStore(t *testing.T) *session.JSONLStore {
	t.Helper()
	store, err := session.NewJSONLStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func runCompletedCodingEvaluation(
	t *testing.T,
	task codingeval.Task,
	profile *coding.Profile,
	store agent.SessionStore,
	provider *fixedCodingProvider,
) codingeval.RunObservation {
	t.Helper()
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:     provider,
		ToolSource:   profile.ToolSource(),
		SessionStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID:      "coding-evaluation",
		SessionID:    task.ID,
		Instructions: profile.Instructions(),
		Input:        []agent.Message{{Role: agent.RoleUser, Text: task.Prompt}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, result, runErr := collectCodingE2ERun(handle)
	duration := time.Since(started)
	if runErr != nil || result.Status != agent.RunStatusCompleted {
		t.Fatalf("Coding evaluation Run = %#v / %v / %#v", result, runErr, events)
	}
	assertSessionFinished(t, store, task.ID)
	bundle := roundTripCodingEvidence(t, result, events, profile.ToolInvocations())
	return codingeval.RunObservation{
		Provider: provider.Name(),
		Result:   result,
		Events:   events,
		Evidence: bundle,
		Duration: duration,
	}
}

func newCodingEvaluationTaskReport(
	t *testing.T,
	task codingeval.Task,
	assessment codingeval.Assessment,
	observations ...codingeval.RunObservation,
) codingeval.TaskReport {
	t.Helper()
	report, err := codingeval.NewTaskReport(task, assessment, observations...)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func codingEvaluationReadDigest(request agent.ModelRequest, path string) (string, error) {
	output, err := codingE2EToolOutput(request, "read_file", false)
	if err != nil {
		return "", err
	}
	var result struct {
		Path    string `json:"path"`
		Digest  string `json:"digest"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return "", fmt.Errorf("decode read_file output: %w", err)
	}
	if result.Path != path || result.Digest == "" {
		return "", fmt.Errorf("read_file output = %#v, want path %q with digest", result, path)
	}
	return result.Digest, nil
}

func requireCodingEvaluationCommandSuccess(output string, want bool) error {
	var result struct {
		ExitCode int  `json:"exit_code"`
		Success  bool `json:"success"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return fmt.Errorf("decode run_command output: %w", err)
	}
	if result.Success != want || (result.ExitCode == 0) != want {
		return fmt.Errorf("run_command success = %t exit_code = %d, want success %t", result.Success, result.ExitCode, want)
	}
	return nil
}

func codingEvaluationFinalMessage(text string) agent.Message {
	message := codingE2EFinalMessage(text)
	message.Usage = &agent.Usage{
		InputTokens:  10,
		OutputTokens: 2,
		TotalTokens:  12,
		CostMicros:   3,
	}
	return message
}

func codingEvaluationHead(t *testing.T, root string) string {
	t.Helper()
	return strings.TrimSpace(runGit(t, root, "rev-parse", "HEAD"))
}

func assertCodingEvaluationHead(t *testing.T, root, want string) {
	t.Helper()
	if got := codingEvaluationHead(t, root); got != want {
		t.Fatalf("Coding evaluation Git HEAD = %q, want %q", got, want)
	}
}

func TestCodingEvaluationReportRejectsMismatchedEvidence(t *testing.T) {
	tasks := codingeval.BaselineTasks()
	_, err := codingeval.NewTaskReport(
		tasks[0],
		codingeval.Assessment{Succeeded: true, Tests: codingeval.TestsNotRun, Diff: codingeval.DiffUnchanged},
		codingeval.RunObservation{
			Provider: "fixed",
			Result: agent.RunResult{
				RunID:  "run-result",
				Status: agent.RunStatusCompleted,
			},
			Events: []agent.Event{{
				Sequence: 1,
				Type:     agent.EventRunCompleted,
				RunID:    "run-result",
			}},
			Evidence: evidenceBundleForCodingEvaluationMismatch(),
		},
	)
	if err == nil || !strings.Contains(err.Error(), "Evidence Bundle Run identity") {
		t.Fatalf("NewTaskReport error = %v", err)
	}
}

func evidenceBundleForCodingEvaluationMismatch() evidence.Bundle {
	return evidence.Bundle{
		Run: evidence.RunDescriptor{
			ID:     "different-run",
			Status: agent.RunStatusCompleted,
		},
	}
}
