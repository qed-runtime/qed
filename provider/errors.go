package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ErrorCode is the provider-neutral classification of one Provider failure
type ErrorCode string

// Provider-neutral error codes
const (
	ErrorCodeRetryable      ErrorCode = "retryable"
	ErrorCodeRateLimited    ErrorCode = "rate_limited"
	ErrorCodeAuthentication ErrorCode = "authentication"
	ErrorCodeInvalidRequest ErrorCode = "invalid_request"
	ErrorCodeTerminal       ErrorCode = "terminal"
)

// ErrorInfo contains the provider-neutral retry information for one failure
type ErrorInfo struct {
	// Code identifies the provider-neutral error class
	Code ErrorCode
	// RetryAfter is the minimum server-requested delay when available
	RetryAfter time.Duration
}

// ClassifiedError exposes provider-neutral retry information from a custom Provider error
type ClassifiedError interface {
	error
	ProviderErrorInfo() ErrorInfo
}

// Retryable reports whether another attempt can recover without changing the request
func (info ErrorInfo) Retryable() bool {
	return info.Code == ErrorCodeRetryable || info.Code == ErrorCodeRateLimited
}

// ProviderErrorInfo returns the provider-neutral classification of an HTTP error
func (apiError *HTTPError) ProviderErrorInfo() ErrorInfo {
	if apiError == nil {
		return ErrorInfo{Code: ErrorCodeTerminal}
	}
	return ErrorInfo{
		Code:       classifyAPIError(apiError.StatusCode, apiError.Type, apiError.Code),
		RetryAfter: apiError.RetryAfter,
	}
}

// ProviderErrorInfo returns the provider-neutral classification of a structured API error
func (apiError *APIError) ProviderErrorInfo() ErrorInfo {
	if apiError == nil {
		return ErrorInfo{Code: ErrorCodeTerminal}
	}
	return ErrorInfo{Code: classifyAPIError(0, apiError.Type, apiError.Code)}
}

// ClassifyError maps a Provider failure to the stable provider-neutral error contract
//
// Unknown errors are terminal. Context cancellation and deadline errors are
// always terminal because retrying would violate the caller's lifecycle
func ClassifyError(err error) ErrorInfo {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrorInfo{Code: ErrorCodeTerminal}
	}

	var classified ClassifiedError
	if errors.As(err, &classified) {
		return normalizeErrorInfo(classified.ProviderErrorInfo())
	}

	var networkError net.Error
	if errors.As(err, &networkError) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrorInfo{Code: ErrorCodeRetryable}
	}
	return ErrorInfo{Code: ErrorCodeTerminal}
}

func normalizeErrorInfo(info ErrorInfo) ErrorInfo {
	switch info.Code {
	case ErrorCodeRetryable, ErrorCodeRateLimited, ErrorCodeAuthentication, ErrorCodeInvalidRequest, ErrorCodeTerminal:
	default:
		info.Code = ErrorCodeTerminal
	}
	if info.RetryAfter < 0 {
		info.RetryAfter = 0
	}
	return info
}

func classifyAPIError(statusCode int, errorType, providerCode string) ErrorCode {
	errorType = normalizeErrorName(errorType)
	providerCode = normalizeErrorName(providerCode)

	if isOneOf(errorType, providerCode,
		"credit_balance_exhausted",
		"insufficient_quota",
		"organization_spend_limit_exceeded",
		"organization_usage_limit_exceeded",
		"project_spend_limit_exceeded",
		"billing_error",
		"billing_hard_limit_reached",
		"billing_not_active",
	) {
		return ErrorCodeTerminal
	}
	if isOneOf(errorType, providerCode,
		"authentication_error",
		"authentication_required",
		"invalid_api_key",
		"invalid_authentication",
		"unauthorized",
	) {
		return ErrorCodeAuthentication
	}
	if isOneOf(errorType, providerCode,
		"invalid_request_error",
		"invalid_request",
		"request_too_large",
	) {
		return ErrorCodeInvalidRequest
	}
	if isOneOf(errorType, providerCode,
		"rate_limit_error",
		"rate_limit_exceeded",
		"rate_limit",
	) {
		return ErrorCodeRateLimited
	}
	if isOneOf(errorType, providerCode,
		"api_error",
		"internal_error",
		"overloaded_error",
		"server_error",
		"server_is_overloaded",
		"temporarily_unavailable",
		"timeout_error",
	) {
		return ErrorCodeRetryable
	}

	switch statusCode {
	case http.StatusUnauthorized:
		return ErrorCodeAuthentication
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return ErrorCodeInvalidRequest
	case http.StatusTooManyRequests:
		return ErrorCodeRateLimited
	case http.StatusRequestTimeout:
		return ErrorCodeRetryable
	default:
		if statusCode >= http.StatusInternalServerError {
			return ErrorCodeRetryable
		}
	}
	return ErrorCodeTerminal
}

func normalizeErrorName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func isOneOf(first, second string, values ...string) bool {
	for _, value := range values {
		if first == value || second == value {
			return true
		}
	}
	return false
}
