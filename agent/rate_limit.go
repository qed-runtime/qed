package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	defaultProviderMaxConcurrency = 4
	maximumProviderConcurrency    = 1024
)

// ProviderRateLimitPolicy configures outbound Provider request concurrency
type ProviderRateLimitPolicy struct {
	// MaxConcurrency bounds active Provider streams, defaults to four, and must not exceed 1024
	MaxConcurrency int
}

// ProviderRateLimitController coordinates outbound Provider attempts
//
// Acquire must call onWait before blocking for capacity or cooldown and may
// call it again when the active constraint changes. It must return a non-nil
// idempotent release function on success and honor Context cancellation.
// ObserveRateLimit publishes the effective delay before the current attempt
// releases its capacity. Implementations must be safe for concurrent use and
// MaxConcurrency must remain a stable value from one through 1024
type ProviderRateLimitController interface {
	// Acquire waits until one outbound attempt may start
	Acquire(
		ctx context.Context,
		onWait func(ProviderRateLimitWaitInfo) error,
	) (release func(), waitDuration time.Duration, err error)
	// ObserveRateLimit extends the shared cooldown when delay ends later
	ObserveRateLimit(delay time.Duration)
	// MaxConcurrency returns the stable active-stream limit for this controller
	MaxConcurrency() int
}

// ProviderRateLimiter coordinates active streams and shared rate-limit cooldowns
//
// Share one ProviderRateLimiter between every Runtime that uses the same
// Provider credential and rate-limit pool. Its zero value is ready for use with
// the default policy. A ProviderRateLimiter must not be copied after first use
type ProviderRateLimiter struct {
	initializeOnce  sync.Once
	maxConcurrency  int
	active          chan struct{}
	cooldownMu      sync.Mutex
	cooldownUntil   time.Time
	cooldownChanged chan struct{}
}

// NewProviderRateLimiter validates policy and returns a concurrency-safe limiter
func NewProviderRateLimiter(policy ProviderRateLimitPolicy) (*ProviderRateLimiter, error) {
	if policy.MaxConcurrency < 0 || policy.MaxConcurrency > maximumProviderConcurrency {
		return nil, fmt.Errorf(
			"Provider rate limit max concurrency must be between 1 and %d when set",
			maximumProviderConcurrency,
		)
	}
	if policy.MaxConcurrency == 0 {
		policy.MaxConcurrency = defaultProviderMaxConcurrency
	}
	limiter := &ProviderRateLimiter{maxConcurrency: policy.MaxConcurrency}
	limiter.initialize()
	return limiter, nil
}

// MaxConcurrency returns the configured maximum number of active Provider streams
func (limiter *ProviderRateLimiter) MaxConcurrency() int {
	limiter.initialize()
	return limiter.maxConcurrency
}

func (limiter *ProviderRateLimiter) initialize() {
	limiter.initializeOnce.Do(func() {
		if limiter.maxConcurrency == 0 {
			limiter.maxConcurrency = defaultProviderMaxConcurrency
		}
		limiter.active = make(chan struct{}, limiter.maxConcurrency)
		limiter.cooldownChanged = make(chan struct{})
	})
}

// Acquire waits for active stream capacity and any shared cooldown
func (limiter *ProviderRateLimiter) Acquire(
	ctx context.Context,
	onWait func(ProviderRateLimitWaitInfo) error,
) (func(), time.Duration, error) {
	if ctx == nil {
		return nil, 0, errors.New("context must not be nil")
	}
	limiter.initialize()
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	waitStartedAt := time.Now()
	waiting := false
	var lastWait *ProviderRateLimitWaitInfo
	notify := func(wait ProviderRateLimitWaitInfo) error {
		waiting = true
		if onWait == nil {
			return nil
		}
		if lastWait != nil && *lastWait == wait {
			return nil
		}
		last := wait
		lastWait = &last
		return onWait(wait)
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if delay, changed := limiter.cooldown(); delay > 0 {
			if err := notify(ProviderRateLimitWaitInfo{
				Reason:                 ProviderRateLimitWaitCooldown,
				MaxConcurrency:         limiter.maxConcurrency,
				RetryAfterMilliseconds: delay.Milliseconds(),
			}); err != nil {
				return nil, 0, err
			}
			if err := waitForProviderCapacity(ctx, delay, changed); err != nil {
				return nil, 0, err
			}
			continue
		}

		select {
		case limiter.active <- struct{}{}:
			if delay, _ := limiter.cooldown(); delay > 0 {
				<-limiter.active
				continue
			}
			if err := ctx.Err(); err != nil {
				<-limiter.active
				return nil, 0, err
			}
			var releaseOnce sync.Once
			release := func() {
				releaseOnce.Do(func() { <-limiter.active })
			}
			if !waiting {
				return release, 0, nil
			}
			return release, time.Since(waitStartedAt), nil
		default:
			if err := notify(ProviderRateLimitWaitInfo{
				Reason:         ProviderRateLimitWaitConcurrency,
				MaxConcurrency: limiter.maxConcurrency,
			}); err != nil {
				return nil, 0, err
			}
		}

		select {
		case limiter.active <- struct{}{}:
			if delay, _ := limiter.cooldown(); delay > 0 {
				<-limiter.active
				continue
			}
			if err := ctx.Err(); err != nil {
				<-limiter.active
				return nil, 0, err
			}
			var releaseOnce sync.Once
			release := func() {
				releaseOnce.Do(func() { <-limiter.active })
			}
			return release, time.Since(waitStartedAt), nil
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
}

// ObserveRateLimit extends the shared cooldown by delay when it is later
func (limiter *ProviderRateLimiter) ObserveRateLimit(delay time.Duration) {
	if delay <= 0 {
		return
	}
	limiter.initialize()
	until := time.Now().Add(delay)

	limiter.cooldownMu.Lock()
	if until.After(limiter.cooldownUntil) {
		limiter.cooldownUntil = until
		close(limiter.cooldownChanged)
		limiter.cooldownChanged = make(chan struct{})
	}
	limiter.cooldownMu.Unlock()
}

func (limiter *ProviderRateLimiter) cooldown() (time.Duration, <-chan struct{}) {
	limiter.cooldownMu.Lock()
	delay := time.Until(limiter.cooldownUntil)
	changed := limiter.cooldownChanged
	limiter.cooldownMu.Unlock()
	if delay <= 0 {
		return 0, changed
	}
	return delay, changed
}

func waitForProviderCapacity(ctx context.Context, delay time.Duration, changed <-chan struct{}) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		return nil
	case <-timer.C:
		return nil
	}
}

func validateProviderRateLimitWait(info ProviderRateLimitWaitInfo, maxConcurrency int) error {
	switch info.Reason {
	case ProviderRateLimitWaitConcurrency, ProviderRateLimitWaitCooldown:
	default:
		return fmt.Errorf("Provider rate limiter returned unsupported wait reason %q", info.Reason)
	}
	if info.MaxConcurrency != maxConcurrency {
		return fmt.Errorf(
			"Provider rate limiter wait max concurrency is %d, want %d",
			info.MaxConcurrency,
			maxConcurrency,
		)
	}
	if info.RetryAfterMilliseconds < 0 {
		return errors.New("Provider rate limiter returned a negative retry delay")
	}
	return nil
}
