package orchestration_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/orchestration"
	"github.com/qed-runtime/qed/session"
)

func TestAgentRegistryAcceptsPersistedResumeWithoutNewInput(t *testing.T) {
	t.Parallel()

	runtime, err := agent.NewRuntime(agent.Options{
		Provider:     &scriptedProvider{},
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
	handle, err := registry.Start(context.Background(), agent.RunRequest{
		AgentID:   "main",
		SessionID: "missing",
		Resume:    &agent.WaitResponse{RequestID: "wait-1"},
	})
	if err != nil {
		t.Fatalf("Start(resume) error = %v", err)
	}
	for range handle.Events() {
	}
	if _, err := handle.Wait(); err == nil || !strings.Contains(err.Error(), "has no pending wait") {
		t.Fatalf("Wait(resume) error = %v", err)
	}
}

func TestAgentRegistryRunsMixedProviderSubagentTool(t *testing.T) {
	t.Parallel()

	childProvider := namedScriptedProvider{
		name: "anthropic-messages",
		scriptedProvider: &scriptedProvider{
			responses: []providerResponse{
				{message: agent.Message{Role: agent.RoleAssistant, Text: "answer from Anthropic"}},
			},
		},
	}
	childRuntime := newTestRuntime(t, &childProvider, nil)
	registry, err := orchestration.NewAgentRegistry(orchestration.AgentRegistryOptions{
		Agents: []orchestration.AgentDefinition{
			{ID: "anthropic-specialist", Runtime: childRuntime, Instructions: "Analyze as a specialist"},
		},
	})
	if err != nil {
		t.Fatalf("NewAgentRegistry() error = %v", err)
	}

	delegateTool, err := orchestration.NewSubagentTool(orchestration.SubagentToolOptions{
		Name:         "consult_anthropic",
		Description:  "Consult the Anthropic specialist",
		Registry:     registry,
		Strategy:     orchestration.TeamStrategyDelegate,
		AgentIDs:     []string{"anthropic-specialist"},
		Instructions: "Return a concise recommendation",
	})
	if err != nil {
		t.Fatalf("NewSubagentTool() error = %v", err)
	}

	parentProvider := namedScriptedProvider{
		name: "openai-responses",
		scriptedProvider: &scriptedProvider{
			responses: []providerResponse{
				{
					message: agent.Message{
						Role: agent.RoleAssistant,
						ToolCalls: []agent.ToolCall{
							{
								ID:        "delegate-1",
								Name:      "consult_anthropic",
								Arguments: json.RawMessage(`{"prompt":"Review this design"}`),
							},
						},
					},
				},
				{message: agent.Message{Role: agent.RoleAssistant, Text: "answer from OpenAI using the specialist"}},
			},
		},
	}
	parentRuntime := newTestRuntime(t, &parentProvider, []agent.Tool{delegateTool})
	if err := registry.Register(orchestration.AgentDefinition{
		ID:           "openai-coordinator",
		Runtime:      parentRuntime,
		Instructions: "Coordinate the answer",
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	handle, err := registry.Start(context.Background(), agent.RunRequest{
		AgentID:   "openai-coordinator",
		SessionID: "session-1",
		Input:     []agent.Message{{Role: agent.RoleUser, Text: "Solve the task"}},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	events, result, err := collectRun(handle)
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if len(events) != 12 || events[0].Type != agent.EventRunStarted || events[len(events)-1].Type != agent.EventRunCompleted {
		t.Errorf("parent events = %#v", events)
	}
	if got := result.Messages[len(result.Messages)-1].Text; got != "answer from OpenAI using the specialist" {
		t.Fatalf("last message = %q", got)
	}
	if len(result.ToolResults) != 1 || result.ToolResults[0].IsError {
		t.Fatalf("ToolResults = %#v, want one successful result", result.ToolResults)
	}

	var delegated struct {
		Strategy   orchestration.TeamStrategy `json:"strategy"`
		Output     string                     `json:"output"`
		Candidates []struct {
			AgentID     string `json:"agent_id"`
			RunID       string `json:"run_id"`
			ParentRunID string `json:"parent_run_id"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(result.ToolResults[0].Output), &delegated); err != nil {
		t.Fatalf("decode Tool result: %v", err)
	}
	if delegated.Strategy != orchestration.TeamStrategyDelegate || delegated.Output != "answer from Anthropic" {
		t.Errorf("delegated result = %#v", delegated)
	}
	if len(delegated.Candidates) != 1 || delegated.Candidates[0].AgentID != "anthropic-specialist" {
		t.Fatalf("delegated candidates = %#v", delegated.Candidates)
	}
	if delegated.Candidates[0].RunID == "" || delegated.Candidates[0].ParentRunID != result.RunID {
		t.Errorf("child linkage = %#v, parent RunID %q", delegated.Candidates[0], result.RunID)
	}

	childRequests := childProvider.Requests()
	if len(childRequests) != 1 {
		t.Fatalf("child request count = %d, want 1", len(childRequests))
	}
	if got := childRequests[0].Messages[0].Text; got != "Review this design" {
		t.Errorf("child prompt = %q", got)
	}
	if got := childRequests[0].Instructions; got != "Analyze as a specialist\n\nReturn a concise recommendation" {
		t.Errorf("child instructions = %q", got)
	}
	if len(parentProvider.Requests()) != 2 {
		t.Errorf("parent Provider calls = %d, want 2", len(parentProvider.Requests()))
	}
}

func TestAgentRegistryValidatesDefinitionsAndListsStableIDs(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntime(t, &scriptedProvider{}, nil)
	registry := newTestRegistry(t, orchestration.AgentRegistryOptions{})
	if err := registry.Register(orchestration.AgentDefinition{ID: "beta", Runtime: runtime}); err != nil {
		t.Fatalf("Register(beta) error = %v", err)
	}
	if err := registry.Register(orchestration.AgentDefinition{ID: "alpha", Runtime: runtime}); err != nil {
		t.Fatalf("Register(alpha) error = %v", err)
	}
	if got := strings.Join(registry.AgentIDs(), ","); got != "alpha,beta" {
		t.Errorf("AgentIDs() = %q, want alpha,beta", got)
	}
	if err := registry.Register(orchestration.AgentDefinition{ID: "alpha", Runtime: runtime}); err == nil {
		t.Error("duplicate Register() error = nil")
	}
	if err := registry.Register(orchestration.AgentDefinition{ID: string([]byte{0xff}), Runtime: runtime}); err == nil ||
		!strings.Contains(err.Error(), "valid UTF-8") {
		t.Errorf("invalid UTF-8 Register() error = %v", err)
	}

	_, err := registry.Start(context.Background(), agent.RunRequest{
		AgentID: "missing",
		Input:   []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
	})
	if !errors.Is(err, orchestration.ErrAgentNotRegistered) {
		t.Errorf("Start(missing) error = %v, want ErrAgentNotRegistered", err)
	}
}

func TestRunTeamExecutesCandidatesConcurrentlyAndKeepsOrder(t *testing.T) {
	t.Parallel()

	entered := make(chan string, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	secondRuntime := newTestRuntime(t, gatedProvider{
		name:    "second-provider",
		output:  "second output",
		entered: entered,
		release: release,
	}, nil)
	firstRuntime := newTestRuntime(t, gatedProvider{
		name:    "first-provider",
		output:  "first output",
		entered: entered,
		release: release,
	}, nil)
	registry, err := orchestration.NewAgentRegistry(orchestration.AgentRegistryOptions{
		Agents: []orchestration.AgentDefinition{
			{ID: "first", Runtime: firstRuntime},
			{ID: "second", Runtime: secondRuntime},
		},
	})
	if err != nil {
		t.Fatalf("NewAgentRegistry() error = %v", err)
	}

	type teamResponse struct {
		result orchestration.TeamResult
		err    error
	}
	response := make(chan teamResponse, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		result, runErr := registry.RunTeam(ctx, orchestration.TeamRequest{
			Strategy: orchestration.TeamStrategyCollect,
			AgentIDs: []string{"second", "first"},
			Input:    []agent.Message{{Role: agent.RoleUser, Text: "compare"}},
		})
		response <- teamResponse{result: result, err: runErr}
	}()

	started := make(map[string]bool, 2)
	for len(started) < 2 {
		select {
		case providerName := <-entered:
			started[providerName] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("candidate Providers did not start concurrently: %#v", started)
		}
	}
	releaseOnce.Do(func() { close(release) })

	team := <-response
	if team.err != nil {
		t.Fatalf("RunTeam() error = %v", team.err)
	}
	if len(team.result.Candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2", len(team.result.Candidates))
	}
	if team.result.Candidates[0].AgentID != "second" || team.result.Candidates[0].Output != "second output" {
		t.Errorf("candidate[0] = %#v", team.result.Candidates[0])
	}
	if team.result.Candidates[1].AgentID != "first" || team.result.Candidates[1].Output != "first output" {
		t.Errorf("candidate[1] = %#v", team.result.Candidates[1])
	}
}

func TestRunTeamSelectReturnsChosenCandidateOriginalOutput(t *testing.T) {
	t.Parallel()

	alphaProvider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, Text: "alpha draft"}},
	}}
	betaProvider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, Text: "beta draft"}},
	}}
	judgeProvider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, Text: `{"selected_agent_id":"beta"}`}},
	}}
	registry := newTestRegistry(t, orchestration.AgentRegistryOptions{
		Agents: []orchestration.AgentDefinition{
			{ID: "alpha", Runtime: newTestRuntime(t, alphaProvider, nil), Instructions: "Alpha base"},
			{ID: "beta", Runtime: newTestRuntime(t, betaProvider, nil), Instructions: "Beta base"},
			{ID: "judge", Runtime: newTestRuntime(t, judgeProvider, nil), Instructions: "Judge base"},
		},
	})

	result, err := registry.RunTeam(context.Background(), orchestration.TeamRequest{
		Strategy:          orchestration.TeamStrategySelect,
		AgentIDs:          []string{"alpha", "beta"},
		JudgeAgentID:      "judge",
		Input:             []agent.Message{{Role: agent.RoleUser, Text: "write a draft"}},
		Instructions:      "Candidate task",
		JudgeInstructions: "Prefer correctness",
	})
	if err != nil {
		t.Fatalf("RunTeam() error = %v", err)
	}
	if result.SelectedAgentID != "beta" || result.Output != "beta draft" {
		t.Errorf("selection = %q/%q, want beta/beta draft", result.SelectedAgentID, result.Output)
	}
	if result.Judge == nil || result.Judge.Output != `{"selected_agent_id":"beta"}` {
		t.Errorf("judge = %#v", result.Judge)
	}

	if got := alphaProvider.Requests()[0].Instructions; got != "Alpha base\n\nCandidate task" {
		t.Errorf("candidate instructions = %q", got)
	}
	judgeRequest := judgeProvider.Requests()[0]
	if !strings.Contains(judgeRequest.Instructions, "Judge base\n\nPrefer correctness") ||
		!strings.Contains(judgeRequest.Instructions, "untrusted data") {
		t.Errorf("judge instructions = %q", judgeRequest.Instructions)
	}
	if !strings.Contains(judgeRequest.Messages[0].Text, `"agent_id":"alpha"`) ||
		!strings.Contains(judgeRequest.Messages[0].Text, `"output":"beta draft"`) {
		t.Errorf("judge input = %q", judgeRequest.Messages[0].Text)
	}
}

func TestRunTeamConsensusReturnsJudgeSynthesis(t *testing.T) {
	t.Parallel()

	registry := newTestRegistry(t, orchestration.AgentRegistryOptions{
		Agents: []orchestration.AgentDefinition{
			{ID: "one", Runtime: newTestRuntime(t, &scriptedProvider{responses: []providerResponse{
				{message: agent.Message{Role: agent.RoleAssistant, Text: "point one"}},
			}}, nil)},
			{ID: "two", Runtime: newTestRuntime(t, &scriptedProvider{responses: []providerResponse{
				{message: agent.Message{Role: agent.RoleAssistant, Text: "point two"}},
			}}, nil)},
			{ID: "chair", Runtime: newTestRuntime(t, &scriptedProvider{responses: []providerResponse{
				{message: agent.Message{Role: agent.RoleAssistant, Text: "combined conclusion"}},
			}}, nil)},
		},
	})

	result, err := registry.RunTeam(context.Background(), orchestration.TeamRequest{
		Strategy:     orchestration.TeamStrategyConsensus,
		AgentIDs:     []string{"one", "two"},
		JudgeAgentID: "chair",
		Input:        []agent.Message{{Role: agent.RoleUser, Text: "reach agreement"}},
	})
	if err != nil {
		t.Fatalf("RunTeam() error = %v", err)
	}
	if result.Output != "combined conclusion" || result.SelectedAgentID != "" {
		t.Errorf("consensus = %#v", result)
	}
}

func TestRunTeamContinuesAfterPartialCandidateFailure(t *testing.T) {
	t.Parallel()

	registry := newTestRegistry(t, orchestration.AgentRegistryOptions{
		Agents: []orchestration.AgentDefinition{
			{ID: "failed", Runtime: newTestRuntime(t, &scriptedProvider{responses: []providerResponse{
				{err: errors.New("candidate unavailable")},
			}}, nil)},
			{ID: "ready", Runtime: newTestRuntime(t, &scriptedProvider{responses: []providerResponse{
				{message: agent.Message{Role: agent.RoleAssistant, Text: "usable answer"}},
			}}, nil)},
		},
	})

	result, err := registry.RunTeam(context.Background(), orchestration.TeamRequest{
		Strategy: orchestration.TeamStrategyCollect,
		AgentIDs: []string{"failed", "ready"},
		Input:    []agent.Message{{Role: agent.RoleUser, Text: "answer"}},
	})
	if err != nil {
		t.Fatalf("RunTeam() error = %v", err)
	}
	if !strings.Contains(result.Candidates[0].Error, "candidate unavailable") {
		t.Errorf("failed candidate = %#v", result.Candidates[0])
	}
	if result.Candidates[1].Output != "usable answer" || result.Candidates[1].Error != "" {
		t.Errorf("ready candidate = %#v", result.Candidates[1])
	}
}

func TestRunTeamRejectsInvalidJudgeSelection(t *testing.T) {
	t.Parallel()

	registry := newTestRegistry(t, orchestration.AgentRegistryOptions{
		Agents: []orchestration.AgentDefinition{
			{ID: "candidate", Runtime: newTestRuntime(t, &scriptedProvider{responses: []providerResponse{
				{message: agent.Message{Role: agent.RoleAssistant, Text: "candidate output"}},
			}}, nil)},
			{ID: "judge", Runtime: newTestRuntime(t, &scriptedProvider{responses: []providerResponse{
				{message: agent.Message{Role: agent.RoleAssistant, Text: "unknown-agent"}},
			}}, nil)},
		},
	})

	_, err := registry.RunTeam(context.Background(), orchestration.TeamRequest{
		Strategy:     orchestration.TeamStrategySelect,
		AgentIDs:     []string{"candidate"},
		JudgeAgentID: "judge",
		Input:        []agent.Message{{Role: agent.RoleUser, Text: "answer"}},
	})
	if !errors.Is(err, orchestration.ErrInvalidSelection) {
		t.Fatalf("RunTeam() error = %v, want ErrInvalidSelection", err)
	}
}

func TestAgentRegistrySharesProviderCallBudget(t *testing.T) {
	t.Parallel()

	firstProvider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, Text: "first"}},
	}}
	secondProvider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, Text: "second"}},
	}}
	registry := newTestRegistry(t, orchestration.AgentRegistryOptions{
		MaxProviderCalls: 1,
		Agents: []orchestration.AgentDefinition{
			{ID: "first", Runtime: newTestRuntime(t, firstProvider, nil)},
			{ID: "second", Runtime: newTestRuntime(t, secondProvider, nil)},
		},
	})

	result, err := registry.RunTeam(context.Background(), orchestration.TeamRequest{
		Strategy: orchestration.TeamStrategyCollect,
		AgentIDs: []string{"first", "second"},
		Input:    []agent.Message{{Role: agent.RoleUser, Text: "answer"}},
	})
	if err != nil {
		t.Fatalf("RunTeam() error = %v", err)
	}

	successes := 0
	budgetFailures := 0
	for _, candidate := range result.Candidates {
		if candidate.Error == "" {
			successes++
		}
		if strings.Contains(candidate.Error, orchestration.ErrSharedProviderCallLimit.Error()) {
			budgetFailures++
		}
	}
	if successes != 1 || budgetFailures != 1 {
		t.Errorf("candidate budget outcomes = %#v", result.Candidates)
	}
	if got := len(firstProvider.Requests()) + len(secondProvider.Requests()); got != 1 {
		t.Errorf("total Provider calls = %d, want 1", got)
	}
}

func TestRunTeamCountsJudgeAgainstSharedRunBudget(t *testing.T) {
	t.Parallel()

	judgeProvider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, Text: "first"}},
	}}
	registry := newTestRegistry(t, orchestration.AgentRegistryOptions{
		MaxRuns: 2,
		Agents: []orchestration.AgentDefinition{
			{ID: "first", Runtime: newTestRuntime(t, &scriptedProvider{responses: []providerResponse{
				{message: agent.Message{Role: agent.RoleAssistant, Text: "first"}},
			}}, nil)},
			{ID: "second", Runtime: newTestRuntime(t, &scriptedProvider{responses: []providerResponse{
				{message: agent.Message{Role: agent.RoleAssistant, Text: "second"}},
			}}, nil)},
			{ID: "judge", Runtime: newTestRuntime(t, judgeProvider, nil)},
		},
	})

	result, err := registry.RunTeam(context.Background(), orchestration.TeamRequest{
		Strategy:     orchestration.TeamStrategySelect,
		AgentIDs:     []string{"first", "second"},
		JudgeAgentID: "judge",
		Input:        []agent.Message{{Role: agent.RoleUser, Text: "choose"}},
	})
	if !errors.Is(err, orchestration.ErrAgentRunLimit) {
		t.Fatalf("RunTeam() error = %v, want ErrAgentRunLimit", err)
	}
	if result.Judge == nil || !strings.Contains(result.Judge.Error, orchestration.ErrAgentRunLimit.Error()) {
		t.Errorf("judge outcome = %#v", result.Judge)
	}
	if len(judgeProvider.Requests()) != 0 {
		t.Errorf("judge Provider calls = %d, want 0", len(judgeProvider.Requests()))
	}
}

func TestSubagentToolEnforcesSharedDepthLimit(t *testing.T) {
	t.Parallel()

	childProvider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, Text: "should not run"}},
	}}
	registry := newTestRegistry(t, orchestration.AgentRegistryOptions{
		MaxDepth: 1,
		Agents: []orchestration.AgentDefinition{
			{ID: "child", Runtime: newTestRuntime(t, childProvider, nil)},
		},
	})
	tool, err := orchestration.NewSubagentTool(orchestration.SubagentToolOptions{
		Name:     "delegate",
		Registry: registry,
		Strategy: orchestration.TeamStrategyDelegate,
		AgentIDs: []string{"child"},
	})
	if err != nil {
		t.Fatalf("NewSubagentTool() error = %v", err)
	}

	parentProvider := &scriptedProvider{responses: []providerResponse{
		{
			message: agent.Message{
				Role: agent.RoleAssistant,
				ToolCalls: []agent.ToolCall{
					{Name: "delegate", Arguments: json.RawMessage(`{"prompt":"go deeper"}`)},
				},
			},
		},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "handled delegation error"}},
	}}
	parentRuntime := newTestRuntime(t, parentProvider, []agent.Tool{tool})
	if err := registry.Register(orchestration.AgentDefinition{ID: "parent", Runtime: parentRuntime}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	result, err := registry.Run(context.Background(), agent.RunRequest{
		AgentID: "parent",
		Input:   []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(result.ToolResults) != 1 || !result.ToolResults[0].IsError {
		t.Fatalf("ToolResults = %#v, want depth error", result.ToolResults)
	}
	if !strings.Contains(result.ToolResults[0].Output, orchestration.ErrAgentDepthLimit.Error()) {
		t.Errorf("Tool error = %q", result.ToolResults[0].Output)
	}
	if len(childProvider.Requests()) != 0 {
		t.Errorf("child Provider calls = %d, want 0", len(childProvider.Requests()))
	}
}

type namedScriptedProvider struct {
	name string
	*scriptedProvider
}

func (provider *namedScriptedProvider) Name() string {
	return provider.name
}

type gatedProvider struct {
	name    string
	output  string
	entered chan<- string
	release <-chan struct{}
}

func (provider gatedProvider) Name() string {
	return provider.name
}

func (provider gatedProvider) Complete(ctx context.Context, _ agent.ModelRequest) (agent.Message, error) {
	select {
	case provider.entered <- provider.name:
	case <-ctx.Done():
		return agent.Message{}, ctx.Err()
	}
	select {
	case <-provider.release:
		return agent.Message{Role: agent.RoleAssistant, Text: provider.output}, nil
	case <-ctx.Done():
		return agent.Message{}, ctx.Err()
	}
}

func (provider gatedProvider) Stream(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	message, err := provider.Complete(ctx, request)
	if err != nil {
		return nil, err
	}
	return agent.MessageStream(message), nil
}

func newTestRuntime(t *testing.T, provider agent.Provider, tools []agent.Tool) *agent.Runtime {
	t.Helper()

	runtime, err := agent.NewRuntime(agent.Options{Provider: provider, Tools: tools})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	return runtime
}

func newTestRegistry(t *testing.T, options orchestration.AgentRegistryOptions) *orchestration.AgentRegistry {
	t.Helper()

	registry, err := orchestration.NewAgentRegistry(options)
	if err != nil {
		t.Fatalf("NewAgentRegistry() error = %v", err)
	}
	return registry
}

type providerResponse struct {
	message agent.Message
	err     error
}

type scriptedProvider struct {
	mu        sync.Mutex
	responses []providerResponse
	requests  []agent.ModelRequest
}

func (provider *scriptedProvider) Name() string {
	return "scripted"
}

func (provider *scriptedProvider) Complete(_ context.Context, request agent.ModelRequest) (agent.Message, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()

	provider.requests = append(provider.requests, request)
	index := len(provider.requests) - 1
	if index >= len(provider.responses) {
		return agent.Message{}, errors.New("scripted provider exhausted")
	}
	response := provider.responses[index]
	return response.message, response.err
}

func (provider *scriptedProvider) Stream(ctx context.Context, request agent.ModelRequest) (agent.ModelStream, error) {
	message, err := provider.Complete(ctx, request)
	if err != nil {
		return nil, err
	}
	return agent.MessageStream(message), nil
}

func (provider *scriptedProvider) Requests() []agent.ModelRequest {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]agent.ModelRequest(nil), provider.requests...)
}

func collectRun(handle *agent.RunHandle) ([]agent.Event, agent.RunResult, error) {
	var events []agent.Event
	for event := range handle.Events() {
		events = append(events, event)
	}
	result, err := handle.Wait()
	return events, result, err
}
