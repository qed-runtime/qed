package agent

import (
	"context"
	"reflect"
	"testing"
)

func TestCheckpointHierarchySeparatesSessionTaskAndEpisode(t *testing.T) {
	t.Parallel()

	messages := make([]Message, 8)
	for index := range messages {
		messages[index] = Message{Role: RoleUser, Text: string(rune('a' + index))}
	}
	events := []Event{{RunID: "run-old", Sequence: 1, Type: EventRunStarted}}
	for index := 0; index < 4; index++ {
		message := messages[index]
		events = append(events, Event{
			RunID: "run-old", Sequence: uint64(index + 2), Type: EventUserMessageAdded, Message: &message,
		})
	}
	events = append(events, Event{RunID: "run-old", Sequence: 6, Type: EventRunCompleted})
	events = append(events, Event{RunID: "run-current", Sequence: 1, Type: EventRunStarted})
	for index := 4; index < len(messages); index++ {
		message := messages[index]
		events = append(events, Event{
			RunID: "run-current", Sequence: uint64(index - 2), Type: EventUserMessageAdded, Message: &message,
		})
	}
	ledger, err := BuildContextLedger(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	layers, err := buildCheckpointLayers(context.Background(), messages, events, &ledger, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := []ContextCheckpointLayer{
		{Level: ContextCheckpointLevelSessionSynopsis, SourceMessageEnd: 4},
		{Level: ContextCheckpointLevelTask, SourceMessageEnd: 6},
		{Level: ContextCheckpointLevelEpisode, SourceMessageEnd: 7},
	}
	if !reflect.DeepEqual(layers, want) {
		t.Fatalf("Checkpoint layers = %#v, want %#v", layers, want)
	}
}

func TestCheckpointHierarchyDoesNotSplitToolTransaction(t *testing.T) {
	t.Parallel()

	messages := []Message{
		{Role: RoleUser, Text: "request"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "read"}}},
		{Role: RoleTool, ToolCallID: "call-1", ToolName: "read", Text: "result"},
		{Role: RoleUser, Text: "continue"},
		{Role: RoleAssistant, Text: "working"},
	}
	layers, err := buildCheckpointLayers(context.Background(), messages, nil, nil, len(messages), 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []ContextCheckpointLayer{
		{Level: ContextCheckpointLevelTask, SourceMessageEnd: 1},
		{Level: ContextCheckpointLevelEpisode, SourceMessageEnd: 5},
	}
	if !reflect.DeepEqual(layers, want) {
		t.Fatalf("Checkpoint layers = %#v, want %#v", layers, want)
	}

	checkpoint := ContextCheckpoint{
		Version: ContextCheckpointVersion, SourceMessageCount: len(messages),
		Layers: []ContextCheckpointLayer{
			{Level: ContextCheckpointLevelTask, SourceMessageEnd: 2},
			{Level: ContextCheckpointLevelEpisode, SourceMessageEnd: 5},
		},
	}
	if err := validateCheckpointHierarchy(context.Background(), checkpoint, messages, nil, nil); err == nil {
		t.Fatal("Checkpoint hierarchy accepted a Tool transaction split")
	}
}

func TestCheckpointModelViewSelectsOnlyPopulatedLevels(t *testing.T) {
	t.Parallel()

	checkpoint := ContextCheckpoint{
		Version:            ContextCheckpointVersion,
		Generation:         3,
		SourceMessageCount: 7,
		Layers: []ContextCheckpointLayer{
			{Level: ContextCheckpointLevelSessionSynopsis, SourceMessageEnd: 4},
			{Level: ContextCheckpointLevelTask, SourceMessageEnd: 6},
			{Level: ContextCheckpointLevelEpisode, SourceMessageEnd: 7},
		},
		Facts: []CheckpointFact{{SourceMessage: 1, Summary: "session fact"}},
		Goal:  &CheckpointFact{SourceMessage: 6, Summary: "episode goal"},
	}
	view := checkpointModelView(checkpoint)
	if len(view.Layers) != 2 ||
		view.Layers[0].Level != ContextCheckpointLevelSessionSynopsis ||
		view.Layers[1].Level != ContextCheckpointLevelEpisode {
		t.Fatalf("selected model layers = %#v", view.Layers)
	}
	if got := checkpointModelLevels(&checkpoint); !reflect.DeepEqual(got, []ContextCheckpointLevel{
		ContextCheckpointLevelSessionSynopsis,
		ContextCheckpointLevelEpisode,
	}) {
		t.Fatalf("model levels = %#v", got)
	}
}
