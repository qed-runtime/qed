package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qed-runtime/qed/agent"
)

const maximumRecentSessions = 256

// SessionDescriptor is a content-free summary used to navigate recent Sessions
type SessionDescriptor struct {
	// ID identifies the Session accepted by SessionStore methods
	ID string
	// Revision is the latest persisted Event revision
	Revision uint64
	// MessageCount is the number of replayable user and assistant Messages
	MessageCount int
	// LastRunID identifies the Run that emitted the latest Event
	LastRunID string
	// UpdatedAt is the latest persisted Event time
	UpdatedAt time.Time
	// Waiting reports whether the Session has a pending WaitRequest
	Waiting bool
}

// Catalog lists bounded, content-free summaries for recent Sessions
//
// Implementations return at most limit entries in newest-first order and use
// lexical Session ID order when timestamps are equal. The standard stores
// accept limits from one through 256. Implementations must honor ctx
// cancellation.
type Catalog interface {
	RecentSessions(ctx context.Context, limit int) ([]SessionDescriptor, error)
}

// RecentSessions returns bounded Session summaries in newest-first order
func (store *MemoryStore) RecentSessions(ctx context.Context, limit int) ([]SessionDescriptor, error) {
	if err := validateCatalogRequest(ctx, limit); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	descriptors := make([]SessionDescriptor, 0, min(limit, len(store.sessions)))
	for _, snapshot := range store.sessions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		descriptors = retainRecentSession(descriptors, describeSnapshot(snapshot), limit)
	}
	return descriptors, nil
}

// RecentSessions returns bounded Session summaries in newest-first order
func (store *JSONLStore) RecentSessions(ctx context.Context, limit int) ([]SessionDescriptor, error) {
	if err := validateCatalogRequest(ctx, limit); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	directory, err := os.Open(store.root)
	if err != nil {
		return nil, fmt.Errorf("open Session Store catalog: %w", err)
	}
	defer directory.Close()
	descriptors := make([]SessionDescriptor, 0, limit)
	for {
		entries, readErr := directory.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return nil, fmt.Errorf("inspect Session Event Log %q: %w", entry.Name(), err)
			}
			if !info.Mode().IsRegular() {
				continue
			}
			eventPath := filepath.Join(store.root, entry.Name())
			lockPath := strings.TrimSuffix(eventPath, ".jsonl") + ".lock"
			release, err := acquireSessionLock(ctx, lockPath)
			if err != nil {
				return nil, err
			}
			descriptor, describeErr := describeEventLog(ctx, eventPath)
			release()
			if describeErr != nil {
				return nil, describeErr
			}
			if filepath.Base(store.eventPath(descriptor.ID)) != entry.Name() {
				return nil, fmt.Errorf("Session Event Log %q has an invalid identity", entry.Name())
			}
			descriptors = retainRecentSession(descriptors, descriptor, limit)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("list Session Store catalog: %w", readErr)
		}
	}
	return descriptors, nil
}

func validateCatalogRequest(ctx context.Context, limit int) error {
	if ctx == nil {
		return errors.New("Session catalog context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if limit <= 0 || limit > maximumRecentSessions {
		return fmt.Errorf("Session catalog limit must be between 1 and %d", maximumRecentSessions)
	}
	return nil
}

func describeSnapshot(snapshot agent.SessionSnapshot) SessionDescriptor {
	descriptor := SessionDescriptor{
		ID:           snapshot.ID,
		Revision:     snapshot.Revision,
		MessageCount: len(snapshot.Messages),
		Waiting:      snapshot.PendingWait != nil,
	}
	if len(snapshot.Events) != 0 {
		latest := snapshot.Events[len(snapshot.Events)-1]
		descriptor.LastRunID = latest.RunID
		descriptor.UpdatedAt = latest.Time
	}
	return descriptor
}

func describeEventLog(ctx context.Context, path string) (SessionDescriptor, error) {
	file, err := os.Open(path)
	if err != nil {
		return SessionDescriptor{}, fmt.Errorf("open Session Event Log: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	var descriptor SessionDescriptor
	for {
		if err := ctx.Err(); err != nil {
			return SessionDescriptor{}, err
		}
		var record eventRecord
		if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return SessionDescriptor{}, fmt.Errorf("decode Session Event revision %d: %w", descriptor.Revision+1, err)
		}
		event := record.Event
		if event.SessionID == "" {
			return SessionDescriptor{}, errors.New("Session Event Log contains an empty Session ID")
		}
		if descriptor.ID == "" {
			descriptor.ID = event.SessionID
		} else if event.SessionID != descriptor.ID {
			return SessionDescriptor{}, fmt.Errorf("Session Event Log contains ID %q, want %q", event.SessionID, descriptor.ID)
		}
		if event.SessionRevision != descriptor.Revision+1 {
			return SessionDescriptor{}, fmt.Errorf("Session Event revision = %d, want %d", event.SessionRevision, descriptor.Revision+1)
		}
		descriptor.Revision = event.SessionRevision
		descriptor.LastRunID = event.RunID
		descriptor.UpdatedAt = event.Time
		switch event.Type {
		case agent.EventUserMessageAdded, agent.EventMessageCompleted:
			if event.Message != nil {
				descriptor.MessageCount++
			}
		case agent.EventRunWaiting:
			descriptor.Waiting = event.WaitRequest != nil
		case agent.EventRunResumed, agent.EventRunCompleted, agent.EventRunFailed, agent.EventRunCanceled:
			descriptor.Waiting = false
		}
	}
	if descriptor.ID == "" {
		return SessionDescriptor{}, errors.New("Session Event Log is empty")
	}
	if err := validateSessionID(descriptor.ID); err != nil {
		return SessionDescriptor{}, fmt.Errorf("Session Event Log identity: %w", err)
	}
	return descriptor, nil
}

func retainRecentSession(
	descriptors []SessionDescriptor,
	descriptor SessionDescriptor,
	limit int,
) []SessionDescriptor {
	descriptors = append(descriptors, descriptor)
	sort.Slice(descriptors, func(first, second int) bool {
		if !descriptors[first].UpdatedAt.Equal(descriptors[second].UpdatedAt) {
			return descriptors[first].UpdatedAt.After(descriptors[second].UpdatedAt)
		}
		return descriptors[first].ID < descriptors[second].ID
	})
	if len(descriptors) > limit {
		descriptors = descriptors[:limit]
	}
	return descriptors
}
