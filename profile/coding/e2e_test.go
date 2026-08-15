package coding_test

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
	"sync"
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/evidence"
	"github.com/qed-runtime/qed/extension/host"
	"github.com/qed-runtime/qed/extension/selfexec"
	gitextension "github.com/qed-runtime/qed/extensions/git"
	processextension "github.com/qed-runtime/qed/extensions/process"
	workspaceextension "github.com/qed-runtime/qed/extensions/workspace"
	"github.com/qed-runtime/qed/profile/coding"
	"github.com/qed-runtime/qed/session"
)

const (
	codingE2EBlockArgument     = "__qed_coding_e2e_block"
	codingE2EMarkerEnvironment = "QED_CODING_E2E_MARKER"
	codingE2EModel             = "fixed-coding-e2e-v1"
)

func TestCodingProfileRecordsDeterministicToolFailure(t *testing.T) {
	root, goExecutable := newCodingE2EWorkspace(t)
	profile := newCodingE2EProfile(t, root, codingE2EProfileOptions{
		policy: newCodingE2EPolicy(t, capability.StaticPolicyOptions{Allow: []capability.Name{
			capability.FilesystemRead,
			capability.FilesystemWrite,
			capability.ProcessExecute,
			capability.GitRead,
		}}),
	})
	store, err := session.NewJSONLStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	arguments := mustJSON(t, map[string]any{"argv": []string{goExecutable, "test", "./..."}})
	provider := newFixedCodingProvider(
		func(agent.ModelRequest) (agent.Message, error) {
			return codingE2EToolCall("failed-check", processextension.RunCommandToolName, arguments), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			output, err := codingE2EToolOutput(request, processextension.RunCommandToolName, true)
			if err != nil {
				return agent.Message{}, err
			}
			var response struct {
				ExitCode int  `json:"exit_code"`
				Success  bool `json:"success"`
			}
			if err := json.Unmarshal([]byte(output), &response); err != nil {
				return agent.Message{}, fmt.Errorf("decode failed check output: %w", err)
			}
			if response.Success || response.ExitCode == 0 {
				return agent.Message{}, fmt.Errorf("check unexpectedly succeeded: %s", output)
			}
			return codingE2EFinalMessage("Observed the expected failing check"), nil
		},
	)
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:     provider,
		ToolSource:   profile.ToolSource(),
		SessionStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID:      "coding",
		SessionID:    "tool-failure",
		Instructions: profile.Instructions(),
		Input:        []agent.Message{{Role: agent.RoleUser, Text: "Run the failing check"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, result, runErr := collectCodingE2ERun(handle)
	if runErr != nil {
		t.Fatalf("Run error = %v", runErr)
	}
	if result.Status != agent.RunStatusCompleted || result.ToolCalls != 1 || result.ProviderCalls != 2 ||
		len(result.ToolResults) != 1 || !result.ToolResults[0].IsError {
		t.Fatalf("Run result = %#v", result)
	}
	invocations := profile.ToolInvocations()
	bundle := roundTripCodingEvidence(t, result, events, invocations)
	if len(invocations) != 1 || !invocations[0].IsError || len(bundle.Commands) != 1 ||
		len(bundle.Checks) != 1 || bundle.Commands[0].Succeeded || bundle.Checks[0].Succeeded {
		t.Fatalf("failure Evidence = %#v / %#v", invocations, bundle)
	}
	assertSessionFinished(t, store, "tool-failure")
}

func TestCodingProfileRecordsDeterministicApprovalDenial(t *testing.T) {
	root, _ := newCodingE2EWorkspace(t)
	before := readCodingE2EFile(t, root, "calc.go")
	arguments := codingE2EPatchArguments(t, before)
	profile := newCodingE2EProfile(t, root, codingE2EProfileOptions{
		policy: newCodingE2EPolicy(t, capability.StaticPolicyOptions{
			Allow: []capability.Name{capability.FilesystemRead, capability.ProcessExecute, capability.GitRead},
			Ask:   []capability.Name{capability.FilesystemWrite},
		}),
		approver: capability.WaitApprover{},
	})
	store, err := session.NewJSONLStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	provider := newFixedCodingProvider(
		func(agent.ModelRequest) (agent.Message, error) {
			return codingE2EToolCall("denied-edit", "apply_patch", arguments), nil
		},
		func(request agent.ModelRequest) (agent.Message, error) {
			output, err := codingE2EToolOutput(request, "apply_patch", true)
			if err != nil {
				return agent.Message{}, err
			}
			if !strings.Contains(output, capability.ErrDenied.Error()) {
				return agent.Message{}, fmt.Errorf("denied Tool output = %q", output)
			}
			return codingE2EFinalMessage("The edit was denied"), nil
		},
	)
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:     provider,
		ToolSource:   profile.ToolSource(),
		SessionStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID:      "coding",
		SessionID:    "approval-denial",
		Instructions: profile.Instructions(),
		Input:        []agent.Message{{Role: agent.RoleUser, Text: "Apply the proposed edit"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var events []agent.Event
	var resumeErr error
	waiting := 0
	resumed := 0
	for event := range handle.Events() {
		events = append(events, event)
		switch event.Type {
		case agent.EventRunWaiting:
			waiting++
			if event.WaitRequest == nil || event.WaitRequest.Kind != agent.WaitKindApproval {
				t.Errorf("approval wait Event = %#v", event)
			}
			if event.WaitRequest == nil {
				handle.Cancel()
				continue
			}
			var approval struct {
				Tool                string                      `json:"tool"`
				ArgumentsDigest     string                      `json:"arguments_digest"`
				ExtensionID         string                      `json:"extension_id"`
				ExtensionGeneration uint64                      `json:"extension_generation"`
				Preview             *capability.ApprovalPreview `json:"preview"`
			}
			if err := json.Unmarshal(event.WaitRequest.Payload, &approval); err != nil ||
				approval.Tool != "apply_patch" || !strings.HasPrefix(approval.ArgumentsDigest, "sha256:") ||
				approval.ExtensionID != workspaceextension.ID || approval.ExtensionGeneration == 0 ||
				approval.Preview == nil || len(approval.Preview.Details) != 1 ||
				approval.Preview.Details[0].Label != "update" || approval.Preview.Details[0].Value != "calc.go" {
				t.Errorf("approval preview = %#v, %v", approval, err)
			}
			resumeErr = handle.Resume(agent.WaitResponse{
				RequestID: event.WaitRequest.ID,
				Payload:   json.RawMessage(`{"approved":false}`),
			})
			if resumeErr != nil {
				handle.Cancel()
			}
		case agent.EventRunResumed:
			resumed++
		}
	}
	result, runErr := handle.Wait()
	if resumeErr != nil {
		t.Fatalf("Resume error = %v", resumeErr)
	}
	if runErr != nil {
		t.Fatalf("Run error = %v", runErr)
	}
	if result.Status != agent.RunStatusCompleted || waiting != 1 || resumed != 1 ||
		len(result.ToolResults) != 1 || !result.ToolResults[0].IsError {
		t.Fatalf("approval result/events = %#v / %#v", result, events)
	}
	after := readCodingE2EFile(t, root, "calc.go")
	if after != before {
		t.Fatalf("denied edit changed calc.go = %q", after)
	}
	invocations := profile.ToolInvocations()
	bundle := roundTripCodingEvidence(t, result, events, invocations)
	if len(invocations) != 1 || invocations[0].PolicyOutcome != string(capability.OutcomeDeny) ||
		invocations[0].PolicyReason != "approval was rejected" || !invocations[0].IsError ||
		len(bundle.Changes) != 1 || bundle.Changes[0].Succeeded {
		t.Fatalf("denial Evidence = %#v / %#v", invocations, bundle)
	}
	assertSessionFinished(t, store, "approval-denial")
}

func TestCodingProfileCancelsDeterministicCommand(t *testing.T) {
	root, _ := newCodingE2EWorkspace(t)
	marker := filepath.Join(t.TempDir(), "command-started")
	profile := newCodingE2EProfile(t, root, codingE2EProfileOptions{
		policy: newCodingE2EPolicy(t, capability.StaticPolicyOptions{Allow: []capability.Name{
			capability.FilesystemRead,
			capability.FilesystemWrite,
			capability.ProcessExecute,
			capability.GitRead,
		}}),
		environment: map[string]string{codingE2EMarkerEnvironment: marker},
	})
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	testExecutable, err = filepath.Abs(testExecutable)
	if err != nil {
		t.Fatal(err)
	}
	arguments := mustJSON(t, map[string]any{"argv": []string{testExecutable, codingE2EBlockArgument}})
	provider := newFixedCodingProvider(func(agent.ModelRequest) (agent.Message, error) {
		return codingE2EToolCall("blocking-command", processextension.RunCommandToolName, arguments), nil
	})
	store, err := session.NewJSONLStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:     provider,
		ToolSource:   profile.ToolSource(),
		SessionStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID:      "coding",
		SessionID:    "cancel-command",
		Instructions: profile.Instructions(),
		Input:        []agent.Message{{Role: agent.RoleUser, Text: "Run the blocking command"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitForCodingE2EFile(marker, 5*time.Second); err != nil {
		handle.Cancel()
		_, _, _ = collectCodingE2ERun(handle)
		t.Fatal(err)
	}
	handle.Cancel()
	events, result, runErr := collectCodingE2ERun(handle)
	if !errors.Is(runErr, context.Canceled) || result.Status != agent.RunStatusCanceled ||
		len(events) == 0 || events[len(events)-1].Type != agent.EventRunCanceled || provider.Calls() != 1 {
		t.Fatalf("canceled Run = %#v / %v / %#v", result, runErr, events)
	}
	invocations := profile.ToolInvocations()
	bundle := roundTripCodingEvidence(t, result, events, invocations)
	if len(invocations) != 1 || !invocations[0].IsError || len(bundle.Commands) != 1 ||
		len(bundle.Checks) != 1 || bundle.Commands[0].Succeeded || bundle.Checks[0].Succeeded {
		t.Fatalf("cancel Evidence = %#v / %#v", invocations, bundle)
	}
	assertSessionFinished(t, store, "cancel-command")
}

func TestCodingProfileResumesPersistedApprovalAfterRestart(t *testing.T) {
	root, _ := newCodingE2EWorkspace(t)
	before := readCodingE2EFile(t, root, "calc.go")
	arguments := codingE2EPatchArguments(t, before)
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
		return codingE2EToolCall("resumed-edit", "apply_patch", arguments), nil
	})
	firstRuntime, err := agent.NewRuntime(agent.Options{
		Provider:     firstProvider,
		ToolSource:   firstProfile.ToolSource(),
		SessionStore: firstStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstHandle, err := firstRuntime.Run(context.Background(), agent.RunRequest{
		AgentID:      "coding",
		SessionID:    "persisted-resume",
		Instructions: firstProfile.Instructions(),
		Input:        []agent.Message{{Role: agent.RoleUser, Text: "Apply the edit after approval"}},
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
		t.Fatalf("first Run did not wait: %#v / %v / %#v", result, runErr, firstEvents)
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
	if !errors.Is(firstRunErr, context.Canceled) || firstResult.Status != agent.RunStatusCanceled || firstProvider.Calls() != 1 {
		t.Fatalf("interrupted Run = %#v / %v / %#v", firstResult, firstRunErr, firstEvents)
	}
	if err := firstProfile.Close(); err != nil {
		t.Fatalf("close first Profile: %v", err)
	}

	restartedStore, err := session.NewJSONLStore(restartedSessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := restartedStore.Load(context.Background(), "persisted-resume")
	if err != nil {
		t.Fatal(err)
	}
	if pending.PendingWait == nil || pending.PendingWait.ID != waitRequest.ID || pending.PendingTool == nil ||
		pending.PendingTool.ID != "resumed-edit" {
		t.Fatalf("persisted pending Session = %#v", pending)
	}

	restartedProfile := newCodingE2EProfile(t, root, codingE2EProfileOptions{
		policy:   newCodingE2EPolicy(t, policyOptions),
		approver: capability.WaitApprover{},
	})
	restartedProvider := newFixedCodingProvider(func(request agent.ModelRequest) (agent.Message, error) {
		if _, err := codingE2EToolOutput(request, "apply_patch", false); err != nil {
			return agent.Message{}, err
		}
		return codingE2EFinalMessage("Resumed and applied the approved edit"), nil
	})
	restartedRuntime, err := agent.NewRuntime(agent.Options{
		Provider:     restartedProvider,
		ToolSource:   restartedProfile.ToolSource(),
		SessionStore: restartedStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	restartedHandle, err := restartedRuntime.Run(context.Background(), agent.RunRequest{
		AgentID:      "coding",
		SessionID:    "persisted-resume",
		Instructions: restartedProfile.Instructions(),
		Resume: &agent.WaitResponse{
			RequestID: waitRequest.ID,
			Payload:   json.RawMessage(`{"approved":true}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, result, runErr := collectCodingE2ERun(restartedHandle)
	if runErr != nil || result.Status != agent.RunStatusCompleted || result.ProviderCalls != 1 ||
		result.ToolCalls != 1 || restartedProvider.Calls() != 1 {
		t.Fatalf("resumed Run = %#v / %v / %#v", result, runErr, events)
	}
	if !hasCodingE2EEvent(events, agent.EventRunResumed) || hasCodingE2EEvent(events, agent.EventRunWaiting) {
		t.Fatalf("resumed Events = %#v", events)
	}
	after := readCodingE2EFile(t, root, "calc.go")
	if !strings.Contains(after, "return first + second") {
		t.Fatalf("resumed calc.go = %q", after)
	}
	invocations := restartedProfile.ToolInvocations()
	bundle := roundTripCodingEvidence(t, result, events, invocations)
	if len(invocations) != 1 || invocations[0].PolicyOutcome != string(capability.OutcomeAllow) ||
		invocations[0].PolicyReason != "approval was granted" || invocations[0].IsError ||
		len(bundle.Changes) != 1 || !bundle.Changes[0].Succeeded {
		t.Fatalf("resume Evidence = %#v / %#v", invocations, bundle)
	}
	assertSessionFinished(t, restartedStore, "persisted-resume")
}

type codingE2EProfileOptions struct {
	policy      capability.Policy
	approver    capability.Approver
	environment map[string]string
}

func newCodingE2EWorkspace(t *testing.T) (string, string) {
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
	writeFile(t, root, "AGENTS.md", codingE2EInstructions)
	initializeRepository(t, root, []string{"AGENTS.md", "calc.go", "calc_test.go", "go.mod"})
	return root, goExecutable
}

func newCodingE2EPolicy(t *testing.T, options capability.StaticPolicyOptions) capability.Policy {
	t.Helper()
	policy, err := capability.NewStaticPolicy(options)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func newCodingE2EProfile(t *testing.T, root string, options codingE2EProfileOptions) *coding.Profile {
	t.Helper()
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	testExecutable, err = filepath.Abs(testExecutable)
	if err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{
		"GOCACHE":     t.TempDir(),
		"GOENV":       "off",
		"GOTOOLCHAIN": "local",
		"HOME":        t.TempDir(),
		"PATH":        os.Getenv("PATH"),
	}
	for name, value := range options.environment {
		environment[name] = value
	}
	profile, err := coding.New(context.Background(), coding.Options{
		Root:     root,
		Policy:   options.policy,
		Approver: options.approver,
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
		CommandEnvironment: environment,
		Process: processextension.Options{
			DefaultTimeout: 15 * time.Second,
			MaximumTimeout: 30 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("coding.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := profile.Close(); err != nil {
			t.Errorf("Profile.Close() error = %v", err)
		}
	})
	return profile
}

type fixedCodingStep func(agent.ModelRequest) (agent.Message, error)

type fixedCodingProvider struct {
	mu    sync.Mutex
	steps []fixedCodingStep
	calls int
}

func newFixedCodingProvider(steps ...fixedCodingStep) *fixedCodingProvider {
	return &fixedCodingProvider{steps: append([]fixedCodingStep(nil), steps...)}
}

func (*fixedCodingProvider) Name() string {
	return "fixed-coding-e2e"
}

func (*fixedCodingProvider) ModelID() string {
	return codingE2EModel
}

func (provider *fixedCodingProvider) Stream(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	provider.mu.Lock()
	index := provider.calls
	provider.calls++
	var step fixedCodingStep
	if index < len(provider.steps) {
		step = provider.steps[index]
	}
	provider.mu.Unlock()
	if step == nil {
		return nil, errors.New("fixed Coding Provider exhausted")
	}
	message, err := step(request)
	if err != nil {
		return nil, err
	}
	return agent.MessageStream(message), nil
}

func (provider *fixedCodingProvider) Calls() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.calls
}

func codingE2EToolCall(id, name string, arguments json.RawMessage) agent.Message {
	return agent.Message{
		Role:       agent.RoleAssistant,
		StopReason: agent.StopReasonToolUse,
		Model:      codingE2EModel,
		ToolCalls: []agent.ToolCall{{
			ID:        id,
			Name:      name,
			Arguments: append(json.RawMessage(nil), arguments...),
		}},
	}
}

func codingE2EFinalMessage(text string) agent.Message {
	return agent.Message{
		Role:       agent.RoleAssistant,
		Text:       text,
		StopReason: agent.StopReasonEndTurn,
		Model:      codingE2EModel,
	}
}

func codingE2EToolOutput(request agent.ModelRequest, name string, wantError bool) (string, error) {
	if len(request.Messages) == 0 {
		return "", errors.New("Tool result message is missing")
	}
	message := request.Messages[len(request.Messages)-1]
	if message.Role != agent.RoleTool || message.ToolName != name || message.ToolIsError != wantError {
		return "", fmt.Errorf("last message = %#v, want %s error=%t result", message, name, wantError)
	}
	return message.Text, nil
}

func codingE2EPatchArguments(t *testing.T, before string) json.RawMessage {
	t.Helper()
	return mustJSON(t, map[string]any{
		"patch": "--- a/calc.go\n+++ b/calc.go\n@@ -2,4 +2,4 @@\n \n func Add(first, second int) int {\n-\treturn first - second\n+\treturn first + second\n }\n",
		"preconditions": []map[string]string{{
			"path":   "calc.go",
			"sha256": codingE2EDigest(before),
		}},
	})
}

func codingE2EDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func collectCodingE2ERun(handle *agent.RunHandle) ([]agent.Event, agent.RunResult, error) {
	var events []agent.Event
	for event := range handle.Events() {
		events = append(events, event)
	}
	result, err := handle.Wait()
	return events, result, err
}

func roundTripCodingEvidence(
	t *testing.T,
	result agent.RunResult,
	events []agent.Event,
	invocations []evidence.ToolInvocation,
) evidence.Bundle {
	t.Helper()
	for index, event := range events {
		if event.Sequence != uint64(index+1) || event.RunID != result.RunID {
			t.Fatalf("Event[%d] identity = %#v", index, event)
		}
	}
	bundle, err := evidence.NewBundle(result, evidence.BundleOptions{
		Events:          events,
		ToolInvocations: invocations,
		ConfigDigest:    "sha256:" + strings.Repeat("0", 64),
		WorkspaceID:     "coding-e2e-workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := evidence.NewJSONStore(filepath.Join(t.TempDir(), "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	descriptors, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Run.ID != result.RunID || loaded.WorkspaceState.ID != "coding-e2e-workspace" ||
		len(loaded.Events) != len(events) || len(loaded.ToolTrace) != len(invocations) ||
		len(descriptors) != 1 || descriptors[0].ID != result.RunID {
		t.Fatalf("round-tripped Evidence = %#v / %#v", loaded, descriptors)
	}
	return loaded
}

func assertSessionFinished(t *testing.T, store agent.SessionStore, sessionID string) {
	t.Helper()
	snapshot, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PendingWait != nil || snapshot.PendingTool != nil || snapshot.Revision == 0 {
		t.Fatalf("finished Session = %#v", snapshot)
	}
}

func readCodingE2EFile(t *testing.T, root, path string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func waitForCodingE2EFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for command marker %q", filepath.Base(path))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func copyCodingE2ESessionLog(source, destination string) error {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	copied := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(destination, entry.Name()), content, 0o600); err != nil {
			return err
		}
		copied++
	}
	if copied != 1 {
		return fmt.Errorf("copied %d Session logs, want 1", copied)
	}
	return nil
}

func hasCodingE2EEvent(events []agent.Event, eventType agent.EventType) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}
