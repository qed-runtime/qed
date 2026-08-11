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

func (provider *Provider) validatedCachePlan(request agent.ModelRequest) (*agent.CachePlan, error) {
	plan := request.CachePlan
	if plan == nil || plan.Mode == agent.CacheModeDisabled {
		return nil, nil
	}
	capabilities := provider.CacheCapabilities()
	if plan.FamilyID == "" {
		return nil, errors.New("OpenAI Cache Plan family is required")
	}
	switch plan.Mode {
	case agent.CacheModeAutomatic:
		if !capabilities.SupportsAutomatic {
			return nil, errors.New("OpenAI automatic prompt cache is unsupported by this endpoint")
		}
	case agent.CacheModeExplicit:
		if !capabilities.SupportsExplicit {
			return nil, errors.New("OpenAI explicit prompt cache is unsupported by this API or model")
		}
		if len(plan.Breakpoints) == 0 || len(plan.Breakpoints) > capabilities.MaxWriteBreakpoints {
			return nil, errors.New("OpenAI explicit Cache Plan has an invalid breakpoint count")
		}
	default:
		return nil, fmt.Errorf("unsupported OpenAI Cache Plan mode %q", plan.Mode)
	}
	for _, breakpoint := range plan.Breakpoints {
		if breakpoint.MessageIndex < 0 || breakpoint.MessageIndex >= len(request.Messages) ||
			request.Messages[breakpoint.MessageIndex].Role != agent.RoleUser {
			return nil, errors.New("OpenAI Cache Plan breakpoint does not identify a user message")
		}
	}
	return plan, nil
}

type inputTokenDetails struct {
	CachedTokens     int64 `json:"cached_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

func usage(input, output, total int64, details *inputTokenDetails) *agent.Usage {
	if input == 0 && output == 0 && total == 0 {
		return nil
	}
	if total == 0 {
		total = input + output
	}
	reported := &agent.Usage{InputTokens: input, OutputTokens: output, TotalTokens: total}
	if details == nil {
		return reported
	}
	if input < 0 || details.CachedTokens < 0 || details.CacheWriteTokens < 0 ||
		details.CachedTokens > input || details.CacheWriteTokens > input-details.CachedTokens {
		return reported
	}
	classified := details.CachedTokens + details.CacheWriteTokens
	reported.InputTokenDetailsReported = true
	reported.UncachedInputTokens = input - classified
	reported.CacheReadInputTokens = details.CachedTokens
	reported.CacheWriteInputTokens = details.CacheWriteTokens
	return reported
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
