package process

import (
	"testing"

	"github.com/qed-runtime/qed/agent"
)

func TestClassifyContextOperation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		argv []string
		kind agent.ContextOperationKind
	}{
		{name: "empty"},
		{name: "go test", argv: []string{"go", "test", "./..."}, kind: agent.ContextOperationVerification},
		{name: "absolute go is conservative", argv: []string{"/usr/local/bin/go", "test", "./..."}, kind: agent.ContextOperationMutation},
		{name: "npm lint", argv: []string{"npm", "run", "lint"}, kind: agent.ContextOperationVerification},
		{name: "git commit", argv: []string{"git", "commit", "-m", "change"}, kind: agent.ContextOperationCommit},
		{name: "ordinary command", argv: []string{"printf", "ok"}, kind: agent.ContextOperationMutation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			operation := classifyContextOperation(test.argv)
			if test.kind == "" {
				if operation != nil {
					t.Fatalf("Context operation = %#v, want nil", operation)
				}
				return
			}
			if operation == nil || operation.Kind != test.kind {
				t.Fatalf("Context operation = %#v, want %q", operation, test.kind)
			}
		})
	}
}
