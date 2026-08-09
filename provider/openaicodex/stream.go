package openaicodex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/provider/internal/httpjson"
)

type responsesStreamAccumulator struct {
	provider        *Provider
	stream          *httpjson.SSEStream
	completed       bool
	streamedText    strings.Builder
	completedText   strings.Builder
	doneOutputItems map[int]json.RawMessage
}

type responsesStreamEnvelope struct {
	Type        string            `json:"type"`
	Delta       string            `json:"delta"`
	Text        string            `json:"text"`
	Refusal     string            `json:"refusal"`
	OutputIndex *int              `json:"output_index"`
	Item        json.RawMessage   `json:"item"`
	Response    responsesResponse `json:"response"`
	Code        string            `json:"code"`
	Message     string            `json:"message"`
	Error       *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (accumulator *responsesStreamAccumulator) next() (agent.ModelStreamEvent, error) {
	if accumulator.completed {
		return agent.ModelStreamEvent{}, io.EOF
	}
	for {
		event, err := accumulator.stream.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return agent.ModelStreamEvent{}, errors.New("OpenAI Codex Responses stream ended without a terminal response")
			}
			return agent.ModelStreamEvent{}, err
		}
		if event.Event == "http.response" {
			var response responsesResponse
			if err := json.Unmarshal(event.Data, &response); err != nil {
				return agent.ModelStreamEvent{}, fmt.Errorf("decode OpenAI Codex Responses fallback response: %w", err)
			}
			message, err := accumulator.completedMessage(response)
			if err != nil {
				return agent.ModelStreamEvent{}, err
			}
			accumulator.completed = true
			return agent.ModelStreamEvent{Type: agent.ModelStreamMessageComplete, Message: &message}, nil
		}
		var envelope responsesStreamEnvelope
		if err := json.Unmarshal(event.Data, &envelope); err != nil {
			return agent.ModelStreamEvent{}, fmt.Errorf("decode OpenAI Codex Responses stream event: %w", err)
		}
		switch envelope.Type {
		case "response.output_text.delta", "response.refusal.delta":
			if envelope.Delta == "" {
				continue
			}
			accumulator.streamedText.WriteString(envelope.Delta)
			return agent.ModelStreamEvent{Type: agent.ModelStreamTextDelta, TextDelta: envelope.Delta}, nil
		case "response.output_text.done":
			accumulator.completedText.WriteString(envelope.Text)
			continue
		case "response.refusal.done":
			accumulator.completedText.WriteString(envelope.Refusal)
			continue
		case "response.output_item.done":
			if err := accumulator.captureDoneOutputItem(envelope.OutputIndex, envelope.Item); err != nil {
				return agent.ModelStreamEvent{}, err
			}
			continue
		case "response.done", "response.completed", "response.incomplete":
			message, err := accumulator.completedMessage(envelope.Response)
			if err != nil {
				return agent.ModelStreamEvent{}, err
			}
			accumulator.completed = true
			return agent.ModelStreamEvent{Type: agent.ModelStreamMessageComplete, Message: &message}, nil
		case "response.failed", "response.cancelled":
			if _, err := accumulator.provider.messageFromResponsesResponse(envelope.Response); err != nil {
				return agent.ModelStreamEvent{}, err
			}
			return agent.ModelStreamEvent{}, fmt.Errorf("OpenAI Codex response ended with status %q", envelope.Response.Status)
		case "error":
			if envelope.Error != nil {
				envelope.Code = envelope.Error.Code
				envelope.Message = envelope.Error.Message
			}
			return agent.ModelStreamEvent{}, fmt.Errorf("OpenAI Codex Responses stream failed %s: %s", envelope.Code, envelope.Message)
		default:
			continue
		}
	}
}

func (accumulator *responsesStreamAccumulator) captureDoneOutputItem(
	outputIndex *int,
	item json.RawMessage,
) error {
	if outputIndex == nil || *outputIndex < 0 {
		return errors.New("OpenAI Codex Responses output item has an invalid output index")
	}
	if len(item) == 0 || string(item) == "null" || !json.Valid(item) {
		return errors.New("OpenAI Codex Responses output item is invalid")
	}
	if accumulator.doneOutputItems == nil {
		accumulator.doneOutputItems = make(map[int]json.RawMessage)
	}
	accumulator.doneOutputItems[*outputIndex] = append(json.RawMessage(nil), item...)
	return nil
}

func (accumulator *responsesStreamAccumulator) completedMessage(
	response responsesResponse,
) (agent.Message, error) {
	response.Output = accumulator.mergeDoneOutputItems(response.Output)
	message, err := accumulator.provider.messageFromResponsesResponse(response)
	if err != nil {
		return agent.Message{}, err
	}
	if message.Text != "" {
		return message, nil
	}
	fallbackText := accumulator.completedText.String()
	if fallbackText == "" {
		fallbackText = accumulator.streamedText.String()
	}
	if fallbackText == "" {
		return message, nil
	}
	syntheticMessage, err := messageInput("assistant", "output_text", fallbackText)
	if err != nil {
		return agent.Message{}, fmt.Errorf("reconstruct OpenAI Codex streamed text: %w", err)
	}
	response.Output = append(response.Output, syntheticMessage)
	return accumulator.provider.messageFromResponsesResponse(response)
}

func (accumulator *responsesStreamAccumulator) mergeDoneOutputItems(
	terminalItems []json.RawMessage,
) []json.RawMessage {
	if len(accumulator.doneOutputItems) == 0 {
		return terminalItems
	}
	itemsByIndex := make(map[int]json.RawMessage, len(terminalItems)+len(accumulator.doneOutputItems))
	for index, item := range terminalItems {
		itemsByIndex[index] = item
	}
	for index, item := range accumulator.doneOutputItems {
		itemsByIndex[index] = item
	}
	indexes := make([]int, 0, len(itemsByIndex))
	for index := range itemsByIndex {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	items := make([]json.RawMessage, 0, len(indexes))
	for _, index := range indexes {
		items = append(items, append(json.RawMessage(nil), itemsByIndex[index]...))
	}
	return items
}
