package extensionlock_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/internal/extensionlock"
)

func TestGenerateProducesDeterministicCatalog(t *testing.T) {
	t.Parallel()

	lockPath := writeLock(t, `{
		"version":1,
		"extensions":[
			{
				"go_package":"example.com/extensions/first",
				"factory":"NewServerOptions",
				"manifest":{"id":"second","version":"v2","protocol_version":1}
			},
			{
				"go_package":"example.com/extensions/first",
				"manifest":{
					"id":"first",
					"version":"v1",
					"protocol_version":1,
					"capabilities":["test.read"],
					"commands":[{
						"name":"inspect",
						"description":"Inspect state",
						"input_schema":{"type":"object"},
						"capabilities":["test.read"]
					}]
				}
			}
		]
	}`)
	source, err := extensionlock.Generate(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "registry_gen.go", source, parser.AllErrors); err != nil {
		t.Fatalf("generated source is invalid: %v\n%s", err, source)
	}
	text := string(source)
	if first, second := strings.Index(text, `"first":`), strings.Index(text, `"second":`); first < 0 || second < 0 || first >= second {
		t.Fatalf("generated catalog is not sorted by Extension ID:\n%s", text)
	}
	for _, fragment := range []string{
		`serverOptions: extension0.ServerOptions`,
		`serverOptions: extension0.NewServerOptions`,
		`json.RawMessage("{\"type\":\"object\"}")`,
	} {
		if !strings.Contains(text, fragment) {
			t.Errorf("generated source does not contain %q:\n%s", fragment, text)
		}
	}
	if count := strings.Count(text, `extension0 "example.com/extensions/first"`); count != 1 {
		t.Errorf("shared Go package import count = %d, want 1:\n%s", count, text)
	}

	outputPath := filepath.Join(t.TempDir(), "registry_gen.go")
	if err := extensionlock.WriteGenerated(outputPath, source); err != nil {
		t.Fatal(err)
	}
	current, err := extensionlock.CheckGenerated(outputPath, source)
	if err != nil || !current {
		t.Fatalf("CheckGenerated() = %t, %v", current, err)
	}
	if err := os.WriteFile(outputPath, append(source, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = extensionlock.CheckGenerated(outputPath, source)
	if err != nil || current {
		t.Fatalf("CheckGenerated(stale) = %t, %v", current, err)
	}
}

func TestGenerateRejectsInvalidLocks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		document string
		want     string
	}{
		{
			name: "unknown field",
			document: `{"version":1,"unknown":true,"extensions":[{
				"go_package":"example.com/extension",
				"manifest":{"id":"one","version":"v1","protocol_version":1}
			}]}`,
			want: `unknown field "unknown"`,
		},
		{
			name: "duplicate ID",
			document: `{"version":1,"extensions":[
				{"go_package":"example.com/one","manifest":{"id":"duplicate","version":"v1","protocol_version":1}},
				{"go_package":"example.com/two","manifest":{"id":"duplicate","version":"v2","protocol_version":1}}
			]}`,
			want: `ID "duplicate" is declared more than once`,
		},
		{
			name: "invalid package",
			document: `{"version":1,"extensions":[{
				"go_package":"../extension",
				"manifest":{"id":"one","version":"v1","protocol_version":1}
			}]}`,
			want: "go_package must be a clean import path",
		},
		{
			name: "private factory",
			document: `{"version":1,"extensions":[{
				"go_package":"example.com/extension",
				"factory":"serverOptions",
				"manifest":{"id":"one","version":"v1","protocol_version":1}
			}]}`,
			want: "factory must be an exported Go identifier",
		},
		{
			name: "undeclared command capability",
			document: `{"version":1,"extensions":[{
				"go_package":"example.com/extension",
				"manifest":{
					"id":"one","version":"v1","protocol_version":1,
					"commands":[{"name":"inspect","capabilities":["test.read"]}]
				}
			}]}`,
			want: `capability "test.read" is absent from manifest capabilities`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := extensionlock.Generate(writeLock(t, test.document))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Generate() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestCheckedInCatalogIsCurrent(t *testing.T) {
	t.Parallel()

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	source, err := extensionlock.Generate(filepath.Join(root, extensionlock.Filename))
	if err != nil {
		t.Fatal(err)
	}
	current, err := extensionlock.CheckGenerated(
		filepath.Join(root, "internal", "extensionregistry", "registry_gen.go"),
		source,
	)
	if err != nil || !current {
		t.Fatalf("checked-in Extension catalog is stale: current=%t error=%v", current, err)
	}
}

func writeLock(t *testing.T, document string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), extensionlock.Filename)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
