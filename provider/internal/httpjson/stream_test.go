package httpjson

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSSEDecoder(t *testing.T) {
	t.Parallel()

	decoder := newSSEDecoder(strings.NewReader(": keepalive\r\nevent: message\r\ndata: first\r\ndata: second\r\n\r\ndata: final\n\n"), 1024)
	first, err := decoder.Next()
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if first.Event != "message" || string(first.Data) != "first\nsecond" {
		t.Fatalf("first event = %#v", first)
	}
	second, err := decoder.Next()
	if err != nil {
		t.Fatalf("second Next: %v", err)
	}
	if second.Event != "" || string(second.Data) != "final" {
		t.Fatalf("second event = %#v", second)
	}
	if _, err := decoder.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("final Next error = %v, want EOF", err)
	}
}

func TestSSEDecoderRejectsLargeEvent(t *testing.T) {
	t.Parallel()

	decoder := newSSEDecoder(strings.NewReader("data: 123456789\n\n"), 8)
	if _, err := decoder.Next(); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("Next error = %v", err)
	}
}
