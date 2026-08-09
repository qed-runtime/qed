// Package command executes bounded child processes for official Extensions
package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Request describes one direct executable invocation without shell expansion
type Request struct {
	Executable     string
	Arguments      []string
	Directory      string
	Environment    []string
	Timeout        time.Duration
	MaxOutputBytes int
}

// Result contains bounded output and terminal process state
type Result struct {
	Stdout          string
	Stderr          string
	ExitCode        int
	Duration        time.Duration
	TimedOut        bool
	StdoutTruncated bool
	StderrTruncated bool
}

// Run executes one child process and terminates its process group on timeout or cancellation
func Run(ctx context.Context, request Request) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("command context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if request.Executable == "" {
		return Result{}, errors.New("command executable is required")
	}
	if request.Directory == "" {
		return Result{}, errors.New("command directory is required")
	}
	if request.Timeout <= 0 {
		return Result{}, errors.New("command timeout must be positive")
	}
	if request.MaxOutputBytes <= 0 {
		return Result{}, errors.New("command output limit must be positive")
	}

	executable, err := resolveExecutable(request.Executable, request.Environment)
	if err != nil {
		return Result{}, fmt.Errorf("resolve command executable: %w", err)
	}
	stdout := &limitedBuffer{maximum: request.MaxOutputBytes}
	stderr := &limitedBuffer{maximum: request.MaxOutputBytes}
	child := exec.Command(executable, request.Arguments...)
	child.Dir = request.Directory
	child.Env = append([]string(nil), request.Environment...)
	child.Stdout = stdout
	child.Stderr = stderr
	prepareProcess(child)

	startedAt := time.Now()
	if err := child.Start(); err != nil {
		return Result{}, fmt.Errorf("start command: %w", err)
	}
	waited := make(chan error, 1)
	go func() {
		waited <- child.Wait()
	}()

	timer := time.NewTimer(request.Timeout)
	defer timer.Stop()
	var waitErr error
	result := Result{ExitCode: -1}
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		_ = terminateProcess(child)
		waitErr = <-waited
		result.Duration = time.Since(startedAt)
		result.Stdout, result.StdoutTruncated = stdout.Result()
		result.Stderr, result.StderrTruncated = stderr.Result()
		return result, ctx.Err()
	case <-timer.C:
		result.TimedOut = true
		_ = terminateProcess(child)
		waitErr = <-waited
	}

	result.Duration = time.Since(startedAt)
	result.Stdout, result.StdoutTruncated = stdout.Result()
	result.Stderr, result.StderrTruncated = stderr.Result()
	if waitErr == nil {
		result.ExitCode = 0
		return result, nil
	}
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		result.ExitCode = exitError.ExitCode()
		return result, nil
	}
	return result, fmt.Errorf("wait for command: %w", waitErr)
}

func resolveExecutable(executable string, environment []string) (string, error) {
	if strings.ContainsAny(executable, `/\\`) {
		return executable, nil
	}
	pathValue := environmentValue(environment, "PATH")
	if pathValue == "" {
		return "", fmt.Errorf("%q: %w", executable, exec.ErrNotFound)
	}
	extensions := []string{""}
	if runtime.GOOS == "windows" && filepath.Ext(executable) == "" {
		extensions = filepath.SplitList(environmentValue(environment, "PATHEXT"))
		if len(extensions) == 0 {
			extensions = []string{".com", ".exe", ".bat", ".cmd"}
		}
	}
	for _, directory := range filepath.SplitList(pathValue) {
		if !filepath.IsAbs(directory) {
			continue
		}
		for _, extension := range extensions {
			candidate := filepath.Join(directory, executable+extension)
			info, err := os.Stat(candidate)
			if err != nil || info.IsDir() {
				continue
			}
			if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
				continue
			}
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%q: %w", executable, exec.ErrNotFound)
}

func environmentValue(environment []string, name string) string {
	for _, entry := range environment {
		separator := strings.IndexByte(entry, '=')
		if separator <= 0 {
			continue
		}
		entryName := entry[:separator]
		if entryName == name || (runtime.GOOS == "windows" && strings.EqualFold(entryName, name)) {
			return entry[separator+1:]
		}
	}
	return ""
}

type limitedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	maximum   int
	truncated bool
}

func (buffer *limitedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	remaining := buffer.maximum - buffer.buffer.Len()
	if remaining > 0 {
		portion := value
		if len(portion) > remaining {
			portion = portion[:remaining]
		}
		_, _ = buffer.buffer.Write(portion)
	}
	if len(value) > remaining {
		buffer.truncated = true
	}
	return len(value), nil
}

func (buffer *limitedBuffer) Result() (string, bool) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String(), buffer.truncated
}
