package process_test

import (
	"context"
	"testing"
	"time"

	"github.com/qed-runtime/qed/extension/protocol"
	processextension "github.com/qed-runtime/qed/extensions/process"
)

func TestInitializeReturnsProcessTool(t *testing.T) {
	t.Parallel()

	configuration, err := processextension.MarshalConfiguration(processextension.Options{
		DefaultTimeout: 2 * time.Second,
		MaximumTimeout: 5 * time.Second,
		MaxOutputBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := processextension.Initialize(context.Background(), protocol.InitializeRequest{
		WorkspaceRoot: t.TempDir(),
		Environment:   map[string]string{},
		Configuration: configuration,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Definition().Name != processextension.RunCommandToolName {
		t.Fatalf("Tools = %#v", tools)
	}
	options := processextension.ServerOptions()
	if options.ID != processextension.ID || options.Version != processextension.Version || options.Initialize == nil {
		t.Fatalf("ServerOptions() = %#v", options)
	}
}

func TestMarshalConfigurationRejectsProcessEnvironment(t *testing.T) {
	t.Parallel()

	if _, err := processextension.MarshalConfiguration(processextension.Options{
		Environment: map[string]string{"PATH": "/bin"},
	}); err == nil {
		t.Fatal("MarshalConfiguration() accepted environment values")
	}
}
