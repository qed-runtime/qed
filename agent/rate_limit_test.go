package agent_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
	providerbase "github.com/qed-runtime/qed/provider"
)

func TestProviderRateLimiterPolicy(t *testing.T) {
	t.Parallel()

	limiter, err := agent.NewProviderRateLimiter(agent.ProviderRateLimitPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if limiter.MaxConcurrency() != 4 {
		t.Fatalf("default MaxConcurrency = %d, want 4", limiter.MaxConcurrency())
	}

	configured, err := agent.NewProviderRateLimiter(agent.ProviderRateLimitPolicy{MaxConcurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	if configured.MaxConcurrency() != 2 {
		t.Fatalf("configured MaxConcurrency = %d, want 2", configured.MaxConcurrency())
	}

	if _, err := agent.NewProviderRateLimiter(agent.ProviderRateLimitPolicy{MaxConcurrency: -1}); err == nil {
		t.Fatal("negative MaxConcurrency succeeded")
	}
	if _, err := agent.NewProviderRateLimiter(agent.ProviderRateLimitPolicy{MaxConcurrency: 1025}); err == nil {
		t.Fatal("excessive MaxConcurrency succeeded")
	}
	if _, _, err := configured.Acquire(nil, nil); err == nil {
		t.Fatal("Acquire(nil) succeeded")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if release, _, err := configured.Acquire(canceled, nil); !errors.Is(err, context.Canceled) || release != nil {
		t.Fatalf("Acquire(canceled) release_nil=%t error=%v", release == nil, err)
	}
}

func TestRuntimeRejectsInvalidProviderRateLimitController(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{{
		message: agent.Message{Role: agent.RoleAssistant, Text: "must not run"},
	}}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:            provider,
		ProviderRateLimiter: nilReleaseRateLimitController{},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, runErr := collectRun(handle)
	if runErr == nil || !strings.Contains(runErr.Error(), "nil release function") {
		t.Fatalf("Run error = %v", runErr)
	}
	if result.ProviderCalls != 0 || len(provider.Requests()) != 0 {
		t.Fatalf("invalid controller reached Provider: result=%d requests=%d", result.ProviderCalls, len(provider.Requests()))
	}
}

func TestRuntimeSharesProviderConcurrencyLimit(t *testing.T) {
	t.Parallel()

	limiter, err := agent.NewProviderRateLimiter(agent.ProviderRateLimitPolicy{MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	provider := newControlledProvider(2)
	firstRuntime := newRateLimitedRuntime(t, provider, limiter, agent.ProviderRetryPolicy{MaxAttempts: 1})
	secondRuntime := newRateLimitedRuntime(t, provider, limiter, agent.ProviderRetryPolicy{MaxAttempts: 1})

	first, _ := startCollectedRun(t, firstRuntime, context.Background())
	awaitProviderStart(t, provider.started)
	second, secondWaiting := startCollectedRun(t, secondRuntime, context.Background())

	select {
	case <-secondWaiting:
	case <-provider.started:
		t.Fatal("second Provider request started while the shared limit was occupied")
	case <-time.After(time.Second):
		t.Fatal("second Run did not report a Provider capacity wait")
	}

	provider.release <- struct{}{}
	awaitProviderStart(t, provider.started)
	provider.release <- struct{}{}

	firstOutcome := <-first
	secondOutcome := <-second
	for index, outcome := range []collectedRun{firstOutcome, secondOutcome} {
		if outcome.err != nil || outcome.result.Status != agent.RunStatusCompleted {
			t.Fatalf("Run %d = %#v, %v", index+1, outcome.result, outcome.err)
		}
	}
	if provider.MaximumActive() != 1 {
		t.Fatalf("maximum active Provider streams = %d, want 1", provider.MaximumActive())
	}
	assertProviderWait(t, secondOutcome.events, agent.ProviderRateLimitWaitConcurrency)
}

func TestRuntimeCancelsWhileWaitingForProviderCapacityWithoutChargingBudget(t *testing.T) {
	t.Parallel()

	limiter, err := agent.NewProviderRateLimiter(agent.ProviderRateLimitPolicy{MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	provider := newControlledProvider(1)
	firstRuntime := newRateLimitedRuntime(t, provider, limiter, agent.ProviderRetryPolicy{MaxAttempts: 1})
	secondRuntime := newRateLimitedRuntime(t, provider, limiter, agent.ProviderRetryPolicy{MaxAttempts: 1})

	first, _ := startCollectedRun(t, firstRuntime, context.Background())
	awaitProviderStart(t, provider.started)
	budget, err := agent.NewBudget(agent.BudgetLimits{MaxProviderCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	secondHandle, err := secondRuntime.Run(ctx, agent.RunRequest{
		Budget: budget,
		Input:  []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var secondEvents []agent.Event
	for event := range secondHandle.Events() {
		secondEvents = append(secondEvents, event)
		if event.Type == agent.EventProviderRateLimitWait {
			cancel()
		}
	}
	secondResult, secondErr := secondHandle.Wait()
	secondOutcome := collectedRun{events: secondEvents, result: secondResult, err: secondErr}
	if !errors.Is(secondOutcome.err, context.Canceled) || secondOutcome.result.Status != agent.RunStatusCanceled {
		t.Fatalf("waiting Run = %#v, %v", secondOutcome.result, secondOutcome.err)
	}
	if secondOutcome.result.ProviderCalls != 0 || budget.Snapshot().ProviderCalls != 0 {
		t.Fatalf("waiting Run charged Provider calls: result=%d budget=%d", secondOutcome.result.ProviderCalls, budget.Snapshot().ProviderCalls)
	}
	assertProviderWait(t, secondOutcome.events, agent.ProviderRateLimitWaitConcurrency)

	provider.release <- struct{}{}
	firstOutcome := <-first
	if firstOutcome.err != nil {
		t.Fatal(firstOutcome.err)
	}
}

func TestProviderRateLimiterRechecksCooldownAfterCapacityWait(t *testing.T) {
	t.Parallel()

	limiter, err := agent.NewProviderRateLimiter(agent.ProviderRateLimitPolicy{MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	firstRelease, _, err := limiter.Acquire(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := make(chan agent.ProviderRateLimitWaitReason, 2)
	type acquireOutcome struct {
		release func()
		err     error
	}
	outcome := make(chan acquireOutcome, 1)
	go func() {
		release, _, acquireErr := limiter.Acquire(ctx, func(wait agent.ProviderRateLimitWaitInfo) error {
			waits <- wait.Reason
			return nil
		})
		outcome <- acquireOutcome{release: release, err: acquireErr}
	}()

	select {
	case reason := <-waits:
		if reason != agent.ProviderRateLimitWaitConcurrency {
			t.Fatalf("first wait reason = %q, want %q", reason, agent.ProviderRateLimitWaitConcurrency)
		}
	case <-time.After(time.Second):
		t.Fatal("queued acquisition did not report a concurrency wait")
	}

	limiter.ObserveRateLimit(time.Second)
	firstRelease()
	select {
	case reason := <-waits:
		if reason != agent.ProviderRateLimitWaitCooldown {
			t.Fatalf("second wait reason = %q, want %q", reason, agent.ProviderRateLimitWaitCooldown)
		}
	case acquired := <-outcome:
		if acquired.release != nil {
			acquired.release()
		}
		t.Fatalf("queued acquisition bypassed cooldown: %v", acquired.err)
	case <-time.After(time.Second):
		t.Fatal("queued acquisition did not recheck cooldown")
	}

	cancel()
	acquired := <-outcome
	if !errors.Is(acquired.err, context.Canceled) || acquired.release != nil {
		t.Fatalf("canceled acquisition = %#v", acquired)
	}
}

func TestProviderRateLimiterHonorsDeadlineDuringCooldown(t *testing.T) {
	t.Parallel()

	limiter, err := agent.NewProviderRateLimiter(agent.ProviderRateLimitPolicy{MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	limiter.ObserveRateLimit(time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	waitObserved := false
	release, _, err := limiter.Acquire(ctx, func(wait agent.ProviderRateLimitWaitInfo) error {
		waitObserved = wait.Reason == agent.ProviderRateLimitWaitCooldown
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || release != nil {
		t.Fatalf("deadline acquisition release_nil=%t error=%v", release == nil, err)
	}
	if !waitObserved {
		t.Fatal("deadline acquisition did not report a cooldown wait")
	}
}

func TestRuntimeSharesRateLimitCooldownBetweenRuns(t *testing.T) {
	t.Parallel()

	const retryAfter = 150 * time.Millisecond
	provider := &cooldownProvider{retryAfter: retryAfter, secondStarted: make(chan time.Time, 1)}
	limiter, err := agent.NewProviderRateLimiter(agent.ProviderRateLimitPolicy{MaxConcurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	policy := agent.ProviderRetryPolicy{
		MaxAttempts:    1,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
	}
	firstRuntime := newRateLimitedRuntime(t, provider, limiter, policy)
	secondRuntime := newRateLimitedRuntime(t, provider, limiter, policy)
	budget, err := agent.NewBudget(agent.BudgetLimits{MaxProviderCalls: 2})
	if err != nil {
		t.Fatal(err)
	}

	firstHandle, err := firstRuntime.Run(context.Background(), agent.RunRequest{
		Budget: budget,
		Input:  []agent.Message{{Role: agent.RoleUser, Text: "first"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, firstResult, firstErr := collectRun(firstHandle)
	if firstErr == nil || firstResult.ProviderCalls != 1 {
		t.Fatalf("first Run = %#v, %v", firstResult, firstErr)
	}

	secondStartedAt := time.Now()
	secondHandle, err := secondRuntime.Run(context.Background(), agent.RunRequest{
		Budget: budget,
		Input:  []agent.Message{{Role: agent.RoleUser, Text: "second"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	secondEvents, secondResult, secondErr := collectRun(secondHandle)
	if secondErr != nil || secondResult.Status != agent.RunStatusCompleted {
		t.Fatalf("second Run = %#v, %v", secondResult, secondErr)
	}
	providerStartedAt := <-provider.secondStarted
	if waited := providerStartedAt.Sub(secondStartedAt); waited < 100*time.Millisecond {
		t.Fatalf("shared cooldown wait = %s, want at least 100ms", waited)
	}
	assertProviderWait(t, secondEvents, agent.ProviderRateLimitWaitCooldown)
	if budget.Snapshot().ProviderCalls != 2 {
		t.Fatalf("shared Provider call budget = %d, want 2", budget.Snapshot().ProviderCalls)
	}
}

type collectedRun struct {
	events []agent.Event
	result agent.RunResult
	err    error
}

func newRateLimitedRuntime(
	t *testing.T,
	provider agent.Provider,
	limiter *agent.ProviderRateLimiter,
	retry agent.ProviderRetryPolicy,
) *agent.Runtime {
	t.Helper()
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:            provider,
		ProviderRetry:       retry,
		ProviderRateLimiter: limiter,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func startCollectedRun(
	t *testing.T,
	runtime *agent.Runtime,
	ctx context.Context,
) (<-chan collectedRun, <-chan struct{}) {
	t.Helper()
	handle, err := runtime.Run(ctx, agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan collectedRun, 1)
	waiting := make(chan struct{}, 1)
	go func() {
		var events []agent.Event
		for event := range handle.Events() {
			events = append(events, event)
			if event.Type == agent.EventProviderRateLimitWait {
				select {
				case waiting <- struct{}{}:
				default:
				}
			}
		}
		runResult, runErr := handle.Wait()
		result <- collectedRun{events: events, result: runResult, err: runErr}
	}()
	return result, waiting
}

func awaitProviderStart(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Provider did not start")
	}
}

func assertProviderWait(t *testing.T, events []agent.Event, reason agent.ProviderRateLimitWaitReason) {
	t.Helper()
	for _, event := range events {
		if event.Type != agent.EventProviderRateLimitWait {
			continue
		}
		if event.ProviderRateLimitWait == nil || event.ProviderRateLimitWait.Reason != reason {
			t.Fatalf("Provider wait Event = %#v, want reason %q", event, reason)
		}
		return
	}
	t.Fatalf("Events do not contain a Provider wait: %#v", events)
}

type controlledProvider struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	active  int
	maximum int
}

func newControlledProvider(capacity int) *controlledProvider {
	return &controlledProvider{
		started: make(chan struct{}, capacity),
		release: make(chan struct{}, capacity),
	}
}

func (provider *controlledProvider) Name() string { return "controlled" }

func (provider *controlledProvider) Stream(ctx context.Context, _ agent.ModelRequest) (agent.ModelStream, error) {
	provider.mu.Lock()
	provider.active++
	if provider.active > provider.maximum {
		provider.maximum = provider.active
	}
	provider.mu.Unlock()
	provider.started <- struct{}{}

	select {
	case <-ctx.Done():
		provider.finish()
		return nil, ctx.Err()
	case <-provider.release:
		provider.finish()
		return agent.MessageStream(agent.Message{Role: agent.RoleAssistant, Text: "done"}), nil
	}
}

func (provider *controlledProvider) finish() {
	provider.mu.Lock()
	provider.active--
	provider.mu.Unlock()
}

func (provider *controlledProvider) MaximumActive() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.maximum
}

type cooldownProvider struct {
	mu            sync.Mutex
	calls         int
	retryAfter    time.Duration
	secondStarted chan time.Time
}

type nilReleaseRateLimitController struct{}

func (nilReleaseRateLimitController) Acquire(
	context.Context,
	func(agent.ProviderRateLimitWaitInfo) error,
) (func(), time.Duration, error) {
	return nil, 0, nil
}

func (nilReleaseRateLimitController) ObserveRateLimit(time.Duration) {}

func (nilReleaseRateLimitController) MaxConcurrency() int { return 1 }

func (provider *cooldownProvider) Name() string { return "cooldown" }

func (provider *cooldownProvider) Stream(context.Context, agent.ModelRequest) (agent.ModelStream, error) {
	provider.mu.Lock()
	provider.calls++
	call := provider.calls
	provider.mu.Unlock()
	if call == 1 {
		return nil, &providerbase.HTTPError{StatusCode: 429, RetryAfter: provider.retryAfter}
	}
	provider.secondStarted <- time.Now()
	return agent.MessageStream(agent.Message{Role: agent.RoleAssistant, Text: "done"}), nil
}
