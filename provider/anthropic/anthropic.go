// Package anthropic provides an Anthropic Messages Provider
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/qed-runtime/qed/agent"
	providerbase "github.com/qed-runtime/qed/provider"
	"github.com/qed-runtime/qed/provider/internal/httpjson"
	providerprofile "github.com/qed-runtime/qed/provider/internal/profile"
)

const (
	defaultBaseURL         = "https://api.anthropic.com/v1"
	defaultAPIVersion      = "2023-06-01"
	defaultMaxOutputTokens = 1024
)

var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// Config configures an Anthropic Messages Provider
type Config struct {
	// ProfileID distinguishes this Provider instance from other Anthropic instances
	ProfileID string
	// APIKey is sent in the x-api-key header when non-empty
	APIKey string
	// CredentialSource resolves an API key for every request
	//
	// APIKey and CredentialSource are mutually exclusive
	CredentialSource providerbase.CredentialSource
	// BaseURL defaults to https://api.anthropic.com/v1
	//
	// A custom BaseURL receives the configured credential and should only be used when trusted
	BaseURL string
	// APIVersion defaults to 2023-06-01
	APIVersion string
	// Model is the exact model identifier sent with every request
	Model string
	// MaxOutputTokens defaults to 1024 when zero
	MaxOutputTokens int
	// HTTPClient defaults to http.DefaultClient
	HTTPClient providerbase.HTTPClient
	// CacheCapabilities overrides the standard Anthropic Messages declaration
	CacheCapabilities *agent.CacheCapabilities
}

// Provider implements agent.Provider using the Anthropic Messages API
type Provider struct {
	apiKey            string
	credentialSource  providerbase.CredentialSource
	apiVersion        string
	endpoint          string
	model             string
	maxOutputTokens   int
	client            providerbase.HTTPClient
	name              string
	official          bool
	cacheCapabilities *agent.CacheCapabilities
}

// New validates config and constructs an Anthropic Provider
func New(config Config) (*Provider, error) {
	if config.APIKey != "" && config.CredentialSource != nil {
		return nil, errors.New("Anthropic API key and credential source are mutually exclusive")
	}
	name, err := providerprofile.Name("anthropic/messages", config.ProfileID)
	if err != nil {
		return nil, fmt.Errorf("configure Anthropic profile: %w", err)
	}
	model := strings.TrimSpace(config.Model)
	if model == "" {
		return nil, errors.New("Anthropic model is required")
	}

	maxOutputTokens := config.MaxOutputTokens
	if maxOutputTokens == 0 {
		maxOutputTokens = defaultMaxOutputTokens
	}
	if maxOutputTokens < 0 {
		return nil, errors.New("Anthropic max output tokens must be positive")
	}

	apiVersion := strings.TrimSpace(config.APIVersion)
	if apiVersion == "" {
		apiVersion = defaultAPIVersion
	}
	endpoint, err := httpjson.Endpoint(config.BaseURL, defaultBaseURL, "messages")
	if err != nil {
		return nil, fmt.Errorf("configure Anthropic endpoint: %w", err)
	}
	if config.CacheCapabilities != nil {
		if err := agent.ValidateCacheCapabilities(*config.CacheCapabilities); err != nil {
			return nil, fmt.Errorf("configure Anthropic Cache Capabilities: %w", err)
		}
	}

	return &Provider{
		apiKey:            config.APIKey,
		credentialSource:  config.CredentialSource,
		apiVersion:        apiVersion,
		endpoint:          endpoint,
		model:             model,
		maxOutputTokens:   maxOutputTokens,
		client:            config.HTTPClient,
		name:              name,
		official:          config.BaseURL == "" || strings.TrimRight(config.BaseURL, "/") == defaultBaseURL,
		cacheCapabilities: cloneCacheCapabilities(config.CacheCapabilities),
	}, nil
}

// Name returns the Anthropic API dialect used in diagnostics
func (provider *Provider) Name() string {
	return provider.name
}

// ModelID returns the exact model identifier configured for this Provider
func (provider *Provider) ModelID() string {
	return provider.model
}

// CacheCapabilities reports prompt cache behavior for Anthropic Messages
func (provider *Provider) CacheCapabilities() agent.CacheCapabilities {
	if provider != nil && provider.cacheCapabilities != nil {
		return *cloneCacheCapabilities(provider.cacheCapabilities)
	}
	if provider == nil || !provider.official {
		return agent.CacheCapabilities{}
	}
	return agent.CacheCapabilities{
		ExactPrefix:         true,
		SupportsExplicit:    true,
		SupportsAutomatic:   true,
		MaxWriteBreakpoints: 4,
		MinimumPrefixTokens: anthropicMinimumCacheTokens(provider.model),
		SupportedTTLs: []agent.CacheTTL{
			agent.CacheTTLFiveMinutes,
			agent.CacheTTLOneHour,
		},
		ExposesReadTokens:  true,
		ExposesWriteTokens: true,
	}
}

func anthropicMinimumCacheTokens(model string) int64 {
	model = strings.ToLower(model)
	switch {
	case strings.Contains(model, "fable-5"), strings.Contains(model, "mythos-5"):
		return 512
	case strings.Contains(model, "mythos-preview"), strings.Contains(model, "opus-4-7"):
		return 2048
	case strings.Contains(model, "opus-4-6"), strings.Contains(model, "opus-4-5"),
		strings.Contains(model, "haiku-4-5"):
		return 4096
	case strings.Contains(model, "haiku-3-5"):
		return 2048
	case strings.Contains(model, "opus-4-8"), strings.Contains(model, "sonnet-5"),
		strings.Contains(model, "sonnet-4"), strings.Contains(model, "opus-4"):
		return 1024
	default:
		return 4096
	}
}

func cloneCacheCapabilities(capabilities *agent.CacheCapabilities) *agent.CacheCapabilities {
	if capabilities == nil {
		return nil
	}
	cloned := *capabilities
	cloned.SupportedTTLs = append([]agent.CacheTTL(nil), capabilities.SupportedTTLs...)
	return &cloned
}

// Complete sends one Messages API request and returns its completed Message
func (provider *Provider) Complete(ctx context.Context, request agent.ModelRequest) (agent.Message, error) {
	if ctx == nil {
		return agent.Message{}, errors.New("Anthropic context must not be nil")
	}

	payload, err := provider.messagesRequest(request)
	if err != nil {
		return agent.Message{}, err
	}

	var response messagesResponse
	headers, err := provider.headers(ctx)
	if err != nil {
		return agent.Message{}, err
	}
	if err := httpjson.Post(ctx, provider.client, provider.endpoint, headers, payload, &response); err != nil {
		return agent.Message{}, err
	}
	return provider.messageFromMessagesResponse(response)
}

func (provider *Provider) messageFromMessagesResponse(response messagesResponse) (agent.Message, error) {
	var text strings.Builder
	var toolCalls []agent.ToolCall
	for _, rawBlock := range response.Content {
		var blockType struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(rawBlock, &blockType); err != nil {
			return agent.Message{}, fmt.Errorf("decode Anthropic content block: %w", err)
		}

		switch blockType.Type {
		case "text":
			var block struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(rawBlock, &block); err != nil {
				return agent.Message{}, fmt.Errorf("decode Anthropic text block: %w", err)
			}
			text.WriteString(block.Text)
		case "tool_use":
			var block struct {
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(rawBlock, &block); err != nil {
				return agent.Message{}, fmt.Errorf("decode Anthropic tool use block: %w", err)
			}
			if block.ID == "" || block.Name == "" {
				return agent.Message{}, errors.New("Anthropic returned an incomplete tool use block")
			}
			if len(block.Input) == 0 {
				block.Input = json.RawMessage(`{}`)
			}
			toolCalls = append(toolCalls, agent.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: append(json.RawMessage(nil), block.Input...),
			})
		}
	}

	state, err := json.Marshal(response.Content)
	if err != nil {
		return agent.Message{}, fmt.Errorf("preserve Anthropic response state: %w", err)
	}
	rawStopReason := response.StopReason
	if len(toolCalls) > 0 {
		rawStopReason = "tool_use"
	}
	reportedUsage, err := usage(
		response.Usage.InputTokens,
		response.Usage.OutputTokens,
		response.Usage.CacheCreationInputTokens,
		response.Usage.CacheReadInputTokens,
	)
	if err != nil {
		return agent.Message{}, err
	}
	return agent.Message{
		Role:          agent.RoleAssistant,
		Text:          text.String(),
		ToolCalls:     toolCalls,
		StopReason:    mapStopReason(rawStopReason, len(toolCalls) > 0),
		RawStopReason: rawStopReason,
		Usage:         reportedUsage,
		ResponseID:    response.ID,
		Model:         response.Model,
		ProviderState: &agent.ProviderState{
			Provider: provider.Name(),
			Data:     state,
		},
	}, nil
}

type messagesRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    int                    `json:"max_tokens"`
	System       string                 `json:"system,omitempty"`
	Messages     []anthropicMessage     `json:"messages"`
	Tools        []anthropicTool        `json:"tools,omitempty"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
	Stream       bool                   `json:"stream,omitempty"`
}

type anthropicCacheControl struct {
	Type string         `json:"type"`
	TTL  agent.CacheTTL `json:"ttl,omitempty"`
}

type anthropicMessage struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type messagesResponse struct {
	ID         string            `json:"id"`
	Model      string            `json:"model"`
	Role       string            `json:"role"`
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

func (provider *Provider) messagesRequest(request agent.ModelRequest) (messagesRequest, error) {
	cachePlan, err := provider.validatedCachePlan(request)
	if err != nil {
		return messagesRequest{}, err
	}
	breakpoints := make(map[int]struct{})
	if cachePlan != nil && cachePlan.Mode == agent.CacheModeExplicit {
		for _, breakpoint := range cachePlan.Breakpoints {
			breakpoints[breakpoint.MessageIndex] = struct{}{}
		}
	}
	messages := make([]anthropicMessage, 0, len(request.Messages))
	for index := 0; index < len(request.Messages); index++ {
		message := request.Messages[index]
		switch message.Role {
		case agent.RoleUser:
			_, marked := breakpoints[index]
			block, err := json.Marshal(struct {
				Type         string                 `json:"type"`
				Text         string                 `json:"text"`
				CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
			}{
				Type:         "text",
				Text:         message.Text,
				CacheControl: anthropicExplicitCacheControl(cachePlan, marked),
			})
			if err != nil {
				return messagesRequest{}, err
			}
			messages = append(messages, anthropicMessage{Role: "user", Content: []json.RawMessage{block}})
		case agent.RoleAssistant:
			var blocks []json.RawMessage
			if rawState, ok := stateData(message, provider.Name()); ok {
				if err := json.Unmarshal(rawState, &blocks); err != nil {
					return messagesRequest{}, fmt.Errorf("decode Anthropic continuation state: %w", err)
				}
			} else {
				if message.Text != "" || len(message.ToolCalls) == 0 {
					block, err := json.Marshal(struct {
						Type string `json:"type"`
						Text string `json:"text"`
					}{Type: "text", Text: message.Text})
					if err != nil {
						return messagesRequest{}, err
					}
					blocks = append(blocks, block)
				}
				for _, call := range message.ToolCalls {
					if call.ID == "" || call.Name == "" {
						return messagesRequest{}, errors.New("assistant tool call ID and name are required")
					}
					arguments := call.Arguments
					if len(arguments) == 0 {
						arguments = json.RawMessage(`{}`)
					}
					block, err := json.Marshal(struct {
						Type  string          `json:"type"`
						ID    string          `json:"id"`
						Name  string          `json:"name"`
						Input json.RawMessage `json:"input"`
					}{Type: "tool_use", ID: call.ID, Name: call.Name, Input: arguments})
					if err != nil {
						return messagesRequest{}, err
					}
					blocks = append(blocks, block)
				}
			}
			messages = append(messages, anthropicMessage{Role: "assistant", Content: blocks})
		case agent.RoleTool:
			var blocks []json.RawMessage
			for index < len(request.Messages) && request.Messages[index].Role == agent.RoleTool {
				toolMessage := request.Messages[index]
				if toolMessage.ToolCallID == "" {
					return messagesRequest{}, errors.New("tool message call ID is required")
				}
				block, err := json.Marshal(struct {
					Type      string `json:"type"`
					ToolUseID string `json:"tool_use_id"`
					Content   string `json:"content"`
					IsError   bool   `json:"is_error,omitempty"`
				}{
					Type:      "tool_result",
					ToolUseID: toolMessage.ToolCallID,
					Content:   toolMessage.Text,
					IsError:   toolMessage.ToolIsError,
				})
				if err != nil {
					return messagesRequest{}, err
				}
				blocks = append(blocks, block)
				index++
			}
			index--
			messages = append(messages, anthropicMessage{Role: "user", Content: blocks})
		default:
			return messagesRequest{}, fmt.Errorf("unsupported message role %q", message.Role)
		}
	}

	tools := make([]anthropicTool, 0, len(request.Tools))
	for _, definition := range request.Tools {
		if definition.Name == "" {
			return messagesRequest{}, errors.New("tool name is required")
		}
		schema, err := toolSchema(definition)
		if err != nil {
			return messagesRequest{}, err
		}
		tools = append(tools, anthropicTool{
			Name:        definition.Name,
			Description: definition.Description,
			InputSchema: schema,
		})
	}

	payload := messagesRequest{
		Model:     provider.model,
		MaxTokens: provider.maxOutputTokens,
		System:    request.Instructions,
		Messages:  messages,
		Tools:     tools,
	}
	if cachePlan != nil && cachePlan.Mode == agent.CacheModeAutomatic {
		payload.CacheControl = &anthropicCacheControl{Type: "ephemeral", TTL: cachePlan.TTL}
	}
	return payload, nil
}

func (provider *Provider) validatedCachePlan(request agent.ModelRequest) (*agent.CachePlan, error) {
	plan := request.CachePlan
	if plan == nil || plan.Mode == agent.CacheModeDisabled {
		return nil, nil
	}
	capabilities := provider.CacheCapabilities()
	if plan.FamilyID == "" {
		return nil, errors.New("Anthropic Cache Plan family is required")
	}
	switch plan.Mode {
	case agent.CacheModeAutomatic:
		if !capabilities.SupportsAutomatic {
			return nil, errors.New("Anthropic automatic prompt cache is unsupported")
		}
	case agent.CacheModeExplicit:
		if !capabilities.SupportsExplicit || len(plan.Breakpoints) == 0 ||
			len(plan.Breakpoints) > capabilities.MaxWriteBreakpoints {
			return nil, errors.New("Anthropic explicit Cache Plan has an invalid breakpoint count")
		}
	default:
		return nil, fmt.Errorf("unsupported Anthropic Cache Plan mode %q", plan.Mode)
	}
	for _, breakpoint := range plan.Breakpoints {
		if breakpoint.MessageIndex < 0 || breakpoint.MessageIndex >= len(request.Messages) ||
			request.Messages[breakpoint.MessageIndex].Role != agent.RoleUser {
			return nil, errors.New("Anthropic Cache Plan breakpoint does not identify a user message")
		}
	}
	return plan, nil
}

func anthropicExplicitCacheControl(plan *agent.CachePlan, marked bool) *anthropicCacheControl {
	if plan == nil || plan.Mode != agent.CacheModeExplicit || !marked {
		return nil
	}
	return &anthropicCacheControl{Type: "ephemeral", TTL: plan.TTL}
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

func stateData(message agent.Message, providerName string) (json.RawMessage, bool) {
	if message.ProviderState == nil || message.ProviderState.Provider != providerName || len(message.ProviderState.Data) == 0 {
		return nil, false
	}
	return message.ProviderState.Data, true
}

func usage(
	uncachedInputTokens,
	outputTokens,
	cacheWriteInputTokens,
	cacheReadInputTokens int64,
) (*agent.Usage, error) {
	if uncachedInputTokens < 0 || outputTokens < 0 || cacheWriteInputTokens < 0 || cacheReadInputTokens < 0 {
		return nil, errors.New("Anthropic Usage token values must not be negative")
	}
	inputTokens, err := addUsageTokens(uncachedInputTokens, cacheWriteInputTokens)
	if err != nil {
		return nil, err
	}
	inputTokens, err = addUsageTokens(inputTokens, cacheReadInputTokens)
	if err != nil {
		return nil, err
	}
	if inputTokens == 0 && outputTokens == 0 {
		return nil, nil
	}
	totalTokens, err := addUsageTokens(inputTokens, outputTokens)
	if err != nil {
		return nil, err
	}
	return &agent.Usage{
		InputTokens:               inputTokens,
		OutputTokens:              outputTokens,
		TotalTokens:               totalTokens,
		InputTokenDetailsReported: true,
		UncachedInputTokens:       uncachedInputTokens,
		CacheReadInputTokens:      cacheReadInputTokens,
		CacheWriteInputTokens:     cacheWriteInputTokens,
	}, nil
}

func addUsageTokens(first, second int64) (int64, error) {
	if second > math.MaxInt64-first {
		return 0, errors.New("Anthropic Usage token value overflow")
	}
	return first + second, nil
}

func mapStopReason(raw string, hasToolCalls bool) agent.StopReason {
	if hasToolCalls {
		return agent.StopReasonToolUse
	}
	switch raw {
	case "end_turn", "stop_sequence":
		return agent.StopReasonEndTurn
	case "tool_use":
		return agent.StopReasonToolUse
	case "max_tokens", "model_context_window_exceeded":
		return agent.StopReasonMaxTokens
	case "refusal":
		return agent.StopReasonRefusal
	default:
		return agent.StopReasonUnknown
	}
}
