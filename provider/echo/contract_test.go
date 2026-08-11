package echo_test

import (
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/provider/contracttest"
	"github.com/qed-runtime/qed/provider/echo"
)

func TestProviderTextContract(t *testing.T) {
	t.Parallel()
	contracttest.RunText(t, contracttest.TextOptions{
		Provider: echo.New(),
		Request: agent.ModelRequest{Messages: []agent.Message{
			{Role: agent.RoleUser, Text: "first"},
			{Role: agent.RoleAssistant, Text: "ignored"},
			{Role: agent.RoleUser, Text: contracttest.FixtureText},
		}},
		ExpectedText:   contracttest.FixtureText,
		ExpectedDeltas: []string{contracttest.FixtureText},
		AssertMessage: func(t *testing.T, message agent.Message) {
			if message.ResponseID != "" || message.Model != "" || message.ProviderState != nil {
				t.Errorf("Echo Provider-specific fields = %#v", message)
			}
		},
	})
}
