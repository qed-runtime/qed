// Package openaicodex provides a ChatGPT-authenticated Codex Responses Provider
package openaicodex

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/qed-runtime/qed/agent"
	providerbase "github.com/qed-runtime/qed/provider"
	"github.com/qed-runtime/qed/provider/internal/httpjson"
	providerprofile "github.com/qed-runtime/qed/provider/internal/profile"
)

const (
	defaultEndpoint     = "https://chatgpt.com/backend-api/codex/responses"
	defaultInstructions = "You are a helpful assistant."
	defaultOriginator   = "qed"
)

// Authorization contains the short-lived ChatGPT authorization required by
// the Codex backend
//
// Callers must treat every field except FedRAMP as a secret or private account
// identifier and must not log the value
type Authorization struct {
	// AccessToken is the OAuth bearer token sent to the Codex backend
	AccessToken string
	// AccountID selects the ChatGPT account associated with AccessToken
	AccountID string
	// FedRAMP enables the backend's FedRAMP routing header
	FedRAMP bool
}

// AuthorizationSource resolves ChatGPT authorization for each request
//
// Implementations must honor context cancellation, be safe for concurrent use,
// refresh expiring credentials when possible, and exclude secret values from
// returned errors
type AuthorizationSource interface {
	Authorization(ctx context.Context) (Authorization, error)
}

// AuthorizationSourceFunc adapts a function to AuthorizationSource
type AuthorizationSourceFunc func(context.Context) (Authorization, error)

// Authorization resolves ChatGPT authorization
func (source AuthorizationSourceFunc) Authorization(ctx context.Context) (Authorization, error) {
	if source == nil {
		return Authorization{}, errors.New("OpenAI Codex authorization source function is nil")
	}
	return source(ctx)
}

// UnauthorizedRecoverer can refresh or reload authorization after the backend
// rejects a request
//
// The Provider retries at most once and passes the rejected authorization so a
// shared source can avoid refreshing a credential that another caller already
// replaced
type UnauthorizedRecoverer interface {
	RecoverUnauthorized(ctx context.Context, rejected Authorization) (Authorization, error)
}

// Config configures a ChatGPT-authenticated Codex Provider
type Config struct {
	// ProfileID distinguishes this Provider instance from other Codex profiles
	ProfileID string
	// AuthorizationSource supplies ChatGPT access and account credentials
	AuthorizationSource AuthorizationSource
	// Model is the exact Codex model identifier sent with every request
	Model string
	// Originator identifies the embedding client and defaults to qed
	Originator string
	// Version identifies the embedding client version and defaults to the QED
	// module version when available
	Version string
	// HTTPClient defaults to http.DefaultClient
	HTTPClient providerbase.HTTPClient

	endpoint string
}

// Provider implements agent.Provider for the ChatGPT Codex Responses backend
type Provider struct {
	authorizationSource AuthorizationSource
	endpoint            string
	model               string
	originator          string
	version             string
	client              providerbase.HTTPClient
	name                string
}

// New validates config and constructs a ChatGPT-authenticated Codex Provider
func New(config Config) (*Provider, error) {
	if config.AuthorizationSource == nil {
		return nil, errors.New("OpenAI Codex authorization source is required")
	}
	model := strings.TrimSpace(config.Model)
	if model == "" {
		return nil, errors.New("OpenAI Codex model is required")
	}
	if model != config.Model {
		return nil, errors.New("OpenAI Codex model must not have surrounding whitespace")
	}
	originator := config.Originator
	if originator == "" {
		originator = defaultOriginator
	}
	if strings.TrimSpace(originator) != originator {
		return nil, errors.New("OpenAI Codex originator must not have surrounding whitespace")
	}
	version := config.Version
	if version == "" {
		version = moduleVersion()
	}
	if strings.TrimSpace(version) != version {
		return nil, errors.New("OpenAI Codex version must not have surrounding whitespace")
	}
	name, err := providerprofile.Name("openai-codex/responses", config.ProfileID)
	if err != nil {
		return nil, fmt.Errorf("configure OpenAI Codex profile: %w", err)
	}
	endpoint := config.endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}

	return &Provider{
		authorizationSource: config.AuthorizationSource,
		endpoint:            endpoint,
		model:               model,
		originator:          originator,
		version:             version,
		client:              config.HTTPClient,
		name:                name,
	}, nil
}

// Name returns the Provider identity used for diagnostics and continuation state
func (provider *Provider) Name() string {
	return provider.name
}

// ModelID returns the exact model identifier configured for this Provider
func (provider *Provider) ModelID() string {
	return provider.model
}

// CacheCapabilities reports the observable automatic cache behavior of the ChatGPT backend
func (provider *Provider) CacheCapabilities() agent.CacheCapabilities {
	return agent.CacheCapabilities{
		ExactPrefix:         true,
		SupportsAutomatic:   true,
		MinimumPrefixTokens: 1024,
		ExposesReadTokens:   true,
		ExposesWriteTokens:  true,
	}
}

// Stream sends one model request to the ChatGPT Codex Responses backend
func (provider *Provider) Stream(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	if ctx == nil {
		return nil, errors.New("OpenAI Codex context must not be nil")
	}
	payload, err := provider.responsesRequest(request)
	if err != nil {
		return nil, err
	}
	authorization, err := provider.authorization(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := httpjson.PostSSE(ctx, provider.client, provider.endpoint, provider.headers(authorization), payload)
	if err != nil && isUnauthorized(err) {
		if recoverer, ok := provider.authorizationSource.(UnauthorizedRecoverer); ok {
			authorization, recoverErr := recoverer.RecoverUnauthorized(ctx, authorization)
			if recoverErr != nil {
				return nil, fmt.Errorf("recover OpenAI Codex authorization: %w", recoverErr)
			}
			if err := validateAuthorization(authorization); err != nil {
				return nil, err
			}
			stream, err = httpjson.PostSSE(ctx, provider.client, provider.endpoint, provider.headers(authorization), payload)
		}
	}
	if err != nil {
		return nil, err
	}
	accumulator := &responsesStreamAccumulator{provider: provider, stream: stream}
	return &agent.ModelStreamFunc{
		NextFunc:  accumulator.next,
		CloseFunc: stream.Close,
	}, nil
}

func (provider *Provider) authorization(ctx context.Context) (Authorization, error) {
	authorization, err := provider.authorizationSource.Authorization(ctx)
	if err != nil {
		return Authorization{}, fmt.Errorf("resolve OpenAI Codex authorization: %w", err)
	}
	if err := validateAuthorization(authorization); err != nil {
		return Authorization{}, err
	}
	return authorization, nil
}

func validateAuthorization(authorization Authorization) error {
	if strings.TrimSpace(authorization.AccessToken) == "" {
		return errors.New("OpenAI Codex authorization source returned an empty access token")
	}
	if strings.TrimSpace(authorization.AccessToken) != authorization.AccessToken {
		return errors.New("OpenAI Codex authorization source returned an invalid access token")
	}
	if strings.TrimSpace(authorization.AccountID) == "" {
		return errors.New("OpenAI Codex authorization source returned an empty account ID")
	}
	if strings.TrimSpace(authorization.AccountID) != authorization.AccountID {
		return errors.New("OpenAI Codex authorization source returned an invalid account ID")
	}
	return nil
}

func (provider *Provider) headers(authorization Authorization) map[string]string {
	headers := map[string]string{
		"Authorization":      "Bearer " + authorization.AccessToken,
		"ChatGPT-Account-ID": authorization.AccountID,
		"OpenAI-Beta":        "responses=experimental",
		"originator":         provider.originator,
		"User-Agent":         "qed-runtime/" + provider.version,
		"version":            provider.version,
	}
	if authorization.FedRAMP {
		headers["X-OpenAI-Fedramp"] = "true"
	}
	return headers
}

func isUnauthorized(err error) bool {
	var httpError *providerbase.HTTPError
	return errors.As(err, &httpError) && httpError.StatusCode == 401
}

func moduleVersion() string {
	const developmentVersion = "0.0.0-dev"
	build, ok := debug.ReadBuildInfo()
	if !ok {
		return developmentVersion
	}
	if build.Main.Path == "github.com/qed-runtime/qed" && build.Main.Version != "" && build.Main.Version != "(devel)" {
		return strings.TrimPrefix(build.Main.Version, "v")
	}
	for _, dependency := range build.Deps {
		if dependency.Path == "github.com/qed-runtime/qed" && dependency.Version != "" && dependency.Version != "(devel)" {
			return strings.TrimPrefix(dependency.Version, "v")
		}
	}
	return developmentVersion
}
