package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/qed-runtime/qed/agent"
)

var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

func toolSchema(definition agent.ToolDefinition) (json.RawMessage, error) {
	if len(definition.InputSchema) == 0 {
		return append(json.RawMessage(nil), emptyObjectSchema...), nil
	}
	if !json.Valid(definition.InputSchema) {
		return nil, fmt.Errorf("tool %q has an invalid input schema", definition.Name)
	}
	return append(json.RawMessage(nil), definition.InputSchema...), nil
}

func toolArguments(call agent.ToolCall) string {
	if len(call.Arguments) == 0 {
		return "{}"
	}
	return string(call.Arguments)
}

func stateData(message agent.Message, providerName string) (json.RawMessage, bool) {
	if message.ProviderState == nil || message.ProviderState.Provider != providerName || len(message.ProviderState.Data) == 0 {
		return nil, false
	}
	return message.ProviderState.Data, true
}

func usage(input, output, total int64) *agent.Usage {
	if input == 0 && output == 0 && total == 0 {
		return nil
	}
	if total == 0 {
		total = input + output
	}
	return &agent.Usage{InputTokens: input, OutputTokens: output, TotalTokens: total}
}

func mapStopReason(raw string, hasToolCalls bool) agent.StopReason {
	if hasToolCalls {
		return agent.StopReasonToolUse
	}
	switch raw {
	case "stop", "completed", "end_turn":
		return agent.StopReasonEndTurn
	case "tool_calls", "tool_use":
		return agent.StopReasonToolUse
	case "length", "max_tokens", "max_output_tokens":
		return agent.StopReasonMaxTokens
	case "content_filter":
		return agent.StopReasonContentFilter
	case "refusal":
		return agent.StopReasonRefusal
	default:
		return agent.StopReasonUnknown
	}
}

type textContent string

func (content *textContent) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		*content = ""
		return nil
	}

	var text string
	if json.Unmarshal(data, &text) == nil {
		*content = textContent(text)
		return nil
	}

	var parts []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	}
	if err := json.Unmarshal(data, &parts); err != nil {
		return errors.New("message content must be a string, array, or null")
	}
	var builder strings.Builder
	for _, part := range parts {
		switch part.Type {
		case "text", "output_text":
			builder.WriteString(part.Text)
		case "refusal":
			builder.WriteString(part.Refusal)
		}
	}
	*content = textContent(builder.String())
	return nil
}
