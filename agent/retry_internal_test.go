package agent

import (
	"testing"
	"time"
)

func TestProviderRetryDelayWithJitterIsBoundedAndStableWithinRun(t *testing.T) {
	t.Parallel()

	policy := ProviderRetryPolicy{
		InitialBackoff: 8 * time.Second,
		MaxBackoff:     8 * time.Second,
	}
	const serverHint = 10 * time.Second
	first := providerRetryDelayWithJitter(policy, 1, serverHint, "run-stable")
	repeated := providerRetryDelayWithJitter(policy, 1, serverHint, "run-stable")
	if first != repeated {
		t.Fatalf("same Run retry delays = %s and %s", first, repeated)
	}
	if first < serverHint || first > serverHint+maximumProviderRetryJitter {
		t.Fatalf("retry delay = %s, want %s through %s", first, serverHint, serverHint+maximumProviderRetryJitter)
	}
	otherRun := providerRetryDelayWithJitter(policy, 1, serverHint, "run-other")
	if otherRun == first {
		t.Fatalf("different Runs received the same deterministic jitter: %s", first)
	}

	fallback := providerRetryDelayWithJitter(policy, 1, 0, "run-fallback")
	if fallback < policy.InitialBackoff || fallback > policy.InitialBackoff+maximumProviderRetryJitter {
		t.Fatalf(
			"fallback retry delay = %s, want %s through %s",
			fallback,
			policy.InitialBackoff,
			policy.InitialBackoff+maximumProviderRetryJitter,
		)
	}
}
