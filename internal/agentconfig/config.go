// Package agentconfig loads declarative Provider profiles and Agent graphs
package agentconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/evidence"
	"github.com/qed-runtime/qed/extension"
	"github.com/qed-runtime/qed/extension/host"
	extensionmanifest "github.com/qed-runtime/qed/extension/manifest"
	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/extension/selfexec"
	"github.com/qed-runtime/qed/internal/chatauth"
	"github.com/qed-runtime/qed/internal/jsonstrict"
	"github.com/qed-runtime/qed/orchestration"
	"github.com/qed-runtime/qed/profile/coding"
	providerbase "github.com/qed-runtime/qed/provider"
	"github.com/qed-runtime/qed/provider/anthropic"
	"github.com/qed-runtime/qed/provider/echo"
	"github.com/qed-runtime/qed/provider/openai"
	"github.com/qed-runtime/qed/provider/openaicodex"
	sessionstore "github.com/qed-runtime/qed/session"
)

const (
	// CurrentVersion is the supported declarative configuration version
	CurrentVersion = 1

	maximumConfigurationBytes = 1 << 20

	protocolEcho            = "echo"
	protocolOpenAIResponses = "openai-responses"
	protocolOpenAIChat      = "openai-chat"
	protocolOpenAICodex     = "openai-codex"
	protocolAnthropic       = "anthropic"
)

// LookupEnv resolves one environment variable without exposing all process environment values
type LookupEnv func(string) (string, bool)

// LoadOptions supplies host-owned resources that must not be embedded in the
// declarative configuration
type LoadOptions struct {
	LookupEnv     LookupEnv
	WorkspaceRoot string
	// AuthStorePath overrides the OS-default QED credential file for
	// openai-codex profiles when non-empty
	AuthStorePath string
	// SelfExecutable is the absolute host executable used by self-exec Extensions
	SelfExecutable string
	// SelfExecCatalog contains the linked Extensions available from SelfExecutable
	SelfExecCatalog *selfexec.Catalog
	// Context bounds Extension startup and defaults to context.Background
	Context  context.Context
	Approver capability.Approver
	Recorder evidence.Recorder
	// ToolInputValidator compiles Tool schemas for Runtime and Extension boundaries
	ToolInputValidator agent.ToolInputValidator
	// ContextSemanticScorer optionally augments relevance-ordered Context search
	ContextSemanticScorer agent.ContextSemanticScorer
	// TokenEstimator overrides Provider and canonical fallback token estimation
	TokenEstimator agent.TokenEstimator
	// Verbose enables safe structured Runtime and Extension diagnostics
	Verbose bool
	// DebugWriter receives JSON diagnostics when Logger is nil
	DebugWriter io.Writer
	// Logger receives safe structured diagnostics when Verbose is true
	Logger *slog.Logger
}

// Configuration contains a built Agent registry and its optional default Agent
type Configuration struct {
	Registry      *orchestration.AgentRegistry
	DefaultAgent  string
	SessionStore  agent.SessionStore
	EvidenceStore evidence.Store
	// ExtensionStateStore contains host-owned opaque Extension state when configured
	ExtensionStateStore extension.StateStore
	agentIDs            map[string]struct{}
	recorder            evidence.Recorder
	memory              *evidence.MemoryRecorder
	profiles            []*coding.Profile
	configDigest        string
	workspaceID         string
	extensionIDs        []string
	closeMu             sync.Mutex
}

// ExtensionIDs returns configured and discovered Extension IDs in lexical order
func (configuration *Configuration) ExtensionIDs() []string {
	return append([]string(nil), configuration.extensionIDs...)
}

// ResolveAgent returns an explicit Agent ID or the configured default
func (configuration *Configuration) ResolveAgent(agentID string) (string, error) {
	if agentID == "" {
		agentID = configuration.DefaultAgent
	}
	if agentID == "" {
		return "", errors.New("agent ID is required because default_agent is not configured")
	}
	if err := validateID("agent ID", agentID); err != nil {
		return "", err
	}
	if _, ok := configuration.agentIDs[agentID]; !ok {
		return "", fmt.Errorf("agent %q is not configured", agentID)
	}
	return agentID, nil
}

// Recorder returns the host-owned Evidence recorder used by configured Profiles
func (configuration *Configuration) Recorder() evidence.Recorder {
	return configuration.recorder
}

// ToolInvocations returns recorded Evidence when Load created its own in-memory
// recorder, or nil when the caller supplied a custom Recorder
func (configuration *Configuration) ToolInvocations() []evidence.ToolInvocation {
	if configuration.memory == nil {
		return nil
	}
	return configuration.memory.ToolInvocations()
}

// SaveRunEvidence builds and persists one Bundle when an Evidence Store is configured
func (configuration *Configuration) SaveRunEvidence(
	ctx context.Context,
	result agent.RunResult,
	events []agent.Event,
) (evidence.Bundle, error) {
	if configuration.EvidenceStore == nil {
		return evidence.Bundle{}, errors.New("configuration has no Evidence Store")
	}
	invocations := configuration.ToolInvocations()
	filtered := make([]evidence.ToolInvocation, 0, len(invocations))
	for _, invocation := range invocations {
		if invocation.RunID == result.RunID {
			filtered = append(filtered, invocation)
		}
	}
	bundle, err := evidence.NewBundle(result, evidence.BundleOptions{
		Events:          events,
		ToolInvocations: filtered,
		ConfigDigest:    configuration.configDigest,
		WorkspaceID:     configuration.workspaceID,
	})
	if err != nil {
		return evidence.Bundle{}, err
	}
	if err := configuration.EvidenceStore.Save(ctx, bundle); err != nil {
		return evidence.Bundle{}, err
	}
	return bundle, nil
}

// CloseContext drains and closes all configured Extension processes
func (configuration *Configuration) CloseContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("configuration close context must not be nil")
	}
	configuration.closeMu.Lock()
	defer configuration.closeMu.Unlock()
	var closeErr error
	for _, profile := range configuration.profiles {
		closeErr = errors.Join(closeErr, profile.CloseContext(ctx))
	}
	return closeErr
}

// Close drains and closes all configured Extension processes
func (configuration *Configuration) Close() error {
	return configuration.CloseContext(context.Background())
}

// Load reads, validates, and builds one declarative Agent configuration
func Load(path string, options LoadOptions) (*Configuration, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("configuration path is required")
	}

	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve configuration path: %w", err)
	}
	data, err := readFile(absolutePath)
	if err != nil {
		return nil, err
	}
	var document fileConfig
	if err := jsonstrict.Decode(data, maximumConfigurationBytes, &document); err != nil {
		return nil, fmt.Errorf("decode configuration: %w", err)
	}
	configuration, err := build(document, options, filepath.Dir(absolutePath))
	if err != nil {
		return nil, err
	}
	configuration.configDigest = digestBytes(data)
	configuration.workspaceID = digestBytes([]byte(options.WorkspaceRoot))
	return configuration, nil
}

type fileConfig struct {
	Version              int                         `json:"version"`
	DefaultAgent         string                      `json:"default_agent,omitempty"`
	Limits               registryLimits              `json:"limits,omitempty"`
	Providers            map[string]providerProfile  `json:"providers"`
	Extensions           map[string]extensionProfile `json:"extensions,omitempty"`
	ExtensionDirectories []string                    `json:"extension_directories,omitempty"`
	Profiles             map[string]executionProfile `json:"profiles,omitempty"`
	Agents               map[string]agentProfile     `json:"agents"`
	Session              *sessionProfile             `json:"session,omitempty"`
	Evidence             *evidenceProfile            `json:"evidence,omitempty"`
	ExtensionState       *stateProfile               `json:"extension_state,omitempty"`
}

type sessionProfile struct {
	Store string `json:"store"`
	Path  string `json:"path,omitempty"`
}

type evidenceProfile struct {
	Store        string `json:"store"`
	Path         string `json:"path"`
	IsolationKey string `json:"isolation_key,omitempty"`
}

type stateProfile struct {
	Store string `json:"store"`
	Path  string `json:"path,omitempty"`
}

type registryLimits struct {
	MaxRuns          int `json:"max_runs,omitempty"`
	MaxDepth         int `json:"max_depth,omitempty"`
	MaxProviderCalls int `json:"max_provider_calls,omitempty"`
}

type providerProfile struct {
	Protocol          string                    `json:"protocol"`
	BaseURL           string                    `json:"base_url,omitempty"`
	Model             string                    `json:"model,omitempty"`
	TokenEnv          string                    `json:"token_env,omitempty"`
	AuthProfile       string                    `json:"auth_profile,omitempty"`
	MaxOutputTokens   int                       `json:"max_output_tokens,omitempty"`
	APIVersion        string                    `json:"api_version,omitempty"`
	Pricing           *cachePricingProfile      `json:"pricing,omitempty"`
	CacheCapabilities *agent.CacheCapabilities  `json:"cache_capabilities,omitempty"`
	RateLimit         *providerRateLimitProfile `json:"rate_limit,omitempty"`
}

type providerRateLimitProfile struct {
	MaxConcurrency int `json:"max_concurrency,omitempty"`
}

type agentProfile struct {
	Provider                string                `json:"provider"`
	Profile                 string                `json:"profile,omitempty"`
	Instructions            string                `json:"instructions,omitempty"`
	MaxProviderCalls        int                   `json:"max_provider_calls,omitempty"`
	MaxToolCalls            int                   `json:"max_tool_calls,omitempty"`
	MaxRepeatedToolFailures int                   `json:"max_repeated_tool_failures,omitempty"`
	ProviderRetry           *providerRetryProfile `json:"provider_retry,omitempty"`
	Delegations             []delegationProfile   `json:"delegations,omitempty"`
	Context                 *contextProfile       `json:"context,omitempty"`
	Cache                   *cacheProfile         `json:"cache,omitempty"`
}

type providerRetryProfile struct {
	MaxAttempts    int    `json:"max_attempts,omitempty"`
	InitialBackoff string `json:"initial_backoff,omitempty"`
	MaxBackoff     string `json:"max_backoff,omitempty"`
}

type contextProfile struct {
	MaxInputBytes            int64                         `json:"max_input_bytes"`
	RecentMessages           int                           `json:"recent_messages,omitempty"`
	EvidenceThresholdBytes   int                           `json:"evidence_threshold_bytes,omitempty"`
	EvidenceExcerptBytes     int                           `json:"evidence_excerpt_bytes,omitempty"`
	CheckpointMaxBytes       int                           `json:"checkpoint_max_bytes,omitempty"`
	RebaseGenerationInterval uint64                        `json:"rebase_generation_interval,omitempty"`
	EvidenceSensitivity      agent.EvidenceSensitivity     `json:"evidence_sensitivity,omitempty"`
	PredictiveBudget         *agent.PredictiveBudgetPolicy `json:"predictive_budget,omitempty"`
	Retrieval                *contextRetrievalProfile      `json:"retrieval,omitempty"`
}

type contextRetrievalProfile struct {
	MaxCallsPerRun        int   `json:"max_calls_per_run,omitempty"`
	MaxItemsPerCall       int   `json:"max_items_per_call,omitempty"`
	MaxItemsPerRun        int   `json:"max_items_per_run,omitempty"`
	MaxOutputBytesPerCall int64 `json:"max_output_bytes_per_call,omitempty"`
	MaxOutputBytesPerRun  int64 `json:"max_output_bytes_per_run,omitempty"`
}

type cacheProfile struct {
	Mode          agent.CacheMode `json:"mode"`
	TTL           agent.CacheTTL  `json:"ttl,omitempty"`
	ExpectedReuse int             `json:"expected_reuse,omitempty"`
	Required      bool            `json:"required,omitempty"`
	IsolationKey  string          `json:"isolation_key,omitempty"`
	Family        string          `json:"family,omitempty"`
}

type cachePricingProfile struct {
	Currency                      string `json:"currency"`
	UncachedInputMicrosPerMillion int64  `json:"uncached_input_micros_per_million"`
	CacheReadMicrosPerMillion     int64  `json:"cache_read_micros_per_million"`
	CacheWriteMicrosPerMillion    int64  `json:"cache_write_micros_per_million"`
	OutputMicrosPerMillion        int64  `json:"output_micros_per_million,omitempty"`
}

func clonePredictiveBudgetPolicy(context *contextProfile) *agent.PredictiveBudgetPolicy {
	if context == nil || context.PredictiveBudget == nil {
		return nil
	}
	cloned := *context.PredictiveBudget
	return &cloned
}

type executionProfile struct {
	Kind         string           `json:"kind"`
	Extensions   []string         `json:"extensions"`
	Capabilities *capabilityRules `json:"capabilities"`
	Environment  []string         `json:"environment,omitempty"`
}

type extensionProfile struct {
	Mode          string          `json:"mode"`
	Command       []string        `json:"command,omitempty"`
	Directory     string          `json:"directory,omitempty"`
	Environment   []string        `json:"environment,omitempty"`
	Configuration json.RawMessage `json:"configuration,omitempty"`
	Manifest      string          `json:"manifest,omitempty"`
}

type configuredExtension struct {
	command          host.Command
	configuration    json.RawMessage
	expectedVersion  string
	expectedManifest *protocol.Manifest
}

type capabilityRules struct {
	Allow []capability.Name `json:"allow,omitempty"`
	Ask   []capability.Name `json:"ask,omitempty"`
	Deny  []capability.Name `json:"deny,omitempty"`
}

type delegationProfile struct {
	Name              string   `json:"name"`
	Description       string   `json:"description,omitempty"`
	Strategy          string   `json:"strategy"`
	Agents            []string `json:"agents"`
	Judge             string   `json:"judge,omitempty"`
	Instructions      string   `json:"instructions,omitempty"`
	JudgeInstructions string   `json:"judge_instructions,omitempty"`
}

func build(document fileConfig, options LoadOptions, configurationDirectory string) (*Configuration, error) {
	if options.Verbose && options.Logger == nil && options.DebugWriter != nil {
		options.Logger = slog.New(slog.NewJSONHandler(options.DebugWriter, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	if !options.Verbose {
		options.Logger = nil
	}
	if document.Version != CurrentVersion {
		return nil, fmt.Errorf("unsupported configuration version %d, want %d", document.Version, CurrentVersion)
	}
	if len(document.Providers) == 0 {
		return nil, errors.New("at least one provider profile is required")
	}
	if len(document.Agents) == 0 {
		return nil, errors.New("at least one agent is required")
	}

	providers, providerLimiters, err := buildProviders(document.Providers, options)
	if err != nil {
		return nil, err
	}
	recorder := options.Recorder
	var memory *evidence.MemoryRecorder
	if recorder == nil {
		memory = &evidence.MemoryRecorder{}
		recorder = memory
	}
	extensions, err := buildExtensionCommands(document.Extensions, document.ExtensionDirectories, options, configurationDirectory)
	if err != nil {
		return nil, err
	}
	extensionState, err := buildExtensionStateStore(document.ExtensionState, configurationDirectory)
	if err != nil {
		return nil, err
	}
	profiles, err := buildExecutionProfiles(document.Profiles, extensions, options, recorder, extensionState)
	if err != nil {
		return nil, err
	}
	closeProfiles := func() {
		for _, profile := range profiles {
			_ = profile.Close()
		}
	}
	sessions, err := buildSessionStore(document.Session, configurationDirectory)
	if err != nil {
		closeProfiles()
		return nil, err
	}
	evidenceStore, err := buildEvidenceStore(document.Evidence, configurationDirectory)
	if err != nil {
		closeProfiles()
		return nil, err
	}
	pricing, err := buildCachePricing(document.Providers)
	if err != nil {
		closeProfiles()
		return nil, err
	}
	var objectStore agent.EvidenceObjectStore
	if evidenceStore != nil {
		objectStore, _ = evidenceStore.(agent.EvidenceObjectStore)
	}
	registry, err := orchestration.NewAgentRegistry(orchestration.AgentRegistryOptions{
		MaxRuns:          document.Limits.MaxRuns,
		MaxDepth:         document.Limits.MaxDepth,
		MaxProviderCalls: document.Limits.MaxProviderCalls,
	})
	if err != nil {
		closeProfiles()
		return nil, fmt.Errorf("configure Agent registry: %w", err)
	}

	builder := graphBuilder{
		agents:         document.Agents,
		providers:      providers,
		limiters:       providerLimiters,
		profiles:       profiles,
		registry:       registry,
		sessions:       sessions,
		objects:        objectStore,
		evidenceTenant: evidenceIsolationKey(document.Evidence),
		pricing:        pricing,
		states:         make(map[string]buildState, len(document.Agents)),
		logger:         options.Logger,
		toolValidator:  options.ToolInputValidator,
		semanticScorer: options.ContextSemanticScorer,
		tokenEstimator: options.TokenEstimator,
	}
	for _, agentID := range sortedKeys(document.Agents) {
		if err := validateID("agent ID", agentID); err != nil {
			closeProfiles()
			return nil, err
		}
		if err := builder.buildAgent(agentID); err != nil {
			closeProfiles()
			return nil, err
		}
	}

	if document.DefaultAgent != "" {
		if err := validateID("default_agent", document.DefaultAgent); err != nil {
			closeProfiles()
			return nil, err
		}
		if _, ok := document.Agents[document.DefaultAgent]; !ok {
			closeProfiles()
			return nil, fmt.Errorf("default agent %q is not configured", document.DefaultAgent)
		}
	}
	agentIDs := make(map[string]struct{}, len(document.Agents))
	for agentID := range document.Agents {
		agentIDs[agentID] = struct{}{}
	}
	profileClosers := make([]*coding.Profile, 0, len(profiles))
	for _, profileID := range sortedKeys(profiles) {
		profileClosers = append(profileClosers, profiles[profileID])
	}
	return &Configuration{
		Registry:            registry,
		DefaultAgent:        document.DefaultAgent,
		SessionStore:        sessions,
		EvidenceStore:       evidenceStore,
		ExtensionStateStore: extensionState,
		agentIDs:            agentIDs,
		recorder:            recorder,
		memory:              memory,
		profiles:            profileClosers,
		extensionIDs:        sortedKeys(extensions),
	}, nil
}

func buildExtensionStateStore(specification *stateProfile, configurationDirectory string) (extension.StateStore, error) {
	if specification == nil {
		return nil, nil
	}
	switch specification.Store {
	case "memory":
		if specification.Path != "" {
			return nil, errors.New("memory Extension State Store does not accept path")
		}
		return extension.NewMemoryStateStore(), nil
	case "json":
		path, err := resolveConfigurationPath(configurationDirectory, specification.Path)
		if err != nil {
			return nil, fmt.Errorf("Extension State Store path: %w", err)
		}
		store, err := extension.NewJSONStateStore(path)
		if err != nil {
			return nil, fmt.Errorf("configure Extension State Store: %w", err)
		}
		return store, nil
	default:
		return nil, fmt.Errorf("unsupported Extension State Store %q", specification.Store)
	}
}

func buildEvidenceStore(specification *evidenceProfile, configurationDirectory string) (evidence.Store, error) {
	if specification == nil {
		return nil, nil
	}
	if specification.Store != "json" {
		return nil, fmt.Errorf("unsupported Evidence Store %q", specification.Store)
	}
	path, err := resolveConfigurationPath(configurationDirectory, specification.Path)
	if err != nil {
		return nil, fmt.Errorf("Evidence Store path: %w", err)
	}
	store, err := evidence.NewJSONStore(path)
	if err != nil {
		return nil, fmt.Errorf("configure Evidence Store: %w", err)
	}
	return store, nil
}

func buildCachePricing(profiles map[string]providerProfile) (map[string]*agent.CachePricing, error) {
	pricing := make(map[string]*agent.CachePricing, len(profiles))
	for profileID, profile := range profiles {
		if profile.Pricing == nil {
			continue
		}
		configured := &agent.CachePricing{
			Currency:                      profile.Pricing.Currency,
			UncachedInputMicrosPerMillion: profile.Pricing.UncachedInputMicrosPerMillion,
			CacheReadMicrosPerMillion:     profile.Pricing.CacheReadMicrosPerMillion,
			CacheWriteMicrosPerMillion:    profile.Pricing.CacheWriteMicrosPerMillion,
			OutputMicrosPerMillion:        profile.Pricing.OutputMicrosPerMillion,
		}
		if _, err := agent.ForecastCacheCost(*configured, 0, 1); err != nil {
			return nil, fmt.Errorf("provider profile %q pricing: %w", profileID, err)
		}
		pricing[profileID] = configured
	}
	return pricing, nil
}

func buildSessionStore(specification *sessionProfile, configurationDirectory string) (agent.SessionStore, error) {
	if specification == nil {
		return nil, nil
	}
	if strings.TrimSpace(specification.Store) != specification.Store || specification.Store == "" {
		return nil, errors.New("Session Store is required and must not have surrounding whitespace")
	}
	switch specification.Store {
	case "memory":
		if specification.Path != "" {
			return nil, errors.New("memory Session Store does not accept path")
		}
		return sessionstore.NewMemoryStore(), nil
	case "jsonl":
		path, err := resolveConfigurationPath(configurationDirectory, specification.Path)
		if err != nil {
			return nil, fmt.Errorf("Session Store path: %w", err)
		}
		store, err := sessionstore.NewJSONLStore(path)
		if err != nil {
			return nil, fmt.Errorf("configure JSONL Session Store: %w", err)
		}
		return store, nil
	default:
		return nil, fmt.Errorf("unsupported Session Store %q", specification.Store)
	}
}

func buildExecutionProfiles(
	profiles map[string]executionProfile,
	extensions map[string]configuredExtension,
	options LoadOptions,
	recorder evidence.Recorder,
	stateStore extension.StateStore,
) (map[string]*coding.Profile, error) {
	if len(profiles) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(options.WorkspaceRoot) == "" {
		return nil, errors.New("workspace root is required when a Profile is configured")
	}

	configured := make(map[string]*coding.Profile, len(profiles))
	closeConfigured := func() {
		for _, profile := range configured {
			_ = profile.Close()
		}
	}
	for _, profileID := range sortedKeys(profiles) {
		if err := validateID("Profile ID", profileID); err != nil {
			closeConfigured()
			return nil, err
		}
		specification := profiles[profileID]
		if specification.Kind != "coding" {
			closeConfigured()
			return nil, fmt.Errorf("Profile %q has unsupported kind %q", profileID, specification.Kind)
		}
		if specification.Capabilities == nil {
			closeConfigured()
			return nil, fmt.Errorf("Profile %q capabilities are required", profileID)
		}
		if err := validateCodingCapabilities(*specification.Capabilities); err != nil {
			closeConfigured()
			return nil, fmt.Errorf("Profile %q: %w", profileID, err)
		}
		if len(specification.Extensions) == 0 {
			closeConfigured()
			return nil, fmt.Errorf("Profile %q requires at least one Extension", profileID)
		}
		profileExtensions := make([]coding.ExtensionOptions, 0, len(specification.Extensions))
		seenExtensions := make(map[string]struct{}, len(specification.Extensions))
		for _, extensionID := range specification.Extensions {
			if err := validateID("Extension reference", extensionID); err != nil {
				closeConfigured()
				return nil, fmt.Errorf("Profile %q: %w", profileID, err)
			}
			if _, duplicate := seenExtensions[extensionID]; duplicate {
				closeConfigured()
				return nil, fmt.Errorf("Profile %q references Extension %q more than once", profileID, extensionID)
			}
			seenExtensions[extensionID] = struct{}{}
			configuredExtension, ok := extensions[extensionID]
			if !ok {
				closeConfigured()
				return nil, fmt.Errorf("Profile %q references unknown Extension %q", profileID, extensionID)
			}
			profileExtensions = append(profileExtensions, coding.ExtensionOptions{
				ID:               extensionID,
				Command:          configuredExtension.command,
				Configuration:    append(json.RawMessage(nil), configuredExtension.configuration...),
				ExpectedVersion:  configuredExtension.expectedVersion,
				ExpectedManifest: configuredExtension.expectedManifest,
			})
		}
		policy, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{
			Allow: append([]capability.Name(nil), specification.Capabilities.Allow...),
			Ask:   append([]capability.Name(nil), specification.Capabilities.Ask...),
			Deny:  append([]capability.Name(nil), specification.Capabilities.Deny...),
		})
		if err != nil {
			closeConfigured()
			return nil, fmt.Errorf("Profile %q capabilities: %w", profileID, err)
		}
		environment, err := selectedEnvironment(specification.Environment, options.LookupEnv)
		if err != nil {
			closeConfigured()
			return nil, fmt.Errorf("Profile %q environment: %w", profileID, err)
		}
		profileContext := options.Context
		if profileContext == nil {
			profileContext = context.Background()
		}
		profile, err := coding.New(profileContext, coding.Options{
			Root:               options.WorkspaceRoot,
			Extensions:         profileExtensions,
			Policy:             policy,
			ToolInputValidator: options.ToolInputValidator,
			Approver:           options.Approver,
			Recorder:           recorder,
			StateStore:         stateStore,
			StateScope:         "workspace:" + digestBytes([]byte(options.WorkspaceRoot+"\x00"+profileID)),
			Verbose:            options.Verbose,
			DebugWriter:        options.DebugWriter,
			Logger:             options.Logger,
			CommandEnvironment: environment,
		})
		if err != nil {
			closeConfigured()
			return nil, fmt.Errorf("configure Profile %q: %w", profileID, err)
		}
		configured[profileID] = profile
	}
	return configured, nil
}

func buildExtensionCommands(
	extensions map[string]extensionProfile,
	discoveryDirectories []string,
	options LoadOptions,
	configurationDirectory string,
) (map[string]configuredExtension, error) {
	configured := make(map[string]configuredExtension, len(extensions))
	for _, extensionID := range sortedKeys(extensions) {
		if err := validateID("Extension ID", extensionID); err != nil {
			return nil, err
		}
		specification := extensions[extensionID]
		if specification.Mode != "self-exec" && specification.Mode != "external" && specification.Mode != "manifest" {
			return nil, fmt.Errorf("Extension %q has unsupported mode %q", extensionID, specification.Mode)
		}
		environment, err := selectedEnvironment(specification.Environment, options.LookupEnv)
		if err != nil {
			return nil, fmt.Errorf("Extension %q environment: %w", extensionID, err)
		}
		command := host.Command{Environment: environment}
		configuredValue := configuredExtension{}
		switch specification.Mode {
		case "self-exec":
			if len(specification.Command) != 0 || specification.Manifest != "" {
				return nil, fmt.Errorf("Extension %q self-exec mode does not accept command or manifest", extensionID)
			}
			if options.SelfExecutable == "" || !filepath.IsAbs(options.SelfExecutable) {
				return nil, fmt.Errorf("Extension %q requires an absolute self executable", extensionID)
			}
			if options.SelfExecCatalog == nil {
				return nil, fmt.Errorf("Extension %q requires a self-exec Catalog", extensionID)
			}
			definition, registered := options.SelfExecCatalog.Lookup(extensionID)
			if !registered {
				return nil, fmt.Errorf("Extension %q is not registered for self-exec", extensionID)
			}
			command, err = definition.Command(options.SelfExecutable)
			if err != nil {
				return nil, fmt.Errorf("Extension %q self-exec command: %w", extensionID, err)
			}
			command.Environment = environment
			expected := definition.Manifest.ProtocolManifest()
			configuredValue.expectedVersion = definition.Manifest.Version
			configuredValue.expectedManifest = &expected
		case "external":
			if specification.Manifest != "" {
				return nil, fmt.Errorf("Extension %q external mode does not accept manifest", extensionID)
			}
			if len(specification.Command) == 0 || specification.Command[0] == "" {
				return nil, fmt.Errorf("Extension %q external command is required", extensionID)
			}
			path, err := resolveConfigurationPath(configurationDirectory, specification.Command[0])
			if err != nil {
				return nil, fmt.Errorf("Extension %q command: %w", extensionID, err)
			}
			command.Path = path
			command.Args = append([]string(nil), specification.Command[1:]...)
		case "manifest":
			if len(specification.Command) != 0 || specification.Directory != "" {
				return nil, fmt.Errorf("Extension %q manifest mode does not accept command or directory", extensionID)
			}
			manifestPath, err := resolveConfigurationPath(configurationDirectory, specification.Manifest)
			if err != nil {
				return nil, fmt.Errorf("Extension %q manifest: %w", extensionID, err)
			}
			resolved, err := extensionmanifest.Load(manifestPath)
			if err != nil {
				return nil, fmt.Errorf("Extension %q manifest: %w", extensionID, err)
			}
			if resolved.Manifest.ID != extensionID {
				return nil, fmt.Errorf("Extension manifest ID %q does not match configuration key %q", resolved.Manifest.ID, extensionID)
			}
			command.Path = resolved.Entrypoint
			command.Directory = resolved.Directory
			expected := resolved.Manifest.ProtocolManifest()
			configuredValue.expectedVersion = resolved.Manifest.Version
			configuredValue.expectedManifest = &expected
		}
		if specification.Mode != "manifest" && specification.Directory != "" {
			directory, err := resolveConfigurationPath(configurationDirectory, specification.Directory)
			if err != nil {
				return nil, fmt.Errorf("Extension %q directory: %w", extensionID, err)
			}
			command.Directory = directory
		}
		if len(specification.Configuration) > 0 && !json.Valid(specification.Configuration) {
			return nil, fmt.Errorf("Extension %q configuration is invalid JSON", extensionID)
		}
		configuredValue.command = command
		configuredValue.configuration = append(json.RawMessage(nil), specification.Configuration...)
		configured[extensionID] = configuredValue
	}
	directories := make([]string, len(discoveryDirectories))
	for index, directory := range discoveryDirectories {
		resolved, err := resolveConfigurationPath(configurationDirectory, directory)
		if err != nil {
			return nil, fmt.Errorf("Extension discovery directory %d: %w", index, err)
		}
		directories[index] = resolved
	}
	if len(directories) > 0 {
		discovered, err := extensionmanifest.Discover(directories...)
		if err != nil {
			return nil, fmt.Errorf("discover Extensions: %w", err)
		}
		for _, resolved := range discovered {
			extensionID := resolved.Manifest.ID
			if err := validateID("discovered Extension ID", extensionID); err != nil {
				return nil, err
			}
			if _, duplicate := configured[extensionID]; duplicate {
				return nil, fmt.Errorf("Extension %q is both configured and discovered", extensionID)
			}
			expected := resolved.Manifest.ProtocolManifest()
			configured[extensionID] = configuredExtension{
				command: host.Command{
					Path:        resolved.Entrypoint,
					Directory:   resolved.Directory,
					Environment: map[string]string{},
				},
				expectedVersion:  resolved.Manifest.Version,
				expectedManifest: &expected,
			}
		}
	}
	return configured, nil
}

func resolveConfigurationPath(base, value string) (string, error) {
	if strings.IndexByte(value, 0) >= 0 || strings.TrimSpace(value) == "" {
		return "", errors.New("path is required and must not contain NUL")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func validateCodingCapabilities(rules capabilityRules) error {
	groups := [][]capability.Name{rules.Allow, rules.Ask, rules.Deny}
	for _, names := range groups {
		for _, name := range names {
			if err := capability.ValidateName(name); err != nil {
				return err
			}
		}
	}
	return nil
}

func selectedEnvironment(names []string, lookupEnv LookupEnv) (map[string]string, error) {
	if len(names) == 0 {
		return map[string]string{}, nil
	}
	if lookupEnv == nil {
		return nil, errors.New("environment lookup is required when environment names are configured")
	}
	result := make(map[string]string, len(names))
	for _, name := range names {
		if err := validateEnvironmentName("environment name", name); err != nil {
			return nil, err
		}
		if _, duplicate := result[name]; duplicate {
			return nil, fmt.Errorf("environment name %q is configured more than once", name)
		}
		value, ok := lookupEnv(name)
		if !ok {
			return nil, fmt.Errorf("environment %q is not set", name)
		}
		result[name] = value
	}
	return result, nil
}

func buildProviders(
	profiles map[string]providerProfile,
	options LoadOptions,
) (map[string]agent.Provider, map[string]*agent.ProviderRateLimiter, error) {
	providers := make(map[string]agent.Provider, len(profiles))
	limiters := make(map[string]*agent.ProviderRateLimiter, len(profiles))
	for _, profileID := range sortedKeys(profiles) {
		if err := validateID("provider profile ID", profileID); err != nil {
			return nil, nil, err
		}
		configured, err := buildProvider(profileID, profiles[profileID], options)
		if err != nil {
			return nil, nil, fmt.Errorf("provider profile %q: %w", profileID, err)
		}
		limiter, err := buildProviderRateLimiter(profiles[profileID].RateLimit)
		if err != nil {
			return nil, nil, fmt.Errorf("provider profile %q rate limit: %w", profileID, err)
		}
		providers[profileID] = configured
		limiters[profileID] = limiter
	}
	return providers, limiters, nil
}

func buildProviderRateLimiter(specification *providerRateLimitProfile) (*agent.ProviderRateLimiter, error) {
	policy := agent.ProviderRateLimitPolicy{}
	if specification != nil {
		policy.MaxConcurrency = specification.MaxConcurrency
	}
	return agent.NewProviderRateLimiter(policy)
}

func buildProvider(profileID string, profile providerProfile, options LoadOptions) (agent.Provider, error) {
	if strings.TrimSpace(profile.Protocol) != profile.Protocol || profile.Protocol == "" {
		return nil, errors.New("protocol is required and must not have surrounding whitespace")
	}
	if strings.TrimSpace(profile.BaseURL) != profile.BaseURL {
		return nil, errors.New("base_url must not have surrounding whitespace")
	}
	if strings.TrimSpace(profile.Model) != profile.Model {
		return nil, errors.New("model must not have surrounding whitespace")
	}
	if strings.TrimSpace(profile.APIVersion) != profile.APIVersion {
		return nil, errors.New("api_version must not have surrounding whitespace")
	}
	if strings.TrimSpace(profile.AuthProfile) != profile.AuthProfile {
		return nil, errors.New("auth_profile must not have surrounding whitespace")
	}
	switch profile.Protocol {
	case protocolEcho, protocolOpenAIResponses, protocolOpenAIChat, protocolOpenAICodex, protocolAnthropic:
	default:
		return nil, fmt.Errorf("unsupported protocol %q", profile.Protocol)
	}

	switch profile.Protocol {
	case protocolEcho:
		if profile.BaseURL != "" || profile.Model != "" || profile.TokenEnv != "" ||
			profile.AuthProfile != "" || profile.MaxOutputTokens != 0 || profile.APIVersion != "" ||
			profile.CacheCapabilities != nil {
			return nil, errors.New("echo profile does not accept model, endpoint, credential, or API options")
		}
		return echo.NewProfile(profileID)
	case protocolOpenAIResponses, protocolOpenAIChat:
		if profile.AuthProfile != "" {
			return nil, errors.New("auth_profile is only supported by openai-codex profiles")
		}
		if profile.Model == "" {
			return nil, errors.New("model is required")
		}
		if profile.APIVersion != "" {
			return nil, errors.New("api_version is only supported by anthropic profiles")
		}
		api := openai.APIResponses
		if profile.Protocol == protocolOpenAIChat {
			api = openai.APIChatCompletions
		}
		credentialSource, err := configuredCredential(profileID, profile, options.LookupEnv)
		if err != nil {
			return nil, err
		}
		return openai.New(openai.Config{
			ProfileID:         profileID,
			API:               api,
			CredentialSource:  credentialSource,
			BaseURL:           profile.BaseURL,
			Model:             profile.Model,
			MaxOutputTokens:   profile.MaxOutputTokens,
			CacheCapabilities: profile.CacheCapabilities,
		})
	case protocolOpenAICodex:
		if profile.Model == "" {
			return nil, errors.New("model is required")
		}
		if profile.AuthProfile == "" {
			return nil, errors.New("auth_profile is required")
		}
		if profile.BaseURL != "" || profile.TokenEnv != "" || profile.MaxOutputTokens != 0 || profile.APIVersion != "" ||
			profile.CacheCapabilities != nil {
			return nil, errors.New("openai-codex profile accepts only protocol, model, auth_profile, pricing, and rate_limit")
		}
		var authService *chatauth.Service
		var err error
		if options.AuthStorePath == "" {
			authService, err = chatauth.NewDefault()
		} else {
			authService, err = chatauth.New(options.AuthStorePath)
		}
		if err != nil {
			return nil, fmt.Errorf("configure ChatGPT auth store: %w", err)
		}
		source, err := authService.CredentialSource(profile.AuthProfile)
		if err != nil {
			return nil, err
		}
		validationContext := options.Context
		if validationContext == nil {
			validationContext = context.Background()
		}
		if err := authService.ValidateProfile(validationContext, profile.AuthProfile); err != nil {
			return nil, err
		}
		return openaicodex.New(openaicodex.Config{
			ProfileID:           profileID,
			AuthorizationSource: source,
			Model:               profile.Model,
		})
	case protocolAnthropic:
		if profile.AuthProfile != "" {
			return nil, errors.New("auth_profile is only supported by openai-codex profiles")
		}
		if profile.Model == "" {
			return nil, errors.New("model is required")
		}
		credentialSource, err := configuredCredential(profileID, profile, options.LookupEnv)
		if err != nil {
			return nil, err
		}
		return anthropic.New(anthropic.Config{
			ProfileID:         profileID,
			CredentialSource:  credentialSource,
			BaseURL:           profile.BaseURL,
			APIVersion:        profile.APIVersion,
			Model:             profile.Model,
			MaxOutputTokens:   profile.MaxOutputTokens,
			CacheCapabilities: profile.CacheCapabilities,
		})
	}
	return nil, fmt.Errorf("unsupported protocol %q", profile.Protocol)
}

func configuredCredential(profileID string, profile providerProfile, lookupEnv LookupEnv) (providerbase.CredentialSource, error) {
	if profile.Protocol == protocolEcho {
		return nil, nil
	}
	if profile.TokenEnv == "" {
		if profile.BaseURL == "" {
			return nil, errors.New("token_env is required for the default API endpoint")
		}
		return nil, nil
	}
	if err := validateEnvironmentName("token_env", profile.TokenEnv); err != nil {
		return nil, err
	}
	if lookupEnv == nil {
		return nil, errors.New("environment lookup is required when token_env is configured")
	}

	source := environmentCredentialSource{
		profileID: profileID,
		name:      profile.TokenEnv,
		lookup:    lookupEnv,
	}
	if _, err := source.Credential(context.Background()); err != nil {
		return nil, err
	}
	return source, nil
}

type environmentCredentialSource struct {
	profileID string
	name      string
	lookup    LookupEnv
}

func (source environmentCredentialSource) Credential(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	value, ok := source.lookup(source.name)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("credential environment %q for profile %q is not set", source.name, source.profileID)
	}
	return value, nil
}

type buildState uint8

const (
	buildPending buildState = iota
	buildVisiting
	buildComplete
)

type graphBuilder struct {
	agents         map[string]agentProfile
	providers      map[string]agent.Provider
	limiters       map[string]*agent.ProviderRateLimiter
	profiles       map[string]*coding.Profile
	registry       *orchestration.AgentRegistry
	sessions       agent.SessionStore
	objects        agent.EvidenceObjectStore
	evidenceTenant string
	pricing        map[string]*agent.CachePricing
	states         map[string]buildState
	stack          []string
	logger         *slog.Logger
	toolValidator  agent.ToolInputValidator
	semanticScorer agent.ContextSemanticScorer
	tokenEstimator agent.TokenEstimator
}

func (builder *graphBuilder) buildAgent(agentID string) error {
	switch builder.states[agentID] {
	case buildComplete:
		return nil
	case buildVisiting:
		return fmt.Errorf("agent delegation cycle: %s", strings.Join(builder.cycle(agentID), " -> "))
	}

	specification, ok := builder.agents[agentID]
	if !ok {
		return fmt.Errorf("agent %q is not configured", agentID)
	}
	if err := validateID("provider reference", specification.Provider); err != nil {
		return fmt.Errorf("agent %q: %w", agentID, err)
	}
	modelProvider, ok := builder.providers[specification.Provider]
	if !ok {
		return fmt.Errorf("agent %q references unknown provider profile %q", agentID, specification.Provider)
	}

	builder.states[agentID] = buildVisiting
	builder.stack = append(builder.stack, agentID)
	defer func() {
		builder.stack = builder.stack[:len(builder.stack)-1]
	}()

	var instructions string
	tools := make([]agent.Tool, 0, 6+len(specification.Delegations))
	var componentSource agent.ComponentSource
	var currentWorldStateSource agent.CurrentWorldStateSource
	var resultReducer orchestration.ResultReducer
	if specification.Profile != "" {
		if err := validateID("Profile reference", specification.Profile); err != nil {
			return fmt.Errorf("agent %q: %w", agentID, err)
		}
		configuredProfile, exists := builder.profiles[specification.Profile]
		if !exists {
			return fmt.Errorf("agent %q references unknown Profile %q", agentID, specification.Profile)
		}
		componentSource = configuredProfile.ComponentSource()
		currentWorldStateSource = configuredProfile.CurrentWorldStateSource()
		resultReducer = configuredProfile.ResultReducer()
		instructions = configuredProfile.Instructions()
	}
	instructions = combineInstructions(instructions, specification.Instructions)

	var contextCompiler agent.ContextCompiler
	var contextRetrieval *agent.ContextRetrievalOptions
	if specification.Context != nil {
		if builder.objects == nil {
			return fmt.Errorf("agent %q context compaction requires a JSON Evidence Store", agentID)
		}
		configured, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
			MaxInputBytes:            specification.Context.MaxInputBytes,
			RecentMessages:           specification.Context.RecentMessages,
			EvidenceThresholdBytes:   specification.Context.EvidenceThresholdBytes,
			EvidenceExcerptBytes:     specification.Context.EvidenceExcerptBytes,
			CheckpointMaxBytes:       specification.Context.CheckpointMaxBytes,
			RebaseGenerationInterval: specification.Context.RebaseGenerationInterval,
		}, builder.objects, nil)
		if err != nil {
			return fmt.Errorf("agent %q context: %w", agentID, err)
		}
		contextCompiler = configured
		if specification.Context.Retrieval != nil {
			scopedStore, ok := builder.objects.(agent.ScopedEvidenceObjectStore)
			if !ok {
				return fmt.Errorf("agent %q context retrieval requires a scoped Evidence Object Store", agentID)
			}
			retrieval := specification.Context.Retrieval
			contextRetrieval = &agent.ContextRetrievalOptions{
				ObjectStore:    scopedStore,
				SemanticScorer: builder.semanticScorer,
				Limits: agent.ContextRetrievalLimits{
					MaxCallsPerRun:        retrieval.MaxCallsPerRun,
					MaxItemsPerCall:       retrieval.MaxItemsPerCall,
					MaxItemsPerRun:        retrieval.MaxItemsPerRun,
					MaxOutputBytesPerCall: retrieval.MaxOutputBytesPerCall,
					MaxOutputBytesPerRun:  retrieval.MaxOutputBytesPerRun,
				},
			}
		}
	}
	cachePolicy := agent.CachePolicy{Pricing: builder.pricing[specification.Provider]}
	if specification.Cache != nil {
		cachePolicy.Mode = specification.Cache.Mode
		cachePolicy.TTL = specification.Cache.TTL
		cachePolicy.ExpectedReuse = specification.Cache.ExpectedReuse
		cachePolicy.Required = specification.Cache.Required
		cachePolicy.IsolationKey = specification.Cache.IsolationKey
		cachePolicy.Family = specification.Cache.Family
	}
	providerRetry, err := buildProviderRetryPolicy(specification.ProviderRetry)
	if err != nil {
		return fmt.Errorf("agent %q Provider retry: %w", agentID, err)
	}
	var runtimeEvidenceAccess *agent.RuntimeEvidenceAccess
	if builder.objects != nil {
		profileID := specification.Profile
		if profileID == "" {
			profileID = agentID
		}
		sensitivity := agent.EvidenceSensitivityPrivate
		if specification.Context != nil && specification.Context.EvidenceSensitivity != "" {
			sensitivity = specification.Context.EvidenceSensitivity
		}
		runtimeEvidenceAccess = &agent.RuntimeEvidenceAccess{
			TenantID:     builder.evidenceTenant,
			ProfileID:    profileID,
			PrincipalID:  "qed.runtime:" + agentID,
			Capabilities: []string{agent.EvidenceReadCapability, agent.EvidenceWriteCapability},
			Sensitivity:  sensitivity,
		}
	}

	for index, delegation := range specification.Delegations {
		if delegation.Name == "" || strings.TrimSpace(delegation.Name) != delegation.Name {
			return fmt.Errorf("agent %q delegation %d name is required and must not have surrounding whitespace", agentID, index)
		}
		if delegation.Strategy == "" || strings.TrimSpace(delegation.Strategy) != delegation.Strategy {
			return fmt.Errorf("agent %q delegation %d strategy is required and must not have surrounding whitespace", agentID, index)
		}
		for _, childID := range delegation.Agents {
			if err := validateID("child agent ID", childID); err != nil {
				return fmt.Errorf("agent %q delegation %d: %w", agentID, index, err)
			}
			if err := builder.buildAgent(childID); err != nil {
				return fmt.Errorf("agent %q delegation %d: %w", agentID, index, err)
			}
		}
		if delegation.Judge != "" {
			if err := validateID("judge agent ID", delegation.Judge); err != nil {
				return fmt.Errorf("agent %q delegation %d: %w", agentID, index, err)
			}
			if err := builder.buildAgent(delegation.Judge); err != nil {
				return fmt.Errorf("agent %q delegation %d judge: %w", agentID, index, err)
			}
		}
		tool, err := orchestration.NewSubagentTool(orchestration.SubagentToolOptions{
			Name:              delegation.Name,
			Description:       delegation.Description,
			Registry:          builder.registry,
			Strategy:          orchestration.TeamStrategy(delegation.Strategy),
			AgentIDs:          append([]string(nil), delegation.Agents...),
			JudgeAgentID:      delegation.Judge,
			Instructions:      delegation.Instructions,
			JudgeInstructions: delegation.JudgeInstructions,
		})
		if err != nil {
			return fmt.Errorf("agent %q delegation %d: %w", agentID, index, err)
		}
		tools = append(tools, tool)
	}

	runtime, err := agent.NewRuntime(agent.Options{
		Provider:                modelProvider,
		ProviderRateLimiter:     builder.limiters[specification.Provider],
		ToolInputValidator:      builder.toolValidator,
		Tools:                   tools,
		ComponentSource:         componentSource,
		MaxProviderCalls:        specification.MaxProviderCalls,
		MaxToolCalls:            specification.MaxToolCalls,
		MaxRepeatedToolFailures: specification.MaxRepeatedToolFailures,
		SessionStore:            builder.sessions,
		ContextCompiler:         contextCompiler,
		TokenEstimator:          builder.tokenEstimator,
		PredictiveBudget:        clonePredictiveBudgetPolicy(specification.Context),
		EvidenceAccess:          runtimeEvidenceAccess,
		ContextRetrieval:        contextRetrieval,
		CurrentWorldStateSource: currentWorldStateSource,
		CachePolicy:             cachePolicy,
		ProviderRetry:           providerRetry,
		Logger:                  builder.logger,
	})
	if err != nil {
		return fmt.Errorf("agent %q runtime: %w", agentID, err)
	}
	if err := builder.registry.Register(orchestration.AgentDefinition{
		ID:            agentID,
		Runtime:       runtime,
		Instructions:  instructions,
		ResultReducer: resultReducer,
	}); err != nil {
		return fmt.Errorf("register agent %q: %w", agentID, err)
	}
	builder.states[agentID] = buildComplete
	return nil
}

func evidenceIsolationKey(specification *evidenceProfile) string {
	if specification == nil || specification.IsolationKey == "" {
		return "local"
	}
	return specification.IsolationKey
}

func buildProviderRetryPolicy(specification *providerRetryProfile) (agent.ProviderRetryPolicy, error) {
	if specification == nil {
		return agent.ProviderRetryPolicy{}, nil
	}
	policy := agent.ProviderRetryPolicy{MaxAttempts: specification.MaxAttempts}
	var err error
	if specification.InitialBackoff != "" {
		policy.InitialBackoff, err = time.ParseDuration(specification.InitialBackoff)
		if err != nil || policy.InitialBackoff <= 0 {
			return agent.ProviderRetryPolicy{}, fmt.Errorf("initial_backoff %q must be a positive Go duration", specification.InitialBackoff)
		}
	}
	if specification.MaxBackoff != "" {
		policy.MaxBackoff, err = time.ParseDuration(specification.MaxBackoff)
		if err != nil || policy.MaxBackoff <= 0 {
			return agent.ProviderRetryPolicy{}, fmt.Errorf("max_backoff %q must be a positive Go duration", specification.MaxBackoff)
		}
	}
	return policy, nil
}

func combineInstructions(first, second string) string {
	if first == "" {
		return second
	}
	if second == "" {
		return first
	}
	return first + "\n\n" + second
}

func (builder *graphBuilder) cycle(agentID string) []string {
	start := 0
	for index, stackedID := range builder.stack {
		if stackedID == agentID {
			start = index
			break
		}
	}
	cycle := append([]string(nil), builder.stack[start:]...)
	return append(cycle, agentID)
}

func readFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open configuration: %w", err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maximumConfigurationBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if len(data) > maximumConfigurationBytes {
		return nil, fmt.Errorf("configuration exceeds %d bytes", maximumConfigurationBytes)
	}
	return data, nil
}

func validateEnvironmentName(field, name string) error {
	if strings.TrimSpace(name) != name || name == "" {
		return fmt.Errorf("%s is required and must not have surrounding whitespace", field)
	}
	for index, character := range name {
		if (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') || character == '_' ||
			(index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return fmt.Errorf("%s %q must match [A-Za-z_][A-Za-z0-9_]*", field, name)
	}
	return nil
}

func validateID(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", kind)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", kind)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must not have leading or trailing whitespace", kind)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s must not contain control characters", kind)
		}
	}
	return nil
}

func sortedKeys[Value any](values map[string]Value) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}
