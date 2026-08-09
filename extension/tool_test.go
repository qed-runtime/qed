package extension_test

import (
	"context"
	"encoding/json"
	"errors"
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
	_, err = proxy.Execute(context.Background(), agent.ToolCall{ID: "call-1", Name: "record", Arguments: json.RawMessage(`{}`)})
	if !errors.Is(err, capability.ErrDenied) {
		t.Fatalf("Execute() error = %v", err)
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
