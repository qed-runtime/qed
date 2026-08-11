package evidence

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/qed-runtime/qed/agent"
)

const (
	objectDirectoryName = "objects"
	maximumObjectBytes  = 64 << 20
)

var (
	// ErrObjectNotFound indicates that an Evidence Object digest is unavailable
	ErrObjectNotFound = errors.New("Evidence Object not found")
	// ErrObjectCorrupt indicates that stored bytes do not match their reference
	ErrObjectCorrupt = errors.New("Evidence Object is corrupt")
)

// ObjectRef is the content-addressed reference used by Evidence Object Stores
type ObjectRef = agent.EvidenceObjectRef

// ObjectStore persists immutable content used by Context Checkpoints
type ObjectStore = agent.EvidenceObjectStore

// MemoryObjectStore keeps immutable Evidence Objects in process memory
type MemoryObjectStore struct {
	mu      sync.RWMutex
	objects map[string][]byte
}

// NewMemoryObjectStore creates an empty concurrency-safe Object Store
func NewMemoryObjectStore() *MemoryObjectStore {
	return &MemoryObjectStore{objects: make(map[string][]byte)}
}

// PutObject stores one immutable object by its SHA-256 digest
func (store *MemoryObjectStore) PutObject(
	ctx context.Context,
	mediaType string,
	content []byte,
) (ObjectRef, error) {
	if store == nil {
		return ObjectRef{}, errors.New("Evidence Object Store must not be nil")
	}
	if err := validateObjectInput(ctx, mediaType, content); err != nil {
		return ObjectRef{}, err
	}
	reference := newObjectRef(mediaType, content)
	store.mu.Lock()
	if store.objects == nil {
		store.objects = make(map[string][]byte)
	}
	if _, exists := store.objects[reference.Digest]; !exists {
		store.objects[reference.Digest] = append([]byte(nil), content...)
	}
	store.mu.Unlock()
	return reference, nil
}

// GetObject loads and verifies one immutable object
func (store *MemoryObjectStore) GetObject(ctx context.Context, reference ObjectRef) ([]byte, error) {
	if store == nil {
		return nil, errors.New("Evidence Object Store must not be nil")
	}
	if err := validateObjectReference(ctx, reference); err != nil {
		return nil, err
	}
	store.mu.RLock()
	content, exists := store.objects[reference.Digest]
	content = append([]byte(nil), content...)
	store.mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrObjectNotFound, reference.Digest)
	}
	if err := verifyObject(reference, content); err != nil {
		return nil, err
	}
	return content, nil
}

// PutObject atomically stores one immutable object below the JSON Store root
func (store *JSONStore) PutObject(
	ctx context.Context,
	mediaType string,
	content []byte,
) (ObjectRef, error) {
	if store == nil {
		return ObjectRef{}, errors.New("Evidence Object Store must not be nil")
	}
	if err := validateObjectInput(ctx, mediaType, content); err != nil {
		return ObjectRef{}, err
	}
	reference := newObjectRef(mediaType, content)
	name, err := objectName(reference.Digest)
	if err != nil {
		return ObjectRef{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	root, err := os.OpenRoot(store.root)
	if err != nil {
		return ObjectRef{}, fmt.Errorf("open Evidence Store root: %w", err)
	}
	defer root.Close()
	if err := ensureObjectDirectory(root); err != nil {
		return ObjectRef{}, err
	}
	path := filepath.Join(objectDirectoryName, name)
	if existing, openErr := root.Open(path); openErr == nil {
		data, readErr := readBoundedObject(existing)
		closeErr := existing.Close()
		if readErr != nil {
			return ObjectRef{}, readErr
		}
		if closeErr != nil {
			return ObjectRef{}, fmt.Errorf("close Evidence Object: %w", closeErr)
		}
		if err := verifyObject(reference, data); err != nil {
			return ObjectRef{}, err
		}
		return reference, nil
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return ObjectRef{}, fmt.Errorf("open Evidence Object: %w", openErr)
	}

	temporaryName, temporary, err := createObjectTemporary(root)
	if err != nil {
		return ObjectRef{}, err
	}
	defer root.Remove(temporaryName)
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return ObjectRef{}, fmt.Errorf("write Evidence Object: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return ObjectRef{}, fmt.Errorf("sync Evidence Object: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return ObjectRef{}, fmt.Errorf("close Evidence Object: %w", err)
	}
	if err := root.Rename(temporaryName, path); err != nil {
		return ObjectRef{}, fmt.Errorf("publish Evidence Object: %w", err)
	}
	return reference, nil
}

// GetObject loads and verifies one immutable object below the JSON Store root
func (store *JSONStore) GetObject(ctx context.Context, reference ObjectRef) ([]byte, error) {
	if store == nil {
		return nil, errors.New("Evidence Object Store must not be nil")
	}
	if err := validateObjectReference(ctx, reference); err != nil {
		return nil, err
	}
	name, err := objectName(reference.Digest)
	if err != nil {
		return nil, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	root, err := os.OpenRoot(store.root)
	if err != nil {
		return nil, fmt.Errorf("open Evidence Store root: %w", err)
	}
	defer root.Close()
	file, err := root.Open(filepath.Join(objectDirectoryName, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrObjectNotFound, reference.Digest)
	}
	if err != nil {
		return nil, fmt.Errorf("open Evidence Object: %w", err)
	}
	data, readErr := readBoundedObject(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close Evidence Object: %w", closeErr)
	}
	if err := verifyObject(reference, data); err != nil {
		return nil, err
	}
	return data, nil
}

func validateObjectInput(ctx context.Context, mediaType string, content []byte) error {
	if ctx == nil {
		return errors.New("Evidence Object context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateMediaType(mediaType); err != nil {
		return err
	}
	if len(content) > maximumObjectBytes {
		return fmt.Errorf("Evidence Object exceeds %d bytes", maximumObjectBytes)
	}
	return nil
}

func validateObjectReference(ctx context.Context, reference ObjectRef) error {
	if ctx == nil {
		return errors.New("Evidence Object context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := objectName(reference.Digest); err != nil {
		return err
	}
	if reference.Bytes < 0 || reference.Bytes > maximumObjectBytes {
		return fmt.Errorf("Evidence Object size must be between 0 and %d bytes", maximumObjectBytes)
	}
	if reference.MediaType != "" {
		return validateMediaType(reference.MediaType)
	}
	return nil
}

func validateMediaType(mediaType string) error {
	if strings.TrimSpace(mediaType) != mediaType || mediaType == "" {
		return errors.New("Evidence Object media type is required and must not have surrounding whitespace")
	}
	if !utf8.ValidString(mediaType) || strings.IndexByte(mediaType, 0) >= 0 || len(mediaType) > 256 {
		return errors.New("Evidence Object media type is invalid")
	}
	if _, _, err := mime.ParseMediaType(mediaType); err != nil {
		return errors.New("Evidence Object media type is invalid")
	}
	return nil
}

func newObjectRef(mediaType string, content []byte) ObjectRef {
	digest := sha256.Sum256(content)
	return ObjectRef{
		Digest:    "sha256:" + hex.EncodeToString(digest[:]),
		Bytes:     int64(len(content)),
		MediaType: mediaType,
	}
}

func objectName(digest string) (string, error) {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) || len(digest) != len(prefix)+sha256.Size*2 {
		return "", errors.New("Evidence Object digest must be a sha256 digest")
	}
	hexDigest := strings.TrimPrefix(digest, prefix)
	if _, err := hex.DecodeString(hexDigest); err != nil {
		return "", errors.New("Evidence Object digest must be a sha256 digest")
	}
	return hexDigest + ".blob", nil
}

func verifyObject(reference ObjectRef, content []byte) error {
	actual := newObjectRef(reference.MediaType, content)
	if actual.Digest != reference.Digest {
		return fmt.Errorf("%w: digest mismatch", ErrObjectCorrupt)
	}
	if reference.Bytes != 0 && actual.Bytes != reference.Bytes {
		return fmt.Errorf("%w: size = %d, want %d", ErrObjectCorrupt, actual.Bytes, reference.Bytes)
	}
	return nil
}

func ensureObjectDirectory(root *os.Root) error {
	if err := root.Mkdir(objectDirectoryName, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create Evidence Object directory: %w", err)
	}
	info, err := root.Lstat(objectDirectoryName)
	if err != nil {
		return fmt.Errorf("inspect Evidence Object directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Evidence Object directory must be a real directory")
	}
	if err := root.Chmod(objectDirectoryName, 0o700); err != nil {
		return fmt.Errorf("protect Evidence Object directory: %w", err)
	}
	return nil
}

func createObjectTemporary(root *os.Root) (string, *os.File, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		name := filepath.Join(objectDirectoryName, ".object-"+hex.EncodeToString(random[:])+".tmp")
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return name, file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, fmt.Errorf("create Evidence Object temporary file: %w", err)
		}
	}
	return "", nil, errors.New("could not allocate Evidence Object temporary file")
}

func readBoundedObject(file *os.File) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect Evidence Object: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maximumObjectBytes {
		return nil, fmt.Errorf("%w: invalid object file", ErrObjectCorrupt)
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumObjectBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Evidence Object: %w", err)
	}
	if len(data) > maximumObjectBytes {
		return nil, fmt.Errorf("%w: object exceeds %d bytes", ErrObjectCorrupt, maximumObjectBytes)
	}
	return data, nil
}
