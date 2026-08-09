package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

var (
	// ErrRunNotWaiting indicates that Resume was called without a pending request
	ErrRunNotWaiting = errors.New("run is not waiting for input")
	// ErrWaitResponseMismatch indicates that a response targets another request
	ErrWaitResponseMismatch = errors.New("wait response request ID does not match")
	// ErrWaitUnavailable indicates that execution has no Run waiting broker
	ErrWaitUnavailable = errors.New("run waiting is unavailable")
)

// WaitKind identifies why a Run needs external input
type WaitKind string

// Standard Run wait kinds
const (
	WaitKindUser     WaitKind = "user"
	WaitKindHuman    WaitKind = "human"
	WaitKindApproval WaitKind = "approval"
)

// WaitRequest describes external input required before a Run can continue
//
// Payload must not contain credentials or raw Tool arguments unless the caller
// explicitly intends to persist and expose them through Run Events.
type WaitRequest struct {
	ID      string          `json:"id"`
	Kind    WaitKind        `json:"kind"`
	Prompt  string          `json:"prompt,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// WaitResponse resumes one pending Run request
type WaitResponse struct {
	RequestID string          `json:"request_id"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type runWaiterContextKey struct{}

type runWaiter interface {
	wait(context.Context, WaitRequest) (WaitResponse, error)
}

// WaitForInput suspends the current Run until its handle receives a matching response
func WaitForInput(ctx context.Context, request WaitRequest) (WaitResponse, error) {
	if ctx == nil {
		return WaitResponse{}, errors.New("wait context must not be nil")
	}
	waiter, ok := ctx.Value(runWaiterContextKey{}).(runWaiter)
	if !ok || waiter == nil {
		return WaitResponse{}, ErrWaitUnavailable
	}
	return waiter.wait(ctx, request)
}

type waitBroker struct {
	emit func(Event) error

	mu        sync.Mutex
	pending   *WaitRequest
	response  chan WaitResponse
	responded bool
	closed    bool
	preload   *preloadedWait
}

type preloadedWait struct {
	request  WaitRequest
	response WaitResponse
}

func newWaitBroker(emit func(Event) error) *waitBroker {
	return &waitBroker{emit: emit}
}

func newResumingWaitBroker(emit func(Event) error, request WaitRequest, response WaitResponse) *waitBroker {
	return &waitBroker{
		emit: emit,
		preload: &preloadedWait{
			request:  cloneWaitRequest(request),
			response: cloneWaitResponse(response),
		},
	}
}

func (broker *waitBroker) wait(ctx context.Context, request WaitRequest) (WaitResponse, error) {
	if request.ID == "" {
		id, err := newRunID()
		if err != nil {
			return WaitResponse{}, err
		}
		request.ID = "wait_" + id[len("run_"):]
	}
	request.Payload = cloneRawMessage(request.Payload)
	broker.mu.Lock()
	if broker.closed {
		broker.mu.Unlock()
		return WaitResponse{}, context.Canceled
	}
	if broker.pending != nil {
		broker.mu.Unlock()
		return WaitResponse{}, errors.New("run already has a pending wait request")
	}
	if broker.preload != nil {
		preloaded := broker.preload
		if request.ID != preloaded.request.ID || request.Kind != preloaded.request.Kind {
			broker.mu.Unlock()
			return WaitResponse{}, ErrWaitResponseMismatch
		}
		broker.preload = nil
		broker.mu.Unlock()
		response := cloneWaitResponse(preloaded.response)
		if err := broker.emit(Event{Type: EventRunResumed, WaitResponse: &response}); err != nil {
			return WaitResponse{}, err
		}
		return response, nil
	}
	response := make(chan WaitResponse, 1)
	broker.pending = &request
	broker.response = response
	broker.responded = false
	broker.mu.Unlock()

	requestCopy := cloneWaitRequest(request)
	if err := broker.emit(Event{Type: EventRunWaiting, WaitRequest: &requestCopy}); err != nil {
		broker.clear(request.ID)
		return WaitResponse{}, err
	}

	select {
	case value := <-response:
		broker.clear(request.ID)
		value.Payload = cloneRawMessage(value.Payload)
		if err := broker.emit(Event{Type: EventRunResumed, WaitResponse: &value}); err != nil {
			return WaitResponse{}, err
		}
		return value, nil
	case <-ctx.Done():
		broker.clear(request.ID)
		return WaitResponse{}, ctx.Err()
	}
}

func (broker *waitBroker) resume(response WaitResponse) error {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.pending == nil || broker.response == nil {
		return ErrRunNotWaiting
	}
	if response.RequestID != broker.pending.ID {
		return ErrWaitResponseMismatch
	}
	if broker.responded {
		return ErrRunNotWaiting
	}
	response.Payload = cloneRawMessage(response.Payload)
	broker.response <- response
	broker.responded = true
	return nil
}

func (broker *waitBroker) pendingRequest() (WaitRequest, bool) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.pending == nil {
		return WaitRequest{}, false
	}
	return cloneWaitRequest(*broker.pending), true
}

func (broker *waitBroker) clear(requestID string) {
	broker.mu.Lock()
	if broker.pending != nil && broker.pending.ID == requestID {
		broker.pending = nil
		broker.response = nil
		broker.responded = false
	}
	broker.mu.Unlock()
}

func (broker *waitBroker) close() {
	broker.mu.Lock()
	broker.closed = true
	broker.mu.Unlock()
}

func cloneWaitRequest(request WaitRequest) WaitRequest {
	request.Payload = cloneRawMessage(request.Payload)
	return request
}

func cloneWaitResponse(response WaitResponse) WaitResponse {
	response.Payload = cloneRawMessage(response.Payload)
	return response
}
