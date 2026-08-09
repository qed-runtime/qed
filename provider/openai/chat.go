package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/provider/internal/httpjson"
)

type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []chatMessage `json:"messages"`
	Tools               []chatTool    `json:"tools,omitempty"`
	MaxCompletionTokens int           `json:"max_completion_tokens,omitempty"`
	Stream              bool          `json:"stream,omitempty"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatTool struct {
	Type     string           `json:"type"`
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatToolCall struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function chatToolCallFunction `json:"function"`
}

type chatToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role      string         `json:"role"`
			Content   textContent    `json:"content"`
			Refusal   string         `json:"refusal"`
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

func (provider *Provider) completeChat(ctx context.Context, request agent.ModelRequest) (agent.Message, error) {
	payload, err := provider.chatRequest(request)
	if err != nil {
		return agent.Message{}, err
	}
	headers, err := provider.headers(ctx)
	if err != nil {
		return agent.Message{}, err
	}

	var response chatResponse
	if err := httpjson.Post(ctx, provider.client, provider.endpoint, headers, payload, &response); err != nil {
		return agent.Message{}, err
	}
	return messageFromChatResponse(response)
}

func messageFromChatResponse(response chatResponse) (agent.Message, error) {
	if len(response.Choices) == 0 {
		return agent.Message{}, errors.New("OpenAI Chat Completions response has no choices")
	}

	choice := response.Choices[0]
	for _, candidate := range response.Choices {
		if candidate.Index == 0 {
			choice = candidate
			break
		}
	}

	toolCalls := make([]agent.ToolCall, 0, len(choice.Message.ToolCalls))
	for _, call := range choice.Message.ToolCalls {
		if call.Type != "" && call.Type != "function" {
			return agent.Message{}, fmt.Errorf("OpenAI Chat Completions returned unsupported tool call type %q", call.Type)
		}
		if call.ID == "" || call.Function.Name == "" {
			return agent.Message{}, errors.New("OpenAI Chat Completions returned an incomplete tool call")
		}
		arguments := json.RawMessage(call.Function.Arguments)
		if len(arguments) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		toolCalls = append(toolCalls, agent.ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: arguments,
		})
	}

	text := string(choice.Message.Content)
	if text == "" {
		text = choice.Message.Refusal
	}
	return agent.Message{
		Role:          agent.RoleAssistant,
		Text:          text,
		ToolCalls:     toolCalls,
		StopReason:    mapStopReason(choice.FinishReason, len(toolCalls) > 0),
		RawStopReason: choice.FinishReason,
		Usage: usage(
			response.Usage.PromptTokens,
			response.Usage.CompletionTokens,
			response.Usage.TotalTokens,
		),
		ResponseID: response.ID,
		Model:      response.Model,
	}, nil
}

func (provider *Provider) streamChat(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	payload, err := provider.chatRequest(request)
	if err != nil {
		return nil, err
	}
	payload.Stream = true
	payload.StreamOptions = &struct {
		IncludeUsage bool `json:"include_usage"`
	}{IncludeUsage: true}
	headers, err := provider.headers(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := httpjson.PostSSE(ctx, provider.client, provider.endpoint, headers, payload)
	if err != nil {
		return nil, err
	}
	accumulator := &chatStreamAccumulator{stream: stream, calls: make(map[int]*chatStreamToolCall)}
	return &agent.ModelStreamFunc{
		NextFunc:  accumulator.next,
		CloseFunc: stream.Close,
	}, nil
}

type chatStreamToolCall struct {
	id        string
	typeName  string
	name      strings.Builder
	arguments strings.Builder
}

type chatStreamAccumulator struct {
	stream       *httpjson.SSEStream
	id           string
	model        string
	text         strings.Builder
	refusal      strings.Builder
	finishReason string
	usage        struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	}
	calls     map[int]*chatStreamToolCall
	completed bool
}

func (accumulator *chatStreamAccumulator) next() (agent.ModelStreamEvent, error) {
	if accumulator.completed {
		return agent.ModelStreamEvent{}, io.EOF
	}
	for {
		event, err := accumulator.stream.Next()
		if err != nil {
			if errors.Is(err, io.EOF) && accumulator.finishReason != "" {
				return accumulator.complete()
			}
			return agent.ModelStreamEvent{}, err
		}
		if string(event.Data) == "[DONE]" {
			if accumulator.finishReason == "" {
				return agent.ModelStreamEvent{}, errors.New("OpenAI Chat Completions stream ended without a finish reason")
			}
			return accumulator.complete()
		}
		if event.Event == "http.response" {
			var response chatResponse
			if err := json.Unmarshal(event.Data, &response); err != nil {
				return agent.ModelStreamEvent{}, fmt.Errorf("decode OpenAI Chat Completions fallback response: %w", err)
			}
			message, err := messageFromChatResponse(response)
			if err != nil {
				return agent.ModelStreamEvent{}, err
			}
			accumulator.completed = true
			return agent.ModelStreamEvent{Type: agent.ModelStreamMessageComplete, Message: &message}, nil
		}

		var chunk struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Error *struct {
				Code    json.RawMessage `json:"code"`
				Message string          `json:"message"`
			} `json:"error"`
			Choices []struct {
				Index        int    `json:"index"`
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content   string `json:"content"`
					Refusal   string `json:"refusal"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
				TotalTokens      int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(event.Data, &chunk); err != nil {
			return agent.ModelStreamEvent{}, fmt.Errorf("decode OpenAI Chat Completions stream event: %w", err)
		}
		if chunk.Error != nil {
			return agent.ModelStreamEvent{}, fmt.Errorf("OpenAI Chat Completions stream failed: %s", chunk.Error.Message)
		}
		if chunk.ID != "" {
			accumulator.id = chunk.ID
		}
		if chunk.Model != "" {
			accumulator.model = chunk.Model
		}
		if chunk.Usage.PromptTokens != 0 || chunk.Usage.CompletionTokens != 0 || chunk.Usage.TotalTokens != 0 {
			accumulator.usage = chunk.Usage
		}

		var delta strings.Builder
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			if choice.FinishReason != "" {
				accumulator.finishReason = choice.FinishReason
			}
			accumulator.text.WriteString(choice.Delta.Content)
			accumulator.refusal.WriteString(choice.Delta.Refusal)
			delta.WriteString(choice.Delta.Content)
			delta.WriteString(choice.Delta.Refusal)
			for _, fragment := range choice.Delta.ToolCalls {
				call := accumulator.calls[fragment.Index]
				if call == nil {
					call = &chatStreamToolCall{}
					accumulator.calls[fragment.Index] = call
				}
				if fragment.ID != "" {
					call.id = fragment.ID
				}
				if fragment.Type != "" {
					call.typeName = fragment.Type
				}
				call.name.WriteString(fragment.Function.Name)
				call.arguments.WriteString(fragment.Function.Arguments)
			}
		}
		if delta.Len() > 0 {
			return agent.ModelStreamEvent{Type: agent.ModelStreamTextDelta, TextDelta: delta.String()}, nil
		}
	}
}

func (accumulator *chatStreamAccumulator) complete() (agent.ModelStreamEvent, error) {
	indices := make([]int, 0, len(accumulator.calls))
	for index := range accumulator.calls {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	toolCalls := make([]chatToolCall, 0, len(indices))
	for _, index := range indices {
		call := accumulator.calls[index]
		toolCalls = append(toolCalls, chatToolCall{
			ID:   call.id,
			Type: call.typeName,
			Function: chatToolCallFunction{
				Name:      call.name.String(),
				Arguments: call.arguments.String(),
			},
		})
	}
	response := chatResponse{ID: accumulator.id, Model: accumulator.model}
	response.Usage = accumulator.usage
	response.Choices = append(response.Choices, struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role      string         `json:"role"`
			Content   textContent    `json:"content"`
			Refusal   string         `json:"refusal"`
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"message"`
	}{Index: 0, FinishReason: accumulator.finishReason})
	response.Choices[0].Message.Role = "assistant"
	response.Choices[0].Message.Content = textContent(accumulator.text.String())
	response.Choices[0].Message.Refusal = accumulator.refusal.String()
	response.Choices[0].Message.ToolCalls = toolCalls
	message, err := messageFromChatResponse(response)
	if err != nil {
		return agent.ModelStreamEvent{}, err
	}
	accumulator.completed = true
	return agent.ModelStreamEvent{Type: agent.ModelStreamMessageComplete, Message: &message}, nil
}

func (provider *Provider) chatRequest(request agent.ModelRequest) (chatRequest, error) {
	messages := make([]chatMessage, 0, len(request.Messages)+1)
	if request.Instructions != "" {
		instructions := request.Instructions
		messages = append(messages, chatMessage{Role: "system", Content: &instructions})
	}

	for _, message := range request.Messages {
		switch message.Role {
		case agent.RoleUser:
			text := message.Text
			messages = append(messages, chatMessage{Role: "user", Content: &text})
		case agent.RoleAssistant:
			converted := chatMessage{Role: "assistant"}
			if message.Text != "" || len(message.ToolCalls) == 0 {
				text := message.Text
				converted.Content = &text
			}
			for _, call := range message.ToolCalls {
				if call.ID == "" || call.Name == "" {
					return chatRequest{}, errors.New("assistant tool call ID and name are required")
				}
				converted.ToolCalls = append(converted.ToolCalls, chatToolCall{
					ID:   call.ID,
					Type: "function",
					Function: chatToolCallFunction{
						Name:      call.Name,
						Arguments: toolArguments(call),
					},
				})
			}
			messages = append(messages, converted)
		case agent.RoleTool:
			if message.ToolCallID == "" {
				return chatRequest{}, errors.New("tool message call ID is required")
			}
			text := message.Text
			messages = append(messages, chatMessage{
				Role:       "tool",
				Content:    &text,
				ToolCallID: message.ToolCallID,
			})
		default:
			return chatRequest{}, fmt.Errorf("unsupported message role %q", message.Role)
		}
	}

	tools := make([]chatTool, 0, len(request.Tools))
	for _, definition := range request.Tools {
		if definition.Name == "" {
			return chatRequest{}, errors.New("tool name is required")
		}
		schema, err := toolSchema(definition)
		if err != nil {
			return chatRequest{}, err
		}
		tools = append(tools, chatTool{
			Type: "function",
			Function: chatToolFunction{
				Name:        definition.Name,
				Description: definition.Description,
				Parameters:  schema,
			},
		})
	}

	return chatRequest{
		Model:               provider.model,
		Messages:            messages,
		Tools:               tools,
		MaxCompletionTokens: provider.maxOutputTokens,
	}, nil
}
