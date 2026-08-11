package provider_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/qed-runtime/qed/provider"
)

func TestClassifyError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		want       provider.ErrorCode
		retryAfter time.Duration
	}{
		{
			name:       "rate limit with server hint",
			err:        &provider.HTTPError{StatusCode: http.StatusTooManyRequests, RetryAfter: 3 * time.Second},
			want:       provider.ErrorCodeRateLimited,
			retryAfter: 3 * time.Second,
		},
		{
			name: "quota uses terminal classification before HTTP status",
			err:  &provider.HTTPError{StatusCode: http.StatusTooManyRequests, Code: "credit_balance_exhausted"},
			want: provider.ErrorCodeTerminal,
		},
		{
			name: "authentication",
			err:  &provider.HTTPError{StatusCode: http.StatusUnauthorized},
			want: provider.ErrorCodeAuthentication,
		},
		{
			name: "invalid request",
			err:  &provider.HTTPError{StatusCode: http.StatusBadRequest},
			want: provider.ErrorCodeInvalidRequest,
		},
		{
			name: "service unavailable",
			err:  &provider.HTTPError{StatusCode: http.StatusServiceUnavailable},
			want: provider.ErrorCodeRetryable,
		},
		{
			name: "Anthropic overloaded",
			err:  &provider.HTTPError{StatusCode: 529, Type: "overloaded_error"},
			want: provider.ErrorCodeRetryable,
		},
		{
			name: "OpenAI Codex stream overloaded",
			err:  &provider.APIError{Code: "server_is_overloaded"},
			want: provider.ErrorCodeRetryable,
		},
		{
			name: "structured code takes precedence over generic status",
			err:  &provider.HTTPError{StatusCode: http.StatusBadRequest, Code: "server_is_overloaded"},
			want: provider.ErrorCodeRetryable,
		},
		{
			name: "stream rate limit",
			err:  &provider.APIError{Type: "rate_limit_error"},
			want: provider.ErrorCodeRateLimited,
		},
		{
			name: "network failure",
			err:  &net.DNSError{IsTimeout: true},
			want: provider.ErrorCodeRetryable,
		},
		{
			name: "custom classified failure",
			err:  classifiedTestError{},
			want: provider.ErrorCodeRetryable,
		},
		{
			name: "canceled network failure",
			err:  errors.Join(&net.DNSError{IsTimeout: true}, context.Canceled),
			want: provider.ErrorCodeTerminal,
		},
		{
			name: "unknown failure",
			err:  errors.New("unknown"),
			want: provider.ErrorCodeTerminal,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			info := provider.ClassifyError(test.err)
			if info.Code != test.want || info.RetryAfter != test.retryAfter {
				t.Fatalf("ClassifyError() = %#v, want code %q and RetryAfter %s", info, test.want, test.retryAfter)
			}
			if got := info.Retryable(); got != (test.want == provider.ErrorCodeRetryable || test.want == provider.ErrorCodeRateLimited) {
				t.Fatalf("Retryable() = %t for %q", got, test.want)
			}
		})
	}
}

type classifiedTestError struct{}

func (classifiedTestError) Error() string {
	return "classified"
}

func (classifiedTestError) ProviderErrorInfo() provider.ErrorInfo {
	return provider.ErrorInfo{Code: provider.ErrorCodeRetryable}
}
