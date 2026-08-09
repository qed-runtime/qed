// Package selfexec provides the public catalog and child-process dispatcher
// used to embed Go Extensions in a host executable without changing the
// Extension Protocol boundary
package selfexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qed-runtime/qed/extension/host"
	"github.com/qed-runtime/qed/extension/manifest"
	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/extension/server"
)

const (
	// ChildArgument selects the private self-exec Extension child mode
	ChildArgument = "__extension"
)

var (
	// ErrInvalidInvocation indicates a malformed or unknown self-exec child request
	ErrInvalidInvocation = errors.New("invalid self-exec Extension invocation")
)

// ServerOptionsFactory constructs one linked Extension protocol server
type ServerOptionsFactory func() server.Options

// Definition binds one locked Extension declaration to its linked Go server
type Definition struct {
	// Manifest is the locked transport-independent Extension declaration
	Manifest manifest.Declaration
	// ServerOptions constructs the statically linked protocol server
	ServerOptions ServerOptionsFactory
}

// Catalog is an immutable set of linked self-exec Extension definitions
//
// Catalog is safe for concurrent use after construction
type Catalog struct {
	definitions map[string]Definition
	ids         []string
}

// NewCatalog validates and copies linked Extension definitions
func NewCatalog(definitions []Definition) (*Catalog, error) {
	catalog := &Catalog{
		definitions: make(map[string]Definition, len(definitions)),
		ids:         make([]string, 0, len(definitions)),
	}
	for index, definition := range definitions {
		if err := manifest.ValidateDeclaration(definition.Manifest); err != nil {
			return nil, fmt.Errorf("self-exec Extension definition %d: %w", index, err)
		}
		if definition.ServerOptions == nil {
			return nil, fmt.Errorf("self-exec Extension %q ServerOptions factory is required", definition.Manifest.ID)
		}
		if _, duplicate := catalog.definitions[definition.Manifest.ID]; duplicate {
			return nil, fmt.Errorf("self-exec Extension %q is registered more than once", definition.Manifest.ID)
		}
		definition.Manifest = cloneDeclaration(definition.Manifest)
		catalog.definitions[definition.Manifest.ID] = definition
		catalog.ids = append(catalog.ids, definition.Manifest.ID)
	}
	sort.Strings(catalog.ids)
	return catalog, nil
}

// MustNewCatalog constructs a Catalog or panics when generated definitions are invalid
//
// Generated catalog source uses MustNewCatalog because its lock file is
// validated before source generation
func MustNewCatalog(definitions []Definition) *Catalog {
	catalog, err := NewCatalog(definitions)
	if err != nil {
		panic(err)
	}
	return catalog
}

// IDs returns linked Extension IDs in lexical order
func (catalog *Catalog) IDs() []string {
	if catalog == nil {
		return nil
	}
	return append([]string(nil), catalog.ids...)
}

// Lookup returns one isolated linked Extension definition
func (catalog *Catalog) Lookup(id string) (Definition, bool) {
	if catalog == nil {
		return Definition{}, false
	}
	definition, registered := catalog.definitions[id]
	if !registered {
		return Definition{}, false
	}
	definition.Manifest = cloneDeclaration(definition.Manifest)
	return definition, true
}

// NewServerOptions constructs linked Server options and validates their identity
func (definition Definition) NewServerOptions() (server.Options, error) {
	if err := manifest.ValidateDeclaration(definition.Manifest); err != nil {
		return server.Options{}, err
	}
	if definition.ServerOptions == nil {
		return server.Options{}, errors.New("self-exec Extension ServerOptions factory is unavailable")
	}
	options := definition.ServerOptions()
	if options.ID != definition.Manifest.ID || options.Version != definition.Manifest.Version {
		return server.Options{}, fmt.Errorf(
			"self-exec Extension Server identity %q version %q does not match locked identity %q version %q",
			options.ID,
			options.Version,
			definition.Manifest.ID,
			definition.Manifest.Version,
		)
	}
	return options, nil
}

// Command returns the process command used to launch this definition from the
// supplied host executable
func (definition Definition) Command(executable string) (host.Command, error) {
	if strings.TrimSpace(executable) != executable || executable == "" || strings.IndexByte(executable, 0) >= 0 {
		return host.Command{}, errors.New("self-exec host executable is required and must not contain whitespace padding or NUL")
	}
	if !filepath.IsAbs(executable) {
		return host.Command{}, errors.New("self-exec host executable must be absolute")
	}
	if err := manifest.ValidateDeclaration(definition.Manifest); err != nil {
		return host.Command{}, err
	}
	return host.Command{
		Path: filepath.Clean(executable),
		Args: []string{ChildArgument, definition.Manifest.ID},
	}, nil
}

// DispatchOptions supplies one process invocation to Catalog.Dispatch
type DispatchOptions struct {
	// Arguments excludes argv[0]
	Arguments []string
	// Input is the child process protocol input, normally os.Stdin
	Input io.Reader
	// Output is the child process protocol output, normally os.Stdout
	Output io.Writer
	// DebugWriter receives safe Extension diagnostics after verbose Initialize
	DebugWriter io.Writer
}

// Dispatch serves a linked Extension when Arguments select child mode
//
// handled is false for an ordinary host invocation. A true handled result
// means the caller must terminate the current process after observing err
func (catalog *Catalog) Dispatch(ctx context.Context, options DispatchOptions) (handled bool, err error) {
	if len(options.Arguments) == 0 || options.Arguments[0] != ChildArgument {
		return false, nil
	}
	if ctx == nil {
		return true, errors.New("self-exec dispatch context must not be nil")
	}
	if len(options.Arguments) != 2 || options.Input == nil || options.Output == nil {
		return true, ErrInvalidInvocation
	}
	definition, registered := catalog.Lookup(options.Arguments[1])
	if !registered {
		return true, ErrInvalidInvocation
	}
	serverOptions, err := definition.NewServerOptions()
	if err != nil {
		return true, fmt.Errorf("prepare self-exec Extension %q: %w", definition.Manifest.ID, err)
	}
	if options.DebugWriter != nil {
		serverOptions.DebugWriter = options.DebugWriter
	}
	if err := server.Serve(ctx, options.Input, options.Output, serverOptions); err != nil {
		return true, fmt.Errorf("serve self-exec Extension %q: %w", definition.Manifest.ID, err)
	}
	return true, nil
}

func cloneDeclaration(declaration manifest.Declaration) manifest.Declaration {
	declaration.Capabilities = append([]string(nil), declaration.Capabilities...)
	declaration.Hooks = append([]string(nil), declaration.Hooks...)
	declaration.Commands = append([]protocol.CommandDefinition(nil), declaration.Commands...)
	for index := range declaration.Commands {
		declaration.Commands[index].InputSchema = append(json.RawMessage(nil), declaration.Commands[index].InputSchema...)
		declaration.Commands[index].Capabilities = append([]string(nil), declaration.Commands[index].Capabilities...)
	}
	return declaration
}
