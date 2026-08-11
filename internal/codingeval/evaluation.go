// Package codingeval records content-free measurements for the fixed local
// Coding Profile evaluation baseline
package codingeval

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/evidence"
)

const reportVersion = 1

// TaskKind classifies one fixed Coding Profile evaluation task
type TaskKind string

const (
	// TaskInvestigation reads and explains an existing workspace without changing it
	TaskInvestigation TaskKind = "investigation"
	// TaskSmallEdit applies one bounded change without running a test suite
	TaskSmallEdit TaskKind = "small_edit"
	// TaskTestRepair observes a failing test, repairs it, and verifies the result
	TaskTestRepair TaskKind = "test_repair"
	// TaskSessionResume continues an approval wait from a persisted Session
	TaskSessionResume TaskKind = "session_resume"
)

const (
	investigationTaskID = "coding-investigation-v1"
	smallEditTaskID     = "coding-small-edit-v1"
	testRepairTaskID    = "coding-test-repair-v1"
	sessionResumeTaskID = "coding-session-resume-v1"
)

// Task defines one stable baseline prompt without embedding machine-specific paths
type Task struct {
	ID     string
	Kind   TaskKind
	Prompt string
}

var baselineTasks = []Task{
	{
		ID:     investigationTaskID,
		Kind:   TaskInvestigation,
		Prompt: "Inspect calc.go and calc_test.go, identify why TestAdd would fail, and do not modify the workspace",
	},
	{
		ID:     smallEditTaskID,
		Kind:   TaskSmallEdit,
		Prompt: "Change the README.md title from Evaluation Fixture to QED Evaluation Fixture, then inspect the Git change without running tests",
	},
	{
		ID:     testRepairTaskID,
		Kind:   TaskTestRepair,
		Prompt: "Run the Go tests, repair the failing Add implementation, rerun the tests, and inspect the resulting Git status and diff",
	},
	{
		ID:     sessionResumeTaskID,
		Kind:   TaskSessionResume,
		Prompt: "Apply the requested calc.go change after approval and continue from the persisted Session without repeating the original Provider request",
	},
}

// BaselineTasks returns an isolated copy of the ordered Stage 1 task set
func BaselineTasks() []Task {
	return append([]Task(nil), baselineTasks...)
}

// TestOutcome records whether tests were applicable and successful
type TestOutcome string

const (
	// TestsNotRun indicates that the task did not require a test command
	TestsNotRun TestOutcome = "not_run"
	// TestsPassed indicates that all task-required checks passed
	TestsPassed TestOutcome = "passed"
	// TestsFailed indicates that one or more task-required checks remained failing
	TestsFailed TestOutcome = "failed"
)

// DiffOutcome records the task oracle's judgment of the final workspace diff
type DiffOutcome string

const (
	// DiffUnchanged indicates that the task correctly left the workspace unchanged
	DiffUnchanged DiffOutcome = "unchanged"
	// DiffAccepted indicates that the final workspace change matched the task oracle
	DiffAccepted DiffOutcome = "accepted"
	// DiffRejected indicates that the final workspace change did not match the task oracle
	DiffRejected DiffOutcome = "rejected"
)

// Assessment contains task-specific facts supplied by a deterministic oracle
type Assessment struct {
	Succeeded         bool        `json:"succeeded"`
	Tests             TestOutcome `json:"tests"`
	Diff              DiffOutcome `json:"diff"`
	UnexpectedChanges int         `json:"unexpected_changes"`
}

// RunObservation supplies one completed Run and its matching Evidence Bundle
type RunObservation struct {
	Provider string
	Result   agent.RunResult
	Events   []agent.Event
	Evidence evidence.Bundle
	Duration time.Duration
}

// Metrics contains additive, content-free measurements for one or more Runs
type Metrics struct {
	Runs              int         `json:"runs"`
	ProviderCalls     int         `json:"provider_calls"`
	ToolCalls         int         `json:"tool_calls"`
	ToolFailures      int         `json:"tool_failures"`
	ApprovalRequests  int         `json:"approval_requests"`
	Resumes           int         `json:"resumes"`
	CanceledRuns      int         `json:"canceled_runs"`
	FailedRuns        int         `json:"failed_runs"`
	PolicyDenials     int         `json:"policy_denials"`
	EvidenceRecords   int         `json:"evidence_records"`
	Changes           int         `json:"changes"`
	SuccessfulChanges int         `json:"successful_changes"`
	Checks            int         `json:"checks"`
	SuccessfulChecks  int         `json:"successful_checks"`
	GitDiffs          int         `json:"git_diffs"`
	DurationMillis    int64       `json:"duration_ms"`
	Usage             agent.Usage `json:"usage"`
}

// RunReport identifies one observed Run without message or Tool content
type RunReport struct {
	RunID        string          `json:"run_id"`
	SessionID    string          `json:"session_id,omitempty"`
	Provider     string          `json:"provider"`
	Model        string          `json:"model,omitempty"`
	Status       agent.RunStatus `json:"status"`
	ConfigDigest string          `json:"config_digest,omitempty"`
	WorkspaceID  string          `json:"workspace_id,omitempty"`
	Metrics      Metrics         `json:"metrics"`
}

// TaskReport records one fixed task result and its content-free measurements
type TaskReport struct {
	Version    int         `json:"version"`
	TaskID     string      `json:"task_id"`
	TaskKind   TaskKind    `json:"task_kind"`
	TaskDigest string      `json:"task_digest"`
	Assessment Assessment  `json:"assessment"`
	DiffDigest string      `json:"diff_digest,omitempty"`
	Metrics    Metrics     `json:"metrics"`
	Runs       []RunReport `json:"runs"`
}

// Summary contains non-additive outcome counts for the complete baseline
type Summary struct {
	Tasks             int `json:"tasks"`
	SuccessfulTasks   int `json:"successful_tasks"`
	AcceptedDiffs     int `json:"accepted_diffs"`
	RejectedDiffs     int `json:"rejected_diffs"`
	UnchangedTasks    int `json:"unchanged_tasks"`
	PassedTestTasks   int `json:"passed_test_tasks"`
	FailedTestTasks   int `json:"failed_test_tasks"`
	TestsNotRunTasks  int `json:"tests_not_run_tasks"`
	UnexpectedChanges int `json:"unexpected_changes"`
}

// BaselineReport contains the complete ordered Stage 1 evaluation result
type BaselineReport struct {
	Version        int          `json:"version"`
	BaselineDigest string       `json:"baseline_digest"`
	Summary        Summary      `json:"summary"`
	Metrics        Metrics      `json:"metrics"`
	Tasks          []TaskReport `json:"tasks"`
}

// NewTaskReport validates and measures one task without retaining message content
func NewTaskReport(task Task, assessment Assessment, observations ...RunObservation) (TaskReport, error) {
	if err := validateTask(task); err != nil {
		return TaskReport{}, err
	}
	if err := validateAssessment(assessment); err != nil {
		return TaskReport{}, err
	}
	if len(observations) == 0 {
		return TaskReport{}, errors.New("Coding evaluation requires at least one Run observation")
	}

	report := TaskReport{
		Version:    reportVersion,
		TaskID:     task.ID,
		TaskKind:   task.Kind,
		TaskDigest: taskDigest(task),
		Assessment: assessment,
		Runs:       make([]RunReport, 0, len(observations)),
	}
	for index, observation := range observations {
		run, diffDigest, err := measureRun(observation)
		if err != nil {
			return TaskReport{}, fmt.Errorf("measure Coding evaluation Run[%d]: %w", index, err)
		}
		report.Runs = append(report.Runs, run)
		if diffDigest != "" {
			report.DiffDigest = diffDigest
		}
		if err := addMetrics(&report.Metrics, run.Metrics); err != nil {
			return TaskReport{}, fmt.Errorf("aggregate Coding evaluation Run[%d]: %w", index, err)
		}
	}
	if err := validateMeasuredTask(task, report); err != nil {
		return TaskReport{}, err
	}
	return report, nil
}

// NewBaselineReport validates one report for every fixed task and orders them canonically
func NewBaselineReport(reports ...TaskReport) (BaselineReport, error) {
	if len(reports) != len(baselineTasks) {
		return BaselineReport{}, fmt.Errorf(
			"Coding evaluation baseline has %d task reports, want %d",
			len(reports),
			len(baselineTasks),
		)
	}
	byID := make(map[string]TaskReport, len(reports))
	for _, report := range reports {
		if report.Version != reportVersion {
			return BaselineReport{}, fmt.Errorf("Coding evaluation task %q has unsupported version %d", report.TaskID, report.Version)
		}
		if _, duplicate := byID[report.TaskID]; duplicate {
			return BaselineReport{}, fmt.Errorf("Coding evaluation task %q is reported more than once", report.TaskID)
		}
		byID[report.TaskID] = report
	}

	baseline := BaselineReport{
		Version: reportVersion,
		Tasks:   make([]TaskReport, 0, len(baselineTasks)),
	}
	var taskDigests []string
	for _, task := range baselineTasks {
		report, ok := byID[task.ID]
		if !ok {
			return BaselineReport{}, fmt.Errorf("Coding evaluation task %q is missing", task.ID)
		}
		wantDigest := taskDigest(task)
		if report.TaskKind != task.Kind || report.TaskDigest != wantDigest {
			return BaselineReport{}, fmt.Errorf("Coding evaluation task %q does not match the fixed baseline", task.ID)
		}
		baseline.Tasks = append(baseline.Tasks, cloneTaskReport(report))
		taskDigests = append(taskDigests, wantDigest)
		addSummary(&baseline.Summary, report.Assessment)
		if err := addMetrics(&baseline.Metrics, report.Metrics); err != nil {
			return BaselineReport{}, fmt.Errorf("aggregate Coding evaluation task %q: %w", task.ID, err)
		}
	}
	baseline.BaselineDigest = digestParts("qed.coding-evaluation.baseline.v1", taskDigests...)
	return baseline, nil
}

func validateTask(task Task) error {
	if task.ID == "" || strings.TrimSpace(task.ID) != task.ID {
		return errors.New("Coding evaluation task ID is required without surrounding whitespace")
	}
	switch task.Kind {
	case TaskInvestigation, TaskSmallEdit, TaskTestRepair, TaskSessionResume:
	default:
		return fmt.Errorf("Coding evaluation task %q has unsupported kind %q", task.ID, task.Kind)
	}
	if strings.TrimSpace(task.Prompt) == "" {
		return fmt.Errorf("Coding evaluation task %q prompt is required", task.ID)
	}
	return nil
}

func validateAssessment(assessment Assessment) error {
	switch assessment.Tests {
	case TestsNotRun, TestsPassed, TestsFailed:
	default:
		return fmt.Errorf("Coding evaluation has unsupported test outcome %q", assessment.Tests)
	}
	switch assessment.Diff {
	case DiffUnchanged, DiffAccepted, DiffRejected:
	default:
		return fmt.Errorf("Coding evaluation has unsupported diff outcome %q", assessment.Diff)
	}
	if assessment.UnexpectedChanges < 0 {
		return errors.New("Coding evaluation unexpected changes must not be negative")
	}
	return nil
}

func validateMeasuredTask(task Task, report TaskReport) error {
	if report.Metrics.EvidenceRecords != report.Metrics.ToolCalls {
		return errors.New("Coding evaluation requires one Evidence record for every Tool call")
	}
	if report.Assessment.Diff == DiffAccepted && report.DiffDigest == "" {
		return errors.New("accepted Coding evaluation diff requires a content-free digest")
	}
	if report.Assessment.Tests == TestsPassed && report.Metrics.SuccessfulChecks == 0 {
		return errors.New("passed Coding evaluation tests require a successful check")
	}
	if report.Assessment.Tests == TestsFailed && report.Metrics.Checks == report.Metrics.SuccessfulChecks {
		return errors.New("failed Coding evaluation tests require a failed check")
	}
	if !report.Assessment.Succeeded {
		return nil
	}

	switch task.Kind {
	case TaskInvestigation:
		if report.Assessment.Tests != TestsNotRun || report.Assessment.Diff != DiffUnchanged ||
			report.Metrics.Changes != 0 {
			return errors.New("successful investigation must leave the workspace unchanged without running tests")
		}
	case TaskSmallEdit:
		if report.Assessment.Tests != TestsNotRun || report.Assessment.Diff != DiffAccepted ||
			report.Metrics.SuccessfulChanges != 1 {
			return errors.New("successful small edit requires one accepted change without running tests")
		}
	case TaskTestRepair:
		if report.Assessment.Tests != TestsPassed || report.Assessment.Diff != DiffAccepted ||
			report.Metrics.Checks < 2 || report.Metrics.SuccessfulChecks == report.Metrics.Checks ||
			report.Metrics.SuccessfulChanges != 1 {
			return errors.New("successful test repair must observe a failed check, apply one change, and pass verification")
		}
	case TaskSessionResume:
		if report.Assessment.Tests != TestsNotRun || report.Assessment.Diff != DiffAccepted ||
			report.Metrics.Runs != 2 || report.Metrics.CanceledRuns != 1 ||
			report.Metrics.ApprovalRequests != 1 || report.Metrics.Resumes != 1 ||
			report.Metrics.SuccessfulChanges != 1 {
			return errors.New("successful Session resume requires one interrupted approval Run and one resumed change")
		}
	}
	return nil
}

func measureRun(observation RunObservation) (RunReport, string, error) {
	if strings.TrimSpace(observation.Provider) == "" {
		return RunReport{}, "", errors.New("Provider identity is required")
	}
	if observation.Duration < 0 {
		return RunReport{}, "", errors.New("Run duration must not be negative")
	}
	result := observation.Result
	if result.RunID == "" {
		return RunReport{}, "", errors.New("Run ID is required")
	}
	if result.ProviderCalls < 0 || result.ToolCalls < 0 {
		return RunReport{}, "", errors.New("Run call counts must not be negative")
	}
	if err := validateUsage(result.Usage); err != nil {
		return RunReport{}, "", err
	}
	bundle := observation.Evidence
	if bundle.Run.ID != result.RunID || bundle.Run.Status != result.Status ||
		bundle.Run.SessionID != result.SessionID {
		return RunReport{}, "", errors.New("Evidence Bundle Run identity does not match Run result")
	}
	if len(bundle.Events) != len(observation.Events) {
		return RunReport{}, "", errors.New("Evidence Bundle Event count does not match observed Events")
	}
	for index, event := range observation.Events {
		if event.Sequence != uint64(index+1) || event.RunID != result.RunID {
			return RunReport{}, "", fmt.Errorf("Event[%d] has invalid sequence or Run identity", index)
		}
		persisted := bundle.Events[index]
		if persisted.Sequence != event.Sequence || persisted.Type != event.Type || persisted.RunID != event.RunID {
			return RunReport{}, "", fmt.Errorf("Evidence Event[%d] does not match the observed Event", index)
		}
	}
	if err := validateTerminalEvent(result.Status, observation.Events); err != nil {
		return RunReport{}, "", err
	}

	metrics := Metrics{
		Runs:            1,
		ProviderCalls:   result.ProviderCalls,
		ToolCalls:       result.ToolCalls,
		EvidenceRecords: len(bundle.ToolTrace),
		Changes:         len(bundle.Changes),
		Checks:          len(bundle.Checks),
		DurationMillis:  observation.Duration.Milliseconds(),
		Usage:           result.Usage,
	}
	switch result.Status {
	case agent.RunStatusCompleted:
	case agent.RunStatusCanceled:
		metrics.CanceledRuns = 1
	case agent.RunStatusFailed:
		metrics.FailedRuns = 1
	default:
		return RunReport{}, "", fmt.Errorf("Run has unsupported status %q", result.Status)
	}
	for _, event := range observation.Events {
		switch event.Type {
		case agent.EventRunWaiting:
			if event.WaitRequest != nil && event.WaitRequest.Kind == agent.WaitKindApproval {
				metrics.ApprovalRequests++
			}
		case agent.EventRunResumed:
			metrics.Resumes++
		}
	}
	for _, change := range bundle.Changes {
		if change.Succeeded {
			metrics.SuccessfulChanges++
		}
	}
	for _, check := range bundle.Checks {
		if check.Succeeded {
			metrics.SuccessfulChecks++
		}
	}
	var diffDigest string
	for _, invocation := range bundle.ToolTrace {
		if invocation.RunID != "" && invocation.RunID != result.RunID {
			return RunReport{}, "", errors.New("Tool Evidence Run identity does not match Run result")
		}
		if invocation.IsError || invocation.Error != "" {
			metrics.ToolFailures++
		}
		if invocation.PolicyOutcome == "deny" {
			metrics.PolicyDenials++
		}
		if invocation.Tool == "git_diff" {
			metrics.GitDiffs++
			if invocation.OutputDigest != "" {
				diffDigest = invocation.OutputDigest
			}
		}
	}

	return RunReport{
		RunID:        result.RunID,
		SessionID:    result.SessionID,
		Provider:     observation.Provider,
		Model:        bundle.Model.Name,
		Status:       result.Status,
		ConfigDigest: bundle.ConfigDigest,
		WorkspaceID:  bundle.WorkspaceState.ID,
		Metrics:      metrics,
	}, diffDigest, nil
}

func validateTerminalEvent(status agent.RunStatus, events []agent.Event) error {
	if len(events) == 0 {
		return errors.New("Run has no Events")
	}
	want := agent.EventRunCompleted
	switch status {
	case agent.RunStatusCompleted:
	case agent.RunStatusCanceled:
		want = agent.EventRunCanceled
	case agent.RunStatusFailed:
		want = agent.EventRunFailed
	default:
		return fmt.Errorf("Run has unsupported status %q", status)
	}
	if got := events[len(events)-1].Type; got != want {
		return fmt.Errorf("terminal Event is %q, want %q", got, want)
	}
	return nil
}

func addSummary(summary *Summary, assessment Assessment) {
	summary.Tasks++
	if assessment.Succeeded {
		summary.SuccessfulTasks++
	}
	switch assessment.Diff {
	case DiffAccepted:
		summary.AcceptedDiffs++
	case DiffRejected:
		summary.RejectedDiffs++
	case DiffUnchanged:
		summary.UnchangedTasks++
	}
	switch assessment.Tests {
	case TestsPassed:
		summary.PassedTestTasks++
	case TestsFailed:
		summary.FailedTestTasks++
	case TestsNotRun:
		summary.TestsNotRunTasks++
	}
	summary.UnexpectedChanges += assessment.UnexpectedChanges
}

func addMetrics(total *Metrics, value Metrics) error {
	var err error
	total.Runs, err = addInt(total.Runs, value.Runs)
	if err != nil {
		return err
	}
	fields := []struct {
		target *int
		value  int
	}{
		{&total.ProviderCalls, value.ProviderCalls},
		{&total.ToolCalls, value.ToolCalls},
		{&total.ToolFailures, value.ToolFailures},
		{&total.ApprovalRequests, value.ApprovalRequests},
		{&total.Resumes, value.Resumes},
		{&total.CanceledRuns, value.CanceledRuns},
		{&total.FailedRuns, value.FailedRuns},
		{&total.PolicyDenials, value.PolicyDenials},
		{&total.EvidenceRecords, value.EvidenceRecords},
		{&total.Changes, value.Changes},
		{&total.SuccessfulChanges, value.SuccessfulChanges},
		{&total.Checks, value.Checks},
		{&total.SuccessfulChecks, value.SuccessfulChecks},
		{&total.GitDiffs, value.GitDiffs},
	}
	for _, field := range fields {
		*field.target, err = addInt(*field.target, field.value)
		if err != nil {
			return err
		}
	}
	total.DurationMillis, err = addInt64(total.DurationMillis, value.DurationMillis)
	if err != nil {
		return err
	}
	total.Usage, err = addUsage(total.Usage, value.Usage, total.Runs-value.Runs > 0)
	return err
}

func addUsage(total, value agent.Usage, hadPrevious bool) (agent.Usage, error) {
	if err := validateUsage(value); err != nil {
		return agent.Usage{}, err
	}
	var err error
	total.InputTokens, err = addInt64(total.InputTokens, value.InputTokens)
	if err != nil {
		return agent.Usage{}, err
	}
	total.OutputTokens, err = addInt64(total.OutputTokens, value.OutputTokens)
	if err != nil {
		return agent.Usage{}, err
	}
	total.TotalTokens, err = addInt64(total.TotalTokens, value.TotalTokens)
	if err != nil {
		return agent.Usage{}, err
	}
	total.CostMicros, err = addInt64(total.CostMicros, value.CostMicros)
	if err != nil {
		return agent.Usage{}, err
	}
	detailsReported := value.InputTokenDetailsReported
	if hadPrevious {
		detailsReported = total.InputTokenDetailsReported && value.InputTokenDetailsReported
	}
	if !detailsReported {
		total.InputTokenDetailsReported = false
		total.UncachedInputTokens = 0
		total.CacheReadInputTokens = 0
		total.CacheWriteInputTokens = 0
		return total, nil
	}
	total.UncachedInputTokens, err = addInt64(total.UncachedInputTokens, value.UncachedInputTokens)
	if err != nil {
		return agent.Usage{}, err
	}
	total.CacheReadInputTokens, err = addInt64(total.CacheReadInputTokens, value.CacheReadInputTokens)
	if err != nil {
		return agent.Usage{}, err
	}
	total.CacheWriteInputTokens, err = addInt64(total.CacheWriteInputTokens, value.CacheWriteInputTokens)
	if err != nil {
		return agent.Usage{}, err
	}
	total.InputTokenDetailsReported = true
	return total, nil
}

func validateUsage(usage agent.Usage) error {
	values := []int64{
		usage.InputTokens,
		usage.OutputTokens,
		usage.TotalTokens,
		usage.UncachedInputTokens,
		usage.CacheReadInputTokens,
		usage.CacheWriteInputTokens,
		usage.CostMicros,
	}
	for _, value := range values {
		if value < 0 {
			return errors.New("Run Usage values must not be negative")
		}
	}
	return nil
}

func addInt(left, right int) (int, error) {
	if left < 0 || right < 0 {
		return 0, errors.New("Coding evaluation metrics must not be negative")
	}
	maximum := int(^uint(0) >> 1)
	if left > maximum-right {
		return 0, errors.New("Coding evaluation metric overflow")
	}
	return left + right, nil
}

func addInt64(left, right int64) (int64, error) {
	if left < 0 || right < 0 {
		return 0, errors.New("Coding evaluation metrics must not be negative")
	}
	const maximum = int64(^uint64(0) >> 1)
	if left > maximum-right {
		return 0, errors.New("Coding evaluation metric overflow")
	}
	return left + right, nil
}

func taskDigest(task Task) string {
	return digestParts("qed.coding-evaluation.task.v1", task.ID, string(task.Kind), task.Prompt)
}

func digestParts(domain string, values ...string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(domain))
	for _, value := range values {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(value))
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func cloneTaskReport(report TaskReport) TaskReport {
	report.Runs = append([]RunReport(nil), report.Runs...)
	return report
}
