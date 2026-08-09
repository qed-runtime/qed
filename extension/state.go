package extension

import (
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
	"unicode/utf8"
)

const maximumStateBytes = 1 << 20

// ErrStateNotFound indicates that an Extension state key does not exist
var ErrStateNotFound = errors.New("Extension state not found")

// StateStore persists host-owned state under an Extension and scope namespace
//
// Implementations must be safe for concurrent use and return isolated byte slices
type StateStore interface {
	Get(ctx context.Context, extensionID, scope, key string) ([]byte, error)
	Set(ctx context.Context, extensionID, scope, key string, value []byte) error
}

// MemoryStateStore is a process-local concurrent StateStore
type MemoryStateStore struct {
	mu     sync.RWMutex
	values map[stateKey][]byte
}

type stateKey struct {
	extensionID string
	scope       string
	key         string
}

// NewMemoryStateStore constructs an empty in-memory Extension State Store
func NewMemoryStateStore() *MemoryStateStore {
	return &MemoryStateStore{values: make(map[stateKey][]byte)}
}

// Get returns an isolated copy of one state value
func (store *MemoryStateStore) Get(ctx context.Context, extensionID, scope, key string) ([]byte, error) {
	if err := validateStateRequest(ctx, extensionID, scope, key); err != nil {
		return nil, err
	}
	store.mu.RLock()
	value, ok := store.values[stateKey{extensionID: extensionID, scope: scope, key: key}]
	store.mu.RUnlock()
	if !ok {
		return nil, ErrStateNotFound
	}
	return append([]byte(nil), value...), nil
}

// Set stores an isolated copy of one bounded state value
func (store *MemoryStateStore) Set(ctx context.Context, extensionID, scope, key string, value []byte) error {
	if err := validateStateRequest(ctx, extensionID, scope, key); err != nil {
		return err
	}
	if len(value) > maximumStateBytes {
		return fmt.Errorf("Extension state exceeds %d bytes", maximumStateBytes)
	}
	store.mu.Lock()
	store.values[stateKey{extensionID: extensionID, scope: scope, key: key}] = append([]byte(nil), value...)
	store.mu.Unlock()
	return nil
}

// JSONStateStore persists each namespaced state value as one private JSON file
//
// Writes are atomic within one filesystem and serialized within the process
type JSONStateStore struct {
	root string
	mu   sync.RWMutex
}

type stateEnvelope struct {
	ExtensionID string `json:"extension_id"`
	Scope       string `json:"scope"`
	Key         string `json:"key"`
	Value       []byte `json:"value"`
}

// NewJSONStateStore opens or creates one private Extension State Store directory
func NewJSONStateStore(root string) (*JSONStateStore, error) {
	if strings.TrimSpace(root) == "" || strings.IndexByte(root, 0) >= 0 {
		return nil, errors.New("Extension State Store path is required and must not contain NUL")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve Extension State Store path: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create Extension State Store: %w", err)
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("protect Extension State Store: %w", err)
	}
	return &JSONStateStore{root: filepath.Clean(absolute)}, nil
}

// Get reads and verifies one namespaced state value
func (store *JSONStateStore) Get(ctx context.Context, extensionID, scope, key string) ([]byte, error) {
	if err := validateStateRequest(ctx, extensionID, scope, key); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	file, err := os.Open(store.path(extensionID, scope, key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrStateNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open Extension state: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumStateBytes*2+1))
	if err != nil {
		return nil, fmt.Errorf("read Extension state: %w", err)
	}
	if len(data) > maximumStateBytes*2 {
		return nil, errors.New("encoded Extension state exceeds its limit")
	}
	var envelope stateEnvelope
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode Extension state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("Extension state contains trailing JSON")
	}
	if envelope.ExtensionID != extensionID || envelope.Scope != scope || envelope.Key != key {
		return nil, errors.New("Extension state namespace does not match its path")
	}
	if len(envelope.Value) > maximumStateBytes {
		return nil, fmt.Errorf("Extension state exceeds %d bytes", maximumStateBytes)
	}
	return append([]byte(nil), envelope.Value...), nil
}

// Set atomically writes one private namespaced state value
func (store *JSONStateStore) Set(ctx context.Context, extensionID, scope, key string, value []byte) error {
	if err := validateStateRequest(ctx, extensionID, scope, key); err != nil {
		return err
	}
	if len(value) > maximumStateBytes {
		return fmt.Errorf("Extension state exceeds %d bytes", maximumStateBytes)
	}
	encoded, err := json.Marshal(stateEnvelope{
		ExtensionID: extensionID,
		Scope:       scope,
		Key:         key,
		Value:       append([]byte(nil), value...),
	})
	if err != nil {
		return fmt.Errorf("encode Extension state: %w", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	temporary, err := os.CreateTemp(store.root, ".qed-extension-state-*")
	if err != nil {
		return fmt.Errorf("create temporary Extension state: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("protect temporary Extension state: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		cleanup()
		return fmt.Errorf("write temporary Extension state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temporary Extension state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("close temporary Extension state: %w", err)
	}
	if err := os.Rename(temporaryPath, store.path(extensionID, scope, key)); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("replace Extension state: %w", err)
	}
	return nil
}

func (store *JSONStateStore) path(extensionID, scope, key string) string {
	digest := sha256.Sum256([]byte(extensionID + "\x00" + scope + "\x00" + key))
	return filepath.Join(store.root, hex.EncodeToString(digest[:])+".json")
}

func validateStateRequest(ctx context.Context, extensionID, scope, key string) error {
	if ctx == nil {
		return errors.New("Extension State Store context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"Extension ID": extensionID,
		"state scope":  scope,
		"state key":    key,
	} {
		if value == "" || strings.TrimSpace(value) != value || strings.IndexByte(value, 0) >= 0 || !utf8.ValidString(value) {
			return fmt.Errorf("%s is required, valid UTF-8, and must not have surrounding whitespace or NUL", name)
		}
		if len(value) > 256 {
			return fmt.Errorf("%s exceeds 256 bytes", name)
		}
	}
	return nil
}
