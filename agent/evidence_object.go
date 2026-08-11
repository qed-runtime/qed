package agent

import "context"

// EvidenceObjectRef identifies immutable content stored outside model context
//
// Digest is content-addressed and does not grant access by itself. Applications
// must enforce the same tenant and Session boundaries used by their Evidence
// Object Store.
type EvidenceObjectRef struct {
	// Digest is a sha256-prefixed digest of the exact object bytes
	Digest string `json:"digest"`
	// Bytes is the exact object size, or zero when a lookup supplies only Digest
	Bytes int64 `json:"bytes"`
	// MediaType describes the stored representation and may be empty for lookup
	MediaType string `json:"media_type"`
}

// EvidenceObjectStore persists immutable content used by Context Checkpoints
//
// Implementations must be safe for concurrent use. PutObject must be
// idempotent for identical content and GetObject must verify content identity.
type EvidenceObjectStore interface {
	PutObject(ctx context.Context, mediaType string, content []byte) (EvidenceObjectRef, error)
	GetObject(ctx context.Context, reference EvidenceObjectRef) ([]byte, error)
}
