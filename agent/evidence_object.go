package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// EvidenceScopeVersion is the scoped Evidence Object binding schema
	EvidenceScopeVersion uint32 = 1
	// EvidenceObjectAccessRecordVersion is the retrieval audit record schema
	EvidenceObjectAccessRecordVersion uint32 = 1
	// EvidenceReadCapability authorizes retrieval of ordinary scoped Evidence Objects
	EvidenceReadCapability = "evidence.read"
	// EvidenceWriteCapability authorizes persistence of scoped Evidence Objects
	EvidenceWriteCapability = "evidence.write"

	evidenceScopeDigestDomain   = "qed.evidence.scope.v1"
	evidenceBindingDigestDomain = "qed.evidence.binding.v1"
	evidencePrincipalDomain     = "qed.evidence.principal.v1"
	maximumEvidenceIdentitySize = 512
)

var (
	// ErrEvidenceAccessDenied indicates that an Evidence scope or capability did not authorize access
	ErrEvidenceAccessDenied = errors.New("Evidence Object access denied")
	// ErrEvidenceScopeRequired indicates that a scoped Evidence operation omitted its scope
	ErrEvidenceScopeRequired = errors.New("Evidence Object scope is required")
	// ErrSecretEvidenceRejected indicates that a Store cannot safely persist secret content
	ErrSecretEvidenceRejected = errors.New("secret Evidence Object storage is not protected")
)

// EvidenceSensitivity classifies the protection required for one Evidence Object
type EvidenceSensitivity string

// Evidence Object sensitivity classes
const (
	// EvidenceSensitivityPrivate permits persistence in a private access-controlled Store
	EvidenceSensitivityPrivate EvidenceSensitivity = "private"
	// EvidenceSensitivitySecret requires an encrypted Store or must not be persisted
	EvidenceSensitivitySecret EvidenceSensitivity = "secret"
)

// EvidenceScope identifies the host-authenticated isolation domain for an Object
//
// TenantID and ProfileID are required. Exactly one of SessionID or RunID is
// required. SessionID permits follow-up Runs in the same Session to share
// Evidence, while RunID isolates an ephemeral Run. Raw values are used only to
// derive ScopeDigest and are not stored in EvidenceObjectRef.
type EvidenceScope struct {
	// TenantID identifies one host-authenticated tenant or local isolation domain
	TenantID string
	// SessionID identifies one persistent Session scope
	SessionID string
	// RunID identifies one ephemeral Run scope when SessionID is empty
	RunID string
	// ProfileID identifies the execution Profile that owns the Object
	ProfileID string
}

// EvidenceAccess supplies host-authenticated identity and capabilities
//
// PrincipalID is reduced to a digest in audit records. It is never included in
// an Evidence Object reference.
type EvidenceAccess struct {
	// Scope identifies the tenant, Session or Run, and Profile boundary
	Scope EvidenceScope
	// PrincipalID identifies the application, operator, or Runtime performing access
	PrincipalID string
	// Capabilities contains the host-authorized Evidence capabilities
	Capabilities []string
}

// EvidenceScopeReference binds one Object to an opaque scope and capability set
type EvidenceScopeReference struct {
	// Version identifies the binding schema
	Version uint32 `json:"version"`
	// ScopeDigest identifies tenant, Session or Run, and Profile without exposing them
	ScopeDigest string `json:"scope_digest"`
	// BindingDigest binds object identity, metadata, scope, capabilities, and sensitivity
	BindingDigest string `json:"binding_digest"`
	// RequiredCapabilities contains the sorted capabilities required for retrieval
	RequiredCapabilities []string `json:"required_capabilities"`
	// Sensitivity identifies the required storage protection
	Sensitivity EvidenceSensitivity `json:"sensitivity"`
}

// EvidenceObjectRef identifies immutable content stored outside model context
//
// Digest is content-addressed and does not grant access by itself. Scoped
// references additionally require matching EvidenceAccess. A nil Scope is a
// legacy unscoped reference and must not be exposed to model-initiated
// retrieval.
type EvidenceObjectRef struct {
	// Digest is a sha256-prefixed digest of the exact object bytes
	Digest string `json:"digest"`
	// Bytes is the exact object size, or zero when a legacy lookup supplies only Digest
	Bytes int64 `json:"bytes"`
	// MediaType describes the stored representation and may be empty for legacy lookup
	MediaType string `json:"media_type"`
	// Scope contains the opaque authorization binding for a scoped Object
	Scope *EvidenceScopeReference `json:"scope,omitempty"`
}

// Identity returns the authorization-bound identity used to deduplicate a reference
func (reference EvidenceObjectRef) Identity() string {
	if reference.Scope != nil {
		return strings.ToLower(reference.Scope.BindingDigest)
	}
	return strings.ToLower(reference.Digest)
}

func evidenceObjectRefsEqual(first, second EvidenceObjectRef) bool {
	if first.Digest != second.Digest || first.Bytes != second.Bytes || first.MediaType != second.MediaType {
		return false
	}
	if first.Scope == nil || second.Scope == nil {
		return first.Scope == nil && second.Scope == nil
	}
	return first.Scope.Version == second.Scope.Version &&
		first.Scope.ScopeDigest == second.Scope.ScopeDigest &&
		first.Scope.BindingDigest == second.Scope.BindingDigest &&
		first.Scope.Sensitivity == second.Scope.Sensitivity &&
		equalEvidenceCapabilities(first.Scope.RequiredCapabilities, second.Scope.RequiredCapabilities)
}

func cloneEvidenceObjectRef(reference EvidenceObjectRef) EvidenceObjectRef {
	if reference.Scope == nil {
		return reference
	}
	scope := *reference.Scope
	scope.RequiredCapabilities = append([]string(nil), reference.Scope.RequiredCapabilities...)
	reference.Scope = &scope
	return reference
}

func cloneEvidenceObjectRefs(references []EvidenceObjectRef) []EvidenceObjectRef {
	if references == nil {
		return nil
	}
	result := make([]EvidenceObjectRef, len(references))
	for index, reference := range references {
		result[index] = cloneEvidenceObjectRef(reference)
	}
	return result
}

// EvidenceObjectStore persists immutable content used by Context Checkpoints
//
// Implementations must be safe for concurrent use. PutObject must be
// idempotent for identical content and GetObject must verify content identity.
// These methods support legacy unscoped references. Model-facing retrieval
// must use ScopedEvidenceObjectStore.
type EvidenceObjectStore interface {
	PutObject(ctx context.Context, mediaType string, content []byte) (EvidenceObjectRef, error)
	GetObject(ctx context.Context, reference EvidenceObjectRef) ([]byte, error)
}

// EvidenceObjectPutRequest describes one scoped immutable write
type EvidenceObjectPutRequest struct {
	// Access is the host-authenticated writer identity
	Access EvidenceAccess
	// MediaType describes Content
	MediaType string
	// Content contains the exact immutable bytes
	Content []byte
	// RequiredCapabilities are required for every later retrieval
	RequiredCapabilities []string
	// Sensitivity selects the required storage protection
	Sensitivity EvidenceSensitivity
}

// EvidenceObjectGetRequest describes one scoped retrieval
type EvidenceObjectGetRequest struct {
	// Access is the host-authenticated reader identity
	Access EvidenceAccess
	// Reference is the complete scoped Object reference
	Reference EvidenceObjectRef
}

// ScopedEvidenceObjectStore enforces tenant, Session or Run, Profile, and capability boundaries
//
// Implementations that accept EvidenceSensitivitySecret must protect content at
// rest. Built-in stores reject secret writes.
type ScopedEvidenceObjectStore interface {
	EvidenceObjectStore
	PutObjectScoped(ctx context.Context, request EvidenceObjectPutRequest) (EvidenceObjectRef, error)
	GetObjectScoped(ctx context.Context, request EvidenceObjectGetRequest) ([]byte, error)
}

// EvidenceObjectAdminStore permits an explicitly privileged local operator read
//
// Admin reads bypass tenant and capability authorization but remain audited.
// Implementations may omit this interface when no administrative retrieval is
// safe or required.
type EvidenceObjectAdminStore interface {
	GetObjectAdmin(ctx context.Context, reference EvidenceObjectRef, principalID string) ([]byte, error)
}

// EvidenceObjectAccessOperation identifies one audited scoped operation
type EvidenceObjectAccessOperation string

// Audited Evidence Object operations
const (
	EvidenceObjectAccessPut      EvidenceObjectAccessOperation = "put"
	EvidenceObjectAccessGet      EvidenceObjectAccessOperation = "get"
	EvidenceObjectAccessAdminGet EvidenceObjectAccessOperation = "admin_get"
)

// EvidenceObjectAccessOutcome identifies one audited access result
type EvidenceObjectAccessOutcome string

// Audited Evidence Object access outcomes
const (
	EvidenceObjectAccessAllowed  EvidenceObjectAccessOutcome = "allowed"
	EvidenceObjectAccessDenied   EvidenceObjectAccessOutcome = "denied"
	EvidenceObjectAccessNotFound EvidenceObjectAccessOutcome = "not_found"
	EvidenceObjectAccessError    EvidenceObjectAccessOutcome = "error"
)

// EvidenceObjectAccessRecord is a content-free scoped access audit entry
//
// It contains no tenant, Session, Run, Profile, principal, capability, or
// object content text. Digests remain sensitive metadata and must receive the
// same storage protection as other Evidence.
type EvidenceObjectAccessRecord struct {
	// Version identifies the audit schema
	Version uint32 `json:"version"`
	// Time is the Store-observed operation time
	Time time.Time `json:"time"`
	// Operation identifies the attempted action
	Operation EvidenceObjectAccessOperation `json:"operation"`
	// Outcome identifies whether access succeeded and why it did not
	Outcome EvidenceObjectAccessOutcome `json:"outcome"`
	// PrincipalDigest identifies the caller without exposing PrincipalID
	PrincipalDigest string `json:"principal_digest"`
	// ScopeDigest identifies the expected isolation domain
	ScopeDigest string `json:"scope_digest"`
	// AccessScopeDigest identifies the caller's attempted isolation domain
	//
	// It is empty only for an explicitly privileged administrative read.
	AccessScopeDigest string `json:"access_scope_digest,omitempty"`
	// BindingDigest identifies the authorization-bound Object reference
	BindingDigest string `json:"binding_digest"`
	// ObjectDigest identifies exact content without copying it
	ObjectDigest string `json:"object_digest"`
	// Bytes is the referenced exact content size
	Bytes int64 `json:"bytes"`
}

// EvidenceObjectAccessRecorder persists one immutable access audit record
//
// Retrieval must fail closed when recording an allowed access fails.
type EvidenceObjectAccessRecorder interface {
	RecordEvidenceObjectAccess(ctx context.Context, record EvidenceObjectAccessRecord) error
}

// EvidenceObjectAccessLog exposes isolated access audit snapshots
type EvidenceObjectAccessLog interface {
	EvidenceObjectAccessRecords(ctx context.Context) ([]EvidenceObjectAccessRecord, error)
}

type evidenceAccessContextKey struct{}
type evidenceTenantContextKey struct{}

// WithEvidenceTenant returns a child context carrying a host-authenticated tenant identity
//
// Runtime validates the value and combines it with RuntimeEvidenceAccess. Use
// this at an authenticated server boundary when one Runtime serves multiple
// tenants.
func WithEvidenceTenant(ctx context.Context, tenantID string) context.Context {
	if ctx == nil {
		panic("nil Context")
	}
	return context.WithValue(ctx, evidenceTenantContextKey{}, tenantID)
}

// EvidenceTenantFromContext returns the host-authenticated tenant identity
func EvidenceTenantFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	tenantID, ok := ctx.Value(evidenceTenantContextKey{}).(string)
	return tenantID, ok
}

// WithEvidenceAccess returns a child context carrying host-authenticated Evidence access
func WithEvidenceAccess(ctx context.Context, access EvidenceAccess) context.Context {
	if ctx == nil {
		panic("nil Context")
	}
	return context.WithValue(ctx, evidenceAccessContextKey{}, cloneEvidenceAccess(access))
}

// EvidenceAccessFromContext returns an isolated host-authenticated Evidence access value
func EvidenceAccessFromContext(ctx context.Context) (EvidenceAccess, bool) {
	if ctx == nil {
		return EvidenceAccess{}, false
	}
	access, ok := ctx.Value(evidenceAccessContextKey{}).(EvidenceAccess)
	if !ok {
		return EvidenceAccess{}, false
	}
	return cloneEvidenceAccess(access), true
}

// ValidateEvidenceScope validates one concrete isolation domain
func ValidateEvidenceScope(scope EvidenceScope) error {
	if err := validateEvidenceIdentity("tenant ID", scope.TenantID); err != nil {
		return err
	}
	if err := validateEvidenceIdentity("Profile ID", scope.ProfileID); err != nil {
		return err
	}
	if (scope.SessionID == "") == (scope.RunID == "") {
		return errors.New("Evidence scope requires exactly one Session ID or Run ID")
	}
	if scope.SessionID != "" {
		if err := validateEvidenceIdentity("Session ID", scope.SessionID); err != nil {
			return err
		}
	}
	if scope.RunID != "" {
		if err := validateEvidenceIdentity("Run ID", scope.RunID); err != nil {
			return err
		}
	}
	return nil
}

// ValidateEvidenceAccess validates one concrete principal and scope
func ValidateEvidenceAccess(access EvidenceAccess) error {
	if err := ValidateEvidenceScope(access.Scope); err != nil {
		return err
	}
	if err := validateEvidenceIdentity("principal ID", access.PrincipalID); err != nil {
		return err
	}
	_, err := normalizeEvidenceCapabilities(access.Capabilities)
	return err
}

// BindEvidenceObjectReference creates an opaque authorization binding
func BindEvidenceObjectReference(
	reference EvidenceObjectRef,
	access EvidenceAccess,
	requiredCapabilities []string,
	sensitivity EvidenceSensitivity,
) (EvidenceObjectRef, error) {
	if reference.Scope != nil {
		return EvidenceObjectRef{}, errors.New("Evidence Object reference is already scoped")
	}
	if err := validateUnscopedEvidenceReference(reference); err != nil {
		return EvidenceObjectRef{}, err
	}
	if err := ValidateEvidenceAccess(access); err != nil {
		return EvidenceObjectRef{}, err
	}
	required, err := normalizeEvidenceCapabilities(requiredCapabilities)
	if err != nil {
		return EvidenceObjectRef{}, err
	}
	if len(required) == 0 {
		return EvidenceObjectRef{}, errors.New("scoped Evidence Object requires a retrieval capability")
	}
	if err := validateEvidenceSensitivity(sensitivity); err != nil {
		return EvidenceObjectRef{}, err
	}
	scopeDigest := evidenceScopeDigest(access.Scope)
	reference.Scope = &EvidenceScopeReference{
		Version:              EvidenceScopeVersion,
		ScopeDigest:          scopeDigest,
		RequiredCapabilities: required,
		Sensitivity:          sensitivity,
	}
	reference.Scope.BindingDigest = evidenceBindingDigest(reference)
	return reference, nil
}

// ValidateEvidenceObjectRef validates content identity and an optional scope binding
func ValidateEvidenceObjectRef(reference EvidenceObjectRef) error {
	if err := validateUnscopedEvidenceReference(EvidenceObjectRef{
		Digest: reference.Digest, Bytes: reference.Bytes, MediaType: reference.MediaType,
	}); err != nil {
		return err
	}
	if reference.Scope == nil {
		return nil
	}
	if reference.Scope.Version != EvidenceScopeVersion {
		return fmt.Errorf("Evidence scope version = %d, want %d", reference.Scope.Version, EvidenceScopeVersion)
	}
	if !validEvidenceDigest(reference.Scope.ScopeDigest) || !validEvidenceDigest(reference.Scope.BindingDigest) {
		return errors.New("Evidence scope contains an invalid digest")
	}
	required, err := normalizeEvidenceCapabilities(reference.Scope.RequiredCapabilities)
	if err != nil {
		return err
	}
	if len(required) == 0 || !equalEvidenceCapabilities(required, reference.Scope.RequiredCapabilities) {
		return errors.New("Evidence scope capabilities are empty or not canonical")
	}
	if err := validateEvidenceSensitivity(reference.Scope.Sensitivity); err != nil {
		return err
	}
	if reference.Scope.BindingDigest != evidenceBindingDigest(reference) {
		return errors.New("Evidence scope binding does not match Object metadata")
	}
	return nil
}

// AuthorizeEvidenceObjectAccess validates scope and required capabilities
func AuthorizeEvidenceObjectAccess(reference EvidenceObjectRef, access EvidenceAccess) error {
	if err := ValidateEvidenceObjectRef(reference); err != nil {
		return err
	}
	if reference.Scope == nil {
		return ErrEvidenceScopeRequired
	}
	if err := ValidateEvidenceAccess(access); err != nil {
		return fmt.Errorf("%w: %v", ErrEvidenceAccessDenied, err)
	}
	if reference.Scope.ScopeDigest != evidenceScopeDigest(access.Scope) {
		return fmt.Errorf("%w: scope mismatch", ErrEvidenceAccessDenied)
	}
	available := make(map[string]struct{}, len(access.Capabilities))
	for _, capability := range access.Capabilities {
		available[capability] = struct{}{}
	}
	for _, capability := range reference.Scope.RequiredCapabilities {
		if _, ok := available[capability]; !ok {
			return fmt.Errorf("%w: required capability is unavailable", ErrEvidenceAccessDenied)
		}
	}
	return nil
}

// EvidencePrincipalDigest returns the content-free identity used in audit records
func EvidencePrincipalDigest(principalID string) (string, error) {
	if err := validateEvidenceIdentity("principal ID", principalID); err != nil {
		return "", err
	}
	return evidenceDigest(evidencePrincipalDomain, []byte(principalID)), nil
}

// NewEvidenceObjectAccessRecord builds one validated content-free audit entry
func NewEvidenceObjectAccessRecord(
	at time.Time,
	operation EvidenceObjectAccessOperation,
	outcome EvidenceObjectAccessOutcome,
	reference EvidenceObjectRef,
	access EvidenceAccess,
) (EvidenceObjectAccessRecord, error) {
	if at.IsZero() {
		return EvidenceObjectAccessRecord{}, errors.New("Evidence access time is required")
	}
	if err := validateEvidenceAccessOperation(operation); err != nil {
		return EvidenceObjectAccessRecord{}, err
	}
	if err := validateEvidenceAccessOutcome(outcome); err != nil {
		return EvidenceObjectAccessRecord{}, err
	}
	if err := ValidateEvidenceObjectRef(reference); err != nil {
		return EvidenceObjectAccessRecord{}, err
	}
	if reference.Scope == nil {
		return EvidenceObjectAccessRecord{}, ErrEvidenceScopeRequired
	}
	principalDigest, err := EvidencePrincipalDigest(access.PrincipalID)
	if err != nil {
		return EvidenceObjectAccessRecord{}, err
	}
	accessScopeDigest := ""
	if operation != EvidenceObjectAccessAdminGet {
		if err := ValidateEvidenceScope(access.Scope); err != nil {
			return EvidenceObjectAccessRecord{}, err
		}
		accessScopeDigest = evidenceScopeDigest(access.Scope)
	}
	return EvidenceObjectAccessRecord{
		Version:           EvidenceObjectAccessRecordVersion,
		Time:              at.UTC(),
		Operation:         operation,
		Outcome:           outcome,
		PrincipalDigest:   principalDigest,
		ScopeDigest:       reference.Scope.ScopeDigest,
		AccessScopeDigest: accessScopeDigest,
		BindingDigest:     reference.Scope.BindingDigest,
		ObjectDigest:      reference.Digest,
		Bytes:             reference.Bytes,
	}, nil
}

// ValidateEvidenceObjectAccessRecord validates one persisted audit entry
func ValidateEvidenceObjectAccessRecord(record EvidenceObjectAccessRecord) error {
	if record.Version != EvidenceObjectAccessRecordVersion {
		return fmt.Errorf("Evidence access record version = %d, want %d", record.Version, EvidenceObjectAccessRecordVersion)
	}
	if record.Time.IsZero() {
		return errors.New("Evidence access record time is required")
	}
	if err := validateEvidenceAccessOperation(record.Operation); err != nil {
		return err
	}
	if err := validateEvidenceAccessOutcome(record.Outcome); err != nil {
		return err
	}
	for _, digest := range []string{
		record.PrincipalDigest, record.ScopeDigest, record.BindingDigest, record.ObjectDigest,
	} {
		if !validEvidenceDigest(digest) {
			return errors.New("Evidence access record contains an invalid digest")
		}
	}
	if record.Operation == EvidenceObjectAccessAdminGet {
		if record.AccessScopeDigest != "" {
			return errors.New("administrative Evidence access record contains an access scope")
		}
	} else if !validEvidenceDigest(record.AccessScopeDigest) {
		return errors.New("Evidence access record contains an invalid access scope digest")
	}
	if record.Bytes < 0 {
		return errors.New("Evidence access record contains a negative size")
	}
	return nil
}

func evidenceScopeDigest(scope EvidenceScope) string {
	encoded, _ := json.Marshal(struct {
		TenantID  string `json:"tenant_id"`
		SessionID string `json:"session_id,omitempty"`
		RunID     string `json:"run_id,omitempty"`
		ProfileID string `json:"profile_id"`
	}{
		TenantID: scope.TenantID, SessionID: scope.SessionID,
		RunID: scope.RunID, ProfileID: scope.ProfileID,
	})
	return evidenceDigest(evidenceScopeDigestDomain, encoded)
}

func evidenceBindingDigest(reference EvidenceObjectRef) string {
	scope := reference.Scope
	encoded, _ := json.Marshal(struct {
		Digest               string              `json:"digest"`
		Bytes                int64               `json:"bytes"`
		MediaType            string              `json:"media_type"`
		ScopeDigest          string              `json:"scope_digest"`
		RequiredCapabilities []string            `json:"required_capabilities"`
		Sensitivity          EvidenceSensitivity `json:"sensitivity"`
	}{
		Digest: reference.Digest, Bytes: reference.Bytes, MediaType: reference.MediaType,
		ScopeDigest: scope.ScopeDigest, RequiredCapabilities: scope.RequiredCapabilities,
		Sensitivity: scope.Sensitivity,
	})
	return evidenceDigest(evidenceBindingDigestDomain, encoded)
}

func evidenceDigest(domain string, value []byte) string {
	hash := sha256.New()
	writeHashPart(hash, []byte(domain))
	writeHashPart(hash, value)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func validateUnscopedEvidenceReference(reference EvidenceObjectRef) error {
	if !validEvidenceDigest(reference.Digest) {
		return errors.New("Evidence Object digest is invalid")
	}
	if reference.Bytes < 0 {
		return errors.New("Evidence Object size must not be negative")
	}
	if reference.MediaType != "" &&
		(strings.TrimSpace(reference.MediaType) != reference.MediaType || !utf8.ValidString(reference.MediaType) ||
			strings.IndexByte(reference.MediaType, 0) >= 0) {
		return errors.New("Evidence Object media type is invalid")
	}
	return nil
}

func validEvidenceDigest(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

func validateEvidenceIdentity(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("Evidence %s is required and must not have surrounding whitespace", name)
	}
	if len(value) > maximumEvidenceIdentitySize || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("Evidence %s is invalid", name)
	}
	return nil
}

func normalizeEvidenceCapabilities(capabilities []string) ([]string, error) {
	normalized := append([]string(nil), capabilities...)
	for _, capability := range normalized {
		if err := validateEvidenceCapability(capability); err != nil {
			return nil, err
		}
	}
	sort.Strings(normalized)
	for index := 1; index < len(normalized); index++ {
		if normalized[index] == normalized[index-1] {
			return nil, fmt.Errorf("Evidence capability %q is duplicated", normalized[index])
		}
	}
	return normalized, nil
}

func validateEvidenceCapability(capability string) error {
	if capability == "" || strings.TrimSpace(capability) != capability {
		return errors.New("Evidence capability is required and must not have surrounding whitespace")
	}
	for _, segment := range strings.Split(capability, ".") {
		if segment == "" {
			return errors.New("Evidence capability must contain non-empty dot-separated segments")
		}
		for _, character := range segment {
			if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
				(character >= '0' && character <= '9') || character == '_' || character == '-' {
				continue
			}
			return errors.New("Evidence capability contains an unsupported character")
		}
	}
	return nil
}

func equalEvidenceCapabilities(first, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func validateEvidenceSensitivity(sensitivity EvidenceSensitivity) error {
	switch sensitivity {
	case EvidenceSensitivityPrivate, EvidenceSensitivitySecret:
		return nil
	default:
		return fmt.Errorf("unsupported Evidence sensitivity %q", sensitivity)
	}
}

func validateEvidenceAccessOperation(operation EvidenceObjectAccessOperation) error {
	switch operation {
	case EvidenceObjectAccessPut, EvidenceObjectAccessGet, EvidenceObjectAccessAdminGet:
		return nil
	default:
		return fmt.Errorf("unsupported Evidence access operation %q", operation)
	}
}

func validateEvidenceAccessOutcome(outcome EvidenceObjectAccessOutcome) error {
	switch outcome {
	case EvidenceObjectAccessAllowed, EvidenceObjectAccessDenied,
		EvidenceObjectAccessNotFound, EvidenceObjectAccessError:
		return nil
	default:
		return fmt.Errorf("unsupported Evidence access outcome %q", outcome)
	}
}

func cloneEvidenceAccess(access EvidenceAccess) EvidenceAccess {
	access.Capabilities = append([]string(nil), access.Capabilities...)
	return access
}
