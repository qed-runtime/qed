// Package greeting provides a downstream self-exec Extension fixture
package greeting

import (
	"context"
	"encoding/json"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/extension/server"
)

type tool struct{}

func (tool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:         "greet",
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		Capabilities: []string{"downstream.read"},
	}
}

func (tool) Execute(_ context.Context, call agent.ToolCall) (agent.ToolResult, error) {
	return agent.ToolResult{CallID: call.ID, Name: call.Name, Output: "hello from downstream"}, nil
}

// ServerOptions returns the downstream Extension protocol server
func ServerOptions() server.Options {
	return server.Options{
		ID:      "downstream.greeting",
		Version: "1.0.0",
		Initialize: func(context.Context, protocol.InitializeRequest) ([]agent.Tool, error) {
			return []agent.Tool{tool{}}, nil
		},
	}
}
