// Package httpjson provides the shared JSON-over-HTTP transport for Providers
package httpjson

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	providerbase "github.com/qed-runtime/qed/provider"
)

const maximumErrorBodyBytes = 1 << 20

// Endpoint validates a base URL and appends one API path component
func Endpoint(baseURL, defaultBaseURL, apiPath string) (string, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultBaseURL
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("base URL scheme must be http or https")
	}
	if parsed.Host == "" {
		return "", errors.New("base URL host is required")
	}
	if parsed.User != nil {
		return "", errors.New("base URL must not contain credentials")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("base URL must not contain a query or fragment")
	}
	if parsed.RawPath != "" {
		return "", errors.New("base URL must not contain an escaped path")
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + strings.TrimLeft(apiPath, "/")
	return parsed.String(), nil
}

// Post sends a JSON request and decodes one JSON response
func Post(
	ctx context.Context,
	client providerbase.HTTPClient,
	endpoint string,
	headers map[string]string,
	requestValue, responseValue any,
) error {
	payload, err := json.Marshal(requestValue)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "qed-runtime")
	for name, value := range headers {
		if value != "" {
			request.Header.Set(name, value)
		}
	}

	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return decodeHTTPError(response)
	}

	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(responseValue); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode response: multiple JSON values")
		}
		return fmt.Errorf("decode response trailer: %w", err)
	}
	return nil
}

func decodeHTTPError(response *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumErrorBodyBytes+1))
	if err != nil {
		return fmt.Errorf("read HTTP %d error response: %w", response.StatusCode, err)
	}
	if len(body) > maximumErrorBodyBytes {
		body = body[:maximumErrorBodyBytes]
	}

	apiError := &providerbase.HTTPError{
		StatusCode: response.StatusCode,
		RequestID:  response.Header.Get("x-request-id"),
		RetryAfter: parseRetryAfter(response.Header.Get("retry-after"), time.Now()),
	}
	if apiError.RequestID == "" {
		apiError.RequestID = response.Header.Get("request-id")
	}

	var envelope struct {
		RequestID string `json:"request_id"`
		Error     struct {
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		apiError.Type = envelope.Error.Type
		apiError.Code = rawScalar(envelope.Error.Code)
		apiError.Message = envelope.Error.Message
		if apiError.RequestID == "" {
			apiError.RequestID = envelope.RequestID
		}
	}
	if apiError.Message == "" {
		apiError.Message = http.StatusText(response.StatusCode)
	}
	return apiError
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseUint(value, 10, 31); err == nil {
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}

func rawScalar(value json.RawMessage) string {
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return ""
	}
	var text string
	if json.Unmarshal(value, &text) == nil {
		return text
	}
	return string(value)
}
