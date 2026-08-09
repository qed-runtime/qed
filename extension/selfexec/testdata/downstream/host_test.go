package downstream_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"example.com/downstream/extensionregistry"
	"github.com/qed-runtime/qed"
	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/extension/host"
	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/extension/selfexec"
)

func TestMain(testingMain *testing.M) {
	if len(os.Args) >= 2 && os.Args[1] == selfexec.ChildArgument {
		handled, err := extensionregistry.Catalog.Dispatch(context.Background(), selfexec.DispatchOptions{
			Arguments: os.Args[1:],
			Input:     os.Stdin,
			Output:    os.Stdout,
		})
		if !handled || err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(testingMain.Run())
}

func TestGeneratedCatalogRunsWithoutQEDInternalImports(t *testing.T) {
	definition, registered := extensionregistry.Catalog.Lookup("downstream.greeting")
	if !registered {
		t.Fatal("downstream Extension is not registered")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command, err := definition.Command(executable)
	if err != nil {
		t.Fatal(err)
	}
	expected := definition.Manifest.ProtocolManifest()
	process, err := host.Start(context.Background(), host.ProcessOptions{
		Command:          command,
		ExpectedID:       definition.Manifest.ID,
		ExpectedVersion:  definition.Manifest.Version,
		ExpectedManifest: &expected,
		Initialize:       protocol.InitializeRequest{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := process.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	tools := process.Tools(1)
	if len(tools) != 1 || tools[0].Definition().Name != "greet" {
		t.Fatalf("Tools() = %#v", tools)
	}
	result, err := tools[0].Execute(context.Background(), agent.ToolCall{
		ID:        "call-1",
		Name:      "greet",
		Arguments: json.RawMessage(`{}`),
	})
	if err != nil || result.Output != "hello from downstream" {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
}

func TestPublicHostLoadsInDownstreamModule(t *testing.T) {
	directory := t.TempDir()
	configurationPath := filepath.Join(directory, "qed.json")
	if err := os.WriteFile(configurationPath, []byte(`{
		"version":1,
		"default_agent":"main",
		"providers":{"local":{"protocol":"echo"}},
		"extensions":{"downstream.greeting":{"mode":"self-exec"}},
		"profiles":{"custom":{
			"kind":"coding",
			"extensions":["downstream.greeting"],
			"capabilities":{"allow":["downstream.read"]}
		}},
		"agents":{"main":{"provider":"local","profile":"custom"}}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := qed.LoadHost(configurationPath, qed.HostLoadOptions{
		WorkspaceRoot:   t.TempDir(),
		SelfExecutable:  executable,
		SelfExecCatalog: extensionregistry.Catalog,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := loaded.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	outcome, err := loaded.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "downstream"}},
	}, nil)
	if err != nil || outcome.Result.Status != agent.RunStatusCompleted {
		t.Fatalf("Run() = %#v, %v", outcome, err)
	}
}
