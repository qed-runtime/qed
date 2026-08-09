// Package extensionlock validates extensions.lock and generates a self-exec
// Extension catalog for the QED binary
package extensionlock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"go/token"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/qed-runtime/qed/extension/manifest"
	"github.com/qed-runtime/qed/internal/jsonstrict"
)

const (
	// Filename is the conventional embedded Extension selection lock filename
	Filename = "extensions.lock"
	// CurrentVersion is the only lock format version supported by this package
	CurrentVersion = 1

	maximumLockBytes        = 1 << 20
	maximumGeneratedBytes   = 8 << 20
	maximumLockedExtensions = 1024
)

type document struct {
	Version    int     `json:"version"`
	Extensions []entry `json:"extensions"`
}

type entry struct {
	GoPackage string               `json:"go_package"`
	Factory   string               `json:"factory,omitempty"`
	Manifest  manifest.Declaration `json:"manifest"`
}

// Generate reads a strict extensions.lock and returns deterministic Go source
// for the internal self-exec catalog
func Generate(lockPath string) ([]byte, error) {
	document, err := load(lockPath)
	if err != nil {
		return nil, err
	}
	return generate(document)
}

// WriteGenerated atomically replaces one regular generated source file
func WriteGenerated(outputPath string, source []byte) error {
	if strings.TrimSpace(outputPath) == "" || strings.IndexByte(outputPath, 0) >= 0 {
		return errors.New("generated Extension catalog path is required and must not contain NUL")
	}
	if len(source) == 0 {
		return errors.New("generated Extension catalog source is empty")
	}
	if len(source) > maximumGeneratedBytes {
		return fmt.Errorf("generated Extension catalog exceeds %d bytes", maximumGeneratedBytes)
	}
	absolute, err := filepath.Abs(outputPath)
	if err != nil {
		return fmt.Errorf("resolve generated Extension catalog path: %w", err)
	}
	if info, statErr := os.Lstat(absolute); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("generated Extension catalog must be a regular non-symlink file")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("stat generated Extension catalog: %w", statErr)
	}
	directory := filepath.Dir(absolute)
	info, err := os.Stat(directory)
	if err != nil {
		return fmt.Errorf("stat generated Extension catalog directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("generated Extension catalog parent must be a directory")
	}
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(absolute)+".*")
	if err != nil {
		return fmt.Errorf("create temporary Extension catalog: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set temporary Extension catalog permissions: %w", err)
	}
	if _, err := temporary.Write(source); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary Extension catalog: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary Extension catalog: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary Extension catalog: %w", err)
	}
	if err := os.Rename(temporaryPath, absolute); err != nil {
		return fmt.Errorf("replace generated Extension catalog: %w", err)
	}
	return nil
}

// CheckGenerated reports whether outputPath is a regular file with source as
// its exact contents
func CheckGenerated(outputPath string, source []byte) (bool, error) {
	if strings.TrimSpace(outputPath) == "" || strings.IndexByte(outputPath, 0) >= 0 {
		return false, errors.New("generated Extension catalog path is required and must not contain NUL")
	}
	absolute, err := filepath.Abs(outputPath)
	if err != nil {
		return false, fmt.Errorf("resolve generated Extension catalog path: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return false, fmt.Errorf("stat generated Extension catalog: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, errors.New("generated Extension catalog must be a regular non-symlink file")
	}
	if info.Size() > maximumGeneratedBytes {
		return false, nil
	}
	data, err := os.ReadFile(absolute)
	if err != nil {
		return false, fmt.Errorf("read generated Extension catalog: %w", err)
	}
	return bytes.Equal(data, source), nil
}

func load(lockPath string) (document, error) {
	if strings.TrimSpace(lockPath) == "" || strings.IndexByte(lockPath, 0) >= 0 {
		return document{}, errors.New("Extension lock path is required and must not contain NUL")
	}
	absolute, err := filepath.Abs(lockPath)
	if err != nil {
		return document{}, fmt.Errorf("resolve Extension lock path: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return document{}, fmt.Errorf("stat Extension lock: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return document{}, errors.New("Extension lock must be a regular non-symlink file")
	}
	file, err := os.Open(absolute)
	if err != nil {
		return document{}, fmt.Errorf("open Extension lock: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumLockBytes+1))
	if err != nil {
		return document{}, fmt.Errorf("read Extension lock: %w", err)
	}
	var decoded document
	if err := jsonstrict.Decode(data, maximumLockBytes, &decoded); err != nil {
		return document{}, fmt.Errorf("decode Extension lock: %w", err)
	}
	if decoded.Version != CurrentVersion {
		return document{}, fmt.Errorf("unsupported Extension lock version %d, want %d", decoded.Version, CurrentVersion)
	}
	if len(decoded.Extensions) == 0 {
		return document{}, errors.New("Extension lock requires at least one Extension")
	}
	if len(decoded.Extensions) > maximumLockedExtensions {
		return document{}, fmt.Errorf("Extension lock exceeds %d Extensions", maximumLockedExtensions)
	}
	seenIDs := make(map[string]struct{}, len(decoded.Extensions))
	for index := range decoded.Extensions {
		locked := &decoded.Extensions[index]
		if err := manifest.ValidateDeclaration(locked.Manifest); err != nil {
			return document{}, fmt.Errorf("Extension lock entry %d: %w", index, err)
		}
		if _, duplicate := seenIDs[locked.Manifest.ID]; duplicate {
			return document{}, fmt.Errorf("Extension lock ID %q is declared more than once", locked.Manifest.ID)
		}
		seenIDs[locked.Manifest.ID] = struct{}{}
		if err := validateGoPackage(locked.GoPackage); err != nil {
			return document{}, fmt.Errorf("Extension lock %q: %w", locked.Manifest.ID, err)
		}
		if locked.Factory == "" {
			locked.Factory = "ServerOptions"
		}
		if !token.IsIdentifier(locked.Factory) || !token.IsExported(locked.Factory) {
			return document{}, fmt.Errorf("Extension lock %q factory must be an exported Go identifier", locked.Manifest.ID)
		}
	}
	sort.Slice(decoded.Extensions, func(first, second int) bool {
		return decoded.Extensions[first].Manifest.ID < decoded.Extensions[second].Manifest.ID
	})
	return decoded, nil
}

func validateGoPackage(value string) error {
	if value == "" || strings.TrimSpace(value) != value || strings.IndexByte(value, 0) >= 0 {
		return errors.New("go_package is required and must not have surrounding whitespace or NUL")
	}
	if strings.HasPrefix(value, ".") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || path.Clean(value) != value {
		return errors.New("go_package must be a clean import path")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("go_package must contain non-empty path segments")
		}
		for _, character := range segment {
			if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
				(character >= '0' && character <= '9') || strings.ContainsRune("._~-+", character) {
				continue
			}
			return errors.New("go_package contains an unsupported character")
		}
	}
	if value == "encoding/json" || value == "github.com/qed-runtime/qed/extension/manifest" ||
		value == "github.com/qed-runtime/qed/extension/protocol" {
		return errors.New("go_package conflicts with a catalog generator import")
	}
	return nil
}

func generate(document document) ([]byte, error) {
	aliases := make(map[string]string)
	seenPackages := make(map[string]struct{})
	packages := make([]string, 0, len(document.Extensions))
	hasCommands := false
	for _, locked := range document.Extensions {
		if _, exists := seenPackages[locked.GoPackage]; !exists {
			seenPackages[locked.GoPackage] = struct{}{}
			packages = append(packages, locked.GoPackage)
		}
		if len(locked.Manifest.Commands) > 0 {
			hasCommands = true
		}
	}
	sort.Strings(packages)
	for index, packagePath := range packages {
		aliases[packagePath] = fmt.Sprintf("extension%d", index)
	}

	var source bytes.Buffer
	source.WriteString("// Code generated by qed extension generate; DO NOT EDIT.\n")
	source.WriteString("// Source: extensions.lock\n\n")
	source.WriteString("package extensionregistry\n\n")
	source.WriteString("import (\n")
	if hasCommands {
		source.WriteString("\t\"encoding/json\"\n")
	}
	source.WriteString("\n\textensionmanifest \"github.com/qed-runtime/qed/extension/manifest\"\n")
	if hasCommands {
		source.WriteString("\textensionprotocol \"github.com/qed-runtime/qed/extension/protocol\"\n")
	}
	for _, packagePath := range packages {
		fmt.Fprintf(&source, "\t%s %s\n", aliases[packagePath], strconv.Quote(packagePath))
	}
	source.WriteString(")\n\n")
	source.WriteString("var generatedCatalog = map[string]catalogEntry{\n")
	for _, locked := range document.Extensions {
		fmt.Fprintf(&source, "\t%s: {\n", strconv.Quote(locked.Manifest.ID))
		source.WriteString("\t\tdeclaration: extensionmanifest.Declaration{\n")
		fmt.Fprintf(&source, "\t\t\tID: %s,\n", strconv.Quote(locked.Manifest.ID))
		fmt.Fprintf(&source, "\t\t\tVersion: %s,\n", strconv.Quote(locked.Manifest.Version))
		fmt.Fprintf(&source, "\t\t\tProtocolVersion: %d,\n", locked.Manifest.ProtocolVersion)
		writeStringSlice(&source, "Capabilities", locked.Manifest.Capabilities, 3)
		writeStringSlice(&source, "Hooks", locked.Manifest.Hooks, 3)
		if len(locked.Manifest.Commands) > 0 {
			source.WriteString("\t\t\tCommands: []extensionprotocol.CommandDefinition{\n")
			for _, command := range locked.Manifest.Commands {
				source.WriteString("\t\t\t\t{\n")
				fmt.Fprintf(&source, "\t\t\t\t\tName: %s,\n", strconv.Quote(command.Name))
				if command.Description != "" {
					fmt.Fprintf(&source, "\t\t\t\t\tDescription: %s,\n", strconv.Quote(command.Description))
				}
				if len(command.InputSchema) > 0 {
					compacted := &bytes.Buffer{}
					if err := json.Compact(compacted, command.InputSchema); err != nil {
						return nil, fmt.Errorf("compact Extension Command %q schema: %w", command.Name, err)
					}
					fmt.Fprintf(&source, "\t\t\t\t\tInputSchema: json.RawMessage(%s),\n", strconv.Quote(compacted.String()))
				}
				writeStringSlice(&source, "Capabilities", command.Capabilities, 5)
				source.WriteString("\t\t\t\t},\n")
			}
			source.WriteString("\t\t\t},\n")
		}
		source.WriteString("\t\t},\n")
		fmt.Fprintf(&source, "\t\tserverOptions: %s.%s,\n", aliases[locked.GoPackage], locked.Factory)
		source.WriteString("\t},\n")
	}
	source.WriteString("}\n")
	formatted, err := format.Source(source.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated Extension catalog: %w", err)
	}
	return formatted, nil
}

func writeStringSlice(source *bytes.Buffer, field string, values []string, indentation int) {
	if len(values) == 0 {
		return
	}
	indent := strings.Repeat("\t", indentation)
	fmt.Fprintf(source, "%s%s: []string{", indent, field)
	for index, value := range values {
		if index > 0 {
			source.WriteString(", ")
		}
		source.WriteString(strconv.Quote(value))
	}
	source.WriteString("},\n")
}
