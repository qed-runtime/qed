package echo_test

import (
	"context"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/provider/echo"
)

func TestProviderReturnsMostRecentUserMessage(t *testing.T) {
	t.Parallel()

	provider := echo.New()
	message, err := provider.Complete(context.Background(), agent.ModelRequest{
		Messages: []agent.Message{
			{Role: agent.RoleUser, Text: "first"},
			{Role: agent.RoleAssistant, Text: "response"},
			{Role: agent.RoleUser, Text: "second"},
		},
	})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if message.Role != agent.RoleAssistant || message.Text != "second" || message.StopReason != agent.StopReasonEndTurn {
		t.Errorf("Complete() = %#v", message)
	}
}

func TestProfileName(t *testing.T) {
	t.Parallel()

	provider, err := echo.NewProfile("local")
	if err != nil {
		t.Fatalf("NewProfile() error = %v", err)
	}
	if provider.Name() != "echo:local" {
		t.Errorf("Name() = %q", provider.Name())
	}
	if got := (&echo.Provider{}).Name(); got != "echo" {
		t.Errorf("zero Provider Name() = %q", got)
	}
}
