// Package reload provides external Extension development and reload primitives
package reload

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maximumWatchedFiles = 10000

// WatchOptions configures a bounded polling file watcher
type WatchOptions struct {
	Root     string
	Interval time.Duration
	Ignore   []string
}

// Watch invokes changed after each stable filesystem snapshot change
//
// Watch uses metadata snapshots, does not follow directory symlinks, and stops
// when ctx is canceled
func Watch(ctx context.Context, options WatchOptions, changed func(context.Context) error) error {
	if ctx == nil {
		return errors.New("Extension watch context must not be nil")
	}
	if changed == nil {
		return errors.New("Extension watch callback is required")
	}
	configured, err := configureWatch(options)
	if err != nil {
		return err
	}
	previous, err := snapshot(configured)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(configured.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			current, err := snapshot(configured)
			if err != nil {
				return err
			}
			if current == previous {
				continue
			}
			previous = current
			if err := changed(ctx); err != nil {
				return err
			}
		}
	}
}

func configureWatch(options WatchOptions) (WatchOptions, error) {
	if strings.TrimSpace(options.Root) == "" || strings.IndexByte(options.Root, 0) >= 0 {
		return WatchOptions{}, errors.New("Extension watch root is required and must not contain NUL")
	}
	absolute, err := filepath.Abs(options.Root)
	if err != nil {
		return WatchOptions{}, fmt.Errorf("resolve Extension watch root: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return WatchOptions{}, fmt.Errorf("stat Extension watch root: %w", err)
	}
	if !info.IsDir() {
		return WatchOptions{}, errors.New("Extension watch root must be a directory")
	}
	if options.Interval == 0 {
		options.Interval = 500 * time.Millisecond
	}
	if options.Interval < 50*time.Millisecond {
		return WatchOptions{}, errors.New("Extension watch interval must be at least 50ms")
	}
	options.Root = filepath.Clean(absolute)
	configuredIgnore := []string{".git", ".qed"}
	for _, ignored := range options.Ignore {
		if ignored == "" || filepath.IsAbs(ignored) || !filepath.IsLocal(ignored) {
			return WatchOptions{}, fmt.Errorf("Extension watch ignore path %q must be local and relative", ignored)
		}
		configuredIgnore = append(configuredIgnore, filepath.Clean(ignored))
	}
	options.Ignore = configuredIgnore
	return options, nil
}

func snapshot(options WatchOptions) ([sha256.Size]byte, error) {
	type entry struct {
		path     string
		size     int64
		mode     fs.FileMode
		modified int64
	}
	entries := make([]entry, 0)
	err := filepath.WalkDir(options.Root, func(path string, directoryEntry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(options.Root, path)
		if err != nil {
			return err
		}
		if relative != "." && ignoredPath(relative, options.Ignore) {
			if directoryEntry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if directoryEntry.Type()&os.ModeSymlink != 0 {
			if directoryEntry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if directoryEntry.IsDir() {
			return nil
		}
		if len(entries) >= maximumWatchedFiles {
			return fmt.Errorf("Extension watch exceeds %d files", maximumWatchedFiles)
		}
		info, err := directoryEntry.Info()
		if err != nil {
			return err
		}
		entries = append(entries, entry{
			path:     filepath.ToSlash(relative),
			size:     info.Size(),
			mode:     info.Mode(),
			modified: info.ModTime().UnixNano(),
		})
		return nil
	})
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("snapshot Extension sources: %w", err)
	}
	sort.Slice(entries, func(first, second int) bool { return entries[first].path < entries[second].path })
	hash := sha256.New()
	var number [8]byte
	for _, item := range entries {
		_, _ = hash.Write([]byte(item.path))
		_, _ = hash.Write([]byte{0})
		binary.BigEndian.PutUint64(number[:], uint64(item.size))
		_, _ = hash.Write(number[:])
		binary.BigEndian.PutUint64(number[:], uint64(item.mode))
		_, _ = hash.Write(number[:])
		binary.BigEndian.PutUint64(number[:], uint64(item.modified))
		_, _ = hash.Write(number[:])
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func ignoredPath(relative string, ignored []string) bool {
	cleaned := filepath.Clean(relative)
	for _, prefix := range ignored {
		if cleaned == prefix || strings.HasPrefix(cleaned, prefix+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
