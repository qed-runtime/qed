// Package capability evaluates permissions requested by Tools
package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Name identifies one host permission
type Name string

// Standard host capabilities used by the Coding Profile
const (
	FilesystemRead   Name = "filesystem.read"
	FilesystemWrite  Name = "filesystem.write"
	FilesystemDelete Name = "filesystem.delete"
	ProcessExecute   Name = "process.execute"
	GitRead          Name = "git.read"
	SecretsRead      Name = "secrets.read"
)

// Outcome is a Policy decision before optional human approval
type Outcome string

// Policy outcomes
const (
	OutcomeAllow Outcome = "allow"
	OutcomeAsk   Outcome = "ask"
	OutcomeDeny  Outcome = "deny"
)

var (
	// ErrDenied indicates that Policy rejected a Tool invocation
	ErrDenied = errors.New("capability denied")
	// ErrApprovalRequired indicates that Policy requires an unavailable approver
	ErrApprovalRequired = errors.New("capability approval required")
)

// Request describes the capabilities required by one Tool invocation
type Request struct {
	// CallID identifies the Tool invocation within its Run
	CallID string
	// Tool identifies the requested Tool
	Tool string
	// Capabilities lists the host permissions required by the invocation
	Capabilities []Name
	// Arguments contains the exact Tool arguments and must not be displayed by
	// an Approver unless the Tool explicitly derives a safe ApprovalPreview
	Arguments json.RawMessage
	// ArgumentsDigest binds approval metadata to the exact Arguments bytes
	ArgumentsDigest string
	// ExtensionID identifies the Extension that would execute the Tool
	ExtensionID string
	// ExtensionGeneration identifies the pinned Extension process generation
	ExtensionGeneration uint64
	// Preview contains optional bounded content for human review
	Preview *ApprovalPreview
}

// Decision records one Policy evaluation
type Decision struct {
	Outcome Outcome
	Reason  string
}

// Policy evaluates Tool capability requests without executing the Tool
type Policy interface {
	Evaluate(ctx context.Context, request Request) (Decision, error)
}

// Approver resolves a Policy decision whose outcome is ask
type Approver interface {
	Approve(ctx context.Context, request Request) (bool, error)
}

// ApproverFunc adapts a function to Approver
type ApproverFunc func(context.Context, Request) (bool, error)

// Approve resolves one approval request
func (approver ApproverFunc) Approve(ctx context.Context, request Request) (bool, error) {
	if approver == nil {
		return false, errors.New("approver function is nil")
	}
	return approver(ctx, request)
}

// StaticPolicyOptions configures capability outcomes by name
type StaticPolicyOptions struct {
	Allow []Name
	Ask   []Name
	Deny  []Name
}

// StaticPolicy applies immutable capability rules and denies unspecified names
type StaticPolicy struct {
	rules map[Name]Outcome
}

// NewStaticPolicy validates and constructs an immutable StaticPolicy
func NewStaticPolicy(options StaticPolicyOptions) (*StaticPolicy, error) {
	policy := &StaticPolicy{rules: make(map[Name]Outcome)}
	groups := []struct {
		outcome Outcome
		names   []Name
	}{
		{outcome: OutcomeAllow, names: options.Allow},
		{outcome: OutcomeAsk, names: options.Ask},
		{outcome: OutcomeDeny, names: options.Deny},
	}
	for _, group := range groups {
		for _, name := range group.names {
			if err := ValidateName(name); err != nil {
				return nil, err
			}
			if previous, exists := policy.rules[name]; exists {
				return nil, fmt.Errorf("capability %q has both %q and %q rules", name, previous, group.outcome)
			}
			policy.rules[name] = group.outcome
		}
	}
	return policy, nil
}

// Evaluate returns deny when any capability is denied or unspecified, ask
// when at least one capability requires approval, and allow otherwise
func (policy *StaticPolicy) Evaluate(ctx context.Context, request Request) (Decision, error) {
	if policy == nil {
		return Decision{}, errors.New("StaticPolicy must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	names := append([]Name(nil), request.Capabilities...)
	sort.Slice(names, func(first, second int) bool { return names[first] < names[second] })
	decision := Decision{Outcome: OutcomeAllow, Reason: "all requested capabilities are allowed"}
	for _, name := range names {
		outcome, configured := policy.rules[name]
		if !configured {
			return Decision{Outcome: OutcomeDeny, Reason: fmt.Sprintf("capability %q is not configured", name)}, nil
		}
		if outcome == OutcomeDeny {
			return Decision{Outcome: OutcomeDeny, Reason: fmt.Sprintf("capability %q is denied", name)}, nil
		}
		if outcome == OutcomeAsk {
			decision = Decision{Outcome: OutcomeAsk, Reason: fmt.Sprintf("capability %q requires approval", name)}
		}
	}
	return decision, nil
}

// ValidateName validates an extensible dot-separated capability name
func ValidateName(name Name) error {
	value := string(name)
	if value == "" || strings.TrimSpace(value) != value {
		return errors.New("capability name is required and must not have surrounding whitespace")
	}
	for _, segment := range strings.Split(value, ".") {
		if segment == "" {
			return errors.New("capability name must contain non-empty dot-separated segments")
		}
		for _, character := range segment {
			if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
				(character >= '0' && character <= '9') || character == '_' || character == '-' {
				continue
			}
			return errors.New("capability name contains an unsupported character")
		}
	}
	return nil
}
