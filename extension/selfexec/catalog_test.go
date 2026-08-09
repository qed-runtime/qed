package selfexec_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/extension/manifest"
	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/extension/selfexec"
	"github.com/qed-runtime/qed/extension/server"
)

func TestCatalogCopiesDefinitionsAndBuildsCommands(t *testing.T) {
	t.Parallel()

	declaration := manifest.Declaration{
		ID:              "example",
		Version:         "1.0.0",
		ProtocolVersion: protocol.Version,
		Capabilities:    []string{"example.read"},
	}
	catalog, err := selfexec.NewCatalog([]selfexec.Definition{{
		Manifest: declaration,
		ServerOptions: func() server.Options {
			return testServerOptions("example", "1.0.0")
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	declaration.Capabilities[0] = "changed"
	if got := catalog.IDs(); !reflect.DeepEqual(got, []string{"example"}) {
		t.Fatalf("IDs() = %v", got)
	}
	definition, ok := catalog.Lookup("example")
	if !ok || !reflect.DeepEqual(definition.Manifest.Capabilities, []string{"example.read"}) {
		t.Fatalf("Lookup() = %#v, %t", definition, ok)
	}
	executable := filepath.Join(t.TempDir(), "host")
	command, err := definition.Command(executable)
	if err != nil {
		t.Fatal(err)
	}
	if command.Path != executable || !reflect.DeepEqual(command.Args, []string{selfexec.ChildArgument, "example"}) {
		t.Fatalf("Command() = %#v", command)
	}
	options, err := definition.NewServerOptions()
	if err != nil || options.ID != "example" || options.Version != "1.0.0" {
		t.Fatalf("NewServerOptions() = %#v, %v", options, err)
	}
}

func TestCatalogRejectsInvalidDefinitionsAndFactoryIdentity(t *testing.T) {
	t.Parallel()

	definition := selfexec.Definition{
		Manifest: manifest.Declaration{ID: "example", Version: "1.0.0", ProtocolVersion: protocol.Version},
		ServerOptions: func() server.Options {
			return testServerOptions("other", "1.0.0")
		},
	}
	if _, err := selfexec.NewCatalog([]selfexec.Definition{definition, definition}); err == nil {
		t.Fatal("NewCatalog() accepted duplicate definitions")
	}
	catalog, err := selfexec.NewCatalog([]selfexec.Definition{definition})
	if err != nil {
		t.Fatal(err)
	}
	registered, _ := catalog.Lookup("example")
	if _, err := registered.NewServerOptions(); err == nil {
		t.Fatal("NewServerOptions() accepted a mismatched factory identity")
	}
	if _, err := registered.Command("relative-host"); err == nil {
		t.Fatal("Command() accepted a relative executable")
	}
}

func TestCatalogDispatchesOnlyChildInvocations(t *testing.T) {
	t.Parallel()

	catalog := selfexec.MustNewCatalog([]selfexec.Definition{{
		Manifest: manifest.Declaration{ID: "example", Version: "1.0.0", ProtocolVersion: protocol.Version},
		ServerOptions: func() server.Options {
			return testServerOptions("example", "1.0.0")
		},
	}})
	handled, err := catalog.Dispatch(context.Background(), selfexec.DispatchOptions{
		Arguments: []string{"serve"},
		Input:     bytes.NewReader(nil),
		Output:    io.Discard,
	})
	if err != nil || handled {
		t.Fatalf("Dispatch(host) = %t, %v", handled, err)
	}
	handled, err = catalog.Dispatch(context.Background(), selfexec.DispatchOptions{
		Arguments: []string{selfexec.ChildArgument, "missing"},
		Input:     bytes.NewReader(nil),
		Output:    io.Discard,
	})
	if !handled || !errors.Is(err, selfexec.ErrInvalidInvocation) {
		t.Fatalf("Dispatch(missing) = %t, %v", handled, err)
	}
	handled, err = catalog.Dispatch(context.Background(), selfexec.DispatchOptions{
		Arguments: []string{selfexec.ChildArgument, "example"},
		Input:     bytes.NewReader(nil),
		Output:    io.Discard,
	})
	if !handled || err != nil {
		t.Fatalf("Dispatch(child) = %t, %v", handled, err)
	}
}

func testServerOptions(id, version string) server.Options {
	return server.Options{
		ID:      id,
		Version: version,
		Initialize: func(context.Context, protocol.InitializeRequest) ([]agent.Tool, error) {
			return nil, nil
		},
	}
}
