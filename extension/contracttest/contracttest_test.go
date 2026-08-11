package contracttest_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/extension/contracttest"
	"github.com/qed-runtime/qed/extension/host"
	"github.com/qed-runtime/qed/extension/manifest"
	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/extension/selfexec"
	"github.com/qed-runtime/qed/extension/server"
)

const (
	lifecycleFixtureID        = "qed.contracttest.lifecycle"
	lifecycleFixtureVersion   = "1.0.0"
	lifecycleExternalArgument = "__qed_extension_lifecycle_contract_test"
)

func TestMain(testingMain *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == contracttest.ExternalChildArgument {
		options := contracttest.ServerOptions()
		options.DebugWriter = os.Stderr
		if err := server.Serve(context.Background(), os.Stdin, os.Stdout, options); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 2 && os.Args[1] == lifecycleExternalArgument {
		if err := server.Serve(context.Background(), os.Stdin, os.Stdout, lifecycleServerOptions()); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	catalog := selfexec.MustNewCatalog([]selfexec.Definition{{
		Manifest:      contracttest.Declaration(),
		ServerOptions: contracttest.ServerOptions,
	}, {
		Manifest:      lifecycleDeclaration(),
		ServerOptions: lifecycleServerOptions,
	}})
	handled, err := catalog.Dispatch(context.Background(), selfexec.DispatchOptions{
		Arguments:   os.Args[1:],
		Input:       os.Stdin,
		Output:      os.Stdout,
		DebugWriter: os.Stderr,
	})
	if handled {
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(testingMain.Run())
}

func TestExternalExecutableContract(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contracttest.Run(t, contracttest.SuiteOptions{
		Command: host.Command{
			Path: executable,
			Args: []string{contracttest.ExternalChildArgument},
		},
	})
}

func TestSelfExecContract(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	definition := selfexec.Definition{
		Manifest:      contracttest.Declaration(),
		ServerOptions: contracttest.ServerOptions,
	}
	command, err := definition.Command(executable)
	if err != nil {
		t.Fatal(err)
	}
	contracttest.Run(t, contracttest.SuiteOptions{Command: command})
}

func TestExternalExecutableLifecycle(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contracttest.RunLifecycle(t, contracttest.LifecycleOptions{
		Command: host.Command{
			Path: executable,
			Args: []string{lifecycleExternalArgument},
		},
		Declaration: lifecycleDeclaration(),
	})
}

func TestSelfExecLifecycle(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	definition := selfexec.Definition{
		Manifest:      lifecycleDeclaration(),
		ServerOptions: lifecycleServerOptions,
	}
	command, err := definition.Command(executable)
	if err != nil {
		t.Fatal(err)
	}
	contracttest.RunLifecycle(t, contracttest.LifecycleOptions{
		Command:     command,
		Declaration: lifecycleDeclaration(),
	})
}

func lifecycleDeclaration() manifest.Declaration {
	return manifest.Declaration{
		ID:              lifecycleFixtureID,
		Version:         lifecycleFixtureVersion,
		ProtocolVersion: protocol.Version,
	}
}

func lifecycleServerOptions() server.Options {
	return server.Options{
		ID:      lifecycleFixtureID,
		Version: lifecycleFixtureVersion,
		Initialize: func(context.Context, protocol.InitializeRequest) ([]agent.Tool, error) {
			return nil, nil
		},
	}
}
