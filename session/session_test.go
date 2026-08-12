package session_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestSessionStoresRebuildIdenticalContextLedgers(t *testing.T) {
	t.Parallel()

	constructors := map[string]func(*testing.T) agent.SessionStore{
		"memory": func(*testing.T) agent.SessionStore { return session.NewMemoryStore() },
		"jsonl": func(t *testing.T) agent.SessionStore {
			store, err := session.NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return store
		},
	}
	baseTime := time.Date(2026, time.August, 12, 0, 0, 0, 0, time.UTC)
	events := []agent.Event{
		{RunID: "run-ledger", Sequence: 1, Type: agent.EventRunStarted, Time: baseTime},
		{
			RunID: "run-ledger", Sequence: 2, Type: agent.EventUserMessageAdded,
			Time: baseTime.Add(time.Second), Message: &agent.Message{Role: agent.RoleUser, Text: "persist this constraint"},
		},
		{RunID: "run-ledger", Sequence: 3, Type: agent.EventRunCompleted, Time: baseTime.Add(2 * time.Second)},
	}
	var want string
	for name, construct := range constructors {
		store := construct(t)
		if _, err := store.Append(context.Background(), "ledger-replay", 0, events); err != nil {
			t.Fatalf("%s Append: %v", name, err)
		}
		snapshot, err := store.Load(context.Background(), "ledger-replay")
		if err != nil {
			t.Fatalf("%s Load: %v", name, err)
		}
		ledger, err := agent.BuildContextLedger(context.Background(), snapshot.Events)
		if err != nil {
			t.Fatalf("%s BuildContextLedger: %v", name, err)
		}
		encoded, err := json.Marshal(ledger)
		if err != nil {
			t.Fatal(err)
		}
		if want == "" {
			want = string(encoded)
		} else if string(encoded) != want {
			t.Fatalf("%s rebuilt a different Ledger:\n%s\n%s", name, encoded, want)
		}
	}
}

func TestSessionStoresPreserveToolPolicyMetadata(t *testing.T) {
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
			policy := &agent.ToolPolicyDecision{
				Outcome:      "allow",
				Capabilities: []string{"filesystem.read"},
				ReasonDigest: "sha256:" + strings.Repeat("a", 64),
			}
			result := &agent.ToolResult{CallID: "call-1", Name: "read_file", Policy: policy}
			store := construct(t)
			if _, err := store.Append(context.Background(), "tool-policy", 0, []agent.Event{{
				Type: agent.EventToolCompleted, ToolResult: result,
			}}); err != nil {
				t.Fatal(err)
			}
			policy.Outcome = "deny"
			policy.Capabilities[0] = "changed"
			snapshot, err := store.Load(context.Background(), "tool-policy")
			if err != nil {
				t.Fatal(err)
			}
			stored := snapshot.Events[0].ToolResult.Policy
			if stored == nil || stored.Outcome != "allow" || stored.Capabilities[0] != "filesystem.read" {
				t.Fatalf("stored Tool Policy = %#v", stored)
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

func TestSessionStoresPreservePrefixManifest(t *testing.T) {
	t.Parallel()

	compiled, err := (agent.DefaultContextCompiler{}).Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: []agent.Message{{Role: agent.RoleUser, Text: "hello"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := agent.BuildPrefixManifest(
		agent.PrefixManifestOptions{Provider: "test", Model: "test-model"},
		compiled.Segments,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantEpoch := manifest.Epoch
	wantHash := manifest.Segments[0].ContentHash

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
			eventManifest := manifest
			eventManifest.Segments = append([]agent.SegmentFingerprint(nil), manifest.Segments...)
			if _, err := store.Append(context.Background(), "manifest", 0, []agent.Event{{
				Type:           agent.EventModelRequest,
				PrefixManifest: &eventManifest,
			}}); err != nil {
				t.Fatal(err)
			}
			eventManifest.Epoch = "changed"
			eventManifest.Segments[0].ContentHash = "changed"

			snapshot, err := store.Load(context.Background(), "manifest")
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Events) != 1 || snapshot.Events[0].PrefixManifest == nil {
				t.Fatalf("Snapshot Events = %#v", snapshot.Events)
			}
			stored := snapshot.Events[0].PrefixManifest
			if stored.Epoch != wantEpoch || stored.Segments[0].ContentHash != wantHash {
				t.Fatalf("stored Prefix Manifest = %#v", stored)
			}
		})
	}
}

func TestSessionStoresIsolateCachePlans(t *testing.T) {
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
			plan := &agent.CachePlan{
				Version:  1,
				FamilyID: "cache_" + strings.Repeat("a", 64),
				Mode:     agent.CacheModeExplicit,
				Breakpoints: []agent.CacheBreakpoint{{
					AfterSegmentID: "message/0000000000",
				}},
				Pricing:  &agent.CachePricing{Currency: "USD"},
				Forecast: &agent.CostForecast{Currency: "USD"},
			}
			if _, err := store.Append(context.Background(), "cache-plan", 0, []agent.Event{{
				Type:      agent.EventModelRequest,
				CachePlan: plan,
			}}); err != nil {
				t.Fatal(err)
			}
			plan.Breakpoints[0].AfterSegmentID = "changed"
			plan.Pricing.Currency = "changed"
			plan.Forecast.Currency = "changed"

			first, err := store.Load(context.Background(), "cache-plan")
			if err != nil {
				t.Fatal(err)
			}
			stored := first.Events[0].CachePlan
			if stored == nil || stored.Breakpoints[0].AfterSegmentID != "message/0000000000" ||
				stored.Pricing.Currency != "USD" || stored.Forecast.Currency != "USD" {
				t.Fatalf("stored Cache Plan = %#v", stored)
			}
			stored.Breakpoints[0].AfterSegmentID = "mutated snapshot"
			stored.Pricing.Currency = "mutated snapshot"

			second, err := store.Load(context.Background(), "cache-plan")
			if err != nil {
				t.Fatal(err)
			}
			reloaded := second.Events[0].CachePlan
			if reloaded.Breakpoints[0].AfterSegmentID != "message/0000000000" || reloaded.Pricing.Currency != "USD" {
				t.Fatalf("reloaded Cache Plan = %#v", reloaded)
			}
		})
	}
}

func TestSessionStoresPreserveProviderRateLimitWait(t *testing.T) {
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
			wait := &agent.ProviderRateLimitWaitInfo{
				Reason:                 agent.ProviderRateLimitWaitCooldown,
				MaxConcurrency:         2,
				RetryAfterMilliseconds: 1250,
			}
			if _, err := store.Append(context.Background(), "provider-wait", 0, []agent.Event{{
				Type:                  agent.EventProviderRateLimitWait,
				ProviderAttempt:       2,
				ProviderRateLimitWait: wait,
			}}); err != nil {
				t.Fatal(err)
			}
			wait.Reason = agent.ProviderRateLimitWaitConcurrency
			wait.MaxConcurrency = 99

			snapshot, err := store.Load(context.Background(), "provider-wait")
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Events) != 1 || snapshot.Events[0].ProviderRateLimitWait == nil {
				t.Fatalf("Snapshot Events = %#v", snapshot.Events)
			}
			stored := snapshot.Events[0].ProviderRateLimitWait
			if snapshot.Events[0].ProviderAttempt != 2 ||
				stored.Reason != agent.ProviderRateLimitWaitCooldown ||
				stored.MaxConcurrency != 2 || stored.RetryAfterMilliseconds != 1250 {
				t.Fatalf("stored Provider wait = %#v", snapshot.Events[0])
			}
		})
	}
}

func TestSessionStoresPreserveContextCheckpoint(t *testing.T) {
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
			checkpoint := agent.ContextCheckpoint{
				Version:            1,
				Generation:         2,
				SourceMessageCount: 3,
				SourceHash:         "sha256:" + strings.Repeat("1", 64),
				Ledger: &agent.ContextLedgerReference{
					Version: 1, Digest: "sha256:" + strings.Repeat("3", 64),
					SourceEventCount: 2, SourceHash: "sha256:" + strings.Repeat("4", 64), SessionRevision: 2,
				},
				Narrative: "checkpoint",
				Evidence: []agent.EvidenceObjectRef{{
					Digest:    "sha256:" + strings.Repeat("2", 64),
					Bytes:     10,
					MediaType: "application/json",
				}},
			}
			report := agent.ContextCompactionReport{
				Applied:       true,
				Reason:        "input_limit",
				OriginalBytes: 100,
				CompiledBytes: 40,
				Externalized:  append([]agent.EvidenceObjectRef(nil), checkpoint.Evidence...),
			}
			store := construct(t)
			if _, err := store.Append(context.Background(), "checkpoint", 0, []agent.Event{{
				Type:              agent.EventContextCompacted,
				ContextCheckpoint: &checkpoint,
				ContextCompaction: &report,
			}}); err != nil {
				t.Fatal(err)
			}
			checkpoint.Narrative = "mutated"
			checkpoint.Ledger.Digest = "mutated"
			report.Externalized[0].Digest = "mutated"
			snapshot, err := store.Load(context.Background(), "checkpoint")
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Checkpoint == nil || snapshot.Checkpoint.Narrative != "checkpoint" ||
				snapshot.Checkpoint.Ledger == nil || snapshot.Checkpoint.Ledger.Digest == "mutated" ||
				len(snapshot.EvidenceObjects) != 1 || snapshot.EvidenceObjects[0].Digest == "mutated" {
				t.Fatalf("Checkpoint Snapshot = %#v", snapshot)
			}
		})
	}
}

func TestJSONLStoreUsesDeltaPrefixManifestRecords(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := session.NewJSONLStore(root)
	if err != nil {
		t.Fatal(err)
	}
	segments := make([]agent.SegmentFingerprint, 64)
	for index := range segments {
		segments[index] = agent.SegmentFingerprint{
			ID:          fmt.Sprintf("stable/%04d", index),
			Kind:        agent.SegmentKindMessage,
			Version:     "1",
			ContentHash: "sha256:" + strings.Repeat(fmt.Sprintf("%x", index%16), 64),
			Bytes:       128,
			Stability:   agent.StabilityAppendOnly,
		}
	}
	var full bytes.Buffer
	fullEncoder := json.NewEncoder(&full)
	revision := uint64(0)
	for turn := 0; turn < 80; turn++ {
		segments = append(segments, agent.SegmentFingerprint{
			ID:          fmt.Sprintf("message/%04d", turn),
			Kind:        agent.SegmentKindMessage,
			Version:     "1",
			ContentHash: "sha256:" + strings.Repeat(fmt.Sprintf("%x", (turn+1)%16), 64),
			Bytes:       128,
			Stability:   agent.StabilityAppendOnly,
		})
		manifest := agent.PrefixManifest{
			Version:  1,
			Provider: "test",
			Model:    "model",
			Epoch:    fmt.Sprintf("epoch-%d", turn),
			Segments: append([]agent.SegmentFingerprint(nil), segments...),
		}
		event := agent.Event{Type: agent.EventModelRequest, PrefixManifest: &manifest}
		if _, err := store.Append(context.Background(), "delta", revision, []agent.Event{event}); err != nil {
			t.Fatal(err)
		}
		revision++
		if err := fullEncoder.Encode(struct {
			Event agent.Event `json:"event"`
		}{Event: event}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var persisted []byte
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".jsonl") {
			persisted, err = os.ReadFile(root + string(os.PathSeparator) + entry.Name())
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(persisted) == 0 || !bytes.Contains(persisted, []byte(`"prefix_manifest_delta"`)) {
		t.Fatalf("persisted JSONL does not contain Prefix Manifest deltas: %s", persisted)
	}
	if len(persisted)*3 >= full.Len() {
		t.Fatalf("delta JSONL size = %d, full records = %d", len(persisted), full.Len())
	}
	snapshot, err := store.Load(context.Background(), "delta")
	if err != nil {
		t.Fatal(err)
	}
	last := snapshot.Events[len(snapshot.Events)-1].PrefixManifest
	if last == nil || len(last.Segments) != len(segments) || last.Epoch != "epoch-79" {
		t.Fatalf("restored final Prefix Manifest = %#v", last)
	}
}

func TestJSONLStoreReadsFullPrefixManifestThenAppendsDelta(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := session.NewJSONLStore(root)
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "legacy-manifest"
	first := agent.PrefixManifest{
		Version:  1,
		Provider: "test",
		Model:    "model",
		Epoch:    "epoch-1",
		Segments: []agent.SegmentFingerprint{{
			ID:          "instructions",
			Kind:        agent.SegmentKindInstructions,
			Version:     "1",
			ContentHash: "sha256:" + strings.Repeat("1", 64),
			Bytes:       10,
			Stability:   agent.StabilityProject,
		}},
	}
	legacyRecord := struct {
		Event agent.Event `json:"event"`
	}{Event: agent.Event{
		Type:            agent.EventModelRequest,
		SessionID:       sessionID,
		SessionRevision: 1,
		PrefixManifest:  &first,
	}}
	encoded, err := json.Marshal(legacyRecord)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(sessionID))
	path := filepath.Join(root, fmt.Sprintf("%x.jsonl", digest))
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	second := first
	second.Epoch = "epoch-2"
	second.Segments = append(append([]agent.SegmentFingerprint(nil), first.Segments...), agent.SegmentFingerprint{
		ID:          "message/0000000000",
		Kind:        agent.SegmentKindMessage,
		Version:     "1",
		ContentHash: "sha256:" + strings.Repeat("2", 64),
		Bytes:       20,
		Stability:   agent.StabilityAppendOnly,
	})
	if _, err := store.Append(context.Background(), sessionID, 1, []agent.Event{{
		Type:           agent.EventModelRequest,
		PrefixManifest: &second,
	}}); err != nil {
		t.Fatal(err)
	}

	snapshot, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 2 || len(snapshot.Events) != 2 ||
		snapshot.Events[0].PrefixManifest == nil || snapshot.Events[0].PrefixManifest.Epoch != "epoch-1" ||
		snapshot.Events[1].PrefixManifest == nil || snapshot.Events[1].PrefixManifest.Epoch != "epoch-2" ||
		len(snapshot.Events[1].PrefixManifest.Segments) != 2 {
		t.Fatalf("restored legacy and delta Manifests = %#v", snapshot.Events)
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

func TestSessionStoresReplaySteeringAndFollowUpBoundaries(t *testing.T) {
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
			initial := agent.Message{Role: agent.RoleUser, Text: "initial"}
			first := agent.Message{Role: agent.RoleAssistant, Text: "first answer"}
			steering := agent.Message{Role: agent.RoleUser, Text: "steering"}
			second := agent.Message{Role: agent.RoleAssistant, Text: "second answer"}
			followUp := agent.Message{Role: agent.RoleUser, Text: "follow up"}
			third := agent.Message{Role: agent.RoleAssistant, Text: "third answer"}
			events := []agent.Event{
				{RunID: "run-first", Sequence: 1, Type: agent.EventRunStarted},
				{RunID: "run-first", Sequence: 2, Type: agent.EventUserMessageAdded, Message: &initial},
				{RunID: "run-first", Sequence: 3, Type: agent.EventMessageCompleted, Message: &first},
				{
					RunID: "run-first", Sequence: 4, Type: agent.EventUserMessageAdded,
					UserMessageOrigin: agent.UserMessageOriginSteering,
					Message:           &steering,
				},
				{RunID: "run-first", Sequence: 5, Type: agent.EventMessageCompleted, Message: &second},
				{RunID: "run-first", Sequence: 6, Type: agent.EventRunCompleted},
				{RunID: "run-follow-up", Sequence: 1, Type: agent.EventRunStarted},
				{RunID: "run-follow-up", Sequence: 2, Type: agent.EventUserMessageAdded, Message: &followUp},
				{RunID: "run-follow-up", Sequence: 3, Type: agent.EventMessageCompleted, Message: &third},
				{RunID: "run-follow-up", Sequence: 4, Type: agent.EventRunCompleted},
			}
			revision, err := store.Append(context.Background(), "steering-replay", 0, events)
			if err != nil || revision != uint64(len(events)) {
				t.Fatalf("Append() = %d, %v", revision, err)
			}
			snapshot, err := store.Load(context.Background(), "steering-replay")
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Events) != len(events) || snapshot.Revision != uint64(len(events)) {
				t.Fatalf("Snapshot identity = revision %d Events %d", snapshot.Revision, len(snapshot.Events))
			}
			for index, event := range snapshot.Events {
				if event.SessionRevision != uint64(index+1) {
					t.Fatalf("Event[%d] Session revision = %d", index, event.SessionRevision)
				}
			}
			if got := snapshot.Events[3]; got.RunID != "run-first" || got.Sequence != 4 ||
				got.UserMessageOrigin != agent.UserMessageOriginSteering || got.Message == nil || got.Message.Text != "steering" {
				t.Fatalf("replayed steering Event = %#v", got)
			}
			if got := snapshot.Events[6]; got.RunID != "run-follow-up" || got.Sequence != 1 || got.Type != agent.EventRunStarted {
				t.Fatalf("replayed follow-up boundary = %#v", got)
			}
			wantMessages := []string{"initial", "first answer", "steering", "second answer", "follow up", "third answer"}
			if len(snapshot.Messages) != len(wantMessages) {
				t.Fatalf("Snapshot Messages = %#v", snapshot.Messages)
			}
			for index, want := range wantMessages {
				if snapshot.Messages[index].Text != want {
					t.Fatalf("Message[%d] = %q, want %q", index, snapshot.Messages[index].Text, want)
				}
			}
		})
	}
}
