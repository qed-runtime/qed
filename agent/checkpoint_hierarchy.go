package agent

import (
	"context"
	"errors"
	"fmt"
)

type contextCheckpointModelView struct {
	Version            uint32                       `json:"version"`
	Generation         uint64                       `json:"generation"`
	SourceMessageCount int                          `json:"source_message_count"`
	Layers             []contextCheckpointLayerView `json:"layers"`
	Narrative          string                       `json:"narrative"`
	Evidence           []EvidenceObjectRef          `json:"evidence"`
}

type contextCheckpointLayerView struct {
	Level              ContextCheckpointLevel `json:"level"`
	SourceMessageStart int                    `json:"start"`
	SourceMessageEnd   int                    `json:"end"`
	Goal               *CheckpointFact        `json:"goal,omitempty"`
	Facts              []CheckpointFact       `json:"facts,omitempty"`
	Decisions          []CheckpointFact       `json:"decisions,omitempty"`
	Executions         []CheckpointExecution  `json:"executions,omitempty"`
}

func attachCheckpointHierarchy(
	ctx context.Context,
	checkpoint *ContextCheckpoint,
	messages []Message,
	events []Event,
	ledger *ContextLedger,
	recentMessages int,
) error {
	if checkpoint == nil {
		return errors.New("Context Checkpoint must not be nil")
	}
	if checkpoint.Version != ContextCheckpointVersion {
		return nil
	}
	layers, err := buildCheckpointLayers(
		ctx,
		messages,
		events,
		ledger,
		checkpoint.SourceMessageCount,
		recentMessages,
	)
	if err != nil {
		return err
	}
	checkpoint.Version = ContextCheckpointVersion
	checkpoint.Layers = layers
	return nil
}

func buildCheckpointLayers(
	ctx context.Context,
	messages []Message,
	events []Event,
	ledger *ContextLedger,
	sourceMessageCount int,
	recentMessages int,
) ([]ContextCheckpointLayer, error) {
	if ctx == nil {
		return nil, errors.New("Context Checkpoint hierarchy context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sourceMessageCount <= 0 || sourceMessageCount > len(messages) {
		return nil, errors.New("Context Checkpoint hierarchy source is outside raw messages")
	}
	if recentMessages < 1 {
		return nil, errors.New("Context Checkpoint hierarchy recent message count must be positive")
	}
	boundaryMessages := checkpointBoundaryMessages(messages, events)
	plan, err := buildContextSafeCutPlan(ctx, boundaryMessages, events, ledger)
	if err != nil {
		return nil, fmt.Errorf("build Context Checkpoint hierarchy boundaries: %w", err)
	}
	taskStart, err := currentTaskMessageStart(events, len(messages))
	if err != nil {
		return nil, err
	}
	if taskStart > sourceMessageCount {
		taskStart = sourceMessageCount
	}
	if taskStart > 0 && taskStart < sourceMessageCount && !plan.safe(taskStart) {
		return nil, errors.New("current Task boundary splits a protected context transaction")
	}

	episodeStart := taskStart
	preferred := sourceMessageCount - recentMessages
	for cut := taskStart + 1; cut <= preferred && cut < sourceMessageCount; cut++ {
		if plan.safe(cut) {
			episodeStart = cut
		}
	}

	layers := make([]ContextCheckpointLayer, 0, 3)
	appendLayer := func(level ContextCheckpointLevel, start, end int) {
		if start >= end {
			return
		}
		layers = append(layers, ContextCheckpointLayer{
			Level:            level,
			SourceMessageEnd: end,
		})
	}
	appendLayer(ContextCheckpointLevelSessionSynopsis, 0, taskStart)
	appendLayer(ContextCheckpointLevelTask, taskStart, episodeStart)
	appendLayer(ContextCheckpointLevelEpisode, episodeStart, sourceMessageCount)
	if len(layers) == 0 {
		return nil, errors.New("Context Checkpoint hierarchy has no source layer")
	}
	return layers, nil
}

func currentTaskMessageStart(events []Event, messageCount int) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	starts := make(map[string]int)
	currentRunID := ""
	messageIndex := 0
	for _, event := range events {
		if event.Type == EventRunStarted && event.RunID != "" {
			starts[event.RunID] = messageIndex
		}
		if ledgerEventMessageCount(event) == 0 {
			continue
		}
		if event.RunID != "" {
			currentRunID = event.RunID
		}
		messageIndex++
	}
	if messageIndex != messageCount {
		return 0, fmt.Errorf(
			"Context Checkpoint hierarchy Event messages = %d, want %d",
			messageIndex,
			messageCount,
		)
	}
	if currentRunID == "" {
		return 0, nil
	}
	start, ok := starts[currentRunID]
	if !ok {
		return 0, fmt.Errorf("current Run %q has no run.started Event", currentRunID)
	}
	return start, nil
}

func validateCheckpointHierarchy(
	ctx context.Context,
	checkpoint ContextCheckpoint,
	messages []Message,
	events []Event,
	ledger *ContextLedger,
) error {
	if checkpoint.Version != ContextCheckpointVersion {
		return fmt.Errorf("Checkpoint version = %d, want %d", checkpoint.Version, ContextCheckpointVersion)
	}
	if len(checkpoint.Layers) == 0 || len(checkpoint.Layers) > 3 {
		return errors.New("Context Checkpoint hierarchy must contain one to three layers")
	}
	order := map[ContextCheckpointLevel]int{
		ContextCheckpointLevelSessionSynopsis: 0,
		ContextCheckpointLevelTask:            1,
		ContextCheckpointLevelEpisode:         2,
	}
	cursor := 0
	previousOrder := -1
	for _, layer := range checkpoint.Layers {
		layerOrder, ok := order[layer.Level]
		if !ok {
			return fmt.Errorf("unsupported Context Checkpoint level %q", layer.Level)
		}
		if layerOrder <= previousOrder {
			return errors.New("Context Checkpoint hierarchy levels are not in canonical order")
		}
		if layer.SourceMessageEnd <= cursor ||
			layer.SourceMessageEnd > checkpoint.SourceMessageCount {
			return errors.New("Context Checkpoint hierarchy does not form a contiguous source partition")
		}
		cursor = layer.SourceMessageEnd
		previousOrder = layerOrder
	}
	if cursor != checkpoint.SourceMessageCount {
		return errors.New("Context Checkpoint hierarchy does not cover its complete source")
	}
	boundaryMessages := checkpointBoundaryMessages(messages, events)
	plan, err := buildContextSafeCutPlan(ctx, boundaryMessages, events, ledger)
	if err != nil {
		return fmt.Errorf("validate Context Checkpoint hierarchy boundaries: %w", err)
	}
	for _, layer := range checkpoint.Layers[:len(checkpoint.Layers)-1] {
		if !plan.safe(layer.SourceMessageEnd) {
			return errors.New("Context Checkpoint hierarchy splits a protected context transaction")
		}
	}
	return nil
}

func checkpointModelView(checkpoint ContextCheckpoint) contextCheckpointModelView {
	layers := make([]contextCheckpointLayerView, 0, len(checkpoint.Layers))
	start := 0
	for _, layer := range checkpoint.Layers {
		view := contextCheckpointLayerView{
			Level:              layer.Level,
			SourceMessageStart: start,
			SourceMessageEnd:   layer.SourceMessageEnd,
		}
		if checkpoint.Goal != nil && checkpointFactInRange(*checkpoint.Goal, start, layer.SourceMessageEnd) {
			goal := *checkpoint.Goal
			view.Goal = &goal
		}
		for _, fact := range checkpoint.Facts {
			if checkpointFactInRange(fact, start, layer.SourceMessageEnd) {
				view.Facts = append(view.Facts, fact)
			}
		}
		for _, decision := range checkpoint.Decisions {
			if checkpointFactInRange(decision, start, layer.SourceMessageEnd) {
				view.Decisions = append(view.Decisions, decision)
			}
		}
		for _, execution := range checkpoint.Executions {
			if execution.SourceMessage >= start && execution.SourceMessage < layer.SourceMessageEnd {
				view.Executions = append(view.Executions, execution)
			}
		}
		if checkpointLayerHasContent(view) {
			layers = append(layers, view)
		}
		start = layer.SourceMessageEnd
	}
	return contextCheckpointModelView{
		Version:            checkpoint.Version,
		Generation:         checkpoint.Generation,
		SourceMessageCount: checkpoint.SourceMessageCount,
		Layers:             layers,
		Narrative:          checkpoint.Narrative,
		Evidence:           checkpoint.Evidence,
	}
}

func checkpointModelLevels(checkpoint *ContextCheckpoint) []ContextCheckpointLevel {
	if checkpoint == nil || checkpoint.Version != ContextCheckpointVersion {
		return nil
	}
	view := checkpointModelView(*checkpoint)
	levels := make([]ContextCheckpointLevel, 0, len(view.Layers))
	for _, layer := range view.Layers {
		levels = append(levels, layer.Level)
	}
	return levels
}

func checkpointFactInRange(fact CheckpointFact, start, end int) bool {
	return fact.SourceMessage >= start && fact.SourceMessage < end
}

func checkpointLayerHasContent(layer contextCheckpointLayerView) bool {
	return layer.Goal != nil || len(layer.Facts) > 0 || len(layer.Decisions) > 0 || len(layer.Executions) > 0
}

func checkpointBoundaryMessages(messages []Message, events []Event) []Message {
	if len(events) == 0 {
		return messages
	}
	observed := make([]Message, 0, len(messages))
	for _, event := range events {
		if ledgerEventMessageCount(event) > 0 && event.Message != nil {
			observed = append(observed, cloneMessage(*event.Message))
		}
	}
	if len(observed) != len(messages) {
		return messages
	}
	return observed
}

func validateCheckpointModelLevels(levels []ContextCheckpointLevel) error {
	seen := make(map[ContextCheckpointLevel]struct{}, len(levels))
	previous := -1
	order := map[ContextCheckpointLevel]int{
		ContextCheckpointLevelSessionSynopsis: 0,
		ContextCheckpointLevelTask:            1,
		ContextCheckpointLevelEpisode:         2,
	}
	for _, level := range levels {
		position, ok := order[level]
		if !ok {
			return fmt.Errorf("unsupported Context Checkpoint model level %q", level)
		}
		if _, duplicate := seen[level]; duplicate || position <= previous {
			return errors.New("Context Checkpoint model levels are not unique and ordered")
		}
		seen[level] = struct{}{}
		previous = position
	}
	return nil
}
