package extensionregistry_test

import (
	"reflect"
	"testing"

	gitextension "github.com/qed-runtime/qed/extensions/git"
	processextension "github.com/qed-runtime/qed/extensions/process"
	workspaceextension "github.com/qed-runtime/qed/extensions/workspace"
	"github.com/qed-runtime/qed/internal/extensionregistry"
)

func TestRegistryContainsOfficialExtensions(t *testing.T) {
	t.Parallel()

	want := []string{gitextension.ID, processextension.ID, workspaceextension.ID}
	if got := extensionregistry.IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs() = %v, want %v", got, want)
	}
	for _, id := range want {
		definition, registered := extensionregistry.Lookup(id)
		if !registered || definition.Manifest.ID != id {
			t.Errorf("Lookup(%q) = %#v, %t", id, definition, registered)
			continue
		}
		options, err := definition.NewServerOptions()
		if err != nil || options.ID != id || options.Version != definition.Manifest.Version {
			t.Errorf("NewServerOptions(%q) = %#v, %v", id, options, err)
		}
	}
	if _, registered := extensionregistry.Lookup("coding-tools"); registered {
		t.Fatal("Lookup() retained the removed coding-tools alias")
	}
}
