package manifest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/qed-runtime/qed/extension/manifest"
	"github.com/qed-runtime/qed/extension/protocol"
)

func TestDeclarationProducesIsolatedProtocolManifest(t *testing.T) {
	t.Parallel()

	declaration := manifest.Declaration{
		ID:              "test-extension",
		Version:         "v1",
		ProtocolVersion: protocol.Version,
		Capabilities:    []string{"test.read"},
		Hooks:           []string{"run.started"},
		Commands: []protocol.CommandDefinition{{
			Name:         "inspect",
			InputSchema:  json.RawMessage(`{"type":"object"}`),
			Capabilities: []string{"test.read"},
		}},
	}
	if err := manifest.ValidateDeclaration(declaration); err != nil {
		t.Fatal(err)
	}
	converted := declaration.ProtocolManifest()
	converted.Capabilities[0] = "changed"
	converted.Hooks[0] = "changed"
	converted.Commands[0].InputSchema[0] = '['
	converted.Commands[0].Capabilities[0] = "changed"
	if declaration.Capabilities[0] != "test.read" || declaration.Hooks[0] != "run.started" ||
		string(declaration.Commands[0].InputSchema) != `{"type":"object"}` || declaration.Commands[0].Capabilities[0] != "test.read" {
		t.Fatalf("ProtocolManifest() mutated Declaration: %#v", declaration)
	}
}

func TestLoadResolvesValidatedEntrypoint(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	entrypoint := filepath.Join(directory, "extension")
	if err := os.WriteFile(entrypoint, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	document := `{
  "id": "test-extension",
  "version": "v1",
  "protocol_version": 1,
  "entrypoint": "extension",
  "capabilities": ["test.read"],
  "hooks": ["run.started"],
  "commands": [{"name":"inspect","input_schema":{"type":"object"},"capabilities":["test.read"]}]
}`
	if err := os.WriteFile(filepath.Join(directory, manifest.Filename), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := manifest.Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	canonicalEntrypoint, err := filepath.EvalSymlinks(entrypoint)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Manifest.ID != "test-extension" || resolved.Entrypoint != canonicalEntrypoint {
		t.Fatalf("Resolved = %#v", resolved)
	}
}

func TestLoadRejectsEntrypointEscapeAndAmbiguousJSON(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "extension")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []string{
		`{"id":"one","id":"two","version":"v1","protocol_version":1,"entrypoint":"extension"}`,
		`{"id":"one","version":"v1","protocol_version":1,"entrypoint":"../outside"}`,
	}
	for index, document := range tests {
		if err := os.WriteFile(filepath.Join(directory, manifest.Filename), []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := manifest.Load(directory); err == nil {
			t.Fatalf("Load() case %d succeeded", index)
		}
	}
}

func TestLoadForDevelopmentAllowsMissingBuildOutput(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	manifestPath := filepath.Join(directory, manifest.Filename)
	if err := os.WriteFile(manifestPath, []byte(`{
		"id":"development-extension",
		"version":"v1",
		"protocol_version":1,
		"entrypoint":"bin/development-extension"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := manifest.LoadForDevelopment(directory)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Entrypoint != filepath.Join(resolved.Directory, "bin", "development-extension") {
		t.Fatalf("Entrypoint = %q", resolved.Entrypoint)
	}
	if _, err := manifest.Load(directory); err == nil {
		t.Fatal("Load() accepted a missing runtime entrypoint")
	}
}

func TestDiscoverRejectsDuplicateExtensionIDs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for _, name := range []string{"first", "second"} {
		directory := filepath.Join(root, name)
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "extension"), []byte("binary"), 0o700); err != nil {
			t.Fatal(err)
		}
		document := `{"id":"duplicate","version":"v1","protocol_version":1,"entrypoint":"extension"}`
		if err := os.WriteFile(filepath.Join(directory, manifest.Filename), []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manifest.Discover(root); err == nil {
		t.Fatal("Discover() succeeded with duplicate IDs")
	}
}
