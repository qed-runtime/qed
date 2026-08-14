package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	// ContextLedgerVersion is the schema version emitted by BuildContextLedger
	ContextLedgerVersion uint32 = 1

	contextLedgerSourceHashDomain = "qed.context.ledger.sources.v1"
	contextLedgerDigestDomain     = "qed.context.ledger.v1"
	contextLedgerEventHashDomain  = "qed.context.ledger.event.v1"
	contextLedgerValueHashDomain  = "qed.context.ledger.value.v1"
)

// ArtifactLedgerKind identifies a durable output represented by the Artifact Ledger
type ArtifactLedgerKind string

// Artifact Ledger kinds reconstructed from Runtime Events
const (
	ArtifactLedgerToolOutput     ArtifactLedgerKind = "tool_output"
	ArtifactLedgerEvidenceObject ArtifactLedgerKind = "evidence_object"
)

// ExecutionLedgerKind identifies work represented by the Execution Ledger
type ExecutionLedgerKind string

// Execution Ledger kinds reconstructed from Runtime Events
const (
	ExecutionLedgerProviderCall ExecutionLedgerKind = "provider_call"
	ExecutionLedgerToolCall     ExecutionLedgerKind = "tool_call"
)

// ExecutionLedgerState identifies the latest observable execution state
type ExecutionLedgerState string

// Execution Ledger states
const (
	ExecutionLedgerPending   ExecutionLedgerState = "pending"
	ExecutionLedgerSucceeded ExecutionLedgerState = "succeeded"
	ExecutionLedgerFailed    ExecutionLedgerState = "failed"
	ExecutionLedgerCanceled  ExecutionLedgerState = "canceled"
)

// ConstraintLedgerKind identifies a statement retained without semantic inference
type ConstraintLedgerKind string

// Constraint Ledger kinds reconstructed from Runtime Events
const (
	ConstraintLedgerUserInput ConstraintLedgerKind = "user_input"
)

// FactState identifies the deterministic lifecycle state of one Constraint Fact
type FactState string

// Constraint Fact lifecycle states
const (
	// FactActive identifies a Fact that remains part of current context
	FactActive FactState = "active"
	// FactSuperseded identifies a Fact replaced by a later Fact
	FactSuperseded FactState = "superseded"
	// FactResolved identifies a Fact explicitly retired without a replacement
	FactResolved FactState = "resolved"
)

// PolicyLedgerKind identifies one host or human authorization boundary
type PolicyLedgerKind string

// Policy Ledger kinds reconstructed from Runtime Events
const (
	PolicyLedgerToolAuthorization PolicyLedgerKind = "tool_authorization"
	PolicyLedgerHumanApproval     PolicyLedgerKind = "human_approval"
)

// PolicyLedgerOutcome identifies the latest observable authorization result
type PolicyLedgerOutcome string

// Policy Ledger outcomes
const (
	PolicyLedgerPending  PolicyLedgerOutcome = "pending"
	PolicyLedgerAllowed  PolicyLedgerOutcome = "allow"
	PolicyLedgerDenied   PolicyLedgerOutcome = "deny"
	PolicyLedgerAsked    PolicyLedgerOutcome = "ask"
	PolicyLedgerUnknown  PolicyLedgerOutcome = "unknown"
	PolicyLedgerCanceled PolicyLedgerOutcome = "canceled"
)

// TaskLedgerState identifies the latest observable state of one Run task
type TaskLedgerState string

// Task Ledger states
const (
	TaskLedgerRunning   TaskLedgerState = "running"
	TaskLedgerWaiting   TaskLedgerState = "waiting"
	TaskLedgerCompleted TaskLedgerState = "completed"
	TaskLedgerFailed    TaskLedgerState = "failed"
	TaskLedgerCanceled  TaskLedgerState = "canceled"
)

// ContextLedgerEventRef identifies one immutable source Event without copying payloads
type ContextLedgerEventRef struct {
	// RunID identifies the source Run when present in the Event Log
	RunID string `json:"run_id"`
	// Sequence identifies the Event position within RunID
	Sequence uint64 `json:"sequence"`
	// SessionRevision identifies the Event position within a persisted Session
	SessionRevision uint64 `json:"session_revision,omitempty"`
}

// ContextLedgerSource fingerprints one ordered source Event used by a Ledger
type ContextLedgerSource struct {
	ContextLedgerEventRef
	// Type identifies the source Event transition
	Type EventType `json:"type"`
	// ContentHash identifies the complete public Event payload and exact raw JSON bytes
	ContentHash string `json:"content_hash"`
}

// ArtifactLedgerEntry identifies one immutable output or externalized object
type ArtifactLedgerEntry struct {
	// ID is a stable domain-separated identity derived from the source artifact
	ID string `json:"id"`
	// Kind identifies how Runtime observed the artifact
	Kind ArtifactLedgerKind `json:"kind"`
	// Name identifies the producing Tool or artifact class
	Name string `json:"name"`
	// Digest identifies the exact artifact content
	Digest string `json:"digest"`
	// Bytes is the exact artifact size
	Bytes int64 `json:"bytes"`
	// MediaType describes the artifact representation
	MediaType string `json:"media_type"`
	// Sources identifies the ordered Events that produced or referenced the artifact
	Sources []ContextLedgerEventRef `json:"sources"`
}

// ExecutionLedgerEntry identifies one Provider or Tool attempt and its latest state
type ExecutionLedgerEntry struct {
	// ID is a stable domain-separated identity for this attempt
	ID string `json:"id"`
	// Kind identifies Provider or Tool work
	Kind ExecutionLedgerKind `json:"kind"`
	// Name identifies the Provider or Tool
	Name string `json:"name"`
	// RunID identifies the owning Run
	RunID string `json:"run_id"`
	// CallID identifies the Provider attempt or Tool call within the Run
	CallID string `json:"call_id,omitempty"`
	// Attempt is the one-based attempt number when available
	Attempt int `json:"attempt,omitempty"`
	// State is the latest state reconstructed from source Events
	State ExecutionLedgerState `json:"state"`
	// ArgumentsDigest identifies exact Tool arguments without retaining them here
	ArgumentsDigest string `json:"arguments_digest,omitempty"`
	// OutputDigest identifies exact assistant or Tool output without retaining it here
	OutputDigest string `json:"output_digest,omitempty"`
	// ErrorCode contains the provider-neutral Provider classification when available
	ErrorCode string `json:"error_code,omitempty"`
	// Sources identifies the ordered Events used to reconstruct this execution
	Sources []ContextLedgerEventRef `json:"sources"`
}

// ConstraintLedgerEntry preserves explicit user input and its deterministic lifecycle
type ConstraintLedgerEntry struct {
	// ID is a stable domain-separated identity for this user statement
	ID string `json:"id"`
	// Kind identifies how the statement entered Runtime
	Kind ConstraintLedgerKind `json:"kind"`
	// Text preserves exact user input for lifecycle-aware context compilation
	Text string `json:"text"`
	// ContentHash identifies the complete provider-neutral user Message
	ContentHash string `json:"content_hash"`
	// SourceMessage is the zero-based raw Session message index
	SourceMessage int `json:"source_message"`
	// Origin distinguishes Run input from active-Run steering
	Origin UserMessageOrigin `json:"origin,omitempty"`
	// State identifies whether this Fact remains active or has been retired
	State FactState `json:"state"`
	// StateSource identifies the Event that established the current State
	StateSource ContextLedgerEventRef `json:"state_source"`
	// Supersedes identifies earlier Facts retired by this active replacement
	Supersedes []string `json:"supersedes,omitempty"`
	// SupersededBy identifies the replacement Fact when State is superseded
	SupersededBy string `json:"superseded_by,omitempty"`
	// Sources identifies the source user.message.added Event and any transition Event
	Sources []ContextLedgerEventRef `json:"sources"`
}

// PolicyLedgerEntry identifies one authorization decision and its provenance
type PolicyLedgerEntry struct {
	// ID is a stable domain-separated identity for this authorization boundary
	ID string `json:"id"`
	// Kind identifies host Tool authorization or human approval
	Kind PolicyLedgerKind `json:"kind"`
	// Subject identifies the Tool or approval subject when available
	Subject string `json:"subject"`
	// Outcome is the latest authorization result reconstructed from Events
	Outcome PolicyLedgerOutcome `json:"outcome"`
	// Capabilities contains sorted capability names without Tool arguments
	Capabilities []string `json:"capabilities,omitempty"`
	// ReasonDigest identifies a host Policy reason without exposing its text
	ReasonDigest string `json:"reason_digest,omitempty"`
	// Sources identifies the Events that established this result
	Sources []ContextLedgerEventRef `json:"sources"`
}

// TaskLedgerEntry identifies one Run and its latest replayed state
type TaskLedgerEntry struct {
	// ID is a stable domain-separated identity for this task
	ID string `json:"id"`
	// RunID identifies the Run represented as a Runtime task
	RunID string `json:"run_id"`
	// ParentRunID identifies the delegating Run when present
	ParentRunID string `json:"parent_run_id,omitempty"`
	// AgentID identifies the configured Agent when present
	AgentID string `json:"agent_id,omitempty"`
	// State is the latest Run state reconstructed from Events
	State TaskLedgerState `json:"state"`
	// InputCount counts Run input and steering user Messages
	InputCount int `json:"input_count"`
	// ErrorDigest identifies terminal error text without copying it into the entry
	ErrorDigest string `json:"error_digest,omitempty"`
	// Sources identifies Events that changed task state or input count
	Sources []ContextLedgerEventRef `json:"sources"`
}

// ContextLedger is a content-addressed deterministic view over ordered Run Events
//
// Sources retain content-free provenance for every input Event. The five
// ledgers contain only state that Runtime can reconstruct without model-based
// interpretation or access to a live workspace.
type ContextLedger struct {
	// Version identifies the Ledger schema
	Version uint32 `json:"version"`
	// SessionID identifies the logical Session when supplied by Run Events
	SessionID string `json:"session_id,omitempty"`
	// SessionRevision is the last source Event revision for persisted Sessions
	SessionRevision uint64 `json:"session_revision,omitempty"`
	// SourceEventCount is the number of ordered Events represented by this Ledger
	SourceEventCount int `json:"source_event_count"`
	// SourceHash identifies the exact ordered ContextLedgerSource sequence
	SourceHash string `json:"source_hash"`
	// Digest identifies the complete canonical Ledger snapshot
	Digest string `json:"digest"`
	// Sources contains content-free provenance for every input Event
	Sources []ContextLedgerSource `json:"sources"`
	// CheckpointReferences contains prior Ledger generations verified during replay
	CheckpointReferences []ContextLedgerReference `json:"checkpoint_references"`
	// Artifacts contains immutable Tool outputs and externalized Evidence Objects
	Artifacts []ArtifactLedgerEntry `json:"artifacts"`
	// Executions contains Provider and Tool attempts
	Executions []ExecutionLedgerEntry `json:"executions"`
	// Constraints contains user Facts and explicit deterministic lifecycle state
	Constraints []ConstraintLedgerEntry `json:"constraints"`
	// Policies contains host authorization and human approval results
	Policies []PolicyLedgerEntry `json:"policies"`
	// Tasks contains the latest observable state of each Run
	Tasks []TaskLedgerEntry `json:"tasks"`
}

// ContextLedgerReference is a content-free Checkpoint reference to one Ledger generation
type ContextLedgerReference struct {
	// Version identifies the referenced Ledger schema
	Version uint32 `json:"version"`
	// Digest identifies the complete referenced Ledger snapshot
	Digest string `json:"digest"`
	// SourceEventCount is the Event prefix length represented by the snapshot
	SourceEventCount int `json:"source_event_count"`
	// SourceHash identifies the exact ordered Event prefix
	SourceHash string `json:"source_hash"`
	// SessionRevision is the final revision of the referenced Event prefix
	SessionRevision uint64 `json:"session_revision,omitempty"`
}

// Reference returns a content-free immutable reference to this Ledger
func (ledger ContextLedger) Reference() ContextLedgerReference {
	return ContextLedgerReference{
		Version:          ledger.Version,
		Digest:           ledger.Digest,
		SourceEventCount: ledger.SourceEventCount,
		SourceHash:       ledger.SourceHash,
		SessionRevision:  ledger.SessionRevision,
	}
}

type ledgerTask struct {
	entry    TaskLedgerEntry
	terminal bool
}

type ledgerExecution struct {
	entry ExecutionLedgerEntry
	order int
}

type ledgerPolicy struct {
	entry PolicyLedgerEntry
	order int
}

type ledgerArtifact struct {
	entry ArtifactLedgerEntry
	order int
}

// BuildContextLedger validates and reduces one complete ordered Event Log
//
// The same Events always produce byte-equivalent JSON and the same Digest.
// Events with Session revisions must start at one and be contiguous. Ephemeral
// Events must preserve contiguous per-Run sequences.
func BuildContextLedger(ctx context.Context, events []Event) (ContextLedger, error) {
	if ctx == nil {
		return ContextLedger{}, errors.New("Context Ledger context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return ContextLedger{}, err
	}

	ledger := ContextLedger{
		Version:              ContextLedgerVersion,
		Sources:              make([]ContextLedgerSource, 0, len(events)),
		CheckpointReferences: make([]ContextLedgerReference, 0),
		Artifacts:            make([]ArtifactLedgerEntry, 0),
		Executions:           make([]ExecutionLedgerEntry, 0),
		Constraints:          make([]ConstraintLedgerEntry, 0),
		Policies:             make([]PolicyLedgerEntry, 0),
		Tasks:                make([]TaskLedgerEntry, 0),
	}
	tasks := make(map[string]*ledgerTask)
	var taskOrder []string
	executions := make(map[string]*ledgerExecution)
	artifacts := make(map[string]*ledgerArtifact)
	policies := make(map[string]*ledgerPolicy)
	constraintIndexes := make(map[string]int)
	pendingProviders := make(map[string]string)
	pendingApprovals := make(map[string]string)
	perRunSequence := make(map[string]uint64)
	runIdentity := make(map[string]Event)
	seenEvents := make(map[string]struct{}, len(events))
	persisted := false
	sourceMessageCount := 0
	var activeCheckpoint *ContextCheckpoint
	var preparedCheckpoint *ContextCheckpoint
	seenContextCompaction := false

	for index := range events {
		if err := ctx.Err(); err != nil {
			return ContextLedger{}, err
		}
		event := cloneEvent(events[index])
		if err := validatePredictiveBudgetEvent(event); err != nil {
			return ContextLedger{}, fmt.Errorf("Context Ledger Event %d: %w", index, err)
		}
		if err := validateLedgerEventIdentity(event, index, &ledger, &persisted, perRunSequence, seenEvents); err != nil {
			return ContextLedger{}, fmt.Errorf("Context Ledger Event %d: %w", index, err)
		}
		if event.Type == EventContextCompactionPrepared && event.RunID == "" {
			return ContextLedger{}, fmt.Errorf("Context Ledger Event %d: legacy context.compaction.prepared is unsupported", index)
		}
		source, err := contextLedgerSource(event)
		if err != nil {
			return ContextLedger{}, fmt.Errorf("Context Ledger Event %d: %w", index, err)
		}
		if event.FactDirective != nil && event.Type != EventUserMessageAdded {
			return ContextLedger{}, fmt.Errorf("Context Ledger Event %d: Fact lifecycle directive requires user.message.added", index)
		}
		if event.Message != nil && event.Message.FactDirective != nil {
			return ContextLedger{}, fmt.Errorf("Context Ledger Event %d: Message retains a Fact lifecycle directive", index)
		}
		if event.Type == EventCurrentWorldStateCaptured && event.CurrentWorldState == nil {
			return ContextLedger{}, fmt.Errorf("Context Ledger Event %d: current_world_state.captured requires Current World State", index)
		}
		if event.Type != EventCurrentWorldStateCaptured && event.CurrentWorldState != nil {
			return ContextLedger{}, fmt.Errorf("Context Ledger Event %d: Event %q must not contain Current World State", index, event.Type)
		}
		if event.ToolResult != nil && event.ToolResult.ContextRetrieval != nil {
			if event.Type != EventToolCompleted {
				return ContextLedger{}, fmt.Errorf("Context Ledger Event %d: Context retrieval metadata requires tool.completed", index)
			}
			if err := ValidateContextRetrievalMetadata(
				event.ToolResult.Name,
				event.ToolResult.Output,
				event.ToolResult.IsError,
				event.ToolResult.ContextRetrieval,
			); err != nil {
				return ContextLedger{}, fmt.Errorf("Context Ledger Event %d: %w", index, err)
			}
			if event.ToolResult.ContextRetrieval.PostCompaction != seenContextCompaction {
				return ContextLedger{}, fmt.Errorf("Context Ledger Event %d: Context retrieval post-compaction status does not match its Event prefix", index)
			}
		}
		if event.Type == EventContextCompactionPrepared {
			if err := validateContextPreparationTransition(event, activeCheckpoint); err != nil {
				return ContextLedger{}, fmt.Errorf("Context Ledger Event %d: %w", index, err)
			}
			preparedCheckpoint = cloneContextCheckpointPointer(event.ContextCheckpoint)
		}
		if event.Type == EventContextCompacted {
			if err := validateContextValidationTransition(event, activeCheckpoint); err != nil {
				return ContextLedger{}, fmt.Errorf("Context Ledger Event %d: %w", index, err)
			}
			if event.ContextCheckpoint != nil {
				activeCheckpoint = cloneContextCheckpointPointer(event.ContextCheckpoint)
			}
			preparedCheckpoint = nil
			seenContextCompaction = true
		}
		if event.Type == EventModelRequest && event.PredictiveBudget != nil {
			switch event.PredictiveBudget.Action {
			case PredictiveBudgetActionPrepare:
				if preparedCheckpoint == nil ||
					preparedCheckpoint.Generation != event.PredictiveBudget.CandidateGeneration {
					return ContextLedger{}, fmt.Errorf("Context Ledger Event %d: model request has no matching prepared candidate", index)
				}
			case PredictiveBudgetActionAdopt:
				if activeCheckpoint == nil ||
					activeCheckpoint.Generation != event.PredictiveBudget.CandidateGeneration {
					return ContextLedger{}, fmt.Errorf("Context Ledger Event %d: model request has no matching adopted candidate", index)
				}
			}
		}
		ledger.Sources = append(ledger.Sources, source)
		ref := source.ContextLedgerEventRef
		sourceMessage := sourceMessageCount
		sourceMessageCount += ledgerEventMessageCount(event)
		if event.RunID == "" {
			if err := reduceLegacyLedgerEvent(&ledger, constraintIndexes, artifacts, policies, pendingApprovals, event, ref, sourceMessage, index); err != nil {
				return ContextLedger{}, fmt.Errorf("Context Ledger legacy Event %d: %w", index, err)
			}
			continue
		}

		if event.Type == EventRunStarted {
			if _, exists := tasks[event.RunID]; exists {
				return ContextLedger{}, fmt.Errorf("Run %q starts more than once", event.RunID)
			}
			tasks[event.RunID] = &ledgerTask{entry: TaskLedgerEntry{
				ID:          contextLedgerID("task", event.RunID),
				RunID:       event.RunID,
				ParentRunID: event.ParentRunID,
				AgentID:     event.AgentID,
				State:       TaskLedgerRunning,
				Sources:     []ContextLedgerEventRef{ref},
			}}
			taskOrder = append(taskOrder, event.RunID)
			runIdentity[event.RunID] = event
			continue
		}

		task := tasks[event.RunID]
		if task == nil {
			return ContextLedger{}, fmt.Errorf("Run %q has Event %q before run.started", event.RunID, event.Type)
		}
		identity := runIdentity[event.RunID]
		if event.ParentRunID != identity.ParentRunID || event.AgentID != identity.AgentID || event.SessionID != identity.SessionID {
			return ContextLedger{}, fmt.Errorf("Run %q identity changes after run.started", event.RunID)
		}
		if task.terminal {
			return ContextLedger{}, fmt.Errorf("Run %q emits Event %q after terminal state", event.RunID, event.Type)
		}

		switch event.Type {
		case EventUserMessageAdded:
			if event.Message == nil {
				return ContextLedger{}, errors.New("user.message.added requires a Message")
			}
			if event.UserMessageOrigin != UserMessageOriginRunInput && event.UserMessageOrigin != UserMessageOriginSteering {
				return ContextLedger{}, fmt.Errorf("user.message.added has unsupported origin %q", event.UserMessageOrigin)
			}
			if event.UserMessageOrigin == UserMessageOriginSteering {
				if err := validateSteeringMessage(*event.Message); err != nil {
					return ContextLedger{}, fmt.Errorf("steering user.message.added: %w", err)
				}
			}
			task.entry.InputCount++
			task.entry.Sources = append(task.entry.Sources, ref)
			if err := applyConstraintFactEvent(&ledger, constraintIndexes, event, ref, sourceMessage); err != nil {
				return ContextLedger{}, err
			}

		case EventCurrentWorldStateCaptured:
			if pendingProviders[event.RunID] != "" {
				return ContextLedger{}, errors.New("current_world_state.captured overlaps one pending Provider call")
			}
			for _, execution := range executions {
				if execution.entry.RunID == event.RunID && execution.entry.Kind == ExecutionLedgerToolCall &&
					execution.entry.State == ExecutionLedgerPending {
					return ContextLedger{}, errors.New("current_world_state.captured overlaps one pending Tool call")
				}
			}
			prefix, err := finalizeContextLedger(
				ledger,
				ledger.Sources[:len(ledger.Sources)-1],
				tasks,
				taskOrder,
				executions,
				artifacts,
				policies,
			)
			if err != nil {
				return ContextLedger{}, err
			}
			if err := validateCurrentWorldStateAgainstPrefix(
				*event.CurrentWorldState,
				prefix,
				events[:index],
			); err != nil {
				return ContextLedger{}, fmt.Errorf("current_world_state.captured: %w", err)
			}

		case EventModelRequest:
			if event.ProviderCall <= 0 || event.ProviderAttempt <= 0 || event.PrefixManifest == nil || event.PrefixManifest.Provider == "" {
				return ContextLedger{}, errors.New("model.request.started requires Provider call, attempt, and Prefix Manifest")
			}
			if pendingProviders[event.RunID] != "" {
				return ContextLedger{}, errors.New("model.request.started overlaps one pending Provider call")
			}
			key := providerExecutionKey(event.RunID, event.ProviderCall)
			if _, exists := executions[key]; exists {
				return ContextLedger{}, fmt.Errorf("Provider call %d for Run %q is duplicated", event.ProviderCall, event.RunID)
			}
			executions[key] = &ledgerExecution{order: index, entry: ExecutionLedgerEntry{
				ID:      contextLedgerID("provider", event.RunID, fmt.Sprint(event.ProviderCall)),
				Kind:    ExecutionLedgerProviderCall,
				Name:    event.PrefixManifest.Provider,
				RunID:   event.RunID,
				CallID:  fmt.Sprint(event.ProviderCall),
				Attempt: event.ProviderAttempt,
				State:   ExecutionLedgerPending,
				Sources: []ContextLedgerEventRef{ref},
			}}
			pendingProviders[event.RunID] = key

		case EventProviderRetry:
			key := providerExecutionKey(event.RunID, event.ProviderCall)
			execution := executions[key]
			if execution == nil || execution.entry.State != ExecutionLedgerPending || event.ProviderRetry == nil {
				return ContextLedger{}, errors.New("provider.retry.scheduled does not match one pending Provider call")
			}
			execution.entry.State = ExecutionLedgerFailed
			execution.entry.ErrorCode = string(event.ProviderRetry.Error.Code)
			execution.entry.Sources = append(execution.entry.Sources, ref)
			delete(pendingProviders, event.RunID)

		case EventMessageCompleted:
			if event.Message == nil || event.Message.Role != RoleAssistant {
				return ContextLedger{}, errors.New("message.completed requires an assistant Message")
			}
			key := pendingProviders[event.RunID]
			execution := executions[key]
			if execution == nil || execution.entry.State != ExecutionLedgerPending {
				return ContextLedger{}, errors.New("message.completed does not match one pending Provider call")
			}
			execution.entry.State = ExecutionLedgerSucceeded
			execution.entry.OutputDigest = contextLedgerValueDigest([]byte(event.Message.Text))
			execution.entry.Sources = append(execution.entry.Sources, ref)
			delete(pendingProviders, event.RunID)

		case EventToolStarted:
			if event.ToolCall == nil || event.ToolCall.ID == "" || event.ToolCall.Name == "" {
				return ContextLedger{}, errors.New("tool.started requires a Tool Call identity")
			}
			if pendingProviders[event.RunID] != "" {
				return ContextLedger{}, errors.New("tool.started occurs before Provider completion")
			}
			key := toolExecutionKey(event.RunID, event.ToolCall.ID)
			if _, exists := executions[key]; exists {
				return ContextLedger{}, fmt.Errorf("Tool Call %q for Run %q is duplicated", event.ToolCall.ID, event.RunID)
			}
			executions[key] = &ledgerExecution{order: index, entry: ExecutionLedgerEntry{
				ID:              contextLedgerID("tool", event.RunID, event.ToolCall.ID),
				Kind:            ExecutionLedgerToolCall,
				Name:            event.ToolCall.Name,
				RunID:           event.RunID,
				CallID:          event.ToolCall.ID,
				Attempt:         1,
				State:           ExecutionLedgerPending,
				ArgumentsDigest: contextLedgerValueDigest(event.ToolCall.Arguments),
				Sources:         []ContextLedgerEventRef{ref},
			}}

		case EventToolCompleted:
			if event.ToolCall == nil || event.ToolResult == nil || event.ToolCall.ID == "" || event.ToolCall.Name == "" ||
				event.ToolResult.CallID != event.ToolCall.ID || event.ToolResult.Name != event.ToolCall.Name {
				return ContextLedger{}, errors.New("tool.completed requires matching Tool Call and Tool Result identities")
			}
			if err := ValidateContextOperation(event.ToolResult.ContextOperation); err != nil {
				return ContextLedger{}, fmt.Errorf("tool.completed: %w", err)
			}
			key := toolExecutionKey(event.RunID, event.ToolCall.ID)
			execution := executions[key]
			if execution == nil || execution.entry.State != ExecutionLedgerPending || execution.entry.Name != event.ToolCall.Name {
				return ContextLedger{}, errors.New("tool.completed does not match one pending Tool Call")
			}
			execution.entry.State = ExecutionLedgerSucceeded
			if event.ToolResult.IsError {
				execution.entry.State = ExecutionLedgerFailed
			}
			execution.entry.OutputDigest = contextLedgerValueDigest([]byte(event.ToolResult.Output))
			execution.entry.Sources = append(execution.entry.Sources, ref)
			addToolOutputArtifact(artifacts, event, ref, index)
			if event.ToolResult.Policy != nil {
				if err := addToolPolicy(policies, event, ref, index); err != nil {
					return ContextLedger{}, err
				}
			}

		case EventContextCompactionPrepared, EventContextCompacted:
			if event.ContextCompaction == nil {
				return ContextLedger{}, fmt.Errorf("%s requires a compaction report", event.Type)
			}
			if err := validateContextRebaseEvent(event); err != nil {
				return ContextLedger{}, err
			}
			if event.ContextCheckpoint != nil && event.ContextCheckpoint.Ledger != nil {
				prefix, err := finalizeContextLedger(
					ledger,
					ledger.Sources[:len(ledger.Sources)-1],
					tasks,
					taskOrder,
					executions,
					artifacts,
					policies,
				)
				if err != nil {
					return ContextLedger{}, err
				}
				if event.ContextCheckpoint.Ledger.Version != ContextLedgerVersion {
					return ContextLedger{}, fmt.Errorf(
						"unsupported Context Ledger reference version %d",
						event.ContextCheckpoint.Ledger.Version,
					)
				}
				wantReference := prefix.Reference()
				if *event.ContextCheckpoint.Ledger != wantReference &&
					!containsContextLedgerReference(ledger.CheckpointReferences, *event.ContextCheckpoint.Ledger) {
					if event.Type == EventContextCompacted {
						return ContextLedger{}, errors.New("context.compacted Checkpoint Ledger reference does not match its Event prefix")
					}
					return ContextLedger{}, errors.New("context.compaction.prepared Checkpoint Ledger reference does not match its Event prefix")
				}
				if !containsContextLedgerReference(ledger.CheckpointReferences, *event.ContextCheckpoint.Ledger) {
					ledger.CheckpointReferences = append(ledger.CheckpointReferences, *event.ContextCheckpoint.Ledger)
				}
			}
			for _, reference := range event.ContextCompaction.Externalized {
				if err := ValidateEvidenceObjectRef(reference); err != nil || strings.TrimSpace(reference.MediaType) == "" {
					return ContextLedger{}, fmt.Errorf("%s contains an invalid Evidence Object reference", event.Type)
				}
				if err := addEvidenceArtifact(artifacts, reference, ref, index); err != nil {
					return ContextLedger{}, err
				}
			}

		case EventRunWaiting:
			if event.WaitRequest == nil || event.WaitRequest.ID == "" || event.WaitRequest.Kind == "" {
				return ContextLedger{}, errors.New("run.waiting requires a Wait request identity")
			}
			task.entry.State = TaskLedgerWaiting
			task.entry.Sources = append(task.entry.Sources, ref)
			if event.WaitRequest.Kind == WaitKindApproval {
				if _, exists := pendingApprovals[event.WaitRequest.ID]; exists {
					return ContextLedger{}, fmt.Errorf("approval request %q is already pending", event.WaitRequest.ID)
				}
				entry := approvalPolicyEntry(*event.WaitRequest, ref)
				key := approvalPolicyKey(ref, event.WaitRequest.ID)
				policies[key] = &ledgerPolicy{entry: entry, order: index}
				pendingApprovals[event.WaitRequest.ID] = key
			}

		case EventRunResumed:
			if event.WaitResponse == nil || event.WaitResponse.RequestID == "" {
				return ContextLedger{}, errors.New("run.resumed requires a Wait response identity")
			}
			task.entry.State = TaskLedgerRunning
			task.entry.Sources = append(task.entry.Sources, ref)
			if key := pendingApprovals[event.WaitResponse.RequestID]; key != "" {
				policy := policies[key]
				policy.entry.Outcome = approvalOutcome(event.WaitResponse.Payload)
				policy.entry.Sources = append(policy.entry.Sources, ref)
				delete(pendingApprovals, event.WaitResponse.RequestID)
			}

		case EventRunCompleted, EventRunFailed, EventRunCanceled:
			if event.Type == EventRunCompleted {
				for _, execution := range executions {
					if execution.entry.RunID == event.RunID && execution.entry.State == ExecutionLedgerPending {
						return ContextLedger{}, errors.New("run.completed has pending execution")
					}
				}
				for _, policy := range policies {
					if policy.entry.Outcome == PolicyLedgerPending && len(policy.entry.Sources) > 0 &&
						policy.entry.Sources[0].RunID == event.RunID {
						return ContextLedger{}, errors.New("run.completed has pending approval")
					}
				}
			}
			switch event.Type {
			case EventRunCompleted:
				task.entry.State = TaskLedgerCompleted
			case EventRunFailed:
				task.entry.State = TaskLedgerFailed
			case EventRunCanceled:
				task.entry.State = TaskLedgerCanceled
			}
			if event.Error != "" {
				task.entry.ErrorDigest = contextLedgerValueDigest([]byte(event.Error))
			}
			task.entry.Sources = append(task.entry.Sources, ref)
			task.terminal = true
			for _, execution := range executions {
				if execution.entry.RunID != event.RunID || execution.entry.State != ExecutionLedgerPending {
					continue
				}
				if event.Type == EventRunCanceled {
					execution.entry.State = ExecutionLedgerCanceled
				} else {
					execution.entry.State = ExecutionLedgerFailed
				}
				if event.ProviderError != nil && execution.entry.Kind == ExecutionLedgerProviderCall {
					execution.entry.ErrorCode = string(event.ProviderError.Code)
				}
				execution.entry.Sources = append(execution.entry.Sources, ref)
			}
			delete(pendingProviders, event.RunID)
			for requestID, key := range pendingApprovals {
				policy := policies[key]
				if len(policy.entry.Sources) > 0 && policy.entry.Sources[0].RunID == event.RunID {
					policy.entry.Outcome = PolicyLedgerCanceled
					policy.entry.Sources = append(policy.entry.Sources, ref)
					delete(pendingApprovals, requestID)
				}
			}
		}
	}

	return finalizeContextLedger(ledger, ledger.Sources, tasks, taskOrder, executions, artifacts, policies)
}

// ValidateContextLedger rebuilds a Ledger and rejects any changed derived state
func ValidateContextLedger(ctx context.Context, ledger ContextLedger, events []Event) error {
	rebuilt, err := BuildContextLedger(ctx, events)
	if err != nil {
		return err
	}
	want, err := json.Marshal(rebuilt)
	if err != nil {
		return fmt.Errorf("encode rebuilt Context Ledger: %w", err)
	}
	got, err := json.Marshal(ledger)
	if err != nil {
		return fmt.Errorf("encode supplied Context Ledger: %w", err)
	}
	if string(got) != string(want) {
		return errors.New("Context Ledger does not match its source Events")
	}
	return nil
}

func validateContextLedgerReference(reference ContextLedgerReference, current *ContextLedger) error {
	if reference.Version != ContextLedgerVersion || !validSHA256Digest(reference.Digest) ||
		!validSHA256Digest(reference.SourceHash) || reference.SourceEventCount < 0 {
		return errors.New("reference identity is invalid")
	}
	if current == nil {
		return errors.New("current Ledger is unavailable")
	}
	if reference.SourceEventCount > len(current.Sources) {
		return errors.New("reference exceeds current source Events")
	}
	wantSourceHash := contextLedgerJSONDigest(
		contextLedgerSourceHashDomain,
		current.Sources[:reference.SourceEventCount],
	)
	if reference.SourceHash != wantSourceHash {
		return errors.New("source hash does not match current Event prefix")
	}
	wantRevision := uint64(0)
	if reference.SourceEventCount > 0 {
		wantRevision = current.Sources[reference.SourceEventCount-1].SessionRevision
	}
	if reference.SessionRevision != wantRevision {
		return errors.New("Session revision does not match current Event prefix")
	}
	if reference.SourceEventCount == current.SourceEventCount {
		want := current.Reference()
		if reference != want {
			return errors.New("digest does not match current Ledger")
		}
		return nil
	}
	for _, checkpoint := range current.CheckpointReferences {
		if checkpoint == reference {
			return nil
		}
	}
	return errors.New("reference is not a validated Checkpoint Ledger generation")
}

func validateLedgerEventIdentity(
	event Event,
	index int,
	ledger *ContextLedger,
	persisted *bool,
	perRunSequence map[string]uint64,
	seen map[string]struct{},
) error {
	if event.Type == "" {
		return errors.New("Event Type is required")
	}

	if index == 0 {
		*persisted = event.SessionRevision != 0
		ledger.SessionID = event.SessionID
	}
	if *persisted {
		if event.SessionID == "" || event.SessionID != ledger.SessionID {
			return errors.New("persisted Events must share one non-empty Session ID")
		}
		if event.SessionRevision != uint64(index+1) {
			return fmt.Errorf("Session revision = %d, want %d", event.SessionRevision, index+1)
		}
	} else if event.SessionRevision != 0 || event.SessionID != ledger.SessionID {
		return errors.New("ephemeral Events must preserve one logical Session identity and zero revision")
	}
	if event.RunID == "" || event.Sequence == 0 {
		if !*persisted {
			return errors.New("ephemeral Event Run ID and Sequence are required")
		}
		if event.RunID != "" || event.Sequence != 0 {
			return errors.New("legacy persisted Event must omit both Run ID and Sequence")
		}
		return nil
	}
	key := event.RunID + "\x00" + fmt.Sprint(event.Sequence)
	if _, duplicate := seen[key]; duplicate {
		return errors.New("Event Run ID and Sequence are duplicated")
	}
	seen[key] = struct{}{}
	if event.Sequence != perRunSequence[event.RunID]+1 {
		return fmt.Errorf("Run %q Sequence = %d, want %d", event.RunID, event.Sequence, perRunSequence[event.RunID]+1)
	}
	perRunSequence[event.RunID] = event.Sequence
	return nil
}

func contextLedgerSource(event Event) (ContextLedgerSource, error) {
	rawFields := ledgerRawFields(event)
	event = ledgerHashableEvent(event)
	encoded, err := json.Marshal(struct {
		Event     Event            `json:"event"`
		RawFields []ledgerRawField `json:"raw_fields"`
	}{Event: event, RawFields: rawFields})
	if err != nil {
		return ContextLedgerSource{}, fmt.Errorf("encode source Event: %w", err)
	}
	return ContextLedgerSource{
		ContextLedgerEventRef: ContextLedgerEventRef{
			RunID:           event.RunID,
			Sequence:        event.Sequence,
			SessionRevision: event.SessionRevision,
		},
		Type:        event.Type,
		ContentHash: contextLedgerJSONDigest(contextLedgerEventHashDomain, json.RawMessage(encoded)),
	}, nil
}

func reduceLegacyLedgerEvent(
	ledger *ContextLedger,
	constraintIndexes map[string]int,
	artifacts map[string]*ledgerArtifact,
	policies map[string]*ledgerPolicy,
	pendingApprovals map[string]string,
	event Event,
	ref ContextLedgerEventRef,
	sourceMessage int,
	order int,
) error {
	switch event.Type {
	case EventCurrentWorldStateCaptured:
		return errors.New("legacy current_world_state.captured is unsupported")
	case EventUserMessageAdded:
		if event.Message == nil {
			return errors.New("legacy user.message.added requires a Message")
		}
		if event.UserMessageOrigin != UserMessageOriginRunInput && event.UserMessageOrigin != UserMessageOriginSteering {
			return fmt.Errorf("legacy user.message.added has unsupported origin %q", event.UserMessageOrigin)
		}
		if event.UserMessageOrigin == UserMessageOriginSteering {
			if err := validateSteeringMessage(*event.Message); err != nil {
				return fmt.Errorf("legacy steering user.message.added: %w", err)
			}
		}
		if err := applyConstraintFactEvent(ledger, constraintIndexes, event, ref, sourceMessage); err != nil {
			return err
		}
	case EventContextCompacted:
		if event.ContextCompaction == nil {
			return errors.New("legacy context.compacted requires a compaction report")
		}
		if err := validateContextRebaseEvent(event); err != nil {
			return err
		}
		for _, reference := range event.ContextCompaction.Externalized {
			if err := ValidateEvidenceObjectRef(reference); err != nil || strings.TrimSpace(reference.MediaType) == "" {
				return errors.New("legacy context.compacted contains an invalid Evidence Object reference")
			}
			if err := addEvidenceArtifact(artifacts, reference, ref, order); err != nil {
				return err
			}
		}
	case EventRunWaiting:
		if event.WaitRequest != nil && event.WaitRequest.Kind == WaitKindApproval && event.WaitRequest.ID != "" {
			if _, exists := pendingApprovals[event.WaitRequest.ID]; exists {
				return fmt.Errorf("legacy approval request %q is already pending", event.WaitRequest.ID)
			}
			entry := approvalPolicyEntry(*event.WaitRequest, ref)
			key := approvalPolicyKey(ref, event.WaitRequest.ID)
			policies[key] = &ledgerPolicy{entry: entry, order: order}
			pendingApprovals[event.WaitRequest.ID] = key
		}
	case EventRunResumed:
		if event.WaitResponse == nil || event.WaitResponse.RequestID == "" {
			return errors.New("legacy run.resumed requires a Wait response identity")
		}
		if key := pendingApprovals[event.WaitResponse.RequestID]; key != "" {
			policy := policies[key]
			policy.entry.Outcome = approvalOutcome(event.WaitResponse.Payload)
			policy.entry.Sources = append(policy.entry.Sources, ref)
			delete(pendingApprovals, event.WaitResponse.RequestID)
		}
	}
	return nil
}

func validateContextRebaseEvent(event Event) error {
	report := event.ContextCompaction
	if report == nil {
		return nil
	}
	if err := validateContextValidationEvent(event); err != nil {
		return err
	}
	if report.Rebased != (report.RebaseReason != "") {
		return errors.New("context.compacted Rebase flag and reason are inconsistent")
	}
	if event.ContextCheckpoint != nil &&
		event.ContextCheckpoint.LastRebaseGeneration > event.ContextCheckpoint.Generation {
		return errors.New("context.compacted Checkpoint has an invalid last Rebase generation")
	}
	if !report.Rebased {
		return nil
	}
	if !report.Applied || event.ContextCheckpoint == nil {
		return errors.New("context.compacted Raw Event Rebase requires an applied Checkpoint")
	}
	if event.ContextCheckpoint.LastRebaseGeneration != event.ContextCheckpoint.Generation {
		return errors.New("context.compacted Raw Event Rebase does not identify its Checkpoint generation")
	}
	switch report.RebaseReason {
	case ContextRebaseInitial:
		if event.ContextCheckpoint.Generation != 1 {
			return errors.New("context.compacted initial Rebase requires Checkpoint generation 1")
		}
	case ContextRebaseGenerationInterval, ContextRebaseFactLifecycleChanged,
		ContextRebaseCheckpointInconsistent:
	default:
		return fmt.Errorf("context.compacted has unsupported Rebase reason %q", report.RebaseReason)
	}
	return nil
}

func ledgerEventMessageCount(event Event) int {
	switch event.Type {
	case EventUserMessageAdded, EventMessageCompleted, EventToolCompleted:
		if event.Message != nil {
			return 1
		}
	}
	return 0
}

func applyConstraintFactEvent(
	ledger *ContextLedger,
	constraintIndexes map[string]int,
	event Event,
	ref ContextLedgerEventRef,
	sourceMessage int,
) error {
	if event.Message == nil {
		return errors.New("Fact lifecycle Event requires a Message")
	}
	if event.Message.FactDirective != nil {
		return errors.New("user.message.added Message must not retain its Fact lifecycle directive")
	}
	directive := event.FactDirective
	if directive != nil {
		if event.Message.Role != RoleUser {
			return fmt.Errorf("%w: directive requires a user Message", ErrInvalidFactDirective)
		}
		message := cloneMessage(*event.Message)
		message.FactDirective = cloneFactLifecycleDirective(directive)
		if err := validateFactDirectiveMessage(message); err != nil {
			return err
		}
	}
	if event.Message.Role != RoleUser {
		return nil
	}

	if directive != nil {
		for _, target := range directive.Targets {
			entryIndex, exists := constraintIndexes[target]
			if !exists {
				return fmt.Errorf("%w: target %q does not exist in the earlier Event prefix", ErrInvalidFactDirective, target)
			}
			if ledger.Constraints[entryIndex].State != FactActive {
				return fmt.Errorf(
					"%w: target %q is %q instead of active",
					ErrInvalidFactDirective,
					target,
					ledger.Constraints[entryIndex].State,
				)
			}
		}
	}

	var newFactID string
	if directive == nil || directive.Action == FactLifecycleSupersede {
		var err error
		newFactID, err = ConstraintFactID(ref)
		if err != nil {
			return err
		}
		if _, duplicate := constraintIndexes[newFactID]; duplicate {
			return fmt.Errorf("Constraint Fact %q is duplicated", newFactID)
		}
	}

	if directive != nil {
		for _, target := range directive.Targets {
			entry := &ledger.Constraints[constraintIndexes[target]]
			entry.StateSource = ref
			entry.Sources = append(entry.Sources, ref)
			switch directive.Action {
			case FactLifecycleSupersede:
				entry.State = FactSuperseded
				entry.SupersededBy = newFactID
			case FactLifecycleResolve:
				entry.State = FactResolved
			}
		}
		if directive.Action == FactLifecycleResolve {
			return nil
		}
	}

	contentHash, err := checkpointMessageHash(*event.Message)
	if err != nil {
		return fmt.Errorf("hash user Message: %w", err)
	}
	entry := ConstraintLedgerEntry{
		ID:            newFactID,
		Kind:          ConstraintLedgerUserInput,
		Text:          event.Message.Text,
		ContentHash:   contentHash,
		SourceMessage: sourceMessage,
		Origin:        event.UserMessageOrigin,
		State:         FactActive,
		StateSource:   ref,
		Sources:       []ContextLedgerEventRef{ref},
	}
	if directive != nil {
		entry.Supersedes = append([]string(nil), directive.Targets...)
	}
	constraintIndexes[entry.ID] = len(ledger.Constraints)
	ledger.Constraints = append(ledger.Constraints, entry)
	return nil
}

type ledgerRawField struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Digest  string `json:"digest"`
}

func ledgerRawFields(event Event) []ledgerRawField {
	var fields []ledgerRawField
	appendField := func(path string, value json.RawMessage) {
		fields = append(fields, ledgerRawField{
			Path:    path,
			Present: value != nil,
			Digest:  contextLedgerValueDigest(value),
		})
	}
	if event.Message != nil {
		for index, call := range event.Message.ToolCalls {
			appendField(fmt.Sprintf("message.tool_calls[%d].arguments", index), call.Arguments)
		}
	}
	if event.ToolCall != nil {
		appendField("tool_call.arguments", event.ToolCall.Arguments)
	}
	if event.WaitRequest != nil {
		appendField("wait_request.payload", event.WaitRequest.Payload)
	}
	if event.WaitResponse != nil {
		appendField("wait_response.payload", event.WaitResponse.Payload)
	}
	return fields
}

func ledgerHashableEvent(event Event) Event {
	if event.Message != nil {
		message := cloneMessage(*event.Message)
		for index := range message.ToolCalls {
			message.ToolCalls[index].Arguments = hashableRawMessage(message.ToolCalls[index].Arguments)
		}
		event.Message = &message
	}
	if event.ToolCall != nil {
		call := cloneToolCall(*event.ToolCall)
		call.Arguments = hashableRawMessage(call.Arguments)
		event.ToolCall = &call
	}
	if event.WaitRequest != nil {
		request := cloneWaitRequest(*event.WaitRequest)
		request.Payload = hashableRawMessage(request.Payload)
		event.WaitRequest = &request
	}
	if event.WaitResponse != nil {
		response := cloneWaitResponse(*event.WaitResponse)
		response.Payload = hashableRawMessage(response.Payload)
		event.WaitResponse = &response
	}
	return event
}

func hashableRawMessage(value json.RawMessage) json.RawMessage {
	if value == nil || json.Valid(value) {
		return append(json.RawMessage(nil), value...)
	}
	encoded, _ := json.Marshal("qed.raw.base64:" + base64.StdEncoding.EncodeToString(value))
	return encoded
}

func addToolOutputArtifact(
	artifacts map[string]*ledgerArtifact,
	event Event,
	ref ContextLedgerEventRef,
	order int,
) {
	output := event.ToolResult.Output
	mediaType := "text/plain; charset=utf-8"
	if json.Valid([]byte(output)) {
		mediaType = "application/json"
	}
	key := "tool-output\x00" + event.RunID + "\x00" + event.ToolCall.ID
	artifacts[key] = &ledgerArtifact{order: order, entry: ArtifactLedgerEntry{
		ID:        contextLedgerID("artifact", "tool-output", event.RunID, event.ToolCall.ID),
		Kind:      ArtifactLedgerToolOutput,
		Name:      event.ToolCall.Name,
		Digest:    contextLedgerValueDigest([]byte(output)),
		Bytes:     int64(len(output)),
		MediaType: mediaType,
		Sources:   []ContextLedgerEventRef{ref},
	}}
}

func addEvidenceArtifact(
	artifacts map[string]*ledgerArtifact,
	reference EvidenceObjectRef,
	ref ContextLedgerEventRef,
	order int,
) error {
	identity := reference.Identity()
	key := "evidence\x00" + identity
	if current := artifacts[key]; current != nil {
		if current.entry.Bytes != reference.Bytes || current.entry.MediaType != reference.MediaType {
			return errors.New("Evidence Object metadata changes for one digest")
		}
		current.entry.Sources = appendUniqueLedgerSourceRef(current.entry.Sources, ref)
		return nil
	}
	artifacts[key] = &ledgerArtifact{order: order, entry: ArtifactLedgerEntry{
		ID:        contextLedgerID("artifact", "evidence", identity),
		Kind:      ArtifactLedgerEvidenceObject,
		Name:      "evidence_object",
		Digest:    reference.Digest,
		Bytes:     reference.Bytes,
		MediaType: reference.MediaType,
		Sources:   []ContextLedgerEventRef{ref},
	}}
	return nil
}

func addToolPolicy(
	policies map[string]*ledgerPolicy,
	event Event,
	ref ContextLedgerEventRef,
	order int,
) error {
	decision := event.ToolResult.Policy
	if strings.TrimSpace(decision.Outcome) != decision.Outcome || decision.Outcome == "" ||
		(decision.ReasonDigest != "" && !validSHA256Digest(decision.ReasonDigest)) {
		return errors.New("tool.completed contains an invalid Policy decision")
	}
	capabilities := append([]string(nil), decision.Capabilities...)
	for _, capability := range capabilities {
		if capability == "" || strings.TrimSpace(capability) != capability {
			return errors.New("tool.completed contains an invalid Policy capability")
		}
	}
	sort.Strings(capabilities)
	for index := 1; index < len(capabilities); index++ {
		if capabilities[index] == capabilities[index-1] {
			return errors.New("tool.completed contains a duplicate Policy capability")
		}
	}
	outcome := PolicyLedgerOutcome(decision.Outcome)
	switch outcome {
	case PolicyLedgerAllowed, PolicyLedgerDenied, PolicyLedgerAsked:
	default:
		return fmt.Errorf("tool.completed contains unsupported Policy outcome %q", outcome)
	}
	key := "tool-policy\x00" + event.RunID + "\x00" + event.ToolCall.ID
	policies[key] = &ledgerPolicy{order: order, entry: PolicyLedgerEntry{
		ID:           contextLedgerID("policy", "tool", event.RunID, event.ToolCall.ID),
		Kind:         PolicyLedgerToolAuthorization,
		Subject:      event.ToolCall.Name,
		Outcome:      outcome,
		Capabilities: capabilities,
		ReasonDigest: decision.ReasonDigest,
		Sources:      []ContextLedgerEventRef{ref},
	}}
	return nil
}

func approvalPolicyEntry(request WaitRequest, ref ContextLedgerEventRef) PolicyLedgerEntry {
	var payload struct {
		Tool         string   `json:"tool"`
		Capabilities []string `json:"capabilities"`
	}
	decoder := json.NewDecoder(bytes.NewReader(request.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || !decoderAtEOF(decoder) {
		payload = struct {
			Tool         string   `json:"tool"`
			Capabilities []string `json:"capabilities"`
		}{}
	}
	capabilities := append([]string(nil), payload.Capabilities...)
	sort.Strings(capabilities)
	return PolicyLedgerEntry{
		ID:           contextLedgerID("policy", approvalPolicyKey(ref, request.ID)),
		Kind:         PolicyLedgerHumanApproval,
		Subject:      payload.Tool,
		Outcome:      PolicyLedgerPending,
		Capabilities: capabilities,
		Sources:      []ContextLedgerEventRef{ref},
	}
}

func approvalPolicyKey(ref ContextLedgerEventRef, requestID string) string {
	scope := ref.RunID
	position := ref.Sequence
	if scope == "" {
		scope = "legacy-session"
		position = ref.SessionRevision
	}
	return "approval\x00" + scope + "\x00" + fmt.Sprint(position) + "\x00" + requestID
}

func approvalOutcome(payload json.RawMessage) PolicyLedgerOutcome {
	var response struct {
		Approved *bool `json:"approved"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || response.Approved == nil || !decoderAtEOF(decoder) {
		return PolicyLedgerUnknown
	}
	if *response.Approved {
		return PolicyLedgerAllowed
	}
	return PolicyLedgerDenied
}

func decoderAtEOF(decoder *json.Decoder) bool {
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}

func orderedExecutions(values map[string]*ledgerExecution) []ExecutionLedgerEntry {
	ordered := make([]*ledgerExecution, 0, len(values))
	for _, value := range values {
		ordered = append(ordered, value)
	}
	sort.Slice(ordered, func(first, second int) bool {
		if ordered[first].order != ordered[second].order {
			return ordered[first].order < ordered[second].order
		}
		return ordered[first].entry.ID < ordered[second].entry.ID
	})
	result := make([]ExecutionLedgerEntry, len(ordered))
	for index, value := range ordered {
		result[index] = value.entry
	}
	return result
}

func orderedArtifacts(values map[string]*ledgerArtifact) []ArtifactLedgerEntry {
	ordered := make([]*ledgerArtifact, 0, len(values))
	for _, value := range values {
		ordered = append(ordered, value)
	}
	sort.Slice(ordered, func(first, second int) bool {
		if ordered[first].order != ordered[second].order {
			return ordered[first].order < ordered[second].order
		}
		return ordered[first].entry.ID < ordered[second].entry.ID
	})
	result := make([]ArtifactLedgerEntry, len(ordered))
	for index, value := range ordered {
		result[index] = value.entry
	}
	return result
}

func orderedPolicies(values map[string]*ledgerPolicy) []PolicyLedgerEntry {
	ordered := make([]*ledgerPolicy, 0, len(values))
	for _, value := range values {
		ordered = append(ordered, value)
	}
	sort.Slice(ordered, func(first, second int) bool {
		if ordered[first].order != ordered[second].order {
			return ordered[first].order < ordered[second].order
		}
		return ordered[first].entry.ID < ordered[second].entry.ID
	})
	result := make([]PolicyLedgerEntry, len(ordered))
	for index, value := range ordered {
		result[index] = value.entry
	}
	return result
}

func finalizeContextLedger(
	ledger ContextLedger,
	sources []ContextLedgerSource,
	tasks map[string]*ledgerTask,
	taskOrder []string,
	executions map[string]*ledgerExecution,
	artifacts map[string]*ledgerArtifact,
	policies map[string]*ledgerPolicy,
) (ContextLedger, error) {
	ledger.Sources = append([]ContextLedgerSource(nil), sources...)
	ledger.Tasks = make([]TaskLedgerEntry, 0, len(taskOrder))
	for _, runID := range taskOrder {
		ledger.Tasks = append(ledger.Tasks, tasks[runID].entry)
	}
	ledger.Executions = orderedExecutions(executions)
	ledger.Artifacts = orderedArtifacts(artifacts)
	ledger.Policies = orderedPolicies(policies)
	if err := validateConstraintFacts(ledger.Constraints); err != nil {
		return ContextLedger{}, err
	}
	ledger.SourceEventCount = len(ledger.Sources)
	ledger.SessionRevision = 0
	if len(ledger.Sources) > 0 {
		ledger.SessionRevision = ledger.Sources[len(ledger.Sources)-1].SessionRevision
	}
	ledger.SourceHash = contextLedgerJSONDigest(contextLedgerSourceHashDomain, ledger.Sources)
	digest, err := contextLedgerSnapshotDigest(ledger)
	if err != nil {
		return ContextLedger{}, err
	}
	ledger.Digest = digest
	return ledger, nil
}

func validateConstraintFacts(entries []ConstraintLedgerEntry) error {
	byID := make(map[string]int, len(entries))
	lastSourceMessage := -1
	for index := range entries {
		entry := entries[index]
		if !validConstraintFactID(entry.ID) || entry.Kind != ConstraintLedgerUserInput ||
			!validSHA256Digest(entry.ContentHash) || len(entry.Sources) == 0 ||
			entry.SourceMessage <= lastSourceMessage {
			return fmt.Errorf("Constraint Fact %d has an invalid identity or source", index)
		}
		lastSourceMessage = entry.SourceMessage
		wantID, err := ConstraintFactID(entry.Sources[0])
		if err != nil || entry.ID != wantID {
			return fmt.Errorf("Constraint Fact %q does not match its source Event", entry.ID)
		}
		if entry.StateSource != entry.Sources[len(entry.Sources)-1] {
			return fmt.Errorf("Constraint Fact %q State source is not its latest source", entry.ID)
		}
		if _, duplicate := byID[entry.ID]; duplicate {
			return fmt.Errorf("Constraint Fact %q is duplicated", entry.ID)
		}
		byID[entry.ID] = index
	}

	for index := range entries {
		entry := entries[index]
		switch entry.State {
		case FactActive:
			if entry.StateSource != entry.Sources[0] || entry.SupersededBy != "" {
				return fmt.Errorf("active Constraint Fact %q has terminal transition state", entry.ID)
			}
		case FactSuperseded:
			if len(entry.Sources) < 2 || entry.StateSource == entry.Sources[0] || !validConstraintFactID(entry.SupersededBy) {
				return fmt.Errorf("superseded Constraint Fact %q has no valid replacement", entry.ID)
			}
		case FactResolved:
			if len(entry.Sources) < 2 || entry.StateSource == entry.Sources[0] || entry.SupersededBy != "" {
				return fmt.Errorf("resolved Constraint Fact %q names a replacement", entry.ID)
			}
		default:
			return fmt.Errorf("Constraint Fact %q has unsupported state %q", entry.ID, entry.State)
		}

		seenTargets := make(map[string]struct{}, len(entry.Supersedes))
		for _, target := range entry.Supersedes {
			if _, duplicate := seenTargets[target]; duplicate {
				return fmt.Errorf("Constraint Fact %q supersedes target %q more than once", entry.ID, target)
			}
			seenTargets[target] = struct{}{}
			targetIndex, exists := byID[target]
			if !exists {
				return fmt.Errorf("Constraint Fact %q supersedes missing target %q", entry.ID, target)
			}
			targetEntry := entries[targetIndex]
			if targetEntry.State != FactSuperseded || targetEntry.SupersededBy != entry.ID {
				return fmt.Errorf("Constraint Fact %q has inconsistent supersedes target %q", entry.ID, target)
			}
		}
		if entry.State == FactSuperseded {
			replacementIndex, exists := byID[entry.SupersededBy]
			if !exists {
				return fmt.Errorf("Constraint Fact %q replacement is missing", entry.ID)
			}
			replacement := entries[replacementIndex]
			found := false
			for _, target := range replacement.Supersedes {
				if target == entry.ID {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("Constraint Fact %q replacement does not link back", entry.ID)
			}
		}
	}

	visiting := make(map[string]bool, len(entries))
	visited := make(map[string]bool, len(entries))
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("Constraint Fact supersedes relation contains a cycle at %q", id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, target := range entries[byID[id]].Supersedes {
			if err := visit(target); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func appendUniqueLedgerSourceRef(current []ContextLedgerEventRef, value ContextLedgerEventRef) []ContextLedgerEventRef {
	for _, existing := range current {
		if existing == value {
			return current
		}
	}
	return append(current, value)
}

func containsContextLedgerReference(
	references []ContextLedgerReference,
	want ContextLedgerReference,
) bool {
	for _, reference := range references {
		if reference == want {
			return true
		}
	}
	return false
}

func contextLedgerSnapshotDigest(ledger ContextLedger) (string, error) {
	ledger.Digest = ""
	encoded, err := json.Marshal(ledger)
	if err != nil {
		return "", fmt.Errorf("encode Context Ledger: %w", err)
	}
	return contextLedgerJSONDigest(contextLedgerDigestDomain, json.RawMessage(encoded)), nil
}

func contextLedgerJSONDigest(domain string, value any) string {
	encoded, _ := json.Marshal(value)
	return contextLedgerDigestParts(domain, encoded)
}

func contextLedgerValueDigest(value []byte) string {
	return contextLedgerDigestParts(contextLedgerValueHashDomain, value)
}

func contextLedgerID(kind string, values ...string) string {
	parts := make([][]byte, 0, len(values)+1)
	parts = append(parts, []byte(kind))
	for _, value := range values {
		parts = append(parts, []byte(value))
	}
	digest := contextLedgerDigestParts("qed.context.ledger.id.v1", parts...)
	return kind + "_" + strings.TrimPrefix(digest, "sha256:")
}

func contextLedgerDigestParts(domain string, values ...[]byte) string {
	hash := sha256.New()
	writeHashPart(hash, []byte(domain))
	for _, value := range values {
		writeHashPart(hash, value)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func providerExecutionKey(runID string, call int) string {
	return "provider\x00" + runID + "\x00" + fmt.Sprint(call)
}

func toolExecutionKey(runID, callID string) string {
	return "tool\x00" + runID + "\x00" + callID
}

func cloneContextLedgerPointer(ledger *ContextLedger) *ContextLedger {
	if ledger == nil {
		return nil
	}
	cloned := *ledger
	if ledger.Sources != nil {
		cloned.Sources = append(make([]ContextLedgerSource, 0, len(ledger.Sources)), ledger.Sources...)
	}
	if ledger.CheckpointReferences != nil {
		cloned.CheckpointReferences = append(
			make([]ContextLedgerReference, 0, len(ledger.CheckpointReferences)),
			ledger.CheckpointReferences...,
		)
	}
	cloned.Artifacts = make([]ArtifactLedgerEntry, len(ledger.Artifacts))
	for index := range ledger.Artifacts {
		cloned.Artifacts[index] = ledger.Artifacts[index]
		cloned.Artifacts[index].Sources = append([]ContextLedgerEventRef(nil), ledger.Artifacts[index].Sources...)
	}
	cloned.Executions = make([]ExecutionLedgerEntry, len(ledger.Executions))
	for index := range ledger.Executions {
		cloned.Executions[index] = ledger.Executions[index]
		cloned.Executions[index].Sources = append([]ContextLedgerEventRef(nil), ledger.Executions[index].Sources...)
	}
	cloned.Constraints = make([]ConstraintLedgerEntry, len(ledger.Constraints))
	for index := range ledger.Constraints {
		cloned.Constraints[index] = ledger.Constraints[index]
		cloned.Constraints[index].Supersedes = append([]string(nil), ledger.Constraints[index].Supersedes...)
		cloned.Constraints[index].Sources = append([]ContextLedgerEventRef(nil), ledger.Constraints[index].Sources...)
	}
	cloned.Policies = make([]PolicyLedgerEntry, len(ledger.Policies))
	for index := range ledger.Policies {
		cloned.Policies[index] = ledger.Policies[index]
		cloned.Policies[index].Capabilities = append([]string(nil), ledger.Policies[index].Capabilities...)
		cloned.Policies[index].Sources = append([]ContextLedgerEventRef(nil), ledger.Policies[index].Sources...)
	}
	cloned.Tasks = make([]TaskLedgerEntry, len(ledger.Tasks))
	for index := range ledger.Tasks {
		cloned.Tasks[index] = ledger.Tasks[index]
		cloned.Tasks[index].Sources = append([]ContextLedgerEventRef(nil), ledger.Tasks[index].Sources...)
	}
	return &cloned
}
