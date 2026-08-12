// Package extension adapts capability-aware Extensions to the Agent Tool API
package extension

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/evidence"
)

// DynamicCapabilities adds invocation-specific capabilities after decoding a Tool Call
type DynamicCapabilities interface {
	RequiredCapabilities(ctx context.Context, call agent.ToolCall) ([]capability.Name, error)
}

// ToolOptions configures one host-side Extension Tool Proxy
type ToolOptions struct {
	Tool agent.Tool
	// ToolInputValidator compiles the Tool schema at the host enforcement boundary
	ToolInputValidator agent.ToolInputValidator
	Policy             capability.Policy
	Approver           capability.Approver
	Recorder           evidence.Recorder
}

// ToolProxy enforces Policy and records Evidence around one Extension Tool
type ToolProxy struct {
	tool      agent.Tool
	validator agent.CompiledToolInputValidator
	policy    capability.Policy
	approver  capability.Approver
	recorder  evidence.Recorder
}

// NewTool validates options and constructs a ToolProxy
func NewTool(options ToolOptions) (*ToolProxy, error) {
	if options.Tool == nil {
		return nil, errors.New("extension Tool is required")
	}
	if options.Policy == nil {
		return nil, errors.New("extension Tool Policy is required")
	}
	definition := options.Tool.Definition()
	validator, err := agent.CompileToolInputSchema(options.ToolInputValidator, definition.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("extension Tool %q input schema: %w", definition.Name, err)
	}
	recorder := options.Recorder
	if recorder == nil {
		recorder = evidence.NopRecorder{}
	}
	return &ToolProxy{
		tool:      options.Tool,
		validator: validator,
		policy:    options.Policy,
		approver:  options.Approver,
		recorder:  recorder,
	}, nil
}

// Definition returns an isolated Provider-facing Tool definition
func (proxy *ToolProxy) Definition() agent.ToolDefinition {
	definition := proxy.tool.Definition()
	definition.InputSchema = append([]byte(nil), definition.InputSchema...)
	definition.Capabilities = append([]string(nil), definition.Capabilities...)
	return definition
}

// Execute authorizes and invokes the wrapped Extension Tool
func (proxy *ToolProxy) Execute(ctx context.Context, call agent.ToolCall) (result agent.ToolResult, resultErr error) {
	if ctx == nil {
		return agent.ToolResult{}, errors.New("extension Tool context must not be nil")
	}
	if len(call.Arguments) == 0 {
		call.Arguments = json.RawMessage(`{}`)
	} else {
		call.Arguments = append(json.RawMessage(nil), call.Arguments...)
	}
	startedAt := time.Now().UTC()
	definition := proxy.tool.Definition()
	var capabilities []capability.Name
	var err error
	decision := capability.Decision{}
	defer func() {
		invocation := evidence.ToolInvocation{
			CallID:              call.ID,
			Tool:                call.Name,
			ExtensionID:         definition.ExtensionID,
			ExtensionGeneration: definition.ExtensionGeneration,
			Capabilities:        namesAsStrings(capabilities),
			ArgumentsDigest:     digest(call.Arguments),
			PolicyOutcome:       string(decision.Outcome),
			PolicyReason:        decision.Reason,
			StartedAt:           startedAt,
			CompletedAt:         time.Now().UTC(),
			OutputDigest:        digest([]byte(result.Output)),
			IsError:             result.IsError || resultErr != nil,
		}
		if resultErr != nil {
			invocation.Error = resultErr.Error()
		}
		if info, ok := agent.RunInfoFromContext(ctx); ok {
			invocation.RunID = info.RunID
			invocation.ParentRunID = info.ParentRunID
			invocation.AgentID = info.AgentID
			invocation.SessionID = info.SessionID
		}
		proxy.recorder.RecordToolInvocation(context.WithoutCancel(ctx), invocation)
	}()
	if err := agent.ValidateToolInput(proxy.validator, call.Arguments); err != nil {
		return agent.ToolResult{}, fmt.Errorf("Tool %q input validation: %w", definition.Name, err)
	}
	capabilities, err = proxy.requiredCapabilities(ctx, call)
	if err != nil {
		return agent.ToolResult{}, err
	}

	request := capability.Request{
		CallID:       call.ID,
		Tool:         call.Name,
		Capabilities: capabilities,
		Arguments:    append([]byte(nil), call.Arguments...),
	}
	decision, err = proxy.policy.Evaluate(ctx, request)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("evaluate Tool Policy: %w", err)
	}
	switch decision.Outcome {
	case capability.OutcomeAllow:
	case capability.OutcomeDeny:
		return agent.ToolResult{}, fmt.Errorf("%w for Tool %q: %s", capability.ErrDenied, call.Name, decision.Reason)
	case capability.OutcomeAsk:
		if proxy.approver == nil {
			return agent.ToolResult{}, fmt.Errorf("%w for Tool %q: %s", capability.ErrApprovalRequired, call.Name, decision.Reason)
		}
		approved, approveErr := proxy.approver.Approve(ctx, request)
		if approveErr != nil {
			return agent.ToolResult{}, fmt.Errorf("approve Tool %q: %w", call.Name, approveErr)
		}
		if !approved {
			decision = capability.Decision{Outcome: capability.OutcomeDeny, Reason: "approval was rejected"}
			return agent.ToolResult{}, fmt.Errorf("%w for Tool %q: approval was rejected", capability.ErrDenied, call.Name)
		}
		decision = capability.Decision{Outcome: capability.OutcomeAllow, Reason: "approval was granted"}
	default:
		return agent.ToolResult{}, fmt.Errorf("Policy returned unsupported outcome %q", decision.Outcome)
	}

	result, resultErr = proxy.tool.Execute(ctx, call)
	result.CallID = call.ID
	result.Name = call.Name
	return result, resultErr
}

func (proxy *ToolProxy) requiredCapabilities(ctx context.Context, call agent.ToolCall) ([]capability.Name, error) {
	definition := proxy.tool.Definition()
	names := make([]capability.Name, 0, len(definition.Capabilities))
	for _, value := range definition.Capabilities {
		name := capability.Name(value)
		if err := capability.ValidateName(name); err != nil {
			return nil, fmt.Errorf("Tool %q: %w", definition.Name, err)
		}
		names = append(names, name)
	}
	if dynamic, ok := proxy.tool.(DynamicCapabilities); ok {
		additional, err := dynamic.RequiredCapabilities(ctx, call)
		if err != nil {
			return nil, fmt.Errorf("resolve Tool %q capabilities: %w", definition.Name, err)
		}
		names = append(names, additional...)
	}

	unique := make(map[capability.Name]struct{}, len(names))
	result := make([]capability.Name, 0, len(names))
	for _, name := range names {
		if err := capability.ValidateName(name); err != nil {
			return nil, fmt.Errorf("Tool %q: %w", definition.Name, err)
		}
		if _, exists := unique[name]; exists {
			continue
		}
		unique[name] = struct{}{}
		result = append(result, name)
	}
	sort.Slice(result, func(first, second int) bool { return result[first] < result[second] })
	if info, ok := agent.RunInfoFromContext(ctx); ok && len(info.Capabilities) > 0 {
		allowed := make(map[string]struct{}, len(info.Capabilities))
		for _, name := range info.Capabilities {
			allowed[name] = struct{}{}
		}
		for _, name := range result {
			if _, ok := allowed[string(name)]; !ok {
				return nil, fmt.Errorf("%w for Tool %q: capability %q is outside the Run capability set", capability.ErrDenied, definition.Name, name)
			}
		}
	}
	return result, nil
}

func namesAsStrings(names []capability.Name) []string {
	result := make([]string, len(names))
	for index, name := range names {
		result[index] = string(name)
	}
	return result
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}
