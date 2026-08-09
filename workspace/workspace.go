// Package workspace constrains file operations to one canonical directory tree
package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
)

// Workspace owns one canonical root and coordinates in-process access to it
type Workspace struct {
	root string
	mu   sync.RWMutex
}

// New validates root and constructs a Workspace
func New(root string) (*Workspace, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("workspace root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root symlinks: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return nil, fmt.Errorf("stat workspace root: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("workspace root must be a directory")
	}
	return &Workspace{root: filepath.Clean(canonical)}, nil
}

// Root returns the canonical absolute workspace root
func (workspace *Workspace) Root() string {
	return workspace.root
}

// AcquireRead prevents in-process Coding Tools from mutating the Workspace
// until the returned release function is called
func (workspace *Workspace) AcquireRead() func() {
	workspace.mu.RLock()
	return workspace.mu.RUnlock
}

// AcquireWrite prevents concurrent in-process Coding Tool access until the
// returned release function is called
func (workspace *Workspace) AcquireWrite() func() {
	workspace.mu.Lock()
	return workspace.mu.Unlock
}

// Resolve resolves an existing relative path and rejects symlink leaves or
// paths that escape the Workspace
func (workspace *Workspace) Resolve(path string) (string, error) {
	cleaned, err := cleanRelative(path, true)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(workspace.root, cleaned)
	leaf, err := os.Lstat(candidate)
	if err != nil {
		return "", fmt.Errorf("stat workspace path %q: %w", cleaned, err)
	}
	if leaf.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("workspace path %q must not be a symlink", cleaned)
	}
	canonical, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve workspace path %q: %w", cleaned, err)
	}
	if !workspace.contains(canonical) {
		return "", fmt.Errorf("workspace path %q escapes the root", cleaned)
	}
	return canonical, nil
}

// ResolveFile resolves an existing regular file
func (workspace *Workspace) ResolveFile(path string) (string, error) {
	resolved, err := workspace.Resolve(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat workspace file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("workspace path %q must be a regular file", path)
	}
	return resolved, nil
}

// ResolveDirectory resolves an existing directory
func (workspace *Workspace) ResolveDirectory(path string) (string, error) {
	resolved, err := workspace.Resolve(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat workspace directory %q: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace path %q must be a directory", path)
	}
	return resolved, nil
}

// ResolveTarget resolves a relative file target whose leaf may not exist
func (workspace *Workspace) ResolveTarget(path string) (resolved string, exists bool, err error) {
	cleaned, err := cleanRelative(path, false)
	if err != nil {
		return "", false, err
	}
	candidate := filepath.Join(workspace.root, cleaned)
	if _, err := os.Lstat(candidate); err == nil {
		resolved, resolveErr := workspace.Resolve(cleaned)
		return resolved, true, resolveErr
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("stat workspace target %q: %w", cleaned, err)
	}

	parentRelative := filepath.Dir(cleaned)
	parent, err := workspace.ResolveDirectory(parentRelative)
	if err != nil {
		return "", false, fmt.Errorf("resolve workspace target parent: %w", err)
	}
	resolved = filepath.Join(parent, filepath.Base(cleaned))
	if !workspace.contains(resolved) {
		return "", false, fmt.Errorf("workspace target %q escapes the root", cleaned)
	}
	return resolved, false, nil
}

// Open opens a relative path for reading with traversal-resistant os.Root
// semantics
func (workspace *Workspace) Open(path string) (*os.File, error) {
	cleaned, err := cleanRelative(path, true)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(workspace.root)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	file, openErr := root.Open(cleaned)
	closeErr := root.Close()
	if openErr != nil {
		return nil, fmt.Errorf("open workspace path %q: %w", filepath.ToSlash(cleaned), openErr)
	}
	if closeErr != nil {
		_ = file.Close()
		return nil, fmt.Errorf("close workspace root: %w", closeErr)
	}
	return file, nil
}

// Lstat returns information about a relative path without following its final
// symbolic link and prevents traversal outside the Workspace
func (workspace *Workspace) Lstat(path string) (os.FileInfo, error) {
	cleaned, err := cleanRelative(path, true)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(workspace.root)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	info, operationErr := root.Lstat(cleaned)
	closeErr := root.Close()
	if operationErr != nil {
		return nil, fmt.Errorf("stat workspace path %q: %w", filepath.ToSlash(cleaned), operationErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close workspace root: %w", closeErr)
	}
	return info, nil
}

// Remove removes one relative file with traversal-resistant os.Root semantics
func (workspace *Workspace) Remove(path string) error {
	cleaned, err := cleanRelative(path, false)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(workspace.root)
	if err != nil {
		return fmt.Errorf("open workspace root: %w", err)
	}
	operationErr := root.Remove(cleaned)
	closeErr := root.Close()
	if operationErr != nil {
		return fmt.Errorf("remove workspace path %q: %w", filepath.ToSlash(cleaned), operationErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close workspace root: %w", closeErr)
	}
	return nil
}

// AtomicWrite replaces one relative file using a temporary file in the same
// directory and traversal-resistant os.Root operations
func (workspace *Workspace) AtomicWrite(path string, data []byte, mode os.FileMode) error {
	cleaned, err := cleanRelative(path, false)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(workspace.root)
	if err != nil {
		return fmt.Errorf("open workspace root: %w", err)
	}
	defer root.Close()

	temporaryPath, temporary, err := createTemporary(root, filepath.Dir(cleaned), mode)
	if err != nil {
		return fmt.Errorf("create temporary workspace file: %w", err)
	}
	defer root.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary workspace file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary workspace file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary workspace file: %w", err)
	}
	if err := root.Rename(temporaryPath, cleaned); err != nil {
		return fmt.Errorf("replace workspace path %q: %w", filepath.ToSlash(cleaned), err)
	}
	return nil
}

// Relative converts a contained absolute path to a slash-separated relative path
func (workspace *Workspace) Relative(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("absolute path is required")
	}
	cleaned := filepath.Clean(path)
	if !workspace.contains(cleaned) {
		return "", errors.New("path is outside the workspace")
	}
	relative, err := filepath.Rel(workspace.root, cleaned)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(relative), nil
}

func (workspace *Workspace) contains(path string) bool {
	relative, err := filepath.Rel(workspace.root, filepath.Clean(path))
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func cleanRelative(path string, allowRoot bool) (string, error) {
	if path == "" {
		return "", errors.New("workspace path is required")
	}
	if !utf8.ValidString(path) {
		return "", errors.New("workspace path must be valid UTF-8")
	}
	if strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("workspace path must not contain NUL")
	}
	localized := filepath.FromSlash(path)
	if !filepath.IsLocal(localized) || filepath.IsAbs(localized) || filepath.VolumeName(localized) != "" {
		return "", errors.New("workspace path must be relative")
	}
	cleaned := filepath.Clean(localized)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("workspace path must not escape the root")
	}
	if cleaned == "." && !allowRoot {
		return "", errors.New("workspace file path is required")
	}
	return cleaned, nil
}

func createTemporary(root *os.Root, directory string, mode os.FileMode) (string, *os.File, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		path := filepath.Join(directory, ".qed-patch-"+hex.EncodeToString(random[:]))
		file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
		if err == nil {
			return path, file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("could not allocate a temporary workspace file")
}
