package agent_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/qed-runtime/qed/agent"
)

func TestRuntimeCapturesCurrentWorldStateAtProviderBoundaries(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{
			ID: "call-1", Name: "uppercase", Arguments: []byte(`{"text":"hello"}`),
		}}}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "done"}},
	}}
	source := &recordingWorldStateSource{snapshots: []agent.CurrentWorldStateSnapshot{
		worldStateSnapshot("sha256:7692c3ad3540bb803c020b3aee66cd8887123234ea0c6e7143c0add73ff431ed", 3),
		worldStateSnapshot("sha256:3fc4ccfe745870e2c0d99f71f30ff0656c8d6bb8d4d0bbf5a5e2b6526a8f949b", 3),
	}}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider, Tools: []agent.Tool{uppercaseTool{}}, CurrentWorldStateSource: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "make it uppercase"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, result, err := collectRun(handle)
	if err != nil {
		t.Fatal(err)
	}
	if result.CurrentWorldState == nil || result.CurrentWorldState.Snapshot.Files[0].Digest != source.snapshots[1].Files[0].Digest {
		t.Fatalf("Run Current World State = %#v", result.CurrentWorldState)
	}
	result.CurrentWorldState.Snapshot.Files[0].Path = "caller-mutated.txt"
	reloaded, err := handle.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CurrentWorldState == nil || reloaded.CurrentWorldState.Snapshot.Files[0].Path != "note.txt" {
		t.Fatal("Run Current World State aliases caller memory")
	}
	if len(result.Messages) != 4 {
		t.Fatalf("Run Messages = %#v", result.Messages)
	}
	for _, message := range result.Messages {
		if strings.Contains(message.Text, "Host-captured current world state") {
			t.Fatal("Current World State leaked into replayable conversation Messages")
		}
	}

	requests := provider.Requests()
	if len(requests) != 2 {
		t.Fatalf("Provider requests = %d, want 2", len(requests))
	}
	if len(requests[0].Messages) != 2 || requests[0].Messages[0].Role != agent.RoleUser ||
		!strings.Contains(requests[0].Messages[0].Text, source.snapshots[0].Files[0].Digest) ||
		requests[0].Messages[1].Text != "make it uppercase" {
		t.Fatalf("first Provider Messages = %#v", requests[0].Messages)
	}
	last := requests[1].Messages[len(requests[1].Messages)-1]
	if last.Role != agent.RoleUser || !strings.Contains(last.Text, source.snapshots[1].Files[0].Digest) {
		t.Fatalf("second Provider state Message = %#v", last)
	}

	captures := 0
	modelRequests := 0
	for index, event := range events {
		if event.Type == agent.EventModelRequest {
			modelRequests++
			if event.PrefixManifest == nil || !hasWorldStateSegment(event.PrefixManifest.Segments) {
				t.Fatalf("model request Prefix Manifest = %#v", event.PrefixManifest)
			}
		}
		if event.Type != agent.EventCurrentWorldStateCaptured {
			continue
		}
		captures++
		if event.CurrentWorldState == nil {
			t.Fatal("capture Event has no Current World State")
		}
		if err := agent.ValidateCurrentWorldState(context.Background(), *event.CurrentWorldState, events[:index]); err != nil {
			t.Fatalf("ValidateCurrentWorldState() error = %v", err)
		}
		if index+1 >= len(events) || events[index+1].Type != agent.EventModelRequest {
			t.Fatalf("capture Event[%d] is not immediately before model request: %#v", index, events)
		}
	}
	if captures != 2 || modelRequests != 2 || source.Calls() != 2 {
		t.Fatalf("captures/model requests/source calls = %d/%d/%d, want 2/2/2", captures, modelRequests, source.Calls())
	}
	if _, err := agent.BuildContextLedger(context.Background(), events); err != nil {
		t.Fatalf("BuildContextLedger() error = %v", err)
	}

	tampered := append([]agent.Event(nil), events...)
	for index := range tampered {
		if tampered[index].CurrentWorldState != nil {
			state := *tampered[index].CurrentWorldState
			state.Digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			tampered[index].CurrentWorldState = &state
			break
		}
	}
	if _, err := agent.BuildContextLedger(context.Background(), tampered); err == nil {
		t.Fatal("BuildContextLedger() accepted tampered Current World State")
	}
}

func hasWorldStateSegment(segments []agent.SegmentFingerprint) bool {
	for _, segment := range segments {
		if segment.Kind == agent.SegmentKindCurrentWorldState && segment.Stability == agent.StabilityVolatile {
			return true
		}
	}
	return false
}

func TestRuntimeFailsWhenCurrentWorldStateSourceFails(t *testing.T) {
	t.Parallel()

	runtime, err := agent.NewRuntime(agent.Options{
		Provider: &scriptedProvider{responses: []providerResponse{{message: agent.Message{Role: agent.RoleAssistant, Text: "unused"}}}},
		CurrentWorldStateSource: worldStateSourceFunc(func(context.Context, agent.CurrentWorldStateRequest) (agent.CurrentWorldStateSnapshot, error) {
			return agent.CurrentWorldStateSnapshot{}, errors.New("snapshot unavailable")
		}),
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
	events, result, runErr := collectRun(handle)
	if runErr == nil || !strings.Contains(runErr.Error(), "capture Current World State") {
		t.Fatalf("Run error = %v", runErr)
	}
	if result.ProviderCalls != 0 || events[len(events)-1].Type != agent.EventRunFailed {
		t.Fatalf("failed Run = %#v Events %#v", result, events)
	}
}

type worldStateSourceFunc func(context.Context, agent.CurrentWorldStateRequest) (agent.CurrentWorldStateSnapshot, error)

func (source worldStateSourceFunc) Snapshot(
	ctx context.Context,
	request agent.CurrentWorldStateRequest,
) (agent.CurrentWorldStateSnapshot, error) {
	return source(ctx, request)
}

type recordingWorldStateSource struct {
	mu        sync.Mutex
	snapshots []agent.CurrentWorldStateSnapshot
	calls     int
}

func (source *recordingWorldStateSource) Snapshot(
	_ context.Context,
	request agent.CurrentWorldStateRequest,
) (agent.CurrentWorldStateSnapshot, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	if request.Ledger.SourceEventCount != len(request.Events) {
		return agent.CurrentWorldStateSnapshot{}, errors.New("request Ledger does not match Events")
	}
	if source.calls >= len(source.snapshots) {
		return agent.CurrentWorldStateSnapshot{}, errors.New("snapshot source exhausted")
	}
	snapshot := source.snapshots[source.calls]
	source.calls++
	return snapshot, nil
}

func (source *recordingWorldStateSource) Calls() int {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.calls
}

func worldStateSnapshot(digest string, bytes int64) agent.CurrentWorldStateSnapshot {
	return agent.CurrentWorldStateSnapshot{
		FilesAvailable: true,
		Files: []agent.CurrentWorldFile{{
			Path: "note.txt", Status: agent.CurrentWorldFilePresent, Digest: digest, Bytes: bytes,
		}},
		Git: &agent.CurrentWorldGitState{Available: false},
	}
}
