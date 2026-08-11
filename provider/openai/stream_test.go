package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/provider/openai"
)

func TestResponsesStreamEmitsDeltasAndCompletedMessage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || !body.Stream {
			t.Errorf("request stream = %v, error = %v", body.Stream, err)
		}
		if request.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q", request.Header.Get("Accept"))
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hel\"}\n\n")
		_, _ = fmt.Fprint(writer, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n")
		_, _ = fmt.Fprint(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"test-model\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":1,\"cache_write_tokens\":1}}}}\n\n")
	}))
	defer server.Close()

	provider, err := openai.New(openai.Config{API: openai.APIResponses, BaseURL: server.URL, Model: "test-model", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), agent.ModelRequest{Messages: []agent.Message{{Role: agent.RoleUser, Text: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	deltas, message := collectOpenAIStream(t, stream)
	if deltas != "hello" || message.Text != "hello" || message.ResponseID != "resp_1" {
		t.Fatalf("deltas/message = %q/%#v", deltas, message)
	}
	if message.Usage == nil || message.Usage.TotalTokens != 5 ||
		!message.Usage.InputTokenDetailsReported || message.Usage.UncachedInputTokens != 1 ||
		message.Usage.CacheReadInputTokens != 1 || message.Usage.CacheWriteInputTokens != 1 {
		t.Fatalf("usage = %#v", message.Usage)
	}
}

func TestChatStreamReassemblesToolCallAndUsage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "data: {\"id\":\"chat_1\",\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi \"}}]}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"id\":\"chat_1\",\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"upper\",\"arguments\":\"{\\\"text\\\":\"}}]}}]}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"id\":\"chat_1\",\"model\":\"test-model\",\"choices\":[{\"index\":0,\"finish_reason\":\"tool_calls\",\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"hello\\\"}\"}}]}}]}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":3,\"total_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":2,\"cache_write_tokens\":1}}}\n\n")
		_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	provider, err := openai.New(openai.Config{API: openai.APIChatCompletions, BaseURL: server.URL, Model: "test-model", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), agent.ModelRequest{Messages: []agent.Message{{Role: agent.RoleUser, Text: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	deltas, message := collectOpenAIStream(t, stream)
	if deltas != "Hi " || message.Text != "Hi " || len(message.ToolCalls) != 1 {
		t.Fatalf("deltas/message = %q/%#v", deltas, message)
	}
	call := message.ToolCalls[0]
	if call.ID != "call_1" || call.Name != "upper" || string(call.Arguments) != `{"text":"hello"}` {
		t.Fatalf("tool call = %#v", call)
	}
	if message.Usage == nil || message.Usage.TotalTokens != 7 ||
		!message.Usage.InputTokenDetailsReported || message.Usage.UncachedInputTokens != 1 ||
		message.Usage.CacheReadInputTokens != 2 || message.Usage.CacheWriteInputTokens != 1 {
		t.Fatalf("usage = %#v", message.Usage)
	}
}

func collectOpenAIStream(t *testing.T, stream agent.ModelStream) (string, agent.Message) {
	t.Helper()
	defer stream.Close()
	var text strings.Builder
	var message agent.Message
	for {
		event, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch event.Type {
		case agent.ModelStreamTextDelta:
			text.WriteString(event.TextDelta)
		case agent.ModelStreamMessageComplete:
			if event.Message == nil {
				t.Fatal("completed event has nil Message")
			}
			message = *event.Message
		}
	}
	return text.String(), message
}
