package agent

import (
	"context"
	"errors"
)

var (
	// ErrSessionNotFound indicates that a Session Store has no matching Session
	ErrSessionNotFound = errors.New("session not found")
	// ErrSessionConflict indicates an optimistic Session revision mismatch
	ErrSessionConflict = errors.New("session revision conflict")
)

// SessionSnapshot contains one replayable Session state
type SessionSnapshot struct {
	ID              string              `json:"id"`
	Revision        uint64              `json:"revision"`
	Messages        []Message           `json:"messages,omitempty"`
	Events          []Event             `json:"events,omitempty"`
	Checkpoint      *ContextCheckpoint  `json:"checkpoint,omitempty"`
	EvidenceObjects []EvidenceObjectRef `json:"evidence_objects,omitempty"`
	PendingWait     *WaitRequest        `json:"pending_wait,omitempty"`
	PendingTool     *ToolCall           `json:"pending_tool,omitempty"`
}

// SessionStore persists ordered Run Events with optimistic revisions
//
// Append must atomically reject an unexpected revision with ErrSessionConflict.
// Implementations must be safe for concurrent use.
type SessionStore interface {
	Load(ctx context.Context, id string) (SessionSnapshot, error)
	Append(ctx context.Context, id string, expectedRevision uint64, events []Event) (uint64, error)
	Snapshot(ctx context.Context, id string) (SessionSnapshot, error)
}
