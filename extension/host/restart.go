package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/qed-runtime/qed/extension"
)

const (
	defaultRestartAttempts       = 3
	maximumRestartAttempts       = 100
	defaultRestartInitialBackoff = 100 * time.Millisecond
	defaultRestartMaxBackoff     = 2 * time.Second
	defaultRestartStableAfter    = 30 * time.Second
)

var (
	// ErrExtensionRestarting indicates that no generation can be leased while
	// the Manager is validating a replacement for a crashed process
	ErrExtensionRestarting = errors.New("Extension is restarting")
	// ErrExtensionCircuitOpen indicates that automatic restart reached its
	// configured limit and requires an explicit successful Reload
	ErrExtensionCircuitOpen = errors.New("Extension restart circuit is open")
)

// RestartPolicy bounds automatic recovery after an unexpected process exit
//
// MaxAttempts set to zero disables automatic restart. Attempts include failed
// candidate startups and candidates that exit before StableAfter
type RestartPolicy struct {
	// MaxAttempts is the number of candidates allowed before the circuit opens
	MaxAttempts int
	// InitialBackoff defaults to 100 milliseconds when restart is enabled
	InitialBackoff time.Duration
	// MaxBackoff defaults to 2 seconds and caps exponential delay
	MaxBackoff time.Duration
	// StableAfter resets the consecutive attempt count after one generation
	// survives for this interval and defaults to 30 seconds
	StableAfter time.Duration
}

// DefaultRestartPolicy returns the bounded policy used by Coding Profiles and
// the Extension development host
func DefaultRestartPolicy() RestartPolicy {
	return RestartPolicy{
		MaxAttempts:    defaultRestartAttempts,
		InitialBackoff: defaultRestartInitialBackoff,
		MaxBackoff:     defaultRestartMaxBackoff,
		StableAfter:    defaultRestartStableAfter,
	}
}

// RestartState identifies the externally observable Manager recovery state
type RestartState string

const (
	// RestartStateDisabled means automatic restart is not configured
	RestartStateDisabled RestartState = "disabled"
	// RestartStateReady means a generation is available for new leases
	RestartStateReady RestartState = "ready"
	// RestartStateRestarting means a replacement candidate is being validated
	RestartStateRestarting RestartState = "restarting"
	// RestartStateCircuitOpen means the bounded attempt limit was reached
	RestartStateCircuitOpen RestartState = "circuit_open"
	// RestartStateClosed means the Manager no longer accepts work
	RestartStateClosed RestartState = "closed"
)

// RestartStatus is a payload-free snapshot of automatic recovery state
type RestartStatus struct {
	// State is the current recovery state
	State RestartState
	// Attempts is the consecutive replacement count since the last stable generation
	Attempts int
	// MaxAttempts is the configured circuit limit and is zero when disabled
	MaxAttempts int
	// Generation is the generation available to new leases or zero when unavailable
	Generation uint64
	// LastErrorType contains no error message or RPC payload
	LastErrorType string
}

// RestartStatus returns a concurrent-safe snapshot of recovery state
func (manager *Manager) RestartStatus() RestartStatus {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	generation := uint64(0)
	if manager.current != nil {
		generation = manager.current.number
	}
	return RestartStatus{
		State:         manager.restartState,
		Attempts:      manager.restartAttempts,
		MaxAttempts:   manager.restartPolicy.MaxAttempts,
		Generation:    generation,
		LastErrorType: manager.restartErrorType,
	}
}

func normalizeRestartPolicy(policy RestartPolicy) (RestartPolicy, error) {
	if policy.MaxAttempts < 0 || policy.MaxAttempts > maximumRestartAttempts {
		return RestartPolicy{}, fmt.Errorf("Extension restart attempts must be between 0 and %d", maximumRestartAttempts)
	}
	if policy.MaxAttempts == 0 {
		return RestartPolicy{}, nil
	}
	if policy.InitialBackoff == 0 {
		policy.InitialBackoff = defaultRestartInitialBackoff
	}
	if policy.MaxBackoff == 0 {
		policy.MaxBackoff = defaultRestartMaxBackoff
	}
	if policy.StableAfter == 0 {
		policy.StableAfter = defaultRestartStableAfter
	}
	if policy.InitialBackoff < 0 || policy.MaxBackoff < 0 || policy.StableAfter < 0 {
		return RestartPolicy{}, errors.New("Extension restart durations must not be negative")
	}
	if policy.InitialBackoff > policy.MaxBackoff {
		return RestartPolicy{}, errors.New("Extension restart initial backoff must not exceed maximum backoff")
	}
	return policy, nil
}

func initialRestartState(policy RestartPolicy) RestartState {
	if policy.MaxAttempts == 0 {
		return RestartStateDisabled
	}
	return RestartStateReady
}

func (manager *Manager) acquireErrorLocked() error {
	if manager.closed {
		return ErrHostClosed
	}
	if manager.current != nil {
		if !manager.current.process.exited() {
			return nil
		}
		if manager.restartPolicy.MaxAttempts == 0 {
			return ErrProcessExited
		}
		return ErrExtensionRestarting
	}
	switch manager.restartState {
	case RestartStateRestarting:
		return ErrExtensionRestarting
	case RestartStateCircuitOpen:
		return ErrExtensionCircuitOpen
	default:
		return ErrHostClosed
	}
}

func (manager *Manager) watchGeneration(generation *generation, stabilityPending bool) {
	if manager.restartPolicy.MaxAttempts == 0 {
		return
	}
	manager.restartWait.Add(1)
	go func() {
		defer manager.restartWait.Done()
		if stabilityPending {
			timer := time.NewTimer(manager.restartPolicy.StableAfter)
			select {
			case <-generation.process.done():
				stopTimer(timer)
				manager.restartGeneration(generation)
				return
			case <-timer.C:
				manager.markGenerationStable(generation)
			case <-manager.restartContext.Done():
				stopTimer(timer)
				return
			}
		}
		select {
		case <-generation.process.done():
			manager.restartGeneration(generation)
		case <-manager.restartContext.Done():
		}
	}()
}

func (manager *Manager) restartGeneration(failed *generation) {
	manager.reloadMu.Lock()
	defer manager.reloadMu.Unlock()

	manager.mu.Lock()
	if manager.closed || manager.current != failed || failed.retiring {
		manager.mu.Unlock()
		return
	}
	failed.retiring = true
	manager.current = nil
	manager.restartState = RestartStateRestarting
	manager.restartErrorType = errorType(ErrProcessExited)
	retireNow := failed.refs == 0
	manager.mu.Unlock()
	if retireNow {
		manager.retire(failed)
	}

	for {
		attempt, ok := manager.beginRestartAttempt()
		if !ok {
			status := manager.RestartStatus()
			if status.State == RestartStateCircuitOpen {
				manager.debug("extension.restart.circuit_open", "attempts", status.Attempts)
			}
			return
		}
		delay := restartBackoff(manager.restartPolicy, attempt)
		manager.debug("extension.restart.scheduled",
			"failed_generation", failed.number,
			"attempt", attempt,
			"delay_ms", delay.Milliseconds(),
		)
		if !waitForRestart(manager.restartContext, delay) {
			return
		}
		manager.debug("extension.restart.started",
			"failed_generation", failed.number,
			"candidate_generation", manager.next,
			"attempt", attempt,
		)
		candidateProcess, err := Start(manager.restartContext, manager.processOptions)
		if err == nil {
			err = restoreProcessState(
				manager.restartContext,
				manager.stateStore,
				manager.extensionID,
				manager.stateScope,
				candidateProcess,
				true,
			)
		}
		var candidate *generation
		if err == nil {
			candidate, err = manager.configureGeneration(candidateProcess, manager.next)
		}
		if err == nil && candidateProcess.exited() {
			err = ErrProcessExited
		}
		if err != nil {
			if candidateProcess != nil {
				_ = candidateProcess.Close()
			}
			if !manager.recordRestartFailure(attempt, err) {
				return
			}
			continue
		}

		manager.mu.Lock()
		if manager.closed || manager.current != nil {
			manager.mu.Unlock()
			_ = candidateProcess.Close()
			return
		}
		number := manager.next
		manager.current = candidate
		manager.next++
		manager.all[number] = candidate
		manager.restartState = RestartStateReady
		manager.restartErrorType = ""
		manager.mu.Unlock()
		manager.watchGeneration(candidate, true)
		manager.debug("extension.restart.completed",
			"failed_generation", failed.number,
			"generation", number,
			"attempt", attempt,
		)
		return
	}
}

func (manager *Manager) beginRestartAttempt() (int, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return 0, false
	}
	if manager.restartAttempts >= manager.restartPolicy.MaxAttempts {
		manager.restartState = RestartStateCircuitOpen
		return 0, false
	}
	manager.restartAttempts++
	return manager.restartAttempts, true
}

func (manager *Manager) recordRestartFailure(attempt int, err error) bool {
	manager.mu.Lock()
	manager.restartErrorType = errorType(err)
	exhausted := manager.restartAttempts >= manager.restartPolicy.MaxAttempts
	if exhausted {
		manager.restartState = RestartStateCircuitOpen
	}
	closed := manager.closed
	manager.mu.Unlock()
	manager.debug("extension.restart.failed",
		"attempt", attempt,
		"error_type", errorType(err),
	)
	if exhausted {
		manager.debug("extension.restart.circuit_open", "attempts", attempt)
	}
	return !closed && !exhausted
}

func (manager *Manager) markGenerationStable(generation *generation) {
	manager.mu.Lock()
	if !manager.closed && manager.current == generation && !generation.process.exited() {
		manager.restartAttempts = 0
		manager.restartErrorType = ""
	}
	manager.mu.Unlock()
}

func (manager *Manager) reloadState(ctx context.Context, old *generation) (json.RawMessage, bool, error) {
	if old != nil && !old.process.exited() {
		state, err := old.process.Snapshot(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("snapshot generation %d: %w", old.number, err)
		}
		if manager.stateStore != nil {
			if err := manager.stateStore.Set(ctx, manager.extensionID, manager.stateScope, "snapshot", state); err != nil {
				return nil, false, fmt.Errorf("persist generation %d state: %w", old.number, err)
			}
		}
		return state, true, nil
	}
	if manager.stateStore == nil {
		return nil, false, nil
	}
	state, err := manager.stateStore.Get(ctx, manager.extensionID, manager.stateScope, "snapshot")
	if errors.Is(err, extension.ErrStateNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load persisted Extension state: %w", err)
	}
	return append(json.RawMessage(nil), state...), true, nil
}

func (manager *Manager) persistProcessState(ctx context.Context, process *Process) error {
	if manager.stateStore == nil {
		return nil
	}
	state, err := process.Snapshot(ctx)
	if err != nil {
		return err
	}
	return manager.stateStore.Set(ctx, manager.extensionID, manager.stateScope, "snapshot", state)
}

func restoreProcessState(
	ctx context.Context,
	store extension.StateStore,
	extensionID string,
	stateScope string,
	process *Process,
	persist bool,
) error {
	if store == nil {
		return nil
	}
	persisted, loadErr := store.Get(ctx, extensionID, stateScope, "snapshot")
	if loadErr == nil {
		if err := process.Restore(ctx, persisted); err != nil {
			return fmt.Errorf("restore persisted Extension state: %w", err)
		}
		if _, err := process.HealthCheck(ctx); err != nil {
			return fmt.Errorf("health check persisted Extension state: %w", err)
		}
	} else if !errors.Is(loadErr, extension.ErrStateNotFound) {
		return fmt.Errorf("load persisted Extension state: %w", loadErr)
	}
	if !persist {
		return nil
	}
	state, err := process.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("snapshot Extension state: %w", err)
	}
	if err := store.Set(ctx, extensionID, stateScope, "snapshot", state); err != nil {
		return fmt.Errorf("persist Extension state: %w", err)
	}
	return nil
}

func restartBackoff(policy RestartPolicy, attempt int) time.Duration {
	delay := policy.InitialBackoff
	for current := 1; current < attempt && delay < policy.MaxBackoff; current++ {
		if delay > policy.MaxBackoff/2 {
			return policy.MaxBackoff
		}
		delay *= 2
	}
	if delay > policy.MaxBackoff {
		return policy.MaxBackoff
	}
	return delay
}

func waitForRestart(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer stopTimer(timer)
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
