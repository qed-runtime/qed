package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/qed-runtime/qed/agent"
	providerbase "github.com/qed-runtime/qed/provider"
	"github.com/qed-runtime/qed/provider/internal/httpjson"
)

// Stream sends one streaming Messages API request
func (provider *Provider) Stream(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	if ctx == nil {
		return nil, errors.New("Anthropic context must not be nil")
	}
	payload, err := provider.messagesRequest(request)
	if err != nil {
		return nil, err
	}
	payload.Stream = true
	headers, err := provider.headers(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := httpjson.PostSSE(ctx, provider.client, provider.endpoint, headers, payload)
	if err != nil {
		return nil, err
	}
	accumulator := &messagesStreamAccumulator{
		provider: provider,
		stream:   stream,
		blocks:   make(map[int]*streamContentBlock),
	}
	return &agent.ModelStreamFunc{
		NextFunc:  accumulator.next,
		CloseFunc: stream.Close,
	}, nil
}

func (provider *Provider) headers(ctx context.Context) (map[string]string, error) {
	credential := provider.apiKey
	if provider.credentialSource != nil {
		resolved, err := provider.credentialSource.Credential(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve Anthropic credential: %w", err)
		}
		if strings.TrimSpace(resolved) == "" {
			return nil, errors.New("Anthropic credential source returned an empty credential")
		}
		credential = resolved
	}
	headers := map[string]string{"anthropic-version": provider.apiVersion}
	if credential != "" {
		headers["x-api-key"] = credential
	}
	return headers, nil
}

type messagesStreamAccumulator struct {
	provider   *Provider
	stream     *httpjson.SSEStream
	response   messagesResponse
	blocks     map[int]*streamContentBlock
	completed  bool
	started    bool
	stopReason string
}

type streamContentBlock struct {
	typeName      string
	rawStart      json.RawMessage
	text          strings.Builder
	thinking      strings.Builder
	signature     strings.Builder
	id            string
	name          string
	initialInput  json.RawMessage
	partialInput  strings.Builder
	hasInputDelta bool
	citations     []json.RawMessage
	stopped       bool
}

func (accumulator *messagesStreamAccumulator) next() (agent.ModelStreamEvent, error) {
	if accumulator.completed {
		return agent.ModelStreamEvent{}, io.EOF
	}
	for {
		event, err := accumulator.stream.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return agent.ModelStreamEvent{}, errors.New("Anthropic Messages stream ended without message_stop")
			}
			return agent.ModelStreamEvent{}, err
		}
		if event.Event == "http.response" {
			var response messagesResponse
			if err := json.Unmarshal(event.Data, &response); err != nil {
				return agent.ModelStreamEvent{}, fmt.Errorf("decode Anthropic Messages fallback response: %w", err)
			}
			message, err := accumulator.provider.messageFromMessagesResponse(response)
			if err != nil {
				return agent.ModelStreamEvent{}, err
			}
			accumulator.completed = true
			return agent.ModelStreamEvent{Type: agent.ModelStreamMessageComplete, Message: &message}, nil
		}
		var envelope struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message *struct {
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
			} `json:"message"`
			ContentBlock json.RawMessage `json:"content_block"`
			Delta        struct {
				Type        string          `json:"type"`
				Text        string          `json:"text"`
				PartialJSON string          `json:"partial_json"`
				Thinking    string          `json:"thinking"`
				Signature   string          `json:"signature"`
				Citation    json.RawMessage `json:"citation"`
				StopReason  string          `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(event.Data, &envelope); err != nil {
			return agent.ModelStreamEvent{}, fmt.Errorf("decode Anthropic Messages stream event: %w", err)
		}
		if envelope.Type == "" {
			envelope.Type = event.Event
		}
		switch envelope.Type {
		case "message_start":
			if envelope.Message == nil {
				return agent.ModelStreamEvent{}, errors.New("Anthropic message_start is missing message")
			}
			if accumulator.started {
				return agent.ModelStreamEvent{}, errors.New("Anthropic Messages stream started more than once")
			}
			accumulator.started = true
			accumulator.response.ID = envelope.Message.ID
			accumulator.response.Model = envelope.Message.Model
			accumulator.response.Role = envelope.Message.Role
			accumulator.response.Usage = envelope.Message.Usage
		case "content_block_start":
			if _, exists := accumulator.blocks[envelope.Index]; exists {
				return agent.ModelStreamEvent{}, fmt.Errorf("Anthropic content block %d started more than once", envelope.Index)
			}
			block, initialText, err := decodeStreamContentBlock(envelope.ContentBlock)
			if err != nil {
				return agent.ModelStreamEvent{}, err
			}
			accumulator.blocks[envelope.Index] = block
			if initialText != "" {
				return agent.ModelStreamEvent{Type: agent.ModelStreamTextDelta, TextDelta: initialText}, nil
			}
		case "content_block_delta":
			block := accumulator.blocks[envelope.Index]
			if block == nil || block.stopped {
				return agent.ModelStreamEvent{}, fmt.Errorf("Anthropic content block %d received a delta outside its lifetime", envelope.Index)
			}
			switch envelope.Delta.Type {
			case "text_delta":
				block.text.WriteString(envelope.Delta.Text)
				if envelope.Delta.Text != "" {
					return agent.ModelStreamEvent{Type: agent.ModelStreamTextDelta, TextDelta: envelope.Delta.Text}, nil
				}
			case "input_json_delta":
				block.hasInputDelta = true
				block.partialInput.WriteString(envelope.Delta.PartialJSON)
			case "thinking_delta":
				block.thinking.WriteString(envelope.Delta.Thinking)
			case "signature_delta":
				block.signature.WriteString(envelope.Delta.Signature)
			case "citations_delta":
				if len(envelope.Delta.Citation) > 0 {
					block.citations = append(block.citations, append(json.RawMessage(nil), envelope.Delta.Citation...))
				}
			default:
				// Anthropic may add new delta variants. Unknown variants are ignored
				// while the terminal message remains usable.
			}
		case "content_block_stop":
			block := accumulator.blocks[envelope.Index]
			if block == nil || block.stopped {
				return agent.ModelStreamEvent{}, fmt.Errorf("Anthropic content block %d stopped outside its lifetime", envelope.Index)
			}
			block.stopped = true
		case "message_delta":
			if envelope.Delta.StopReason != "" {
				accumulator.stopReason = envelope.Delta.StopReason
			}
			accumulator.response.Usage.OutputTokens = envelope.Usage.OutputTokens
		case "message_stop":
			return accumulator.complete()
		case "error":
			if envelope.Error == nil {
				return agent.ModelStreamEvent{}, errors.New("Anthropic Messages stream failed")
			}
			return agent.ModelStreamEvent{}, fmt.Errorf("Anthropic Messages stream failed: %w", &providerbase.APIError{
				Type:    envelope.Error.Type,
				Message: envelope.Error.Message,
			})
		case "ping":
			continue
		default:
			// The Anthropic versioning policy permits new event types. They are
			// ignored unless they change a content block understood above.
			continue
		}
	}
}

func decodeStreamContentBlock(raw json.RawMessage) (*streamContentBlock, string, error) {
	if len(raw) == 0 {
		return nil, "", errors.New("Anthropic content_block_start is missing content_block")
	}
	var start struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Signature string          `json:"signature"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &start); err != nil {
		return nil, "", fmt.Errorf("decode Anthropic content block start: %w", err)
	}
	if start.Type == "" {
		return nil, "", errors.New("Anthropic content block type is required")
	}
	block := &streamContentBlock{
		typeName:     start.Type,
		rawStart:     append(json.RawMessage(nil), raw...),
		id:           start.ID,
		name:         start.Name,
		initialInput: append(json.RawMessage(nil), start.Input...),
	}
	block.text.WriteString(start.Text)
	block.thinking.WriteString(start.Thinking)
	block.signature.WriteString(start.Signature)
	return block, start.Text, nil
}

func (accumulator *messagesStreamAccumulator) complete() (agent.ModelStreamEvent, error) {
	if !accumulator.started {
		return agent.ModelStreamEvent{}, errors.New("Anthropic Messages stream stopped before message_start")
	}
	indices := make([]int, 0, len(accumulator.blocks))
	for index, block := range accumulator.blocks {
		if !block.stopped {
			return agent.ModelStreamEvent{}, fmt.Errorf("Anthropic content block %d did not stop", index)
		}
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for expected, index := range indices {
		if index != expected {
			return agent.ModelStreamEvent{}, errors.New("Anthropic content block indices are not contiguous")
		}
		block, err := accumulator.blocks[index].marshal()
		if err != nil {
			return agent.ModelStreamEvent{}, err
		}
		accumulator.response.Content = append(accumulator.response.Content, block)
	}
	accumulator.response.StopReason = accumulator.stopReason
	message, err := accumulator.provider.messageFromMessagesResponse(accumulator.response)
	if err != nil {
		return agent.ModelStreamEvent{}, err
	}
	accumulator.completed = true
	return agent.ModelStreamEvent{Type: agent.ModelStreamMessageComplete, Message: &message}, nil
}

func (block *streamContentBlock) marshal() (json.RawMessage, error) {
	switch block.typeName {
	case "text":
		value := struct {
			Type      string            `json:"type"`
			Text      string            `json:"text"`
			Citations []json.RawMessage `json:"citations,omitempty"`
		}{Type: "text", Text: block.text.String(), Citations: block.citations}
		return json.Marshal(value)
	case "thinking":
		return json.Marshal(struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{Type: "thinking", Thinking: block.thinking.String(), Signature: block.signature.String()})
	case "tool_use":
		input := block.initialInput
		if block.hasInputDelta {
			input = json.RawMessage(block.partialInput.String())
		}
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		if !json.Valid(input) {
			return nil, errors.New("Anthropic tool input stream did not produce valid JSON")
		}
		return json.Marshal(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{Type: "tool_use", ID: block.id, Name: block.name, Input: input})
	default:
		return append(json.RawMessage(nil), block.rawStart...), nil
	}
}
