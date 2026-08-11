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
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/evidence"
	editextension "github.com/qed-runtime/qed/extensions/edit"
	filesystemextension "github.com/qed-runtime/qed/extensions/filesystem"
	gitextension "github.com/qed-runtime/qed/extensions/git"
	processextension "github.com/qed-runtime/qed/extensions/process"
	"github.com/qed-runtime/qed/internal/chatauth"
	"github.com/qed-runtime/qed/internal/jsonstrict"
	"github.com/qed-runtime/qed/provider/openaicodex"
)

const (
	liveCodingE2EEnabledEnvironment     = "QED_LIVE_CODING_E2E"
	liveCodingE2EAuthProfileEnvironment = "QED_LIVE_CODING_AUTH_PROFILE"
	liveCodingE2EModelEnvironment       = "QED_LIVE_CODING_MODEL"
	protectedLiveCodingReasonPrefix     = "protected_live_e2e:"

	protectedLiveCodingPolicyCapabilities       = "policy_capabilities"
	protectedLiveCodingPolicyInvalidArguments   = "policy_invalid_arguments"
	protectedLiveCodingPolicyPatchFormat        = "policy_patch_format"
	protectedLiveCodingPolicyPatchTarget        = "policy_patch_target"
	protectedLiveCodingPolicyPatchOperation     = "policy_patch_operation"
	protectedLiveCodingPolicyPreconditionCount  = "policy_precondition_count"
	protectedLiveCodingPolicyPreconditionTarget = "policy_precondition_target"
	protectedLiveCodingPolicyPreconditionDigest = "policy_precondition_digest"
	protectedLiveCodingPolicyWorkspaceState     = "policy_workspace_state"

	codingE2EGoModContent = "module example.com/codinge2e\n\ngo 1.25.0\n"
	codingE2ECalcBefore   = "package codinge2e\n\nfunc Add(first, second int) int {\n\treturn first - second\n}\n"
	codingE2ECalcAfter    = "package codinge2e\n\nfunc Add(first, second int) int {\n\treturn first + second\n}\n"
	codingE2ECalcTest     = "package codinge2e\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"unexpected sum\")\n\t}\n}\n"
	codingE2EInstructions = "# Test instructions\n\nRun the Go test after editing\n"
)

func TestProtectedLiveCodingPolicyAllowsExpectedOperations(t *testing.T) {
	root, _ := newCodingE2EWorkspace(t)
	policy := &protectedLiveCodingPolicy{root: root}

	tests := []struct {
		name         string
		tool         string
		capabilities []capability.Name
		arguments    json.RawMessage
	}{
		{
			name:         "search synthetic workspace",
			tool:         "search_text",
			capabilities: []capability.Name{capability.FilesystemRead},
			arguments:    json.RawMessage(`{"query":"return first - second","paths":["calc.go"]}`),
		},
		{
			name:         "read synthetic source",
			tool:         "read_file",
			capabilities: []capability.Name{capability.FilesystemRead},
			arguments:    json.RawMessage(`{"path":"calc.go"}`),
		},
		{
			name:         "patch synthetic source",
			tool:         "apply_patch",
			capabilities: []capability.Name{capability.FilesystemWrite},
			arguments:    codingE2EPatchArguments(t, codingE2ECalcBefore),
		},
		{
			name:         "run offline test",
			tool:         "run_command",
			capabilities: []capability.Name{capability.ProcessExecute},
			arguments:    json.RawMessage(`{"argv":["go","test","./..."]}`),
		},
		{
			name:         "read Git status",
			tool:         "git_status",
			capabilities: []capability.Name{capability.GitRead},
			arguments:    json.RawMessage(`{}`),
		},
		{
			name:         "read bounded Git diff",
			tool:         "git_diff",
			capabilities: []capability.Name{capability.GitRead},
			arguments:    json.RawMessage(`{"scope":"worktree","paths":["calc.go"]}`),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := policy.Evaluate(context.Background(), capability.Request{
				CallID:       "allowed-call",
				Tool:         test.tool,
				Capabilities: test.capabilities,
				Arguments:    test.arguments,
			})
			if err != nil {
				t.Fatal(err)
			}
			if decision.Outcome != capability.OutcomeAllow {
				t.Fatalf("Policy decision = %#v", decision)
			}
		})
	}
}

func TestProtectedLiveCodingPolicyRejectsUnsafeOperations(t *testing.T) {
	root, _ := newCodingE2EWorkspace(t)
	policy := &protectedLiveCodingPolicy{root: root}

	tests := []struct {
		name         string
		tool         string
		capabilities []capability.Name
		arguments    json.RawMessage
	}{
		{
			name:         "read outside fixture",
			tool:         "read_file",
			capabilities: []capability.Name{capability.FilesystemRead},
			arguments:    json.RawMessage(`{"path":"../secret"}`),
		},
		{
			name:         "search outside fixture",
			tool:         "search_text",
			capabilities: []capability.Name{capability.FilesystemRead},
			arguments:    json.RawMessage(`{"query":"secret","paths":[".."]}`),
		},
		{
			name:         "patch another file",
			tool:         "apply_patch",
			capabilities: []capability.Name{capability.FilesystemWrite},
			arguments: mustJSON(t, map[string]any{
				"patch":         "--- a/go.mod\n+++ b/go.mod\n@@ -1,3 +1,3 @@\n-module example.com/codinge2e\n+module example.com/changed\n \n go 1.25.0\n",
				"preconditions": []map[string]string{{"path": "go.mod", "sha256": codingE2EDigest(codingE2EGoModContent)}},
			}),
		},
		{
			name:         "delete source",
			tool:         "apply_patch",
			capabilities: []capability.Name{capability.FilesystemWrite, capability.FilesystemDelete},
			arguments:    codingE2EPatchArguments(t, codingE2ECalcBefore),
		},
		{
			name:         "network command",
			tool:         "run_command",
			capabilities: []capability.Name{capability.ProcessExecute},
			arguments:    json.RawMessage(`{"argv":["curl","https://example.invalid"]}`),
		},
		{
			name:         "Git write command",
			tool:         "run_command",
			capabilities: []capability.Name{capability.ProcessExecute},
			arguments:    json.RawMessage(`{"argv":["git","commit","-am","unsafe"]}`),
		},
		{
			name:         "unexpected Go command",
			tool:         "run_command",
			capabilities: []capability.Name{capability.ProcessExecute},
			arguments:    json.RawMessage(`{"argv":["go","env"]}`),
		},
		{
			name:         "base-relative Git diff",
			tool:         "git_diff",
			capabilities: []capability.Name{capability.GitRead},
			arguments:    json.RawMessage(`{"scope":"base","base":"HEAD"}`),
		},
		{
			name:         "credential capability",
			tool:         "read_file",
			capabilities: []capability.Name{capability.FilesystemRead, capability.SecretsRead},
			arguments:    json.RawMessage(`{"path":"calc.go"}`),
		},
		{
			name:         "unknown Tool",
			tool:         "external_request",
			capabilities: []capability.Name{"network.request"},
			arguments:    json.RawMessage(`{}`),
		},
		{
			name:         "ambiguous JSON",
			tool:         "read_file",
			capabilities: []capability.Name{capability.FilesystemRead},
			arguments:    json.RawMessage(`{"path":"calc.go","path":"go.mod"}`),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := policy.Evaluate(context.Background(), capability.Request{
				CallID:       "denied-call",
				Tool:         test.tool,
				Capabilities: test.capabilities,
				Arguments:    test.arguments,
			})
			if err != nil {
				t.Fatal(err)
			}
			if decision.Outcome != capability.OutcomeDeny {
				t.Fatalf("Policy decision = %#v", decision)
			}
		})
	}
}

func TestProtectedLiveCodingPolicyRejectsCommandAfterUnexpectedMutation(t *testing.T) {
	root, _ := newCodingE2EWorkspace(t)
	policy := &protectedLiveCodingPolicy{root: root}
	writeFile(t, root, "unexpected.go", "package codinge2e\n")

	decision, err := policy.Evaluate(context.Background(), capability.Request{
		CallID:       "mutated-workspace",
		Tool:         processextension.RunCommandToolName,
		Capabilities: []capability.Name{capability.ProcessExecute},
		Arguments:    json.RawMessage(`{"argv":["go","test","./..."]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != capability.OutcomeDeny {
		t.Fatalf("Policy decision = %#v", decision)
	}
}

func TestProtectedLiveCodingPolicyClassifiesPatchRejection(t *testing.T) {
	root, _ := newCodingE2EWorkspace(t)
	policy := &protectedLiveCodingPolicy{root: root}

	tests := []struct {
		name      string
		arguments json.RawMessage
		wantClass string
	}{
		{
			name: "unsupported patch format",
			arguments: mustJSON(t, map[string]any{
				"patch": "*** Begin Patch\n*** Update File: calc.go\n@@\n-\treturn first - second\n+\treturn first + second\n*** End Patch\n",
				"preconditions": []map[string]string{{
					"path": "calc.go", "sha256": codingE2EDigest(codingE2ECalcBefore),
				}},
			}),
			wantClass: protectedLiveCodingPolicyPatchFormat,
		},
		{
			name: "unexpected patch target",
			arguments: mustJSON(t, map[string]any{
				"patch": "--- a/go.mod\n+++ b/go.mod\n@@ -1,1 +1,1 @@\n-module example.com/codinge2e\n+module example.com/changed\n",
				"preconditions": []map[string]string{{
					"path": "go.mod", "sha256": codingE2EDigest(codingE2EGoModContent),
				}},
			}),
			wantClass: protectedLiveCodingPolicyPatchTarget,
		},
		{
			name: "unexpected precondition digest",
			arguments: mustJSON(t, map[string]any{
				"patch": "--- a/calc.go\n+++ b/calc.go\n@@ -4,1 +4,1 @@\n-\treturn first - second\n+\treturn first + second\n",
				"preconditions": []map[string]string{{
					"path": "calc.go", "sha256": codingE2EDigest("stale\n"),
				}},
			}),
			wantClass: protectedLiveCodingPolicyPreconditionDigest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := policy.Evaluate(context.Background(), capability.Request{
				CallID:       "classified-rejection",
				Tool:         editextension.ApplyPatchToolName,
				Capabilities: []capability.Name{capability.FilesystemWrite},
				Arguments:    test.arguments,
			})
			if err != nil {
				t.Fatal(err)
			}
			if decision.Outcome != capability.OutcomeDeny {
				t.Fatalf("Policy decision = %#v", decision)
			}
			wantReasonPrefix := protectedLiveCodingReasonPrefix + test.wantClass + ":"
			if !strings.HasPrefix(decision.Reason, wantReasonPrefix) {
				t.Fatalf("Policy reason = %q, want prefix %q", decision.Reason, wantReasonPrefix)
			}
		})
	}
}

func TestProtectedLiveCodingPolicyAllowsEquivalentPatchHeaders(t *testing.T) {
	root, _ := newCodingE2EWorkspace(t)
	policy := &protectedLiveCodingPolicy{root: root}
	patches := []string{
		"--- calc.go\n+++ calc.go\n@@ -4,1 +4,1 @@\n-\treturn first - second\n+\treturn first + second\n",
		"diff --git calc.go calc.go\n--- calc.go\tbefore\n+++ calc.go\tafter\n@@ -4,1 +4,1 @@\n-\treturn first - second\n+\treturn first + second\n",
	}

	for index, patch := range patches {
		decision, err := policy.Evaluate(context.Background(), capability.Request{
			CallID:       fmt.Sprintf("equivalent-header-%d", index),
			Tool:         editextension.ApplyPatchToolName,
			Capabilities: []capability.Name{capability.FilesystemWrite},
			Arguments: mustJSON(t, map[string]any{
				"patch": patch,
				"preconditions": []map[string]string{{
					"path": "calc.go", "sha256": codingE2EDigest(codingE2ECalcBefore),
				}},
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		if decision.Outcome != capability.OutcomeAllow {
			t.Fatalf("Policy decision[%d] = %#v", index, decision)
		}
	}
}

func TestLiveCodingEventTraceOmitsContent(t *testing.T) {
	const secret = "TRACE_MUST_NOT_EXPOSE_THIS_VALUE"
	startedAt := time.Unix(1_800_000_000, 0).UTC()
	events := []agent.Event{
		{Sequence: 1, Type: agent.EventRunStarted, Time: startedAt},
		{
			Sequence: 2,
			Type:     agent.EventUserMessageAdded,
			Time:     startedAt.Add(time.Millisecond),
			Message:  &agent.Message{Role: agent.RoleUser, Text: secret},
		},
		{Sequence: 3, Type: agent.EventModelRequest, Time: startedAt.Add(10 * time.Millisecond)},
		{Sequence: 4, Type: agent.EventMessageStarted, Time: startedAt.Add(30 * time.Millisecond)},
		{Sequence: 5, Type: agent.EventMessageDelta, Time: startedAt.Add(40 * time.Millisecond), Delta: secret},
		{
			Sequence: 6,
			Type:     agent.EventMessageCompleted,
			Time:     startedAt.Add(50 * time.Millisecond),
			Message: &agent.Message{
				Role:          agent.RoleAssistant,
				Text:          secret,
				StopReason:    agent.StopReason(secret),
				RawStopReason: secret,
				Model:         secret,
				Usage:         &agent.Usage{InputTokens: 11, OutputTokens: 7, TotalTokens: 18},
				ToolCalls: []agent.ToolCall{
					{Name: filesystemextension.ReadFileToolName, Arguments: json.RawMessage(`{"path":"` + secret + `"}`)},
					{Name: secret, Arguments: json.RawMessage(`{"value":"` + secret + `"}`)},
				},
			},
		},
		{
			Sequence: 7,
			Type:     agent.EventToolStarted,
			Time:     startedAt.Add(60 * time.Millisecond),
			ToolCall: &agent.ToolCall{Name: filesystemextension.ReadFileToolName, Arguments: json.RawMessage(`{"path":"` + secret + `"}`)},
		},
		{
			Sequence:   8,
			Type:       agent.EventToolCompleted,
			Time:       startedAt.Add(70 * time.Millisecond),
			ToolCall:   &agent.ToolCall{Name: filesystemextension.ReadFileToolName, Arguments: json.RawMessage(`{"path":"` + secret + `"}`)},
			ToolResult: &agent.ToolResult{Name: filesystemextension.ReadFileToolName, Output: secret, IsError: true},
		},
		{Sequence: 9, Type: agent.EventModelRequest, Time: startedAt.Add(80 * time.Millisecond)},
		{Sequence: 10, Type: agent.EventMessageStarted, Time: startedAt.Add(90 * time.Millisecond)},
		{Sequence: 11, Type: agent.EventRunFailed, Time: startedAt.Add(180 * time.Millisecond), Error: secret},
	}

	trace := &liveCodingEventTrace{}
	var summaries []string
	for _, event := range events {
		if summary := trace.observe(event); summary != "" {
			summaries = append(summaries, summary)
		}
	}
	output := strings.Join(summaries, "\n")
	if strings.Contains(output, secret) {
		t.Fatalf("live trace exposed content = %q", output)
	}
	for _, expected := range []string{
		"event=model.request.started provider_call=1",
		"stop_reason=unknown tools=read_file,unregistered input_tokens=11 output_tokens=7",
		"event=tool.completed tool=read_file duration_ms=10 is_error=true error_class=execution_error",
		"event=run.failed provider_calls=2 active_provider_call=2 active_duration_ms=100",
	} {
		if !strings.Contains(output, expected) {
			t.Errorf("live trace %q does not contain %q", output, expected)
		}
	}
	lastSequence, lastType := liveCodingLastEvent(events)
	if lastSequence != 11 || lastType != agent.EventRunFailed {
		t.Fatalf("last live Event = %d %q", lastSequence, lastType)
	}
}

func TestSafeLiveCodingToolErrorClass(t *testing.T) {
	tests := []struct {
		name   string
		result *agent.ToolResult
		want   string
	}{
		{name: "missing result", want: "none"},
		{name: "successful result", result: &agent.ToolResult{Output: "ignored"}, want: "none"},
		{
			name: "classified policy rejection",
			result: &agent.ToolResult{
				IsError: true,
				Output: "capability denied: " + protectedLiveCodingPolicyReason(
					protectedLiveCodingPolicyPreconditionDigest,
					"digest does not match",
				),
			},
			want: protectedLiveCodingPolicyPreconditionDigest,
		},
		{name: "generic policy rejection", result: &agent.ToolResult{IsError: true, Output: "capability denied"}, want: "policy_denied"},
		{name: "approval required", result: &agent.ToolResult{IsError: true, Output: "capability approval required"}, want: "approval_required"},
		{name: "invalid JSON", result: &agent.ToolResult{IsError: true, Output: "tool arguments are not valid JSON"}, want: "invalid_json"},
		{name: "invalid arguments", result: &agent.ToolResult{IsError: true, Output: "decode apply_patch arguments: invalid value"}, want: "invalid_arguments"},
		{name: "invalid patch", result: &agent.ToolResult{IsError: true, Output: "Extension RPC failed: parse apply_patch patch: expected old file header"}, want: "invalid_patch"},
		{name: "precondition mismatch", result: &agent.ToolResult{IsError: true, Output: "patch precondition does not match the current file"}, want: "precondition_mismatch"},
		{name: "invalid precondition", result: &agent.ToolResult{IsError: true, Output: "apply_patch requires exactly one precondition"}, want: "invalid_precondition"},
		{name: "patch conflict", result: &agent.ToolResult{IsError: true, Output: "apply patch to file: hunk preimage does not match"}, want: "patch_conflict"},
		{name: "canceled", result: &agent.ToolResult{IsError: true, Output: context.Canceled.Error()}, want: "canceled"},
		{name: "deadline", result: &agent.ToolResult{IsError: true, Output: context.DeadlineExceeded.Error()}, want: "deadline_exceeded"},
		{name: "unclassified", result: &agent.ToolResult{IsError: true, Output: "TRACE_MUST_NOT_EXPOSE_THIS_VALUE"}, want: "execution_error"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := safeLiveCodingToolErrorClass(test.result); got != test.want {
				t.Fatalf("safeLiveCodingToolErrorClass() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestValidateProtectedLiveCodingEvidenceAllowsSafeRecovery(t *testing.T) {
	allowReason := "operation is inside the protected live Coding E2E boundary"
	digestReason := protectedLiveCodingPolicyReason(
		protectedLiveCodingPolicyPreconditionDigest,
		"sha256 must equal the digest returned by read_file",
	)
	invocations := []evidence.ToolInvocation{
		{
			Tool:          filesystemextension.ReadFileToolName,
			PolicyOutcome: string(capability.OutcomeAllow),
			PolicyReason:  allowReason,
		},
		{
			Tool:    editextension.ApplyPatchToolName,
			IsError: true,
			Error:   "resolve Tool capabilities: parse apply_patch patch: expected old file header",
		},
		{
			Tool:          editextension.ApplyPatchToolName,
			PolicyOutcome: string(capability.OutcomeDeny),
			PolicyReason:  digestReason,
			IsError:       true,
			Error:         capability.ErrDenied.Error() + ": " + digestReason,
		},
		{
			Tool:          editextension.ApplyPatchToolName,
			PolicyOutcome: string(capability.OutcomeAllow),
			PolicyReason:  allowReason,
		},
		{
			Tool:          processextension.RunCommandToolName,
			PolicyOutcome: string(capability.OutcomeAllow),
			PolicyReason:  allowReason,
		},
		{
			Tool:          gitextension.StatusToolName,
			PolicyOutcome: string(capability.OutcomeAllow),
			PolicyReason:  allowReason,
		},
		{
			Tool:          gitextension.DiffToolName,
			PolicyOutcome: string(capability.OutcomeAllow),
			PolicyReason:  allowReason,
		},
	}

	if err := validateProtectedLiveCodingEvidence(invocations); err != nil {
		t.Fatal(err)
	}
}

func TestValidateProtectedLiveCodingEvidenceRejectsUnsafeRecovery(t *testing.T) {
	targetReason := protectedLiveCodingPolicyReason(
		protectedLiveCodingPolicyPatchTarget,
		"patch must be one unified diff update of calc.go",
	)
	err := validateProtectedLiveCodingEvidence([]evidence.ToolInvocation{{
		Tool:          editextension.ApplyPatchToolName,
		PolicyOutcome: string(capability.OutcomeDeny),
		PolicyReason:  targetReason,
		IsError:       true,
		Error:         capability.ErrDenied.Error() + ": " + targetReason,
	}})
	if err == nil || !strings.Contains(err.Error(), protectedLiveCodingPolicyPatchTarget) {
		t.Fatalf("validateProtectedLiveCodingEvidence() error = %v", err)
	}
}

func TestProtectedLiveCodingProfileCompletesDeterministicLoop(t *testing.T) {
	root, goExecutable := newCodingE2EWorkspace(t)
	baselineCommit := strings.TrimSpace(runGit(t, root, "rev-parse", "HEAD"))
	profile := newCodingE2EProfile(t, root, codingE2EProfileOptions{
		policy:      &protectedLiveCodingPolicy{root: root},
		environment: protectedLiveCodingEnvironment(t, goExecutable),
	})
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:        &codingLoopProvider{goExecutable: "go"},
		ComponentSource: profile.ComponentSource(),
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID:      "protected-coding-e2e",
		Instructions: profile.Instructions(),
		Input: []agent.Message{{
			Role: agent.RoleUser,
			Text: "Fix Add and verify the synthetic change",
		}},
		Capabilities: []string{
			string(capability.FilesystemRead),
			string(capability.FilesystemWrite),
			string(capability.ProcessExecute),
			string(capability.GitRead),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, result, runErr := collectCodingE2ERun(handle)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if result.Status != agent.RunStatusCompleted || result.ToolCalls != 6 || result.ProviderCalls != 7 {
		t.Fatalf("protected Coding Run = %#v", result)
	}
	if err := verifyProtectedCodingWorkspace(root, protectedWorkspaceAfter); err != nil {
		t.Fatal(err)
	}
	if head := strings.TrimSpace(runGit(t, root, "rev-parse", "HEAD")); head != baselineCommit {
		t.Fatalf("Git HEAD changed from %q to %q", baselineCommit, head)
	}
	invocations := profile.ToolInvocations()
	assertProtectedLiveCodingEvidence(t, invocations)
	bundle := roundTripCodingEvidence(t, result, events, invocations)
	if len(bundle.Changes) != 1 || !bundle.Changes[0].Succeeded || len(bundle.Checks) != 1 ||
		!bundle.Checks[0].Succeeded {
		t.Fatalf("protected Coding Evidence Bundle = %#v", bundle)
	}
}

func TestLiveCodingProfileWritesTemporaryWorkspace(t *testing.T) {
	if os.Getenv(liveCodingE2EEnabledEnvironment) != "1" {
		t.Skip("set QED_LIVE_CODING_E2E=1 to run the protected live model E2E")
	}
	profileID := requiredLiveCodingEnvironment(t, liveCodingE2EAuthProfileEnvironment)
	model := requiredLiveCodingEnvironment(t, liveCodingE2EModelEnvironment)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	authService, err := chatauth.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	if err := authService.ValidateProfile(ctx, profileID); err != nil {
		t.Fatal(err)
	}
	authorizationSource, err := authService.CredentialSource(profileID)
	if err != nil {
		t.Fatal(err)
	}
	modelProvider, err := openaicodex.New(openaicodex.Config{
		ProfileID:           profileID,
		AuthorizationSource: authorizationSource,
		Model:               model,
	})
	if err != nil {
		t.Fatal(err)
	}

	root, goExecutable := newCodingE2EWorkspace(t)
	baselineCommit := strings.TrimSpace(runGit(t, root, "rev-parse", "HEAD"))
	profile := newCodingE2EProfile(t, root, codingE2EProfileOptions{
		policy:      &protectedLiveCodingPolicy{root: root},
		environment: protectedLiveCodingEnvironment(t, goExecutable),
	})
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:         modelProvider,
		ComponentSource:  profile.ComponentSource(),
		MaxProviderCalls: 12,
		MaxToolCalls:     24,
	})
	if err != nil {
		t.Fatal(err)
	}
	budget, err := agent.NewBudget(agent.BudgetLimits{
		MaxDuration:      3 * time.Minute,
		MaxProviderCalls: 12,
		MaxToolCalls:     24,
	})
	if err != nil {
		t.Fatal(err)
	}

	instructions := profile.Instructions() + `

This is a protected live verification using only a synthetic temporary repository.
Change only calc.go, replacing "return first - second" with "return first + second".
Read calc.go before editing and pass its digest as the apply_patch precondition.
Run no command before the edit. After editing, call run_command with exactly {"argv":["go","test","./..."]}.
Then inspect git_status and git_diff for calc.go. Do not access absolute paths, use network Tools, or perform Git writes.`
	handle, err := runtime.Run(ctx, agent.RunRequest{
		AgentID:      "live-coding-e2e",
		Instructions: instructions,
		Input: []agent.Message{{
			Role: agent.RoleUser,
			Text: "Fix the synthetic Add implementation, run the required test, and inspect the resulting Git change",
		}},
		Budget:   budget,
		Deadline: time.Now().Add(3 * time.Minute),
		Capabilities: []string{
			string(capability.FilesystemRead),
			string(capability.FilesystemWrite),
			string(capability.ProcessExecute),
			string(capability.GitRead),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, result, runErr := collectLiveCodingE2ERun(t, handle)
	if runErr != nil {
		lastSequence, lastEvent := liveCodingLastEvent(events)
		t.Fatalf(
			"live Coding Run failed: status=%q provider_calls=%d tool_calls=%d messages=%d "+
				"tool_evidence=%d input_tokens=%d output_tokens=%d total_tokens=%d "+
				"last_sequence=%d last_event=%q: %v",
			result.Status,
			result.ProviderCalls,
			result.ToolCalls,
			len(result.Messages),
			len(profile.ToolInvocations()),
			result.Usage.InputTokens,
			result.Usage.OutputTokens,
			result.Usage.TotalTokens,
			lastSequence,
			lastEvent,
			runErr,
		)
	}
	if result.Status != agent.RunStatusCompleted {
		t.Fatalf("live Coding Run status = %q", result.Status)
	}
	if err := verifyProtectedCodingWorkspace(root, protectedWorkspaceAfter); err != nil {
		t.Fatal(err)
	}
	if head := strings.TrimSpace(runGit(t, root, "rev-parse", "HEAD")); head != baselineCommit {
		t.Fatalf("Git HEAD changed from %q to %q", baselineCommit, head)
	}
	if status := runGit(t, root, "status", "--porcelain", "--untracked-files=all"); status != " M calc.go\n" {
		t.Fatalf("Git status = %q", status)
	}

	invocations := profile.ToolInvocations()
	assertProtectedLiveCodingEvidence(t, invocations)
	bundle := roundTripCodingEvidence(t, result, events, invocations)
	changeAttempts := countLiveCodingToolInvocations(invocations, editextension.ApplyPatchToolName)
	checkAttempts := countLiveCodingToolInvocations(invocations, processextension.RunCommandToolName)
	if bundle.Run.Status != agent.RunStatusCompleted || len(bundle.Changes) != changeAttempts ||
		len(bundle.Checks) != checkAttempts || len(bundle.ToolTrace) != len(invocations) {
		t.Fatalf("live Coding Evidence Bundle = %#v", bundle)
	}
	successfulChange := 0
	successfulCheck := false
	for _, change := range bundle.Changes {
		if change.Succeeded {
			successfulChange++
		}
	}
	for _, check := range bundle.Checks {
		if check.Succeeded {
			successfulCheck = true
		}
	}
	if successfulChange != 1 || !successfulCheck {
		t.Fatalf("live Coding Evidence changes/checks = %#v / %#v", bundle.Changes, bundle.Checks)
	}
}

type protectedLiveCodingPolicy struct {
	root string
}

func (policy *protectedLiveCodingPolicy) Evaluate(ctx context.Context, request capability.Request) (capability.Decision, error) {
	if policy == nil {
		return capability.Decision{}, errors.New("protected live Coding Policy must not be nil")
	}
	if ctx == nil {
		return capability.Decision{}, errors.New("protected live Coding Policy context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return capability.Decision{}, err
	}

	allowed := false
	denialReason := protectedLiveCodingPolicyReason(
		"policy_denied",
		"operation is outside the protected live Coding E2E boundary",
	)
	switch request.Tool {
	case filesystemextension.SearchTextToolName:
		allowed = protectedCapabilitiesMatch(request.Capabilities, capability.FilesystemRead) &&
			policy.allowSearch(request.Arguments)
	case filesystemextension.ReadFileToolName:
		allowed = protectedCapabilitiesMatch(request.Capabilities, capability.FilesystemRead) &&
			policy.allowRead(request.Arguments)
	case editextension.ApplyPatchToolName:
		if !protectedCapabilitiesMatch(request.Capabilities, capability.FilesystemWrite) {
			denialReason = protectedLiveCodingPolicyReason(
				protectedLiveCodingPolicyCapabilities,
				"apply_patch requires only filesystem.write",
			)
			break
		}
		allowed, denialReason = policy.allowPatch(request.Arguments)
	case processextension.RunCommandToolName:
		allowed = protectedCapabilitiesMatch(request.Capabilities, capability.ProcessExecute) &&
			policy.allowCommand(request.Arguments)
	case gitextension.StatusToolName:
		allowed = protectedCapabilitiesMatch(request.Capabilities, capability.GitRead) &&
			policy.allowGitStatus(request.Arguments)
	case gitextension.DiffToolName:
		allowed = protectedCapabilitiesMatch(request.Capabilities, capability.GitRead) &&
			policy.allowGitDiff(request.Arguments)
	}
	if !allowed {
		return capability.Decision{
			Outcome: capability.OutcomeDeny,
			Reason:  denialReason,
		}, nil
	}
	return capability.Decision{
		Outcome: capability.OutcomeAllow,
		Reason:  "operation is inside the protected live Coding E2E boundary",
	}, nil
}

func (policy *protectedLiveCodingPolicy) allowSearch(arguments json.RawMessage) bool {
	var input struct {
		Query         string   `json:"query"`
		Paths         []string `json:"paths,omitempty"`
		Mode          string   `json:"mode,omitempty"`
		CaseSensitive *bool    `json:"case_sensitive,omitempty"`
		MaxResults    int      `json:"max_results,omitempty"`
	}
	if !decodeProtectedArguments(arguments, &input) || input.Query == "" {
		return false
	}
	for _, path := range input.Paths {
		if !protectedCodingReadPath(path, true) {
			return false
		}
	}
	return verifyProtectedCodingWorkspace(policy.root, protectedWorkspaceEither) == nil
}

func (policy *protectedLiveCodingPolicy) allowRead(arguments json.RawMessage) bool {
	var input struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line,omitempty"`
		EndLine   int    `json:"end_line,omitempty"`
	}
	if !decodeProtectedArguments(arguments, &input) || !protectedCodingReadPath(input.Path, false) {
		return false
	}
	return verifyProtectedCodingWorkspace(policy.root, protectedWorkspaceEither) == nil
}

func (policy *protectedLiveCodingPolicy) allowPatch(arguments json.RawMessage) (bool, string) {
	var input struct {
		Patch         string `json:"patch"`
		Preconditions []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256,omitempty"`
			Absent bool   `json:"absent,omitempty"`
		} `json:"preconditions"`
	}
	if !decodeProtectedArguments(arguments, &input) {
		return false, protectedLiveCodingPolicyReason(
			protectedLiveCodingPolicyInvalidArguments,
			"apply_patch arguments must be one strict JSON object",
		)
	}
	if class := protectedCodingPatchPolicyClass(input.Patch); class != "" {
		return false, protectedLiveCodingPolicyReason(
			class,
			"patch must be one unified diff update of calc.go",
		)
	}
	if len(input.Preconditions) != 1 {
		return false, protectedLiveCodingPolicyReason(
			protectedLiveCodingPolicyPreconditionCount,
			"apply_patch requires exactly one precondition",
		)
	}
	condition := input.Preconditions[0]
	if condition.Path != "calc.go" || condition.Absent {
		return false, protectedLiveCodingPolicyReason(
			protectedLiveCodingPolicyPreconditionTarget,
			"precondition must identify the existing calc.go file",
		)
	}
	if condition.SHA256 != codingE2EDigest(codingE2ECalcBefore) {
		return false, protectedLiveCodingPolicyReason(
			protectedLiveCodingPolicyPreconditionDigest,
			"sha256 must equal the digest returned by read_file",
		)
	}
	if verifyProtectedCodingWorkspace(policy.root, protectedWorkspaceBefore) != nil {
		return false, protectedLiveCodingPolicyReason(
			protectedLiveCodingPolicyWorkspaceState,
			"workspace is no longer in the expected pre-edit state",
		)
	}
	return true, ""
}

func (policy *protectedLiveCodingPolicy) allowCommand(arguments json.RawMessage) bool {
	var input struct {
		Argv      []string `json:"argv"`
		CWD       string   `json:"cwd,omitempty"`
		TimeoutMS int64    `json:"timeout_ms,omitempty"`
	}
	if !decodeProtectedArguments(arguments, &input) || len(input.Argv) != 3 || input.Argv[0] != "go" ||
		input.Argv[1] != "test" || input.Argv[2] != "./..." || (input.CWD != "" && input.CWD != ".") ||
		input.TimeoutMS < 0 || input.TimeoutMS > int64((30*time.Second)/time.Millisecond) {
		return false
	}
	return verifyProtectedCodingWorkspace(policy.root, protectedWorkspaceEither) == nil
}

func (policy *protectedLiveCodingPolicy) allowGitStatus(arguments json.RawMessage) bool {
	var input struct{}
	return decodeProtectedArguments(arguments, &input) &&
		verifyProtectedCodingWorkspace(policy.root, protectedWorkspaceEither) == nil
}

func (policy *protectedLiveCodingPolicy) allowGitDiff(arguments json.RawMessage) bool {
	var input struct {
		Scope        string   `json:"scope,omitempty"`
		Base         string   `json:"base,omitempty"`
		Paths        []string `json:"paths,omitempty"`
		ContextLines *int     `json:"context_lines,omitempty"`
	}
	if !decodeProtectedArguments(arguments, &input) || (input.Scope != "" && input.Scope != "worktree") ||
		input.Base != "" {
		return false
	}
	for _, path := range input.Paths {
		if path != "calc.go" {
			return false
		}
	}
	if input.ContextLines != nil && (*input.ContextLines < 0 || *input.ContextLines > 20) {
		return false
	}
	return verifyProtectedCodingWorkspace(policy.root, protectedWorkspaceEither) == nil
}

type protectedWorkspaceState int

const (
	protectedWorkspaceBefore protectedWorkspaceState = iota + 1
	protectedWorkspaceAfter
	protectedWorkspaceEither
)

func verifyProtectedCodingWorkspace(root string, state protectedWorkspaceState) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return errors.New("read protected workspace")
	}
	expected := map[string]string{
		"AGENTS.md":    codingE2EInstructions,
		"calc_test.go": codingE2ECalcTest,
		"go.mod":       codingE2EGoModContent,
	}
	seen := make(map[string]bool, len(expected)+2)
	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return errors.New("protected workspace .git is not a directory")
			}
			seen[name] = true
			continue
		}
		if name != "calc.go" {
			if _, ok := expected[name]; !ok {
				return fmt.Errorf("protected workspace contains unexpected path %q", name)
			}
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("protected workspace path %q is not a regular file", name)
		}
		content, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return fmt.Errorf("read protected workspace path %q", name)
		}
		if name == "calc.go" {
			before := string(content) == codingE2ECalcBefore
			after := string(content) == codingE2ECalcAfter
			if (state == protectedWorkspaceBefore && !before) || (state == protectedWorkspaceAfter && !after) ||
				(state == protectedWorkspaceEither && !before && !after) {
				return errors.New("protected workspace calc.go has unexpected content")
			}
		} else if string(content) != expected[name] {
			return fmt.Errorf("protected workspace path %q has unexpected content", name)
		}
		seen[name] = true
	}
	for _, name := range []string{".git", "AGENTS.md", "calc.go", "calc_test.go", "go.mod"} {
		if !seen[name] {
			return fmt.Errorf("protected workspace path %q is missing", name)
		}
	}
	return nil
}

func protectedCapabilitiesMatch(actual []capability.Name, expected ...capability.Name) bool {
	if len(actual) != len(expected) {
		return false
	}
	seen := make(map[capability.Name]struct{}, len(actual))
	for _, name := range actual {
		seen[name] = struct{}{}
	}
	if len(seen) != len(expected) {
		return false
	}
	for _, name := range expected {
		if _, ok := seen[name]; !ok {
			return false
		}
	}
	return true
}

func decodeProtectedArguments(arguments json.RawMessage, target any) bool {
	trimmed := strings.TrimSpace(string(arguments))
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return false
	}
	return jsonstrict.Decode(arguments, 64<<10, target) == nil
}

func protectedCodingReadPath(path string, allowRoot bool) bool {
	if allowRoot && path == "." {
		return true
	}
	switch path {
	case "AGENTS.md", "calc.go", "calc_test.go", "go.mod":
		return true
	default:
		return false
	}
}

func protectedCodingPatchPolicyClass(patch string) string {
	if patch == "" || len(patch) > 4096 || strings.IndexByte(patch, 0) >= 0 {
		return protectedLiveCodingPolicyPatchFormat
	}
	oldHeaders := 0
	newHeaders := 0
	diffHeaders := 0
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			diffHeaders++
		case strings.HasPrefix(line, "--- "):
			oldHeaders++
			path, operation, ok := protectedCodingPatchHeaderPath(strings.TrimPrefix(line, "--- "))
			if operation {
				return protectedLiveCodingPolicyPatchOperation
			}
			if !ok || path != "calc.go" {
				return protectedLiveCodingPolicyPatchTarget
			}
		case strings.HasPrefix(line, "+++ "):
			newHeaders++
			path, operation, ok := protectedCodingPatchHeaderPath(strings.TrimPrefix(line, "+++ "))
			if operation {
				return protectedLiveCodingPolicyPatchOperation
			}
			if !ok || path != "calc.go" {
				return protectedLiveCodingPolicyPatchTarget
			}
		case strings.HasPrefix(line, "new file mode "), strings.HasPrefix(line, "deleted file mode "),
			strings.HasPrefix(line, "rename from "), strings.HasPrefix(line, "rename to "),
			strings.HasPrefix(line, "copy from "), strings.HasPrefix(line, "copy to "),
			line == "GIT binary patch", strings.HasPrefix(line, "Binary files "):
			return protectedLiveCodingPolicyPatchOperation
		}
	}
	if oldHeaders != 1 || newHeaders != 1 || diffHeaders > 1 {
		return protectedLiveCodingPolicyPatchFormat
	}
	return ""
}

func protectedCodingPatchHeaderPath(value string) (path string, operation bool, ok bool) {
	if before, _, found := strings.Cut(value, "\t"); found {
		value = before
	}
	if value == "/dev/null" {
		return "", true, true
	}
	if value == "" || strings.HasPrefix(value, "\"") {
		return "", false, false
	}
	if strings.HasPrefix(value, "a/") || strings.HasPrefix(value, "b/") {
		value = value[2:]
	}
	if filepath.IsAbs(value) || filepath.VolumeName(value) != "" {
		return "", false, false
	}
	cleaned := filepath.Clean(filepath.FromSlash(value))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", false, false
	}
	return filepath.ToSlash(cleaned), false, true
}

func protectedLiveCodingPolicyReason(class, guidance string) string {
	return protectedLiveCodingReasonPrefix + class + ": " + guidance
}

func protectedLiveCodingEnvironment(t *testing.T, goExecutable string) map[string]string {
	t.Helper()
	gitExecutable, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git executable is unavailable")
	}
	pathDirectories := make([]string, 0, 2)
	seen := make(map[string]struct{}, 2)
	for _, executable := range []string{goExecutable, gitExecutable} {
		absolute, err := filepath.Abs(executable)
		if err != nil {
			t.Fatal(err)
		}
		directory := filepath.Dir(absolute)
		if _, ok := seen[directory]; !ok {
			seen[directory] = struct{}{}
			pathDirectories = append(pathDirectories, directory)
		}
	}
	return map[string]string{
		"CGO_ENABLED": "0",
		"GOCACHE":     t.TempDir(),
		"GOENV":       "off",
		"GOFLAGS":     "-mod=readonly",
		"GOMODCACHE":  t.TempDir(),
		"GONOSUMDB":   "*",
		"GOPATH":      t.TempDir(),
		"GOPROXY":     "off",
		"GOSUMDB":     "off",
		"GOTELEMETRY": "off",
		"GOTOOLCHAIN": "local",
		"GOVCS":       "*:off",
		"GOWORK":      "off",
		"HOME":        t.TempDir(),
		"PATH":        strings.Join(pathDirectories, string(os.PathListSeparator)),
		"TMPDIR":      t.TempDir(),
	}
}

func requiredLiveCodingEnvironment(t *testing.T, name string) string {
	t.Helper()
	value, ok := os.LookupEnv(name)
	if !ok || value == "" {
		t.Fatalf("%s is required when %s=1", name, liveCodingE2EEnabledEnvironment)
	}
	if strings.TrimSpace(value) != value {
		t.Fatalf("%s must not have surrounding whitespace", name)
	}
	return value
}

func collectLiveCodingE2ERun(t *testing.T, handle *agent.RunHandle) ([]agent.Event, agent.RunResult, error) {
	t.Helper()
	trace := &liveCodingEventTrace{}
	var events []agent.Event
	for event := range handle.Events() {
		events = append(events, event)
		if summary := trace.observe(event); summary != "" {
			t.Log(summary)
		}
	}
	result, err := handle.Wait()
	return events, result, err
}

type liveCodingEventTrace struct {
	startedAt         time.Time
	providerStartedAt time.Time
	toolStartedAt     time.Time
	providerCall      int
}

func (trace *liveCodingEventTrace) observe(event agent.Event) string {
	if trace.startedAt.IsZero() {
		trace.startedAt = event.Time
	}
	elapsed := event.Time.Sub(trace.startedAt).Milliseconds()
	prefix := fmt.Sprintf("live trace elapsed_ms=%d sequence=%d event=%s", elapsed, event.Sequence, event.Type)

	switch event.Type {
	case agent.EventRunStarted, agent.EventContextCompacted:
		return prefix
	case agent.EventModelRequest:
		trace.providerCall++
		trace.providerStartedAt = event.Time
		return fmt.Sprintf("%s provider_call=%d", prefix, trace.providerCall)
	case agent.EventMessageStarted:
		return fmt.Sprintf(
			"%s provider_call=%d response_started_ms=%d",
			prefix,
			trace.providerCall,
			event.Time.Sub(trace.providerStartedAt).Milliseconds(),
		)
	case agent.EventMessageCompleted:
		duration := event.Time.Sub(trace.providerStartedAt).Milliseconds()
		trace.providerStartedAt = time.Time{}
		if event.Message == nil {
			return fmt.Sprintf("%s provider_call=%d duration_ms=%d", prefix, trace.providerCall, duration)
		}
		inputTokens := int64(0)
		outputTokens := int64(0)
		if event.Message.Usage != nil {
			inputTokens = event.Message.Usage.InputTokens
			outputTokens = event.Message.Usage.OutputTokens
		}
		return fmt.Sprintf(
			"%s provider_call=%d duration_ms=%d stop_reason=%s tools=%s input_tokens=%d output_tokens=%d",
			prefix,
			trace.providerCall,
			duration,
			safeLiveCodingStopReason(event.Message.StopReason),
			safeLiveCodingToolNames(event.Message.ToolCalls),
			inputTokens,
			outputTokens,
		)
	case agent.EventToolStarted:
		trace.toolStartedAt = event.Time
		tool := "unknown"
		if event.ToolCall != nil {
			tool = safeLiveCodingToolName(event.ToolCall.Name)
		}
		return fmt.Sprintf("%s tool=%s", prefix, tool)
	case agent.EventToolCompleted:
		duration := event.Time.Sub(trace.toolStartedAt).Milliseconds()
		trace.toolStartedAt = time.Time{}
		tool := "unknown"
		isError := false
		if event.ToolResult != nil {
			tool = safeLiveCodingToolName(event.ToolResult.Name)
			isError = event.ToolResult.IsError
		} else if event.ToolCall != nil {
			tool = safeLiveCodingToolName(event.ToolCall.Name)
		}
		summary := fmt.Sprintf("%s tool=%s duration_ms=%d is_error=%t", prefix, tool, duration, isError)
		if isError {
			summary += " error_class=" + safeLiveCodingToolErrorClass(event.ToolResult)
		}
		return summary
	case agent.EventRunCompleted, agent.EventRunFailed, agent.EventRunCanceled:
		if trace.providerStartedAt.IsZero() {
			return fmt.Sprintf("%s provider_calls=%d", prefix, trace.providerCall)
		}
		return fmt.Sprintf(
			"%s provider_calls=%d active_provider_call=%d active_duration_ms=%d",
			prefix,
			trace.providerCall,
			trace.providerCall,
			event.Time.Sub(trace.providerStartedAt).Milliseconds(),
		)
	default:
		return ""
	}
}

func safeLiveCodingToolErrorClass(result *agent.ToolResult) string {
	if result == nil || !result.IsError {
		return "none"
	}
	output := result.Output
	for _, class := range []string{
		protectedLiveCodingPolicyCapabilities,
		protectedLiveCodingPolicyInvalidArguments,
		protectedLiveCodingPolicyPatchFormat,
		protectedLiveCodingPolicyPatchTarget,
		protectedLiveCodingPolicyPatchOperation,
		protectedLiveCodingPolicyPreconditionCount,
		protectedLiveCodingPolicyPreconditionTarget,
		protectedLiveCodingPolicyPreconditionDigest,
		protectedLiveCodingPolicyWorkspaceState,
	} {
		if strings.Contains(output, protectedLiveCodingReasonPrefix+class+":") {
			return class
		}
	}
	switch {
	case strings.Contains(output, capability.ErrApprovalRequired.Error()):
		return "approval_required"
	case strings.Contains(output, capability.ErrDenied.Error()):
		return "policy_denied"
	case strings.Contains(output, "tool arguments are not valid JSON"):
		return "invalid_json"
	case strings.Contains(output, "decode apply_patch arguments:"):
		return "invalid_arguments"
	case strings.Contains(output, "apply_patch patch is required"),
		strings.Contains(output, "apply_patch patch exceeds"),
		strings.Contains(output, "parse apply_patch patch:"):
		return "invalid_patch"
	case strings.Contains(output, "does not match the current file"):
		return "precondition_mismatch"
	case strings.Contains(output, "precondition"):
		return "invalid_precondition"
	case strings.Contains(output, "apply patch to "):
		return "patch_conflict"
	case strings.Contains(output, context.DeadlineExceeded.Error()):
		return "deadline_exceeded"
	case strings.Contains(output, context.Canceled.Error()):
		return "canceled"
	default:
		return "execution_error"
	}
}

func safeLiveCodingToolNames(calls []agent.ToolCall) string {
	if len(calls) == 0 {
		return "none"
	}
	names := make([]string, len(calls))
	for index, call := range calls {
		names[index] = safeLiveCodingToolName(call.Name)
	}
	return strings.Join(names, ",")
}

func safeLiveCodingToolName(name string) string {
	switch name {
	case filesystemextension.SearchTextToolName,
		filesystemextension.ReadFileToolName,
		editextension.ApplyPatchToolName,
		processextension.RunCommandToolName,
		gitextension.StatusToolName,
		gitextension.DiffToolName:
		return name
	default:
		return "unregistered"
	}
}

func safeLiveCodingStopReason(reason agent.StopReason) agent.StopReason {
	switch reason {
	case agent.StopReasonEndTurn,
		agent.StopReasonToolUse,
		agent.StopReasonMaxTokens,
		agent.StopReasonRefusal,
		agent.StopReasonContentFilter:
		return reason
	default:
		return agent.StopReasonUnknown
	}
}

func liveCodingLastEvent(events []agent.Event) (uint64, agent.EventType) {
	if len(events) == 0 {
		return 0, ""
	}
	last := events[len(events)-1]
	return last.Sequence, last.Type
}

func assertProtectedLiveCodingEvidence(t *testing.T, invocations []evidence.ToolInvocation) {
	t.Helper()
	if err := validateProtectedLiveCodingEvidence(invocations); err != nil {
		t.Fatal(err)
	}
}

func validateProtectedLiveCodingEvidence(invocations []evidence.ToolInvocation) error {
	requiredSuccessful := map[string]bool{
		filesystemextension.ReadFileToolName: false,
		editextension.ApplyPatchToolName:     false,
		processextension.RunCommandToolName:  false,
		gitextension.StatusToolName:          false,
		gitextension.DiffToolName:            false,
	}
	allowed := map[string]bool{
		filesystemextension.SearchTextToolName: true,
		filesystemextension.ReadFileToolName:   true,
		editextension.ApplyPatchToolName:       true,
		processextension.RunCommandToolName:    true,
		gitextension.StatusToolName:            true,
		gitextension.DiffToolName:              true,
	}
	for index, invocation := range invocations {
		if !allowed[invocation.Tool] {
			return fmt.Errorf("live Coding Evidence[%d] used unexpected Tool %q", index, invocation.Tool)
		}
		failed := invocation.IsError || invocation.Error != ""
		if failed {
			switch invocation.Tool {
			case editextension.ApplyPatchToolName:
				if err := validateRecoverableLiveCodingPatchFailure(invocation); err != nil {
					return fmt.Errorf("live Coding Evidence[%d] apply_patch failure: %w", index, err)
				}
			case processextension.RunCommandToolName:
				if invocation.Error != "" {
					return fmt.Errorf("live Coding Evidence[%d] run_command failed outside structured Tool output", index)
				}
				if err := validateAllowedLiveCodingPolicy(invocation); err != nil {
					return fmt.Errorf("live Coding Evidence[%d] run_command Policy: %w", index, err)
				}
			default:
				return fmt.Errorf("live Coding Evidence[%d] Tool %q failed", index, invocation.Tool)
			}
			continue
		}
		if err := validateAllowedLiveCodingPolicy(invocation); err != nil {
			return fmt.Errorf("live Coding Evidence[%d] Policy: %w", index, err)
		}
		if _, ok := requiredSuccessful[invocation.Tool]; ok {
			requiredSuccessful[invocation.Tool] = true
		}
	}
	for _, tool := range []string{
		filesystemextension.ReadFileToolName,
		editextension.ApplyPatchToolName,
		processextension.RunCommandToolName,
		gitextension.StatusToolName,
		gitextension.DiffToolName,
	} {
		if !requiredSuccessful[tool] {
			return fmt.Errorf("live Coding Evidence did not record successful %s", tool)
		}
	}
	return nil
}

func validateAllowedLiveCodingPolicy(invocation evidence.ToolInvocation) error {
	if invocation.PolicyOutcome != string(capability.OutcomeAllow) || invocation.PolicyReason == "" {
		return fmt.Errorf(
			"outcome=%q reason_present=%t",
			invocation.PolicyOutcome,
			invocation.PolicyReason != "",
		)
	}
	return nil
}

func validateRecoverableLiveCodingPatchFailure(invocation evidence.ToolInvocation) error {
	if !invocation.IsError || invocation.Error == "" {
		return errors.New("missing execution error")
	}
	class := safeLiveCodingToolErrorClass(&agent.ToolResult{IsError: true, Output: invocation.Error})
	switch class {
	case "invalid_json", "invalid_arguments", "invalid_patch":
		if invocation.PolicyOutcome != "" || invocation.PolicyReason != "" {
			return fmt.Errorf("%s reached an unexpected Policy decision", class)
		}
		return nil
	case protectedLiveCodingPolicyInvalidArguments,
		protectedLiveCodingPolicyPatchFormat,
		protectedLiveCodingPolicyPreconditionCount,
		protectedLiveCodingPolicyPreconditionTarget,
		protectedLiveCodingPolicyPreconditionDigest:
		if invocation.PolicyOutcome != string(capability.OutcomeDeny) ||
			!strings.HasPrefix(invocation.PolicyReason, protectedLiveCodingReasonPrefix+class+":") {
			return fmt.Errorf("%s has an inconsistent Policy decision", class)
		}
		return nil
	case "precondition_mismatch", "invalid_precondition", "patch_conflict":
		if err := validateAllowedLiveCodingPolicy(invocation); err != nil {
			return fmt.Errorf("%s Policy: %w", class, err)
		}
		return nil
	default:
		return fmt.Errorf("unsafe or unclassified error_class=%s", class)
	}
}

func countLiveCodingToolInvocations(invocations []evidence.ToolInvocation, tool string) int {
	count := 0
	for _, invocation := range invocations {
		if invocation.Tool == tool {
			count++
		}
	}
	return count
}
