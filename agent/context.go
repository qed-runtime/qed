package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	prefixManifestVersion       = 1
	canonicalContextVersion     = "1"
	contextSegmentHashDomain    = "qed.context.segment.v1"
	prefixManifestHashDomain    = "qed.context.manifest.v1"
	contextInstructionsPriority = 100
	contextToolABIPriority      = 90
	contextWorldStatePriority   = 80
	contextMessagePriority      = 50
	contextMetadataPriority     = 10
)

var defaultToolInputSchema = json.RawMessage(`{"properties":{},"type":"object"}`)

type currentRunContextKey struct{}

// WithRunInfo returns a child context carrying host-authenticated Run identity
//
// Tool adapters use this when execution crosses a process or transport boundary
func WithRunInfo(ctx context.Context, info RunInfo) context.Context {
	return context.WithValue(ctx, currentRunContextKey{}, info)
}

// RunInfoFromContext returns the identity of the Run currently executing a Tool
func RunInfoFromContext(ctx context.Context) (RunInfo, bool) {
	info, ok := ctx.Value(currentRunContextKey{}).(RunInfo)
	return info, ok
}

// SegmentKind identifies the role of one provider-neutral Context Segment
type SegmentKind string

// Context Segment kinds emitted by DefaultContextCompiler
const (
	SegmentKindInstructions SegmentKind = "instructions"
	SegmentKindToolABI      SegmentKind = "tool_abi"
	SegmentKindCheckpoint   SegmentKind = "checkpoint"
	// SegmentKindCurrentWorldState identifies a host-captured canonical state suffix
	SegmentKindCurrentWorldState SegmentKind = "current_world_state"
	SegmentKindMessage           SegmentKind = "message"
	SegmentKindMetadata          SegmentKind = "metadata"
)

// StabilityClass describes how frequently a Context Segment is expected to change
type StabilityClass string

// Context stability classes ordered from long-lived to request-local
const (
	StabilityKernel     StabilityClass = "kernel"
	StabilityRelease    StabilityClass = "release"
	StabilityProject    StabilityClass = "project"
	StabilityPhase      StabilityClass = "phase"
	StabilityAppendOnly StabilityClass = "append_only"
	StabilityVolatile   StabilityClass = "volatile"
)

// ContextSegment describes one logical portion of a compiled model request
//
// ContentHash fingerprints canonical content without exposing that content.
// TokenEstimate remains zero when no tokenizer-backed estimate is available.
type ContextSegment struct {
	// ID is stable for the same logical position within a compiled request
	ID string `json:"id"`
	// Kind identifies the logical content represented by the Segment
	Kind SegmentKind `json:"kind"`
	// Version identifies the canonical rendering format
	Version string `json:"version"`
	// Stability describes the expected lifetime of the Segment
	Stability StabilityClass `json:"stability"`
	// Required reports whether the Compiler considers the Segment mandatory
	Required bool `json:"required"`
	// Priority is a relative retention hint for future context reduction
	Priority int `json:"priority"`
	// ContentHash is a domain-separated SHA-256 digest of canonical content
	ContentHash string `json:"content_hash"`
	// Bytes is the size of canonical content before hashing
	Bytes int64 `json:"bytes"`
	// TokenEstimate is an optional tokenizer-backed size estimate
	TokenEstimate int64 `json:"token_estimate,omitempty"`
}

// SegmentFingerprint is the content-free representation persisted in a Prefix Manifest
type SegmentFingerprint struct {
	// ID identifies the corresponding logical Context Segment
	ID string `json:"id"`
	// Kind identifies the logical content represented by the Segment
	Kind SegmentKind `json:"kind"`
	// Version identifies the canonical rendering format
	Version string `json:"version"`
	// ContentHash identifies the canonical Segment content
	ContentHash string `json:"content_hash"`
	// Bytes is the size of canonical content before hashing
	Bytes int64 `json:"bytes"`
	// TokenEstimate is an optional tokenizer-backed size estimate
	TokenEstimate int64 `json:"token_estimate,omitempty"`
	// Stability describes the expected lifetime of the Segment
	Stability StabilityClass `json:"stability"`
}

// PrefixManifest identifies the ordered provider-neutral Context Segments for one model request
//
// Epoch is an observability digest and must not be used as a Provider cache key.
// Provider adapters may render the logical request differently on the wire.
type PrefixManifest struct {
	// Version identifies the Prefix Manifest schema
	Version uint32 `json:"version"`
	// Provider identifies the exact Provider instance receiving the request
	Provider string `json:"provider"`
	// Model identifies the configured model when the Provider exposes it
	Model string `json:"model,omitempty"`
	// CacheFamily identifies an optional host-selected cache routing family
	CacheFamily string `json:"cache_family,omitempty"`
	// Epoch fingerprints Provider, Model, CacheFamily, and the ordered Segments
	Epoch string `json:"epoch"`
	// Segments contains ordered content-free fingerprints
	Segments []SegmentFingerprint `json:"segments"`
}

// ContextCompileRequest supplies one provider-neutral request to a Context Compiler
type ContextCompileRequest struct {
	// Provider identifies the exact Provider instance receiving the request
	Provider string
	// Model identifies the configured model when available
	Model string
	// ModelRequest contains the current instructions, messages, and Tool definitions
	ModelRequest ModelRequest
	// SessionRevision identifies the immutable Session state being compiled
	SessionRevision uint64
	// Checkpoint is the latest validated Checkpoint for this Session
	Checkpoint *ContextCheckpoint
	// Ledger is the deterministic state reconstructed from ordered Run Events
	Ledger *ContextLedger
	// Events is the exact ordered Event prefix represented by Ledger and ModelRequest
	//
	// Compacting compilers use it to preserve approval, delegation, mutation,
	// verification, and commit transaction boundaries. Callers that omit Events
	// receive the legacy Tool Call and Tool result boundary behavior.
	Events []Event
	// CurrentWorldState is the latest canonical host snapshot when configured
	//
	// Runtime appends its required volatile Context Segment after Compile returns.
	// Compilers with input limits must reserve its rendered byte size.
	CurrentWorldState *CurrentWorldState
}

// CompiledContext contains a canonical model request and its logical Segments
type CompiledContext struct {
	// ModelRequest is the request that Runtime passes to the Provider
	ModelRequest ModelRequest
	// Segments describe the ordered logical prompt without exposing its content
	Segments []ContextSegment
	// Checkpoint is the active validated Checkpoint when context was compacted
	Checkpoint *ContextCheckpoint
	// Compaction describes reductions applied to this model view
	Compaction *ContextCompactionReport
}

// ContextCompiler prepares one Provider call from current Run context
//
// Implementations must be safe for concurrent use, honor context cancellation,
// preserve ModelRequest identity and metadata, and return immutable values.
type ContextCompiler interface {
	Compile(ctx context.Context, request ContextCompileRequest) (CompiledContext, error)
}

// DefaultContextCompiler provides QED's deterministic provider-neutral v1 rendering
//
// Its zero value is ready for concurrent use.
type DefaultContextCompiler struct{}

// Compile canonicalizes Tool order and JSON values and fingerprints the logical request
func (DefaultContextCompiler) Compile(ctx context.Context, request ContextCompileRequest) (CompiledContext, error) {
	if ctx == nil {
		return CompiledContext{}, errors.New("Context Compiler context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return CompiledContext{}, err
	}

	modelRequest, err := canonicalModelRequest(request.ModelRequest)
	if err != nil {
		return CompiledContext{}, err
	}
	segments, err := contextSegments(modelRequest)
	if err != nil {
		return CompiledContext{}, err
	}
	return CompiledContext{ModelRequest: modelRequest, Segments: segments}, nil
}

// PrefixManifestOptions identifies the routing scope represented by a Prefix Manifest
type PrefixManifestOptions struct {
	// Provider identifies the exact Provider instance receiving the request
	Provider string
	// Model identifies the configured model when available
	Model string
	// CacheFamily identifies an optional host-selected cache routing family
	CacheFamily string
}

// BuildPrefixManifest validates ordered Segments and creates their observability digest
func BuildPrefixManifest(
	options PrefixManifestOptions,
	segments []ContextSegment,
) (PrefixManifest, error) {
	if strings.TrimSpace(options.Provider) != options.Provider || options.Provider == "" {
		return PrefixManifest{}, errors.New("Prefix Manifest Provider is required and must not have surrounding whitespace")
	}
	if strings.TrimSpace(options.Model) != options.Model {
		return PrefixManifest{}, errors.New("Prefix Manifest Model must not have surrounding whitespace")
	}
	if strings.TrimSpace(options.CacheFamily) != options.CacheFamily {
		return PrefixManifest{}, errors.New("Prefix Manifest Cache Family must not have surrounding whitespace")
	}
	if len(segments) == 0 {
		return PrefixManifest{}, errors.New("Prefix Manifest requires at least one Context Segment")
	}

	fingerprints := make([]SegmentFingerprint, 0, len(segments))
	identifiers := make(map[string]struct{}, len(segments))
	for index, segment := range segments {
		if err := validateContextSegment(segment); err != nil {
			return PrefixManifest{}, fmt.Errorf("validate Context Segment %d: %w", index, err)
		}
		if _, duplicate := identifiers[segment.ID]; duplicate {
			return PrefixManifest{}, fmt.Errorf("Context Segment ID %q is duplicated", segment.ID)
		}
		identifiers[segment.ID] = struct{}{}
		fingerprints = append(fingerprints, SegmentFingerprint{
			ID:            segment.ID,
			Kind:          segment.Kind,
			Version:       segment.Version,
			ContentHash:   segment.ContentHash,
			Bytes:         segment.Bytes,
			TokenEstimate: segment.TokenEstimate,
			Stability:     segment.Stability,
		})
	}

	manifest := PrefixManifest{
		Version:     prefixManifestVersion,
		Provider:    options.Provider,
		Model:       options.Model,
		CacheFamily: options.CacheFamily,
		Segments:    fingerprints,
	}
	epochSegments := make([]prefixEpochSegment, len(manifest.Segments))
	for index, segment := range manifest.Segments {
		epochSegments[index] = prefixEpochSegment{
			ID:          segment.ID,
			Kind:        segment.Kind,
			Version:     segment.Version,
			ContentHash: segment.ContentHash,
		}
	}
	epochInput, err := json.Marshal(struct {
		Domain      string               `json:"domain"`
		Version     uint32               `json:"version"`
		Provider    string               `json:"provider"`
		Model       string               `json:"model"`
		CacheFamily string               `json:"cache_family"`
		Segments    []prefixEpochSegment `json:"segments"`
	}{
		Domain:      prefixManifestHashDomain,
		Version:     manifest.Version,
		Provider:    manifest.Provider,
		Model:       manifest.Model,
		CacheFamily: manifest.CacheFamily,
		Segments:    epochSegments,
	})
	if err != nil {
		return PrefixManifest{}, fmt.Errorf("encode Prefix Manifest: %w", err)
	}
	manifest.Epoch = sha256Digest(epochInput)
	return manifest, nil
}

type prefixEpochSegment struct {
	ID          string      `json:"id"`
	Kind        SegmentKind `json:"kind"`
	Version     string      `json:"version"`
	ContentHash string      `json:"content_hash"`
}

func canonicalModelRequest(request ModelRequest) (ModelRequest, error) {
	request = cloneModelRequest(request)
	for index := range request.Tools {
		schema := request.Tools[index].InputSchema
		if len(schema) == 0 {
			schema = defaultToolInputSchema
		}
		canonical, err := canonicalJSON(schema)
		if err != nil {
			return ModelRequest{}, fmt.Errorf("canonicalize Tool %q input schema: %w", request.Tools[index].Name, err)
		}
		request.Tools[index].InputSchema = canonical
	}
	sort.SliceStable(request.Tools, func(first, second int) bool {
		return request.Tools[first].Name < request.Tools[second].Name
	})
	for messageIndex := range request.Messages {
		for callIndex := range request.Messages[messageIndex].ToolCalls {
			arguments := request.Messages[messageIndex].ToolCalls[callIndex].Arguments
			if len(arguments) == 0 {
				request.Messages[messageIndex].ToolCalls[callIndex].Arguments = json.RawMessage(`{}`)
				continue
			}
			if !json.Valid(arguments) {
				continue
			}
			canonical, err := canonicalJSON(arguments)
			if err != nil {
				return ModelRequest{}, fmt.Errorf("canonicalize Tool Call arguments: %w", err)
			}
			request.Messages[messageIndex].ToolCalls[callIndex].Arguments = canonical
		}
	}
	return request, nil
}

func contextSegments(request ModelRequest) ([]ContextSegment, error) {
	segments := make([]ContextSegment, 0, len(request.Messages)+3)
	segments = append(segments, newContextSegment(
		"instructions",
		SegmentKindInstructions,
		StabilityProject,
		contextInstructionsPriority,
		[]byte(request.Instructions),
	))

	toolContent, err := json.Marshal(contextTools(request.Tools))
	if err != nil {
		return nil, fmt.Errorf("encode Tool ABI Context Segment: %w", err)
	}
	segments = append(segments, newContextSegment(
		"tool-abi",
		SegmentKindToolABI,
		StabilityRelease,
		contextToolABIPriority,
		toolContent,
	))

	for index, message := range request.Messages {
		content, err := contextMessageContent(message)
		if err != nil {
			return nil, fmt.Errorf("encode message Context Segment %d: %w", index, err)
		}
		segments = append(segments, newContextSegment(
			fmt.Sprintf("message/%010d", index),
			SegmentKindMessage,
			StabilityAppendOnly,
			contextMessagePriority,
			content,
		))
	}

	if len(request.Metadata) > 0 {
		content, err := json.Marshal(request.Metadata)
		if err != nil {
			return nil, fmt.Errorf("encode metadata Context Segment: %w", err)
		}
		segments = append(segments, newContextSegment(
			"request-metadata",
			SegmentKindMetadata,
			StabilityVolatile,
			contextMetadataPriority,
			content,
		))
	}
	return segments, nil
}

type contextTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func contextTools(definitions []ToolDefinition) []contextTool {
	tools := make([]contextTool, len(definitions))
	for index, definition := range definitions {
		tools[index] = contextTool{
			Name:        definition.Name,
			Description: definition.Description,
			InputSchema: cloneRawMessage(definition.InputSchema),
		}
	}
	return tools
}

type contextToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments []byte `json:"arguments,omitempty"`
}

type contextProviderState struct {
	Provider string `json:"provider"`
	Data     []byte `json:"data"`
}

type contextMessage struct {
	Role          Role                  `json:"role"`
	Text          string                `json:"text,omitempty"`
	ToolCallID    string                `json:"tool_call_id,omitempty"`
	ToolName      string                `json:"tool_name,omitempty"`
	ToolIsError   bool                  `json:"tool_is_error,omitempty"`
	ToolCalls     []contextToolCall     `json:"tool_calls,omitempty"`
	ProviderState *contextProviderState `json:"provider_state,omitempty"`
}

func contextMessageContent(message Message) ([]byte, error) {
	content := contextMessage{
		Role:        message.Role,
		Text:        message.Text,
		ToolCallID:  message.ToolCallID,
		ToolName:    message.ToolName,
		ToolIsError: message.ToolIsError,
	}
	for _, call := range message.ToolCalls {
		content.ToolCalls = append(content.ToolCalls, contextToolCall{
			ID:        call.ID,
			Name:      call.Name,
			Arguments: append([]byte(nil), call.Arguments...),
		})
	}
	if message.ProviderState != nil {
		content.ProviderState = &contextProviderState{
			Provider: message.ProviderState.Provider,
			Data:     append([]byte(nil), message.ProviderState.Data...),
		}
	}
	return json.Marshal(content)
}

func newContextSegment(
	id string,
	kind SegmentKind,
	stability StabilityClass,
	priority int,
	content []byte,
) ContextSegment {
	return ContextSegment{
		ID:          id,
		Kind:        kind,
		Version:     canonicalContextVersion,
		Stability:   stability,
		Required:    true,
		Priority:    priority,
		ContentHash: contextSegmentDigest(kind, canonicalContextVersion, content),
		Bytes:       int64(len(content)),
	}
}

func contextSegmentDigest(kind SegmentKind, version string, content []byte) string {
	hash := sha256.New()
	writeHashPart(hash, []byte(contextSegmentHashDomain))
	writeHashPart(hash, []byte(kind))
	writeHashPart(hash, []byte(version))
	writeHashPart(hash, content)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

type hashWriter interface {
	Write([]byte) (int, error)
}

func writeHashPart(writer hashWriter, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = writer.Write(value)
}

func sha256Digest(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func canonicalJSON(value []byte) (json.RawMessage, error) {
	if err := rejectDuplicateJSONKeys(value); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("JSON contains more than one value")
		}
		return nil, err
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

func rejectDuplicateJSONKeys(value []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	if err := readUniqueJSONValue(decoder, ""); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON contains more than one value")
		}
		return err
	}
	return nil
}

func readUniqueJSONValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			keyPath := key
			if path != "" {
				keyPath = path + "." + key
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON key %q", keyPath)
			}
			seen[key] = struct{}{}
			if err := readUniqueJSONValue(decoder, keyPath); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("JSON object is not closed")
		}
	case '[':
		index := 0
		for decoder.More() {
			itemPath := fmt.Sprintf("%s[%d]", path, index)
			if err := readUniqueJSONValue(decoder, itemPath); err != nil {
				return err
			}
			index++
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("JSON array is not closed")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

func validateContextSegment(segment ContextSegment) error {
	if strings.TrimSpace(segment.ID) != segment.ID || segment.ID == "" {
		return errors.New("Context Segment ID is required and must not have surrounding whitespace")
	}
	if strings.TrimSpace(string(segment.Kind)) != string(segment.Kind) || segment.Kind == "" {
		return errors.New("Context Segment Kind is required and must not have surrounding whitespace")
	}
	if strings.TrimSpace(segment.Version) != segment.Version || segment.Version == "" {
		return errors.New("Context Segment Version is required and must not have surrounding whitespace")
	}
	if strings.TrimSpace(string(segment.Stability)) != string(segment.Stability) || segment.Stability == "" {
		return errors.New("Context Segment Stability is required and must not have surrounding whitespace")
	}
	if segment.Bytes < 0 {
		return errors.New("Context Segment byte count must not be negative")
	}
	if segment.TokenEstimate < 0 {
		return errors.New("Context Segment token estimate must not be negative")
	}
	if !validSHA256Digest(segment.ContentHash) {
		return errors.New("Context Segment Content Hash must be a sha256 digest")
	}
	return nil
}

func validSHA256Digest(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

func cloneModelRequest(request ModelRequest) ModelRequest {
	request.Metadata = cloneMetadata(request.Metadata)
	request.Messages = cloneMessages(request.Messages)
	request.Tools = cloneToolDefinitions(request.Tools)
	request.CachePlan = cloneCachePlanPointer(request.CachePlan)
	return request
}

func cloneContextSegments(segments []ContextSegment) []ContextSegment {
	return append([]ContextSegment(nil), segments...)
}

func clonePrefixManifest(manifest PrefixManifest) PrefixManifest {
	manifest.Segments = append([]SegmentFingerprint(nil), manifest.Segments...)
	return manifest
}
