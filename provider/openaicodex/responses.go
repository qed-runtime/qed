package openaicodex

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/qed-runtime/qed/agent"
)

var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

type responsesRequest struct {
	Model             string            `json:"model"`
	Instructions      string            `json:"instructions"`
	Input             []json.RawMessage `json:"input"`
	Tools             []responsesTool   `json:"tools,omitempty"`
	ToolChoice        string            `json:"tool_choice"`
	ParallelToolCalls bool              `json:"parallel_tool_calls"`
	Store             bool              `json:"store"`
	Stream            bool              `json:"stream"`
	Include           []string          `json:"include"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type responsesResponse struct {
	ID                string            `json:"id"`
	Model             string            `json:"model"`
	Status            string            `json:"status"`
	Output            []json.RawMessage `json:"output"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	} `json:"usage"`
}

func (provider *Provider) responsesRequest(request agent.ModelRequest) (responsesRequest, error) {
	input := make([]json.RawMessage, 0, len(request.Messages))
	for _, message := range request.Messages {
		if message.Role == agent.RoleAssistant {
			if rawState, ok := stateData(message, provider.Name()); ok {
				var items []json.RawMessage
				if err := json.Unmarshal(rawState, &items); err != nil {
					return responsesRequest{}, fmt.Errorf("decode OpenAI Codex continuation state: %w", err)
				}
				input = append(input, items...)
				continue
			}
		}

		switch message.Role {
		case agent.RoleUser:
			raw, err := messageInput("user", "input_text", message.Text)
			if err != nil {
				return responsesRequest{}, err
			}
			input = append(input, raw)
		case agent.RoleAssistant:
			if message.Text != "" || len(message.ToolCalls) == 0 {
				raw, err := messageInput("assistant", "output_text", message.Text)
				if err != nil {
					return responsesRequest{}, err
				}
				input = append(input, raw)
			}
			for _, call := range message.ToolCalls {
				if call.ID == "" || call.Name == "" {
					return responsesRequest{}, errors.New("assistant tool call ID and name are required")
				}
				raw, err := json.Marshal(struct {
					Type      string `json:"type"`
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Type:      "function_call",
					CallID:    call.ID,
					Name:      call.Name,
					Arguments: toolArguments(call),
				})
				if err != nil {
					return responsesRequest{}, err
				}
				input = append(input, raw)
			}
		case agent.RoleTool:
			if message.ToolCallID == "" {
				return responsesRequest{}, errors.New("tool message call ID is required")
			}
			raw, err := json.Marshal(struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Output string `json:"output"`
			}{Type: "function_call_output", CallID: message.ToolCallID, Output: message.Text})
			if err != nil {
				return responsesRequest{}, err
			}
			input = append(input, raw)
		default:
			return responsesRequest{}, fmt.Errorf("unsupported message role %q", message.Role)
		}
	}

	tools := make([]responsesTool, 0, len(request.Tools))
	for _, definition := range request.Tools {
		if definition.Name == "" {
			return responsesRequest{}, errors.New("tool name is required")
		}
		schema, err := toolSchema(definition)
		if err != nil {
			return responsesRequest{}, err
		}
		tools = append(tools, responsesTool{
			Type:        "function",
			Name:        definition.Name,
			Description: definition.Description,
			Parameters:  schema,
			Strict:      false,
		})
	}

	instructions := request.Instructions
	if strings.TrimSpace(instructions) == "" {
		instructions = defaultInstructions
	}
	return responsesRequest{
		Model:             provider.model,
		Instructions:      instructions,
		Input:             input,
		Tools:             tools,
		ToolChoice:        "auto",
		ParallelToolCalls: true,
		Store:             false,
		Stream:            true,
		Include:           []string{"reasoning.encrypted_content"},
	}, nil
}

func messageInput(role, contentType, text string) (json.RawMessage, error) {
	return json.Marshal(struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}{
		Type: "message",
		Role: role,
		Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: contentType, Text: text}},
	})
}

func (provider *Provider) messageFromResponsesResponse(response responsesResponse) (agent.Message, error) {
	if response.Error != nil {
		return agent.Message{}, fmt.Errorf("OpenAI Codex response failed %s: %s", response.Error.Code, response.Error.Message)
	}
	if response.Status == "failed" || response.Status == "cancelled" {
		return agent.Message{}, fmt.Errorf("OpenAI Codex response ended with status %q", response.Status)
	}

	var text strings.Builder
	var toolCalls []agent.ToolCall
	for _, rawItem := range response.Output {
		var itemType struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(rawItem, &itemType); err != nil {
			return agent.Message{}, fmt.Errorf("decode OpenAI Codex output item: %w", err)
		}
		switch itemType.Type {
		case "message":
			var item struct {
				Content []json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(rawItem, &item); err != nil {
				return agent.Message{}, fmt.Errorf("decode OpenAI Codex message output: %w", err)
			}
			for _, rawContent := range item.Content {
				var content struct {
					Type    string `json:"type"`
					Text    string `json:"text"`
					Refusal string `json:"refusal"`
				}
				if err := json.Unmarshal(rawContent, &content); err != nil {
					return agent.Message{}, fmt.Errorf("decode OpenAI Codex message content: %w", err)
				}
				switch content.Type {
				case "output_text":
					text.WriteString(content.Text)
				case "refusal":
					text.WriteString(content.Refusal)
				}
			}
		case "function_call":
			var call struct {
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			if err := json.Unmarshal(rawItem, &call); err != nil {
				return agent.Message{}, fmt.Errorf("decode OpenAI Codex function call: %w", err)
			}
			if call.CallID == "" || call.Name == "" {
				return agent.Message{}, errors.New("OpenAI Codex Responses returned an incomplete function call")
			}
			arguments := json.RawMessage(call.Arguments)
			if len(arguments) == 0 {
				arguments = json.RawMessage(`{}`)
			}
			if !json.Valid(arguments) {
				return agent.Message{}, fmt.Errorf("OpenAI Codex function %q returned invalid JSON arguments", call.Name)
			}
			toolCalls = append(toolCalls, agent.ToolCall{ID: call.CallID, Name: call.Name, Arguments: arguments})
		}
	}

	state, err := json.Marshal(response.Output)
	if err != nil {
		return agent.Message{}, fmt.Errorf("preserve OpenAI Codex response state: %w", err)
	}
	rawStopReason := response.Status
	if response.IncompleteDetails != nil && response.IncompleteDetails.Reason != "" {
		rawStopReason = response.IncompleteDetails.Reason
	}
	if len(toolCalls) > 0 {
		rawStopReason = "tool_use"
	}
	return agent.Message{
		Role:          agent.RoleAssistant,
		Text:          text.String(),
		ToolCalls:     toolCalls,
		StopReason:    mapStopReason(rawStopReason, len(toolCalls) > 0),
		RawStopReason: rawStopReason,
		Usage:         usage(response.Usage.InputTokens, response.Usage.OutputTokens, response.Usage.TotalTokens),
		ResponseID:    response.ID,
		Model:         response.Model,
		ProviderState: &agent.ProviderState{Provider: provider.Name(), Data: state},
	}, nil
}

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
