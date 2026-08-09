// Package qed provides the high-level Host API for embedding QED Runtime in a
// Go server, worker, or desktop application
package qed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/evidence"
	"github.com/qed-runtime/qed/extension"
	"github.com/qed-runtime/qed/extension/selfexec"
	"github.com/qed-runtime/qed/internal/agentconfig"
	"github.com/qed-runtime/qed/orchestration"
)

const evidenceSaveTimeout = 5 * time.Second

var (
	// ErrEvidenceUnavailable indicates that a Host has no configured Evidence Store
	ErrEvidenceUnavailable = errors.New("Host has no Evidence Store")
)

// LookupEnv resolves one selected environment variable
type LookupEnv func(string) (string, bool)

// HostLoadOptions supplies resources owned by an embedding application
type HostLoadOptions struct {
	// LookupEnv resolves only environment names selected by configuration
	LookupEnv LookupEnv
	// WorkspaceRoot is the host-selected root supplied to execution Profiles
	WorkspaceRoot string
	// AuthStorePath overrides the OS-default QED credential file when non-empty
	AuthStorePath string
	// SelfExecutable is the absolute current executable used for self-exec children
	SelfExecutable string
	// SelfExecCatalog contains Extensions statically linked into SelfExecutable
	SelfExecCatalog *selfexec.Catalog
	// Context bounds Extension startup and defaults to context.Background
	Context context.Context
	// Approver handles capability decisions that require external approval
	Approver capability.Approver
	// Recorder receives host-owned Tool invocation records
	Recorder evidence.Recorder
	// Verbose enables safe structured Runtime and Extension diagnostics
	Verbose bool
	// DebugWriter receives JSON diagnostics when Logger is nil
	DebugWriter io.Writer
	// Logger receives safe structured diagnostics when Verbose is true
	Logger *slog.Logger
}

// EventHandler observes one Run Event while Host.Run drains the stream
//
// The RunHandle permits an interactive embedding application to resume a
// waiting Run. Returning an error cancels the Run
type EventHandler func(ctx context.Context, handle *agent.RunHandle, event agent.Event) error

// RunOutcome contains one terminal Run and its host-owned records
type RunOutcome struct {
	// Result is the terminal Agent Run result
	Result agent.RunResult `json:"result"`
	// Events contains every public Event in sequence order
	Events []agent.Event `json:"events"`
	// Evidence is present when the Host configured and saved an Evidence Store
	Evidence *evidence.Bundle `json:"evidence,omitempty"`
}

// Host owns one Agent registry and the resources shared by embedded Runs
//
// Host is safe for concurrent Runs. The embedding application must stop
// accepting work and cancel or finish active Runs before calling CloseContext
type Host struct {
	registry       *orchestration.AgentRegistry
	defaultAgent   string
	sessionStore   agent.SessionStore
	evidenceStore  evidence.Store
	extensionState extension.StateStore
	extensionIDs   []string
	saveEvidence   func(context.Context, agent.RunResult, []agent.Event) (evidence.Bundle, error)
	closeContext   func(context.Context) error
}

// NewHost constructs a programmatic Host from an existing Agent registry
//
// Programmatic Hosts do not own Extension lifecycle or Evidence persistence
func NewHost(registry *orchestration.AgentRegistry, defaultAgent string) (*Host, error) {
	if registry == nil {
		return nil, errors.New("Agent registry is required")
	}
	if defaultAgent != "" {
		registered := false
		for _, id := range registry.AgentIDs() {
			if id == defaultAgent {
				registered = true
				break
			}
		}
		if !registered {
			return nil, fmt.Errorf("default agent %q is not registered", defaultAgent)
		}
	}
	return &Host{
		registry:     registry,
		defaultAgent: defaultAgent,
		closeContext: func(context.Context) error { return nil },
	}, nil
}

// LoadHost loads a declarative Agent graph and starts its configured Extensions
func LoadHost(path string, options HostLoadOptions) (*Host, error) {
	configured, err := agentconfig.Load(path, agentconfig.LoadOptions{
		LookupEnv:       agentconfig.LookupEnv(options.LookupEnv),
		WorkspaceRoot:   options.WorkspaceRoot,
		AuthStorePath:   options.AuthStorePath,
		SelfExecutable:  options.SelfExecutable,
		SelfExecCatalog: options.SelfExecCatalog,
		Context:         options.Context,
		Approver:        options.Approver,
		Recorder:        options.Recorder,
		Verbose:         options.Verbose,
		DebugWriter:     options.DebugWriter,
		Logger:          options.Logger,
	})
	if err != nil {
		return nil, err
	}
	host, err := NewHost(configured.Registry, configured.DefaultAgent)
	if err != nil {
		_ = configured.Close()
		return nil, err
	}
	host.sessionStore = configured.SessionStore
	host.evidenceStore = configured.EvidenceStore
	host.extensionState = configured.ExtensionStateStore
	host.extensionIDs = configured.ExtensionIDs()
	host.saveEvidence = configured.SaveRunEvidence
	host.closeContext = configured.CloseContext
	return host, nil
}

// AgentIDs returns configured Agent IDs in lexical order
func (host *Host) AgentIDs() []string {
	if host == nil || host.registry == nil {
		return nil
	}
	return host.registry.AgentIDs()
}

// ExtensionIDs returns configured and discovered Extension IDs in lexical order
func (host *Host) ExtensionIDs() []string {
	if host == nil {
		return nil
	}
	return append([]string(nil), host.extensionIDs...)
}

// DefaultAgent returns the Agent selected when a RunRequest omits AgentID
func (host *Host) DefaultAgent() string {
	if host == nil {
		return ""
	}
	return host.defaultAgent
}

// SessionStore returns the configured Session Store, if any
func (host *Host) SessionStore() agent.SessionStore {
	if host == nil {
		return nil
	}
	return host.sessionStore
}

// EvidenceStore returns the configured Evidence Store, if any
func (host *Host) EvidenceStore() evidence.Store {
	if host == nil {
		return nil
	}
	return host.evidenceStore
}

// ExtensionStateStore returns the configured Extension State Store, if any
func (host *Host) ExtensionStateStore() extension.StateStore {
	if host == nil {
		return nil
	}
	return host.extensionState
}

// ResolveAgent returns an explicit Agent ID or the Host default
func (host *Host) ResolveAgent(agentID string) (string, error) {
	if host == nil || host.registry == nil {
		return "", errors.New("Host is not initialized")
	}
	if agentID == "" {
		agentID = host.defaultAgent
	}
	if agentID == "" {
		return "", errors.New("agent ID is required because no default Agent is configured")
	}
	for _, registered := range host.registry.AgentIDs() {
		if registered == agentID {
			return agentID, nil
		}
	}
	return "", fmt.Errorf("agent %q is not configured", agentID)
}

// Start starts one embedded Agent Run and returns its low-level handle
//
// Callers using Start own Event draining, waiting input, terminal Wait, and
// optional SaveRunEvidence
func (host *Host) Start(ctx context.Context, request agent.RunRequest) (*agent.RunHandle, error) {
	if ctx == nil {
		return nil, errors.New("context must not be nil")
	}
	agentID, err := host.ResolveAgent(request.AgentID)
	if err != nil {
		return nil, err
	}
	request.AgentID = agentID
	return host.registry.Start(ctx, request)
}

// Run starts a Run, drains every Event, invokes handler, waits for completion,
// and saves Evidence when configured
func (host *Host) Run(ctx context.Context, request agent.RunRequest, handler EventHandler) (RunOutcome, error) {
	handle, err := host.Start(ctx, request)
	if err != nil {
		return RunOutcome{}, err
	}
	outcome := RunOutcome{}
	var handlerErr error
	for event := range handle.Events() {
		outcome.Events = append(outcome.Events, event)
		if handler == nil || handlerErr != nil {
			continue
		}
		if err := handler(ctx, handle, event); err != nil {
			handlerErr = fmt.Errorf("handle Run Event %q: %w", event.Type, err)
			handle.Cancel()
		}
	}
	outcome.Result, err = handle.Wait()
	var evidenceErr error
	if host.evidenceStore != nil && outcome.Result.RunID != "" {
		evidenceContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), evidenceSaveTimeout)
		bundle, saveErr := host.SaveRunEvidence(evidenceContext, outcome.Result, outcome.Events)
		cancel()
		if saveErr != nil {
			evidenceErr = fmt.Errorf("save Run Evidence: %w", saveErr)
		} else {
			outcome.Evidence = &bundle
		}
	}
	return outcome, errors.Join(err, handlerErr, evidenceErr)
}

// SaveRunEvidence builds and saves one Run Bundle when configured
func (host *Host) SaveRunEvidence(
	ctx context.Context,
	result agent.RunResult,
	events []agent.Event,
) (evidence.Bundle, error) {
	if host == nil || host.saveEvidence == nil || host.evidenceStore == nil {
		return evidence.Bundle{}, ErrEvidenceUnavailable
	}
	return host.saveEvidence(ctx, result, events)
}

// CloseContext drains and closes resources owned by a loaded Host
func (host *Host) CloseContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Host close context must not be nil")
	}
	if host == nil || host.closeContext == nil {
		return nil
	}
	return host.closeContext(ctx)
}

// Close drains and closes resources owned by a loaded Host
func (host *Host) Close() error {
	return host.CloseContext(context.Background())
}
