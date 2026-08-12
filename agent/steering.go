package agent

import (
	"errors"
	"fmt"
	"strings"
)

// MaxPendingSteeringMessages bounds the number of active-Run Messages retained before a safe boundary
const MaxPendingSteeringMessages = 64

var (
	// ErrRunClosed indicates that a Run no longer accepts steering input
	ErrRunClosed = errors.New("run no longer accepts steering")
	// ErrRunWaiting indicates that a pending WaitResponse must be supplied before steering
	ErrRunWaiting = errors.New("run is waiting for external input")
	// ErrInvalidSteeringMessage indicates that steering input is not a plain user Message
	ErrInvalidSteeringMessage = errors.New("invalid steering message")
	// ErrSteeringQueueFull indicates that the bounded steering queue has no capacity
	ErrSteeringQueueFull = errors.New("run steering queue is full")
)

type steeringBoundary uint8

const (
	steeringBoundaryContinue steeringBoundary = iota
	steeringBoundaryComplete
	steeringBoundaryCanceled
)

// Steer queues one user Message for the next safe Provider request boundary
//
// The Message must have RoleUser, non-empty text, and no Provider or Tool state.
// An optional FactDirective may explicitly supersede or resolve earlier Facts.
// Steer does not interrupt an in-flight Provider request, retry, or Tool batch.
// A nil error means the Message was queued, while the corresponding
// user.message.added Event confirms that Runtime applied it and, when a
// Session Store is configured, persisted it.
// A pending wait, full queue, or terminal Run returns ErrRunWaiting,
// ErrSteeringQueueFull, or ErrRunClosed. Cancellation, deadline expiry, and a
// terminal Run failure may discard queued Messages without an Event.
// Steer is safe for concurrent use and preserves successful submission order.
func (handle *RunHandle) Steer(message Message) error {
	if err := validateSteeringMessage(message); err != nil {
		return err
	}
	if handle == nil {
		return ErrRunClosed
	}
	message = cloneMessage(message)

	handle.mu.Lock()
	defer handle.mu.Unlock()
	if !handle.steeringOpen {
		return ErrRunClosed
	}
	if handle.waiter != nil {
		if _, waiting := handle.waiter.pendingRequest(); waiting {
			return ErrRunWaiting
		}
	}
	if len(handle.steering) >= MaxPendingSteeringMessages {
		return ErrSteeringQueueFull
	}
	handle.steering = append(handle.steering, message)
	return nil
}

func validateSteeringMessage(message Message) error {
	if message.Role != RoleUser {
		return fmt.Errorf("%w: role must be %q", ErrInvalidSteeringMessage, RoleUser)
	}
	if strings.TrimSpace(message.Text) == "" {
		return fmt.Errorf("%w: text must not be empty", ErrInvalidSteeringMessage)
	}
	if message.ToolCallID != "" || message.ToolName != "" || message.ToolIsError ||
		len(message.ToolCalls) != 0 || message.StopReason != "" || message.RawStopReason != "" ||
		message.Usage != nil || message.ResponseID != "" || message.Model != "" ||
		message.ProviderState != nil {
		return fmt.Errorf("%w: user message contains Provider or Tool state", ErrInvalidSteeringMessage)
	}
	if err := validateFactDirectiveMessage(message); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSteeringMessage, err)
	}
	return nil
}

func (handle *RunHandle) takeSteering() ([]Message, bool) {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.cancelRequested {
		return nil, true
	}
	if !handle.steeringOpen || len(handle.steering) == 0 {
		return nil, false
	}
	messages := handle.steering
	handle.steering = nil
	return messages, false
}

func (handle *RunHandle) resolveEndTurn() ([]Message, steeringBoundary) {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.cancelRequested {
		return nil, steeringBoundaryCanceled
	}
	if !handle.steeringOpen {
		return nil, steeringBoundaryComplete
	}
	if len(handle.steering) == 0 {
		handle.steeringOpen = false
		return nil, steeringBoundaryComplete
	}
	messages := handle.steering
	handle.steering = nil
	return messages, steeringBoundaryContinue
}

func (handle *RunHandle) requestCancel() bool {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if !handle.steeringOpen {
		return false
	}
	handle.steeringOpen = false
	handle.cancelRequested = true
	handle.steering = nil
	return true
}

func (handle *RunHandle) discardSteering() {
	handle.mu.Lock()
	handle.steeringOpen = false
	handle.steering = nil
	handle.mu.Unlock()
}
