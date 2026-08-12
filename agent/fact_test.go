package agent_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"strings"
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/session"
)

func TestBuildContextLedgerAppliesFactLifecycleAcrossRuns(t *testing.T) {
	t.Parallel()

	firstSource := agent.ContextLedgerEventRef{RunID: "run-first", Sequence: 2, SessionRevision: 2}
	firstID, err := agent.ConstraintFactID(firstSource)
	if err != nil {
		t.Fatal(err)
	}
	replacementSource := agent.ContextLedgerEventRef{RunID: "run-replace", Sequence: 2, SessionRevision: 5}
	replacementID, err := agent.ConstraintFactID(replacementSource)
	if err != nil {
		t.Fatal(err)
	}
	events := factLifecycleEvents(firstID, replacementID)
	ledger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	if ledger.Version != 2 || len(ledger.Constraints) != 2 {
		t.Fatalf("Context Ledger = %#v", ledger)
	}
	first := ledger.Constraints[0]
	if first.ID != firstID || first.SourceMessage != 0 || first.State != agent.FactSuperseded || first.SupersededBy != replacementID ||
		first.StateSource != (agent.ContextLedgerEventRef{RunID: "run-replace", Sequence: 2, SessionRevision: 5}) ||
		len(first.Sources) != 2 {
		t.Fatalf("superseded Fact = %#v", first)
	}
	replacement := ledger.Constraints[1]
	if replacement.ID != replacementID || replacement.SourceMessage != 1 || replacement.State != agent.FactResolved ||
		replacement.StateSource != (agent.ContextLedgerEventRef{RunID: "run-resolve", Sequence: 2, SessionRevision: 8}) ||
		len(replacement.Supersedes) != 1 || replacement.Supersedes[0] != firstID || len(replacement.Sources) != 2 {
		t.Fatalf("resolved replacement Fact = %#v", replacement)
	}
	if err := agent.ValidateContextLedger(context.Background(), ledger, events); err != nil {
		t.Fatalf("ValidateContextLedger: %v", err)
	}
}

func TestBuildContextLedgerRejectsInvalidFactLifecycle(t *testing.T) {
	t.Parallel()

	firstRef := agent.ContextLedgerEventRef{RunID: "run-fact", Sequence: 2}
	firstID, err := agent.ConstraintFactID(firstRef)
	if err != nil {
		t.Fatal(err)
	}
	futureID, err := agent.ConstraintFactID(agent.ContextLedgerEventRef{RunID: "run-fact", Sequence: 4})
	if err != nil {
		t.Fatal(err)
	}
	base := []agent.Event{
		{RunID: "run-fact", Sequence: 1, Type: agent.EventRunStarted},
		{RunID: "run-fact", Sequence: 2, Type: agent.EventUserMessageAdded, Message: &agent.Message{Role: agent.RoleUser, Text: "first"}},
	}
	tests := []struct {
		name      string
		directive *agent.FactLifecycleDirective
		message   agent.Message
		want      string
	}{
		{
			name: "unsupported action", message: agent.Message{Role: agent.RoleUser, Text: "change"},
			directive: &agent.FactLifecycleDirective{Action: "replace", Targets: []string{firstID}},
			want:      "unsupported action",
		},
		{
			name: "duplicate target", message: agent.Message{Role: agent.RoleUser, Text: "change"},
			directive: &agent.FactLifecycleDirective{Action: agent.FactLifecycleSupersede, Targets: []string{firstID, firstID}},
			want:      "duplicated",
		},
		{
			name: "future target", message: agent.Message{Role: agent.RoleUser, Text: "change"},
			directive: &agent.FactLifecycleDirective{Action: agent.FactLifecycleSupersede, Targets: []string{futureID}},
			want:      "earlier Event prefix",
		},
		{
			name: "non-user directive", message: agent.Message{Role: agent.RoleAssistant, Text: "change"},
			directive: &agent.FactLifecycleDirective{Action: agent.FactLifecycleResolve, Targets: []string{firstID}},
			want:      "requires a user Message",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			events := append([]agent.Event(nil), base...)
			message := test.message
			events = append(events, agent.Event{
				RunID: "run-fact", Sequence: 3, Type: agent.EventUserMessageAdded,
				Message: &message, FactDirective: test.directive,
			})
			_, err := agent.BuildContextLedger(context.Background(), events)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("BuildContextLedger() error = %v, want %q", err, test.want)
			}
		})
	}

	resolved := append([]agent.Event(nil), base...)
	resolved = append(resolved,
		agent.Event{
			RunID: "run-fact", Sequence: 3, Type: agent.EventUserMessageAdded,
			Message:       &agent.Message{Role: agent.RoleUser, Text: "done"},
			FactDirective: &agent.FactLifecycleDirective{Action: agent.FactLifecycleResolve, Targets: []string{firstID}},
		},
		agent.Event{
			RunID: "run-fact", Sequence: 4, Type: agent.EventUserMessageAdded,
			Message:       &agent.Message{Role: agent.RoleUser, Text: "again"},
			FactDirective: &agent.FactLifecycleDirective{Action: agent.FactLifecycleResolve, Targets: []string{firstID}},
		},
	)
	if _, err := agent.BuildContextLedger(context.Background(), resolved); err == nil || !strings.Contains(err.Error(), "instead of active") {
		t.Fatalf("duplicate transition error = %v", err)
	}
}

func TestBuildContextLedgerSupersedesMultipleFactsInDeclaredOrder(t *testing.T) {
	t.Parallel()

	firstID, err := agent.ConstraintFactID(agent.ContextLedgerEventRef{RunID: "run-many", Sequence: 2})
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := agent.ConstraintFactID(agent.ContextLedgerEventRef{RunID: "run-many", Sequence: 3})
	if err != nil {
		t.Fatal(err)
	}
	replacementID, err := agent.ConstraintFactID(agent.ContextLedgerEventRef{RunID: "run-many", Sequence: 4})
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := agent.BuildContextLedger(context.Background(), []agent.Event{
		{RunID: "run-many", Sequence: 1, Type: agent.EventRunStarted},
		{RunID: "run-many", Sequence: 2, Type: agent.EventUserMessageAdded, Message: &agent.Message{Role: agent.RoleUser, Text: "first"}},
		{RunID: "run-many", Sequence: 3, Type: agent.EventUserMessageAdded, Message: &agent.Message{Role: agent.RoleUser, Text: "second"}},
		{
			RunID: "run-many", Sequence: 4, Type: agent.EventUserMessageAdded,
			Message: &agent.Message{Role: agent.RoleUser, Text: "combined"},
			FactDirective: &agent.FactLifecycleDirective{
				Action: agent.FactLifecycleSupersede, Targets: []string{secondID, firstID},
			},
		},
		{RunID: "run-many", Sequence: 5, Type: agent.EventRunCompleted},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Constraints) != 3 || ledger.Constraints[0].SupersededBy != replacementID ||
		ledger.Constraints[1].SupersededBy != replacementID ||
		len(ledger.Constraints[2].Supersedes) != 2 ||
		ledger.Constraints[2].Supersedes[0] != secondID || ledger.Constraints[2].Supersedes[1] != firstID {
		t.Fatalf("multi-target Constraint Ledger = %#v", ledger.Constraints)
	}
}

func TestBuildContextLedgerAppliesFactLifecycleToLegacyEvents(t *testing.T) {
	t.Parallel()

	targetID, err := agent.ConstraintFactID(agent.ContextLedgerEventRef{SessionRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := agent.BuildContextLedger(context.Background(), []agent.Event{
		{
			SessionID: "legacy-fact", SessionRevision: 1, Type: agent.EventUserMessageAdded,
			Message: &agent.Message{Role: agent.RoleUser, Text: "old"},
		},
		{
			SessionID: "legacy-fact", SessionRevision: 2, Type: agent.EventUserMessageAdded,
			Message: &agent.Message{Role: agent.RoleUser, Text: "new"},
			FactDirective: &agent.FactLifecycleDirective{
				Action: agent.FactLifecycleSupersede, Targets: []string{targetID},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Constraints) != 2 || ledger.Constraints[0].State != agent.FactSuperseded ||
		ledger.Constraints[1].State != agent.FactActive || ledger.Constraints[1].SourceMessage != 1 {
		t.Fatalf("legacy Constraint Ledger = %#v", ledger.Constraints)
	}
}

func TestRuntimeRejectsMalformedFactDirectiveSynchronously(t *testing.T) {
	t.Parallel()

	validID, err := agent.ConstraintFactID(agent.ContextLedgerEventRef{RunID: "run-target", Sequence: 2})
	if err != nil {
		t.Fatal(err)
	}
	tooMany := make([]string, agent.MaxFactLifecycleTargets+1)
	for index := range tooMany {
		tooMany[index], err = agent.ConstraintFactID(agent.ContextLedgerEventRef{
			RunID: "run-target", Sequence: uint64(index + 1),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name      string
		text      string
		directive *agent.FactLifecycleDirective
	}{
		{
			name: "empty target list", text: "change",
			directive: &agent.FactLifecycleDirective{Action: agent.FactLifecycleResolve},
		},
		{
			name: "too many targets", text: "change",
			directive: &agent.FactLifecycleDirective{Action: agent.FactLifecycleResolve, Targets: tooMany},
		},
		{
			name: "malformed target", text: "change",
			directive: &agent.FactLifecycleDirective{Action: agent.FactLifecycleResolve, Targets: []string{"constraint_bad"}},
		},
		{
			name: "empty text", text: " ",
			directive: &agent.FactLifecycleDirective{Action: agent.FactLifecycleResolve, Targets: []string{validID}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runtime, err := agent.NewRuntime(agent.Options{Provider: &scriptedProvider{}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = runtime.Run(context.Background(), agent.RunRequest{Input: []agent.Message{{
				Role: agent.RoleUser, Text: test.text, FactDirective: test.directive,
			}}})
			if !errors.Is(err, agent.ErrInvalidFactDirective) {
				t.Fatalf("Runtime.Run() error = %v, want ErrInvalidFactDirective", err)
			}
		})
	}
}

func TestSteerRejectsMalformedFactDirectiveWithBothClassifications(t *testing.T) {
	t.Parallel()

	var handle *agent.RunHandle
	err := handle.Steer(agent.Message{
		Role: agent.RoleUser,
		Text: "change",
		FactDirective: &agent.FactLifecycleDirective{
			Action: agent.FactLifecycleSupersede,
		},
	})
	if !errors.Is(err, agent.ErrInvalidSteeringMessage) || !errors.Is(err, agent.ErrInvalidFactDirective) {
		t.Fatalf("Steer() error = %v", err)
	}
}

func TestConstraintFactIDValidatesSourceIdentity(t *testing.T) {
	t.Parallel()

	modern, err := agent.ConstraintFactID(agent.ContextLedgerEventRef{RunID: "run", Sequence: 3})
	if err != nil || !strings.HasPrefix(modern, "constraint_") {
		t.Fatalf("modern ID = %q, %v", modern, err)
	}
	legacy, err := agent.ConstraintFactID(agent.ContextLedgerEventRef{SessionRevision: 4})
	if err != nil || !strings.HasPrefix(legacy, "constraint_") || legacy == modern {
		t.Fatalf("legacy ID = %q, %v", legacy, err)
	}
	for _, source := range []agent.ContextLedgerEventRef{
		{},
		{RunID: "run"},
		{Sequence: 1, SessionRevision: 1},
	} {
		if _, err := agent.ConstraintFactID(source); err == nil {
			t.Fatalf("ConstraintFactID(%#v) succeeded", source)
		}
	}
}

func TestRuntimePersistsFactDirectiveOutsideProviderHistory(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, Text: "first"}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "second"}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "third"}},
	}}
	store := session.NewMemoryStore()
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider, SessionStore: store})
	if err != nil {
		t.Fatal(err)
	}

	_, first, err := runFactRequest(t, runtime, agent.RunRequest{
		SessionID: "fact-session", Input: []agent.Message{{Role: agent.RoleUser, Text: "use sqlite"}},
	})
	if err != nil || first.ContextLedger == nil || len(first.ContextLedger.Constraints) != 1 {
		t.Fatalf("first Run = %#v, %v", first, err)
	}
	firstID := first.ContextLedger.Constraints[0].ID
	secondEvents, second, err := runFactRequest(t, runtime, agent.RunRequest{
		SessionID: "fact-session",
		Input: []agent.Message{{
			Role: agent.RoleUser, Text: "use postgres",
			FactDirective: &agent.FactLifecycleDirective{
				Action: agent.FactLifecycleSupersede, Targets: []string{firstID},
			},
		}},
	})
	if err != nil || second.ContextLedger == nil || len(second.ContextLedger.Constraints) != 2 {
		t.Fatalf("second Run = %#v, %v", second, err)
	}
	replacementID := second.ContextLedger.Constraints[1].ID
	if second.ContextLedger.Constraints[1].SourceMessage != 2 {
		t.Fatalf("replacement source Message = %d, want 2", second.ContextLedger.Constraints[1].SourceMessage)
	}
	_, third, err := runFactRequest(t, runtime, agent.RunRequest{
		SessionID: "fact-session",
		Input: []agent.Message{{
			Role: agent.RoleUser, Text: "database choice is complete",
			FactDirective: &agent.FactLifecycleDirective{
				Action: agent.FactLifecycleResolve, Targets: []string{replacementID},
			},
		}},
	})
	if err != nil || third.ContextLedger == nil || third.ContextLedger.Constraints[1].State != agent.FactResolved {
		t.Fatalf("third Run = %#v, %v", third, err)
	}

	foundDirective := false
	for _, event := range secondEvents {
		if event.Type != agent.EventUserMessageAdded || event.FactDirective == nil {
			continue
		}
		foundDirective = true
		if event.Message == nil || event.Message.FactDirective != nil || event.FactDirective.Targets[0] != firstID {
			t.Fatalf("Fact lifecycle Event = %#v", event)
		}
	}
	if !foundDirective {
		t.Fatal("second Run emitted no Fact lifecycle Event")
	}
	for _, request := range provider.Requests() {
		for _, message := range request.Messages {
			if message.FactDirective != nil {
				t.Fatalf("Provider received Fact lifecycle directive: %#v", request.Messages)
			}
		}
	}
	for _, message := range third.Messages {
		if message.FactDirective != nil {
			t.Fatalf("RunResult retained Fact lifecycle directive: %#v", third.Messages)
		}
	}
}

func TestRuntimeRejectsFactDirectiveIntroducedByContextCompiler(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{{
		message: agent.Message{Role: agent.RoleAssistant, Text: "unused"},
	}}}
	target, err := agent.ConstraintFactID(agent.ContextLedgerEventRef{RunID: "compiler-target", Sequence: 2})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewRuntime(agent.Options{
		Provider: provider,
		ContextCompiler: contextCompilerFunc(func(_ context.Context, request agent.ContextCompileRequest) (agent.CompiledContext, error) {
			request.ModelRequest.Messages[0].FactDirective = &agent.FactLifecycleDirective{
				Action:  agent.FactLifecycleResolve,
				Targets: []string{target},
			}
			return agent.CompiledContext{ModelRequest: request.ModelRequest}, nil
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
	if runErr == nil || !strings.Contains(runErr.Error(), "host-only Fact lifecycle state") {
		t.Fatalf("Run error = %v", runErr)
	}
	if result.Status != agent.RunStatusFailed || result.ProviderCalls != 0 || len(provider.Requests()) != 0 {
		t.Fatalf("failed Run = %#v, Provider requests = %d", result, len(provider.Requests()))
	}
	if len(events) != 3 || events[2].Type != agent.EventRunFailed {
		t.Fatalf("Events = %#v", events)
	}
}

func TestRuntimeRejectsFactDirectiveReturnedByProvider(t *testing.T) {
	t.Parallel()

	target, err := agent.ConstraintFactID(agent.ContextLedgerEventRef{RunID: "provider-target", Sequence: 2})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []providerResponse{{
		message: agent.Message{
			Role: agent.RoleAssistant,
			Text: "invalid host metadata",
			FactDirective: &agent.FactLifecycleDirective{
				Action:  agent.FactLifecycleResolve,
				Targets: []string{target},
			},
		},
	}}}
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
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
	if runErr == nil || !strings.Contains(runErr.Error(), "returned host-only Fact lifecycle state") {
		t.Fatalf("Run error = %v", runErr)
	}
	if result.Status != agent.RunStatusFailed || result.ProviderCalls != 1 {
		t.Fatalf("failed Run = %#v", result)
	}
	for _, message := range result.Messages {
		if message.FactDirective != nil {
			t.Fatalf("RunResult retained Provider Fact directive: %#v", result.Messages)
		}
	}
}

func TestRuntimeRejectsInvalidFactDirectiveBeforeObservableCommit(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{responses: []providerResponse{
		{message: agent.Message{Role: agent.RoleAssistant, Text: "first"}},
		{message: agent.Message{Role: agent.RoleAssistant, Text: "unused"}},
	}}
	store := session.NewMemoryStore()
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider, SessionStore: store})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runFactRequest(t, runtime, agent.RunRequest{
		SessionID: "invalid-fact", Input: []agent.Message{{Role: agent.RoleUser, Text: "first"}},
	}); err != nil {
		t.Fatal(err)
	}
	missing, err := agent.ConstraintFactID(agent.ContextLedgerEventRef{RunID: "missing-run", Sequence: 2})
	if err != nil {
		t.Fatal(err)
	}
	events, result, runErr := runFactRequest(t, runtime, agent.RunRequest{
		SessionID: "invalid-fact",
		Input: []agent.Message{{
			Role: agent.RoleUser, Text: "invalid replacement",
			FactDirective: &agent.FactLifecycleDirective{
				Action: agent.FactLifecycleSupersede, Targets: []string{missing},
			},
		}},
	})
	if runErr == nil || !errors.Is(runErr, agent.ErrInvalidFactDirective) || result.Status != agent.RunStatusFailed {
		t.Fatalf("invalid transition Run = %#v, %v", result, runErr)
	}
	for _, message := range result.Messages {
		if message.Text == "invalid replacement" {
			t.Fatalf("uncommitted Fact transition entered RunResult.Messages: %#v", result.Messages)
		}
	}
	for _, event := range events {
		if event.FactDirective != nil {
			t.Fatalf("invalid Fact directive became observable: %#v", event)
		}
	}
	snapshot, err := store.Load(context.Background(), "invalid-fact")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range snapshot.Events {
		if event.FactDirective != nil {
			t.Fatalf("invalid Fact directive was persisted: %#v", event)
		}
	}
	if got := len(provider.Requests()); got != 1 {
		t.Fatalf("Provider request count = %d, want 1", got)
	}

	_, err = runtime.Run(context.Background(), agent.RunRequest{Input: []agent.Message{{
		Role: agent.RoleAssistant, Text: "invalid",
		FactDirective: &agent.FactLifecycleDirective{
			Action: agent.FactLifecycleResolve, Targets: []string{missing},
		},
	}}})
	if err == nil || !errors.Is(err, agent.ErrInvalidFactDirective) {
		t.Fatalf("Run() malformed input error = %v", err)
	}
}

func TestRuntimeAppliesSteeringFactDirectiveAtSafeBoundary(t *testing.T) {
	t.Parallel()

	provider := newSteeringGateProvider(
		agent.Message{Role: agent.RoleAssistant, Text: "first", StopReason: agent.StopReasonEndTurn},
		agent.Message{Role: agent.RoleAssistant, Text: "second", StopReason: agent.StopReasonEndTurn},
	)
	runtime, err := agent.NewRuntime(agent.Options{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "old requirement"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitSteeringSignal(t, provider.entered, "first Provider call")

	var events []agent.Event
	var targetID string
	for targetID == "" {
		select {
		case event := <-handle.Events():
			events = append(events, event)
			if event.Type == agent.EventUserMessageAdded {
				targetID, err = agent.ConstraintFactID(agent.ContextLedgerEventRef{RunID: event.RunID, Sequence: event.Sequence})
				if err != nil {
					t.Fatal(err)
				}
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for initial user Event")
		}
	}
	if err := steerWithin(t, handle, agent.Message{
		Role: agent.RoleUser, Text: "new requirement",
		FactDirective: &agent.FactLifecycleDirective{
			Action: agent.FactLifecycleSupersede, Targets: []string{targetID},
		},
	}); err != nil {
		t.Fatalf("Steer() error = %v", err)
	}
	close(provider.release)
	for event := range handle.Events() {
		events = append(events, event)
	}
	result, err := handle.Wait()
	if err != nil || result.ContextLedger == nil || len(result.ContextLedger.Constraints) != 2 ||
		result.ContextLedger.Constraints[0].State != agent.FactSuperseded ||
		result.ContextLedger.Constraints[1].State != agent.FactActive {
		t.Fatalf("steered Run = %#v, %v", result, err)
	}
	found := false
	for _, event := range events {
		if event.FactDirective != nil {
			found = true
			if event.UserMessageOrigin != agent.UserMessageOriginSteering || event.Message.FactDirective != nil {
				t.Fatalf("steering lifecycle Event = %#v", event)
			}
		}
	}
	if !found {
		t.Fatal("steering lifecycle Event was not emitted")
	}
}

func TestBuildContextLedgerAcceptsV1CheckpointReference(t *testing.T) {
	t.Parallel()

	prefixEvents := []agent.Event{
		{RunID: "run-v1", Sequence: 1, SessionID: "v1-session", SessionRevision: 1, Type: agent.EventRunStarted},
		{
			RunID: "run-v1", Sequence: 2, SessionID: "v1-session", SessionRevision: 2,
			Type: agent.EventUserMessageAdded, Message: &agent.Message{Role: agent.RoleUser, Text: "old event"},
		},
	}
	prefix, err := agent.BuildContextLedger(context.Background(), prefixEvents)
	if err != nil {
		t.Fatal(err)
	}
	reference := legacyContextLedgerReference(t, prefix)
	checkpoint := &agent.ContextCheckpoint{Ledger: &reference}
	events := append(append([]agent.Event(nil), prefixEvents...),
		agent.Event{
			RunID: "run-v1", Sequence: 3, SessionID: "v1-session", SessionRevision: 3,
			Type:              agent.EventContextCompacted,
			ContextCheckpoint: checkpoint,
			ContextCompaction: &agent.ContextCompactionReport{Applied: true, Reason: "legacy"},
		},
		agent.Event{
			RunID: "run-v1", Sequence: 4, SessionID: "v1-session", SessionRevision: 4,
			Type: agent.EventRunCompleted,
		},
	)
	ledger, err := agent.BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.CheckpointReferences) != 1 || ledger.CheckpointReferences[0] != reference {
		t.Fatalf("Checkpoint references = %#v", ledger.CheckpointReferences)
	}

	tampered := cloneLedgerEvents(events)
	tampered[2].ContextCheckpoint.Ledger.Digest = "sha256:" + strings.Repeat("f", 64)
	if _, err := agent.BuildContextLedger(context.Background(), tampered); err == nil {
		t.Fatal("BuildContextLedger accepted changed v1 Checkpoint reference")
	}
}

func factLifecycleEvents(firstID, replacementID string) []agent.Event {
	events := []agent.Event{
		{RunID: "run-first", Sequence: 1, Type: agent.EventRunStarted},
		{RunID: "run-first", Sequence: 2, Type: agent.EventUserMessageAdded, Message: &agent.Message{Role: agent.RoleUser, Text: "use sqlite"}},
		{RunID: "run-first", Sequence: 3, Type: agent.EventRunCompleted},
		{RunID: "run-replace", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "run-replace", Sequence: 2, Type: agent.EventUserMessageAdded,
			UserMessageOrigin: agent.UserMessageOriginSteering,
			Message:           &agent.Message{Role: agent.RoleUser, Text: "use postgres"},
			FactDirective: &agent.FactLifecycleDirective{
				Action: agent.FactLifecycleSupersede, Targets: []string{firstID},
			},
		},
		{RunID: "run-replace", Sequence: 3, Type: agent.EventRunCompleted},
		{RunID: "run-resolve", Sequence: 1, Type: agent.EventRunStarted},
		{
			RunID: "run-resolve", Sequence: 2, Type: agent.EventUserMessageAdded,
			Message: &agent.Message{Role: agent.RoleUser, Text: "database migration complete"},
			FactDirective: &agent.FactLifecycleDirective{
				Action: agent.FactLifecycleResolve, Targets: []string{replacementID},
			},
		},
		{RunID: "run-resolve", Sequence: 3, Type: agent.EventRunCompleted},
	}
	baseTime := time.Date(2026, time.August, 12, 0, 0, 0, 0, time.UTC)
	for index := range events {
		events[index].SessionID = "fact-session"
		events[index].SessionRevision = uint64(index + 1)
		events[index].Time = baseTime.Add(time.Duration(index) * time.Second)
	}
	return events
}

func runFactRequest(t *testing.T, runtime *agent.Runtime, request agent.RunRequest) ([]agent.Event, agent.RunResult, error) {
	t.Helper()
	handle, err := runtime.Run(context.Background(), request)
	if err != nil {
		return nil, agent.RunResult{}, err
	}
	return collectRun(handle)
}

func legacyContextLedgerReference(t *testing.T, ledger agent.ContextLedger) agent.ContextLedgerReference {
	t.Helper()
	type legacyConstraint struct {
		ID          string                        `json:"id"`
		Kind        agent.ConstraintLedgerKind    `json:"kind"`
		Text        string                        `json:"text"`
		ContentHash string                        `json:"content_hash"`
		Origin      agent.UserMessageOrigin       `json:"origin,omitempty"`
		Sources     []agent.ContextLedgerEventRef `json:"sources"`
	}
	type legacyLedger struct {
		Version              uint32                         `json:"version"`
		SessionID            string                         `json:"session_id,omitempty"`
		SessionRevision      uint64                         `json:"session_revision,omitempty"`
		SourceEventCount     int                            `json:"source_event_count"`
		SourceHash           string                         `json:"source_hash"`
		Digest               string                         `json:"digest"`
		Sources              []agent.ContextLedgerSource    `json:"sources"`
		CheckpointReferences []agent.ContextLedgerReference `json:"checkpoint_references"`
		Artifacts            []agent.ArtifactLedgerEntry    `json:"artifacts"`
		Executions           []agent.ExecutionLedgerEntry   `json:"executions"`
		Constraints          []legacyConstraint             `json:"constraints"`
		Policies             []agent.PolicyLedgerEntry      `json:"policies"`
		Tasks                []agent.TaskLedgerEntry        `json:"tasks"`
	}
	constraints := make([]legacyConstraint, len(ledger.Constraints))
	for index, entry := range ledger.Constraints {
		constraints[index] = legacyConstraint{
			ID: entry.ID, Kind: entry.Kind, Text: entry.Text, ContentHash: entry.ContentHash,
			Origin: entry.Origin, Sources: entry.Sources[:1],
		}
	}
	value := legacyLedger{
		Version: 1, SessionID: ledger.SessionID, SessionRevision: ledger.SessionRevision,
		SourceEventCount: ledger.SourceEventCount, SourceHash: ledger.SourceHash,
		Sources: ledger.Sources, CheckpointReferences: ledger.CheckpointReferences,
		Artifacts: ledger.Artifacts, Executions: ledger.Executions, Constraints: constraints,
		Policies: ledger.Policies, Tasks: ledger.Tasks,
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	writeLegacyHashPart(digest, []byte("qed.context.ledger.v1"))
	writeLegacyHashPart(digest, encoded)
	return agent.ContextLedgerReference{
		Version: 1, Digest: "sha256:" + hex.EncodeToString(digest.Sum(nil)),
		SourceEventCount: ledger.SourceEventCount, SourceHash: ledger.SourceHash,
		SessionRevision: ledger.SessionRevision,
	}
}

func writeLegacyHashPart(writer hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = writer.Write(value)
}
