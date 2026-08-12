package orchestration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/evidence"
	"github.com/qed-runtime/qed/orchestration"
	"github.com/qed-runtime/qed/session"
)

func TestRunTeamBuildsAndValidatesProfileResultPacket(t *testing.T) {
	t.Parallel()

	factValue := json.RawMessage(` { "answer" : 42 } `)
	profileState := json.RawMessage(` { "domain" : { "ready" : true } } `)
	reducer := orchestration.ResultReducerFunc(func(
		ctx context.Context,
		request orchestration.ResultReductionRequest,
	) (orchestration.ResultReduction, error) {
		if err := ctx.Err(); err != nil {
			return orchestration.ResultReduction{}, err
		}
		source, ok := findResultPacketSource(request.Result, agent.EventMessageCompleted)
		if !ok {
			return orchestration.ResultReduction{}, errors.New("assistant completion source is missing")
		}
		request.Result.Messages[0].Text = "mutated by reducer"
		request.Result.ContextLedger.Sources[0].Type = agent.EventRunFailed
		return orchestration.ResultReduction{
			Facts: []orchestration.ResultFact{{
				Kind: "review.score", Value: factValue,
				Sources: []agent.ContextLedgerEventRef{source},
			}},
			Artifacts: []orchestration.ResultArtifact{{
				Kind: "review.report", Name: "report.json", Digest: resultPacketDigest("report"),
				Bytes: 6, MediaType: "application/json", Sources: []agent.ContextLedgerEventRef{source},
			}},
			Executions: []orchestration.ResultExecution{{
				Kind: "review.check", Name: "verify", State: orchestration.ResultExecutionSucceeded,
				RunID: request.Result.RunID, OutputDigest: resultPacketDigest("verified"),
				Sources: []agent.ContextLedgerEventRef{source},
			}},
			ProfileState: profileState,
		}, nil
	})
	registry := newTestRegistry(t, orchestration.AgentRegistryOptions{
		Agents: []orchestration.AgentDefinition{{
			ID: "reviewer", Runtime: newTestRuntime(t, &scriptedProvider{responses: []providerResponse{
				{message: agent.Message{Role: agent.RoleAssistant, Text: "reviewed"}},
			}}, nil), ResultReducer: reducer,
		}},
	})

	result, err := registry.RunTeam(context.Background(), orchestration.TeamRequest{
		Strategy: orchestration.TeamStrategyDelegate,
		AgentIDs: []string{"reviewer"},
		Input:    []agent.Message{{Role: agent.RoleUser, Text: "review"}},
	})
	if err != nil {
		t.Fatalf("RunTeam() error = %v", err)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].ResultPacket == nil {
		t.Fatalf("candidate Result Packet = %#v", result.Candidates)
	}
	packet := *result.Candidates[0].ResultPacket
	if result.Candidates[0].Result.Messages[0].Text != "review" ||
		result.Candidates[0].Result.ContextLedger.Sources[0].Type != agent.EventRunStarted {
		t.Fatal("Result reducer mutated the retained child Run result")
	}
	if err := orchestration.ValidateResultPacket(context.Background(), packet, result.Candidates[0].Result); err != nil {
		t.Fatalf("ValidateResultPacket() error = %v", err)
	}
	if packet.Version != orchestration.ResultPacketVersion || packet.Digest == "" ||
		packet.Source != result.Candidates[0].Result.ContextLedger.Reference() {
		t.Fatalf("Result Packet identity = %#v", packet)
	}
	if len(packet.Facts) != 1 || packet.Facts[0].ID == "" || string(packet.Facts[0].Value) != `{"answer":42}` {
		t.Errorf("Result Packet Facts = %#v", packet.Facts)
	}
	if len(packet.Artifacts) != 1 || packet.Artifacts[0].ID == "" ||
		len(packet.Executions) != 1 || packet.Executions[0].ID == "" {
		t.Errorf("Result Packet typed entries = %#v / %#v", packet.Artifacts, packet.Executions)
	}
	if string(packet.ProfileState) != `{"domain":{"ready":true}}` {
		t.Errorf("ProfileState = %s", packet.ProfileState)
	}
	encodedPacket, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	var decodedPacket orchestration.ResultPacket
	if err := json.Unmarshal(encodedPacket, &decodedPacket); err != nil {
		t.Fatal(err)
	}
	if err := orchestration.ValidateResultPacket(
		context.Background(), decodedPacket, result.Candidates[0].Result,
	); err != nil {
		t.Fatalf("ValidateResultPacket(JSON round trip) error = %v", err)
	}

	factValue[0] = '['
	profileState[0] = '['
	if string(packet.Facts[0].Value) != `{"answer":42}` || string(packet.ProfileState) != `{"domain":{"ready":true}}` {
		t.Fatal("Result Packet aliases reducer-owned JSON")
	}
	tampered := packet
	tampered.Facts = append([]orchestration.ResultFact(nil), packet.Facts...)
	tampered.Facts[0].Value = json.RawMessage(`{"answer":43}`)
	if err := orchestration.ValidateResultPacket(context.Background(), tampered, result.Candidates[0].Result); !errors.Is(err, orchestration.ErrInvalidResultPacket) {
		t.Fatalf("ValidateResultPacket(tampered) error = %v", err)
	}
}

func TestLedgerResultReducerReturnsCurrentRunArtifactsAndExecutions(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{
			ID: "inspect-1", Name: "inspect", Arguments: json.RawMessage(`{"value":"input"}`),
		}}}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "done"}},
	}}
	registry := newTestRegistry(t, orchestration.AgentRegistryOptions{
		Agents: []orchestration.AgentDefinition{{
			ID: "worker", Runtime: newTestRuntime(t, provider, []agent.Tool{resultPacketTool{}}),
		}},
	})
	result, err := registry.RunTeam(context.Background(), orchestration.TeamRequest{
		Strategy: orchestration.TeamStrategyDelegate,
		AgentIDs: []string{"worker"},
		Input:    []agent.Message{{Role: agent.RoleUser, Text: "inspect"}},
	})
	if err != nil {
		t.Fatalf("RunTeam() error = %v", err)
	}
	packet := result.Candidates[0].ResultPacket
	if packet == nil {
		t.Fatal("default Result Packet = nil")
	}
	if len(packet.Facts) != 0 || len(packet.Artifacts) != 1 || packet.Artifacts[0].Kind != "tool_output" {
		t.Errorf("default Result Packet artifacts = %#v", packet.Artifacts)
	}
	if len(packet.Executions) != 3 {
		t.Fatalf("default Result Packet executions = %#v", packet.Executions)
	}
	for _, execution := range packet.Executions {
		if execution.RunID != result.Candidates[0].Result.RunID || execution.State != orchestration.ResultExecutionSucceeded {
			t.Errorf("default execution = %#v", execution)
		}
	}
	if err := orchestration.ValidateResultPacket(context.Background(), *packet, result.Candidates[0].Result); err != nil {
		t.Fatalf("ValidateResultPacket() error = %v", err)
	}
}

func TestRunTeamRejectsInvalidResultReductionAndKeepsOtherCandidates(t *testing.T) {
	t.Parallel()

	invalidReducer := orchestration.ResultReducerFunc(func(
		_ context.Context,
		request orchestration.ResultReductionRequest,
	) (orchestration.ResultReduction, error) {
		return orchestration.ResultReduction{Facts: []orchestration.ResultFact{{
			Kind: "invalid.source", Value: json.RawMessage(`true`),
			Sources: []agent.ContextLedgerEventRef{{RunID: request.Result.RunID, Sequence: 999}},
		}}}, nil
	})
	registry := newTestRegistry(t, orchestration.AgentRegistryOptions{
		Agents: []orchestration.AgentDefinition{
			{ID: "invalid", Runtime: newTestRuntime(t, &scriptedProvider{responses: []providerResponse{
				{message: agent.Message{Role: agent.RoleAssistant, Text: "invalid output"}},
			}}, nil), ResultReducer: invalidReducer},
			{ID: "valid", Runtime: newTestRuntime(t, &scriptedProvider{responses: []providerResponse{
				{message: agent.Message{Role: agent.RoleAssistant, Text: "valid output"}},
			}}, nil)},
		},
	})
	result, err := registry.RunTeam(context.Background(), orchestration.TeamRequest{
		Strategy: orchestration.TeamStrategyCollect,
		AgentIDs: []string{"invalid", "valid"},
		Input:    []agent.Message{{Role: agent.RoleUser, Text: "work"}},
	})
	if err != nil {
		t.Fatalf("RunTeam() partial reduction error = %v", err)
	}
	if len(result.Candidates) != 2 || result.Candidates[0].ResultPacket != nil ||
		!strings.Contains(result.Candidates[0].Error, orchestration.ErrInvalidResultPacket.Error()) {
		t.Errorf("invalid reduction outcome = %#v", result.Candidates[0])
	}
	if result.Candidates[1].Error != "" || result.Candidates[1].ResultPacket == nil {
		t.Errorf("valid reduction outcome = %#v", result.Candidates[1])
	}
}

func TestSubagentResultPacketReplaysThroughSessionStores(t *testing.T) {
	t.Parallel()

	stores := map[string]func(*testing.T) agent.SessionStore{
		"memory": func(*testing.T) agent.SessionStore { return session.NewMemoryStore() },
		"jsonl": func(t *testing.T) agent.SessionStore {
			store, err := session.NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return store
		},
	}
	for name, construct := range stores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			reducer := orchestration.ResultReducerFunc(func(
				_ context.Context,
				request orchestration.ResultReductionRequest,
			) (orchestration.ResultReduction, error) {
				source, ok := findResultPacketSource(request.Result, agent.EventMessageCompleted)
				if !ok {
					return orchestration.ResultReduction{}, errors.New("assistant completion source is missing")
				}
				return orchestration.ResultReduction{Facts: []orchestration.ResultFact{{
					Kind: "session.replay", Value: json.RawMessage(`{"replayed":true}`),
					Sources: []agent.ContextLedgerEventRef{source},
				}}}, nil
			})
			childRuntime := newTestRuntime(t, &scriptedProvider{responses: []providerResponse{
				{message: agent.Message{Role: agent.RoleAssistant, Text: "child result"}},
			}}, nil)
			registry := newTestRegistry(t, orchestration.AgentRegistryOptions{Agents: []orchestration.AgentDefinition{{
				ID: "child", Runtime: childRuntime, ResultReducer: reducer,
			}}})
			tool, err := orchestration.NewSubagentTool(orchestration.SubagentToolOptions{
				Name: "delegate", Registry: registry, Strategy: orchestration.TeamStrategyDelegate,
				AgentIDs: []string{"child"},
			})
			if err != nil {
				t.Fatal(err)
			}
			parentProvider := &scriptedProvider{responses: []providerResponse{
				{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{
					ID: "delegate-1", Name: "delegate", Arguments: json.RawMessage(`{"prompt":"work"}`),
				}}}},
				{message: agent.Message{Role: agent.RoleAssistant, Text: "parent result"}},
			}}
			store := construct(t)
			parentRuntime, err := agent.NewRuntime(agent.Options{
				Provider: parentProvider, Tools: []agent.Tool{tool}, SessionStore: store,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.Register(orchestration.AgentDefinition{ID: "parent", Runtime: parentRuntime}); err != nil {
				t.Fatal(err)
			}
			result, err := registry.Run(context.Background(), agent.RunRequest{
				AgentID: "parent", SessionID: "result-packet-session",
				Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
			})
			if err != nil {
				t.Fatalf("parent Run error = %v", err)
			}
			if len(result.ToolResults) != 1 {
				t.Fatalf("parent Tool results = %#v", result.ToolResults)
			}
			snapshot, err := store.Snapshot(context.Background(), "result-packet-session")
			if err != nil {
				t.Fatal(err)
			}
			var replayed string
			for _, event := range snapshot.Events {
				if event.Type == agent.EventToolCompleted && event.ToolResult != nil && event.ToolCall != nil &&
					event.ToolCall.Name == "delegate" {
					replayed = event.ToolResult.Output
				}
			}
			if replayed == "" || replayed != result.ToolResults[0].Output {
				t.Fatalf("replayed Result Packet output differs\nRun: %s\nReplay: %s", result.ToolResults[0].Output, replayed)
			}
			var output struct {
				Candidates []struct {
					ResultPacket *orchestration.ResultPacket `json:"result_packet"`
				} `json:"candidates"`
			}
			if err := json.Unmarshal([]byte(replayed), &output); err != nil {
				t.Fatal(err)
			}
			if len(output.Candidates) != 1 || output.Candidates[0].ResultPacket == nil ||
				len(output.Candidates[0].ResultPacket.Facts) != 1 ||
				output.Candidates[0].ResultPacket.Digest == "" {
				t.Fatalf("replayed Result Packet = %#v", output)
			}
		})
	}
}

func TestResultPacketCarriesOnlyEvidenceExposedByChildRun(t *testing.T) {
	t.Parallel()

	compiler, err := agent.NewCompactingContextCompiler(agent.ContextCompressionPolicy{
		MaxInputBytes:          3500,
		RecentMessages:         2,
		EvidenceThresholdBytes: 4096,
		EvidenceExcerptBytes:   256,
		CheckpointMaxBytes:     2200,
	}, evidence.NewMemoryObjectStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: &scriptedProvider{responses: []providerResponse{{
			message: agent.Message{Role: agent.RoleAssistant, Text: "compacted result"},
		}}},
		ContextCompiler: compiler,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := newTestRegistry(t, orchestration.AgentRegistryOptions{Agents: []orchestration.AgentDefinition{{
		ID: "compactor", Runtime: runtime,
	}}})
	messages := make([]agent.Message, 0, 12)
	for index := 0; index < 6; index++ {
		messages = append(messages,
			agent.Message{Role: agent.RoleUser, Text: strings.Repeat(string(rune('a'+index)), 600)},
			agent.Message{Role: agent.RoleAssistant, Text: strings.Repeat(string(rune('A'+index)), 600)},
		)
	}
	result, err := registry.RunTeam(context.Background(), orchestration.TeamRequest{
		Strategy: orchestration.TeamStrategyDelegate,
		AgentIDs: []string{"compactor"},
		Input:    messages,
	})
	if err != nil {
		t.Fatalf("RunTeam() error = %v", err)
	}
	candidate := result.Candidates[0]
	if candidate.Result.ContextCheckpoint == nil || candidate.ResultPacket == nil ||
		len(candidate.Result.ContextCheckpoint.Evidence) == 0 || len(candidate.ResultPacket.Evidence) == 0 {
		t.Fatalf("checkpoint or Result Packet Evidence is missing: %#v", candidate)
	}
	available := make(map[string]struct{}, len(candidate.Result.ContextCheckpoint.Evidence))
	for _, reference := range candidate.Result.ContextCheckpoint.Evidence {
		available[reference.Identity()] = struct{}{}
	}
	for _, reference := range candidate.ResultPacket.Evidence {
		if _, ok := available[reference.Identity()]; !ok {
			t.Fatalf("packet returned unavailable Evidence: %#v", reference)
		}
	}
	if err := orchestration.ValidateResultPacket(context.Background(), *candidate.ResultPacket, candidate.Result); err != nil {
		t.Fatalf("ValidateResultPacket() error = %v", err)
	}
}

func findResultPacketSource(result agent.RunResult, eventType agent.EventType) (agent.ContextLedgerEventRef, bool) {
	if result.ContextLedger == nil {
		return agent.ContextLedgerEventRef{}, false
	}
	for index := len(result.ContextLedger.Sources) - 1; index >= 0; index-- {
		if result.ContextLedger.Sources[index].Type == eventType &&
			result.ContextLedger.Sources[index].RunID == result.RunID {
			return result.ContextLedger.Sources[index].ContextLedgerEventRef, true
		}
	}
	return agent.ContextLedgerEventRef{}, false
}

func resultPacketDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

type resultPacketTool struct{}

func (resultPacketTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "inspect",
		InputSchema: json.RawMessage(
			`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`,
		),
	}
}

func (resultPacketTool) Execute(context.Context, agent.ToolCall) (agent.ToolResult, error) {
	return agent.ToolResult{Output: `{"observed":true}`}, nil
}
