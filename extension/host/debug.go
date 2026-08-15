package host

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/qed-runtime/qed/extension/protocol"
)

const maximumDebugLineBytes = 64 << 10

type safeDebugForwarder struct {
	mu          sync.Mutex
	logger      *slog.Logger
	extensionID string
	pending     []byte
	dropping    bool
}

type childDebugRecord struct {
	Safe         bool            `json:"qed_safe_debug"`
	Message      string          `json:"msg"`
	Method       protocol.Method `json:"method"`
	DurationMS   int64           `json:"duration_ms"`
	ToolCount    int             `json:"tool_count"`
	HookCount    int             `json:"hook_count"`
	CommandCount int             `json:"command_count"`
	Verbose      bool            `json:"verbose"`
}

func newSafeDebugForwarder(logger *slog.Logger, extensionID string) *safeDebugForwarder {
	return &safeDebugForwarder{logger: logger, extensionID: extensionID}
}

func (forwarder *safeDebugForwarder) Write(data []byte) (int, error) {
	written := len(data)
	forwarder.mu.Lock()
	defer forwarder.mu.Unlock()
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			forwarder.append(data)
			break
		}
		forwarder.append(data[:newline])
		if !forwarder.dropping {
			forwarder.forward(forwarder.pending)
		}
		forwarder.pending = forwarder.pending[:0]
		forwarder.dropping = false
		data = data[newline+1:]
	}
	return written, nil
}

func (forwarder *safeDebugForwarder) append(data []byte) {
	if forwarder.dropping {
		return
	}
	remaining := maximumDebugLineBytes - len(forwarder.pending)
	if len(data) > remaining {
		forwarder.pending = forwarder.pending[:0]
		forwarder.dropping = true
		return
	}
	forwarder.pending = append(forwarder.pending, data...)
}

func (forwarder *safeDebugForwarder) forward(line []byte) {
	var record childDebugRecord
	if json.Unmarshal(line, &record) != nil || !record.Safe || !allowedChildDebugMessage(record.Message) {
		return
	}
	arguments := []any{
		"component", "extension_process",
		"extension_id", forwarder.extensionID,
	}
	if allowedProtocolMethod(record.Method) {
		arguments = append(arguments, "method", record.Method)
	}
	if record.DurationMS >= 0 {
		arguments = append(arguments, "duration_ms", record.DurationMS)
	}
	if record.Message == "extension.initialized" {
		arguments = append(arguments,
			"tool_count", record.ToolCount,
			"hook_count", record.HookCount,
			"command_count", record.CommandCount,
			"verbose", record.Verbose,
		)
	}
	forwarder.logger.Debug(record.Message, arguments...)
}

func allowedChildDebugMessage(message string) bool {
	switch message {
	case "extension.initialized", "extension.rpc.started", "extension.rpc.completed", "extension.rpc.failed":
		return true
	default:
		return false
	}
}

func allowedProtocolMethod(method protocol.Method) bool {
	switch method {
	case protocol.MethodHandshake,
		protocol.MethodDescribe,
		protocol.MethodInitialize,
		protocol.MethodRequiredCapabilities,
		protocol.MethodApprovalPreview,
		protocol.MethodInvokeTool,
		protocol.MethodHandleEvent,
		protocol.MethodInvokeCommand,
		protocol.MethodHealthCheck,
		protocol.MethodSnapshot,
		protocol.MethodRestore,
		protocol.MethodDrain,
		protocol.MethodShutdown,
		protocol.MethodCancel:
		return true
	default:
		return false
	}
}
