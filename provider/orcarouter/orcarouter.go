// Package orcarouter provides OrcaRouter Responses and Chat Completions Providers
package orcarouter

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/qed-runtime/qed/agent"
	providerbase "github.com/qed-runtime/qed/provider"
	providerprofile "github.com/qed-runtime/qed/provider/internal/profile"
	"github.com/qed-runtime/qed/provider/openai"
)

const (
	defaultBaseURL     = "https://api.orcarouter.ai/v1"
	affinityHashDomain = "qed.orcarouter.affinity.v1"
	maximumHeaderBytes = 512
)

// API identifies an OrcaRouter OpenAI-compatible wire protocol
type API = openai.API

// Supported OrcaRouter APIs
const (
	APIResponses       = openai.APIResponses
	APIChatCompletions = openai.APIChatCompletions
)

// Config configures an OrcaRouter Provider
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
	// BaseURL defaults to https://api.orcarouter.ai/v1
	//
	// A custom BaseURL receives the configured credential and should only be used when trusted
	BaseURL string
	// Model is the exact OrcaRouter model or router identifier sent with every request
	Model string
	// MaxOutputTokens limits generated tokens when greater than zero
	MaxOutputTokens int
	// HTTPClient defaults to http.DefaultClient
	HTTPClient providerbase.HTTPClient
	// CacheCapabilities declares trusted prompt-cache behavior for the selected routed model
	//
	// The default is conservative because one routed identifier can resolve to
	// models with different cache behavior.
	CacheCapabilities *agent.CacheCapabilities
}

// Provider implements agent.Provider for one OrcaRouter API dialect
//
// It reuses the OpenAI wire codec while adding OrcaRouter Session Affinity and
// response observability. Raw QED Session and Run identifiers are not sent to
// OrcaRouter.
type Provider struct {
	inner                       *openai.Provider
	name                        string
	cacheCapabilitiesConfigured bool
}

// New validates config and constructs an OrcaRouter Provider
func New(config Config) (*Provider, error) {
	api := config.API
	if api == "" {
		api = APIResponses
	}

	var namePrefix string
	switch api {
	case APIResponses:
		namePrefix = "orcarouter/responses"
	case APIChatCompletions:
		namePrefix = "orcarouter/chat-completions"
	default:
		return nil, fmt.Errorf("unsupported OrcaRouter API %q", api)
	}
	if config.APIKey != "" && config.CredentialSource != nil {
		return nil, errors.New("OrcaRouter API key and credential source are mutually exclusive")
	}
	if strings.TrimSpace(config.Model) == "" {
		return nil, errors.New("OrcaRouter model is required")
	}
	if config.MaxOutputTokens < 0 {
		return nil, errors.New("OrcaRouter max output tokens must not be negative")
	}

	name, err := providerprofile.Name(namePrefix, config.ProfileID)
	if err != nil {
		return nil, fmt.Errorf("configure OrcaRouter profile: %w", err)
	}
	baseURL := config.BaseURL
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultBaseURL
	}
	transport := &transportClient{base: config.HTTPClient}
	inner, err := openai.New(openai.Config{
		ProfileID:         config.ProfileID,
		API:               api,
		APIKey:            config.APIKey,
		CredentialSource:  config.CredentialSource,
		BaseURL:           baseURL,
		Model:             config.Model,
		MaxOutputTokens:   config.MaxOutputTokens,
		HTTPClient:        transport,
		CacheCapabilities: config.CacheCapabilities,
	})
	if err != nil {
		return nil, fmt.Errorf("configure OrcaRouter OpenAI-compatible transport: %w", err)
	}
	return &Provider{
		inner:                       inner,
		name:                        name,
		cacheCapabilitiesConfigured: config.CacheCapabilities != nil,
	}, nil
}

// Name returns the OrcaRouter API dialect used in diagnostics
func (provider *Provider) Name() string {
	if provider == nil {
		return ""
	}
	return provider.name
}

// ModelID returns the exact router or model identifier configured for this Provider
func (provider *Provider) ModelID() string {
	if provider == nil || provider.inner == nil {
		return ""
	}
	return provider.inner.ModelID()
}

// CacheCapabilities reports trusted prompt cache behavior for the configured routed model
func (provider *Provider) CacheCapabilities() agent.CacheCapabilities {
	if provider == nil || provider.inner == nil {
		return agent.CacheCapabilities{}
	}
	if !provider.cacheCapabilitiesConfigured {
		return agent.CacheCapabilities{}
	}
	return provider.inner.CacheCapabilities()
}

// Complete sends one model request and returns its completed Message
func (provider *Provider) Complete(ctx context.Context, request agent.ModelRequest) (agent.Message, error) {
	if ctx == nil {
		return agent.Message{}, errors.New("OrcaRouter context must not be nil")
	}
	if provider == nil || provider.inner == nil {
		return agent.Message{}, errors.New("OrcaRouter Provider must not be nil")
	}
	observation := &responseObservation{}
	request = provider.prepareRequest(request)
	ctx = context.WithValue(ctx, transportContextKey{}, transportContext{
		affinityID:  affinityID(provider.name, provider.inner.ModelID(), request),
		observation: observation,
	})
	message, err := provider.inner.Complete(ctx, request)
	if err != nil {
		return agent.Message{}, err
	}
	return provider.decorateMessage(message, observation), nil
}

// Stream sends one model request and exposes its result through the Provider stream contract
func (provider *Provider) Stream(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	if ctx == nil {
		return nil, errors.New("OrcaRouter context must not be nil")
	}
	if provider == nil || provider.inner == nil {
		return nil, errors.New("OrcaRouter Provider must not be nil")
	}
	observation := &responseObservation{}
	request = provider.prepareRequest(request)
	ctx = context.WithValue(ctx, transportContextKey{}, transportContext{
		affinityID:  affinityID(provider.name, provider.inner.ModelID(), request),
		observation: observation,
	})
	stream, err := provider.inner.Stream(ctx, request)
	if err != nil {
		return nil, err
	}
	return &modelStream{provider: provider, inner: stream, observation: observation}, nil
}

func (provider *Provider) prepareRequest(request agent.ModelRequest) agent.ModelRequest {
	for index := range request.Messages {
		state := request.Messages[index].ProviderState
		if state == nil || state.Provider != provider.name {
			continue
		}
		messages := append([]agent.Message(nil), request.Messages...)
		request.Messages = messages
		for stateIndex := index; stateIndex < len(request.Messages); stateIndex++ {
			state = request.Messages[stateIndex].ProviderState
			if state == nil || state.Provider != provider.name {
				continue
			}
			cloned := *state
			cloned.Provider = provider.inner.Name()
			cloned.Data = append([]byte(nil), state.Data...)
			request.Messages[stateIndex].ProviderState = &cloned
		}
		break
	}
	return request
}

func (provider *Provider) decorateMessage(message agent.Message, observation *responseObservation) agent.Message {
	if message.ProviderState != nil && message.ProviderState.Provider == provider.inner.Name() {
		state := *message.ProviderState
		state.Provider = provider.name
		state.Data = append([]byte(nil), message.ProviderState.Data...)
		message.ProviderState = &state
	}
	if observation.requestID != "" {
		message.RequestID = observation.requestID
	}
	if observation.resolvedModel != "" {
		message.Model = observation.resolvedModel
	}
	return message
}

type modelStream struct {
	provider    *Provider
	inner       agent.ModelStream
	observation *responseObservation
}

func (stream *modelStream) Next() (agent.ModelStreamEvent, error) {
	if stream == nil || stream.inner == nil {
		return agent.ModelStreamEvent{}, io.EOF
	}
	event, err := stream.inner.Next()
	if err != nil || event.Type != agent.ModelStreamMessageComplete || event.Message == nil {
		return event, err
	}
	message := stream.provider.decorateMessage(*event.Message, stream.observation)
	event.Message = &message
	return event, nil
}

func (stream *modelStream) Close() error {
	if stream == nil || stream.inner == nil {
		return nil
	}
	return stream.inner.Close()
}

type transportContextKey struct{}

type transportContext struct {
	affinityID  string
	observation *responseObservation
}

type responseObservation struct {
	requestID     string
	resolvedModel string
}

type transportClient struct {
	base providerbase.HTTPClient
}

func (client *transportClient) Do(request *http.Request) (*http.Response, error) {
	base := client.base
	if base == nil {
		base = http.DefaultClient
	}
	cloned := request.Clone(request.Context())
	cloned.Header.Set("X-OrcaRouter-Include-Cost", "true")
	transport, _ := request.Context().Value(transportContextKey{}).(transportContext)
	if transport.affinityID != "" {
		cloned.Header.Set("X-OrcaRouter-Session-Id", transport.affinityID)
	}
	response, err := base.Do(cloned)
	if response == nil {
		return nil, err
	}
	requestID := safeHeaderValue(response.Header.Get("X-Orca-Request-Id"))
	if transport.observation != nil {
		transport.observation.requestID = requestID
		transport.observation.resolvedModel = safeHeaderValue(response.Header.Get("X-Orca-Resolved-Model"))
	}
	if requestID != "" && response.Header.Get("X-Request-Id") == "" {
		copied := new(http.Response)
		*copied = *response
		copied.Header = response.Header.Clone()
		copied.Header.Set("X-Request-Id", requestID)
		response = copied
	}
	return response, err
}

func affinityID(providerName, model string, request agent.ModelRequest) string {
	scopeKind := "session"
	scopeID := request.SessionID
	if scopeID == "" {
		scopeKind = "run"
		scopeID = request.RunID
	}
	if scopeID == "" {
		return ""
	}
	hash := sha256.New()
	for _, value := range []string{
		affinityHashDomain,
		providerName,
		model,
		request.AgentID,
		scopeKind,
		scopeID,
	} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	return "qed-" + hex.EncodeToString(hash.Sum(nil))
}

func safeHeaderValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maximumHeaderBytes || !utf8.ValidString(value) {
		return ""
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return ""
		}
	}
	return value
}
