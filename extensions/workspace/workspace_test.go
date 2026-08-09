package workspace_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/extensions/edit"
	"github.com/qed-runtime/qed/extensions/filesystem"
	workspaceextension "github.com/qed-runtime/qed/extensions/workspace"
)

func TestInitializeReturnsWorkspaceToolSet(t *testing.T) {
	t.Parallel()

	configuration, err := workspaceextension.MarshalConfiguration(workspaceextension.Options{
		Filesystem: filesystem.Options{MaxSearchResults: 12},
		Edit:       edit.Options{MaxFiles: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := workspaceextension.Initialize(context.Background(), protocol.InitializeRequest{
		WorkspaceRoot: t.TempDir(),
		Configuration: configuration,
	})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(tools))
	for index, tool := range tools {
		names[index] = tool.Definition().Name
	}
	want := []string{filesystem.SearchTextToolName, filesystem.ReadFileToolName, edit.ApplyPatchToolName}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("Tool names = %v, want %v", names, want)
	}
	options := workspaceextension.ServerOptions()
	if options.ID != workspaceextension.ID || options.Version != workspaceextension.Version || options.Initialize == nil {
		t.Fatalf("ServerOptions() = %#v", options)
	}
}

func TestInitializeRejectsUnknownWorkspaceConfiguration(t *testing.T) {
	t.Parallel()

	_, err := workspaceextension.Initialize(context.Background(), protocol.InitializeRequest{
		WorkspaceRoot: t.TempDir(),
		Configuration: []byte(`{"unknown":true}`),
	})
	if err == nil {
		t.Fatal("Initialize() accepted an unknown configuration field")
	}
}
