package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/qed-runtime/qed/extension/protocol"
)

var errClientClosed = errors.New("Extension RPC client is closed")

type client struct {
	reader *protocol.Reader
	writer *protocol.Writer
	nextID atomic.Uint64

	mu       sync.Mutex
	pending  map[string]chan protocol.Envelope
	closed   bool
	closeErr error
	done     chan struct{}
}

func newClient(input io.Reader, output io.Writer) *client {
	configured := &client{
		reader:  protocol.NewReader(input),
		writer:  protocol.NewWriter(output),
		pending: make(map[string]chan protocol.Envelope),
		done:    make(chan struct{}),
	}
	go configured.readLoop()
	return configured
}

func (client *client) call(ctx context.Context, method protocol.Method, params any, result any) error {
	if ctx == nil {
		return errors.New("Extension RPC context must not be nil")
	}
	if method == "" {
		return errors.New("Extension RPC method is required")
	}
	encoded, err := protocol.Marshal(params)
	if err != nil {
		return fmt.Errorf("encode Extension RPC %q params: %w", method, err)
	}
	id := client.newID()
	response := make(chan protocol.Envelope, 1)
	if err := client.register(id, response); err != nil {
		return err
	}
	if err := client.writer.Write(protocol.Envelope{
		Version: protocol.Version,
		ID:      id,
		Method:  method,
		Params:  encoded,
	}); err != nil {
		client.remove(id, response)
		client.fail(fmt.Errorf("write Extension RPC request: %w", err))
		return err
	}

	select {
	case envelope := <-response:
		if envelope.Error != nil {
			return envelope.Error
		}
		if result == nil {
			return nil
		}
		if err := protocol.Unmarshal(envelope.Result, result); err != nil {
			return fmt.Errorf("decode Extension RPC %q result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		client.remove(id, response)
		client.sendCancel(id)
		return ctx.Err()
	case <-client.done:
		client.remove(id, response)
		return client.terminalError()
	}
}

func (client *client) newID() string {
	return fmt.Sprintf("rpc-%d", client.nextID.Add(1))
}

func (client *client) register(id string, response chan protocol.Envelope) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		if client.closeErr != nil {
			return client.closeErr
		}
		return errClientClosed
	}
	client.pending[id] = response
	return nil
}

func (client *client) remove(id string, response chan protocol.Envelope) {
	client.mu.Lock()
	if client.pending[id] == response {
		delete(client.pending, id)
	}
	client.mu.Unlock()
}

func (client *client) sendCancel(requestID string) {
	params, err := protocol.Marshal(protocol.CancelRequest{RequestID: requestID})
	if err != nil {
		return
	}
	_ = client.writer.Write(protocol.Envelope{
		Version: protocol.Version,
		ID:      client.newID(),
		Method:  protocol.MethodCancel,
		Params:  params,
	})
}

func (client *client) readLoop() {
	for {
		envelope, err := client.reader.Read()
		if err != nil {
			client.fail(fmt.Errorf("read Extension RPC response: %w", err))
			return
		}
		if err := validateResponse(envelope); err != nil {
			client.fail(err)
			return
		}
		client.mu.Lock()
		response := client.pending[envelope.ID]
		if response != nil {
			delete(client.pending, envelope.ID)
		}
		client.mu.Unlock()
		if response != nil {
			response <- envelope
		}
	}
}

func validateResponse(envelope protocol.Envelope) error {
	if envelope.Version != protocol.Version {
		return fmt.Errorf(
			"Extension RPC response protocol version %d is unsupported, want %d",
			envelope.Version,
			protocol.Version,
		)
	}
	if envelope.ID == "" || envelope.Method != "" || len(envelope.Params) > 0 {
		return errors.New("Extension RPC response envelope is invalid")
	}
	if envelope.Error != nil && len(envelope.Result) > 0 {
		return errors.New("Extension RPC response contains both result and error")
	}
	if envelope.Error == nil && len(envelope.Result) == 0 {
		return errors.New("Extension RPC response contains neither result nor error")
	}
	if len(envelope.Result) > 0 && !json.Valid(envelope.Result) {
		return errors.New("Extension RPC response result is invalid JSON")
	}
	return nil
}

func (client *client) fail(err error) {
	if err == nil {
		err = errClientClosed
	}
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		return
	}
	client.closed = true
	client.closeErr = err
	close(client.done)
	client.mu.Unlock()
}

func (client *client) terminalError() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closeErr != nil {
		return client.closeErr
	}
	return errClientClosed
}
