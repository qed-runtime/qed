package openai

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
		SupportsExplicit:    true,
		MaxWriteBreakpoints: 2,
		MinimumPrefixTokens: 2048,
		SupportedTTLs:       []agent.CacheTTL{agent.CacheTTLOneHour},
		ExposesReadTokens:   true,
	}
	provider, err := New(Config{
		BaseURL:           "https://example.invalid/v1",
		Model:             "custom-model",
		CacheCapabilities: &configured,
	})
	if err != nil {
		t.Fatal(err)
	}

	configured.SupportedTTLs[0] = agent.CacheTTLFiveMinutes
	want := agent.CacheCapabilities{
		ExactPrefix:         true,
		SupportsExplicit:    true,
		MaxWriteBreakpoints: 2,
		MinimumPrefixTokens: 2048,
		SupportedTTLs:       []agent.CacheTTL{agent.CacheTTLOneHour},
		ExposesReadTokens:   true,
	}
	got := provider.CacheCapabilities()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CacheCapabilities() = %#v, want %#v", got, want)
	}

	got.SupportedTTLs[0] = agent.CacheTTLFiveMinutes
	if again := provider.CacheCapabilities(); !reflect.DeepEqual(again, want) {
		t.Fatalf("CacheCapabilities() after caller mutation = %#v, want %#v", again, want)
	}
}

func TestCacheCapabilitiesUseEndpointAndModelGeneration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider Provider
		explicit bool
	}{
		{name: "GPT 5.6", provider: Provider{model: "gpt-5.6-luna", official: true}, explicit: true},
		{name: "later major", provider: Provider{model: "gpt-6", official: true}, explicit: true},
		{name: "earlier model", provider: Provider{model: "gpt-5.5", official: true}, explicit: false},
		{name: "custom endpoint", provider: Provider{model: "gpt-5.6-luna"}, explicit: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			capabilities := test.provider.CacheCapabilities()
			if capabilities.SupportsExplicit != test.explicit {
				t.Fatalf("Cache Capabilities = %#v", capabilities)
			}
		})
	}
}

func TestUsageIgnoresOverflowingCacheDetails(t *testing.T) {
	t.Parallel()

	reported := usage(math.MaxInt64, 1, math.MaxInt64, &inputTokenDetails{
		CachedTokens:     math.MaxInt64,
		CacheWriteTokens: 1,
	}, "")
	if reported == nil || reported.InputTokenDetailsReported {
		t.Fatalf("Usage = %#v", reported)
	}
}

func TestUsageRoundsReportedUSDCostToMicros(t *testing.T) {
	t.Parallel()

	for value, want := range map[json.Number]int64{"0.0012345": 1235, "1e-6": 1} {
		reported := usage(1, 1, 2, nil, value)
		if reported == nil || reported.CostMicros != want {
			t.Errorf("Usage for cost %q = %#v, want %d cost micros", value, reported, want)
		}
	}
	for _, value := range []json.Number{
		"-1", "999999999999999999999999999999", "1e999999", json.Number(strings.Repeat("9", 65)),
	} {
		if got := usage(1, 1, 2, nil, value); got == nil || got.CostMicros != 0 {
			t.Errorf("Usage for cost %q = %#v, want zero cost micros", value, got)
		}
	}
}

func TestResponsesRequestRendersExplicitCachePlan(t *testing.T) {
	t.Parallel()

	provider := &Provider{api: APIResponses, model: "gpt-5.6-luna", official: true}
	payload, err := provider.responsesRequest(agent.ModelRequest{
		Messages: []agent.Message{{Role: agent.RoleUser, Text: strings.Repeat("stable", 1000)}},
		CachePlan: &agent.CachePlan{
			Version:  1,
			FamilyID: "cache_" + strings.Repeat("a", 64),
			Mode:     agent.CacheModeExplicit,
			TTL:      agent.CacheTTLThirtyMinutes,
			Breakpoints: []agent.CacheBreakpoint{{
				AfterSegmentID: "message/0000000000",
				MessageIndex:   0,
				Write:          true,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if payload.PromptCacheKey == "" || payload.PromptCacheOptions == nil ||
		payload.PromptCacheOptions.Mode != "explicit" || payload.PromptCacheOptions.TTL != agent.CacheTTLThirtyMinutes {
		t.Fatalf("Responses cache fields = %#v", payload)
	}
	if len(payload.Input) != 1 {
		t.Fatalf("Responses input = %#v", payload.Input)
	}
	var message struct {
		Content []struct {
			Type       string `json:"type"`
			Breakpoint *struct {
				Mode string `json:"mode"`
			} `json:"prompt_cache_breakpoint"`
		} `json:"content"`
	}
	if err := json.Unmarshal(payload.Input[0], &message); err != nil {
		t.Fatal(err)
	}
	if len(message.Content) != 1 || message.Content[0].Type != "input_text" ||
		message.Content[0].Breakpoint == nil || message.Content[0].Breakpoint.Mode != "explicit" {
		t.Fatalf("explicit input message = %s", payload.Input[0])
	}
}

func TestChatRequestRendersAutomaticCacheKey(t *testing.T) {
	t.Parallel()

	provider := &Provider{api: APIChatCompletions, model: "gpt-5", official: true}
	payload, err := provider.chatRequest(agent.ModelRequest{
		Messages: []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
		CachePlan: &agent.CachePlan{
			Version:  1,
			FamilyID: "cache_" + strings.Repeat("b", 64),
			Mode:     agent.CacheModeAutomatic,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if payload.PromptCacheKey == "" {
		t.Fatal("Chat Completions request omitted prompt_cache_key")
	}
}

func TestChatRequestRendersExplicitCachePlan(t *testing.T) {
	t.Parallel()

	provider := &Provider{api: APIChatCompletions, model: "gpt-5.6", official: true}
	payload, err := provider.chatRequest(agent.ModelRequest{
		Messages: []agent.Message{{Role: agent.RoleUser, Text: strings.Repeat("stable", 1000)}},
		CachePlan: &agent.CachePlan{
			Version:  1,
			FamilyID: "cache_" + strings.Repeat("c", 64),
			Mode:     agent.CacheModeExplicit,
			TTL:      agent.CacheTTLThirtyMinutes,
			Breakpoints: []agent.CacheBreakpoint{{
				MessageIndex: 0,
				Write:        true,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if payload.PromptCacheKey == "" || payload.PromptCacheOptions == nil ||
		payload.PromptCacheOptions.Mode != "explicit" || payload.PromptCacheOptions.TTL != agent.CacheTTLThirtyMinutes {
		t.Fatalf("Chat cache fields = %#v", payload)
	}
	encoded, err := json.Marshal(payload.Messages[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	var blocks []struct {
		Type       string `json:"type"`
		Breakpoint *struct {
			Mode string `json:"mode"`
		} `json:"prompt_cache_breakpoint"`
	}
	if err := json.Unmarshal(encoded, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Breakpoint == nil ||
		blocks[0].Breakpoint.Mode != "explicit" {
		t.Fatalf("Chat content blocks = %s", encoded)
	}
}
