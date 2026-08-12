// Package session provides standard Agent Session Store implementations
package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qed-runtime/qed/agent"
)

const maximumSessionIDBytes = 512

// MemoryStore keeps Session Events and snapshots in process memory
type MemoryStore struct {
	mu       sync.RWMutex
	sessions map[string]agent.SessionSnapshot
}

// NewMemoryStore constructs an empty concurrency-safe Session Store
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{sessions: make(map[string]agent.SessionSnapshot)}
}

// Load returns a replayable Session snapshot
func (store *MemoryStore) Load(ctx context.Context, id string) (agent.SessionSnapshot, error) {
	if ctx == nil {
		return agent.SessionSnapshot{}, errors.New("Session Load context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return agent.SessionSnapshot{}, err
	}
	if err := validateSessionID(id); err != nil {
		return agent.SessionSnapshot{}, err
	}
	store.mu.RLock()
	snapshot, ok := store.sessions[id]
	store.mu.RUnlock()
	if !ok {
		return agent.SessionSnapshot{}, fmt.Errorf("%w: %q", agent.ErrSessionNotFound, id)
	}
	return cloneSnapshot(snapshot), nil
}

// Append atomically appends Events when expectedRevision matches
func (store *MemoryStore) Append(ctx context.Context, id string, expectedRevision uint64, events []agent.Event) (uint64, error) {
	if ctx == nil {
		return 0, errors.New("Session Append context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := validateSessionID(id); err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, errors.New("at least one Session event is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.sessions == nil {
		store.sessions = make(map[string]agent.SessionSnapshot)
	}
	snapshot, ok := store.sessions[id]
	if !ok {
		snapshot = agent.SessionSnapshot{ID: id}
	}
	if snapshot.Revision != expectedRevision {
		return snapshot.Revision, fmt.Errorf("%w: Session %q has revision %d, expected %d", agent.ErrSessionConflict, id, snapshot.Revision, expectedRevision)
	}
	for index := range events {
		event := cloneEvent(events[index])
		if event.SessionID != "" && event.SessionID != id {
			return snapshot.Revision, fmt.Errorf("Session event targets %q, want %q", event.SessionID, id)
		}
		event.SessionID = id
		event.SessionRevision = snapshot.Revision + 1
		applyEvent(&snapshot, event)
		snapshot.Events = append(snapshot.Events, event)
		snapshot.Revision++
	}
	store.sessions[id] = cloneSnapshot(snapshot)
	return snapshot.Revision, nil
}

// Snapshot returns a replayable Session snapshot
func (store *MemoryStore) Snapshot(ctx context.Context, id string) (agent.SessionSnapshot, error) {
	return store.Load(ctx, id)
}

// JSONLStore persists one append-only JSONL Event Log per Session
//
// File names are SHA-256 digests of Session IDs. Append uses an exclusive lock
// file and fsync so separate processes observe optimistic revisions safely.
type JSONLStore struct {
	root string
	mu   sync.Mutex
}

// NewJSONLStore creates or opens a directory containing Session Event Logs
func NewJSONLStore(root string) (*JSONLStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("Session Store root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve Session Store root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create Session Store root: %w", err)
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("protect Session Store root: %w", err)
	}
	return &JSONLStore{root: absolute}, nil
}

// Load replays one Session Event Log
func (store *JSONLStore) Load(ctx context.Context, id string) (agent.SessionSnapshot, error) {
	if ctx == nil {
		return agent.SessionSnapshot{}, errors.New("Session Load context must not be nil")
	}
	if err := validateSessionID(id); err != nil {
		return agent.SessionSnapshot{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	release, err := store.acquireFileLock(ctx, id)
	if err != nil {
		return agent.SessionSnapshot{}, err
	}
	defer release()
	return store.loadUnlocked(id)
}

// Append atomically validates a revision and appends fsynced JSONL Events
func (store *JSONLStore) Append(ctx context.Context, id string, expectedRevision uint64, events []agent.Event) (uint64, error) {
	if ctx == nil {
		return 0, errors.New("Session Append context must not be nil")
	}
	if err := validateSessionID(id); err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, errors.New("at least one Session event is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	release, err := store.acquireFileLock(ctx, id)
	if err != nil {
		return 0, err
	}
	defer release()
	snapshot, err := store.loadUnlocked(id)
	if errors.Is(err, agent.ErrSessionNotFound) {
		snapshot = agent.SessionSnapshot{ID: id}
		err = nil
	}
	if err != nil {
		return 0, err
	}
	if snapshot.Revision != expectedRevision {
		return snapshot.Revision, fmt.Errorf("%w: Session %q has revision %d, expected %d", agent.ErrSessionConflict, id, snapshot.Revision, expectedRevision)
	}

	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	revision := snapshot.Revision
	previousManifest := lastPrefixManifest(snapshot.Events)
	for index := range events {
		event := cloneEvent(events[index])
		if event.SessionID != "" && event.SessionID != id {
			return revision, fmt.Errorf("Session event targets %q, want %q", event.SessionID, id)
		}
		event.SessionID = id
		revision++
		event.SessionRevision = revision
		if err := encoder.Encode(newEventRecord(event, previousManifest)); err != nil {
			return snapshot.Revision, fmt.Errorf("append Session Event: %w", err)
		}
		if event.PrefixManifest != nil {
			manifest := clonePrefixManifest(*event.PrefixManifest)
			previousManifest = &manifest
		}
	}
	file, err := os.OpenFile(store.eventPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return snapshot.Revision, fmt.Errorf("open Session Event Log: %w", err)
	}
	if _, err := file.Write(encoded.Bytes()); err != nil {
		_ = file.Close()
		return snapshot.Revision, fmt.Errorf("append Session Event Log: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return snapshot.Revision, fmt.Errorf("sync Session Event Log: %w", err)
	}
	if err := file.Close(); err != nil {
		return snapshot.Revision, fmt.Errorf("close Session Event Log: %w", err)
	}
	return revision, nil
}

// Snapshot replays one Session Event Log
func (store *JSONLStore) Snapshot(ctx context.Context, id string) (agent.SessionSnapshot, error) {
	return store.Load(ctx, id)
}

// Root returns the absolute Event Log directory
func (store *JSONLStore) Root() string {
	if store == nil {
		return ""
	}
	return store.root
}

func (store *JSONLStore) loadUnlocked(id string) (agent.SessionSnapshot, error) {
	file, err := os.Open(store.eventPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return agent.SessionSnapshot{}, fmt.Errorf("%w: %q", agent.ErrSessionNotFound, id)
	}
	if err != nil {
		return agent.SessionSnapshot{}, fmt.Errorf("open Session Event Log: %w", err)
	}
	defer file.Close()

	snapshot := agent.SessionSnapshot{ID: id}
	decoder := json.NewDecoder(file)
	var previousManifest *agent.PrefixManifest
	for {
		var record eventRecord
		if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return agent.SessionSnapshot{}, fmt.Errorf("decode Session Event revision %d: %w", snapshot.Revision+1, err)
		}
		event, err := record.event(previousManifest)
		if err != nil {
			return agent.SessionSnapshot{}, fmt.Errorf("reconstruct Session Event revision %d: %w", snapshot.Revision+1, err)
		}
		if event.SessionID != id {
			return agent.SessionSnapshot{}, fmt.Errorf("Session Event Log contains ID %q, want %q", event.SessionID, id)
		}
		if event.SessionRevision != snapshot.Revision+1 {
			return agent.SessionSnapshot{}, fmt.Errorf("Session Event revision = %d, want %d", event.SessionRevision, snapshot.Revision+1)
		}
		applyEvent(&snapshot, event)
		snapshot.Events = append(snapshot.Events, cloneEvent(event))
		if event.PrefixManifest != nil {
			manifest := clonePrefixManifest(*event.PrefixManifest)
			previousManifest = &manifest
		}
		snapshot.Revision++
	}
	return snapshot, nil
}

type eventRecord struct {
	Event               agent.Event                `json:"event"`
	ProviderState       *providerStateEventRecord  `json:"provider_state,omitempty"`
	PrefixManifestDelta *prefixManifestEventRecord `json:"prefix_manifest_delta,omitempty"`
}

type providerStateEventRecord struct {
	Provider string          `json:"provider"`
	Data     json.RawMessage `json:"data"`
}

type prefixManifestEventRecord struct {
	Version      uint32                     `json:"version"`
	Provider     string                     `json:"provider"`
	Model        string                     `json:"model,omitempty"`
	CacheFamily  string                     `json:"cache_family,omitempty"`
	Epoch        string                     `json:"epoch"`
	CommonPrefix int                        `json:"common_prefix,omitempty"`
	Segments     []agent.SegmentFingerprint `json:"segments,omitempty"`
}

func newEventRecord(event agent.Event, previous *agent.PrefixManifest) eventRecord {
	record := eventRecord{Event: event}
	if event.Message != nil && event.Message.ProviderState != nil {
		record.ProviderState = &providerStateEventRecord{
			Provider: event.Message.ProviderState.Provider,
			Data:     cloneRaw(event.Message.ProviderState.Data),
		}
	}
	if event.PrefixManifest != nil {
		common := commonManifestPrefix(previous, event.PrefixManifest)
		record.PrefixManifestDelta = &prefixManifestEventRecord{
			Version:      event.PrefixManifest.Version,
			Provider:     event.PrefixManifest.Provider,
			Model:        event.PrefixManifest.Model,
			CacheFamily:  event.PrefixManifest.CacheFamily,
			Epoch:        event.PrefixManifest.Epoch,
			CommonPrefix: common,
			Segments:     append([]agent.SegmentFingerprint(nil), event.PrefixManifest.Segments[common:]...),
		}
		record.Event.PrefixManifest = nil
	}
	return record
}

func (record eventRecord) event(previous *agent.PrefixManifest) (agent.Event, error) {
	event := record.Event
	if event.PrefixManifest != nil && record.PrefixManifestDelta != nil {
		return agent.Event{}, errors.New("Session Event contains both full and delta Prefix Manifests")
	}
	if record.PrefixManifestDelta != nil {
		delta := record.PrefixManifestDelta
		if delta.CommonPrefix < 0 || (previous == nil && delta.CommonPrefix != 0) ||
			(previous != nil && delta.CommonPrefix > len(previous.Segments)) {
			return agent.Event{}, errors.New("Session Prefix Manifest delta has an invalid common prefix")
		}
		segments := make([]agent.SegmentFingerprint, 0, delta.CommonPrefix+len(delta.Segments))
		if previous != nil {
			segments = append(segments, previous.Segments[:delta.CommonPrefix]...)
		}
		segments = append(segments, delta.Segments...)
		event.PrefixManifest = &agent.PrefixManifest{
			Version:     delta.Version,
			Provider:    delta.Provider,
			Model:       delta.Model,
			CacheFamily: delta.CacheFamily,
			Epoch:       delta.Epoch,
			Segments:    segments,
		}
	}
	if event.Message != nil && record.ProviderState != nil {
		event.Message.ProviderState = &agent.ProviderState{
			Provider: record.ProviderState.Provider,
			Data:     cloneRaw(record.ProviderState.Data),
		}
	}
	return event, nil
}

func commonManifestPrefix(previous, current *agent.PrefixManifest) int {
	if previous == nil || current == nil {
		return 0
	}
	maximum := min(len(previous.Segments), len(current.Segments))
	for index := 0; index < maximum; index++ {
		if previous.Segments[index] != current.Segments[index] {
			return index
		}
	}
	return maximum
}

func lastPrefixManifest(events []agent.Event) *agent.PrefixManifest {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].PrefixManifest != nil {
			manifest := clonePrefixManifest(*events[index].PrefixManifest)
			return &manifest
		}
	}
	return nil
}

func clonePrefixManifest(manifest agent.PrefixManifest) agent.PrefixManifest {
	manifest.Segments = append([]agent.SegmentFingerprint(nil), manifest.Segments...)
	return manifest
}

func (store *JSONLStore) acquireFileLock(ctx context.Context, id string) (func(), error) {
	path := store.lockPath(id)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = file.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("acquire Session Event Log lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (store *JSONLStore) eventPath(id string) string {
	return filepath.Join(store.root, digestSessionID(id)+".jsonl")
}

func (store *JSONLStore) lockPath(id string) string {
	return filepath.Join(store.root, digestSessionID(id)+".lock")
}

func digestSessionID(id string) string {
	digest := sha256.Sum256([]byte(id))
	return hex.EncodeToString(digest[:])
}

func validateSessionID(id string) error {
	if strings.TrimSpace(id) != id || id == "" {
		return errors.New("Session ID is required and must not have surrounding whitespace")
	}
	if len(id) > maximumSessionIDBytes {
		return fmt.Errorf("Session ID exceeds %d bytes", maximumSessionIDBytes)
	}
	return nil
}

func applyEvent(snapshot *agent.SessionSnapshot, event agent.Event) {
	switch event.Type {
	case agent.EventUserMessageAdded, agent.EventMessageCompleted:
		if event.Message != nil {
			message := cloneMessage(*event.Message)
			message.FactDirective = nil
			snapshot.Messages = append(snapshot.Messages, message)
		}
	case agent.EventContextCompacted:
		if event.ContextCheckpoint != nil {
			snapshot.Checkpoint = cloneContextCheckpoint(event.ContextCheckpoint)
		}
		if event.ContextCompaction != nil {
			snapshot.EvidenceObjects = appendUniqueEvidenceObjects(
				snapshot.EvidenceObjects,
				event.ContextCompaction.Externalized,
			)
		}
	case agent.EventCurrentWorldStateCaptured:
		snapshot.CurrentWorldState = cloneCurrentWorldState(event.CurrentWorldState)
	case agent.EventToolStarted:
		if event.ToolCall != nil {
			call := cloneToolCall(*event.ToolCall)
			snapshot.PendingTool = &call
		}
	case agent.EventToolCompleted:
		if event.Message != nil {
			message := cloneMessage(*event.Message)
			message.FactDirective = nil
			snapshot.Messages = append(snapshot.Messages, message)
		}
		snapshot.PendingTool = nil
	case agent.EventRunWaiting:
		if event.WaitRequest != nil {
			request := cloneWaitRequest(*event.WaitRequest)
			snapshot.PendingWait = &request
		}
	case agent.EventRunResumed:
		snapshot.PendingWait = nil
	case agent.EventRunCompleted, agent.EventRunFailed, agent.EventRunCanceled:
		snapshot.PendingWait = nil
		snapshot.PendingTool = nil
	}
}

func cloneSnapshot(snapshot agent.SessionSnapshot) agent.SessionSnapshot {
	snapshot.Messages = cloneMessages(snapshot.Messages)
	snapshot.Events = cloneEvents(snapshot.Events)
	snapshot.Checkpoint = cloneContextCheckpoint(snapshot.Checkpoint)
	snapshot.CurrentWorldState = cloneCurrentWorldState(snapshot.CurrentWorldState)
	snapshot.EvidenceObjects = append([]agent.EvidenceObjectRef(nil), snapshot.EvidenceObjects...)
	if snapshot.PendingWait != nil {
		request := cloneWaitRequest(*snapshot.PendingWait)
		snapshot.PendingWait = &request
	}
	if snapshot.PendingTool != nil {
		call := cloneToolCall(*snapshot.PendingTool)
		snapshot.PendingTool = &call
	}
	return snapshot
}

func cloneEvents(events []agent.Event) []agent.Event {
	if events == nil {
		return nil
	}
	result := make([]agent.Event, len(events))
	for index := range events {
		result[index] = cloneEvent(events[index])
	}
	return result
}

func cloneEvent(event agent.Event) agent.Event {
	if event.PrefixManifest != nil {
		manifest := *event.PrefixManifest
		manifest.Segments = append([]agent.SegmentFingerprint(nil), event.PrefixManifest.Segments...)
		event.PrefixManifest = &manifest
	}
	event.CachePlan = cloneCachePlan(event.CachePlan)
	if event.ProviderError != nil {
		providerError := *event.ProviderError
		event.ProviderError = &providerError
	}
	if event.ProviderRetry != nil {
		providerRetry := *event.ProviderRetry
		event.ProviderRetry = &providerRetry
	}
	if event.ProviderRateLimitWait != nil {
		providerWait := *event.ProviderRateLimitWait
		event.ProviderRateLimitWait = &providerWait
	}
	event.ContextCheckpoint = cloneContextCheckpoint(event.ContextCheckpoint)
	if event.ContextCompaction != nil {
		report := *event.ContextCompaction
		report.Externalized = append([]agent.EvidenceObjectRef(nil), event.ContextCompaction.Externalized...)
		if event.ContextCompaction.Validation != nil {
			validation := *event.ContextCompaction.Validation
			validation.Failures = append(
				[]agent.ContextValidationFailure(nil),
				event.ContextCompaction.Validation.Failures...,
			)
			report.Validation = &validation
		}
		event.ContextCompaction = &report
	}
	event.FactDirective = cloneFactLifecycleDirective(event.FactDirective)
	event.CurrentWorldState = cloneCurrentWorldState(event.CurrentWorldState)
	if event.Message != nil {
		message := cloneMessage(*event.Message)
		event.Message = &message
	}
	if event.ToolCall != nil {
		call := cloneToolCall(*event.ToolCall)
		event.ToolCall = &call
	}
	if event.ToolResult != nil {
		result := *event.ToolResult
		if event.ToolResult.ContextOperation != nil {
			operation := *event.ToolResult.ContextOperation
			result.ContextOperation = &operation
		}
		if event.ToolResult.Policy != nil {
			policy := *event.ToolResult.Policy
			policy.Capabilities = append([]string(nil), event.ToolResult.Policy.Capabilities...)
			result.Policy = &policy
		}
		event.ToolResult = &result
	}
	if event.WaitRequest != nil {
		request := cloneWaitRequest(*event.WaitRequest)
		event.WaitRequest = &request
	}
	if event.WaitResponse != nil {
		response := *event.WaitResponse
		response.Payload = cloneRaw(response.Payload)
		event.WaitResponse = &response
	}
	return event
}

func cloneCurrentWorldState(state *agent.CurrentWorldState) *agent.CurrentWorldState {
	if state == nil {
		return nil
	}
	cloned := *state
	cloned.Snapshot.Files = make([]agent.CurrentWorldFile, len(state.Snapshot.Files))
	for index := range state.Snapshot.Files {
		cloned.Snapshot.Files[index] = state.Snapshot.Files[index]
		if state.Snapshot.Files[index].Observation != nil {
			observation := *state.Snapshot.Files[index].Observation
			cloned.Snapshot.Files[index].Observation = &observation
		}
	}
	if state.Snapshot.Git != nil {
		gitState := *state.Snapshot.Git
		gitState.Changes = append([]agent.CurrentWorldGitChange(nil), state.Snapshot.Git.Changes...)
		if state.Snapshot.Git.Observation != nil {
			observation := *state.Snapshot.Git.Observation
			gitState.Observation = &observation
		}
		cloned.Snapshot.Git = &gitState
	}
	cloned.Snapshot.Checks = make([]agent.CurrentWorldCheck, len(state.Snapshot.Checks))
	for index := range state.Snapshot.Checks {
		cloned.Snapshot.Checks[index] = state.Snapshot.Checks[index]
		cloned.Snapshot.Checks[index].Argv = append([]string(nil), state.Snapshot.Checks[index].Argv...)
	}
	return &cloned
}

func cloneMessages(messages []agent.Message) []agent.Message {
	if messages == nil {
		return nil
	}
	result := make([]agent.Message, len(messages))
	for index := range messages {
		result[index] = cloneMessage(messages[index])
	}
	return result
}

func cloneMessage(message agent.Message) agent.Message {
	message.FactDirective = cloneFactLifecycleDirective(message.FactDirective)
	if message.ToolCalls != nil {
		message.ToolCalls = make([]agent.ToolCall, len(message.ToolCalls))
		for index := range message.ToolCalls {
			message.ToolCalls[index] = cloneToolCall(message.ToolCalls[index])
		}
	}
	if message.Usage != nil {
		usage := *message.Usage
		message.Usage = &usage
	}
	if message.ProviderState != nil {
		state := *message.ProviderState
		state.Data = cloneRaw(state.Data)
		message.ProviderState = &state
	}
	return message
}

func cloneFactLifecycleDirective(directive *agent.FactLifecycleDirective) *agent.FactLifecycleDirective {
	if directive == nil {
		return nil
	}
	cloned := *directive
	cloned.Targets = append([]string(nil), directive.Targets...)
	return &cloned
}

func cloneToolCall(call agent.ToolCall) agent.ToolCall {
	call.Arguments = cloneRaw(call.Arguments)
	return call
}

func cloneWaitRequest(request agent.WaitRequest) agent.WaitRequest {
	request.Payload = cloneRaw(request.Payload)
	return request
}

func cloneRaw(value json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}

func cloneContextCheckpoint(checkpoint *agent.ContextCheckpoint) *agent.ContextCheckpoint {
	if checkpoint == nil {
		return nil
	}
	cloned := *checkpoint
	if checkpoint.Ledger != nil {
		ledger := *checkpoint.Ledger
		cloned.Ledger = &ledger
	}
	if checkpoint.Goal != nil {
		goal := *checkpoint.Goal
		cloned.Goal = &goal
	}
	cloned.Facts = append([]agent.CheckpointFact(nil), checkpoint.Facts...)
	cloned.Decisions = append([]agent.CheckpointFact(nil), checkpoint.Decisions...)
	cloned.Executions = append([]agent.CheckpointExecution(nil), checkpoint.Executions...)
	cloned.Evidence = append([]agent.EvidenceObjectRef(nil), checkpoint.Evidence...)
	return &cloned
}

func cloneCachePlan(plan *agent.CachePlan) *agent.CachePlan {
	if plan == nil {
		return nil
	}
	cloned := *plan
	cloned.Breakpoints = append([]agent.CacheBreakpoint(nil), plan.Breakpoints...)
	if plan.Pricing != nil {
		pricing := *plan.Pricing
		cloned.Pricing = &pricing
	}
	if plan.Forecast != nil {
		forecast := *plan.Forecast
		cloned.Forecast = &forecast
	}
	return &cloned
}

func appendUniqueEvidenceObjects(
	current []agent.EvidenceObjectRef,
	additional []agent.EvidenceObjectRef,
) []agent.EvidenceObjectRef {
	seen := make(map[string]struct{}, len(current)+len(additional))
	result := make([]agent.EvidenceObjectRef, 0, len(current)+len(additional))
	for _, reference := range append(append([]agent.EvidenceObjectRef(nil), current...), additional...) {
		if _, exists := seen[reference.Digest]; exists {
			continue
		}
		seen[reference.Digest] = struct{}{}
		result = append(result, reference)
	}
	return result
}
