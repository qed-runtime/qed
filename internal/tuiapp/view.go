package tuiapp

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/mayahiro/nagi-go/vt"
	tui "github.com/mayahiro/nagitui-go"
	"github.com/mayahiro/nagitui-go/widget"

	"github.com/qed-runtime/qed/agent"
)

type feedEntry struct {
	id       tui.NodeID
	role     agent.Role
	text     string
	state    transcriptState
	activity *runActivity
	heading  bool
}

func buildFeedEntries(presentation *runPresentation) ([]feedEntry, []tui.VirtualFlowItem) {
	entries := make([]feedEntry, 0, len(presentation.transcript)+len(presentation.activities)+2)
	for _, transcript := range presentation.transcript {
		entries = append(entries, feedEntry{
			id:    tui.NewNodeID(fmt.Sprintf("chat-%s-message-%d", presentation.feedKey, transcript.id)),
			role:  transcript.role,
			text:  transcript.text,
			state: transcript.state,
		})
	}
	entries = append(entries, feedEntry{
		id: tui.NewNodeID("chat-" + presentation.feedKey + "-activity-heading"), text: "Activity", heading: true,
	})
	if len(presentation.activities) == 0 {
		entries = append(entries, feedEntry{
			id:   tui.NewNodeID("chat-" + presentation.feedKey + "-activity-empty"),
			text: "Waiting for Run activity...",
		})
	} else {
		for index := range presentation.activities {
			activity := presentation.activities[index]
			entries = append(entries, feedEntry{
				id:       tui.NewNodeID(fmt.Sprintf("chat-%s-activity-%d", presentation.feedKey, activity.id)),
				activity: &activity,
			})
		}
	}
	items := make([]tui.VirtualFlowItem, len(entries))
	for index := range entries {
		items[index] = tui.NewVirtualFlowItem(entries[index].id)
	}
	return entries, items
}

func (entry feedEntry) node() tui.Node[message] {
	switch {
	case entry.activity != nil:
		label := entry.activity.label
		if entry.activity.state != "" {
			label += " [" + string(entry.activity.state) + "]"
		}
		return tui.Text[message](fmt.Sprintf("%03d  %s", entry.activity.sequence, label)).WithID(entry.id)
	case entry.heading:
		return tui.StyledText[message](entry.text, vt.Style{Bold: true}).WithID(entry.id)
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
		return tui.Paragraph[message]([]tui.TextSpan{
			tui.NewTextSpan(role+": ", style),
			tui.NewTextSpan(entry.text+state, vt.Style{}),
		}, tui.DefaultParagraphOptions()).WithID(entry.id)
	default:
		return tui.Text[message](entry.text).WithID(entry.id)
	}
}

func (entry feedEntry) estimatedHeight(width uint32) uint32 {
	if entry.activity != nil || entry.heading {
		return 1
	}
	if width == 0 {
		return 1
	}
	text := entry.text
	if entry.role == agent.RoleUser {
		text = "You: " + text
	} else if entry.role == agent.RoleAssistant {
		text = "Assistant: " + text
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
	return presentation.status
}

func (view *runView) composerVisible() bool {
	return view.historyView == nil && !view.historyLoading &&
		view.composerEnabled &&
		view.presentation.pendingApproval == nil &&
		!view.presentation.waitingUnsupported
}

func (view *runView) markFeedUpdated() {
	if view.historyView != nil || !view.feedAtEnd {
		view.feedUnread = true
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

func virtualFlowUpdate(presentation *runPresentation) tui.VirtualFlowUpdate {
	if presentation.feedPartial {
		return tui.ChangedVirtualFlowUpdate(
			presentation.revision,
			presentation.feedPrevious,
			presentation.feedChangedStart,
			presentation.feedChangedEnd,
		)
	}
	return tui.ResetVirtualFlowUpdate(presentation.revision)
}

func observabilitySummary(presentation *runPresentation) string {
	return contextSummary(presentation) + "  " + cacheSummary(presentation)
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
