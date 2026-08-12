package evidence

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/qed-runtime/qed/agent"
)

const (
	objectDirectoryName       = "objects"
	scopedObjectDirectoryName = "scoped-objects"
	objectAccessLogName       = "object-access.jsonl"
	maximumObjectBytes        = 64 << 20
	maximumAccessRecordBytes  = 64 << 10
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

// ScopedObjectStore enforces authorization-bound immutable Object access
type ScopedObjectStore = agent.ScopedEvidenceObjectStore

// ObjectAdminStore permits explicitly privileged audited local retrieval
type ObjectAdminStore = agent.EvidenceObjectAdminStore

// ObjectAccessLog exposes isolated scoped access audit snapshots
type ObjectAccessLog = agent.EvidenceObjectAccessLog

// MemoryObjectStore keeps immutable Evidence Objects in process memory
type MemoryObjectStore struct {
	mu            sync.RWMutex
	objects       map[string][]byte
	scopedObjects map[string][]byte
	accessRecords []agent.EvidenceObjectAccessRecord
}

// NewMemoryObjectStore creates an empty concurrency-safe Object Store
func NewMemoryObjectStore() *MemoryObjectStore {
	return &MemoryObjectStore{
		objects:       make(map[string][]byte),
		scopedObjects: make(map[string][]byte),
	}
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
	if reference.Scope != nil {
		return nil, agent.ErrEvidenceScopeRequired
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

// PutObjectScoped stores one authorization-bound immutable object
func (store *MemoryObjectStore) PutObjectScoped(
	ctx context.Context,
	request agent.EvidenceObjectPutRequest,
) (ObjectRef, error) {
	if store == nil {
		return ObjectRef{}, errors.New("Evidence Object Store must not be nil")
	}
	reference, err := prepareScopedObjectPut(ctx, request)
	if err != nil {
		return ObjectRef{}, joinAccessAuditError(
			err,
			recordObjectAccess(ctx, store, agent.EvidenceObjectAccessPut, accessFailureOutcome(err), reference, request.Access),
		)
	}

	store.mu.Lock()
	if store.scopedObjects == nil {
		store.scopedObjects = make(map[string][]byte)
	}
	if _, exists := store.scopedObjects[reference.Identity()]; !exists {
		store.scopedObjects[reference.Identity()] = append([]byte(nil), request.Content...)
	}
	store.mu.Unlock()
	if err := recordObjectAccess(
		ctx,
		store,
		agent.EvidenceObjectAccessPut,
		agent.EvidenceObjectAccessAllowed,
		reference,
		request.Access,
	); err != nil {
		return ObjectRef{}, err
	}
	return reference, nil
}

// GetObjectScoped authorizes, loads, verifies, and audits one immutable object
func (store *MemoryObjectStore) GetObjectScoped(
	ctx context.Context,
	request agent.EvidenceObjectGetRequest,
) ([]byte, error) {
	if store == nil {
		return nil, errors.New("Evidence Object Store must not be nil")
	}
	if err := prepareScopedObjectGet(ctx, request); err != nil {
		return nil, joinAccessAuditError(
			err,
			recordObjectAccess(ctx, store, agent.EvidenceObjectAccessGet, accessFailureOutcome(err), request.Reference, request.Access),
		)
	}
	store.mu.RLock()
	content, exists := store.scopedObjects[request.Reference.Identity()]
	content = append([]byte(nil), content...)
	store.mu.RUnlock()
	if !exists {
		err := fmt.Errorf("%w: %s", ErrObjectNotFound, request.Reference.Identity())
		return nil, joinAccessAuditError(err, recordObjectAccess(
			ctx, store, agent.EvidenceObjectAccessGet, agent.EvidenceObjectAccessNotFound, request.Reference, request.Access,
		))
	}
	if err := verifyObject(request.Reference, content); err != nil {
		return nil, joinAccessAuditError(err, recordObjectAccess(
			ctx, store, agent.EvidenceObjectAccessGet, agent.EvidenceObjectAccessError, request.Reference, request.Access,
		))
	}
	if err := recordObjectAccess(
		ctx, store, agent.EvidenceObjectAccessGet, agent.EvidenceObjectAccessAllowed, request.Reference, request.Access,
	); err != nil {
		return nil, err
	}
	return content, nil
}

// GetObjectAdmin performs one explicitly privileged and audited local read
func (store *MemoryObjectStore) GetObjectAdmin(
	ctx context.Context,
	reference ObjectRef,
	principalID string,
) ([]byte, error) {
	if store == nil {
		return nil, errors.New("Evidence Object Store must not be nil")
	}
	access := agent.EvidenceAccess{PrincipalID: principalID}
	if err := prepareAdminObjectGet(ctx, reference, principalID); err != nil {
		return nil, err
	}
	store.mu.RLock()
	content, exists := store.scopedObjects[reference.Identity()]
	content = append([]byte(nil), content...)
	store.mu.RUnlock()
	if !exists {
		err := fmt.Errorf("%w: %s", ErrObjectNotFound, reference.Identity())
		return nil, joinAccessAuditError(err, recordObjectAccess(
			ctx, store, agent.EvidenceObjectAccessAdminGet, agent.EvidenceObjectAccessNotFound, reference, access,
		))
	}
	if err := verifyObject(reference, content); err != nil {
		return nil, joinAccessAuditError(err, recordObjectAccess(
			ctx, store, agent.EvidenceObjectAccessAdminGet, agent.EvidenceObjectAccessError, reference, access,
		))
	}
	if err := recordObjectAccess(
		ctx, store, agent.EvidenceObjectAccessAdminGet, agent.EvidenceObjectAccessAllowed, reference, access,
	); err != nil {
		return nil, err
	}
	return content, nil
}

// RecordEvidenceObjectAccess appends one immutable validated access record
func (store *MemoryObjectStore) RecordEvidenceObjectAccess(
	ctx context.Context,
	record agent.EvidenceObjectAccessRecord,
) error {
	if store == nil {
		return errors.New("Evidence Object Store must not be nil")
	}
	if ctx == nil {
		return errors.New("Evidence access audit context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := agent.ValidateEvidenceObjectAccessRecord(record); err != nil {
		return err
	}
	store.mu.Lock()
	store.accessRecords = append(store.accessRecords, record)
	store.mu.Unlock()
	return nil
}

// EvidenceObjectAccessRecords returns an isolated access audit snapshot
func (store *MemoryObjectStore) EvidenceObjectAccessRecords(
	ctx context.Context,
) ([]agent.EvidenceObjectAccessRecord, error) {
	if store == nil {
		return nil, errors.New("Evidence Object Store must not be nil")
	}
	if ctx == nil {
		return nil, errors.New("Evidence access audit context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.RLock()
	records := append([]agent.EvidenceObjectAccessRecord(nil), store.accessRecords...)
	store.mu.RUnlock()
	return records, nil
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
	if reference.Scope != nil {
		return nil, agent.ErrEvidenceScopeRequired
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

// PutObjectScoped atomically stores one authorization-bound immutable object
func (store *JSONStore) PutObjectScoped(
	ctx context.Context,
	request agent.EvidenceObjectPutRequest,
) (ObjectRef, error) {
	if store == nil {
		return ObjectRef{}, errors.New("Evidence Object Store must not be nil")
	}
	reference, err := prepareScopedObjectPut(ctx, request)
	if err != nil {
		return ObjectRef{}, joinAccessAuditError(
			err,
			recordObjectAccess(ctx, store, agent.EvidenceObjectAccessPut, accessFailureOutcome(err), reference, request.Access),
		)
	}
	if err := store.storeScopedObject(ctx, reference, request.Content); err != nil {
		return ObjectRef{}, joinAccessAuditError(err, recordObjectAccess(
			ctx, store, agent.EvidenceObjectAccessPut, agent.EvidenceObjectAccessError, reference, request.Access,
		))
	}
	if err := recordObjectAccess(
		ctx, store, agent.EvidenceObjectAccessPut, agent.EvidenceObjectAccessAllowed, reference, request.Access,
	); err != nil {
		return ObjectRef{}, err
	}
	return reference, nil
}

// GetObjectScoped authorizes, loads, verifies, and audits one immutable object
func (store *JSONStore) GetObjectScoped(
	ctx context.Context,
	request agent.EvidenceObjectGetRequest,
) ([]byte, error) {
	if store == nil {
		return nil, errors.New("Evidence Object Store must not be nil")
	}
	if err := prepareScopedObjectGet(ctx, request); err != nil {
		return nil, joinAccessAuditError(
			err,
			recordObjectAccess(ctx, store, agent.EvidenceObjectAccessGet, accessFailureOutcome(err), request.Reference, request.Access),
		)
	}
	content, err := store.loadScopedObject(ctx, request.Reference)
	if err != nil {
		outcome := agent.EvidenceObjectAccessError
		if errors.Is(err, ErrObjectNotFound) {
			outcome = agent.EvidenceObjectAccessNotFound
		}
		return nil, joinAccessAuditError(err, recordObjectAccess(
			ctx, store, agent.EvidenceObjectAccessGet, outcome, request.Reference, request.Access,
		))
	}
	if err := recordObjectAccess(
		ctx, store, agent.EvidenceObjectAccessGet, agent.EvidenceObjectAccessAllowed, request.Reference, request.Access,
	); err != nil {
		return nil, err
	}
	return content, nil
}

// GetObjectAdmin performs one explicitly privileged and audited local read
func (store *JSONStore) GetObjectAdmin(
	ctx context.Context,
	reference ObjectRef,
	principalID string,
) ([]byte, error) {
	if store == nil {
		return nil, errors.New("Evidence Object Store must not be nil")
	}
	if err := prepareAdminObjectGet(ctx, reference, principalID); err != nil {
		return nil, err
	}
	access := agent.EvidenceAccess{PrincipalID: principalID}
	content, err := store.loadScopedObject(ctx, reference)
	if err != nil {
		outcome := agent.EvidenceObjectAccessError
		if errors.Is(err, ErrObjectNotFound) {
			outcome = agent.EvidenceObjectAccessNotFound
		}
		return nil, joinAccessAuditError(err, recordObjectAccess(
			ctx, store, agent.EvidenceObjectAccessAdminGet, outcome, reference, access,
		))
	}
	if err := recordObjectAccess(
		ctx, store, agent.EvidenceObjectAccessAdminGet, agent.EvidenceObjectAccessAllowed, reference, access,
	); err != nil {
		return nil, err
	}
	return content, nil
}

// RecordEvidenceObjectAccess durably appends one validated access record
func (store *JSONStore) RecordEvidenceObjectAccess(
	ctx context.Context,
	record agent.EvidenceObjectAccessRecord,
) error {
	if store == nil {
		return errors.New("Evidence Object Store must not be nil")
	}
	if ctx == nil {
		return errors.New("Evidence access audit context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := agent.ValidateEvidenceObjectAccessRecord(record); err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode Evidence access audit record: %w", err)
	}
	if len(encoded) > maximumAccessRecordBytes {
		return errors.New("Evidence access audit record is too large")
	}
	encoded = append(encoded, '\n')

	store.mu.Lock()
	defer store.mu.Unlock()
	root, err := os.OpenRoot(store.root)
	if err != nil {
		return fmt.Errorf("open Evidence Store root: %w", err)
	}
	defer root.Close()
	file, err := root.OpenFile(objectAccessLogName, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open Evidence access audit log: %w", err)
	}
	if err := validateOpenedRegularFile(root, objectAccessLogName, file); err != nil {
		_ = file.Close()
		return fmt.Errorf("inspect Evidence access audit log: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("protect Evidence access audit log: %w", err)
	}
	if written, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return fmt.Errorf("write Evidence access audit log: %w", err)
	} else if written != len(encoded) {
		_ = file.Close()
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync Evidence access audit log: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close Evidence access audit log: %w", err)
	}
	return nil
}

// EvidenceObjectAccessRecords loads and validates the durable access audit log
func (store *JSONStore) EvidenceObjectAccessRecords(
	ctx context.Context,
) ([]agent.EvidenceObjectAccessRecord, error) {
	if store == nil {
		return nil, errors.New("Evidence Object Store must not be nil")
	}
	if ctx == nil {
		return nil, errors.New("Evidence access audit context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	root, err := os.OpenRoot(store.root)
	if err != nil {
		store.mu.Unlock()
		return nil, fmt.Errorf("open Evidence Store root: %w", err)
	}
	file, err := root.Open(objectAccessLogName)
	if errors.Is(err, os.ErrNotExist) {
		_ = root.Close()
		store.mu.Unlock()
		return nil, nil
	}
	if err != nil {
		_ = root.Close()
		store.mu.Unlock()
		return nil, fmt.Errorf("open Evidence access audit log: %w", err)
	}
	if statErr := validateOpenedRegularFile(root, objectAccessLogName, file); statErr != nil {
		_ = file.Close()
		_ = root.Close()
		store.mu.Unlock()
		return nil, fmt.Errorf("inspect Evidence access audit log: %w", statErr)
	}

	var records []agent.EvidenceObjectAccessRecord
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), maximumAccessRecordBytes+1)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			_ = root.Close()
			store.mu.Unlock()
			return nil, err
		}
		var record agent.EvidenceObjectAccessRecord
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil {
			_ = file.Close()
			_ = root.Close()
			store.mu.Unlock()
			return nil, fmt.Errorf("decode Evidence access audit record: %w", err)
		}
		var trailing json.RawMessage
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			_ = file.Close()
			_ = root.Close()
			store.mu.Unlock()
			if err == nil {
				return nil, errors.New("Evidence access audit record contains trailing JSON")
			}
			return nil, fmt.Errorf("decode Evidence access audit record trailer: %w", err)
		}
		if err := agent.ValidateEvidenceObjectAccessRecord(record); err != nil {
			_ = file.Close()
			_ = root.Close()
			store.mu.Unlock()
			return nil, fmt.Errorf("validate Evidence access audit record: %w", err)
		}
		records = append(records, record)
	}
	scanErr := scanner.Err()
	closeErr := file.Close()
	rootCloseErr := root.Close()
	store.mu.Unlock()
	if scanErr != nil {
		return nil, fmt.Errorf("read Evidence access audit log: %w", scanErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close Evidence access audit log: %w", closeErr)
	}
	if rootCloseErr != nil {
		return nil, fmt.Errorf("close Evidence Store root: %w", rootCloseErr)
	}
	return records, nil
}

func (store *JSONStore) storeScopedObject(ctx context.Context, reference ObjectRef, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name, err := objectName(reference.Identity())
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	root, err := os.OpenRoot(store.root)
	if err != nil {
		return fmt.Errorf("open Evidence Store root: %w", err)
	}
	defer root.Close()
	if err := ensureEvidenceObjectDirectory(root, scopedObjectDirectoryName); err != nil {
		return err
	}
	path := filepath.Join(scopedObjectDirectoryName, name)
	if existing, openErr := root.Open(path); openErr == nil {
		data, readErr := readBoundedObject(existing)
		closeErr := existing.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return fmt.Errorf("close scoped Evidence Object: %w", closeErr)
		}
		return verifyObject(reference, data)
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return fmt.Errorf("open scoped Evidence Object: %w", openErr)
	}
	temporaryName, temporary, err := createEvidenceObjectTemporary(root, scopedObjectDirectoryName)
	if err != nil {
		return err
	}
	defer root.Remove(temporaryName)
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write scoped Evidence Object: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync scoped Evidence Object: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close scoped Evidence Object: %w", err)
	}
	if err := root.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("publish scoped Evidence Object: %w", err)
	}
	return nil
}

func (store *JSONStore) loadScopedObject(ctx context.Context, reference ObjectRef) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := objectName(reference.Identity())
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
	file, err := root.Open(filepath.Join(scopedObjectDirectoryName, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrObjectNotFound, reference.Identity())
	}
	if err != nil {
		return nil, fmt.Errorf("open scoped Evidence Object: %w", err)
	}
	data, readErr := readBoundedObject(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close scoped Evidence Object: %w", closeErr)
	}
	if err := verifyObject(reference, data); err != nil {
		return nil, err
	}
	return data, nil
}

func prepareScopedObjectPut(
	ctx context.Context,
	request agent.EvidenceObjectPutRequest,
) (ObjectRef, error) {
	if err := validateObjectInput(ctx, request.MediaType, request.Content); err != nil {
		return ObjectRef{}, err
	}
	if err := agent.ValidateEvidenceAccess(request.Access); err != nil {
		return ObjectRef{}, err
	}
	reference, err := agent.BindEvidenceObjectReference(
		newObjectRef(request.MediaType, request.Content),
		request.Access,
		request.RequiredCapabilities,
		request.Sensitivity,
	)
	if err != nil {
		return ObjectRef{}, err
	}
	if !hasEvidenceCapability(request.Access.Capabilities, agent.EvidenceWriteCapability) {
		return reference, fmt.Errorf("%w: %s is required", agent.ErrEvidenceAccessDenied, agent.EvidenceWriteCapability)
	}
	if request.Sensitivity == agent.EvidenceSensitivitySecret {
		return reference, agent.ErrSecretEvidenceRejected
	}
	return reference, nil
}

func prepareScopedObjectGet(ctx context.Context, request agent.EvidenceObjectGetRequest) error {
	if err := validateObjectReference(ctx, request.Reference); err != nil {
		return err
	}
	if request.Reference.Scope == nil {
		return agent.ErrEvidenceScopeRequired
	}
	if err := agent.ValidateEvidenceObjectRef(request.Reference); err != nil {
		return err
	}
	return agent.AuthorizeEvidenceObjectAccess(request.Reference, request.Access)
}

func prepareAdminObjectGet(ctx context.Context, reference ObjectRef, principalID string) error {
	if err := validateObjectReference(ctx, reference); err != nil {
		return err
	}
	if reference.Scope == nil {
		return agent.ErrEvidenceScopeRequired
	}
	if err := agent.ValidateEvidenceObjectRef(reference); err != nil {
		return err
	}
	_, err := agent.EvidencePrincipalDigest(principalID)
	return err
}

func hasEvidenceCapability(capabilities []string, required string) bool {
	for _, capability := range capabilities {
		if capability == required {
			return true
		}
	}
	return false
}

func accessFailureOutcome(err error) agent.EvidenceObjectAccessOutcome {
	if errors.Is(err, agent.ErrEvidenceAccessDenied) ||
		errors.Is(err, agent.ErrEvidenceScopeRequired) ||
		errors.Is(err, agent.ErrSecretEvidenceRejected) {
		return agent.EvidenceObjectAccessDenied
	}
	return agent.EvidenceObjectAccessError
}

func recordObjectAccess(
	ctx context.Context,
	recorder agent.EvidenceObjectAccessRecorder,
	operation agent.EvidenceObjectAccessOperation,
	outcome agent.EvidenceObjectAccessOutcome,
	reference ObjectRef,
	access agent.EvidenceAccess,
) error {
	if reference.Scope == nil || strings.TrimSpace(access.PrincipalID) == "" {
		return nil
	}
	record, err := agent.NewEvidenceObjectAccessRecord(
		time.Now().UTC(), operation, outcome, reference, access,
	)
	if err != nil {
		return err
	}
	if err := recorder.RecordEvidenceObjectAccess(ctx, record); err != nil {
		return fmt.Errorf("record Evidence Object access: %w", err)
	}
	return nil
}

func joinAccessAuditError(operationErr, auditErr error) error {
	if auditErr == nil {
		return operationErr
	}
	if operationErr == nil {
		return auditErr
	}
	return errors.Join(operationErr, auditErr)
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
	return ensureEvidenceObjectDirectory(root, objectDirectoryName)
}

func ensureEvidenceObjectDirectory(root *os.Root, directory string) error {
	if err := root.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create Evidence Object directory: %w", err)
	}
	info, err := root.Lstat(directory)
	if err != nil {
		return fmt.Errorf("inspect Evidence Object directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Evidence Object directory must be a real directory")
	}
	if err := root.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("protect Evidence Object directory: %w", err)
	}
	return nil
}

func createObjectTemporary(root *os.Root) (string, *os.File, error) {
	return createEvidenceObjectTemporary(root, objectDirectoryName)
}

func createEvidenceObjectTemporary(root *os.Root, directory string) (string, *os.File, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		name := filepath.Join(directory, ".object-"+hex.EncodeToString(random[:])+".tmp")
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

func validateOpenedRegularFile(root *os.Root, name string, file *os.File) error {
	pathInfo, err := root.Lstat(name)
	if err != nil {
		return err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		return err
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 ||
		!openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return errors.New("file must be a real regular file")
	}
	return nil
}
