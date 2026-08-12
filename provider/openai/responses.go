package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/qed-runtime/qed/agent"
	providerbase "github.com/qed-runtime/qed/provider"
	"github.com/qed-runtime/qed/provider/internal/httpjson"
)

type responsesRequest struct {
	Model              string                    `json:"model"`
	Instructions       string                    `json:"instructions,omitempty"`
	Input              []json.RawMessage         `json:"input"`
	Tools              []responsesTool           `json:"tools,omitempty"`
	MaxOutputTokens    int                       `json:"max_output_tokens,omitempty"`
	PromptCacheKey     string                    `json:"prompt_cache_key,omitempty"`
	PromptCacheOptions *openAIPromptCacheOptions `json:"prompt_cache_options,omitempty"`
	Stream             bool                      `json:"stream,omitempty"`
}

type openAIPromptCacheOptions struct {
	Mode string         `json:"mode"`
	TTL  agent.CacheTTL `json:"ttl,omitempty"`
}

type openAIPromptCacheBreakpoint struct {
	Mode string `json:"mode"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
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
		InputTokens        int64              `json:"input_tokens"`
		OutputTokens       int64              `json:"output_tokens"`
		TotalTokens        int64              `json:"total_tokens"`
		InputTokensDetails *inputTokenDetails `json:"input_tokens_details"`
	} `json:"usage"`
}

func (provider *Provider) completeResponses(ctx context.Context, request agent.ModelRequest) (agent.Message, error) {
	payload, err := provider.responsesRequest(request)
	if err != nil {
		return agent.Message{}, err
	}
	headers, err := provider.headers(ctx)
	if err != nil {
		return agent.Message{}, err
	}

	var response responsesResponse
	if err := httpjson.Post(ctx, provider.client, provider.endpoint, headers, payload, &response); err != nil {
		return agent.Message{}, err
	}
	return provider.messageFromResponsesResponse(response)
}

func (provider *Provider) messageFromResponsesResponse(response responsesResponse) (agent.Message, error) {
	if response.Error != nil {
		return agent.Message{}, fmt.Errorf("OpenAI response failed: %w", &providerbase.APIError{
			Code:    response.Error.Code,
			Message: response.Error.Message,
		})
	}
	if response.Status == "failed" || response.Status == "cancelled" {
		return agent.Message{}, fmt.Errorf("OpenAI response ended with status %q", response.Status)
	}

	var text strings.Builder
	var toolCalls []agent.ToolCall
	for _, rawItem := range response.Output {
		var itemType struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(rawItem, &itemType); err != nil {
			return agent.Message{}, fmt.Errorf("decode OpenAI output item: %w", err)
		}

		switch itemType.Type {
		case "message":
			var item struct {
				Content []json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(rawItem, &item); err != nil {
				return agent.Message{}, fmt.Errorf("decode OpenAI message output: %w", err)
			}
			for _, rawContent := range item.Content {
				var content struct {
					Type    string `json:"type"`
					Text    string `json:"text"`
					Refusal string `json:"refusal"`
				}
				if err := json.Unmarshal(rawContent, &content); err != nil {
					return agent.Message{}, fmt.Errorf("decode OpenAI message content: %w", err)
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
				return agent.Message{}, fmt.Errorf("decode OpenAI function call: %w", err)
			}
			if call.CallID == "" || call.Name == "" {
				return agent.Message{}, errors.New("OpenAI Responses returned an incomplete function call")
			}
			arguments := json.RawMessage(call.Arguments)
			if len(arguments) == 0 {
				arguments = json.RawMessage(`{}`)
			}
			toolCalls = append(toolCalls, agent.ToolCall{
				ID:        call.CallID,
				Name:      call.Name,
				Arguments: arguments,
			})
		}
	}

	state, err := json.Marshal(response.Output)
	if err != nil {
		return agent.Message{}, fmt.Errorf("preserve OpenAI response state: %w", err)
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
		Usage: usage(
			response.Usage.InputTokens,
			response.Usage.OutputTokens,
			response.Usage.TotalTokens,
			response.Usage.InputTokensDetails,
		),
		ResponseID: response.ID,
		Model:      response.Model,
		ProviderState: &agent.ProviderState{
			Provider: provider.Name(),
			Data:     state,
		},
	}, nil
}

func (provider *Provider) responsesRequest(request agent.ModelRequest) (responsesRequest, error) {
	cachePlan, err := provider.validatedCachePlan(request)
	if err != nil {
		return responsesRequest{}, err
	}
	breakpoints := make(map[int]struct{})
	if cachePlan != nil && cachePlan.Mode == agent.CacheModeExplicit {
		for _, breakpoint := range cachePlan.Breakpoints {
			breakpoints[breakpoint.MessageIndex] = struct{}{}
		}
	}
	input := make([]json.RawMessage, 0, len(request.Messages))
	for messageIndex, message := range request.Messages {
		if message.Role == agent.RoleAssistant {
			if rawState, ok := stateData(message, provider.Name()); ok {
				var items []json.RawMessage
				if err := json.Unmarshal(rawState, &items); err != nil {
					return responsesRequest{}, fmt.Errorf("decode OpenAI continuation state: %w", err)
				}
				input = append(input, items...)
				continue
			}
		}

		switch message.Role {
		case agent.RoleUser:
			_, marked := breakpoints[messageIndex]
			raw, err := responsesInputMessage("user", "input_text", message.Text, marked)
			if err != nil {
				return responsesRequest{}, err
			}
			input = append(input, raw)
		case agent.RoleAssistant:
			if message.Text != "" || len(message.ToolCalls) == 0 {
				raw, err := responsesInputMessage("assistant", "output_text", message.Text, false)
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
		})
	}

	payload := responsesRequest{
		Model:           provider.model,
		Instructions:    request.Instructions,
		Input:           input,
		Tools:           tools,
		MaxOutputTokens: provider.maxOutputTokens,
	}
	if cachePlan != nil && provider.CacheCapabilities().SupportsCacheKey {
		payload.PromptCacheKey = cachePlan.FamilyID
	}
	if cachePlan != nil && cachePlan.Mode == agent.CacheModeExplicit {
		payload.PromptCacheOptions = &openAIPromptCacheOptions{Mode: "explicit", TTL: cachePlan.TTL}
	}
	return payload, nil
}

func responsesInputMessage(role, contentType, text string, breakpoint bool) (json.RawMessage, error) {
	if !breakpoint {
		return json.Marshal(struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content string `json:"content"`
		}{Type: "message", Role: role, Content: text})
	}
	return json.Marshal(struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type                  string                       `json:"type"`
			Text                  string                       `json:"text"`
			PromptCacheBreakpoint *openAIPromptCacheBreakpoint `json:"prompt_cache_breakpoint,omitempty"`
		} `json:"content"`
	}{
		Type: "message",
		Role: role,
		Content: []struct {
			Type                  string                       `json:"type"`
			Text                  string                       `json:"text"`
			PromptCacheBreakpoint *openAIPromptCacheBreakpoint `json:"prompt_cache_breakpoint,omitempty"`
		}{{
			Type:                  contentType,
			Text:                  text,
			PromptCacheBreakpoint: &openAIPromptCacheBreakpoint{Mode: "explicit"},
		}},
	})
}
