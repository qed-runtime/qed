package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
)

const (
	cachePlanVersion       = 1
	cacheFamilyHashDomain  = "qed.cache.family.v1"
	cacheTokenEstimateKind = "canonical_bytes_div_4"
)

// CacheMode selects how a Provider prompt cache is used
type CacheMode string

// Provider prompt cache modes
const (
	CacheModeDisabled  CacheMode = "disabled"
	CacheModeAdaptive  CacheMode = "adaptive"
	CacheModeAutomatic CacheMode = "automatic"
	CacheModeExplicit  CacheMode = "explicit"
)

// CacheTTL identifies a Provider-supported prompt cache lifetime
type CacheTTL string

// Common Provider prompt cache lifetimes
const (
	CacheTTLFiveMinutes     CacheTTL = "5m"
	CacheTTLThirtyMinutes   CacheTTL = "30m"
	CacheTTLOneHour         CacheTTL = "1h"
	CacheTTLTwentyFourHours CacheTTL = "24h"
)

// CacheCapabilities describes prompt cache behavior exposed by one Provider adapter
//
// Values describe the configured API dialect and model, not every product
// offered by the Provider.
type CacheCapabilities struct {
	// ExactPrefix reports whether cache reuse requires an exact prompt prefix
	ExactPrefix bool `json:"exact_prefix"`
	// SupportsCacheKey reports whether the adapter can send a routing key
	SupportsCacheKey bool `json:"supports_cache_key"`
	// SupportsExplicit reports whether content breakpoints can be sent
	SupportsExplicit bool `json:"supports_explicit"`
	// SupportsAutomatic reports whether the Provider performs implicit or automatic caching
	SupportsAutomatic bool `json:"supports_automatic"`
	// MaxWriteBreakpoints bounds explicit write markers in one request
	MaxWriteBreakpoints int `json:"max_write_breakpoints,omitempty"`
	// MinimumPrefixTokens is the Provider minimum eligible prefix size
	MinimumPrefixTokens int64 `json:"minimum_prefix_tokens,omitempty"`
	// SupportedTTLs lists accepted explicit cache lifetimes
	SupportedTTLs []CacheTTL `json:"supported_ttls,omitempty"`
	// SupportsMixedTTL reports whether one request may combine different lifetimes
	SupportsMixedTTL bool `json:"supports_mixed_ttl,omitempty"`
	// ExposesReadTokens reports whether Usage identifies cache reads
	ExposesReadTokens bool `json:"exposes_read_tokens,omitempty"`
	// ExposesWriteTokens reports whether Usage identifies cache writes
	ExposesWriteTokens bool `json:"exposes_write_tokens,omitempty"`
}

// CacheCapabilityProvider optionally exposes configured prompt cache behavior
type CacheCapabilityProvider interface {
	CacheCapabilities() CacheCapabilities
}

// ValidateCacheCapabilities checks one configured Provider cache declaration
func ValidateCacheCapabilities(capabilities CacheCapabilities) error {
	_, err := validateCacheCapabilities(capabilities)
	return err
}

// CachePricing supplies host-selected rates in millionths of Currency per one million tokens
//
// Pricing is intentionally injected by the host because Provider rates change
// independently from QED releases.
type CachePricing struct {
	// Currency is an ISO-style currency label such as USD
	Currency string `json:"currency"`
	// UncachedInputMicrosPerMillion is the normal input rate
	UncachedInputMicrosPerMillion int64 `json:"uncached_input_micros_per_million"`
	// CacheReadMicrosPerMillion is the cached input read rate
	CacheReadMicrosPerMillion int64 `json:"cache_read_micros_per_million"`
	// CacheWriteMicrosPerMillion is the cache creation rate
	CacheWriteMicrosPerMillion int64 `json:"cache_write_micros_per_million"`
	// OutputMicrosPerMillion is the generated output rate
	OutputMicrosPerMillion int64 `json:"output_micros_per_million,omitempty"`
}

// CachePolicy supplies host intent to the provider-neutral Cache Planner
type CachePolicy struct {
	// Mode requests a specific mode; empty disables QED cache controls
	//
	// Adaptive selects the best supported mode with safe fallback.
	Mode CacheMode
	// TTL requests one Provider-supported lifetime
	TTL CacheTTL
	// ExpectedReuse is the expected total number of uses of the planned prefix
	ExpectedReuse int
	// Required turns unsupported cache behavior into an error instead of a fallback
	Required bool
	// IsolationKey separates tenants or other security domains and is never persisted directly
	IsolationKey string
	// Family identifies an optional host-defined sharing scope and is never persisted directly
	Family string
	// Pricing enables admission and cost forecasting without hard-coded model prices
	Pricing *CachePricing
}

// CacheBreakpoint identifies one Provider-neutral explicit cache boundary
type CacheBreakpoint struct {
	// AfterSegmentID identifies the final logical Segment in the cached prefix
	AfterSegmentID string `json:"after_segment_id"`
	// MessageIndex identifies the corresponding compiled ModelRequest message
	MessageIndex int `json:"message_index"`
	// TTL is the selected Provider cache lifetime
	TTL CacheTTL `json:"ttl,omitempty"`
	// Write requests cache creation at this boundary
	Write bool `json:"write"`
	// Reason explains why the Planner selected this boundary
	Reason string `json:"reason"`
	// PrefixTokenEstimate is the deterministic approximate prefix size
	PrefixTokenEstimate int64 `json:"prefix_token_estimate"`
}

// CostForecast estimates input cost across the expected uses of one prefix
type CostForecast struct {
	// Currency is copied from configured pricing
	Currency string `json:"currency"`
	// ExpectedUses is the number of calls included in this forecast
	ExpectedUses int `json:"expected_uses"`
	// PrefixTokenEstimate is the approximate cached prefix size
	PrefixTokenEstimate int64 `json:"prefix_token_estimate"`
	// WithoutCacheMicros is the estimated normal input cost
	WithoutCacheMicros int64 `json:"without_cache_micros"`
	// WithCacheMicros is one write plus subsequent read cost
	WithCacheMicros int64 `json:"with_cache_micros"`
	// SavingsMicros is WithoutCacheMicros minus WithCacheMicros and may be negative
	SavingsMicros int64 `json:"savings_micros"`
}

// CachePlan is the provider-neutral cache routing and breakpoint decision for one request
type CachePlan struct {
	// Version identifies the Cache Plan schema
	Version uint32 `json:"version"`
	// FamilyID is a domain-separated digest of Provider and host isolation inputs
	FamilyID string `json:"family_id,omitempty"`
	// Mode is the effective mode after capability fallback and admission
	Mode CacheMode `json:"mode"`
	// TTL is the selected request lifetime when applicable
	TTL CacheTTL `json:"ttl,omitempty"`
	// Breakpoints contains explicit write boundaries in prefix order
	Breakpoints []CacheBreakpoint `json:"breakpoints,omitempty"`
	// ExpectedReuse is copied from the normalized policy
	ExpectedReuse int `json:"expected_reuse"`
	// InputTokenEstimate is the approximate complete logical input size
	InputTokenEstimate int64 `json:"input_token_estimate"`
	// TokenEstimateKind identifies the deterministic estimator
	TokenEstimateKind string `json:"token_estimate_kind"`
	// FallbackReason explains why the requested mode was not used
	FallbackReason string `json:"fallback_reason,omitempty"`
	// Pricing contains optional host-injected rates used by Forecast and status tools
	Pricing *CachePricing `json:"pricing,omitempty"`
	// Forecast contains an admission forecast when complete cache pricing is available
	Forecast *CostForecast `json:"forecast,omitempty"`
}

// CachePlanRequest supplies one compiled logical request to a Cache Planner
type CachePlanRequest struct {
	// RunID identifies the current Run without becoming a raw Provider cache key
	RunID string
	// Provider identifies the exact configured Provider instance
	Provider string
	// Model identifies the configured model when available
	Model string
	// ModelRequest is the canonical compiled request
	ModelRequest ModelRequest
	// Segments are the ordered Context Segments for ModelRequest
	Segments []ContextSegment
	// Capabilities describe the configured Provider adapter
	Capabilities CacheCapabilities
	// Policy supplies host cache intent and isolation
	Policy CachePolicy
}

// CachePlanner creates one provider-neutral Cache Plan
type CachePlanner interface {
	Plan(ctx context.Context, request CachePlanRequest) (CachePlan, error)
}

// DefaultCachePlanner selects safe routing, one longest eligible breakpoint, and optional admission
//
// Token estimates use canonical logical bytes divided by four. Provider Usage
// remains authoritative after a request completes.
type DefaultCachePlanner struct{}

// Plan creates a deterministic Cache Plan from configured capabilities and policy
func (DefaultCachePlanner) Plan(ctx context.Context, request CachePlanRequest) (CachePlan, error) {
	if ctx == nil {
		return CachePlan{}, errors.New("Cache Planner context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return CachePlan{}, err
	}
	if strings.TrimSpace(request.Provider) != request.Provider || request.Provider == "" {
		return CachePlan{}, errors.New("Cache Planner Provider is required and must not have surrounding whitespace")
	}
	if request.ModelRequest.SessionID == "" && strings.TrimSpace(request.RunID) == "" {
		return CachePlan{}, errors.New("Cache Planner requires a Run ID when no Session ID is present")
	}
	capabilities, err := validateCacheCapabilities(request.Capabilities)
	if err != nil {
		return CachePlan{}, err
	}
	policy, err := normalizeCachePolicy(request.Policy)
	if err != nil {
		return CachePlan{}, err
	}
	plan := CachePlan{
		Version:            cachePlanVersion,
		Mode:               policy.Mode,
		ExpectedReuse:      policy.ExpectedReuse,
		InputTokenEstimate: estimateSegments(request.Segments),
		TokenEstimateKind:  cacheTokenEstimateKind,
		Pricing:            cloneCachePricing(policy.Pricing),
	}
	if plan.Mode == "" {
		plan.Mode = CacheModeDisabled
	}
	if plan.Mode == CacheModeAdaptive {
		switch {
		case capabilities.SupportsExplicit:
			plan.Mode = CacheModeExplicit
		case capabilities.SupportsAutomatic:
			plan.Mode = CacheModeAutomatic
		default:
			plan.Mode = CacheModeDisabled
		}
		if plan.Mode == CacheModeDisabled && policy.Required {
			return CachePlan{}, errors.New("Provider caching is required but unsupported")
		}
	}
	if plan.Mode == CacheModeExplicit && !capabilities.SupportsExplicit {
		if policy.Required {
			return CachePlan{}, errors.New("explicit Provider cache is required but unsupported")
		}
		plan.FallbackReason = "explicit_cache_unsupported"
		if capabilities.SupportsAutomatic {
			plan.Mode = CacheModeAutomatic
		} else {
			plan.Mode = CacheModeDisabled
		}
	}
	if plan.Mode == CacheModeAutomatic && !capabilities.SupportsAutomatic {
		if policy.Required {
			return CachePlan{}, errors.New("automatic Provider cache is required but unsupported")
		}
		plan.FallbackReason = "automatic_cache_unsupported"
		plan.Mode = CacheModeDisabled
	}
	if plan.Mode == CacheModeDisabled {
		return plan, nil
	}

	plan.FamilyID = cacheFamilyID(request, policy)
	selectedTTL, ttlFallback, err := selectCacheTTL(policy, capabilities)
	if err != nil {
		return CachePlan{}, err
	}
	plan.TTL = selectedTTL
	if ttlFallback != "" {
		plan.FallbackReason = ttlFallback
	}
	if plan.Mode == CacheModeExplicit {
		breakpoint, ok := selectCacheBreakpoint(request, capabilities, selectedTTL)
		if !ok {
			if policy.Required {
				return CachePlan{}, errors.New("explicit Provider cache has no eligible message breakpoint")
			}
			plan.FallbackReason = "no_eligible_cache_breakpoint"
			if capabilities.SupportsAutomatic {
				plan.Mode = CacheModeAutomatic
			} else {
				plan.Mode = CacheModeDisabled
				plan.FamilyID = ""
			}
		} else {
			plan.Breakpoints = []CacheBreakpoint{breakpoint}
		}
	}

	forecastTokens := plan.InputTokenEstimate
	if len(plan.Breakpoints) > 0 {
		forecastTokens = plan.Breakpoints[len(plan.Breakpoints)-1].PrefixTokenEstimate
	}
	if pricingComplete(plan.Pricing) {
		forecast, err := ForecastCacheCost(*plan.Pricing, forecastTokens, plan.ExpectedReuse)
		if err != nil {
			return CachePlan{}, err
		}
		plan.Forecast = &forecast
		if plan.Mode == CacheModeExplicit && forecast.SavingsMicros <= 0 && !policy.Required {
			plan.Breakpoints = nil
			plan.FallbackReason = "explicit_cache_not_economical"
			if capabilities.SupportsAutomatic {
				plan.Mode = CacheModeAutomatic
			} else {
				plan.Mode = CacheModeDisabled
				plan.FamilyID = ""
			}
		}
	}
	if plan.Mode == CacheModeDisabled {
		plan.FamilyID = ""
		plan.TTL = ""
		plan.Breakpoints = nil
		plan.Forecast = nil
	}
	return plan, nil
}

// ForecastCacheCost estimates normal input versus one cache write and subsequent reads
func ForecastCacheCost(pricing CachePricing, prefixTokens int64, expectedUses int) (CostForecast, error) {
	if err := validateCachePricing(pricing); err != nil {
		return CostForecast{}, err
	}
	if !pricingComplete(&pricing) {
		return CostForecast{}, errors.New("cache read and write prices are required for a forecast")
	}
	if prefixTokens < 0 {
		return CostForecast{}, errors.New("forecast prefix tokens must not be negative")
	}
	if expectedUses < 1 {
		return CostForecast{}, errors.New("forecast expected uses must be positive")
	}
	uncached, err := scaledTokenCost(prefixTokens, pricing.UncachedInputMicrosPerMillion)
	if err != nil {
		return CostForecast{}, err
	}
	write, err := scaledTokenCost(prefixTokens, pricing.CacheWriteMicrosPerMillion)
	if err != nil {
		return CostForecast{}, err
	}
	read, err := scaledTokenCost(prefixTokens, pricing.CacheReadMicrosPerMillion)
	if err != nil {
		return CostForecast{}, err
	}
	without, err := checkedMultiply(uncached, int64(expectedUses))
	if err != nil {
		return CostForecast{}, err
	}
	reads, err := checkedMultiply(read, int64(expectedUses-1))
	if err != nil {
		return CostForecast{}, err
	}
	withCache, err := checkedAdd(write, reads)
	if err != nil {
		return CostForecast{}, err
	}
	return CostForecast{
		Currency:            pricing.Currency,
		ExpectedUses:        expectedUses,
		PrefixTokenEstimate: prefixTokens,
		WithoutCacheMicros:  without,
		WithCacheMicros:     withCache,
		SavingsMicros:       without - withCache,
	}, nil
}

// UsageCost is a pricing-derived cost estimate for reported Provider Usage
type UsageCost struct {
	// Currency is copied from configured pricing
	Currency string `json:"currency"`
	// InputMicros is the estimated input cost
	InputMicros int64 `json:"input_micros"`
	// OutputMicros is the estimated output cost
	OutputMicros int64 `json:"output_micros"`
	// TotalMicros is InputMicros plus OutputMicros
	TotalMicros int64 `json:"total_micros"`
	// CacheDetailsReported indicates whether cache categories were used
	CacheDetailsReported bool `json:"cache_details_reported"`
}

// EstimateUsageCost applies injected pricing to Provider-reported token counts
func EstimateUsageCost(pricing CachePricing, usage Usage) (UsageCost, error) {
	if err := validateCachePricing(pricing); err != nil {
		return UsageCost{}, err
	}
	if err := validateUsage(&usage); err != nil {
		return UsageCost{}, err
	}
	uncachedTokens := usage.InputTokens
	readTokens := int64(0)
	writeTokens := int64(0)
	if usage.InputTokenDetailsReported {
		uncachedTokens = usage.UncachedInputTokens
		readTokens = usage.CacheReadInputTokens
		writeTokens = usage.CacheWriteInputTokens
	}
	uncached, err := scaledTokenCost(uncachedTokens, pricing.UncachedInputMicrosPerMillion)
	if err != nil {
		return UsageCost{}, err
	}
	read, err := scaledTokenCost(readTokens, pricing.CacheReadMicrosPerMillion)
	if err != nil {
		return UsageCost{}, err
	}
	write, err := scaledTokenCost(writeTokens, pricing.CacheWriteMicrosPerMillion)
	if err != nil {
		return UsageCost{}, err
	}
	input, err := checkedAdd(uncached, read)
	if err != nil {
		return UsageCost{}, err
	}
	input, err = checkedAdd(input, write)
	if err != nil {
		return UsageCost{}, err
	}
	output, err := scaledTokenCost(usage.OutputTokens, pricing.OutputMicrosPerMillion)
	if err != nil {
		return UsageCost{}, err
	}
	total, err := checkedAdd(input, output)
	if err != nil {
		return UsageCost{}, err
	}
	return UsageCost{
		Currency:             pricing.Currency,
		InputMicros:          input,
		OutputMicros:         output,
		TotalMicros:          total,
		CacheDetailsReported: usage.InputTokenDetailsReported,
	}, nil
}

func validateCacheCapabilities(capabilities CacheCapabilities) (CacheCapabilities, error) {
	if capabilities.MaxWriteBreakpoints < 0 || capabilities.MinimumPrefixTokens < 0 {
		return CacheCapabilities{}, errors.New("Cache Capability limits must not be negative")
	}
	if capabilities.SupportsExplicit && capabilities.MaxWriteBreakpoints == 0 {
		return CacheCapabilities{}, errors.New("explicit Cache Capability requires at least one breakpoint")
	}
	seen := make(map[CacheTTL]struct{}, len(capabilities.SupportedTTLs))
	for _, ttl := range capabilities.SupportedTTLs {
		if strings.TrimSpace(string(ttl)) != string(ttl) || ttl == "" {
			return CacheCapabilities{}, errors.New("Cache Capability TTL is invalid")
		}
		if _, duplicate := seen[ttl]; duplicate {
			return CacheCapabilities{}, fmt.Errorf("Cache Capability TTL %q is duplicated", ttl)
		}
		seen[ttl] = struct{}{}
	}
	capabilities.SupportedTTLs = append([]CacheTTL(nil), capabilities.SupportedTTLs...)
	return capabilities, nil
}

func validateCachePlan(
	plan CachePlan,
	capabilities CacheCapabilities,
	request ModelRequest,
	segments []ContextSegment,
) error {
	validatedCapabilities, err := validateCacheCapabilities(capabilities)
	if err != nil {
		return err
	}
	if plan.Version != cachePlanVersion {
		return fmt.Errorf("Cache Plan version = %d, want %d", plan.Version, cachePlanVersion)
	}
	if plan.ExpectedReuse < 1 || plan.InputTokenEstimate < 0 || plan.TokenEstimateKind == "" {
		return errors.New("Cache Plan estimates are invalid")
	}
	switch plan.Mode {
	case CacheModeDisabled:
		if plan.FamilyID != "" || plan.TTL != "" || len(plan.Breakpoints) != 0 || plan.Forecast != nil {
			return errors.New("disabled Cache Plan must not contain a family, TTL, breakpoints, or forecast")
		}
	case CacheModeAutomatic:
		if !validatedCapabilities.SupportsAutomatic {
			return errors.New("automatic Cache Plan is unsupported by the Provider")
		}
		if len(plan.Breakpoints) != 0 {
			return errors.New("automatic Cache Plan must not contain explicit breakpoints")
		}
	case CacheModeExplicit:
		if !validatedCapabilities.SupportsExplicit {
			return errors.New("explicit Cache Plan is unsupported by the Provider")
		}
		if len(plan.Breakpoints) == 0 || len(plan.Breakpoints) > validatedCapabilities.MaxWriteBreakpoints {
			return errors.New("explicit Cache Plan has an invalid breakpoint count")
		}
	default:
		return fmt.Errorf("Cache Plan mode %q is invalid", plan.Mode)
	}
	if plan.Mode != CacheModeDisabled {
		if !strings.HasPrefix(plan.FamilyID, "cache_") || len(plan.FamilyID) != len("cache_")+sha256.Size*2 {
			return errors.New("Cache Plan family is not a QED cache digest")
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(plan.FamilyID, "cache_")); err != nil {
			return errors.New("Cache Plan family is not a QED cache digest")
		}
	}
	if plan.InputTokenEstimate != estimateSegments(segments) || plan.TokenEstimateKind != cacheTokenEstimateKind {
		return errors.New("Cache Plan token estimate does not match the compiled Context Segments")
	}
	if plan.TTL != "" {
		supported := false
		for _, ttl := range validatedCapabilities.SupportedTTLs {
			if ttl == plan.TTL {
				supported = true
				break
			}
		}
		if !supported {
			return fmt.Errorf("Cache Plan TTL %q is unsupported by the Provider", plan.TTL)
		}
	}
	previousMessageIndex := -1
	for _, breakpoint := range plan.Breakpoints {
		if breakpoint.MessageIndex < 0 || breakpoint.MessageIndex >= len(request.Messages) ||
			breakpoint.MessageIndex+2 >= len(segments) {
			return errors.New("Cache breakpoint message index is outside the compiled request")
		}
		if breakpoint.MessageIndex <= previousMessageIndex {
			return errors.New("Cache breakpoints must be in strictly increasing message order")
		}
		segment := segments[breakpoint.MessageIndex+2]
		prefixEstimate := estimateSegments(segments[:breakpoint.MessageIndex+3])
		if breakpoint.AfterSegmentID != segment.ID || request.Messages[breakpoint.MessageIndex].Role != RoleUser ||
			!breakpoint.Write || strings.TrimSpace(breakpoint.Reason) != breakpoint.Reason || breakpoint.Reason == "" ||
			breakpoint.PrefixTokenEstimate != prefixEstimate ||
			breakpoint.PrefixTokenEstimate < validatedCapabilities.MinimumPrefixTokens {
			return errors.New("Cache breakpoint does not match an eligible compiled message")
		}
		if breakpoint.TTL != plan.TTL {
			return errors.New("Cache breakpoint TTL does not match the Cache Plan")
		}
		previousMessageIndex = breakpoint.MessageIndex
	}
	if plan.Pricing != nil {
		if err := validateCachePricing(*plan.Pricing); err != nil {
			return err
		}
	}
	if plan.Forecast != nil {
		if !pricingComplete(plan.Pricing) {
			return errors.New("Cache Plan forecast requires complete pricing")
		}
		forecastTokens := plan.InputTokenEstimate
		if len(plan.Breakpoints) > 0 {
			forecastTokens = plan.Breakpoints[len(plan.Breakpoints)-1].PrefixTokenEstimate
		}
		forecast, err := ForecastCacheCost(*plan.Pricing, forecastTokens, plan.ExpectedReuse)
		if err != nil {
			return err
		}
		if *plan.Forecast != forecast {
			return errors.New("Cache Plan forecast does not match its pricing and estimates")
		}
	}
	return nil
}

func normalizeCachePolicy(policy CachePolicy) (CachePolicy, error) {
	switch policy.Mode {
	case "", CacheModeDisabled, CacheModeAdaptive, CacheModeAutomatic, CacheModeExplicit:
	default:
		return CachePolicy{}, fmt.Errorf("unsupported Cache mode %q", policy.Mode)
	}
	if policy.ExpectedReuse == 0 {
		policy.ExpectedReuse = 2
	}
	if policy.ExpectedReuse < 1 {
		return CachePolicy{}, errors.New("Cache expected reuse must be positive")
	}
	if strings.TrimSpace(string(policy.TTL)) != string(policy.TTL) {
		return CachePolicy{}, errors.New("Cache TTL must not have surrounding whitespace")
	}
	if len(policy.IsolationKey) > 512 || len(policy.Family) > 512 {
		return CachePolicy{}, errors.New("Cache isolation key and family must not exceed 512 bytes")
	}
	if policy.Pricing != nil {
		if err := validateCachePricing(*policy.Pricing); err != nil {
			return CachePolicy{}, err
		}
		policy.Pricing = cloneCachePricing(policy.Pricing)
	}
	return policy, nil
}

func validateCachePricing(pricing CachePricing) error {
	if strings.TrimSpace(pricing.Currency) != pricing.Currency || pricing.Currency == "" {
		return errors.New("Cache pricing currency is required and must not have surrounding whitespace")
	}
	if pricing.UncachedInputMicrosPerMillion < 0 || pricing.CacheReadMicrosPerMillion < 0 ||
		pricing.CacheWriteMicrosPerMillion < 0 || pricing.OutputMicrosPerMillion < 0 {
		return errors.New("Cache pricing rates must not be negative")
	}
	return nil
}

func pricingComplete(pricing *CachePricing) bool {
	return pricing != nil && pricing.UncachedInputMicrosPerMillion > 0 &&
		pricing.CacheReadMicrosPerMillion > 0 && pricing.CacheWriteMicrosPerMillion > 0
}

func selectCacheTTL(policy CachePolicy, capabilities CacheCapabilities) (CacheTTL, string, error) {
	if policy.TTL == "" {
		if len(capabilities.SupportedTTLs) == 0 {
			return "", "", nil
		}
		return capabilities.SupportedTTLs[0], "", nil
	}
	for _, ttl := range capabilities.SupportedTTLs {
		if ttl == policy.TTL {
			return ttl, "", nil
		}
	}
	if policy.Required {
		return "", "", fmt.Errorf("Cache TTL %q is required but unsupported", policy.TTL)
	}
	if len(capabilities.SupportedTTLs) == 0 {
		return "", "cache_ttl_unsupported", nil
	}
	return capabilities.SupportedTTLs[0], "cache_ttl_fallback", nil
}

func selectCacheBreakpoint(
	request CachePlanRequest,
	capabilities CacheCapabilities,
	ttl CacheTTL,
) (CacheBreakpoint, bool) {
	if capabilities.MaxWriteBreakpoints < 1 || len(request.Segments) < 3 {
		return CacheBreakpoint{}, false
	}
	var cumulativeBytes int64
	for _, segment := range request.Segments[:2] {
		cumulativeBytes += segment.Bytes
	}
	var selected CacheBreakpoint
	found := false
	for index, message := range request.ModelRequest.Messages {
		segmentIndex := index + 2
		if segmentIndex >= len(request.Segments) {
			break
		}
		segment := request.Segments[segmentIndex]
		cumulativeBytes += segment.Bytes
		if message.Role != RoleUser || message.Text == "" ||
			(segment.Kind != SegmentKindMessage && segment.Kind != SegmentKindCheckpoint) {
			continue
		}
		estimate := estimateBytes(cumulativeBytes)
		if estimate < capabilities.MinimumPrefixTokens {
			continue
		}
		selected = CacheBreakpoint{
			AfterSegmentID:      segment.ID,
			MessageIndex:        index,
			TTL:                 ttl,
			Write:               true,
			Reason:              "longest_eligible_user_boundary",
			PrefixTokenEstimate: estimate,
		}
		found = true
	}
	return selected, found
}

func estimateSegments(segments []ContextSegment) int64 {
	var bytes int64
	for _, segment := range segments {
		if segment.Bytes > math.MaxInt64-bytes {
			return math.MaxInt64
		}
		bytes += segment.Bytes
	}
	return estimateBytes(bytes)
}

func estimateBytes(bytes int64) int64 {
	if bytes <= 0 {
		return 0
	}
	return 1 + (bytes-1)/4
}

func cacheFamilyID(request CachePlanRequest, policy CachePolicy) string {
	scope := request.ModelRequest.SessionID
	if scope == "" {
		scope = request.RunID
	}
	hash := sha256.New()
	writeHashPart(hash, []byte(cacheFamilyHashDomain))
	writeHashPart(hash, []byte(request.Provider))
	writeHashPart(hash, []byte(request.Model))
	writeHashPart(hash, []byte(request.ModelRequest.AgentID))
	writeHashPart(hash, []byte(scope))
	writeHashPart(hash, []byte(policy.Family))
	writeHashPart(hash, []byte(policy.IsolationKey))
	return "cache_" + hex.EncodeToString(hash.Sum(nil))
}

func cloneCachePricing(pricing *CachePricing) *CachePricing {
	if pricing == nil {
		return nil
	}
	cloned := *pricing
	return &cloned
}

func cloneCachePlanPointer(plan *CachePlan) *CachePlan {
	if plan == nil {
		return nil
	}
	cloned := *plan
	cloned.Breakpoints = append([]CacheBreakpoint(nil), plan.Breakpoints...)
	cloned.Pricing = cloneCachePricing(plan.Pricing)
	if plan.Forecast != nil {
		forecast := *plan.Forecast
		cloned.Forecast = &forecast
	}
	return &cloned
}

func scaledTokenCost(tokens, microsPerMillion int64) (int64, error) {
	if tokens < 0 || microsPerMillion < 0 {
		return 0, errors.New("token count and rate must not be negative")
	}
	const scale int64 = 1_000_000
	whole, err := checkedMultiply(tokens/scale, microsPerMillion)
	if err != nil {
		return 0, err
	}
	remainderProduct, err := checkedMultiply(tokens%scale, microsPerMillion)
	if err != nil {
		return 0, err
	}
	remainder := remainderProduct / scale
	if remainderProduct%scale != 0 {
		remainder++
	}
	return checkedAdd(whole, remainder)
}

func checkedMultiply(first, second int64) (int64, error) {
	if first < 0 || second < 0 {
		return 0, errors.New("cost operands must not be negative")
	}
	if first != 0 && second > math.MaxInt64/first {
		return 0, errors.New("cost calculation overflow")
	}
	return first * second, nil
}

func checkedAdd(first, second int64) (int64, error) {
	if first < 0 || second < 0 {
		return 0, errors.New("cost operands must not be negative")
	}
	if second > math.MaxInt64-first {
		return 0, errors.New("cost calculation overflow")
	}
	return first + second, nil
}
