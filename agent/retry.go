package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	providerbase "github.com/qed-runtime/qed/provider"
)

const (
	defaultProviderRetryMaxAttempts    = 3
	defaultProviderRetryInitialBackoff = time.Second
	defaultProviderRetryMaxBackoff     = 8 * time.Second
)

// ProviderRetryPolicy controls retry attempts for transient Provider failures
type ProviderRetryPolicy struct {
	// MaxAttempts includes the initial request and defaults to three
	//
	// Set MaxAttempts to one to disable retries
	MaxAttempts int
	// InitialBackoff is the fallback delay after the first failed attempt and defaults to one second
	InitialBackoff time.Duration
	// MaxBackoff caps the exponential fallback delay and defaults to eight seconds
	//
	// A longer server-provided Retry-After value is not capped
	MaxBackoff time.Duration
}

func normalizeProviderRetryPolicy(policy ProviderRetryPolicy) (ProviderRetryPolicy, error) {
	if policy.MaxAttempts < 0 {
		return ProviderRetryPolicy{}, errors.New("Provider retry max attempts must be positive")
	}
	if policy.InitialBackoff < 0 {
		return ProviderRetryPolicy{}, errors.New("Provider retry initial backoff must be positive")
	}
	if policy.MaxBackoff < 0 {
		return ProviderRetryPolicy{}, errors.New("Provider retry max backoff must be positive")
	}
	if policy.MaxAttempts == 0 {
		policy.MaxAttempts = defaultProviderRetryMaxAttempts
	}
	if policy.InitialBackoff == 0 {
		policy.InitialBackoff = defaultProviderRetryInitialBackoff
	}
	if policy.MaxBackoff == 0 {
		policy.MaxBackoff = defaultProviderRetryMaxBackoff
	}
	if policy.MaxBackoff < policy.InitialBackoff {
		return ProviderRetryPolicy{}, errors.New("Provider retry max backoff must not be shorter than initial backoff")
	}
	return policy, nil
}

func providerRetryDelay(policy ProviderRetryPolicy, failedAttempt int, serverHint time.Duration) time.Duration {
	delay := policy.InitialBackoff
	for attempt := 1; attempt < failedAttempt && delay < policy.MaxBackoff; attempt++ {
		if delay > policy.MaxBackoff/2 {
			delay = policy.MaxBackoff
			break
		}
		delay *= 2
	}
	if delay > policy.MaxBackoff {
		delay = policy.MaxBackoff
	}
	if serverHint > delay {
		return serverHint
	}
	return delay
}

func waitForProviderRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type runtimeProviderError struct {
	providerName string
	phase        string
	info         providerbase.ErrorInfo
	attempt      int
	err          error
}

type incompleteProviderStreamError struct{}

func (incompleteProviderStreamError) Error() string {
	return "provider stream ended without a completed message"
}

func (incompleteProviderStreamError) ProviderErrorInfo() providerbase.ErrorInfo {
	return providerbase.ErrorInfo{Code: providerbase.ErrorCodeRetryable}
}

func (providerError *runtimeProviderError) Error() string {
	return fmt.Sprintf("provider %q %s: %v", providerError.providerName, providerError.phase, providerError.err)
}

func (providerError *runtimeProviderError) Unwrap() error {
	return providerError.err
}

func (providerError *runtimeProviderError) eventInfo() ProviderErrorInfo {
	return ProviderErrorInfo{
		Code:                   providerError.info.Code,
		Attempt:                providerError.attempt,
		RetryAfterMilliseconds: providerError.info.RetryAfter.Milliseconds(),
	}
}
