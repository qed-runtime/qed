package anthropic

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
)

func TestCacheCapabilitiesOverrideIsIsolated(t *testing.T) {
	t.Parallel()

	configured := agent.CacheCapabilities{
		ExactPrefix:         true,
		SupportsAutomatic:   true,
		MinimumPrefixTokens: 512,
		SupportedTTLs:       []agent.CacheTTL{agent.CacheTTLFiveMinutes},
		ExposesWriteTokens:  true,
	}
	provider, err := New(Config{
		BaseURL:           "https://example.invalid/v1",
		Model:             "custom-model",
		CacheCapabilities: &configured,
	})
	if err != nil {
		t.Fatal(err)
	}

	configured.SupportedTTLs[0] = agent.CacheTTLOneHour
	want := agent.CacheCapabilities{
		ExactPrefix:         true,
		SupportsAutomatic:   true,
		MinimumPrefixTokens: 512,
		SupportedTTLs:       []agent.CacheTTL{agent.CacheTTLFiveMinutes},
		ExposesWriteTokens:  true,
	}
	got := provider.CacheCapabilities()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CacheCapabilities() = %#v, want %#v", got, want)
	}

	got.SupportedTTLs[0] = agent.CacheTTLOneHour
	if again := provider.CacheCapabilities(); !reflect.DeepEqual(again, want) {
		t.Fatalf("CacheCapabilities() after caller mutation = %#v, want %#v", again, want)
	}
}

func TestUsageRejectsOverflow(t *testing.T) {
	t.Parallel()

	if _, err := usage(math.MaxInt64, 0, 1, 0); err == nil {
		t.Fatal("overflowing Anthropic Usage was accepted")
	}
}

func TestMessagesRequestRendersExplicitCacheControl(t *testing.T) {
	t.Parallel()

	provider := &Provider{model: "claude-test", maxOutputTokens: 1024, official: true}
	payload, err := provider.messagesRequest(agent.ModelRequest{
		Messages: []agent.Message{{Role: agent.RoleUser, Text: strings.Repeat("stable", 1000)}},
		CachePlan: &agent.CachePlan{
			Version:  1,
			FamilyID: "cache_" + strings.Repeat("a", 64),
			Mode:     agent.CacheModeExplicit,
			TTL:      agent.CacheTTLOneHour,
			Breakpoints: []agent.CacheBreakpoint{{
				MessageIndex: 0,
				Write:        true,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if payload.CacheControl != nil || len(payload.Messages) != 1 || len(payload.Messages[0].Content) != 1 {
		t.Fatalf("Messages payload = %#v", payload)
	}
	var block struct {
		CacheControl *struct {
			Type string         `json:"type"`
			TTL  agent.CacheTTL `json:"ttl"`
		} `json:"cache_control"`
	}
	if err := json.Unmarshal(payload.Messages[0].Content[0], &block); err != nil {
		t.Fatal(err)
	}
	if block.CacheControl == nil || block.CacheControl.Type != "ephemeral" ||
		block.CacheControl.TTL != agent.CacheTTLOneHour {
		t.Fatalf("explicit content block = %s", payload.Messages[0].Content[0])
	}
}

func TestMessagesRequestRendersAutomaticCacheControl(t *testing.T) {
	t.Parallel()

	provider := &Provider{model: "claude-test", maxOutputTokens: 1024, official: true}
	payload, err := provider.messagesRequest(agent.ModelRequest{
		Messages: []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
		CachePlan: &agent.CachePlan{
			Version:  1,
			FamilyID: "cache_" + strings.Repeat("b", 64),
			Mode:     agent.CacheModeAutomatic,
			TTL:      agent.CacheTTLFiveMinutes,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if payload.CacheControl == nil || payload.CacheControl.Type != "ephemeral" ||
		payload.CacheControl.TTL != agent.CacheTTLFiveMinutes {
		t.Fatalf("automatic Cache Control = %#v", payload.CacheControl)
	}
}

func TestCacheCapabilitiesUseEndpointAndModelMinimum(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider Provider
		minimum  int64
		explicit bool
	}{
		{name: "sonnet", provider: Provider{model: "claude-sonnet-4-6", official: true}, minimum: 1024, explicit: true},
		{name: "opus", provider: Provider{model: "claude-opus-4-6", official: true}, minimum: 4096, explicit: true},
		{name: "unknown", provider: Provider{model: "claude-future", official: true}, minimum: 4096, explicit: true},
		{name: "custom endpoint", provider: Provider{model: "claude-sonnet-4-6"}, minimum: 0, explicit: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			capabilities := test.provider.CacheCapabilities()
			if capabilities.MinimumPrefixTokens != test.minimum || capabilities.SupportsExplicit != test.explicit {
				t.Fatalf("Cache Capabilities = %#v", capabilities)
			}
		})
	}
}
