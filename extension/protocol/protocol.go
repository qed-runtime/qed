// Package protocol defines the language-independent QED Extension Protocol v1
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// Version is the only Extension Protocol version supported by this package
	Version = 1
	// MaxFrameBytes bounds one encoded protocol envelope
	MaxFrameBytes = 8 << 20
)

// Method identifies one Extension lifecycle or component RPC operation
type Method string

// Extension Protocol v1 methods
const (
	MethodHandshake            Method = "handshake"
	MethodDescribe             Method = "describe"
	MethodInitialize           Method = "initialize"
	MethodRequiredCapabilities Method = "required_capabilities"
	MethodApprovalPreview      Method = "approval_preview"
	MethodInvokeTool           Method = "invoke_tool"
	MethodHandleEvent          Method = "handle_event"
	MethodInvokeCommand        Method = "invoke_command"
	MethodHealthCheck          Method = "health_check"
	MethodSnapshot             Method = "snapshot"
	MethodRestore              Method = "restore"
	MethodDrain                Method = "drain"
	MethodShutdown             Method = "shutdown"
	MethodCancel               Method = "cancel"
)

// Envelope is one framed request or response
//
// Requests have Method and Params. Responses have Result or Error. Every
// envelope has a non-empty correlation ID and an exact protocol Version
type Envelope struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	Method  Method          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// ErrorCode identifies a protocol-level failure
type ErrorCode int

// Protocol error codes
const (
	ErrorCodeInvalidRequest    ErrorCode = -32600
	ErrorCodeMethodNotFound    ErrorCode = -32601
	ErrorCodeInvalidParams     ErrorCode = -32602
	ErrorCodeInternal          ErrorCode = -32603
	ErrorCodeProtocolMismatch  ErrorCode = -32001
	ErrorCodeNotInitialized    ErrorCode = -32002
	ErrorCodeDraining          ErrorCode = -32003
	ErrorCodeRequestCanceled   ErrorCode = -32004
	ErrorCodeExtensionRejected ErrorCode = -32005
)

// RPCError is a stable, serializable protocol failure
type RPCError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// Error implements error
func (rpcError *RPCError) Error() string {
	if rpcError == nil {
		return ""
	}
	return fmt.Sprintf("extension RPC error %d: %s", rpcError.Code, rpcError.Message)
}

// HandshakeRequest proposes one exact protocol version
type HandshakeRequest struct {
	ProtocolVersion int `json:"protocol_version"`
}

// HandshakeResponse accepts one exact protocol version
type HandshakeResponse struct {
	ProtocolVersion  int    `json:"protocol_version"`
	ExtensionID      string `json:"extension_id"`
	ExtensionVersion string `json:"extension_version"`
}

// Manifest describes one Extension and every component it registers
type Manifest struct {
	ID              string              `json:"id"`
	Version         string              `json:"version"`
	ProtocolVersion int                 `json:"protocol_version"`
	Capabilities    []string            `json:"capabilities,omitempty"`
	Tools           []ToolDefinition    `json:"tools,omitempty"`
	Hooks           []string            `json:"hooks,omitempty"`
	Commands        []CommandDefinition `json:"commands,omitempty"`
}

// ToolDefinition is the wire representation of one Extension Tool
type ToolDefinition struct {
	Name                string          `json:"name"`
	Description         string          `json:"description,omitempty"`
	InputSchema         json.RawMessage `json:"input_schema,omitempty"`
	Capabilities        []string        `json:"capabilities,omitempty"`
	DynamicCapabilities bool            `json:"dynamic_capabilities,omitempty"`
}

// DescribeResponse returns the Extension manifest after Handshake
type DescribeResponse struct {
	Manifest Manifest `json:"manifest"`
}

// InitializeRequest supplies host-selected resources to an Extension
type InitializeRequest struct {
	WorkspaceRoot string            `json:"workspace_root,omitempty"`
	Environment   map[string]string `json:"environment,omitempty"`
	Configuration json.RawMessage   `json:"configuration,omitempty"`
	// Verbose enables safe debug diagnostics on the Extension stderr stream
	Verbose bool `json:"verbose,omitempty"`
}

// RunInfo identifies the Run that invoked a Tool without exposing Provider state
type RunInfo struct {
	RunID       string `json:"run_id,omitempty"`
	ParentRunID string `json:"parent_run_id,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
}

// ToolCall is the wire representation of one model-requested Tool invocation
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolResult is the wire representation of one Tool result
type ToolResult struct {
	CallID           string            `json:"call_id"`
	Name             string            `json:"name"`
	Output           string            `json:"output,omitempty"`
	IsError          bool              `json:"is_error,omitempty"`
	ContextOperation *ContextOperation `json:"context_operation,omitempty"`
}

// ContextOperation is content-free Tool metadata used for safe context cuts
type ContextOperation struct {
	// Kind is one of mutation, verification, commit, or subagent
	Kind string `json:"kind"`
}

// RequiredCapabilitiesRequest resolves invocation-specific permissions
type RequiredCapabilitiesRequest struct {
	Call ToolCall `json:"call"`
}

// RequiredCapabilitiesResponse contains additional permissions for one call
type RequiredCapabilitiesResponse struct {
	Capabilities []string `json:"capabilities,omitempty"`
}

// ApprovalPreviewRequest asks an Extension to describe one Tool call without
// executing it
type ApprovalPreviewRequest struct {
	Call ToolCall `json:"call"`
}

// ApprovalPreviewResponse contains optional bounded content for human review
type ApprovalPreviewResponse struct {
	Preview *ApprovalPreview `json:"preview,omitempty"`
}

// ApprovalPreview is the language-independent human-readable Tool summary
type ApprovalPreview struct {
	Summary string                  `json:"summary"`
	Details []ApprovalPreviewDetail `json:"details,omitempty"`
}

// ApprovalPreviewDetail is one labeled fact in an ApprovalPreview
type ApprovalPreviewDetail struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// InvokeToolRequest invokes one initialized Tool for one Run
type InvokeToolRequest struct {
	Run  RunInfo  `json:"run"`
	Call ToolCall `json:"call"`
}

// InvokeToolResponse contains one Tool result
type InvokeToolResponse struct {
	Result ToolResult `json:"result"`
}

// RunEvent is the language-independent representation delivered to a Hook
//
// Payload contains the complete public Agent Event JSON. Provider-private
// continuation state is never serialized into Payload
type RunEvent struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// HandleEventRequest delivers one Run Event before host persistence and publication
type HandleEventRequest struct {
	Run   RunInfo  `json:"run"`
	Event RunEvent `json:"event"`
}

// CommandDefinition describes one host-invoked Extension command
type CommandDefinition struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	Capabilities []string        `json:"capabilities,omitempty"`
}

// CommandCall is one host-requested Extension command invocation
type CommandCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CommandResult is the structured result of one Extension command
type CommandResult struct {
	Output json.RawMessage `json:"output,omitempty"`
}

// InvokeCommandRequest invokes one initialized Extension command
type InvokeCommandRequest struct {
	Run  RunInfo     `json:"run"`
	Call CommandCall `json:"call"`
}

// InvokeCommandResponse contains one Extension command result
type InvokeCommandResponse struct {
	Result CommandResult `json:"result"`
}

// HealthCheckResponse describes the current Extension process state
type HealthCheckResponse struct {
	Status      string `json:"status"`
	Initialized bool   `json:"initialized"`
	Draining    bool   `json:"draining"`
}

// SnapshotResponse carries opaque Extension-owned state
type SnapshotResponse struct {
	State json.RawMessage `json:"state"`
}

// RestoreRequest supplies state from an older compatible generation
type RestoreRequest struct {
	State json.RawMessage `json:"state"`
}

// CancelRequest identifies an in-flight request to cancel
type CancelRequest struct {
	RequestID string `json:"request_id"`
}

// Empty is the result of lifecycle methods with no response data
type Empty struct{}

// Marshal converts a typed value to protocol JSON
func Marshal(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// Unmarshal decodes one protocol value strictly into target
func Unmarshal(data json.RawMessage, target any) error {
	if target == nil {
		return errors.New("protocol decode target is required")
	}
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("protocol value contains trailing JSON")
		}
		return err
	}
	return nil
}
