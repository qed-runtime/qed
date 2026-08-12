package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrInvalidFactDirective indicates malformed or unsupported Fact lifecycle input
	ErrInvalidFactDirective = errors.New("invalid Fact lifecycle directive")
)

// ConstraintFactID returns the stable ID for a user.message.added Event source
//
// Modern Events require RunID and Sequence. Legacy persisted Events require a
// zero RunID and Sequence plus a positive SessionRevision.
func ConstraintFactID(source ContextLedgerEventRef) (string, error) {
	if source.RunID != "" {
		if source.Sequence == 0 {
			return "", errors.New("Constraint Fact source Sequence must be positive")
		}
		return contextLedgerID("constraint", source.RunID, fmt.Sprint(source.Sequence)), nil
	}
	if source.Sequence != 0 || source.SessionRevision == 0 {
		return "", errors.New("legacy Constraint Fact source requires only a positive Session revision")
	}
	return contextLedgerID("constraint", "session", fmt.Sprint(source.SessionRevision)), nil
}

func validateFactLifecycleDirective(directive *FactLifecycleDirective) error {
	if directive == nil {
		return nil
	}
	switch directive.Action {
	case FactLifecycleSupersede, FactLifecycleResolve:
	default:
		return fmt.Errorf("%w: unsupported action %q", ErrInvalidFactDirective, directive.Action)
	}
	if len(directive.Targets) == 0 {
		return fmt.Errorf("%w: at least one target is required", ErrInvalidFactDirective)
	}
	if len(directive.Targets) > MaxFactLifecycleTargets {
		return fmt.Errorf("%w: target count exceeds %d", ErrInvalidFactDirective, MaxFactLifecycleTargets)
	}
	seen := make(map[string]struct{}, len(directive.Targets))
	for _, target := range directive.Targets {
		if strings.TrimSpace(target) != target || !validConstraintFactID(target) {
			return fmt.Errorf("%w: target %q is not a Constraint Fact ID", ErrInvalidFactDirective, target)
		}
		if _, duplicate := seen[target]; duplicate {
			return fmt.Errorf("%w: target %q is duplicated", ErrInvalidFactDirective, target)
		}
		seen[target] = struct{}{}
	}
	return nil
}

func validateFactDirectiveMessage(message Message) error {
	if message.FactDirective == nil {
		return nil
	}
	if message.Role != RoleUser {
		return fmt.Errorf("%w: directive requires role %q", ErrInvalidFactDirective, RoleUser)
	}
	if strings.TrimSpace(message.Text) == "" {
		return fmt.Errorf("%w: directive requires non-empty user text", ErrInvalidFactDirective)
	}
	return validateFactLifecycleDirective(message.FactDirective)
}

func validConstraintFactID(value string) bool {
	const prefix = "constraint_"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

func cloneFactLifecycleDirective(directive *FactLifecycleDirective) *FactLifecycleDirective {
	if directive == nil {
		return nil
	}
	cloned := *directive
	cloned.Targets = append([]string(nil), directive.Targets...)
	return &cloned
}
