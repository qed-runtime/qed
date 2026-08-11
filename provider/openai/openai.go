// Package openai provides OpenAI Responses and Chat Completions Providers
package openai

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/qed-runtime/qed/agent"
	providerbase "github.com/qed-runtime/qed/provider"
	"github.com/qed-runtime/qed/provider/internal/httpjson"
	providerprofile "github.com/qed-runtime/qed/provider/internal/profile"
)

const defaultBaseURL = "https://api.openai.com/v1"

// API identifies an OpenAI wire protocol
type API string

// Supported OpenAI APIs
const (
	APIResponses       API = "responses"
	APIChatCompletions API = "chat-completions"
)

// Config configures an OpenAI Provider
type Config struct {
	// ProfileID distinguishes this Provider instance from other instances using the same API dialect
	ProfileID string
	// API selects Responses by default or Chat Completions for compatibility
	API API
	// APIKey is sent as a bearer token when non-empty
	APIKey string
	// CredentialSource resolves a bearer token for every request
	//
	// APIKey and CredentialSource are mutually exclusive
	CredentialSource providerbase.CredentialSource
	// BaseURL defaults to https://api.openai.com/v1
	//
	// A custom BaseURL receives the configured credential and should only be used when trusted
	BaseURL string
	// Model is the exact model identifier sent with every request
	Model string
	// MaxOutputTokens limits generated tokens when greater than zero
	MaxOutputTokens int
	// HTTPClient defaults to http.DefaultClient
	HTTPClient providerbase.HTTPClient
	// CacheCapabilities overrides conservative endpoint and model detection
	//
	// Use this only when a trusted OpenAI-compatible endpoint documents the
	// corresponding request fields and Usage behavior.
	CacheCapabilities *agent.CacheCapabilities
}

// Provider implements agent.Provider for one OpenAI API dialect
type Provider struct {
	api               API
	apiKey            string
	credentialSource  providerbase.CredentialSource
	endpoint          string
	model             string
	maxOutputTokens   int
	client            providerbase.HTTPClient
	name              string
	official          bool
	cacheCapabilities *agent.CacheCapabilities
}

// New validates config and constructs an OpenAI Provider
func New(config Config) (*Provider, error) {
	api := config.API
	if api == "" {
		api = APIResponses
	}

	var apiPath string
	var name string
	switch api {
	case APIResponses:
		apiPath = "responses"
		name = "openai/responses"
	case APIChatCompletions:
		apiPath = "chat/completions"
		name = "openai/chat-completions"
	default:
		return nil, fmt.Errorf("unsupported OpenAI API %q", api)
	}
	if config.APIKey != "" && config.CredentialSource != nil {
		return nil, errors.New("OpenAI API key and credential source are mutually exclusive")
	}
	name, err := providerprofile.Name(name, config.ProfileID)
	if err != nil {
		return nil, fmt.Errorf("configure OpenAI profile: %w", err)
	}

	model := strings.TrimSpace(config.Model)
	if model == "" {
		return nil, errors.New("OpenAI model is required")
	}
	if config.MaxOutputTokens < 0 {
		return nil, errors.New("OpenAI max output tokens must not be negative")
	}

	endpoint, err := httpjson.Endpoint(config.BaseURL, defaultBaseURL, apiPath)
	if err != nil {
		return nil, fmt.Errorf("configure OpenAI endpoint: %w", err)
	}
	if config.CacheCapabilities != nil {
		if err := agent.ValidateCacheCapabilities(*config.CacheCapabilities); err != nil {
			return nil, fmt.Errorf("configure OpenAI Cache Capabilities: %w", err)
		}
	}

	return &Provider{
		api:               api,
		apiKey:            config.APIKey,
		credentialSource:  config.CredentialSource,
		endpoint:          endpoint,
		model:             model,
		maxOutputTokens:   config.MaxOutputTokens,
		client:            config.HTTPClient,
		name:              name,
		official:          config.BaseURL == "" || strings.TrimRight(config.BaseURL, "/") == defaultBaseURL,
		cacheCapabilities: cloneCacheCapabilities(config.CacheCapabilities),
	}, nil
}

// Name returns the OpenAI API dialect used in diagnostics
func (provider *Provider) Name() string {
	return provider.name
}

// ModelID returns the exact model identifier configured for this Provider
func (provider *Provider) ModelID() string {
	return provider.model
}

// CacheCapabilities reports prompt cache behavior for the configured OpenAI dialect and model
func (provider *Provider) CacheCapabilities() agent.CacheCapabilities {
	if provider != nil && provider.cacheCapabilities != nil {
		return *cloneCacheCapabilities(provider.cacheCapabilities)
	}
	if provider == nil || !provider.official {
		return agent.CacheCapabilities{}
	}
	capabilities := agent.CacheCapabilities{
		ExactPrefix:         true,
		SupportsCacheKey:    true,
		SupportsAutomatic:   true,
		MinimumPrefixTokens: 1024,
		ExposesReadTokens:   true,
		ExposesWriteTokens:  true,
	}
	if supportsExplicitOpenAICache(provider.model) {
		capabilities.SupportsExplicit = true
		capabilities.MaxWriteBreakpoints = 4
		capabilities.SupportedTTLs = []agent.CacheTTL{agent.CacheTTLThirtyMinutes}
	}
	return capabilities
}

func supportsExplicitOpenAICache(model string) bool {
	model = strings.ToLower(model)
	if !strings.HasPrefix(model, "gpt-") {
		return false
	}
	version := strings.TrimPrefix(model, "gpt-")
	majorText, remainder, hasMinor := strings.Cut(version, ".")
	majorText, _, _ = strings.Cut(majorText, "-")
	major, err := strconv.Atoi(majorText)
	if err != nil {
		return false
	}
	if major > 5 {
		return true
	}
	if major != 5 || !hasMinor {
		return false
	}
	minorText, _, _ := strings.Cut(remainder, "-")
	minor, err := strconv.Atoi(minorText)
	return err == nil && minor >= 6
}

func cloneCacheCapabilities(capabilities *agent.CacheCapabilities) *agent.CacheCapabilities {
	if capabilities == nil {
		return nil
	}
	cloned := *capabilities
	cloned.SupportedTTLs = append([]agent.CacheTTL(nil), capabilities.SupportedTTLs...)
	return &cloned
}

// Complete sends one model request and returns its completed Message
func (provider *Provider) Complete(ctx context.Context, request agent.ModelRequest) (agent.Message, error) {
	if ctx == nil {
		return agent.Message{}, errors.New("OpenAI context must not be nil")
	}

	switch provider.api {
	case APIResponses:
		return provider.completeResponses(ctx, request)
	case APIChatCompletions:
		return provider.completeChat(ctx, request)
	default:
		return agent.Message{}, fmt.Errorf("unsupported OpenAI API %q", provider.api)
	}
}

// Stream sends one model request and exposes its result through the Provider stream contract
func (provider *Provider) Stream(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	if ctx == nil {
		return nil, errors.New("OpenAI context must not be nil")
	}

	switch provider.api {
	case APIResponses:
		return provider.streamResponses(ctx, request)
	case APIChatCompletions:
		return provider.streamChat(ctx, request)
	default:
		return nil, fmt.Errorf("unsupported OpenAI API %q", provider.api)
	}
}

func (provider *Provider) headers(ctx context.Context) (map[string]string, error) {
	credential := provider.apiKey
	if provider.credentialSource != nil {
		resolved, err := provider.credentialSource.Credential(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve OpenAI credential: %w", err)
		}
		if strings.TrimSpace(resolved) == "" {
			return nil, errors.New("OpenAI credential source returned an empty credential")
		}
		credential = resolved
	}

	headers := make(map[string]string, 1)
	if credential != "" {
		headers["Authorization"] = "Bearer " + credential
	}
	return headers, nil
}
