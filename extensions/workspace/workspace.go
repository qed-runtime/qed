// Package workspace assembles reusable file Tools as a process-isolated Extension
package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/extension/server"
	"github.com/qed-runtime/qed/extensions/edit"
	"github.com/qed-runtime/qed/extensions/filesystem"
	workspacecore "github.com/qed-runtime/qed/workspace"
)

const (
	// ID is the stable Extension Protocol identity of the official Workspace Extension
	ID = "qed.workspace"
	// Version is the implementation version reported during Handshake
	Version = "0.1.0"
)

// Options configures resource limits for the Workspace Extension
type Options struct {
	Filesystem filesystem.Options
	Edit       edit.Options
}

// MarshalConfiguration encodes Workspace Extension limits for Initialize
func MarshalConfiguration(options Options) (json.RawMessage, error) {
	return protocol.Marshal(wireConfiguration{
		Filesystem: wireFilesystemOptions{
			MaxFileBytes:     options.Filesystem.MaxFileBytes,
			MaxOutputBytes:   options.Filesystem.MaxOutputBytes,
			MaxSearchFiles:   options.Filesystem.MaxSearchFiles,
			MaxSearchResults: options.Filesystem.MaxSearchResults,
			MaxReadLines:     options.Filesystem.MaxReadLines,
		},
		Edit: wireEditOptions{
			MaxPatchBytes: options.Edit.MaxPatchBytes,
			MaxFileBytes:  options.Edit.MaxFileBytes,
			MaxFiles:      options.Edit.MaxFiles,
		},
	})
}

// ServerOptions returns the official stateless Workspace Extension server configuration
func ServerOptions() server.Options {
	return server.Options{
		ID:         ID,
		Version:    Version,
		Initialize: Initialize,
	}
}

// Initialize constructs the Workspace Extension Tools from host-selected resources
func Initialize(ctx context.Context, request protocol.InitializeRequest) ([]agent.Tool, error) {
	if ctx == nil {
		return nil, errors.New("Workspace Extension context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.WorkspaceRoot == "" {
		return nil, errors.New("Workspace Extension workspace root is required")
	}
	options, err := decodeConfiguration(request.Configuration)
	if err != nil {
		return nil, err
	}
	scoped, err := workspacecore.New(request.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	filesystemTools, err := filesystem.NewTools(scoped, options.Filesystem)
	if err != nil {
		return nil, err
	}
	editTool, err := edit.NewTool(scoped, options.Edit)
	if err != nil {
		return nil, err
	}
	tools := make([]agent.Tool, 0, 3)
	tools = append(tools, filesystemTools...)
	tools = append(tools, editTool)
	return tools, nil
}

type wireConfiguration struct {
	Filesystem wireFilesystemOptions `json:"filesystem,omitempty"`
	Edit       wireEditOptions       `json:"edit,omitempty"`
}

type wireFilesystemOptions struct {
	MaxFileBytes     int64 `json:"max_file_bytes,omitempty"`
	MaxOutputBytes   int   `json:"max_output_bytes,omitempty"`
	MaxSearchFiles   int   `json:"max_search_files,omitempty"`
	MaxSearchResults int   `json:"max_search_results,omitempty"`
	MaxReadLines     int   `json:"max_read_lines,omitempty"`
}

type wireEditOptions struct {
	MaxPatchBytes int   `json:"max_patch_bytes,omitempty"`
	MaxFileBytes  int64 `json:"max_file_bytes,omitempty"`
	MaxFiles      int   `json:"max_files,omitempty"`
}

func decodeConfiguration(data json.RawMessage) (Options, error) {
	configuration := wireConfiguration{}
	if len(data) > 0 {
		if err := protocol.Unmarshal(data, &configuration); err != nil {
			return Options{}, fmt.Errorf("decode Workspace Extension configuration: %w", err)
		}
	}
	return Options{
		Filesystem: filesystem.Options{
			MaxFileBytes:     configuration.Filesystem.MaxFileBytes,
			MaxOutputBytes:   configuration.Filesystem.MaxOutputBytes,
			MaxSearchFiles:   configuration.Filesystem.MaxSearchFiles,
			MaxSearchResults: configuration.Filesystem.MaxSearchResults,
			MaxReadLines:     configuration.Filesystem.MaxReadLines,
		},
		Edit: edit.Options{
			MaxPatchBytes: configuration.Edit.MaxPatchBytes,
			MaxFileBytes:  configuration.Edit.MaxFileBytes,
			MaxFiles:      configuration.Edit.MaxFiles,
		},
	}, nil
}
