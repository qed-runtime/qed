package tuiapp

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mayahiro/nagi-go/vt"
	tui "github.com/mayahiro/nagitui-go"
	"github.com/mayahiro/nagitui-go/widget"

	"github.com/qed-runtime/qed/agent"
)

type feedEntry struct {
	id                tui.NodeID
	role              agent.Role
	text              string
	state             transcriptState
	transcriptEntryID uint64
	activities        []runActivity
	showSequence      bool
}

func buildFeedEntries(
	presentation *runPresentation,
	tab feedTab,
) ([]feedEntry, []tui.VirtualFlowItem) {
	var entries []feedEntry
	if tab == feedTabActivity {
		entries = buildActivityFeedEntries(presentation)
	} else {
		entries = buildChatFeedEntries(presentation)
	}
	items := make([]tui.VirtualFlowItem, len(entries))
	for index := range entries {
		items[index] = tui.NewVirtualFlowItem(entries[index].id)
	}
	return entries, items
}

func buildChatFeedEntries(presentation *runPresentation) []feedEntry {
	type orderedEntry struct {
		id         uint64
		transcript *transcriptEntry
		activity   *runActivity
	}
	ordered := make([]orderedEntry, 0, len(presentation.transcript)+len(presentation.activities))
	for index := range presentation.transcript {
		entry := &presentation.transcript[index]
		ordered = append(ordered, orderedEntry{id: entry.id, transcript: entry})
	}
	for index := range presentation.activities {
		activity := &presentation.activities[index]
		if !activity.visibleInChat {
			continue
		}
		ordered = append(ordered, orderedEntry{id: activity.id, activity: activity})
	}
	sort.SliceStable(ordered, func(first, second int) bool {
		return ordered[first].id < ordered[second].id
	})

	entries := make([]feedEntry, 0, len(ordered))
	for _, orderedEntry := range ordered {
		if orderedEntry.transcript != nil {
			transcript := orderedEntry.transcript
			entries = append(entries, feedEntry{
				id:                tui.NewNodeID(fmt.Sprintf("chat-%s-message-%d", presentation.feedKey, transcript.id)),
				role:              transcript.role,
				text:              transcript.text,
				state:             transcript.state,
				transcriptEntryID: transcript.id,
			})
			continue
		}
		activity := *orderedEntry.activity
		if len(entries) != 0 && len(entries[len(entries)-1].activities) != 0 {
			entries[len(entries)-1].activities = append(entries[len(entries)-1].activities, activity)
			continue
		}
		entries = append(entries, feedEntry{
			id:         tui.NewNodeID(fmt.Sprintf("chat-%s-activity-group-%d", presentation.feedKey, activity.id)),
			activities: []runActivity{activity},
		})
	}
	return entries
}

func buildActivityFeedEntries(presentation *runPresentation) []feedEntry {
	if len(presentation.activities) == 0 {
		return nil
	}
	return []feedEntry{{
		id:           tui.NewNodeID("activity-" + presentation.feedKey + "-document"),
		activities:   append([]runActivity(nil), presentation.activities...),
		showSequence: true,
	}}
}

func (entry feedEntry) node(
	tab feedTab,
	selectedID tui.NodeID,
	selectedState widget.SelectableTextState,
) tui.Node[message] {
	content, selectable := entry.selectableContent()
	if !selectable {
		return tui.Text[message](entry.text).WithID(entry.id)
	}
	state := widget.NewSelectableTextState(0)
	if entry.id == selectedID {
		state = selectedState
	}
	style := widget.DefaultSelectableTextStyle()
	style.Focused = vt.Style{}
	return widget.NewSelectableText(
		entry.id,
		content,
		state,
		func(next widget.SelectableTextState) message {
			return message{
				kind: selectableTextChangedMessage, feedTab: tab,
				selectableTextID: entry.id, selectableText: next,
			}
		},
	).Style(style).OnCopy(func(request widget.TextCopyRequest) message {
		return message{kind: copyTextMessage, feedTab: tab, copyText: request.Text}
	}).Node()
}

func (entry feedEntry) selectableContent() (widget.SelectableTextContent, bool) {
	switch {
	case len(entry.activities) != 0:
		spans := make([]tui.TextSpan, 0, 2*len(entry.activities)-1)
		for index, activity := range entry.activities {
			if index != 0 {
				spans = append(spans, tui.NewTextSpan("\n", vt.Style{}))
			}
			label := activity.label
			if activity.state != "" {
				label += " [" + string(activity.state) + "]"
			}
			style := vt.Style{}
			if entry.showSequence {
				label = fmt.Sprintf("%03d  %s", activity.sequence, label)
			} else {
				label = "  " + label
				switch activity.state {
				case activityStateRunning,
					activityStateWaiting,
					activityStateFailed,
					activityStateCanceled,
					activityStateDenied:
				default:
					style.Dim = true
				}
			}
			spans = append(spans, tui.NewTextSpan(label, style))
		}
		return widget.NewSelectableTextContent(spans), true
	case entry.role == agent.RoleUser || entry.role == agent.RoleAssistant:
		role := "Assistant"
		style := vt.Style{}
		if entry.role == agent.RoleUser {
			role = "You"
			style.Bold = true
		}
		state := ""
		if entry.state == transcriptStateQueued || entry.state == transcriptStateCanceled {
			state = " [" + string(entry.state) + "]"
		}
		return widget.NewSelectableTextContent([]tui.TextSpan{
			tui.NewTextSpan(role+": ", style),
			tui.NewTextSpan(entry.text+state, vt.Style{}),
		}), true
	default:
		return widget.SelectableTextContent{}, false
	}
}

func (entry feedEntry) estimatedHeight(width uint32) uint32 {
	if width == 0 {
		return 1
	}
	content, selectable := entry.selectableContent()
	text := entry.text
	if selectable {
		text = content.Text()
	}
	lines := uint64(0)
	for _, line := range strings.Split(text, "\n") {
		cells := uint64(utf8.RuneCountInString(line))
		rows := max((cells+uint64(width)-1)/uint64(width), 1)
		if lines > math.MaxUint32-rows {
			return math.MaxUint32
		}
		lines += rows
	}
	return uint32(max(lines, 1))
}

func (view *runView) displayPresentation() *runPresentation {
	if view.historyView != nil {
		return view.historyView
	}
	return &view.presentation
}

func (view *runView) statusText(presentation *runPresentation) string {
	if view.historyLoading {
		return "loading Session history"
	}
	if view.historyView != nil {
		return fmt.Sprintf("history revision %d", view.historySession.Revision)
	}
	status := presentation.status
	if summary, ok := workSummary(presentation); ok {
		status += "  " + summary
	}
	return status
}

func (view *runView) composerVisible() bool {
	return view.historyView == nil && !view.historyLoading &&
		view.composerEnabled &&
		view.presentation.pendingApproval == nil &&
		!view.presentation.waitingUnsupported
}

func (view *runView) markFeedUpdated() {
	for tab := feedTabChat; tab < feedTabCount; tab++ {
		if view.historyView != nil || !view.feedAtEnd[tab] {
			view.feedUnread[tab] = true
		}
	}
}

func (view *runView) draftText() string {
	return view.composer.TextArea().Value()
}

func (view *runView) clearComposer() {
	view.composer = view.composer.WithTextArea(widget.NewTextAreaStateAtEnd(""))
}

func (view *runView) recordComposerHistory(value string) {
	if strings.TrimSpace(value) == "" || len(value) > maximumComposerBytes {
		return
	}
	entries := view.composerHistory.Entries()
	entries = append(entries, widget.NewComposerHistoryEntry(
		widget.NewComposerHistoryEntryID(fmt.Sprintf("qed-history-%d", view.nextHistoryID)),
		value,
	))
	view.nextHistoryID++
	if len(entries) > maximumComposerHistory {
		entries = entries[len(entries)-maximumComposerHistory:]
	}
	history, err := widget.NewComposerHistory(entries)
	if err != nil {
		return
	}
	view.composerHistory = history
	view.composer = view.composer.ReconcileHistory(history)
}

func virtualFlowUpdate(
	presentation *runPresentation,
	tab feedTab,
	entries []feedEntry,
) tui.VirtualFlowUpdate {
	if tab == feedTabChat && presentation.feedPartial &&
		presentation.streamingEntry >= 0 &&
		presentation.streamingEntry < len(presentation.transcript) {
		streamingID := presentation.transcript[presentation.streamingEntry].id
		for index, entry := range entries {
			if entry.transcriptEntryID != streamingID {
				continue
			}
			return tui.ChangedVirtualFlowUpdate(
				presentation.revision,
				presentation.feedPrevious,
				index,
				index+1,
			)
		}
	}
	return tui.ResetVirtualFlowUpdate(presentation.revision)
}

func feedCopyText(presentation *runPresentation, tab feedTab) (string, bool) {
	entries, _ := buildFeedEntries(presentation, tab)
	if len(entries) == 0 {
		return "", false
	}
	const truncatedMarker = "\n[copy truncated]"
	var builder strings.Builder
	for _, entry := range entries {
		content, selectable := entry.selectableContent()
		if !selectable || content.Text() == "" {
			continue
		}
		separator := ""
		if builder.Len() != 0 {
			separator = "\n"
		}
		text := separator + content.Text()
		if builder.Len()+len(text) <= maximumFeedCopyBytes {
			builder.WriteString(text)
			continue
		}
		remaining := maximumFeedCopyBytes - len(truncatedMarker) - builder.Len()
		if remaining > 0 {
			builder.WriteString(boundedUTF8Prefix(text, remaining))
		}
		prefix := builder.String()
		if len(prefix) > maximumFeedCopyBytes-len(truncatedMarker) {
			prefix = boundedUTF8Prefix(prefix, maximumFeedCopyBytes-len(truncatedMarker))
		}
		return prefix + truncatedMarker, true
	}
	return builder.String(), false
}

func boundedUTF8Prefix(value string, maximum int) string {
	if maximum <= 0 {
		return ""
	}
	if len(value) <= maximum {
		return value
	}
	end := maximum
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func observabilitySummary(presentation *runPresentation) string {
	return contextSummary(presentation) + "  " + cacheSummary(presentation)
}

func workSummary(presentation *runPresentation) (string, bool) {
	work := presentation.work
	if !work.observed {
		return "", false
	}
	checks := "not run"
	if work.checksPassed != 0 || work.checksFailed != 0 {
		checks = fmt.Sprintf("%d passed, %d failed", work.checksPassed, work.checksFailed)
	}
	return fmt.Sprintf(
		"Work: patched=%d checks=%s unverified=%d",
		work.patchedFiles,
		checks,
		work.unverifiedActions,
	), true
}

func contextSummary(presentation *runPresentation) string {
	status := fmt.Sprintf("Context: compactions=%d", presentation.context.compactions)
	last := presentation.context.last
	if last.generationKnown {
		status += fmt.Sprintf(" generation=%d", last.generation)
	}
	if last.hasPredictive {
		status += fmt.Sprintf(" budget=%s input=%d/%d", last.predictiveLevel, last.predictedInput, last.maximumInput)
	}
	return status
}

func cacheSummary(presentation *runPresentation) string {
	if !presentation.cache.available {
		return "Cache: unavailable"
	}
	cache := presentation.cache
	status := fmt.Sprintf(
		"Cache: %s breakpoints=%d estimate=%d",
		cache.latest.mode,
		cache.latest.breakpoints,
		cache.latest.inputTokenEstimate,
	)
	if cache.usageReported {
		status += fmt.Sprintf(" actual=%d", cache.providerInputTokens)
	}
	return status
}

func contextDetails(presentation *runPresentation) []string {
	last := presentation.context.last
	details := []string{
		fmt.Sprintf(
			"Context detail: prepared=%d evidence=%d objects/%d bytes",
			presentation.context.preparedCandidates,
			presentation.context.externalized,
			presentation.context.externalizedBytes,
		),
	}
	if last.hasCompaction {
		details = append(details, fmt.Sprintf(
			"Last compaction: reason=%s applied=%t bytes=%d->%d messages=%d+%d recent",
			last.reason,
			last.applied,
			last.originalBytes,
			last.compiledBytes,
			last.sourceMessages,
			last.recentMessages,
		))
	}
	if presentation.cache.available {
		cache := presentation.cache
		detail := fmt.Sprintf(
			"Cache detail: ttl=%s reuse=%d estimator=%s",
			displayIdentity(string(cache.latest.ttl)),
			cache.latest.expectedReuse,
			displayIdentity(cache.latest.tokenEstimateKind),
		)
		if cache.detailsReported {
			detail += fmt.Sprintf(" read=%d write=%d", cache.cacheReadTokens, cache.cacheWriteTokens)
		}
		if cache.latest.fallbackReason != "" {
			detail += " fallback=" + cache.latest.fallbackReason
		}
		details = append(details, detail)
	}
	details = append(details, "Evidence retrieval: ask the agent to use context_search or context_fetch within its scoped access")
	return details
}

func presentationFromSnapshot(snapshot agent.SessionSnapshot) runPresentation {
	presentation := newRunPresentation("", runIdentity{sessionID: snapshot.ID})
	for _, event := range snapshot.Events {
		presentation.apply(adaptRunEvent(event))
	}
	presentation.reconcileMessages(snapshot.Messages)
	presentation.status = "history"
	presentation.trimTranscript()
	return presentation
}

func (view *runView) seedCurrentSession(
	snapshot agent.SessionSnapshot,
	request agent.RunRequest,
	prompt string,
) {
	presentation := presentationFromSnapshot(snapshot)
	presentation.identity.runID = ""
	presentation.identity.agentID = diagnosticText(request.AgentID)
	presentation.identity.sessionID = diagnosticText(request.SessionID)
	presentation.status = "starting"
	presentation.queueUserMessage(prompt)
	view.presentation = presentation
	view.seedComposerHistory(snapshot)
	view.recordComposerHistory(prompt)
}

func (view *runView) seedIdleSession(snapshot agent.SessionSnapshot, request agent.RunRequest) {
	presentation := presentationFromSnapshot(snapshot)
	presentation.identity.runID = ""
	presentation.identity.agentID = diagnosticText(request.AgentID)
	presentation.identity.sessionID = diagnosticText(request.SessionID)
	presentation.status = "ready"
	view.presentation = presentation
	view.baseRequest.Input = nil
	view.seedComposerHistory(snapshot)
}

func (view *runView) seedComposerHistory(snapshot agent.SessionSnapshot) {
	view.composerHistory, _ = widget.NewComposerHistory(nil)
	view.composer = widget.NewComposerStateAtEnd("")
	view.nextHistoryID = 1
	prior := make([]string, 0, maximumComposerHistory-1)
	for index := len(snapshot.Messages) - 1; index >= 0 && len(prior) < maximumComposerHistory-1; index-- {
		existing := snapshot.Messages[index]
		if existing.Role != agent.RoleUser || strings.TrimSpace(existing.Text) == "" ||
			len(existing.Text) > maximumComposerBytes {
			continue
		}
		prior = append(prior, existing.Text)
	}
	for index := len(prior) - 1; index >= 0; index-- {
		view.recordComposerHistory(prior[index])
	}
}
