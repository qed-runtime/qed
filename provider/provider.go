// Package provider contains shared primitives for model Provider implementations
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// CredentialSource resolves one Provider credential for an HTTP request
//
// Implementations must honor context cancellation, must be safe for concurrent
// use, and must not include credential values in returned errors
type CredentialSource interface {
	Credential(ctx context.Context) (string, error)
}

// CredentialSourceFunc adapts a function to CredentialSource
type CredentialSourceFunc func(context.Context) (string, error)

// Credential resolves one Provider credential
func (source CredentialSourceFunc) Credential(ctx context.Context) (string, error) {
	if source == nil {
		return "", errors.New("credential source function is nil")
	}
	return source(ctx)
}

// HTTPClient performs one HTTP request
//
// *http.Client implements HTTPClient. Implementations must be safe for
// concurrent use when shared by a Provider
type HTTPClient interface {
	Do(request *http.Request) (*http.Response, error)
}

// HTTPError describes a non-success response from a model Provider API
type HTTPError struct {
	// StatusCode is the HTTP response status code
	StatusCode int
	// Type is the Provider-specific error type when available
	Type string
	// Code is the Provider-specific error code when available
	Code string
	// Message is the Provider-provided error description when available
	Message string
	// RequestID identifies the failed API request when available
	RequestID string
}

// Error returns a diagnostic that does not include credentials or request bodies
func (apiError *HTTPError) Error() string {
	message := fmt.Sprintf("provider API returned HTTP %d", apiError.StatusCode)
	if apiError.Type != "" {
		message += " " + apiError.Type
	}
	if apiError.Code != "" {
		message += "[" + apiError.Code + "]"
	}
	if apiError.Message != "" {
		message += ": " + apiError.Message
	}
	if apiError.RequestID != "" {
		message += " (request_id=" + apiError.RequestID + ")"
	}
	return message
}
