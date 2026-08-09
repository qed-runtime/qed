package agentconfig

import (
	"path/filepath"
	"testing"

	"github.com/qed-runtime/qed/internal/extensionregistry"
)

func TestSelfExecUsesLockedManifestExpectation(t *testing.T) {
	t.Parallel()

	const extensionID = "qed.workspace"
	definition, registered := extensionregistry.Lookup(extensionID)
	if !registered {
		t.Fatal("qed.workspace is not registered")
	}
	configured, err := buildExtensionCommands(
		map[string]extensionProfile{extensionID: {Mode: "self-exec"}},
		nil,
		LoadOptions{SelfExecutable: filepath.Join(t.TempDir(), "qed")},
		t.TempDir(),
	)
	if err != nil {
		t.Fatal(err)
	}
	locked := configured[extensionID]
	if locked.expectedManifest == nil || locked.expectedVersion != definition.Manifest.Version ||
		locked.expectedManifest.ID != definition.Manifest.ID ||
		len(locked.expectedManifest.Capabilities) != len(definition.Manifest.Capabilities) {
		t.Fatalf("self-exec expectation = %#v", locked)
	}
}
