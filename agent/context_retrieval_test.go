package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/evidence"
	"github.com/qed-runtime/qed/session"
)

func TestRuntimeContextRetrievalToolsReadBoundedSessionState(t *testing.T) {
	t.Parallel()

	const (
		sessionID = "context-retrieval"
		content   = "first line\nneedle evidence\nlast line"
	)
	objects := evidence.NewMemoryObjectStore()
	access := retrievalEvidenceAccess(sessionID)
	reference, err := objects.PutObjectScoped(context.Background(), agent.EvidenceObjectPutRequest{
		Access: access, MediaType: "text/plain", Content: []byte(content),
		RequiredCapabilities: []string{agent.EvidenceReadCapability},
		Sensitivity:          agent.EvidenceSensitivityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := session.NewMemoryStore()
	seedRetrievalSession(t, store, sessionID, reference)

	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{
			{ID: "search", Name: agent.ContextSearchToolName, Arguments: json.RawMessage(`{"query":"NEEDLE","limit":2}`)},
			{ID: "fetch", Name: agent.ContextFetchToolName, Arguments: json.RawMessage(`{"digest":"` + reference.Digest + `","max_bytes":4096}`)},
			{ID: "timeline", Name: agent.SessionTimelineToolName, Arguments: json.RawMessage(`{"limit":3}`)},
			{ID: "artifacts", Name: agent.ArtifactHistoryToolName, Arguments: json.RawMessage(`{"limit":3}`)},
			{ID: "executions", Name: agent.ExecutionHistoryToolName, Arguments: json.RawMessage(`{"limit":3}`)},
		}}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "done"}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider, SessionStore: store,
		EvidenceAccess: &agent.RuntimeEvidenceAccess{
			TenantID: "tenant", ProfileID: "profile", PrincipalID: "runtime",
			Capabilities: []string{agent.EvidenceReadCapability, agent.EvidenceWriteCapability},
		},
		ContextRetrieval: &agent.ContextRetrievalOptions{ObjectStore: objects},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID: "agent", SessionID: sessionID,
		Input: []agent.Message{{Role: agent.RoleUser, Text: "continue"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, result, runErr := collectRun(handle)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if result.Status != agent.RunStatusCompleted || len(result.ToolResults) != 5 {
		t.Fatalf("Run result = %#v", result)
	}
	wantOperations := []agent.ContextRetrievalOperation{
		agent.ContextRetrievalSearch,
		agent.ContextRetrievalFetch,
		agent.ContextRetrievalSessionTimeline,
		agent.ContextRetrievalArtifactHistory,
		agent.ContextRetrievalExecutionHistory,
	}
	for index, toolResult := range result.ToolResults {
		metadata := toolResult.ContextRetrieval
		if toolResult.IsError || metadata == nil || metadata.Operation != wantOperations[index] ||
			metadata.Outcome != agent.ContextRetrievalSucceeded || metadata.OutputBytes != int64(len(toolResult.Output)) ||
			!metadata.PostCompaction {
			t.Fatalf("Tool result %d = %#v", index, toolResult)
		}
	}
	if !strings.Contains(result.ToolResults[0].Output, "needle") ||
		!strings.Contains(result.ToolResults[0].Output, `"untrusted":true`) {
		t.Fatalf("search output = %s", result.ToolResults[0].Output)
	}
	var fetched struct {
		Content   string `json:"content"`
		Untrusted bool   `json:"untrusted"`
	}
	if err := json.Unmarshal([]byte(result.ToolResults[1].Output), &fetched); err != nil ||
		fetched.Content != content || !fetched.Untrusted {
		t.Fatalf("fetch output = %s, %v", result.ToolResults[1].Output, err)
	}
	for _, index := range []int{2, 3, 4} {
		if strings.Contains(result.ToolResults[index].Output, "prior needle instruction") ||
			strings.Contains(result.ToolResults[index].Output, content) {
			t.Fatalf("metadata history %d exposed content: %s", index, result.ToolResults[index].Output)
		}
	}

	retrievalCompletions := 0
	for _, event := range events {
		if event.Type == agent.EventToolCompleted && event.ToolResult != nil &&
			event.ToolResult.ContextRetrieval != nil {
			retrievalCompletions++
		}
	}
	if retrievalCompletions != 5 {
		t.Fatalf("Context retrieval Tool Events = %d, want 5", retrievalCompletions)
	}
	report, err := agent.BuildContextReport(context.Background(), result.RunID, events)
	if err != nil {
		t.Fatal(err)
	}
	if report.Metrics.PostCompactionRereadCount == nil || *report.Metrics.PostCompactionRereadCount != 1 {
		t.Fatalf("post-compaction rereads = %#v", report.Metrics.PostCompactionRereadCount)
	}
	snapshot, err := store.Snapshot(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	persistedRetrievals := 0
	for _, event := range snapshot.Events {
		if event.ToolResult != nil && event.ToolResult.ContextRetrieval != nil {
			persistedRetrievals++
		}
	}
	if persistedRetrievals != 5 {
		t.Fatalf("persisted Context retrieval metadata = %d, want 5", persistedRetrievals)
	}
	if _, err := agent.BuildContextLedger(context.Background(), snapshot.Events); err != nil {
		t.Fatal(err)
	}
	for index := range snapshot.Events {
		metadata := snapshot.Events[index].ToolResult
		if metadata != nil && metadata.ContextRetrieval != nil &&
			metadata.ContextRetrieval.Operation == agent.ContextRetrievalFetch {
			metadata.ContextRetrieval.PostCompaction = false
			break
		}
	}
	if _, err := agent.BuildContextLedger(context.Background(), snapshot.Events); err == nil ||
		!strings.Contains(err.Error(), "post-compaction status") {
		t.Fatalf("tampered retrieval Event error = %v", err)
	}
	records, err := objects.EvidenceObjectAccessRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := records[len(records)-1]; got.Operation != agent.EvidenceObjectAccessGet ||
		got.Outcome != agent.EvidenceObjectAccessAllowed || got.ObjectDigest != reference.Digest {
		t.Fatalf("Evidence retrieval audit = %#v", got)
	}

	requests := provider.Requests()
	if len(requests) != 2 || len(requests[0].Tools) != 5 {
		t.Fatalf("Provider Tool registry = %#v", requests)
	}
	for _, definition := range requests[0].Tools {
		if strings.Contains(definition.Name, ".") {
			t.Fatalf("Provider-facing Tool name is not portable: %q", definition.Name)
		}
	}
}

func TestRuntimeContextRetrievalRejectsUnreferencedObjectAndEnforcesCallLimit(t *testing.T) {
	t.Parallel()

	objects := evidence.NewMemoryObjectStore()
	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{
			{ID: "denied", Name: agent.ContextFetchToolName, Arguments: json.RawMessage(`{"digest":"sha256:` + strings.Repeat("1", 64) + `"}`)},
			{ID: "limited", Name: agent.SessionTimelineToolName, Arguments: json.RawMessage(`{}`)},
		}}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "done"}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider,
		EvidenceAccess: &agent.RuntimeEvidenceAccess{
			TenantID: "tenant", ProfileID: "profile", PrincipalID: "runtime",
			Capabilities: []string{agent.EvidenceReadCapability},
		},
		ContextRetrieval: &agent.ContextRetrievalOptions{
			ObjectStore: objects,
			Limits:      agent.ContextRetrievalLimits{MaxCallsPerRun: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, runErr := collectRun(handle)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if len(result.ToolResults) != 2 || !result.ToolResults[0].IsError || !result.ToolResults[1].IsError {
		t.Fatalf("Tool results = %#v", result.ToolResults)
	}
	if result.ToolResults[0].ContextRetrieval == nil ||
		result.ToolResults[0].ContextRetrieval.Outcome != agent.ContextRetrievalDenied {
		t.Fatalf("unreferenced fetch = %#v", result.ToolResults[0])
	}
	if result.ToolResults[1].ContextRetrieval == nil ||
		result.ToolResults[1].ContextRetrieval.Outcome != agent.ContextRetrievalLimit {
		t.Fatalf("call limit = %#v", result.ToolResults[1])
	}
	records, err := objects.EvidenceObjectAccessRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("unreferenced digest reached Object Store: %#v", records)
	}
}

func TestRuntimeContextSearchBoundsUTF8Snippet(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{
			ID: "search", Name: agent.ContextSearchToolName,
			Arguments: json.RawMessage(`{"query":"needle"}`),
		}}}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "done"}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider, ContextRetrieval: &agent.ContextRetrievalOptions{},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{
			Role: agent.RoleUser,
			Text: strings.Repeat("界", 300) + "needle" + strings.Repeat("界", 300),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, runErr := collectRun(handle)
	if runErr != nil {
		t.Fatal(runErr)
	}
	var response struct {
		Results []struct {
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	if len(result.ToolResults) != 1 || result.ToolResults[0].IsError {
		t.Fatalf("search Tool result = %#v", result.ToolResults)
	}
	if err := json.Unmarshal([]byte(result.ToolResults[0].Output), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || len(response.Results[0].Snippet) > 512 {
		t.Fatalf("bounded search result = %#v, response = %#v", result.ToolResults, response)
	}
}

func TestRuntimeContextRetrievalRejectsCorruptObjectStoreResult(t *testing.T) {
	t.Parallel()

	const sessionID = "context-retrieval-corrupt-store"
	objects := evidence.NewMemoryObjectStore()
	reference, err := objects.PutObjectScoped(context.Background(), agent.EvidenceObjectPutRequest{
		Access: retrievalEvidenceAccess(sessionID), MediaType: "text/plain", Content: []byte("expected"),
		RequiredCapabilities: []string{agent.EvidenceReadCapability},
		Sensitivity:          agent.EvidenceSensitivityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := session.NewMemoryStore()
	seedRetrievalSession(t, store, sessionID, reference)
	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{
			ID: "fetch", Name: agent.ContextFetchToolName,
			Arguments: json.RawMessage(`{"digest":"` + reference.Digest + `"}`),
		}}}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "done"}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider, SessionStore: store,
		EvidenceAccess: &agent.RuntimeEvidenceAccess{
			TenantID: "tenant", ProfileID: "profile", PrincipalID: "runtime",
			Capabilities: []string{agent.EvidenceReadCapability},
		},
		ContextRetrieval: &agent.ContextRetrievalOptions{ObjectStore: corruptRetrievalObjectStore{
			ScopedEvidenceObjectStore: objects,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID: "agent", SessionID: sessionID,
		Input: []agent.Message{{Role: agent.RoleUser, Text: "continue"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, runErr := collectRun(handle)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if len(result.ToolResults) != 1 || !result.ToolResults[0].IsError ||
		result.ToolResults[0].ContextRetrieval == nil ||
		result.ToolResults[0].ContextRetrieval.Outcome != agent.ContextRetrievalFailed ||
		strings.Contains(result.ToolResults[0].Output, "tampered") {
		t.Fatalf("corrupt Store result = %#v", result.ToolResults)
	}
}

func TestRuntimeContextRetrievalCountsInvalidInputAttempt(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{
			{ID: "invalid", Name: agent.ContextSearchToolName, Arguments: json.RawMessage(`{}`)},
			{ID: "limited", Name: agent.SessionTimelineToolName, Arguments: json.RawMessage(`{}`)},
		}}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "done"}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider,
		ContextRetrieval: &agent.ContextRetrievalOptions{
			Limits: agent.ContextRetrievalLimits{MaxCallsPerRun: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, runErr := collectRun(handle)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if len(result.ToolResults) != 2 || result.ToolResults[0].ContextRetrieval == nil ||
		result.ToolResults[0].ContextRetrieval.Outcome != agent.ContextRetrievalFailed ||
		result.ToolResults[1].ContextRetrieval == nil ||
		result.ToolResults[1].ContextRetrieval.Outcome != agent.ContextRetrievalLimit {
		t.Fatalf("invalid and limited results = %#v", result.ToolResults)
	}
}

func TestRuntimeContextRetrievalEnforcesItemLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		limits     agent.ContextRetrievalLimits
		wantFirst  agent.ContextRetrievalOutcome
		wantSecond agent.ContextRetrievalOutcome
	}{
		{
			name: "items",
			limits: agent.ContextRetrievalLimits{
				MaxCallsPerRun: 2, MaxItemsPerCall: 1, MaxItemsPerRun: 1,
			},
			wantFirst: agent.ContextRetrievalSucceeded, wantSecond: agent.ContextRetrievalLimit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			provider := &scriptedProvider{responses: []providerResponse{
				{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{
					{ID: "first", Name: agent.SessionTimelineToolName, Arguments: json.RawMessage(`{"limit":1}`)},
					{ID: "second", Name: agent.SessionTimelineToolName, Arguments: json.RawMessage(`{"limit":1}`)},
				}}},
				{message: agent.Message{Role: agent.RoleAssistant, Text: "done"}},
			}}
			runtime, err := agent.NewRuntime(agent.Options{
				Provider: provider,
				ContextRetrieval: &agent.ContextRetrievalOptions{
					Limits: test.limits,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			handle, err := runtime.Run(context.Background(), agent.RunRequest{
				Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, result, runErr := collectRun(handle)
			if runErr != nil {
				t.Fatal(runErr)
			}
			if len(result.ToolResults) != 2 || result.ToolResults[0].ContextRetrieval == nil ||
				result.ToolResults[1].ContextRetrieval == nil ||
				result.ToolResults[0].ContextRetrieval.Outcome != test.wantFirst ||
				result.ToolResults[1].ContextRetrieval.Outcome != test.wantSecond {
				t.Fatalf("Tool results = %#v", result.ToolResults)
			}
		})
	}
}

func TestRuntimeContextRetrievalChargesExactOutputBytes(t *testing.T) {
	t.Parallel()

	const calls = 12
	toolCalls := make([]agent.ToolCall, 0, calls)
	for index := range calls {
		toolCalls = append(toolCalls, agent.ToolCall{
			ID: "timeline-" + string(rune('a'+index)), Name: agent.SessionTimelineToolName,
			Arguments: json.RawMessage(`{"limit":1}`),
		})
	}
	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: toolCalls}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "done"}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider,
		ContextRetrieval: &agent.ContextRetrievalOptions{Limits: agent.ContextRetrievalLimits{
			MaxCallsPerRun: calls, MaxOutputBytesPerCall: 1024, MaxOutputBytesPerRun: 1024,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, runErr := collectRun(handle)
	if runErr != nil {
		t.Fatal(runErr)
	}
	var successfulBytes int64
	succeeded := 0
	limited := 0
	for _, toolResult := range result.ToolResults {
		metadata := toolResult.ContextRetrieval
		if metadata == nil {
			t.Fatalf("Tool result is missing Context retrieval metadata: %#v", toolResult)
		}
		switch metadata.Outcome {
		case agent.ContextRetrievalSucceeded:
			succeeded++
			successfulBytes += metadata.OutputBytes
		case agent.ContextRetrievalLimit:
			limited++
		default:
			t.Fatalf("Tool result outcome = %q", metadata.Outcome)
		}
	}
	if succeeded == 0 || limited == 0 || successfulBytes > 1024 {
		t.Fatalf("succeeded = %d, limited = %d, successful bytes = %d", succeeded, limited, successfulBytes)
	}
}

func TestRuntimeContextRetrievalCallLimitPreservesCompactionStatus(t *testing.T) {
	t.Parallel()

	const sessionID = "retrieval-call-limit-compaction"
	objects := evidence.NewMemoryObjectStore()
	reference, err := objects.PutObjectScoped(context.Background(), agent.EvidenceObjectPutRequest{
		Access: retrievalEvidenceAccess(sessionID), MediaType: "text/plain", Content: []byte("evidence"),
		RequiredCapabilities: []string{agent.EvidenceReadCapability},
		Sensitivity:          agent.EvidenceSensitivityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := session.NewMemoryStore()
	seedRetrievalSession(t, store, sessionID, reference)
	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{
			{ID: "first", Name: agent.SessionTimelineToolName, Arguments: json.RawMessage(`{}`)},
			{ID: "limited", Name: agent.SessionTimelineToolName, Arguments: json.RawMessage(`{}`)},
		}}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "done"}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider, SessionStore: store,
		ContextRetrieval: &agent.ContextRetrievalOptions{Limits: agent.ContextRetrievalLimits{MaxCallsPerRun: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID: "agent", SessionID: sessionID,
		Input: []agent.Message{{Role: agent.RoleUser, Text: "continue"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, runErr := collectRun(handle)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if len(result.ToolResults) != 2 || result.ToolResults[1].ContextRetrieval == nil ||
		result.ToolResults[1].ContextRetrieval.Outcome != agent.ContextRetrievalLimit ||
		!result.ToolResults[1].ContextRetrieval.PostCompaction {
		t.Fatalf("limited retrieval result = %#v", result.ToolResults)
	}
	snapshot, err := store.Snapshot(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.BuildContextLedger(context.Background(), snapshot.Events); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeContextRetrievalRejectsNonProgressPage(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{
			ID: "timeline", Name: agent.SessionTimelineToolName, Arguments: json.RawMessage(`{"limit":1}`),
		}}}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "done"}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider,
		ContextRetrieval: &agent.ContextRetrievalOptions{Limits: agent.ContextRetrievalLimits{
			MaxOutputBytesPerCall: 1024, MaxOutputBytesPerRun: 1024,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		AgentID: strings.Repeat("oversized-agent-identity", 64),
		Input:   []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, runErr := collectRun(handle)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if len(result.ToolResults) != 1 || !result.ToolResults[0].IsError ||
		result.ToolResults[0].ContextRetrieval == nil ||
		result.ToolResults[0].ContextRetrieval.Outcome != agent.ContextRetrievalLimit {
		t.Fatalf("oversized timeline result = %#v", result.ToolResults)
	}
}

func TestRuntimeRejectsInvalidContextRetrievalConfiguration(t *testing.T) {
	t.Parallel()

	_, err := agent.NewRuntime(agent.Options{
		Provider: &scriptedProvider{},
		ContextRetrieval: &agent.ContextRetrievalOptions{Limits: agent.ContextRetrievalLimits{
			MaxItemsPerCall: 64,
			MaxItemsPerRun:  32,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "max items per Run") {
		t.Fatalf("NewRuntime() error = %v", err)
	}
}

func TestValidateContextRetrievalMetadataRejectsMismatches(t *testing.T) {
	t.Parallel()

	output := `{"version":1,"results":[],"next_cursor":0,"truncated":false}`
	metadata := &agent.ContextRetrievalMetadata{
		Version: agent.ContextRetrievalMetadataVersion, Operation: agent.ContextRetrievalSearch,
		Outcome: agent.ContextRetrievalSucceeded, OutputBytes: int64(len(output)),
	}
	if err := agent.ValidateContextRetrievalMetadata(agent.ContextSearchToolName, output, false, metadata); err != nil {
		t.Fatal(err)
	}
	changed := *metadata
	changed.Operation = agent.ContextRetrievalFetch
	if err := agent.ValidateContextRetrievalMetadata(agent.ContextSearchToolName, output, false, &changed); err == nil {
		t.Fatal("ValidateContextRetrievalMetadata accepted mismatched operation")
	}
	changed = *metadata
	changed.OutputBytes++
	if err := agent.ValidateContextRetrievalMetadata(agent.ContextSearchToolName, output, false, &changed); err == nil {
		t.Fatal("ValidateContextRetrievalMetadata accepted mismatched output size")
	}
	changed = *metadata
	changed.ItemCount = 1
	if err := agent.ValidateContextRetrievalMetadata(agent.ContextSearchToolName, output, false, &changed); err == nil {
		t.Fatal("ValidateContextRetrievalMetadata accepted mismatched item count")
	}
	changed = *metadata
	changed.Truncated = true
	if err := agent.ValidateContextRetrievalMetadata(agent.ContextSearchToolName, output, false, &changed); err == nil {
		t.Fatal("ValidateContextRetrievalMetadata accepted mismatched truncation")
	}
}

func TestRuntimeRejectsReservedContextRetrievalMetadataFromCustomTool(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{
			ID: "custom", Name: agent.ContextSearchToolName, Arguments: json.RawMessage(`{}`),
		}}}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "done"}},
	}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider, Tools: []agent.Tool{customRetrievalMetadataTool{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "start"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, runErr := collectRun(handle)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if len(result.ToolResults) != 1 || !result.ToolResults[0].IsError ||
		result.ToolResults[0].ContextRetrieval != nil ||
		!strings.Contains(result.ToolResults[0].Output, "reserved Context retrieval metadata") {
		t.Fatalf("custom Tool result = %#v", result.ToolResults)
	}
}

func seedRetrievalSession(
	t *testing.T,
	store agent.SessionStore,
	sessionID string,
	reference agent.EvidenceObjectRef,
) {
	t.Helper()
	events := []agent.Event{
		{RunID: "run-prior", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "run-prior", Sequence: 2, Type: agent.EventUserMessageAdded,
			Message: &agent.Message{Role: agent.RoleUser, Text: "prior needle instruction"},
		},
		{
			RunID: "run-prior", Sequence: 3, Type: agent.EventContextCompacted,
			ContextCompaction: &agent.ContextCompactionReport{
				Applied: true, Reason: "externalize_evidence", OriginalBytes: 100, CompiledBytes: 60,
				SourceMessageCount: 1, RecentMessageCount: 1,
				Externalized: []agent.EvidenceObjectRef{reference},
			},
		},
		{RunID: "run-prior", Sequence: 4, Type: agent.EventRunCompleted},
	}
	if _, err := store.Append(context.Background(), sessionID, 0, events); err != nil {
		t.Fatal(err)
	}
}

func retrievalEvidenceAccess(sessionID string) agent.EvidenceAccess {
	return agent.EvidenceAccess{
		Scope: agent.EvidenceScope{
			TenantID: "tenant", SessionID: sessionID, ProfileID: "profile",
		},
		PrincipalID:  "runtime",
		Capabilities: []string{agent.EvidenceReadCapability, agent.EvidenceWriteCapability},
	}
}

type customRetrievalMetadataTool struct{}

func (customRetrievalMetadataTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: agent.ContextSearchToolName}
}

func (customRetrievalMetadataTool) Execute(context.Context, agent.ToolCall) (agent.ToolResult, error) {
	return agent.ToolResult{
		Output: "{}",
		ContextRetrieval: &agent.ContextRetrievalMetadata{
			Version:     agent.ContextRetrievalMetadataVersion,
			Operation:   agent.ContextRetrievalSearch,
			Outcome:     agent.ContextRetrievalSucceeded,
			OutputBytes: 2,
		},
	}, nil
}

type corruptRetrievalObjectStore struct {
	agent.ScopedEvidenceObjectStore
}

func (corruptRetrievalObjectStore) GetObjectScoped(
	context.Context,
	agent.EvidenceObjectGetRequest,
) ([]byte, error) {
	return []byte("tampered"), nil
}
