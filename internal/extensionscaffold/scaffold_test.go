package extensionscaffold

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qed-runtime/qed/extension/manifest"
	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/extension/selfexec"
)

func TestCreateWritesExpectedScaffoldWithoutModuleMutation(t *testing.T) {
	t.Parallel()

	root := newModule(t, "example.com/project")
	parent := filepath.Join(root, "extensions")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(parent, "greeting")
	result, err := Create(Options{Directory: directory, ID: "example.greeting"})
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := []string{
		".gitignore",
		"README.md",
		"extension/extension.go",
		"main.go",
		"main_test.go",
		manifest.Filename,
	}
	if result.Directory != directory || result.ModuleRoot != root ||
		result.GoPackage != "example.com/project/extensions/greeting/extension" ||
		!reflect.DeepEqual(result.Files, wantFiles) {
		t.Fatalf("Create() = %#v", result)
	}
	for _, relative := range wantFiles {
		info, err := os.Lstat(filepath.Join(directory, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("stat %s: %v", relative, err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o644 {
			t.Fatalf("generated file %s mode = %v", relative, info.Mode())
		}
	}
	resolved, err := manifest.LoadForDevelopment(directory)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Manifest.ID != "example.greeting" || resolved.Manifest.Version != DefaultVersion ||
		resolved.Manifest.ProtocolVersion != protocol.Version || resolved.Manifest.Entrypoint != executableName {
		t.Fatalf("manifest = %#v", resolved.Manifest)
	}
	assertFileContains(t, filepath.Join(directory, "main.go"), `extensionimpl "example.com/project/extensions/greeting/extension"`)
	assertFileContains(t, filepath.Join(directory, "main_test.go"), "contracttest.RunLifecycle")
	assertFileContains(t, filepath.Join(directory, "extension", "extension.go"), `ID = "example.greeting"`)
	assertFileContains(t, filepath.Join(directory, "README.md"), `"go_package": "example.com/project/extensions/greeting/extension"`)
	assertFileContains(t, filepath.Join(directory, ".gitignore"), "/extension-server")

	before := snapshotFiles(t, directory, wantFiles)
	if _, err := Create(Options{Directory: directory, ID: "changed.id"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second Create() error = %v", err)
	}
	after := snapshotFiles(t, directory, wantFiles)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("second Create() changed existing scaffold files")
	}
	moduleData, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if string(moduleData) != "module example.com/project\n\ngo 1.25.0\n" {
		t.Fatalf("go.mod changed: %q", moduleData)
	}
	for _, name := range []string{"go.sum", "extensions.lock"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("unexpected module file %s: %v", name, err)
		}
	}
}

func TestCreateValidatesIdentityModuleAndDestination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(*testing.T) Options
		want    string
	}{
		{
			name: "invalid ID",
			prepare: func(t *testing.T) Options {
				root := newModule(t, "example.com/project")
				return Options{Directory: filepath.Join(root, "sample"), ID: "bad/id"}
			},
			want: "scaffold ID",
		},
		{
			name: "invalid version",
			prepare: func(t *testing.T) Options {
				root := newModule(t, "example.com/project")
				return Options{Directory: filepath.Join(root, "sample"), ID: "example.sample", Version: "+bad"}
			},
			want: "scaffold version",
		},
		{
			name: "outside module",
			prepare: func(t *testing.T) Options {
				root := t.TempDir()
				return Options{Directory: filepath.Join(root, "sample"), ID: "example.sample"}
			},
			want: "inside an existing Go module",
		},
		{
			name: "non importable path",
			prepare: func(t *testing.T) Options {
				root := newModule(t, "example.com/project")
				return Options{Directory: filepath.Join(root, "bad path"), ID: "example.sample"}
			},
			want: "unsupported Go module path character",
		},
		{
			name: "invalid module path",
			prepare: func(t *testing.T) Options {
				root := newModule(t, "example.com/project+invalid")
				return Options{Directory: filepath.Join(root, "sample"), ID: "example.sample"}
			},
			want: "unsupported Go module path character",
		},
		{
			name: "Windows reserved path",
			prepare: func(t *testing.T) Options {
				root := newModule(t, "example.com/project")
				return Options{Directory: filepath.Join(root, "CON.txt"), ID: "example.sample"}
			},
			want: "Windows reserved file name",
		},
		{
			name: "missing parent",
			prepare: func(t *testing.T) Options {
				root := newModule(t, "example.com/project")
				return Options{Directory: filepath.Join(root, "missing", "sample"), ID: "example.sample"}
			},
			want: "resolve Extension scaffold parent",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := test.prepare(t)
			if _, err := Create(options); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Create() error = %v, want %q", err, test.want)
			}
			if _, err := os.Lstat(options.Directory); !os.IsNotExist(err) {
				t.Fatalf("invalid scaffold created destination: %v", err)
			}
		})
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	t.Parallel()

	root := newModule(t, "example.com/project")
	configured, err := configure(Options{
		Directory: filepath.Join(root, "sample"),
		ID:        "example.sample",
		Version:   "1.2.3-beta+build",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := generate(configured)
	if err != nil {
		t.Fatal(err)
	}
	second, err := generate(configured)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("generate() is not deterministic")
	}
}

func TestCreateReadsQuotedModuleDirectiveWithComment(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	document := "module \"example.com/quoted\" // module comment\n\ngo 1.25.0\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Create(Options{
		Directory: filepath.Join(root, "sample"),
		ID:        "example.sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.GoPackage != "example.com/quoted/sample/extension" {
		t.Fatalf("GoPackage = %q", result.GoPackage)
	}
}

func TestCreateRejectsExistingDestinationSymlink(t *testing.T) {
	t.Parallel()

	root := newModule(t, "example.com/project")
	outside := t.TempDir()
	destination := filepath.Join(root, "sample")
	if err := os.Symlink(outside, destination); err != nil {
		t.Skipf("create destination symlink: %v", err)
	}
	if _, err := Create(Options{Directory: destination, ID: "example.sample"}); err == nil ||
		!strings.Contains(err.Error(), "destination already exists") {
		t.Fatalf("Create() error = %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("scaffold wrote through destination symlink: %#v", entries)
	}
}

func TestCreateRejectsSymlinkedModuleFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	moduleTarget := filepath.Join(root, "module.txt")
	if err := os.WriteFile(moduleTarget, []byte("module example.com/project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moduleTarget, filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("create go.mod symlink: %v", err)
	}
	destination := filepath.Join(root, "sample")
	if _, err := Create(Options{Directory: destination, ID: "example.sample"}); err == nil ||
		!strings.Contains(err.Error(), "regular non-symlink") {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("scaffold created destination with symlinked go.mod: %v", err)
	}
}

func TestWriteRollsBackOnlyCreatedScaffoldPaths(t *testing.T) {
	t.Parallel()

	directory := filepath.Join(t.TempDir(), "sample")
	err := write(directory, []generatedFile{
		{path: "created.txt", data: []byte("created")},
		{path: "missing/failure.txt", data: []byte("failure")},
	})
	if err == nil || !strings.Contains(err.Error(), "create Extension scaffold file") {
		t.Fatalf("write() error = %v", err)
	}
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatalf("write() left a partial scaffold: %v", err)
	}
}

func TestGeneratedScaffoldBuildsAndPassesLifecycleContract(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go executable is unavailable")
	}
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve scaffold test source path")
	}
	qedRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	root := t.TempDir()
	moduleDocument := fmt.Sprintf(
		"module example.com/scaffoldtest\n\ngo 1.25.0\n\nrequire github.com/qed-runtime/qed v0.0.0\n\nreplace github.com/qed-runtime/qed => %s\n",
		strconv.Quote(filepath.ToSlash(qedRoot)),
	)
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(moduleDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "sample")
	created, err := Create(Options{Directory: directory, ID: "example.generated"})
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, selfexec.LockFilename)
	lockDocument := fmt.Sprintf(`{
  "version": 1,
  "extensions": [{
    "go_package": %q,
    "factory": "ServerOptions",
    "manifest": {
      "id": "example.generated",
      "version": "0.1.0",
      "protocol_version": 1
    }
  }]
}
`, created.GoPackage)
	if err := os.WriteFile(lockPath, []byte(lockDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	registrySource, err := selfexec.Generate(lockPath, selfexec.GenerateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	registryDirectory := filepath.Join(root, "extensionregistry")
	if err := os.Mkdir(registryDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := selfexec.WriteGenerated(filepath.Join(registryDirectory, "registry_gen.go"), registrySource); err != nil {
		t.Fatal(err)
	}
	registryTest := `package extensionregistry

import "testing"

func TestGeneratedExtensionFactory(t *testing.T) {
	definition, ok := Catalog.Lookup("example.generated")
	if !ok {
		t.Fatal("generated Extension is not registered")
	}
	options, err := definition.NewServerOptions()
	if err != nil || options.ID != "example.generated" || options.Version != "0.1.0" {
		t.Fatalf("NewServerOptions() = %#v, %v", options, err)
	}
}
`
	if err := os.WriteFile(filepath.Join(registryDirectory, "registry_test.go"), []byte(registryTest), 0o600); err != nil {
		t.Fatal(err)
	}
	runGo(t, directory, "test", "-mod=readonly", "-count=1", "./...")
	runGo(t, root, "test", "-mod=readonly", "-count=1", "./extensionregistry")
	runGo(t, directory, "build", "-mod=readonly", "-o", filepath.Join(directory, executableName), ".")
	resolved, err := manifest.Load(created.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Entrypoint != filepath.Join(created.Directory, executableName) {
		t.Fatalf("resolved entrypoint = %q", resolved.Entrypoint)
	}
	if _, err := os.Lstat(filepath.Join(root, "go.sum")); !os.IsNotExist(err) {
		t.Fatalf("generated module command changed go.sum: %v", err)
	}
}

func newModule(t *testing.T, modulePath string) string {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	document := "module " + modulePath + "\n\ngo 1.25.0\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func assertFileContains(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), expected) {
		t.Fatalf("%s does not contain %q:\n%s", filepath.Base(path), expected, data)
	}
}

func snapshotFiles(t *testing.T, directory string, paths []string) map[string]string {
	t.Helper()
	result := make(map[string]string, len(paths))
	for _, relative := range paths {
		data, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		result[relative] = string(data)
	}
	return result
}

func runGo(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(),
		"GOWORK=off",
		"GOTOOLCHAIN=local",
		"GOPROXY=off",
		"GOSUMDB=off",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go %v: %v\n%s", arguments, err, output)
	}
}
