package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
)

// CommandDefinition describes one command exposed to a host user or adapter
type CommandDefinition struct {
	// Name uniquely identifies the command within one Extension
	Name string
	// Description explains the command's user-facing behavior
	Description string
	// InputSchema describes Arguments
	InputSchema json.RawMessage
	// Capabilities identifies host permissions required by the command
	Capabilities []string
	// ExtensionID identifies the Extension that registered the command
	ExtensionID string
	// ExtensionGeneration identifies the pinned Extension generation
	ExtensionGeneration uint64
}

// CommandCall requests one host-initiated Extension command invocation
type CommandCall struct {
	Name      string
	Arguments json.RawMessage
}

// CommandResult contains structured JSON produced by an Extension command
type CommandResult struct {
	Output json.RawMessage
}

// Command is a host-invoked Extension operation that is not exposed to models
//
// Implementations must honor context cancellation and be safe for concurrent use
type Command interface {
	Definition() CommandDefinition
	Execute(ctx context.Context, call CommandCall) (CommandResult, error)
}

// CommandOptions configures one host-side Command capability proxy
type CommandOptions struct {
	Command  Command
	Policy   capability.Policy
	Approver capability.Approver
}

// CommandProxy enforces host Policy before invoking one Extension Command
type CommandProxy struct {
	command  Command
	policy   capability.Policy
	approver capability.Approver
}

// NewCommand validates options and constructs a capability-aware Command
func NewCommand(options CommandOptions) (*CommandProxy, error) {
	if options.Command == nil {
		return nil, errors.New("extension Command is required")
	}
	if options.Policy == nil {
		return nil, errors.New("extension Command Policy is required")
	}
	return &CommandProxy{command: options.Command, policy: options.Policy, approver: options.Approver}, nil
}

// Definition returns an isolated command definition
func (proxy *CommandProxy) Definition() CommandDefinition {
	definition := proxy.command.Definition()
	definition.InputSchema = append(json.RawMessage(nil), definition.InputSchema...)
	definition.Capabilities = append([]string(nil), definition.Capabilities...)
	return definition
}

// Execute authorizes and invokes the wrapped Extension Command
func (proxy *CommandProxy) Execute(ctx context.Context, call CommandCall) (CommandResult, error) {
	if ctx == nil {
		return CommandResult{}, errors.New("extension Command context must not be nil")
	}
	definition := proxy.command.Definition()
	names := make([]capability.Name, 0, len(definition.Capabilities))
	seen := make(map[capability.Name]struct{}, len(definition.Capabilities))
	for _, value := range definition.Capabilities {
		name := capability.Name(value)
		if err := capability.ValidateName(name); err != nil {
			return CommandResult{}, fmt.Errorf("Command %q: %w", definition.Name, err)
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Slice(names, func(first, second int) bool { return names[first] < names[second] })
	if info, ok := agent.RunInfoFromContext(ctx); ok && len(info.Capabilities) > 0 {
		allowed := make(map[string]struct{}, len(info.Capabilities))
		for _, name := range info.Capabilities {
			allowed[name] = struct{}{}
		}
		for _, name := range names {
			if _, ok := allowed[string(name)]; !ok {
				return CommandResult{}, fmt.Errorf("%w for Command %q: capability %q is outside the Run capability set", capability.ErrDenied, definition.Name, name)
			}
		}
	}
	request := capability.Request{
		Tool:         "command:" + definition.Name,
		Capabilities: names,
		Arguments:    append(json.RawMessage(nil), call.Arguments...),
	}
	decision, err := proxy.policy.Evaluate(ctx, request)
	if err != nil {
		return CommandResult{}, fmt.Errorf("evaluate Command Policy: %w", err)
	}
	switch decision.Outcome {
	case capability.OutcomeAllow:
	case capability.OutcomeDeny:
		return CommandResult{}, fmt.Errorf("%w for Command %q: %s", capability.ErrDenied, definition.Name, decision.Reason)
	case capability.OutcomeAsk:
		if proxy.approver == nil {
			return CommandResult{}, fmt.Errorf("%w for Command %q: %s", capability.ErrApprovalRequired, definition.Name, decision.Reason)
		}
		approved, approveErr := proxy.approver.Approve(ctx, request)
		if approveErr != nil {
			return CommandResult{}, fmt.Errorf("approve Command %q: %w", definition.Name, approveErr)
		}
		if !approved {
			return CommandResult{}, fmt.Errorf("%w for Command %q: approval was rejected", capability.ErrDenied, definition.Name)
		}
	default:
		return CommandResult{}, fmt.Errorf("Policy returned unsupported outcome %q", decision.Outcome)
	}
	return proxy.command.Execute(ctx, call)
}
