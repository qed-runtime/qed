package anthropic_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/provider/anthropic"
)

func TestMessagesStreamReassemblesTextToolAndThinkingState(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"model\":\"test-model\",\"content\":[],\"usage\":{\"input_tokens\":5,\"cache_read_input_tokens\":2,\"output_tokens\":1}}}",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"opaque\"}}",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig_1\"}}",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_1\",\"name\":\"upper\",\"input\":{}}}",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"text\\\":\"}}",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"hello\\\"}\"}}",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":6}}",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}",
		}
		for _, event := range events {
			_, _ = fmt.Fprintf(writer, "%s\n\n", event)
		}
	}))
	defer server.Close()

	provider, err := anthropic.New(anthropic.Config{BaseURL: server.URL, Model: "test-model", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), agent.ModelRequest{Messages: []agent.Message{{Role: agent.RoleUser, Text: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var deltas strings.Builder
	var message agent.Message
	for {
		event, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if event.Type == agent.ModelStreamTextDelta {
			deltas.WriteString(event.TextDelta)
		}
		if event.Type == agent.ModelStreamMessageComplete && event.Message != nil {
			message = *event.Message
		}
	}
	if deltas.String() != "hello" || message.Text != "hello" || len(message.ToolCalls) != 1 {
		t.Fatalf("deltas/message = %q/%#v", deltas.String(), message)
	}
	if string(message.ToolCalls[0].Arguments) != `{"text":"hello"}` {
		t.Fatalf("Tool arguments = %s", message.ToolCalls[0].Arguments)
	}
	if message.ProviderState == nil || !strings.Contains(string(message.ProviderState.Data), "sig_1") {
		t.Fatalf("ProviderState = %#v", message.ProviderState)
	}
	if message.Usage == nil || message.Usage.InputTokens != 7 || message.Usage.OutputTokens != 6 {
		t.Fatalf("Usage = %#v", message.Usage)
	}
}
