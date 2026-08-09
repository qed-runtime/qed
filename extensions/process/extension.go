package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/extension/server"
	"github.com/qed-runtime/qed/workspace"
)

const (
	// ID is the stable Extension Protocol identity of the official Process Extension
	ID = "qed.process"
	// Version is the implementation version reported during Handshake
	Version = "0.1.0"
)

// MarshalConfiguration encodes Process Extension limits for Initialize
func MarshalConfiguration(options Options) (json.RawMessage, error) {
	if options.Environment != nil {
		return nil, errors.New("Process Extension environment must be supplied by Initialize")
	}
	return protocol.Marshal(wireConfiguration{
		DefaultTimeoutNS: int64(options.DefaultTimeout),
		MaximumTimeoutNS: int64(options.MaximumTimeout),
		MaxOutputBytes:   options.MaxOutputBytes,
	})
}

// ServerOptions returns the official stateless Process Extension server configuration
func ServerOptions() server.Options {
	return server.Options{
		ID:         ID,
		Version:    Version,
		Initialize: Initialize,
	}
}

// Initialize constructs the Process Extension Tool from host-selected resources
func Initialize(ctx context.Context, request protocol.InitializeRequest) ([]agent.Tool, error) {
	if ctx == nil {
		return nil, errors.New("Process Extension context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.WorkspaceRoot == "" {
		return nil, errors.New("Process Extension workspace root is required")
	}
	options, err := decodeConfiguration(request.Configuration)
	if err != nil {
		return nil, err
	}
	options.Environment = cloneEnvironment(request.Environment)
	scoped, err := workspace.New(request.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	tool, err := NewTool(scoped, options)
	if err != nil {
		return nil, err
	}
	return []agent.Tool{tool}, nil
}

type wireConfiguration struct {
	DefaultTimeoutNS int64 `json:"default_timeout_ns,omitempty"`
	MaximumTimeoutNS int64 `json:"maximum_timeout_ns,omitempty"`
	MaxOutputBytes   int   `json:"max_output_bytes,omitempty"`
}

func decodeConfiguration(data json.RawMessage) (Options, error) {
	configuration := wireConfiguration{}
	if len(data) > 0 {
		if err := protocol.Unmarshal(data, &configuration); err != nil {
			return Options{}, fmt.Errorf("decode Process Extension configuration: %w", err)
		}
	}
	return Options{
		DefaultTimeout: time.Duration(configuration.DefaultTimeoutNS),
		MaximumTimeout: time.Duration(configuration.MaximumTimeoutNS),
		MaxOutputBytes: configuration.MaxOutputBytes,
	}, nil
}

func cloneEnvironment(environment map[string]string) map[string]string {
	cloned := make(map[string]string, len(environment))
	for name, value := range environment {
		cloned[name] = value
	}
	return cloned
}
