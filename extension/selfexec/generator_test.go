package selfexec_test

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qed-runtime/qed/extension/selfexec"
)

func TestGenerateProducesStandaloneDeterministicCatalog(t *testing.T) {
	t.Parallel()

	lockPath := writeLock(t, `{
		"version":1,
		"extensions":[
			{
				"go_package":"example.com/extensions/first",
				"factory":"NewServerOptions",
				"manifest":{"id":"second","version":"v2","protocol_version":2}
			},
			{
				"go_package":"example.com/extensions/first",
				"manifest":{
					"id":"first",
					"version":"v1",
					"protocol_version":2,
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
	source, err := selfexec.Generate(lockPath, selfexec.GenerateOptions{
		PackageName:  "customregistry",
		VariableName: "Linked",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "registry_gen.go", source, parser.AllErrors); err != nil {
		t.Fatalf("generated source is invalid: %v\n%s", err, source)
	}
	text := string(source)
	if first, second := strings.Index(text, `"first",`), strings.Index(text, `"second",`); first < 0 || second < 0 || first >= second {
		t.Fatalf("generated catalog is not sorted by Extension ID:\n%s", text)
	}
	for _, fragment := range []string{
		`package customregistry`,
		`// Linked contains the self-exec Extensions selected by extensions.lock`,
		`var Linked = selfexec.MustNewCatalog`,
		`ServerOptions: extension0.ServerOptions`,
		`ServerOptions: extension0.NewServerOptions`,
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
	if err := selfexec.WriteGenerated(outputPath, source); err != nil {
		t.Fatal(err)
	}
	current, err := selfexec.CheckGenerated(outputPath, source)
	if err != nil || !current {
		t.Fatalf("CheckGenerated() = %t, %v", current, err)
	}
	if err := os.WriteFile(outputPath, append(source, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = selfexec.CheckGenerated(outputPath, source)
	if err != nil || current {
		t.Fatalf("CheckGenerated(stale) = %t, %v", current, err)
	}
}

func TestGenerateRejectsInvalidLocksAndIdentifiers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		document string
		options  selfexec.GenerateOptions
		want     string
	}{
		{
			name: "unknown field",
			document: `{"version":1,"unknown":true,"extensions":[{
				"go_package":"example.com/extension",
				"manifest":{"id":"one","version":"v1","protocol_version":2}
			}]}`,
			want: `unknown field "unknown"`,
		},
		{
			name: "duplicate ID",
			document: `{"version":1,"extensions":[
				{"go_package":"example.com/one","manifest":{"id":"duplicate","version":"v1","protocol_version":2}},
				{"go_package":"example.com/two","manifest":{"id":"duplicate","version":"v2","protocol_version":2}}
			]}`,
			want: `ID "duplicate" is declared more than once`,
		},
		{
			name: "invalid package",
			document: `{"version":1,"extensions":[{
				"go_package":"../extension",
				"manifest":{"id":"one","version":"v1","protocol_version":2}
			}]}`,
			want: "go_package must be a clean import path",
		},
		{
			name: "private factory",
			document: `{"version":1,"extensions":[{
				"go_package":"example.com/extension",
				"factory":"serverOptions",
				"manifest":{"id":"one","version":"v1","protocol_version":2}
			}]}`,
			want: "factory must be an exported Go identifier",
		},
		{
			name: "undeclared command capability",
			document: `{"version":1,"extensions":[{
				"go_package":"example.com/extension",
				"manifest":{
					"id":"one","version":"v1","protocol_version":2,
					"commands":[{"name":"inspect","capabilities":["test.read"]}]
				}
			}]}`,
			want: `capability "test.read" is absent from manifest capabilities`,
		},
		{
			name: "invalid generated package",
			document: `{"version":1,"extensions":[{
				"go_package":"example.com/extension",
				"manifest":{"id":"one","version":"v1","protocol_version":2}
			}]}`,
			options: selfexec.GenerateOptions{PackageName: "package"},
			want:    "package must be a valid",
		},
		{
			name: "private generated variable",
			document: `{"version":1,"extensions":[{
				"go_package":"example.com/extension",
				"manifest":{"id":"one","version":"v1","protocol_version":2}
			}]}`,
			options: selfexec.GenerateOptions{VariableName: "catalog"},
			want:    "variable must be an exported",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := selfexec.Generate(writeLock(t, test.document), test.options)
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
	source, err := selfexec.Generate(
		filepath.Join(root, selfexec.LockFilename),
		selfexec.GenerateOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	current, err := selfexec.CheckGenerated(
		filepath.Join(root, "internal", "extensionregistry", "registry_gen.go"),
		source,
	)
	if err != nil || !current {
		t.Fatalf("checked-in Extension catalog is stale: current=%t error=%v", current, err)
	}
}

func TestDownstreamModuleBuildsAndRunsWithoutForkingQED(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	directory := filepath.Join(filepath.Dir(sourceFile), "testdata", "downstream")
	source, err := selfexec.Generate(
		filepath.Join(directory, selfexec.LockFilename),
		selfexec.GenerateOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	current, err := selfexec.CheckGenerated(filepath.Join(directory, "extensionregistry", "registry_gen.go"), source)
	if err != nil || !current {
		t.Fatalf("downstream generated catalog is stale: current=%t error=%v", current, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", "-count=1", "./...")
	command.Dir = directory
	command.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly", "GOTOOLCHAIN=local")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("downstream go test failed: %v\n%s", err, output)
	}
}

func writeLock(t *testing.T, document string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), selfexec.LockFilename)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
