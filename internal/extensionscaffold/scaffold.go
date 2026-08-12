// Package extensionscaffold creates a safe Go reference Extension layout
package extensionscaffold

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/qed-runtime/qed/extension/manifest"
	"github.com/qed-runtime/qed/extension/protocol"
)

const (
	// DefaultVersion is the initial implementation version used by a scaffold
	DefaultVersion = "0.1.0"

	maximumGoModuleBytes = 1 << 20
	maximumIDBytes       = 256
	maximumVersionBytes  = 128
	executableName       = "extension-server"
)

// Options identifies one new Go Extension scaffold
type Options struct {
	Directory string
	ID        string
	Version   string
}

// Result describes the files created for one Go Extension scaffold
type Result struct {
	Directory  string
	ModuleRoot string
	GoPackage  string
	Files      []string
}

type configuration struct {
	directory  string
	moduleRoot string
	goPackage  string
	id         string
	version    string
}

type generatedFile struct {
	path string
	data []byte
}

// Create validates options and creates a new Extension directory without
// modifying an existing path or the owning module files
func Create(options Options) (Result, error) {
	configured, err := configure(options)
	if err != nil {
		return Result{}, err
	}
	files, err := generate(configured)
	if err != nil {
		return Result{}, err
	}
	if err := write(configured.directory, files); err != nil {
		return Result{}, err
	}
	paths := make([]string, len(files))
	for index, file := range files {
		paths[index] = file.path
	}
	return Result{
		Directory:  configured.directory,
		ModuleRoot: configured.moduleRoot,
		GoPackage:  configured.goPackage,
		Files:      paths,
	}, nil
}

func configure(options Options) (configuration, error) {
	if strings.TrimSpace(options.Directory) == "" || strings.IndexByte(options.Directory, 0) >= 0 {
		return configuration{}, errors.New("Extension scaffold directory is required and must not contain NUL")
	}
	if err := validateIdentifier("Extension scaffold ID", options.ID, maximumIDBytes, false); err != nil {
		return configuration{}, err
	}
	version := options.Version
	if version == "" {
		version = DefaultVersion
	}
	if err := validateIdentifier("Extension scaffold version", version, maximumVersionBytes, true); err != nil {
		return configuration{}, err
	}
	if err := manifest.ValidateDeclaration(manifest.Declaration{
		ID:              options.ID,
		Version:         version,
		ProtocolVersion: protocol.Version,
	}); err != nil {
		return configuration{}, fmt.Errorf("validate Extension scaffold declaration: %w", err)
	}

	absolute, err := filepath.Abs(options.Directory)
	if err != nil {
		return configuration{}, fmt.Errorf("resolve Extension scaffold directory: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return configuration{}, fmt.Errorf("resolve Extension scaffold parent: %w", err)
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return configuration{}, fmt.Errorf("stat Extension scaffold parent: %w", err)
	}
	if !parentInfo.IsDir() {
		return configuration{}, errors.New("Extension scaffold parent must be a directory")
	}
	directory := filepath.Join(parent, filepath.Base(absolute))
	if _, err := os.Lstat(directory); err == nil {
		return configuration{}, errors.New("Extension scaffold destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return configuration{}, fmt.Errorf("stat Extension scaffold destination: %w", err)
	}

	moduleRoot, modulePath, err := findModule(parent)
	if err != nil {
		return configuration{}, err
	}
	relative, err := filepath.Rel(moduleRoot, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return configuration{}, errors.New("Extension scaffold destination must be inside the detected Go module")
	}
	importSubpath := filepath.ToSlash(relative)
	if err := validateImportSubpath(importSubpath); err != nil {
		return configuration{}, err
	}
	goPackage := strings.TrimSuffix(modulePath, "/") + "/" + importSubpath + "/extension"
	if err := validateImportPath(goPackage); err != nil {
		return configuration{}, fmt.Errorf("generated Extension package: %w", err)
	}
	return configuration{
		directory:  filepath.Clean(directory),
		moduleRoot: filepath.Clean(moduleRoot),
		goPackage:  goPackage,
		id:         options.ID,
		version:    version,
	}, nil
}

func validateIdentifier(kind, value string, maximum int, allowPlus bool) error {
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s is required and must not have surrounding whitespace", kind)
	}
	if len(value) > maximum {
		return fmt.Errorf("%s exceeds %d bytes", kind, maximum)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", kind)
	}
	for index, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s must not contain control characters", kind)
		}
		if (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') {
			continue
		}
		if index > 0 && (character == '.' || character == '_' || character == '-' || (allowPlus && character == '+')) {
			continue
		}
		if allowPlus {
			return fmt.Errorf("%s must start with an ASCII letter or digit and contain only letters, digits, dot, underscore, hyphen, or plus", kind)
		}
		return fmt.Errorf("%s must start with an ASCII letter or digit and contain only letters, digits, dot, underscore, or hyphen", kind)
	}
	return nil
}

func findModule(start string) (string, string, error) {
	for directory := filepath.Clean(start); ; directory = filepath.Dir(directory) {
		moduleFile := filepath.Join(directory, "go.mod")
		info, err := os.Lstat(moduleFile)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return "", "", errors.New("detected go.mod must be a regular non-symlink file")
			}
			modulePath, err := readModulePath(moduleFile)
			if err != nil {
				return "", "", err
			}
			return directory, modulePath, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("stat go.mod: %w", err)
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
	}
	return "", "", errors.New("Extension scaffold must be created inside an existing Go module")
}

func readModulePath(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open go.mod: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumGoModuleBytes+1))
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	if len(data) > maximumGoModuleBytes {
		return "", fmt.Errorf("go.mod exceeds %d bytes", maximumGoModuleBytes)
	}
	modulePath := ""
	for _, line := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "module" {
			continue
		}
		if modulePath != "" || len(fields) < 2 {
			return "", errors.New("go.mod must contain exactly one valid module directive")
		}
		if len(fields) > 2 && !strings.HasPrefix(fields[2], "//") {
			return "", errors.New("go.mod module directive contains unexpected arguments")
		}
		value := fields[1]
		if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "`") {
			value, err = strconv.Unquote(value)
			if err != nil {
				return "", fmt.Errorf("decode go.mod module path: %w", err)
			}
		}
		modulePath = value
	}
	if modulePath == "" {
		return "", errors.New("go.mod module directive is required")
	}
	if err := validateImportPath(modulePath); err != nil {
		return "", fmt.Errorf("go.mod module path: %w", err)
	}
	return modulePath, nil
}

func validateImportSubpath(value string) error {
	if value == "" || value == "." {
		return errors.New("Extension scaffold module-relative path is invalid")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." || segment == "testdata" ||
			strings.HasPrefix(segment, ".") || strings.HasPrefix(segment, "_") {
			return fmt.Errorf("Extension scaffold path segment %q is not a buildable Go package path", segment)
		}
		if err := validateModulePathElement(segment); err != nil {
			return fmt.Errorf("Extension scaffold path segment %q: %w", segment, err)
		}
	}
	return nil
}

func validateImportPath(value string) error {
	if value == "" || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") {
		return errors.New("Go import path is invalid")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("Go import path contains an invalid segment")
		}
		if err := validateModulePathElement(segment); err != nil {
			return fmt.Errorf("Go import path element %q: %w", segment, err)
		}
	}
	return nil
}

func validateModulePathElement(value string) error {
	if strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return errors.New("must not begin or end with a dot")
	}
	for _, character := range value {
		if (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '.' || character == '_' || character == '~' {
			continue
		}
		return errors.New("contains an unsupported Go module path character")
	}
	prefix, _, _ := strings.Cut(value, ".")
	upper := strings.ToUpper(prefix)
	if upper == "CON" || upper == "PRN" || upper == "AUX" || upper == "NUL" ||
		(len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) &&
			upper[3] >= '1' && upper[3] <= '9') {
		return errors.New("uses a Windows reserved file name")
	}
	tilde := strings.LastIndexByte(prefix, '~')
	if tilde >= 0 && tilde+1 < len(prefix) {
		digits := true
		for _, character := range prefix[tilde+1:] {
			if character < '0' || character > '9' {
				digits = false
				break
			}
		}
		if digits {
			return errors.New("uses a Windows short-name suffix")
		}
	}
	return nil
}

func generate(config configuration) ([]generatedFile, error) {
	manifestData, err := json.MarshalIndent(manifest.Manifest{
		ID:              config.id,
		Version:         config.version,
		ProtocolVersion: protocol.Version,
		Entrypoint:      executableName,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode Extension scaffold manifest: %w", err)
	}
	manifestData = append(manifestData, '\n')

	implementation, err := formattedGoSource(implementationSource(config))
	if err != nil {
		return nil, fmt.Errorf("format Extension scaffold implementation: %w", err)
	}
	mainSource, err := formattedGoSource(executableSource(config))
	if err != nil {
		return nil, fmt.Errorf("format Extension scaffold executable: %w", err)
	}
	testSource, err := formattedGoSource(contractTestSource(config))
	if err != nil {
		return nil, fmt.Errorf("format Extension scaffold contract test: %w", err)
	}
	readme, err := readmeSource(config)
	if err != nil {
		return nil, fmt.Errorf("generate Extension scaffold README: %w", err)
	}
	return []generatedFile{
		{path: ".gitignore", data: []byte("/" + executableName + "\n")},
		{path: "README.md", data: readme},
		{path: filepath.ToSlash(filepath.Join("extension", "extension.go")), data: implementation},
		{path: "main.go", data: mainSource},
		{path: "main_test.go", data: testSource},
		{path: manifest.Filename, data: manifestData},
	}, nil
}

func formattedGoSource(source string) ([]byte, error) {
	formatted, err := format.Source([]byte(source))
	if err != nil {
		return nil, err
	}
	return formatted, nil
}

func implementationSource(config configuration) string {
	return fmt.Sprintf(`// Package extension provides the %s QED Extension components
package extension

import (
	"context"
	"errors"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/extension/manifest"
	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/extension/server"
)

const (
	// ID is the stable Extension identifier
	ID = %s
	// Version identifies this Extension implementation
	Version = %s
)

// Declaration returns the transport-independent Extension declaration
func Declaration() manifest.Declaration {
	return manifest.Declaration{
		ID:              ID,
		Version:         Version,
		ProtocolVersion: protocol.Version,
	}
}

// ServerOptions returns a fresh Extension protocol server configuration
func ServerOptions() server.Options {
	return server.Options{
		ID:      ID,
		Version: Version,
		Initialize: func(ctx context.Context, _ protocol.InitializeRequest) ([]agent.Tool, error) {
			if ctx == nil {
				return nil, errors.New("Extension context must not be nil")
			}
			// Register model-facing Tools here and declare their capabilities in Declaration
			return nil, nil
		},
	}
}
`, config.id, strconv.Quote(config.id), strconv.Quote(config.version))
}

func executableSource(config configuration) string {
	return fmt.Sprintf(`package main

import (
	"context"
	"fmt"
	"os"

	extensionimpl %s
	"github.com/qed-runtime/qed/extension/server"
)

func main() {
	if err := server.Serve(context.Background(), os.Stdin, os.Stdout, extensionimpl.ServerOptions()); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`, strconv.Quote(config.goPackage))
}

func contractTestSource(config configuration) string {
	return fmt.Sprintf(`package main

import (
	"context"
	"fmt"
	"os"
	"testing"

	extensionimpl %s
	"github.com/qed-runtime/qed/extension/contracttest"
	"github.com/qed-runtime/qed/extension/host"
	"github.com/qed-runtime/qed/extension/server"
)

const extensionTestChildArgument = "__qed_extension_lifecycle_test"

func TestMain(testingMain *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == extensionTestChildArgument {
		if err := server.Serve(context.Background(), os.Stdin, os.Stdout, extensionimpl.ServerOptions()); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(testingMain.Run())
}

func TestExtensionLifecycleContract(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contracttest.RunLifecycle(t, contracttest.LifecycleOptions{
		Command: host.Command{
			Path: executable,
			Args: []string{extensionTestChildArgument},
		},
		Declaration: extensionimpl.Declaration(),
	})
}
`, strconv.Quote(config.goPackage))
}

func readmeSource(config configuration) ([]byte, error) {
	lockEntry := struct {
		GoPackage string               `json:"go_package"`
		Factory   string               `json:"factory"`
		Manifest  manifest.Declaration `json:"manifest"`
	}{
		GoPackage: config.goPackage,
		Factory:   "ServerOptions",
		Manifest: manifest.Declaration{
			ID:              config.id,
			Version:         config.version,
			ProtocolVersion: protocol.Version,
		},
	}
	encoded, err := json.MarshalIndent(lockEntry, "", "  ")
	if err != nil {
		return nil, err
	}
	var result bytes.Buffer
	fmt.Fprintf(&result, `# %s Extension

This directory was generated by QED as a Go Extension reference. The owning
Go module must require `+"`github.com/qed-runtime/qed`"+`; the scaffold does not modify
`+"`go.mod`"+`, `+"`go.sum`"+`, or `+"`extensions.lock`"+`

## Verify and develop

From this directory

`+"```sh"+`
go test ./...
go build -o %s .
qed extension dev .
`+"```"+`

The lifecycle contract test starts the real process boundary and verifies
Handshake, Initialize, Describe, HealthCheck, Snapshot, Restore, Drain, and
Shutdown. Add behavior tests when adding Tools, Hooks, or Commands

## Add components

Implement components in `+"`extension/extension.go`"+` and return them from
`+"`ServerOptions`"+`. Keep `+"`Declaration`"+` and `+"`qed-extension.json`"+` aligned with
declared capabilities, Hooks, and Commands. Runtime Tool definitions are
reported by the live process and are not copied into the external manifest

## Link into one binary

Add this object to the owning application's `+"`extensions.lock`"+` entries

`+"```json"+`
%s
`+"```"+`

Then regenerate the application-owned self-exec catalog
`, config.id, executableName, encoded)
	return result.Bytes(), nil
}

func write(directory string, files []generatedFile) (returnErr error) {
	if err := os.Mkdir(directory, 0o755); err != nil {
		return fmt.Errorf("create Extension scaffold directory: %w", err)
	}
	createdDirectories := []string{directory}
	createdFiles := make([]string, 0, len(files))
	defer func() {
		if returnErr == nil {
			return
		}
		for index := len(createdFiles) - 1; index >= 0; index-- {
			_ = os.Remove(createdFiles[index])
		}
		for index := len(createdDirectories) - 1; index >= 0; index-- {
			_ = os.Remove(createdDirectories[index])
		}
	}()

	implementationDirectory := filepath.Join(directory, "extension")
	if err := os.Mkdir(implementationDirectory, 0o755); err != nil {
		return fmt.Errorf("create Extension scaffold implementation directory: %w", err)
	}
	createdDirectories = append(createdDirectories, implementationDirectory)
	for _, file := range files {
		path := filepath.Join(directory, filepath.FromSlash(file.path))
		output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return fmt.Errorf("create Extension scaffold file %q: %w", file.path, err)
		}
		createdFiles = append(createdFiles, path)
		if err := output.Chmod(0o644); err != nil {
			_ = output.Close()
			return fmt.Errorf("set Extension scaffold file %q permissions: %w", file.path, err)
		}
		if _, err := output.Write(file.data); err != nil {
			_ = output.Close()
			return fmt.Errorf("write Extension scaffold file %q: %w", file.path, err)
		}
		if err := output.Sync(); err != nil {
			_ = output.Close()
			return fmt.Errorf("sync Extension scaffold file %q: %w", file.path, err)
		}
		if err := output.Close(); err != nil {
			return fmt.Errorf("close Extension scaffold file %q: %w", file.path, err)
		}
	}
	return nil
}
