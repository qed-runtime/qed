// Package echo provides a deterministic Provider for local smoke tests
package echo

import (
	"context"
	"errors"

	"github.com/qed-runtime/qed/agent"
	providerprofile "github.com/qed-runtime/qed/provider/internal/profile"
)

// Provider returns the most recent user message without contacting a model
type Provider struct {
	name string
}

// New constructs an echo Provider
func New() *Provider {
	return &Provider{name: "echo"}
}

// NewProfile constructs an echo Provider with a diagnostic profile ID
func NewProfile(profileID string) (*Provider, error) {
	name, err := providerprofile.Name("echo", profileID)
	if err != nil {
		return nil, err
	}
	return &Provider{name: name}, nil
}

// Name returns the Provider identifier used in diagnostics
func (provider *Provider) Name() string {
	if provider.name == "" {
		return "echo"
	}
	return provider.name
}

// CacheCapabilities reports that the local Echo Provider has no prompt cache
func (provider *Provider) CacheCapabilities() agent.CacheCapabilities {
	return agent.CacheCapabilities{}
}

// Complete returns the most recent user message as an assistant message
func (provider *Provider) Complete(ctx context.Context, request agent.ModelRequest) (agent.Message, error) {
	if err := ctx.Err(); err != nil {
		return agent.Message{}, err
	}
	if request.CachePlan != nil && request.CachePlan.Mode != agent.CacheModeDisabled {
		return agent.Message{}, errors.New("echo Provider does not support prompt caching")
	}

	for index := len(request.Messages) - 1; index >= 0; index-- {
		message := request.Messages[index]
		if message.Role == agent.RoleUser {
			return agent.Message{Role: agent.RoleAssistant, Text: message.Text}, nil
		}
	}

	return agent.Message{}, errors.New("echo provider requires a user message")
}

// Stream returns the echoed Message as a finite Provider stream
func (provider *Provider) Stream(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	message, err := provider.Complete(ctx, request)
	if err != nil {
		return nil, err
	}
	return agent.MessageStream(message), nil
}
