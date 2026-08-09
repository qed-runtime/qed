package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/provider/internal/httpjson"
)

func (provider *Provider) streamResponses(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	payload, err := provider.responsesRequest(request)
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
	accumulator := &responsesStreamAccumulator{provider: provider, stream: stream}
	return &agent.ModelStreamFunc{
		NextFunc:  accumulator.next,
		CloseFunc: stream.Close,
	}, nil
}

type responsesStreamAccumulator struct {
	provider  *Provider
	stream    *httpjson.SSEStream
	completed bool
}

func (accumulator *responsesStreamAccumulator) next() (agent.ModelStreamEvent, error) {
	if accumulator.completed {
		return agent.ModelStreamEvent{}, io.EOF
	}
	for {
		event, err := accumulator.stream.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return agent.ModelStreamEvent{}, errors.New("OpenAI Responses stream ended without a terminal response")
			}
			return agent.ModelStreamEvent{}, err
		}
		if event.Event == "http.response" {
			var response responsesResponse
			if err := json.Unmarshal(event.Data, &response); err != nil {
				return agent.ModelStreamEvent{}, fmt.Errorf("decode OpenAI Responses fallback response: %w", err)
			}
			message, err := accumulator.provider.messageFromResponsesResponse(response)
			if err != nil {
				return agent.ModelStreamEvent{}, err
			}
			accumulator.completed = true
			return agent.ModelStreamEvent{Type: agent.ModelStreamMessageComplete, Message: &message}, nil
		}
		var envelope struct {
			Type     string            `json:"type"`
			Delta    string            `json:"delta"`
			Response responsesResponse `json:"response"`
			Code     string            `json:"code"`
			Message  string            `json:"message"`
		}
		if err := json.Unmarshal(event.Data, &envelope); err != nil {
			return agent.ModelStreamEvent{}, fmt.Errorf("decode OpenAI Responses stream event: %w", err)
		}
		switch envelope.Type {
		case "response.output_text.delta", "response.refusal.delta":
			if envelope.Delta == "" {
				continue
			}
			return agent.ModelStreamEvent{Type: agent.ModelStreamTextDelta, TextDelta: envelope.Delta}, nil
		case "response.completed", "response.incomplete":
			message, err := accumulator.provider.messageFromResponsesResponse(envelope.Response)
			if err != nil {
				return agent.ModelStreamEvent{}, err
			}
			accumulator.completed = true
			return agent.ModelStreamEvent{Type: agent.ModelStreamMessageComplete, Message: &message}, nil
		case "response.failed", "response.cancelled":
			if _, err := accumulator.provider.messageFromResponsesResponse(envelope.Response); err != nil {
				return agent.ModelStreamEvent{}, err
			}
			return agent.ModelStreamEvent{}, fmt.Errorf("OpenAI response ended with status %q", envelope.Response.Status)
		case "error":
			return agent.ModelStreamEvent{}, fmt.Errorf("OpenAI Responses stream failed %s: %s", envelope.Code, envelope.Message)
		default:
			// The Responses API adds semantic event types over time. Unknown events
			// do not affect the final response accumulator.
			continue
		}
	}
}
