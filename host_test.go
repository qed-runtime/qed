package qed_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/qed-runtime/qed"
	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/orchestration"
	"github.com/qed-runtime/qed/provider/echo"
	"github.com/qed-runtime/qed/session"
)

func TestLoadHostRunsDefaultAgentAndSavesEvidence(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	configurationPath := filepath.Join(directory, "qed.json")
	if err := os.WriteFile(configurationPath, []byte(`{
		"version":1,
		"default_agent":"main",
		"providers":{"local":{"protocol":"echo"}},
		"agents":{"main":{"provider":"local"}},
		"evidence":{"store":"json","path":"evidence"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := qed.LoadHost(configurationPath, qed.HostLoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := host.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	if host.DefaultAgent() != "main" || !reflect.DeepEqual(host.AgentIDs(), []string{"main"}) {
		t.Fatalf("Host agents = %q/%v", host.DefaultAgent(), host.AgentIDs())
	}
	var observed []agent.EventType
	outcome, err := host.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
	}, func(_ context.Context, _ *agent.RunHandle, event agent.Event) error {
		observed = append(observed, event.Type)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Result.Status != agent.RunStatusCompleted || outcome.Evidence == nil {
		t.Fatalf("Run() = %#v", outcome)
	}
	if len(observed) == 0 || len(outcome.Events) != len(observed) {
		t.Fatalf("observed Events = %v, outcome Events = %d", observed, len(outcome.Events))
	}
	loaded, err := host.EvidenceStore().Load(context.Background(), outcome.Result.RunID)
	if err != nil || loaded.Run.ID != outcome.Result.RunID {
		t.Fatalf("Evidence Load() = %#v, %v", loaded, err)
	}
}

func TestProgrammaticHostResolvesAgentsWithoutOwningEvidence(t *testing.T) {
	t.Parallel()

	runtime, err := agent.NewRuntime(agent.Options{Provider: echo.New()})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := orchestration.NewAgentRegistry(orchestration.AgentRegistryOptions{
		Agents: []orchestration.AgentDefinition{{ID: "main", Runtime: runtime}},
	})
	if err != nil {
		t.Fatal(err)
	}
	host, err := qed.NewHost(registry, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.SaveRunEvidence(context.Background(), agent.RunResult{}, nil); !errors.Is(err, qed.ErrEvidenceUnavailable) {
		t.Fatalf("SaveRunEvidence() error = %v", err)
	}
	if _, err := qed.NewHost(registry, "missing"); err == nil {
		t.Fatal("NewHost() accepted an unregistered default Agent")
	}

	var waitGroup sync.WaitGroup
	errorsByRun := make(chan error, 4)
	for index := 0; index < 4; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			outcome, err := host.Run(context.Background(), agent.RunRequest{
				Input: []agent.Message{{Role: agent.RoleUser, Text: "concurrent"}},
			}, nil)
			if err == nil && outcome.Result.Status != agent.RunStatusCompleted {
				err = errors.New("concurrent Run did not complete")
			}
			errorsByRun <- err
		}()
	}
	waitGroup.Wait()
	close(errorsByRun)
	for err := range errorsByRun {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestHostStartSteersActiveRunAndFollowsUpPersistedSession(t *testing.T) {
	t.Parallel()

	provider := newHostGatedProvider([]agent.Message{
		{Role: agent.RoleAssistant, Text: "first response"},
		{Role: agent.RoleAssistant, Text: "steered response"},
		{Role: agent.RoleAssistant, Text: "follow-up response"},
	})
	runtime, err := agent.NewRuntime(agent.Options{
		Provider:     provider,
		SessionStore: session.NewMemoryStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := orchestration.NewAgentRegistry(orchestration.AgentRegistryOptions{
		Agents: []orchestration.AgentDefinition{{ID: "main", Runtime: runtime}},
	})
	if err != nil {
		t.Fatal(err)
	}
	host, err := qed.NewHost(registry, "main")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := host.Start(ctx, agent.RunRequest{
		SessionID: "host-steering",
		Input:     []agent.Message{{Role: agent.RoleUser, Text: "initial request"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider.waitForCall(t, 0)
	steered := make(chan error, 1)
	go func() {
		steered <- first.Steer(agent.Message{Role: agent.RoleUser, Text: "steering request"})
	}()
	select {
	case err := <-steered:
		if err != nil {
			t.Fatalf("Steer() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Steer blocked until the active Provider call completed")
	}
	provider.releaseCall(0)
	provider.waitForCall(t, 1)
	provider.releaseCall(1)

	firstEvents, firstResult, err := collectHostRun(first)
	if err != nil || firstResult.Status != agent.RunStatusCompleted {
		t.Fatalf("first Run = %#v, %v", firstResult, err)
	}
	steeringEvents := 0
	for _, event := range firstEvents {
		if event.Type == agent.EventUserMessageAdded &&
			event.UserMessageOrigin == agent.UserMessageOriginSteering {
			steeringEvents++
			if event.Message == nil || event.Message.Text != "steering request" {
				t.Fatalf("steering Event = %#v", event)
			}
		}
	}
	if steeringEvents != 1 {
		t.Fatalf("steering Event count = %d, want 1: %#v", steeringEvents, firstEvents)
	}

	followUp, err := host.Start(ctx, agent.RunRequest{
		SessionID: "host-steering",
		Input:     []agent.Message{{Role: agent.RoleUser, Text: "follow-up request"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider.waitForCall(t, 2)
	provider.releaseCall(2)
	_, followUpResult, err := collectHostRun(followUp)
	if err != nil || followUpResult.Status != agent.RunStatusCompleted {
		t.Fatalf("follow-up Run = %#v, %v", followUpResult, err)
	}
	if followUpResult.RunID == firstResult.RunID {
		t.Fatalf("follow-up reused Run ID %q", followUpResult.RunID)
	}

	requests := provider.Requests()
	if len(requests) != 3 {
		t.Fatalf("Provider request count = %d, want 3", len(requests))
	}
	wantHistory := []struct {
		role agent.Role
		text string
	}{
		{role: agent.RoleUser, text: "initial request"},
		{role: agent.RoleAssistant, text: "first response"},
		{role: agent.RoleUser, text: "steering request"},
		{role: agent.RoleAssistant, text: "steered response"},
		{role: agent.RoleUser, text: "follow-up request"},
	}
	if len(requests[2].Messages) != len(wantHistory) {
		t.Fatalf("follow-up history = %#v", requests[2].Messages)
	}
	for index, want := range wantHistory {
		got := requests[2].Messages[index]
		if got.Role != want.role || got.Text != want.text {
			t.Fatalf("follow-up history[%d] = %#v, want %q/%q", index, got, want.role, want.text)
		}
	}
}

type hostGatedProvider struct {
	mu        sync.Mutex
	responses []agent.Message
	requests  []agent.ModelRequest
	entered   chan int
	releases  []chan struct{}
}

func newHostGatedProvider(responses []agent.Message) *hostGatedProvider {
	releases := make([]chan struct{}, len(responses))
	for index := range releases {
		releases[index] = make(chan struct{})
	}
	return &hostGatedProvider{
		responses: append([]agent.Message(nil), responses...),
		entered:   make(chan int, len(responses)),
		releases:  releases,
	}
}

func (provider *hostGatedProvider) Name() string {
	return "host-gated"
}

func (provider *hostGatedProvider) Stream(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	provider.mu.Lock()
	index := len(provider.requests)
	provider.requests = append(provider.requests, request)
	if index >= len(provider.responses) {
		provider.mu.Unlock()
		return nil, fmt.Errorf("host gated Provider exhausted at call %d", index+1)
	}
	response := provider.responses[index]
	release := provider.releases[index]
	provider.mu.Unlock()

	select {
	case provider.entered <- index:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-release:
		return agent.MessageStream(response), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (provider *hostGatedProvider) waitForCall(t *testing.T, want int) {
	t.Helper()
	select {
	case got := <-provider.entered:
		if got != want {
			t.Fatalf("Provider call = %d, want %d", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for Provider call %d", want)
	}
}

func (provider *hostGatedProvider) releaseCall(index int) {
	close(provider.releases[index])
}

func (provider *hostGatedProvider) Requests() []agent.ModelRequest {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]agent.ModelRequest(nil), provider.requests...)
}

func collectHostRun(handle *agent.RunHandle) ([]agent.Event, agent.RunResult, error) {
	var events []agent.Event
	for event := range handle.Events() {
		events = append(events, event)
	}
	result, err := handle.Wait()
	return events, result, err
}
