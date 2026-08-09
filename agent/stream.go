package agent

import (
	"context"
	"errors"
	"io"
	"sync"
)

// ModelStreamEventType identifies one Provider stream item
type ModelStreamEventType string

// Provider stream item types
const (
	ModelStreamTextDelta       ModelStreamEventType = "text.delta"
	ModelStreamMessageComplete ModelStreamEventType = "message.completed"
)

// ModelStreamEvent contains one incremental Provider result
//
// TextDelta is populated for ModelStreamTextDelta. Message is populated exactly
// once for ModelStreamMessageComplete and contains the complete assistant
// message, including Tool calls and Provider continuation state.
type ModelStreamEvent struct {
	Type      ModelStreamEventType
	TextDelta string
	Message   *Message
}

// ModelStream yields Provider output until io.EOF
//
// Implementations must be safe to close concurrently with Next and must honor
// the Context supplied to Provider.Stream.
type ModelStream interface {
	Next() (ModelStreamEvent, error)
	Close() error
}

// MessageStream returns a finite stream for an already completed Message
//
// Providers without transport-level deltas can use this adapter while still
// satisfying the stream contract. Non-empty text is emitted as one delta before
// the completed Message.
func MessageStream(message Message) ModelStream {
	events := make([]ModelStreamEvent, 0, 2)
	if message.Text != "" {
		events = append(events, ModelStreamEvent{
			Type:      ModelStreamTextDelta,
			TextDelta: message.Text,
		})
	}
	message = cloneMessage(message)
	events = append(events, ModelStreamEvent{
		Type:    ModelStreamMessageComplete,
		Message: &message,
	})
	return &sliceModelStream{events: events}
}

// ModelStreamFunc adapts a receive function and optional close function to a ModelStream
type ModelStreamFunc struct {
	NextFunc  func() (ModelStreamEvent, error)
	CloseFunc func() error
}

// Next returns the next Provider stream item
func (stream *ModelStreamFunc) Next() (ModelStreamEvent, error) {
	if stream == nil || stream.NextFunc == nil {
		return ModelStreamEvent{}, io.EOF
	}
	return stream.NextFunc()
}

// Close releases resources owned by the Provider stream
func (stream *ModelStreamFunc) Close() error {
	if stream == nil || stream.CloseFunc == nil {
		return nil
	}
	return stream.CloseFunc()
}

type sliceModelStream struct {
	mu     sync.Mutex
	events []ModelStreamEvent
	index  int
	closed bool
}

func (stream *sliceModelStream) Next() (ModelStreamEvent, error) {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.closed || stream.index >= len(stream.events) {
		return ModelStreamEvent{}, io.EOF
	}
	event := stream.events[stream.index]
	stream.index++
	if event.Message != nil {
		message := cloneMessage(*event.Message)
		event.Message = &message
	}
	return event, nil
}

func (stream *sliceModelStream) Close() error {
	stream.mu.Lock()
	stream.closed = true
	stream.mu.Unlock()
	return nil
}

func consumeModelStream(ctx context.Context, stream ModelStream, onDelta func(string) error) (Message, error) {
	if stream == nil {
		return Message{}, errors.New("provider returned a nil stream")
	}
	defer stream.Close()
	var completed *Message
	for {
		if err := ctx.Err(); err != nil {
			return Message{}, err
		}
		event, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Message{}, err
		}
		switch event.Type {
		case ModelStreamTextDelta:
			if event.TextDelta == "" {
				continue
			}
			if err := onDelta(event.TextDelta); err != nil {
				return Message{}, err
			}
		case ModelStreamMessageComplete:
			if event.Message == nil {
				return Message{}, errors.New("provider completed stream without a message")
			}
			if completed != nil {
				return Message{}, errors.New("provider completed stream more than once")
			}
			message := cloneMessage(*event.Message)
			completed = &message
		default:
			return Message{}, errors.New("provider returned an unsupported stream event")
		}
	}
	if completed == nil {
		return Message{}, errors.New("provider stream ended without a completed message")
	}
	return *completed, nil
}
