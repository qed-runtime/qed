package reload

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/evidence"
	"github.com/qed-runtime/qed/extension"
	"github.com/qed-runtime/qed/extension/host"
	"github.com/qed-runtime/qed/extension/manifest"
	"github.com/qed-runtime/qed/extension/protocol"
)

const maximumBuildOutputBytes = 1 << 20

// DevOptions configures a watching external Extension development host
type DevOptions struct {
	ManifestPath string
	// BuildProgram defaults to go
	BuildProgram string
	// BuildArgs must contain {output}; the default is build -o {output} .
	BuildArgs []string
	// BuildEnvironment is the complete build process environment
	BuildEnvironment []string
	// ExtensionEnvironment is the complete runtime child environment
	ExtensionEnvironment map[string]string
	Configuration        []byte
	WorkspaceRoot        string
	Policy               capability.Policy
	Approver             capability.Approver
	Recorder             evidence.Recorder
	StateStore           extension.StateStore
	StateScope           string
	ControlDirectory     string
	StatusWriter         io.Writer
	WatchInterval        time.Duration
	Debounce             time.Duration
	StartupTimeout       time.Duration
	ShutdownTimeout      time.Duration
	RetireTimeout        time.Duration
	// RestartPolicy defaults to host.DefaultRestartPolicy when nil
	RestartPolicy *host.RestartPolicy

	Verbose     bool
	DebugWriter io.Writer
	Logger      *slog.Logger
}

// Developer builds, watches, and atomically reloads one external Extension
type Developer struct {
	options     DevOptions
	resolved    manifest.Resolved
	tempDir     string
	manager     *host.Manager
	control     *ControlServer
	extensionID string
	watchRoot   string

	mu            sync.Mutex
	buildSequence uint64
	closed        bool
}

// Dev builds an Extension, starts its host, and watches until cancellation
func Dev(ctx context.Context, options DevOptions) error {
	developer, err := StartDev(ctx, options)
	if err != nil {
		return err
	}
	watchErr := Watch(ctx, WatchOptions{
		Root:     developer.watchRoot,
		Interval: developer.options.WatchInterval,
	}, func(watchContext context.Context) error {
		if developer.options.Debounce > 0 {
			timer := time.NewTimer(developer.options.Debounce)
			select {
			case <-watchContext.Done():
				timer.Stop()
				return watchContext.Err()
			case <-timer.C:
			}
		}
		status, err := developer.Reload(watchContext)
		if err != nil {
			developer.writeStatus("reload failed for Extension %q: %v\n", developer.extensionID, err)
			return nil
		}
		developer.writeStatus("reloaded Extension %q generation %d version %s\n", status.ExtensionID, status.Generation, status.Version)
		return nil
	})
	closeErr := developer.Close()
	if errors.Is(watchErr, context.Canceled) || errors.Is(watchErr, context.DeadlineExceeded) {
		watchErr = nil
	}
	return errors.Join(watchErr, closeErr)
}

// StartDev builds and starts one development host without starting its watcher
func StartDev(ctx context.Context, options DevOptions) (*Developer, error) {
	if ctx == nil {
		return nil, errors.New("Extension development context must not be nil")
	}
	configured, resolved, err := configureDev(options)
	if err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp("", "qed-extension-dev-*")
	if err != nil {
		return nil, fmt.Errorf("create Extension build directory: %w", err)
	}
	developer := &Developer{
		options:     configured,
		resolved:    resolved,
		tempDir:     tempDir,
		extensionID: resolved.Manifest.ID,
		watchRoot:   resolved.Directory,
	}
	command, err := developer.build(ctx)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, err
	}
	manager, err := host.NewManager(ctx, host.ManagerOptions{
		Process:       developer.processOptions(command, resolved),
		Policy:        configured.Policy,
		Approver:      configured.Approver,
		Recorder:      configured.Recorder,
		StateStore:    configured.StateStore,
		StateScope:    configured.StateScope,
		RetireTimeout: configured.RetireTimeout,
		RestartPolicy: configuredRestartPolicy(configured.RestartPolicy),
		Logger:        configured.Logger,
	})
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("start Extension development generation: %w", err)
	}
	developer.manager = manager
	control, err := StartControl(ctx, configured.ControlDirectory, resolved.Manifest.ID, developer)
	if err != nil {
		_ = manager.Close()
		_ = os.RemoveAll(tempDir)
		return nil, err
	}
	developer.control = control
	developer.writeStatus("started Extension %q generation %d version %s\n", resolved.Manifest.ID, manager.CurrentGeneration(), resolved.Manifest.Version)
	return developer, nil
}

func configuredRestartPolicy(configured *host.RestartPolicy) host.RestartPolicy {
	if configured == nil {
		return host.DefaultRestartPolicy()
	}
	return *configured
}

func configureDev(options DevOptions) (DevOptions, manifest.Resolved, error) {
	resolved, err := manifest.LoadForDevelopment(options.ManifestPath)
	if err != nil {
		return DevOptions{}, manifest.Resolved{}, err
	}
	if options.Policy == nil {
		return DevOptions{}, manifest.Resolved{}, errors.New("Extension development Policy is required")
	}
	if options.BuildProgram == "" {
		options.BuildProgram = "go"
	}
	if strings.TrimSpace(options.BuildProgram) != options.BuildProgram || strings.IndexByte(options.BuildProgram, 0) >= 0 {
		return DevOptions{}, manifest.Resolved{}, errors.New("Extension build program is required and must not have surrounding whitespace or NUL")
	}
	if len(options.BuildArgs) == 0 {
		options.BuildArgs = []string{"build", "-o", "{output}", "."}
	}
	hasOutput := false
	for index, argument := range options.BuildArgs {
		if strings.IndexByte(argument, 0) >= 0 {
			return DevOptions{}, manifest.Resolved{}, fmt.Errorf("Extension build argument %d contains NUL", index)
		}
		if strings.Contains(argument, "{output}") {
			hasOutput = true
		}
	}
	if !hasOutput {
		return DevOptions{}, manifest.Resolved{}, errors.New("Extension build arguments must contain {output}")
	}
	if options.ControlDirectory == "" {
		options.ControlDirectory = filepath.Join(resolved.Directory, ".qed", "extension-dev")
	}
	if options.StatusWriter == nil {
		options.StatusWriter = io.Discard
	}
	if options.Verbose && options.Logger == nil && options.DebugWriter != nil {
		options.Logger = slog.New(slog.NewJSONHandler(options.DebugWriter, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	if !options.Verbose {
		options.Logger = nil
	}
	if options.WatchInterval == 0 {
		options.WatchInterval = 500 * time.Millisecond
	}
	if options.Debounce == 0 {
		options.Debounce = 250 * time.Millisecond
	}
	if options.Debounce < 0 {
		return DevOptions{}, manifest.Resolved{}, errors.New("Extension reload debounce must not be negative")
	}
	if len(options.Configuration) > 0 && !jsonValid(options.Configuration) {
		return DevOptions{}, manifest.Resolved{}, errors.New("Extension development configuration must contain valid JSON")
	}
	options.BuildArgs = append([]string(nil), options.BuildArgs...)
	options.BuildEnvironment = append([]string(nil), options.BuildEnvironment...)
	if options.BuildEnvironment == nil {
		options.BuildEnvironment = os.Environ()
	}
	options.ExtensionEnvironment = cloneStringMap(options.ExtensionEnvironment)
	options.Configuration = append([]byte(nil), options.Configuration...)
	return options, resolved, nil
}

// Reload rebuilds and atomically swaps one candidate generation
func (developer *Developer) Reload(ctx context.Context) (ControlStatus, error) {
	developer.mu.Lock()
	defer developer.mu.Unlock()
	if developer.closed {
		return ControlStatus{}, host.ErrHostClosed
	}
	resolved, err := manifest.LoadForDevelopment(developer.resolved.Path)
	if err != nil {
		return ControlStatus{}, err
	}
	if resolved.Manifest.ID != developer.resolved.Manifest.ID {
		return ControlStatus{}, fmt.Errorf("reloaded manifest ID %q does not match active Extension %q", resolved.Manifest.ID, developer.resolved.Manifest.ID)
	}
	command, err := developer.build(ctx)
	if err != nil {
		return ControlStatus{}, err
	}
	generation, err := developer.manager.Reload(ctx, developer.processOptions(command, resolved))
	if err != nil {
		return ControlStatus{}, err
	}
	developer.resolved = resolved
	return developer.status(generation), nil
}

// Status returns the active generation without changing it
func (developer *Developer) Status(ctx context.Context) (ControlStatus, error) {
	if ctx == nil {
		return ControlStatus{}, errors.New("Extension development status context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return ControlStatus{}, err
	}
	developer.mu.Lock()
	defer developer.mu.Unlock()
	if developer.closed {
		return ControlStatus{}, host.ErrHostClosed
	}
	return developer.status(developer.manager.CurrentGeneration()), nil
}

// Close stops control traffic, drains generations, and removes temporary builds
func (developer *Developer) Close() error {
	developer.mu.Lock()
	if developer.closed {
		developer.mu.Unlock()
		return nil
	}
	developer.closed = true
	control := developer.control
	manager := developer.manager
	tempDir := developer.tempDir
	developer.mu.Unlock()
	var closeErr error
	if control != nil {
		closeErr = errors.Join(closeErr, control.Close())
	}
	if manager != nil {
		closeErr = errors.Join(closeErr, manager.Close())
	}
	if tempDir != "" {
		closeErr = errors.Join(closeErr, os.RemoveAll(tempDir))
	}
	return closeErr
}

func (developer *Developer) build(ctx context.Context) (host.Command, error) {
	developer.buildSequence++
	filename := fmt.Sprintf("extension-%06d", developer.buildSequence)
	if runtime.GOOS == "windows" {
		filename += ".exe"
	}
	output := filepath.Join(developer.tempDir, filename)
	arguments := make([]string, len(developer.options.BuildArgs))
	for index, argument := range developer.options.BuildArgs {
		arguments[index] = strings.ReplaceAll(argument, "{output}", output)
	}
	command := exec.CommandContext(ctx, developer.options.BuildProgram, arguments...)
	command.Dir = developer.resolved.Directory
	command.Env = append([]string(nil), developer.options.BuildEnvironment...)
	var diagnostics boundedBuildBuffer
	command.Stdout = &diagnostics
	command.Stderr = &diagnostics
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(diagnostics.String())
		if message == "" {
			return host.Command{}, fmt.Errorf("build Extension generation: %w", err)
		}
		return host.Command{}, fmt.Errorf("build Extension generation: %w: %s", err, message)
	}
	absolute, err := filepath.Abs(output)
	if err != nil {
		return host.Command{}, fmt.Errorf("resolve Extension build output: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return host.Command{}, fmt.Errorf("stat Extension build output: %w", err)
	}
	if !info.Mode().IsRegular() {
		return host.Command{}, errors.New("Extension build output is not a regular file")
	}
	return host.Command{
		Path:        filepath.Clean(absolute),
		Directory:   developer.resolved.Directory,
		Environment: cloneStringMap(developer.options.ExtensionEnvironment),
	}, nil
}

func (developer *Developer) processOptions(command host.Command, resolved manifest.Resolved) host.ProcessOptions {
	expected := protocol.Manifest{
		ID:              resolved.Manifest.ID,
		Version:         resolved.Manifest.Version,
		ProtocolVersion: resolved.Manifest.ProtocolVersion,
		Capabilities:    append([]string(nil), resolved.Manifest.Capabilities...),
		Hooks:           append([]string(nil), resolved.Manifest.Hooks...),
		Commands:        append([]protocol.CommandDefinition(nil), resolved.Manifest.Commands...),
	}
	for index := range expected.Commands {
		expected.Commands[index].InputSchema = append([]byte(nil), expected.Commands[index].InputSchema...)
		expected.Commands[index].Capabilities = append([]string(nil), expected.Commands[index].Capabilities...)
	}
	return host.ProcessOptions{
		Command:          command,
		ExpectedID:       resolved.Manifest.ID,
		ExpectedVersion:  resolved.Manifest.Version,
		ExpectedManifest: &expected,
		Initialize: protocol.InitializeRequest{
			WorkspaceRoot: developer.options.WorkspaceRoot,
			Environment:   cloneStringMap(developer.options.ExtensionEnvironment),
			Configuration: append([]byte(nil), developer.options.Configuration...),
			Verbose:       developer.options.Verbose,
		},
		StartupTimeout:  developer.options.StartupTimeout,
		ShutdownTimeout: developer.options.ShutdownTimeout,
		Verbose:         developer.options.Verbose,
		DebugWriter:     developer.options.DebugWriter,
		Logger:          developer.options.Logger,
	}
}

func (developer *Developer) status(generation uint64) ControlStatus {
	return ControlStatus{
		ExtensionID: developer.resolved.Manifest.ID,
		Version:     developer.resolved.Manifest.Version,
		Generation:  generation,
		Manifest:    developer.resolved.Path,
	}
}

func (developer *Developer) writeStatus(format string, arguments ...any) {
	_, _ = fmt.Fprintf(developer.options.StatusWriter, format, arguments...)
}

type boundedBuildBuffer struct {
	buffer    bytes.Buffer
	truncated bool
}

func (buffer *boundedBuildBuffer) Write(data []byte) (int, error) {
	remaining := maximumBuildOutputBytes - buffer.buffer.Len()
	if remaining > 0 {
		amount := len(data)
		if amount > remaining {
			amount = remaining
		}
		_, _ = buffer.buffer.Write(data[:amount])
	}
	if len(data) > remaining {
		buffer.truncated = true
	}
	return len(data), nil
}

func (buffer *boundedBuildBuffer) String() string {
	value := buffer.buffer.String()
	if buffer.truncated {
		value += "\n[build output truncated]"
	}
	return value
}

func cloneStringMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for name, value := range values {
		result[name] = value
	}
	return result
}

func jsonValid(value []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(value))
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return false
	}
	return decoder.Decode(&decoded) == io.EOF
}
