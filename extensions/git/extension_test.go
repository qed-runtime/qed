package git_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/qed-runtime/qed/extension/protocol"
	gitextension "github.com/qed-runtime/qed/extensions/git"
)

func TestInitializeReturnsGitToolSet(t *testing.T) {
	t.Parallel()

	configuration, err := gitextension.MarshalConfiguration(gitextension.Options{
		Timeout:        2 * time.Second,
		MaxOutputBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := gitextension.Initialize(context.Background(), protocol.InitializeRequest{
		WorkspaceRoot: t.TempDir(),
		Environment:   map[string]string{},
		Configuration: configuration,
	})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(tools))
	for index, tool := range tools {
		names[index] = tool.Definition().Name
	}
	want := []string{gitextension.StatusToolName, gitextension.DiffToolName}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("Tool names = %v, want %v", names, want)
	}
	options := gitextension.ServerOptions()
	if options.ID != gitextension.ID || options.Version != gitextension.Version || options.Initialize == nil {
		t.Fatalf("ServerOptions() = %#v", options)
	}
}

func TestMarshalConfigurationRejectsGitEnvironment(t *testing.T) {
	t.Parallel()

	if _, err := gitextension.MarshalConfiguration(gitextension.Options{
		Environment: map[string]string{"PATH": "/bin"},
	}); err == nil {
		t.Fatal("MarshalConfiguration() accepted environment values")
	}
}
