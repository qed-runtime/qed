// Package manifest validates shared Extension declarations and discovers
// external QED Extension manifests
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/internal/jsonstrict"
)

const (
	// Filename is the conventional external Extension manifest filename
	Filename                   = "qed-extension.json"
	maximumManifestBytes       = 1 << 20
	maximumDiscoveredManifests = 1024
)

// Declaration describes the transport-independent identity and components of
// one Extension
type Declaration struct {
	ID              string                       `json:"id"`
	Version         string                       `json:"version"`
	ProtocolVersion int                          `json:"protocol_version"`
	Capabilities    []string                     `json:"capabilities,omitempty"`
	Hooks           []string                     `json:"hooks,omitempty"`
	Commands        []protocol.CommandDefinition `json:"commands,omitempty"`
}

// ProtocolManifest returns the declarations that a started process must match
func (declaration Declaration) ProtocolManifest() protocol.Manifest {
	result := protocol.Manifest{
		ID:              declaration.ID,
		Version:         declaration.Version,
		ProtocolVersion: declaration.ProtocolVersion,
		Capabilities:    append([]string(nil), declaration.Capabilities...),
		Hooks:           append([]string(nil), declaration.Hooks...),
		Commands:        append([]protocol.CommandDefinition(nil), declaration.Commands...),
	}
	for index := range result.Commands {
		result.Commands[index].InputSchema = append(json.RawMessage(nil), result.Commands[index].InputSchema...)
		result.Commands[index].Capabilities = append([]string(nil), result.Commands[index].Capabilities...)
	}
	return result
}

// Manifest describes one distributable external Extension process
type Manifest struct {
	ID              string                       `json:"id"`
	Version         string                       `json:"version"`
	ProtocolVersion int                          `json:"protocol_version"`
	Entrypoint      string                       `json:"entrypoint"`
	Capabilities    []string                     `json:"capabilities,omitempty"`
	Hooks           []string                     `json:"hooks,omitempty"`
	Commands        []protocol.CommandDefinition `json:"commands,omitempty"`
}

// Resolved contains one validated manifest and its canonical local paths
type Resolved struct {
	Manifest   Manifest
	Path       string
	Directory  string
	Entrypoint string
}

// Declaration returns the transport-independent portion of the manifest
func (document Manifest) Declaration() Declaration {
	declaration := Declaration{
		ID:              document.ID,
		Version:         document.Version,
		ProtocolVersion: document.ProtocolVersion,
		Capabilities:    append([]string(nil), document.Capabilities...),
		Hooks:           append([]string(nil), document.Hooks...),
		Commands:        append([]protocol.CommandDefinition(nil), document.Commands...),
	}
	for index := range declaration.Commands {
		declaration.Commands[index].InputSchema = append(json.RawMessage(nil), declaration.Commands[index].InputSchema...)
		declaration.Commands[index].Capabilities = append([]string(nil), declaration.Commands[index].Capabilities...)
	}
	return declaration
}

// ProtocolManifest returns the declarations that a started process must match
func (document Manifest) ProtocolManifest() protocol.Manifest {
	return document.Declaration().ProtocolManifest()
}

// Load reads a manifest file or an Extension directory containing Filename
func Load(path string) (Resolved, error) {
	return load(path, true)
}

// LoadForDevelopment reads a manifest without requiring its build output to exist yet
func LoadForDevelopment(path string) (Resolved, error) {
	return load(path, false)
}

func load(path string, requireEntrypoint bool) (Resolved, error) {
	if strings.TrimSpace(path) == "" || strings.IndexByte(path, 0) >= 0 {
		return Resolved{}, errors.New("Extension manifest path is required and must not contain NUL")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve Extension manifest path: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return Resolved{}, fmt.Errorf("stat Extension manifest path: %w", err)
	}
	if info.IsDir() {
		absolute = filepath.Join(absolute, Filename)
		info, err = os.Lstat(absolute)
		if err != nil {
			return Resolved{}, fmt.Errorf("stat Extension manifest: %w", err)
		}
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Resolved{}, errors.New("Extension manifest must be a regular non-symlink file")
	}
	file, err := os.Open(absolute)
	if err != nil {
		return Resolved{}, fmt.Errorf("open Extension manifest: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumManifestBytes+1))
	if err != nil {
		return Resolved{}, fmt.Errorf("read Extension manifest: %w", err)
	}
	var document Manifest
	if err := jsonstrict.Decode(data, maximumManifestBytes, &document); err != nil {
		return Resolved{}, fmt.Errorf("decode Extension manifest: %w", err)
	}
	if err := validate(document); err != nil {
		return Resolved{}, err
	}

	directory, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve Extension directory: %w", err)
	}
	entrypoint := filepath.Join(directory, filepath.FromSlash(document.Entrypoint))
	if requireEntrypoint {
		entrypoint, err = filepath.EvalSymlinks(entrypoint)
		if err != nil {
			return Resolved{}, fmt.Errorf("resolve Extension entrypoint: %w", err)
		}
	} else {
		entrypoint = filepath.Clean(entrypoint)
	}
	relative, err := filepath.Rel(directory, entrypoint)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return Resolved{}, errors.New("Extension entrypoint escapes its manifest directory")
	}
	if requireEntrypoint {
		entryInfo, err := os.Stat(entrypoint)
		if err != nil {
			return Resolved{}, fmt.Errorf("stat Extension entrypoint: %w", err)
		}
		if !entryInfo.Mode().IsRegular() {
			return Resolved{}, errors.New("Extension entrypoint must resolve to a regular file")
		}
	}
	return Resolved{
		Manifest:   cloneManifest(document),
		Path:       filepath.Clean(absolute),
		Directory:  filepath.Clean(directory),
		Entrypoint: filepath.Clean(entrypoint),
	}, nil
}

// Discover recursively finds conventional manifests below the supplied roots
//
// Directory symlinks are not followed and duplicate Extension IDs are rejected
func Discover(roots ...string) ([]Resolved, error) {
	if len(roots) == 0 {
		return nil, errors.New("at least one Extension discovery root is required")
	}
	var discovered []Resolved
	for _, root := range roots {
		if strings.TrimSpace(root) == "" || strings.IndexByte(root, 0) >= 0 {
			return nil, errors.New("Extension discovery root is required and must not contain NUL")
		}
		absolute, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("resolve Extension discovery root: %w", err)
		}
		info, err := os.Lstat(absolute)
		if err != nil {
			return nil, fmt.Errorf("stat Extension discovery root: %w", err)
		}
		if !info.IsDir() {
			resolved, err := Load(absolute)
			if err != nil {
				return nil, err
			}
			discovered = append(discovered, resolved)
			continue
		}
		err = filepath.WalkDir(absolute, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() || entry.Name() != Filename {
				return nil
			}
			if len(discovered) >= maximumDiscoveredManifests {
				return fmt.Errorf("Extension discovery exceeds %d manifests", maximumDiscoveredManifests)
			}
			resolved, err := Load(path)
			if err != nil {
				return err
			}
			discovered = append(discovered, resolved)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("discover Extensions below %q: %w", absolute, err)
		}
	}
	sort.Slice(discovered, func(first, second int) bool {
		if discovered[first].Manifest.ID == discovered[second].Manifest.ID {
			return discovered[first].Path < discovered[second].Path
		}
		return discovered[first].Manifest.ID < discovered[second].Manifest.ID
	})
	for index := 1; index < len(discovered); index++ {
		if discovered[index-1].Manifest.ID == discovered[index].Manifest.ID {
			return nil, fmt.Errorf("Extension ID %q was discovered more than once", discovered[index].Manifest.ID)
		}
	}
	return discovered, nil
}

// ValidateDeclaration validates a transport-independent Extension declaration
func ValidateDeclaration(document Declaration) error {
	if document.ID == "" || strings.TrimSpace(document.ID) != document.ID {
		return errors.New("Extension manifest ID is required and must not have surrounding whitespace")
	}
	if document.Version == "" || strings.TrimSpace(document.Version) != document.Version {
		return errors.New("Extension manifest version is required and must not have surrounding whitespace")
	}
	if document.ProtocolVersion != protocol.Version {
		return fmt.Errorf("Extension manifest protocol version %d is unsupported, want %d", document.ProtocolVersion, protocol.Version)
	}
	capabilities := make(map[string]struct{}, len(document.Capabilities))
	for _, name := range document.Capabilities {
		if err := capability.ValidateName(capability.Name(name)); err != nil {
			return fmt.Errorf("Extension manifest: %w", err)
		}
		if _, duplicate := capabilities[name]; duplicate {
			return fmt.Errorf("Extension manifest capability %q is declared more than once", name)
		}
		capabilities[name] = struct{}{}
	}
	hooks := make(map[string]struct{}, len(document.Hooks))
	for _, eventType := range document.Hooks {
		if eventType == "" || strings.TrimSpace(eventType) != eventType {
			return errors.New("Extension manifest Hook is required and must not have surrounding whitespace")
		}
		if _, duplicate := hooks[eventType]; duplicate {
			return fmt.Errorf("Extension manifest Hook %q is declared more than once", eventType)
		}
		hooks[eventType] = struct{}{}
	}
	commands := make(map[string]struct{}, len(document.Commands))
	for _, command := range document.Commands {
		if command.Name == "" || strings.TrimSpace(command.Name) != command.Name {
			return errors.New("Extension manifest Command name is required and must not have surrounding whitespace")
		}
		if _, duplicate := commands[command.Name]; duplicate {
			return fmt.Errorf("Extension manifest Command %q is declared more than once", command.Name)
		}
		commands[command.Name] = struct{}{}
		if len(command.InputSchema) > 0 && !json.Valid(command.InputSchema) {
			return fmt.Errorf("Extension manifest Command %q has invalid input schema", command.Name)
		}
		seen := make(map[string]struct{}, len(command.Capabilities))
		for _, name := range command.Capabilities {
			if err := capability.ValidateName(capability.Name(name)); err != nil {
				return fmt.Errorf("Extension manifest Command %q: %w", command.Name, err)
			}
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("Extension manifest Command %q capability %q is declared more than once", command.Name, name)
			}
			seen[name] = struct{}{}
			if _, declared := capabilities[name]; !declared {
				return fmt.Errorf("Extension manifest Command %q capability %q is absent from manifest capabilities", command.Name, name)
			}
		}
	}
	return nil
}

func validate(document Manifest) error {
	if err := ValidateDeclaration(document.Declaration()); err != nil {
		return err
	}
	if document.Entrypoint == "" || strings.TrimSpace(document.Entrypoint) != document.Entrypoint || strings.IndexByte(document.Entrypoint, 0) >= 0 {
		return errors.New("Extension manifest entrypoint is required and must not have surrounding whitespace or NUL")
	}
	entrypoint := filepath.FromSlash(document.Entrypoint)
	if filepath.IsAbs(entrypoint) || !filepath.IsLocal(entrypoint) || filepath.Clean(entrypoint) == "." {
		return errors.New("Extension manifest entrypoint must be a local relative path")
	}
	return nil
}

func cloneManifest(document Manifest) Manifest {
	document.Capabilities = append([]string(nil), document.Capabilities...)
	document.Hooks = append([]string(nil), document.Hooks...)
	document.Commands = append([]protocol.CommandDefinition(nil), document.Commands...)
	for index := range document.Commands {
		document.Commands[index].InputSchema = append(json.RawMessage(nil), document.Commands[index].InputSchema...)
		document.Commands[index].Capabilities = append([]string(nil), document.Commands[index].Capabilities...)
	}
	return document
}
