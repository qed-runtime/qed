package extension_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/evidence"
	"github.com/qed-runtime/qed/extension"
)

func TestToolProxyEnforcesPolicyAndRecordsEvidence(t *testing.T) {
	t.Parallel()

	policy, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{Allow: []capability.Name{capability.FilesystemRead}})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &evidence.MemoryRecorder{}
	proxy, err := extension.NewTool(extension.ToolOptions{Tool: recordingTool{}, Policy: policy, Recorder: recorder})
	if err != nil {
		t.Fatal(err)
	}
	result, err := proxy.Execute(context.Background(), agent.ToolCall{ID: "call-1", Name: "record", Arguments: json.RawMessage(`{"value":"ok"}`)})
	if err != nil || result.Output != "ok" || result.CallID != "call-1" {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	if result.Policy == nil || result.Policy.Outcome != "allow" ||
		len(result.Policy.Capabilities) != 1 || result.Policy.Capabilities[0] != string(capability.FilesystemRead) ||
		!strings.HasPrefix(result.Policy.ReasonDigest, "sha256:") {
		t.Fatalf("Tool Policy metadata = %#v", result.Policy)
	}
	records := recorder.ToolInvocations()
	if len(records) != 1 || records[0].PolicyOutcome != "allow" || records[0].ArgumentsDigest == "" || records[0].OutputDigest == "" {
		t.Fatalf("evidence = %#v", records)
	}
}

func TestToolProxyReturnsDeniedAsToolError(t *testing.T) {
	t.Parallel()

	policy, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{Deny: []capability.Name{capability.FilesystemRead}})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := extension.NewTool(extension.ToolOptions{Tool: recordingTool{}, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	result, err := proxy.Execute(context.Background(), agent.ToolCall{ID: "call-1", Name: "record", Arguments: json.RawMessage(`{}`)})
	if !errors.Is(err, capability.ErrDenied) {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Policy == nil || result.Policy.Outcome != "deny" || result.Policy.ReasonDigest == "" {
		t.Fatalf("denied Tool Policy metadata = %#v", result.Policy)
	}
}

func TestToolProxyRequiresAndRecordsApproval(t *testing.T) {
	t.Parallel()

	policy, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{Ask: []capability.Name{capability.FilesystemRead}})
	if err != nil {
		t.Fatal(err)
	}
	withoutApprover, err := extension.NewTool(extension.ToolOptions{Tool: recordingTool{}, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	_, err = withoutApprover.Execute(context.Background(), agent.ToolCall{ID: "call-1", Name: "record", Arguments: json.RawMessage(`{}`)})
	if !errors.Is(err, capability.ErrApprovalRequired) {
		t.Fatalf("Execute(without Approver) error = %v", err)
	}

	recorder := &evidence.MemoryRecorder{}
	withApprover, err := extension.NewTool(extension.ToolOptions{
		Tool:   recordingTool{},
		Policy: policy,
		Approver: capability.ApproverFunc(func(_ context.Context, request capability.Request) (bool, error) {
			return request.Tool == "record", nil
		}),
		Recorder: recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := withApprover.Execute(context.Background(), agent.ToolCall{ID: "call-2", Name: "record", Arguments: json.RawMessage(`{"value":"approved"}`)})
	if err != nil || result.Output != "approved" {
		t.Fatalf("Execute(with Approver) = %#v, %v", result, err)
	}
	records := recorder.ToolInvocations()
	if len(records) != 1 || records[0].PolicyOutcome != "allow" || records[0].PolicyReason != "approval was granted" {
		t.Fatalf("Evidence = %#v", records)
	}
}

func TestToolProxyRejectsInvalidInputBeforeCapabilitiesAndPolicy(t *testing.T) {
	t.Parallel()

	tool := &validationBoundaryTool{}
	policy := &countingPolicy{}
	recorder := &evidence.MemoryRecorder{}
	proxy, err := extension.NewTool(extension.ToolOptions{
		Tool:     tool,
		Policy:   policy,
		Recorder: recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := proxy.Execute(context.Background(), agent.ToolCall{
		ID:        "invalid-call",
		Name:      "validated",
		Arguments: json.RawMessage(`{"value":1}`),
	})
	if !errors.Is(err, agent.ErrToolInputValidation) || !strings.Contains(err.Error(), "$/value") {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Policy != nil {
		t.Fatalf("validation failure recorded a Policy decision = %#v", result.Policy)
	}
	if tool.capabilityCalls != 0 || tool.executeCalls != 0 || policy.calls != 0 {
		t.Fatalf(
			"side effects = capabilities:%d policy:%d execute:%d",
			tool.capabilityCalls,
			policy.calls,
			tool.executeCalls,
		)
	}
	records := recorder.ToolInvocations()
	if len(records) != 1 || !records[0].IsError || records[0].PolicyOutcome != "" ||
		!strings.Contains(records[0].Error, "input validation") {
		t.Fatalf("Evidence = %#v", records)
	}
}

type recordingTool struct{}

func (recordingTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "record", Capabilities: []string{string(capability.FilesystemRead)}}
}

func (recordingTool) Execute(_ context.Context, call agent.ToolCall) (agent.ToolResult, error) {
	var input struct {
		Value string `json:"value"`
	}
	if len(call.Arguments) > 2 {
		if err := json.Unmarshal(call.Arguments, &input); err != nil {
			return agent.ToolResult{}, err
		}
	}
	return agent.ToolResult{Output: input.Value}, nil
}

type validationBoundaryTool struct {
	capabilityCalls int
	executeCalls    int
}

func (tool *validationBoundaryTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        "validated",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`),
	}
}

func (tool *validationBoundaryTool) RequiredCapabilities(context.Context, agent.ToolCall) ([]capability.Name, error) {
	tool.capabilityCalls++
	return nil, nil
}

func (tool *validationBoundaryTool) Execute(context.Context, agent.ToolCall) (agent.ToolResult, error) {
	tool.executeCalls++
	return agent.ToolResult{Output: "unexpected"}, nil
}

type countingPolicy struct {
	calls int
}

func (policy *countingPolicy) Evaluate(context.Context, capability.Request) (capability.Decision, error) {
	policy.calls++
	return capability.Decision{Outcome: capability.OutcomeAllow}, nil
}
