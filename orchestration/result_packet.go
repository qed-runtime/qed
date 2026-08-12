package orchestration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/internal/jsonstrict"
)

const (
	// ResultPacketVersion is the schema version emitted for reduced subagent results
	ResultPacketVersion uint32 = 1
	// MaxResultPacketFacts bounds semantic facts returned by one subagent
	MaxResultPacketFacts = 4096
	// MaxResultPacketArtifacts bounds durable artifacts returned by one subagent
	MaxResultPacketArtifacts = 4096
	// MaxResultPacketExecutions bounds execution outcomes returned by one subagent
	MaxResultPacketExecutions = 1024
	// MaxResultPacketEvidence bounds Evidence Object references returned by one subagent
	MaxResultPacketEvidence = 1024
	// MaxResultPacketBytes bounds the complete encoded packet
	MaxResultPacketBytes = 1 << 20
	// MaxResultProfileStateBytes bounds opaque Profile-owned JSON in one packet
	MaxResultProfileStateBytes = 512 << 10

	maximumResultFactBytes   = 16 << 10
	maximumResultStringBytes = 4096
	resultPacketDigestDomain = "qed.subagent.result.packet.v1"
	resultFactIDDomain       = "qed.subagent.result.fact.v1"
	resultArtifactIDDomain   = "qed.subagent.result.artifact.v1"
	resultExecutionIDDomain  = "qed.subagent.result.execution.v1"
)

var (
	// ErrInvalidResultPacket indicates malformed, unbounded, or unverifiable subagent output
	ErrInvalidResultPacket = errors.New("invalid subagent Result Packet")
)

// ResultExecutionState identifies the terminal outcome of reduced work
type ResultExecutionState string

// Result execution states
const (
	// ResultExecutionSucceeded identifies completed successful work
	ResultExecutionSucceeded ResultExecutionState = "succeeded"
	// ResultExecutionFailed identifies completed failed work
	ResultExecutionFailed ResultExecutionState = "failed"
	// ResultExecutionCanceled identifies canceled work
	ResultExecutionCanceled ResultExecutionState = "canceled"
)

// ResultFact is one Profile-classified claim with exact child Event provenance
//
// Value is content-bearing untrusted JSON. Kind supplies a Profile-owned type
// name without adding that domain schema to Runtime Core. ID is assigned by
// AgentRegistry from the canonical fact contents.
type ResultFact struct {
	// ID is the domain-separated content identity assigned by AgentRegistry
	ID string `json:"id"`
	// Kind is a stable Profile-owned fact type
	Kind string `json:"kind"`
	// Value contains one bounded canonical JSON value
	Value json.RawMessage `json:"value"`
	// Sources identifies child Run Events supporting this claim
	Sources []agent.ContextLedgerEventRef `json:"sources"`
	// Evidence identifies packet Evidence Objects supporting this claim
	Evidence []string `json:"evidence,omitempty"`
}

// ResultArtifact identifies immutable output derived from one child Run
//
// Digest identifies exact artifact bytes. ID is assigned by AgentRegistry from
// the canonical artifact contents.
type ResultArtifact struct {
	// ID is the domain-separated content identity assigned by AgentRegistry
	ID string `json:"id"`
	// Kind is a stable Profile-owned artifact type
	Kind string `json:"kind"`
	// Name identifies the artifact within its Profile domain
	Name string `json:"name"`
	// Digest identifies exact artifact bytes
	Digest string `json:"digest"`
	// Bytes is the exact artifact size
	Bytes int64 `json:"bytes"`
	// MediaType describes the artifact representation
	MediaType string `json:"media_type"`
	// Sources identifies child Run Events that produced or observed the artifact
	Sources []agent.ContextLedgerEventRef `json:"sources"`
	// Evidence identifies packet Evidence Objects containing or supporting it
	Evidence []string `json:"evidence,omitempty"`
}

// ResultExecution identifies one terminal child execution outcome
//
// Kind and Name remain Profile-owned while State, digests, and Sources provide
// a provider-neutral envelope. ID is assigned by AgentRegistry.
type ResultExecution struct {
	// ID is the domain-separated content identity assigned by AgentRegistry
	ID string `json:"id"`
	// Kind is a stable Profile-owned execution type
	Kind string `json:"kind"`
	// Name identifies the Provider, Tool, check, or domain operation
	Name string `json:"name"`
	// State is the terminal provider-neutral outcome
	State ResultExecutionState `json:"state"`
	// RunID identifies the child Run that performed the work
	RunID string `json:"run_id"`
	// CallID identifies the attempt within RunID when available
	CallID string `json:"call_id,omitempty"`
	// Attempt is the one-based Provider or domain attempt when available
	Attempt int `json:"attempt,omitempty"`
	// ArgumentsDigest identifies exact inputs without copying them into the packet
	ArgumentsDigest string `json:"arguments_digest,omitempty"`
	// OutputDigest identifies exact output without copying it into the packet
	OutputDigest string `json:"output_digest,omitempty"`
	// ErrorCode contains a provider-neutral or Profile-owned failure classification
	ErrorCode string `json:"error_code,omitempty"`
	// Sources identifies child Run Events establishing this outcome
	Sources []agent.ContextLedgerEventRef `json:"sources"`
	// Evidence identifies packet Evidence Objects supporting this outcome
	Evidence []string `json:"evidence,omitempty"`
}

// ResultPacket is one validated, content-addressed subagent result projection
//
// Source binds the projection to the complete terminal child Context Ledger.
// Facts and ProfileState are content-bearing untrusted data. Artifacts,
// Executions, and Evidence retain exact identities and child Event provenance.
type ResultPacket struct {
	// Version identifies the packet schema
	Version uint32 `json:"version"`
	// AgentID identifies the registered child Agent
	AgentID string `json:"agent_id"`
	// RunID identifies the terminal child Run
	RunID string `json:"run_id"`
	// ParentRunID identifies the delegating parent Run when available
	ParentRunID string `json:"parent_run_id,omitempty"`
	// Source binds this packet to the terminal child Context Ledger
	Source agent.ContextLedgerReference `json:"source"`
	// Facts contains Profile-classified claims in reducer order
	Facts []ResultFact `json:"facts,omitempty"`
	// Artifacts contains immutable outputs in reducer order
	Artifacts []ResultArtifact `json:"artifacts,omitempty"`
	// Executions contains terminal work outcomes in reducer order
	Executions []ResultExecution `json:"executions,omitempty"`
	// Evidence contains scoped exact-content references in canonical identity order
	Evidence []agent.EvidenceObjectRef `json:"evidence,omitempty"`
	// ProfileState contains bounded canonical JSON owned by the configured Profile
	//
	// Runtime Core does not interpret this field. It is included in the
	// model-facing subagent Tool result and must not contain secrets.
	ProfileState json.RawMessage `json:"profile_state,omitempty"`
	// Digest identifies the complete canonical packet except this field
	Digest string `json:"digest"`
}

// ResultReductionRequest supplies one successful terminal child Run to a Profile reducer
//
// Result ownership remains with AgentRegistry. Reducers must not mutate its
// slices or nested values and must be safe for concurrent use.
type ResultReductionRequest struct {
	// AgentID identifies the registered child Agent
	AgentID string
	// Result is the complete successful terminal child Run
	Result agent.RunResult
}

// ResultReduction contains Profile-owned additions before packet validation
//
// Reducers leave entry IDs empty. AgentRegistry canonicalizes JSON, validates
// every source and Evidence reference against Result, assigns IDs, and computes
// the packet Digest.
type ResultReduction struct {
	// Facts contains Profile-classified claims in stable priority order
	Facts []ResultFact
	// Artifacts contains immutable outputs in stable priority order
	Artifacts []ResultArtifact
	// Executions contains terminal work outcomes in stable priority order
	Executions []ResultExecution
	// Evidence contains exact references already exposed by the child Run
	Evidence []agent.EvidenceObjectRef
	// ProfileState contains one bounded JSON object owned by the Profile
	ProfileState json.RawMessage
}

// ResultReducer projects one successful child Run into a provider-neutral packet
//
// Implementations are Profile policy, must be deterministic for the same
// request, must honor cancellation, and must be safe for concurrent use.
type ResultReducer interface {
	ReduceResult(ctx context.Context, request ResultReductionRequest) (ResultReduction, error)
}

// ResultReducerFunc adapts a function to ResultReducer
type ResultReducerFunc func(context.Context, ResultReductionRequest) (ResultReduction, error)

// ReduceResult calls reducer
func (reducer ResultReducerFunc) ReduceResult(
	ctx context.Context,
	request ResultReductionRequest,
) (ResultReduction, error) {
	return reducer(ctx, request)
}

// LedgerResultReducer projects deterministic child Ledger artifacts and executions
//
// It deliberately creates no semantic Facts and no ProfileState. Profile
// reducers may start with this result and add domain-owned projections.
type LedgerResultReducer struct{}

// ReduceResult returns current-Run Artifact and Execution Ledger entries plus
// exact Evidence already exposed by the terminal Run
func (LedgerResultReducer) ReduceResult(
	ctx context.Context,
	request ResultReductionRequest,
) (ResultReduction, error) {
	if ctx == nil {
		return ResultReduction{}, errors.New("Result reducer context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return ResultReduction{}, err
	}
	ledger := request.Result.ContextLedger
	if ledger == nil {
		return ResultReduction{}, errors.New("Result reducer requires a terminal Context Ledger")
	}
	reduction := ResultReduction{}
	for _, artifact := range ledger.Artifacts {
		if !resultSourcesContainRun(artifact.Sources, request.Result.RunID) {
			continue
		}
		reduction.Artifacts = append(reduction.Artifacts, ResultArtifact{
			Kind:      string(artifact.Kind),
			Name:      artifact.Name,
			Digest:    artifact.Digest,
			Bytes:     artifact.Bytes,
			MediaType: artifact.MediaType,
			Sources:   append([]agent.ContextLedgerEventRef(nil), artifact.Sources...),
		})
	}
	for _, execution := range ledger.Executions {
		if execution.RunID != request.Result.RunID || execution.State == agent.ExecutionLedgerPending {
			continue
		}
		state, err := resultExecutionState(execution.State)
		if err != nil {
			return ResultReduction{}, err
		}
		reduction.Executions = append(reduction.Executions, ResultExecution{
			Kind:            string(execution.Kind),
			Name:            execution.Name,
			State:           state,
			RunID:           execution.RunID,
			CallID:          execution.CallID,
			Attempt:         execution.Attempt,
			ArgumentsDigest: execution.ArgumentsDigest,
			OutputDigest:    execution.OutputDigest,
			ErrorCode:       execution.ErrorCode,
			Sources:         append([]agent.ContextLedgerEventRef(nil), execution.Sources...),
		})
	}
	reduction.Evidence = resultAvailableEvidence(request.Result)
	return reduction, nil
}

// ValidateResultPacket verifies packet identity, bounds, canonical form, and provenance
func ValidateResultPacket(ctx context.Context, packet ResultPacket, result agent.RunResult) error {
	if ctx == nil {
		return errors.New("Result Packet validation context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := normalizeResultPacket(ctx, packet, result, true)
	return err
}

func buildResultPacket(
	ctx context.Context,
	agentID string,
	result agent.RunResult,
	reducer ResultReducer,
) (ResultPacket, error) {
	if ctx == nil {
		return ResultPacket{}, errors.New("Result Packet context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return ResultPacket{}, err
	}
	if reducer == nil {
		reducer = LedgerResultReducer{}
	}
	isolated, err := cloneResultForReducer(result)
	if err != nil {
		return ResultPacket{}, fmt.Errorf("isolate subagent result: %w", err)
	}
	reduction, err := reducer.ReduceResult(ctx, ResultReductionRequest{AgentID: agentID, Result: isolated})
	if err != nil {
		return ResultPacket{}, fmt.Errorf("reduce subagent result: %w", err)
	}
	packet := ResultPacket{
		Version:      ResultPacketVersion,
		AgentID:      agentID,
		RunID:        result.RunID,
		ParentRunID:  result.ParentRunID,
		Facts:        reduction.Facts,
		Artifacts:    reduction.Artifacts,
		Executions:   reduction.Executions,
		Evidence:     reduction.Evidence,
		ProfileState: reduction.ProfileState,
	}
	if result.ContextLedger != nil {
		packet.Source = result.ContextLedger.Reference()
	}
	return normalizeResultPacket(ctx, packet, result, false)
}

func cloneResultForReducer(result agent.RunResult) (agent.RunResult, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return agent.RunResult{}, err
	}
	var cloned agent.RunResult
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return agent.RunResult{}, err
	}
	return cloned, nil
}

func normalizeResultPacket(
	ctx context.Context,
	packet ResultPacket,
	result agent.RunResult,
	verifyIdentity bool,
) (ResultPacket, error) {
	fail := func(format string, values ...any) (ResultPacket, error) {
		return ResultPacket{}, fmt.Errorf("%w: %s", ErrInvalidResultPacket, fmt.Sprintf(format, values...))
	}
	if err := ctx.Err(); err != nil {
		return ResultPacket{}, err
	}
	if result.Status != agent.RunStatusCompleted || result.RunID == "" || result.ContextLedger == nil {
		return fail("source Run is not a successful terminal Run with a Context Ledger")
	}
	if packet.Version != ResultPacketVersion {
		return fail("version = %d, want %d", packet.Version, ResultPacketVersion)
	}
	if packet.AgentID == "" || packet.AgentID != result.AgentID || packet.RunID != result.RunID ||
		packet.ParentRunID != result.ParentRunID {
		return fail("Run identity does not match the source result")
	}
	if packet.Source != result.ContextLedger.Reference() {
		return fail("source does not match the terminal Context Ledger")
	}
	if len(packet.Facts) > MaxResultPacketFacts || len(packet.Artifacts) > MaxResultPacketArtifacts ||
		len(packet.Executions) > MaxResultPacketExecutions || len(packet.Evidence) > MaxResultPacketEvidence {
		return fail("entry count exceeds a packet limit")
	}

	eventOrder := make(map[agent.ContextLedgerEventRef]int, len(result.ContextLedger.Sources))
	for index, source := range result.ContextLedger.Sources {
		ref := source.ContextLedgerEventRef
		if _, duplicate := eventOrder[ref]; duplicate {
			return fail("terminal Context Ledger contains a duplicate source")
		}
		eventOrder[ref] = index
	}
	availableEvidence := resultAvailableEvidence(result)
	availableByIdentity := make(map[string]agent.EvidenceObjectRef, len(availableEvidence))
	for _, reference := range availableEvidence {
		availableByIdentity[reference.Identity()] = reference
	}

	normalized := packet
	normalized.Facts = make([]ResultFact, len(packet.Facts))
	normalized.Artifacts = make([]ResultArtifact, len(packet.Artifacts))
	normalized.Executions = make([]ResultExecution, len(packet.Executions))
	normalized.Evidence = make([]agent.EvidenceObjectRef, len(packet.Evidence))
	seenEvidence := make(map[string]struct{}, len(packet.Evidence))
	for index, reference := range packet.Evidence {
		if err := agent.ValidateEvidenceObjectRef(reference); err != nil {
			return fail("Evidence %d is invalid", index)
		}
		identity := reference.Identity()
		available, exists := availableByIdentity[identity]
		if !exists || !resultEvidenceEqual(reference, available) {
			return fail("Evidence %d is not exposed by the source Run", index)
		}
		if _, duplicate := seenEvidence[identity]; duplicate {
			return fail("Evidence %d is duplicated", index)
		}
		seenEvidence[identity] = struct{}{}
		normalized.Evidence[index] = cloneResultEvidence(reference)
	}
	sort.Slice(normalized.Evidence, func(first, second int) bool {
		return normalized.Evidence[first].Identity() < normalized.Evidence[second].Identity()
	})
	evidenceIdentities := make(map[string]struct{}, len(normalized.Evidence))
	for _, reference := range normalized.Evidence {
		evidenceIdentities[reference.Identity()] = struct{}{}
	}

	seenFacts := make(map[string]struct{}, len(packet.Facts))
	for index, fact := range packet.Facts {
		if err := validateResultKind(fact.Kind); err != nil {
			return fail("Fact %d Kind: %v", index, err)
		}
		value, err := canonicalResultJSON(fact.Value, maximumResultFactBytes, false)
		if err != nil {
			return fail("Fact %d Value: %v", index, err)
		}
		sources, err := normalizeResultSources(fact.Sources, eventOrder, true)
		if err != nil {
			return fail("Fact %d Sources: %v", index, err)
		}
		evidence, err := normalizeResultEvidenceIDs(fact.Evidence, evidenceIdentities)
		if err != nil {
			return fail("Fact %d Evidence: %v", index, err)
		}
		normalizedFact := ResultFact{Kind: fact.Kind, Value: value, Sources: sources, Evidence: evidence}
		normalizedFact.ID = resultEntryID(resultFactIDDomain, "result_fact", normalizedFact)
		if verifyIdentity && fact.ID != normalizedFact.ID {
			return fail("Fact %d ID does not match its contents", index)
		}
		if _, duplicate := seenFacts[normalizedFact.ID]; duplicate {
			return fail("Fact %d is duplicated", index)
		}
		seenFacts[normalizedFact.ID] = struct{}{}
		normalized.Facts[index] = normalizedFact
	}

	seenArtifacts := make(map[string]struct{}, len(packet.Artifacts))
	for index, artifact := range packet.Artifacts {
		if err := validateResultKind(artifact.Kind); err != nil {
			return fail("Artifact %d Kind: %v", index, err)
		}
		if err := validateResultString(artifact.Name, true); err != nil {
			return fail("Artifact %d Name: %v", index, err)
		}
		if !validResultDigest(artifact.Digest) || artifact.Bytes < 0 {
			return fail("Artifact %d has invalid content identity", index)
		}
		if err := validateResultString(artifact.MediaType, true); err != nil {
			return fail("Artifact %d MediaType: %v", index, err)
		}
		sources, err := normalizeResultSources(artifact.Sources, eventOrder, true)
		if err != nil {
			return fail("Artifact %d Sources: %v", index, err)
		}
		evidence, err := normalizeResultEvidenceIDs(artifact.Evidence, evidenceIdentities)
		if err != nil {
			return fail("Artifact %d Evidence: %v", index, err)
		}
		normalizedArtifact := ResultArtifact{
			Kind: artifact.Kind, Name: artifact.Name, Digest: strings.ToLower(artifact.Digest),
			Bytes: artifact.Bytes, MediaType: artifact.MediaType, Sources: sources, Evidence: evidence,
		}
		normalizedArtifact.ID = resultEntryID(resultArtifactIDDomain, "result_artifact", normalizedArtifact)
		if verifyIdentity && artifact.ID != normalizedArtifact.ID {
			return fail("Artifact %d ID does not match its contents", index)
		}
		if _, duplicate := seenArtifacts[normalizedArtifact.ID]; duplicate {
			return fail("Artifact %d is duplicated", index)
		}
		seenArtifacts[normalizedArtifact.ID] = struct{}{}
		normalized.Artifacts[index] = normalizedArtifact
	}

	seenExecutions := make(map[string]struct{}, len(packet.Executions))
	for index, execution := range packet.Executions {
		if err := validateResultKind(execution.Kind); err != nil {
			return fail("Execution %d Kind: %v", index, err)
		}
		if err := validateResultString(execution.Name, true); err != nil {
			return fail("Execution %d Name: %v", index, err)
		}
		switch execution.State {
		case ResultExecutionSucceeded, ResultExecutionFailed, ResultExecutionCanceled:
		default:
			return fail("Execution %d has unsupported State %q", index, execution.State)
		}
		if execution.RunID != result.RunID || execution.Attempt < 0 {
			return fail("Execution %d does not belong to the child Run", index)
		}
		for name, value := range map[string]string{
			"CallID": execution.CallID, "ErrorCode": execution.ErrorCode,
		} {
			if err := validateResultString(value, false); err != nil {
				return fail("Execution %d %s: %v", index, name, err)
			}
		}
		for name, digest := range map[string]string{
			"ArgumentsDigest": execution.ArgumentsDigest, "OutputDigest": execution.OutputDigest,
		} {
			if digest != "" && !validResultDigest(digest) {
				return fail("Execution %d %s is invalid", index, name)
			}
		}
		sources, err := normalizeResultSources(execution.Sources, eventOrder, true)
		if err != nil {
			return fail("Execution %d Sources: %v", index, err)
		}
		evidence, err := normalizeResultEvidenceIDs(execution.Evidence, evidenceIdentities)
		if err != nil {
			return fail("Execution %d Evidence: %v", index, err)
		}
		normalizedExecution := ResultExecution{
			Kind: execution.Kind, Name: execution.Name, State: execution.State, RunID: execution.RunID,
			CallID: execution.CallID, Attempt: execution.Attempt,
			ArgumentsDigest: strings.ToLower(execution.ArgumentsDigest),
			OutputDigest:    strings.ToLower(execution.OutputDigest), ErrorCode: execution.ErrorCode,
			Sources: sources, Evidence: evidence,
		}
		normalizedExecution.ID = resultEntryID(resultExecutionIDDomain, "result_execution", normalizedExecution)
		if verifyIdentity && execution.ID != normalizedExecution.ID {
			return fail("Execution %d ID does not match its contents", index)
		}
		if _, duplicate := seenExecutions[normalizedExecution.ID]; duplicate {
			return fail("Execution %d is duplicated", index)
		}
		seenExecutions[normalizedExecution.ID] = struct{}{}
		normalized.Executions[index] = normalizedExecution
	}

	if len(packet.ProfileState) > 0 {
		profileState, err := canonicalResultJSON(packet.ProfileState, MaxResultProfileStateBytes, true)
		if err != nil {
			return fail("ProfileState: %v", err)
		}
		normalized.ProfileState = profileState
	} else {
		normalized.ProfileState = nil
	}
	normalized.Digest = ""
	digest, err := resultPacketDigest(normalized)
	if err != nil {
		return fail("encode canonical packet: %v", err)
	}
	if verifyIdentity && packet.Digest != digest {
		return fail("Digest does not match packet contents")
	}
	normalized.Digest = digest
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return fail("encode bounded packet: %v", err)
	}
	if len(encoded) > MaxResultPacketBytes {
		return fail("encoded packet exceeds %d bytes", MaxResultPacketBytes)
	}
	return normalized, nil
}

func normalizeResultSources(
	sources []agent.ContextLedgerEventRef,
	eventOrder map[agent.ContextLedgerEventRef]int,
	required bool,
) ([]agent.ContextLedgerEventRef, error) {
	if required && len(sources) == 0 {
		return nil, errors.New("at least one source Event is required")
	}
	normalized := append([]agent.ContextLedgerEventRef(nil), sources...)
	seen := make(map[agent.ContextLedgerEventRef]struct{}, len(normalized))
	for _, source := range normalized {
		if _, exists := eventOrder[source]; !exists {
			return nil, errors.New("source Event is absent from the terminal Context Ledger")
		}
		if _, duplicate := seen[source]; duplicate {
			return nil, errors.New("source Event is duplicated")
		}
		seen[source] = struct{}{}
	}
	sort.Slice(normalized, func(first, second int) bool {
		return eventOrder[normalized[first]] < eventOrder[normalized[second]]
	})
	return normalized, nil
}

func normalizeResultEvidenceIDs(values []string, allowed map[string]struct{}) ([]string, error) {
	if values == nil {
		return nil, nil
	}
	normalized := append([]string(nil), values...)
	seen := make(map[string]struct{}, len(normalized))
	for index, value := range normalized {
		value = strings.ToLower(value)
		if _, exists := allowed[value]; !exists {
			return nil, errors.New("reference is absent from packet Evidence")
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, errors.New("reference is duplicated")
		}
		seen[value] = struct{}{}
		normalized[index] = value
	}
	sort.Strings(normalized)
	return normalized, nil
}

func canonicalResultJSON(data json.RawMessage, maximum int, requireObject bool) (json.RawMessage, error) {
	if len(data) == 0 {
		return nil, errors.New("JSON value is required")
	}
	var validated json.RawMessage
	if err := jsonstrict.Decode(data, maximum, &validated); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(validated))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("JSON null is not a result value")
	}
	if _, object := value.(map[string]any); requireObject && !object {
		return nil, errors.New("JSON object is required")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

func resultAvailableEvidence(result agent.RunResult) []agent.EvidenceObjectRef {
	var references []agent.EvidenceObjectRef
	if result.ContextCheckpoint != nil {
		references = append(references, result.ContextCheckpoint.Evidence...)
	}
	if result.ContextCompaction != nil {
		references = append(references, result.ContextCompaction.Externalized...)
	}
	seen := make(map[string]struct{}, len(references))
	resultReferences := make([]agent.EvidenceObjectRef, 0, len(references))
	for _, reference := range references {
		if agent.ValidateEvidenceObjectRef(reference) != nil {
			continue
		}
		identity := reference.Identity()
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		resultReferences = append(resultReferences, cloneResultEvidence(reference))
	}
	sort.Slice(resultReferences, func(first, second int) bool {
		return resultReferences[first].Identity() < resultReferences[second].Identity()
	})
	return resultReferences
}

func resultSourcesContainRun(sources []agent.ContextLedgerEventRef, runID string) bool {
	for _, source := range sources {
		if source.RunID == runID {
			return true
		}
	}
	return false
}

func resultExecutionState(state agent.ExecutionLedgerState) (ResultExecutionState, error) {
	switch state {
	case agent.ExecutionLedgerSucceeded:
		return ResultExecutionSucceeded, nil
	case agent.ExecutionLedgerFailed:
		return ResultExecutionFailed, nil
	case agent.ExecutionLedgerCanceled:
		return ResultExecutionCanceled, nil
	default:
		return "", fmt.Errorf("unsupported terminal Execution Ledger state %q", state)
	}
}

func validateResultKind(value string) error {
	if err := validateResultString(value, true); err != nil {
		return err
	}
	if len(value) > 128 {
		return errors.New("value exceeds 128 bytes")
	}
	return nil
}

func validateResultString(value string, required bool) error {
	if required && value == "" {
		return errors.New("value is required")
	}
	if strings.TrimSpace(value) != value {
		return errors.New("value has surrounding whitespace")
	}
	if len(value) > maximumResultStringBytes || !utf8.ValidString(value) {
		return fmt.Errorf("value must be valid UTF-8 and at most %d bytes", maximumResultStringBytes)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return errors.New("value contains a control character")
		}
	}
	return nil
}

func validResultDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func resultEntryID(domain, prefix string, value any) string {
	encoded, _ := json.Marshal(value)
	digest := sha256.New()
	_, _ = digest.Write([]byte(domain))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(encoded)
	return prefix + "_" + hex.EncodeToString(digest.Sum(nil))
}

func resultPacketDigest(packet ResultPacket) (string, error) {
	packet.Digest = ""
	encoded, err := json.Marshal(packet)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(resultPacketDigestDomain))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(encoded)
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func resultEvidenceEqual(first, second agent.EvidenceObjectRef) bool {
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	return bytes.Equal(firstJSON, secondJSON)
}

func cloneResultEvidence(reference agent.EvidenceObjectRef) agent.EvidenceObjectRef {
	if reference.Scope == nil {
		return reference
	}
	scope := *reference.Scope
	scope.RequiredCapabilities = append([]string(nil), reference.Scope.RequiredCapabilities...)
	reference.Scope = &scope
	return reference
}

func cloneResultPacket(packet ResultPacket) ResultPacket {
	packet.Facts = append([]ResultFact(nil), packet.Facts...)
	for index := range packet.Facts {
		packet.Facts[index].Value = append(json.RawMessage(nil), packet.Facts[index].Value...)
		packet.Facts[index].Sources = append([]agent.ContextLedgerEventRef(nil), packet.Facts[index].Sources...)
		packet.Facts[index].Evidence = append([]string(nil), packet.Facts[index].Evidence...)
	}
	packet.Artifacts = append([]ResultArtifact(nil), packet.Artifacts...)
	for index := range packet.Artifacts {
		packet.Artifacts[index].Sources = append([]agent.ContextLedgerEventRef(nil), packet.Artifacts[index].Sources...)
		packet.Artifacts[index].Evidence = append([]string(nil), packet.Artifacts[index].Evidence...)
	}
	packet.Executions = append([]ResultExecution(nil), packet.Executions...)
	for index := range packet.Executions {
		packet.Executions[index].Sources = append([]agent.ContextLedgerEventRef(nil), packet.Executions[index].Sources...)
		packet.Executions[index].Evidence = append([]string(nil), packet.Executions[index].Evidence...)
	}
	packet.Evidence = append([]agent.EvidenceObjectRef(nil), packet.Evidence...)
	for index := range packet.Evidence {
		packet.Evidence[index] = cloneResultEvidence(packet.Evidence[index])
	}
	packet.ProfileState = append(json.RawMessage(nil), packet.ProfileState...)
	return packet
}

func cloneResultPacketPointer(packet *ResultPacket) *ResultPacket {
	if packet == nil {
		return nil
	}
	cloned := cloneResultPacket(*packet)
	return &cloned
}
