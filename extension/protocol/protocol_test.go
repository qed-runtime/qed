package protocol_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/extension/protocol"
)

func TestFrameRoundTrip(t *testing.T) {
	t.Parallel()

	params, err := protocol.Marshal(protocol.HandshakeRequest{ProtocolVersion: protocol.Version})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	want := protocol.Envelope{
		Version: protocol.Version,
		ID:      "request-1",
		Method:  protocol.MethodHandshake,
		Params:  params,
	}
	var stream bytes.Buffer
	if err := protocol.NewWriter(&stream).Write(want); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	got, err := protocol.NewReader(&stream).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.Version != want.Version || got.ID != want.ID || got.Method != want.Method ||
		!bytes.Equal(got.Params, want.Params) {
		t.Fatalf("Read() = %#v, want %#v", got, want)
	}
}

func TestReaderRejectsOversizedFrameBeforeReadingPayload(t *testing.T) {
	t.Parallel()

	var header [4]byte
	binary.BigEndian.PutUint32(header[:], protocol.MaxFrameBytes+1)
	_, err := protocol.NewReader(bytes.NewReader(header[:])).Read()
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Read() error = %v, want size error", err)
	}
}

func TestUnmarshalRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	var request protocol.HandshakeRequest
	err := protocol.Unmarshal(json.RawMessage(`{"protocol_version":2,"unexpected":true}`), &request)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Unmarshal() error = %v, want unknown field", err)
	}
}

func TestToolResultContextOperationRoundTrip(t *testing.T) {
	t.Parallel()

	encoded, err := protocol.Marshal(protocol.ToolResult{
		CallID: "call-1",
		Name:   "edit",
		ContextOperation: &protocol.ContextOperation{
			Kind: "mutation",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var result protocol.ToolResult
	if err := protocol.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if result.ContextOperation == nil || result.ContextOperation.Kind != "mutation" {
		t.Fatalf("Tool result = %#v", result)
	}
}
