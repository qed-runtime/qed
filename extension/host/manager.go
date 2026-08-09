package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/evidence"
	"github.com/qed-runtime/qed/extension"
)

const defaultRetireTimeout = 10 * time.Second

// ManagerOptions configures one reloadable Extension component source
type ManagerOptions struct {
	Process  ProcessOptions
	Policy   capability.Policy
	Approver capability.Approver
	Recorder evidence.Recorder
	// StateStore persists opaque process state under the Extension namespace
	StateStore extension.StateStore
	// StateScope selects the host-owned state namespace and defaults to "process"
	StateScope string
	// Logger receives safe structured generation lifecycle diagnostics
	Logger *slog.Logger
	// RetireTimeout bounds Drain for a generation after its last Run releases it
	RetireTimeout time.Duration
}

// Manager owns Extension generations and pins exactly one generation to each Run
//
// Manager is safe for concurrent use. A failed Reload leaves the current
// generation unchanged
type Manager struct {
	policy        capability.Policy
	approver      capability.Approver
	recorder      evidence.Recorder
	stateStore    extension.StateStore
	stateScope    string
	logger        *slog.Logger
	retireTimeout time.Duration
	extensionID   string

	reloadMu sync.Mutex
	mu       sync.Mutex
	closed   bool
	current  *generation
	next     uint64
	all      map[uint64]*generation
	active   int
	idle     chan struct{}
	errors   []error
}

type generation struct {
	number    uint64
	process   *Process
	tools     []agent.Tool
	hooks     []agent.Hook
	commands  []extension.Command
	refs      int
	retiring  bool
	closeOnce sync.Once
	closed    chan struct{}
}

// NewManager starts generation one and constructs a reloadable component source
func NewManager(ctx context.Context, options ManagerOptions) (*Manager, error) {
	if options.Policy == nil {
		return nil, errors.New("Extension Host Policy is required")
	}
	if options.RetireTimeout == 0 {
		options.RetireTimeout = defaultRetireTimeout
	}
	if options.RetireTimeout <= 0 {
		return nil, errors.New("Extension Host retire timeout must be positive")
	}
	if options.StateScope == "" {
		options.StateScope = "process"
	}
	if strings.TrimSpace(options.StateScope) != options.StateScope {
		return nil, errors.New("Extension State scope must not have surrounding whitespace")
	}
	process, err := Start(ctx, options.Process)
	if err != nil {
		return nil, err
	}
	if options.StateStore != nil {
		persisted, loadErr := options.StateStore.Get(ctx, process.Manifest().ID, options.StateScope, "snapshot")
		if loadErr == nil {
			if err := process.Restore(ctx, persisted); err != nil {
				_ = process.Close()
				return nil, fmt.Errorf("restore persisted Extension state: %w", err)
			}
			if _, err := process.HealthCheck(ctx); err != nil {
				_ = process.Close()
				return nil, fmt.Errorf("health check persisted Extension state: %w", err)
			}
		} else if !errors.Is(loadErr, extension.ErrStateNotFound) {
			_ = process.Close()
			return nil, fmt.Errorf("load persisted Extension state: %w", loadErr)
		}
	}
	idle := make(chan struct{})
	close(idle)
	manager := &Manager{
		policy:        options.Policy,
		approver:      options.Approver,
		recorder:      options.Recorder,
		stateStore:    options.StateStore,
		stateScope:    options.StateScope,
		logger:        options.Logger,
		retireTimeout: options.RetireTimeout,
		extensionID:   process.Manifest().ID,
		next:          2,
		all:           make(map[uint64]*generation),
		idle:          idle,
	}
	initial, err := manager.configureGeneration(process, 1)
	if err != nil {
		_ = process.Close()
		return nil, err
	}
	manager.current = initial
	manager.all[initial.number] = initial
	manager.debug("extension.generation.started", "generation", initial.number)
	return manager, nil
}

// AcquireTools pins the current generation until the returned release function is called
func (manager *Manager) AcquireTools(ctx context.Context) ([]agent.Tool, func(), error) {
	components, release, err := manager.AcquireComponents(ctx)
	if err != nil {
		return nil, nil, err
	}
	return components.Tools, release, nil
}

// AcquireComponents pins the current Tool and Hook generation until release
func (manager *Manager) AcquireComponents(ctx context.Context) (agent.RunComponents, func(), error) {
	if ctx == nil {
		return agent.RunComponents{}, nil, errors.New("Extension Host context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return agent.RunComponents{}, nil, err
	}
	manager.mu.Lock()
	if manager.closed || manager.current == nil {
		manager.mu.Unlock()
		return agent.RunComponents{}, nil, ErrHostClosed
	}
	generation := manager.current
	if manager.active == 0 {
		manager.idle = make(chan struct{})
	}
	manager.active++
	generation.refs++
	tools := append([]agent.Tool(nil), generation.tools...)
	hooks := append([]agent.Hook(nil), generation.hooks...)
	manager.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() { manager.release(generation) })
	}
	return agent.RunComponents{Tools: tools, Hooks: hooks}, release, nil
}

// AcquireCommands pins the current command generation until release
func (manager *Manager) AcquireCommands(ctx context.Context) ([]extension.Command, func(), error) {
	if ctx == nil {
		return nil, nil, errors.New("Extension Host context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	manager.mu.Lock()
	if manager.closed || manager.current == nil {
		manager.mu.Unlock()
		return nil, nil, ErrHostClosed
	}
	generation := manager.current
	if manager.active == 0 {
		manager.idle = make(chan struct{})
	}
	manager.active++
	generation.refs++
	commands := append([]extension.Command(nil), generation.commands...)
	manager.mu.Unlock()

	var once sync.Once
	release := func() { once.Do(func() { manager.release(generation) }) }
	return commands, release, nil
}

// CurrentGeneration returns the generation selected for newly started Runs
func (manager *Manager) CurrentGeneration() uint64 {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.current == nil {
		return 0
	}
	return manager.current.number
}

// ExtensionID returns the validated Extension identity owned by the Manager
func (manager *Manager) ExtensionID() string {
	return manager.extensionID
}

// Reload validates and restores a new process before atomically routing new Runs to it
//
// Existing Runs retain their old generation. Startup, Snapshot, Restore, or
// validation failure closes the candidate and leaves the old generation current
func (manager *Manager) Reload(ctx context.Context, options ProcessOptions) (uint64, error) {
	if ctx == nil {
		return 0, errors.New("Extension reload context must not be nil")
	}
	manager.reloadMu.Lock()
	defer manager.reloadMu.Unlock()

	manager.mu.Lock()
	if manager.closed || manager.current == nil {
		manager.mu.Unlock()
		return 0, ErrHostClosed
	}
	old := manager.current
	number := manager.next
	manager.mu.Unlock()
	manager.debug("extension.reload.started", "current_generation", old.number, "candidate_generation", number)
	if options.ExpectedID != manager.extensionID {
		return 0, fmt.Errorf("reload Extension ID %q does not match current %q", options.ExpectedID, manager.extensionID)
	}

	candidateProcess, err := Start(ctx, options)
	if err != nil {
		return 0, fmt.Errorf("start reload candidate: %w", err)
	}
	candidate, err := manager.configureGeneration(candidateProcess, number)
	if err != nil {
		_ = candidateProcess.Close()
		return 0, fmt.Errorf("configure reload candidate: %w", err)
	}
	state, err := old.process.Snapshot(ctx)
	if err != nil {
		_ = candidateProcess.Close()
		return 0, fmt.Errorf("snapshot generation %d: %w", old.number, err)
	}
	if manager.stateStore != nil {
		if err := manager.stateStore.Set(ctx, manager.extensionID, manager.stateScope, "snapshot", state); err != nil {
			_ = candidateProcess.Close()
			return 0, fmt.Errorf("persist generation %d state: %w", old.number, err)
		}
	}
	if err := candidateProcess.Restore(ctx, state); err != nil {
		_ = candidateProcess.Close()
		return 0, fmt.Errorf("restore generation %d: %w", number, err)
	}
	if _, err := candidateProcess.HealthCheck(ctx); err != nil {
		_ = candidateProcess.Close()
		return 0, fmt.Errorf("health check generation %d after Restore: %w", number, err)
	}

	manager.mu.Lock()
	if manager.closed || manager.current != old {
		manager.mu.Unlock()
		_ = candidateProcess.Close()
		return 0, ErrHostClosed
	}
	manager.current = candidate
	manager.next++
	manager.all[number] = candidate
	old.retiring = true
	retireNow := old.refs == 0
	manager.mu.Unlock()
	if retireNow {
		manager.retire(old)
	}
	manager.debug("extension.reload.completed", "previous_generation", old.number, "generation", number)
	return number, nil
}

// RetirementErrors returns bounded lifecycle failures observed after successful swaps
func (manager *Manager) RetirementErrors() []error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return append([]error(nil), manager.errors...)
}

// CloseContext rejects new Runs, waits for leases until ctx ends, and closes all generations
func (manager *Manager) CloseContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Extension Host close context must not be nil")
	}
	manager.reloadMu.Lock()
	manager.mu.Lock()
	manager.closed = true
	current := manager.current
	manager.current = nil
	idle := manager.idle
	generations := make([]*generation, 0, len(manager.all))
	for _, current := range manager.all {
		current.retiring = true
		generations = append(generations, current)
	}
	manager.mu.Unlock()
	manager.reloadMu.Unlock()

	var closeErr error
	select {
	case <-idle:
	case <-ctx.Done():
		closeErr = ctx.Err()
	}
	if current != nil && manager.stateStore != nil {
		state, err := current.process.Snapshot(ctx)
		if err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("snapshot Extension generation %d: %w", current.number, err))
		} else if err := manager.stateStore.Set(ctx, manager.extensionID, manager.stateScope, "snapshot", state); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("persist Extension generation %d state: %w", current.number, err))
		}
	}
	for _, current := range generations {
		manager.retire(current)
	}
	for _, current := range generations {
		select {
		case <-current.closed:
		case <-ctx.Done():
			closeErr = errors.Join(closeErr, ctx.Err())
		}
	}
	manager.mu.Lock()
	closeErr = errors.Join(closeErr, errors.Join(manager.errors...))
	manager.mu.Unlock()
	return closeErr
}

// Close rejects new Runs and closes every generation without a caller deadline
func (manager *Manager) Close() error {
	return manager.CloseContext(context.Background())
}

func (manager *Manager) configureGeneration(process *Process, number uint64) (*generation, error) {
	rawTools := process.Tools(number)
	tools := make([]agent.Tool, len(rawTools))
	for index, raw := range rawTools {
		configured, err := extension.NewTool(extension.ToolOptions{
			Tool:     raw,
			Policy:   manager.policy,
			Approver: manager.approver,
			Recorder: manager.recorder,
		})
		if err != nil {
			return nil, fmt.Errorf("configure Extension Tool %q: %w", raw.Definition().Name, err)
		}
		tools[index] = configured
	}
	rawCommands := process.Commands(number)
	commands := make([]extension.Command, len(rawCommands))
	for index, raw := range rawCommands {
		configured, err := extension.NewCommand(extension.CommandOptions{
			Command:  raw,
			Policy:   manager.policy,
			Approver: manager.approver,
		})
		if err != nil {
			return nil, fmt.Errorf("configure Extension Command %q: %w", raw.Definition().Name, err)
		}
		commands[index] = configured
	}
	return &generation{
		number:   number,
		process:  process,
		tools:    tools,
		hooks:    process.Hooks(number),
		commands: commands,
		closed:   make(chan struct{}),
	}, nil
}

func (manager *Manager) release(generation *generation) {
	manager.mu.Lock()
	generation.refs--
	manager.active--
	if manager.active == 0 {
		close(manager.idle)
	}
	retireNow := generation.retiring && generation.refs == 0
	manager.mu.Unlock()
	if retireNow {
		manager.retire(generation)
	}
}

func (manager *Manager) retire(generation *generation) {
	generation.closeOnce.Do(func() {
		go func() {
			manager.debug("extension.generation.retiring", "generation", generation.number)
			ctx, cancel := context.WithTimeout(context.Background(), manager.retireTimeout)
			drainErr := generation.process.Drain(ctx)
			cancel()
			closeErr := generation.process.Close()
			if drainErr != nil && !errors.Is(drainErr, ErrProcessExited) {
				closeErr = errors.Join(fmt.Errorf("drain Extension generation %d: %w", generation.number, drainErr), closeErr)
			}
			manager.mu.Lock()
			delete(manager.all, generation.number)
			if closeErr != nil {
				manager.errors = append(manager.errors, closeErr)
			}
			manager.mu.Unlock()
			manager.debug("extension.generation.retired",
				"generation", generation.number,
				"error_type", errorType(closeErr),
			)
			close(generation.closed)
		}()
	})
}

func (manager *Manager) debug(message string, arguments ...any) {
	if manager.logger != nil {
		manager.logger.Debug(message, append([]any{
			"component", "extension_manager",
			"extension_id", manager.extensionID,
		}, arguments...)...)
	}
}
