package capability_test

import (
	"context"
	"testing"

	"github.com/qed-runtime/qed/capability"
)

func TestStaticPolicyUsesMostRestrictiveOutcome(t *testing.T) {
	t.Parallel()

	policy, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{
		Allow: []capability.Name{capability.FilesystemRead},
		Ask:   []capability.Name{capability.FilesystemWrite},
		Deny:  []capability.Name{capability.FilesystemDelete},
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		caps []capability.Name
		want capability.Outcome
	}{
		{name: "allow", caps: []capability.Name{capability.FilesystemRead}, want: capability.OutcomeAllow},
		{name: "ask", caps: []capability.Name{capability.FilesystemRead, capability.FilesystemWrite}, want: capability.OutcomeAsk},
		{name: "deny", caps: []capability.Name{capability.FilesystemWrite, capability.FilesystemDelete}, want: capability.OutcomeDeny},
		{name: "unspecified", caps: []capability.Name{capability.ProcessExecute}, want: capability.OutcomeDeny},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			decision, err := policy.Evaluate(context.Background(), capability.Request{Capabilities: test.caps})
			if err != nil || decision.Outcome != test.want {
				t.Errorf("Evaluate() = %#v, %v, want %q", decision, err, test.want)
			}
		})
	}
}

func TestStaticPolicyRejectsConflictingRules(t *testing.T) {
	t.Parallel()

	_, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{
		Allow: []capability.Name{capability.FilesystemRead},
		Deny:  []capability.Name{capability.FilesystemRead},
	})
	if err == nil {
		t.Error("NewStaticPolicy() error = nil")
	}
}
