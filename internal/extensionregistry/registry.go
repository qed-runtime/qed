// Package extensionregistry contains the generated self-exec Extension catalog
package extensionregistry

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/qed-runtime/qed/extension/manifest"
	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/extension/server"
)

//go:generate go run ../../cmd/qed extension generate --lock ../../extensions.lock --output registry_gen.go

type catalogEntry struct {
	declaration   manifest.Declaration
	serverOptions func() server.Options
}

// Definition contains one locked manifest and its linked Go server
type Definition struct {
	Manifest      manifest.Declaration
	serverOptions func() server.Options
}

// Lookup returns one generated self-exec Extension definition
func Lookup(id string) (Definition, bool) {
	entry, registered := generatedCatalog[id]
	if !registered || entry.serverOptions == nil {
		return Definition{}, false
	}
	declaration := cloneDeclaration(entry.declaration)
	if err := manifest.ValidateDeclaration(declaration); err != nil || declaration.ID != id {
		return Definition{}, false
	}
	return Definition{Manifest: declaration, serverOptions: entry.serverOptions}, true
}

// NewServerOptions constructs the linked Server options and verifies their
// identity against the locked manifest
func (definition Definition) NewServerOptions() (server.Options, error) {
	if definition.serverOptions == nil {
		return server.Options{}, errors.New("self-exec Extension factory is unavailable")
	}
	options := definition.serverOptions()
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

// IDs returns generated self-exec Extension IDs in lexical order
func IDs() []string {
	ids := make([]string, 0, len(generatedCatalog))
	for id := range generatedCatalog {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
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
