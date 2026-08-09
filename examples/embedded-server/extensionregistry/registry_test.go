package extensionregistry_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/qed-runtime/qed/extension/selfexec"
)

func TestGeneratedCatalogIsCurrent(t *testing.T) {
	t.Parallel()

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve registry directory")
	}
	directory := filepath.Dir(sourceFile)
	source, err := selfexec.Generate(filepath.Join(directory, "..", "extensions.lock"), selfexec.GenerateOptions{
		PackageName:  "extensionregistry",
		VariableName: "Catalog",
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := selfexec.CheckGenerated(filepath.Join(directory, "registry_gen.go"), source)
	if err != nil || !current {
		t.Fatalf("generated catalog is stale: current=%t error=%v", current, err)
	}
}
