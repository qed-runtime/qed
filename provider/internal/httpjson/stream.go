package httpjson

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	providerbase "github.com/qed-runtime/qed/provider"
)

const maximumSSEEventBytes = 8 << 20

// SSEEvent contains one decoded server-sent event
type SSEEvent struct {
	Event string
	Data  []byte
}

// SSEStream owns one streaming HTTP response body
//
// Next must be called by at most one goroutine. Close may be called concurrently
// with Next and interrupts a blocked response body read.
type SSEStream struct {
	body      io.ReadCloser
	decoder   *sseDecoder
	closeOnce sync.Once
	closed    atomic.Bool
	closeErr  error
	rawJSON   bool
	jsonRead  atomic.Bool
}

// PostSSE sends a JSON request and returns its server-sent event response
func PostSSE(
	ctx context.Context,
	client providerbase.HTTPClient,
	endpoint string,
	headers map[string]string,
	requestValue any,
) (*SSEStream, error) {
	payload, err := json.Marshal(requestValue)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("User-Agent", "qed-runtime")
	for name, value := range headers {
		if value != "" {
			request.Header.Set(name, value)
		}
	}

	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		return nil, decodeHTTPError(response)
	}

	return &SSEStream{
		body:    response.Body,
		decoder: newSSEDecoder(response.Body, maximumSSEEventBytes),
		rawJSON: strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "application/json"),
	}, nil
}

// Next returns the next server-sent event
func (stream *SSEStream) Next() (SSEEvent, error) {
	if stream == nil || stream.closed.Load() {
		return SSEEvent{}, io.EOF
	}
	if stream.rawJSON {
		if !stream.jsonRead.CompareAndSwap(false, true) {
			return SSEEvent{}, io.EOF
		}
		data, err := io.ReadAll(io.LimitReader(stream.body, maximumSSEEventBytes+1))
		if err != nil {
			return SSEEvent{}, fmt.Errorf("read JSON streaming fallback: %w", err)
		}
		if len(data) > maximumSSEEventBytes {
			return SSEEvent{}, errors.New("JSON streaming fallback exceeds size limit")
		}
		return SSEEvent{Event: "http.response", Data: data}, nil
	}
	return stream.decoder.Next()
}

// Close closes the streaming HTTP response body
func (stream *SSEStream) Close() error {
	if stream == nil {
		return nil
	}
	stream.closeOnce.Do(func() {
		stream.closed.Store(true)
		stream.closeErr = stream.body.Close()
	})
	return stream.closeErr
}

type sseDecoder struct {
	reader   *bufio.Reader
	maxBytes int
}

func newSSEDecoder(reader io.Reader, maxBytes int) *sseDecoder {
	return &sseDecoder{
		reader:   bufio.NewReaderSize(reader, 64<<10),
		maxBytes: maxBytes,
	}
}

func (decoder *sseDecoder) Next() (SSEEvent, error) {
	var eventName string
	var data []byte
	for {
		line, err := decoder.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) && len(data) > 0 {
				return SSEEvent{Event: eventName, Data: data}, nil
			}
			return SSEEvent{}, err
		}
		if len(line) == 0 {
			if len(data) == 0 {
				eventName = ""
				continue
			}
			return SSEEvent{Event: eventName, Data: data}, nil
		}
		if line[0] == ':' {
			continue
		}

		field, value, found := bytes.Cut(line, []byte{':'})
		if !found {
			value = nil
		} else if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		switch string(field) {
		case "event":
			eventName = string(value)
		case "data":
			additional := len(value)
			if len(data) > 0 {
				additional++
			}
			if len(data)+additional > decoder.maxBytes {
				return SSEEvent{}, errors.New("server-sent event exceeds size limit")
			}
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, value...)
		}
	}
}

func (decoder *sseDecoder) readLine() ([]byte, error) {
	line := make([]byte, 0, 256)
	for {
		fragment, err := decoder.reader.ReadSlice('\n')
		if len(line)+len(fragment) > decoder.maxBytes {
			return nil, errors.New("server-sent event line exceeds size limit")
		}
		line = append(line, fragment...)
		switch {
		case err == nil:
			line = line[:len(line)-1]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return line, nil
		default:
			return nil, err
		}
	}
}
