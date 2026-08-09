// Package greeting provides the example Extension linked into embedded-server
package greeting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/extension/server"
)

const (
	// ID is the stable example Extension identifier
	ID = "example.greeting"
	// Version is the example Extension implementation version
	Version = "0.1.0"
)

// ServerOptions returns the linked Extension protocol server
func ServerOptions() server.Options {
	return server.Options{
		ID:      ID,
		Version: Version,
		Initialize: func(ctx context.Context, _ protocol.InitializeRequest) ([]agent.Tool, error) {
			if ctx == nil {
				return nil, errors.New("greeting Extension context must not be nil")
			}
			return []agent.Tool{greetingTool{}}, nil
		},
	}
}

type greetingTool struct{}

func (greetingTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:         "greet",
		Description:  "Build a greeting for one supplied name",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`),
		Capabilities: []string{"example.read"},
	}
}

func (greetingTool) Execute(_ context.Context, call agent.ToolCall) (agent.ToolResult, error) {
	var input struct {
		Name string `json:"name"`
	}
	decoder := json.NewDecoder(bytes.NewReader(call.Arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return agent.ToolResult{}, fmt.Errorf("decode greeting arguments: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return agent.ToolResult{}, errors.New("greeting arguments contain a trailing JSON value")
		}
		return agent.ToolResult{}, fmt.Errorf("decode trailing greeting arguments: %w", err)
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return agent.ToolResult{}, errors.New("greeting name is required")
	}
	return agent.ToolResult{CallID: call.ID, Name: call.Name, Output: "Hello, " + name}, nil
}
