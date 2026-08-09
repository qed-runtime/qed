// Package evidence records host-owned execution facts for Agent Runs
package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qed-runtime/qed/agent"
)

const bundleVersion = 1

// ToolInvocation records one authorized or rejected Tool invocation
type ToolInvocation struct {
	RunID               string    `json:"run_id,omitempty"`
	ParentRunID         string    `json:"parent_run_id,omitempty"`
	AgentID             string    `json:"agent_id,omitempty"`
	SessionID           string    `json:"session_id,omitempty"`
	CallID              string    `json:"call_id"`
	Tool                string    `json:"tool"`
	ExtensionID         string    `json:"extension_id,omitempty"`
	ExtensionGeneration uint64    `json:"extension_generation,omitempty"`
	Capabilities        []string  `json:"capabilities,omitempty"`
	ArgumentsDigest     string    `json:"arguments_digest"`
	OutputDigest        string    `json:"output_digest,omitempty"`
	PolicyOutcome       string    `json:"policy_outcome,omitempty"`
	PolicyReason        string    `json:"policy_reason,omitempty"`
	StartedAt           time.Time `json:"started_at"`
	CompletedAt         time.Time `json:"completed_at"`
	IsError             bool      `json:"is_error,omitempty"`
	Error               string    `json:"error,omitempty"`
}

// Recorder accepts immutable Evidence records
type Recorder interface {
	RecordToolInvocation(ctx context.Context, invocation ToolInvocation)
}

// RecorderFunc adapts a function to Recorder
type RecorderFunc func(context.Context, ToolInvocation)

// RecordToolInvocation records one Tool invocation
func (recorder RecorderFunc) RecordToolInvocation(ctx context.Context, invocation ToolInvocation) {
	if recorder != nil {
		recorder(ctx, invocation)
	}
}

// NopRecorder discards Evidence records
type NopRecorder struct{}

// RecordToolInvocation discards one Tool invocation
func (NopRecorder) RecordToolInvocation(context.Context, ToolInvocation) {}

// MemoryRecorder stores Evidence records safely for concurrent readers
type MemoryRecorder struct {
	mu          sync.Mutex
	invocations []ToolInvocation
}

// RecordToolInvocation appends one immutable Tool invocation
func (recorder *MemoryRecorder) RecordToolInvocation(_ context.Context, invocation ToolInvocation) {
	invocation.Capabilities = append([]string(nil), invocation.Capabilities...)
	recorder.mu.Lock()
	recorder.invocations = append(recorder.invocations, invocation)
	recorder.mu.Unlock()
}

// ToolInvocations returns an isolated snapshot in execution completion order
func (recorder *MemoryRecorder) ToolInvocations() []ToolInvocation {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	result := make([]ToolInvocation, len(recorder.invocations))
	for index := range recorder.invocations {
		result[index] = recorder.invocations[index]
		result[index].Capabilities = append([]string(nil), recorder.invocations[index].Capabilities...)
	}
	return result
}

// Bundle is the durable, inspectable evidence produced for one Agent Run
type Bundle struct {
	Version        int               `json:"version"`
	Run            RunDescriptor     `json:"run"`
	Agent          AgentDescriptor   `json:"agent"`
	Model          ModelDescriptor   `json:"model"`
	InputDigest    string            `json:"input_digest"`
	ConfigDigest   string            `json:"config_digest,omitempty"`
	WorkspaceState WorkspaceState    `json:"workspace_state"`
	Commands       []CommandEvidence `json:"commands,omitempty"`
	Checks         []CheckEvidence   `json:"checks,omitempty"`
	Changes        []ChangeEvidence  `json:"changes,omitempty"`
	Policies       []PolicyDecision  `json:"policies,omitempty"`
	Artifacts      []Artifact        `json:"artifacts,omitempty"`
	ToolTrace      []ToolInvocation  `json:"tool_trace,omitempty"`
	Events         []agent.Event     `json:"events"`
	Usage          agent.Usage       `json:"usage"`
	CreatedAt      time.Time         `json:"created_at"`
}

// RunDescriptor identifies the evidenced Run
type RunDescriptor struct {
	ID          string          `json:"id"`
	ParentRunID string          `json:"parent_run_id,omitempty"`
	SessionID   string          `json:"session_id,omitempty"`
	Status      agent.RunStatus `json:"status"`
}

// AgentDescriptor identifies the configured Agent
type AgentDescriptor struct {
	ID string `json:"id,omitempty"`
}

// ModelDescriptor identifies the model reported by the final assistant message
type ModelDescriptor struct {
	Name string `json:"name,omitempty"`
}

// WorkspaceState contains portable workspace facts without an absolute path
type WorkspaceState struct {
	ID string `json:"id,omitempty"`
}

// CommandEvidence identifies a process Tool call by safe digests
type CommandEvidence struct {
	CallID          string `json:"call_id"`
	ArgumentsDigest string `json:"arguments_digest"`
	OutputDigest    string `json:"output_digest,omitempty"`
	Succeeded       bool   `json:"succeeded"`
}

// CheckEvidence identifies a verification command by safe digests
type CheckEvidence struct {
	CallID          string `json:"call_id"`
	ArgumentsDigest string `json:"arguments_digest"`
	OutputDigest    string `json:"output_digest,omitempty"`
	Succeeded       bool   `json:"succeeded"`
}

// ChangeEvidence identifies a workspace mutation by safe digests
type ChangeEvidence struct {
	CallID          string `json:"call_id"`
	ArgumentsDigest string `json:"arguments_digest"`
	OutputDigest    string `json:"output_digest,omitempty"`
	Succeeded       bool   `json:"succeeded"`
}

// PolicyDecision is one host-enforced Tool authorization outcome
type PolicyDecision struct {
	CallID       string   `json:"call_id"`
	Tool         string   `json:"tool"`
	Capabilities []string `json:"capabilities,omitempty"`
	Outcome      string   `json:"outcome,omitempty"`
	Reason       string   `json:"reason,omitempty"`
}

// Artifact references one external output by digest
type Artifact struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// BundleOptions supplies host facts that are not part of RunResult
type BundleOptions struct {
	Events          []agent.Event
	ToolInvocations []ToolInvocation
	ConfigDigest    string
	WorkspaceID     string
}

// NewBundle builds an immutable Run Evidence Bundle
func NewBundle(result agent.RunResult, options BundleOptions) (Bundle, error) {
	if result.RunID == "" {
		return Bundle{}, errors.New("Evidence Run ID is required")
	}
	events, err := clonePublicEvents(options.Events)
	if err != nil {
		return Bundle{}, err
	}
	invocations := cloneInvocations(options.ToolInvocations)
	bundle := Bundle{
		Version: bundleVersion,
		Run: RunDescriptor{
			ID:          result.RunID,
			ParentRunID: result.ParentRunID,
			SessionID:   result.SessionID,
			Status:      result.Status,
		},
		Agent:          AgentDescriptor{ID: result.AgentID},
		InputDigest:    inputDigest(result.Messages),
		ConfigDigest:   options.ConfigDigest,
		WorkspaceState: WorkspaceState{ID: options.WorkspaceID},
		ToolTrace:      invocations,
		Events:         events,
		Usage:          result.Usage,
		CreatedAt:      time.Now().UTC(),
	}
	for index := len(result.Messages) - 1; index >= 0; index-- {
		if result.Messages[index].Role == agent.RoleAssistant {
			bundle.Model.Name = result.Messages[index].Model
			break
		}
	}
	for _, invocation := range invocations {
		bundle.Policies = append(bundle.Policies, PolicyDecision{
			CallID:       invocation.CallID,
			Tool:         invocation.Tool,
			Capabilities: append([]string(nil), invocation.Capabilities...),
			Outcome:      invocation.PolicyOutcome,
			Reason:       invocation.PolicyReason,
		})
		succeeded := !invocation.IsError && invocation.Error == ""
		switch invocation.Tool {
		case "run_command":
			command := CommandEvidence{
				CallID:          invocation.CallID,
				ArgumentsDigest: invocation.ArgumentsDigest,
				OutputDigest:    invocation.OutputDigest,
				Succeeded:       succeeded,
			}
			bundle.Commands = append(bundle.Commands, command)
			bundle.Checks = append(bundle.Checks, CheckEvidence(command))
		case "apply_patch":
			bundle.Changes = append(bundle.Changes, ChangeEvidence{
				CallID:          invocation.CallID,
				ArgumentsDigest: invocation.ArgumentsDigest,
				OutputDigest:    invocation.OutputDigest,
				Succeeded:       succeeded,
			})
		}
	}
	return bundle, nil
}

// Store persists and loads Run Evidence Bundles
type Store interface {
	Save(ctx context.Context, bundle Bundle) error
	Load(ctx context.Context, runID string) (Bundle, error)
	List(ctx context.Context) ([]RunDescriptor, error)
}

// ErrBundleNotFound indicates that a Run ID has no stored Evidence Bundle
var ErrBundleNotFound = errors.New("Evidence Bundle not found")

// JSONStore stores one private JSON file per Run
type JSONStore struct {
	root string
	mu   sync.Mutex
}

// NewJSONStore creates or opens an Evidence Bundle directory
func NewJSONStore(root string) (*JSONStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("Evidence Store root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve Evidence Store root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create Evidence Store root: %w", err)
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("protect Evidence Store root: %w", err)
	}
	return &JSONStore{root: absolute}, nil
}

// Save atomically writes one Evidence Bundle with mode 0600
func (store *JSONStore) Save(ctx context.Context, bundle Bundle) error {
	if ctx == nil {
		return errors.New("Evidence Save context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if bundle.Version != bundleVersion || bundle.Run.ID == "" {
		return errors.New("Evidence Bundle version and Run ID are required")
	}
	encoded, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Evidence Bundle: %w", err)
	}
	encoded = append(encoded, '\n')

	store.mu.Lock()
	defer store.mu.Unlock()
	temporary, err := os.CreateTemp(store.root, ".bundle-*.tmp")
	if err != nil {
		return fmt.Errorf("create Evidence temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure Evidence temporary file: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write Evidence Bundle: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync Evidence Bundle: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close Evidence Bundle: %w", err)
	}
	if err := os.Rename(temporaryPath, store.path(bundle.Run.ID)); err != nil {
		return fmt.Errorf("replace Evidence Bundle: %w", err)
	}
	return nil
}

// Load reads and validates one Evidence Bundle
func (store *JSONStore) Load(ctx context.Context, runID string) (Bundle, error) {
	if ctx == nil {
		return Bundle{}, errors.New("Evidence Load context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return Bundle{}, err
	}
	store.mu.Lock()
	data, err := os.ReadFile(store.path(runID))
	store.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		return Bundle{}, fmt.Errorf("%w: %q", ErrBundleNotFound, runID)
	}
	if err != nil {
		return Bundle{}, fmt.Errorf("read Evidence Bundle: %w", err)
	}
	var bundle Bundle
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		return Bundle{}, fmt.Errorf("decode Evidence Bundle: %w", err)
	}
	if bundle.Version != bundleVersion || bundle.Run.ID != runID {
		return Bundle{}, errors.New("Evidence Bundle identity or version is invalid")
	}
	return bundle, nil
}

// List returns stored Run descriptors in lexical Run ID order
func (store *JSONStore) List(ctx context.Context) ([]RunDescriptor, error) {
	if ctx == nil {
		return nil, errors.New("Evidence List context must not be nil")
	}
	store.mu.Lock()
	entries, err := os.ReadDir(store.root)
	store.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("list Evidence Store: %w", err)
	}
	var descriptors []RunDescriptor
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(store.root, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read Evidence index entry: %w", err)
		}
		var header struct {
			Version int           `json:"version"`
			Run     RunDescriptor `json:"run"`
		}
		if err := json.Unmarshal(data, &header); err != nil || header.Version != bundleVersion || header.Run.ID == "" {
			return nil, fmt.Errorf("Evidence index entry %q is invalid", entry.Name())
		}
		descriptors = append(descriptors, header.Run)
	}
	sort.Slice(descriptors, func(first, second int) bool { return descriptors[first].ID < descriptors[second].ID })
	return descriptors, nil
}

// Root returns the absolute Evidence Store directory
func (store *JSONStore) Root() string {
	if store == nil {
		return ""
	}
	return store.root
}

func (store *JSONStore) path(runID string) string {
	digest := sha256.Sum256([]byte(runID))
	return filepath.Join(store.root, hex.EncodeToString(digest[:])+".json")
}

func cloneInvocations(invocations []ToolInvocation) []ToolInvocation {
	result := make([]ToolInvocation, len(invocations))
	for index := range invocations {
		result[index] = invocations[index]
		result[index].Capabilities = append([]string(nil), invocations[index].Capabilities...)
	}
	return result
}

func clonePublicEvents(events []agent.Event) ([]agent.Event, error) {
	if events == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		return nil, fmt.Errorf("encode Run Events for Evidence: %w", err)
	}
	var result []agent.Event
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, fmt.Errorf("clone Run Events for Evidence: %w", err)
	}
	return result, nil
}

func inputDigest(messages []agent.Message) string {
	var inputs []agent.Message
	for _, message := range messages {
		if message.Role == agent.RoleUser {
			inputs = append(inputs, message)
		}
	}
	encoded, _ := json.Marshal(inputs)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}
