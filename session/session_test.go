package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/session"
)

func TestSessionStoresReplayEventsAndEnforceRevisions(t *testing.T) {
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
			store := construct(t)
			ctx := context.Background()
			_, err := store.Load(ctx, "session-1")
			if !errors.Is(err, agent.ErrSessionNotFound) {
				t.Fatalf("Load() error = %v, want ErrSessionNotFound", err)
			}

			user := agent.Message{Role: agent.RoleUser, Text: "change the file"}
			call := agent.ToolCall{ID: "call-1", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"note.txt"}`)}
			wait := agent.WaitRequest{ID: "wait-1", Kind: agent.WaitKindApproval, Prompt: "approve apply_patch"}
			revision, err := store.Append(ctx, "session-1", 0, []agent.Event{
				{Type: agent.EventUserMessageAdded, Message: &user},
				{Type: agent.EventToolStarted, ToolCall: &call},
				{Type: agent.EventRunWaiting, WaitRequest: &wait},
			})
			if err != nil || revision != 3 {
				t.Fatalf("Append() = %d, %v, want 3", revision, err)
			}
			snapshot, err := store.Snapshot(ctx, "session-1")
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Revision != 3 || len(snapshot.Messages) != 1 || snapshot.Messages[0].Text != user.Text {
				t.Fatalf("Snapshot() = %#v", snapshot)
			}
			if snapshot.PendingWait == nil || snapshot.PendingWait.ID != wait.ID ||
				snapshot.PendingTool == nil || snapshot.PendingTool.ID != call.ID {
				t.Fatalf("pending state = %#v / %#v", snapshot.PendingWait, snapshot.PendingTool)
			}

			_, err = store.Append(ctx, "session-1", 2, []agent.Event{{Type: agent.EventRunResumed}})
			if !errors.Is(err, agent.ErrSessionConflict) {
				t.Fatalf("conflicting Append() error = %v", err)
			}

			response := agent.WaitResponse{RequestID: wait.ID, Payload: json.RawMessage(`{"approved":true}`)}
			toolMessage := agent.Message{Role: agent.RoleTool, Text: "changed", ToolCallID: call.ID, ToolName: call.Name}
			assistant := agent.Message{Role: agent.RoleAssistant, Text: "done"}
			revision, err = store.Append(ctx, "session-1", 3, []agent.Event{
				{Type: agent.EventRunResumed, WaitResponse: &response},
				{Type: agent.EventToolCompleted, Message: &toolMessage, ToolCall: &call},
				{Type: agent.EventMessageCompleted, Message: &assistant},
				{Type: agent.EventRunCompleted},
			})
			if err != nil || revision != 7 {
				t.Fatalf("second Append() = %d, %v, want 7", revision, err)
			}
			snapshot, err = store.Load(ctx, "session-1")
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.PendingWait != nil || snapshot.PendingTool != nil || len(snapshot.Messages) != 3 {
				t.Fatalf("completed Snapshot() = %#v", snapshot)
			}
			for index, event := range snapshot.Events {
				if event.SessionRevision != uint64(index+1) || event.SessionID != "session-1" {
					t.Fatalf("Event[%d] identity = %q/%d", index, event.SessionID, event.SessionRevision)
				}
			}
		})
	}
}

func TestJSONLStoreReopensPersistedSession(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	first, err := session.NewJSONLStore(root)
	if err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("Session root mode = %o", rootInfo.Mode().Perm())
	}
	message := agent.Message{Role: agent.RoleUser, Text: "persisted"}
	if _, err := first.Append(context.Background(), "reopen", 0, []agent.Event{{
		Type:    agent.EventUserMessageAdded,
		Message: &message,
	}}); err != nil {
		t.Fatal(err)
	}
	second, err := session.NewJSONLStore(root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := second.Load(context.Background(), "reopen")
	if err != nil || snapshot.Revision != 1 || len(snapshot.Messages) != 1 || snapshot.Messages[0].Text != "persisted" {
		t.Fatalf("reopened Snapshot() = %#v, %v", snapshot, err)
	}
}

func TestJSONLStorePreservesPrivateProviderContinuationState(t *testing.T) {
	t.Parallel()

	store, err := session.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	message := agent.Message{
		Role: agent.RoleAssistant,
		Text: "visible",
		ProviderState: &agent.ProviderState{
			Provider: "test/profile",
			Data:     json.RawMessage(`[{"type":"thinking","signature":"opaque-secret-marker"}]`),
		},
	}
	_, err = store.Append(context.Background(), "private-state", 0, []agent.Event{{
		Type:      agent.EventMessageCompleted,
		SessionID: "private-state",
		Message:   &message,
	}})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	snapshot, err := store.Load(context.Background(), "private-state")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(snapshot.Messages) != 1 || snapshot.Messages[0].ProviderState == nil {
		t.Fatalf("Messages = %#v", snapshot.Messages)
	}
	state := snapshot.Messages[0].ProviderState
	if state.Provider != "test/profile" || !strings.Contains(string(state.Data), "opaque-secret-marker") {
		t.Fatalf("ProviderState = %#v", state)
	}
	encoded, err := json.Marshal(snapshot.Events[0])
	if err != nil {
		t.Fatalf("Marshal public Event: %v", err)
	}
	if strings.Contains(string(encoded), "opaque-secret-marker") {
		t.Fatalf("public Event exposed private Provider state: %s", encoded)
	}
}

func TestMemoryStoreAllowsOnlyOneConcurrentExpectedRevision(t *testing.T) {
	t.Parallel()

	store := session.NewMemoryStore()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := store.Append(context.Background(), "shared", 0, []agent.Event{{Type: agent.EventRunStarted}})
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	var success, conflict int
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, agent.ErrSessionConflict):
			conflict++
		default:
			t.Fatalf("Append() error = %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("outcomes = success %d, conflict %d", success, conflict)
	}
}
