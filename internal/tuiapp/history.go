package tuiapp

import (
	"context"
	"errors"
	"strings"

	tui "github.com/mayahiro/nagitui-go"

	"github.com/qed-runtime/qed/session"
)

var (
	errSessionHistoryUnavailable = errors.New("Session history unavailable")
	errNoOlderSession            = errors.New("no older Session")
	errNoNewerSession            = errors.New("no newer Session")
	errSessionHistoryTask        = errors.New("TUI Session history task failed")
	errRunEventStream            = errors.New("TUI Run Event stream failed")
)

func (view *runView) browseSession(direction int) tui.Effect[message] {
	if view.presentation.pendingApproval != nil || view.presentation.waitingUnsupported {
		view.inputNotice = "Resolve the pending input before browsing Session history"
		return tui.NoneEffect[message]()
	}
	store := view.options.SessionStore
	catalog, ok := store.(session.Catalog)
	if store == nil || !ok || view.baseRequest.SessionID == "" {
		view.inputNotice = sessionHistoryNotice(errSessionHistoryUnavailable)
		return tui.NoneEffect[message]()
	}
	currentID := ""
	if view.historyView != nil {
		currentID = view.historySession.ID
	}
	liveID := view.baseRequest.SessionID
	view.historyLoading = true
	view.historyRequest++
	requestID := view.historyRequest
	return tui.LatestEffect(sessionHistoryTaskKey, func(ctx context.Context) message {
		descriptors, err := catalog.RecentSessions(ctx, maximumRecentSessions)
		if err != nil {
			return message{kind: sessionHistoryLoadedMessage, historyRequest: requestID, err: err}
		}
		if len(descriptors) > maximumRecentSessions {
			return message{
				kind: sessionHistoryLoadedMessage, historyRequest: requestID, err: errSessionHistoryUnavailable,
			}
		}
		descriptors = excludeSession(descriptors, liveID)
		descriptor, err := selectSession(descriptors, currentID, direction)
		if err != nil {
			return message{kind: sessionHistoryLoadedMessage, historyRequest: requestID, err: err}
		}
		snapshot, err := store.Snapshot(ctx, descriptor.ID)
		if err != nil {
			return message{kind: sessionHistoryLoadedMessage, historyRequest: requestID, err: err}
		}
		if snapshot.ID != descriptor.ID {
			return message{
				kind: sessionHistoryLoadedMessage, historyRequest: requestID, err: errSessionHistoryUnavailable,
			}
		}
		descriptor.Revision = snapshot.Revision
		descriptor.MessageCount = len(snapshot.Messages)
		descriptor.Waiting = snapshot.PendingWait != nil
		if len(snapshot.Events) != 0 {
			latest := snapshot.Events[len(snapshot.Events)-1]
			descriptor.LastRunID = latest.RunID
			descriptor.UpdatedAt = latest.Time
		}
		return message{
			kind:              sessionHistoryLoadedMessage,
			historyRequest:    requestID,
			sessionDescriptor: descriptor,
			sessionSnapshot:   snapshot,
		}
	})
}

func excludeSession(
	descriptors []session.SessionDescriptor,
	excludedID string,
) []session.SessionDescriptor {
	result := make([]session.SessionDescriptor, 0, len(descriptors))
	seen := make(map[string]struct{}, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.ID == "" || descriptor.ID == excludedID {
			continue
		}
		if _, duplicate := seen[descriptor.ID]; duplicate {
			continue
		}
		seen[descriptor.ID] = struct{}{}
		result = append(result, descriptor)
	}
	return result
}

func selectSession(
	descriptors []session.SessionDescriptor,
	currentID string,
	direction int,
) (session.SessionDescriptor, error) {
	if len(descriptors) == 0 {
		if direction < 0 {
			return session.SessionDescriptor{}, errNoNewerSession
		}
		return session.SessionDescriptor{}, errNoOlderSession
	}
	if currentID == "" {
		if direction < 0 {
			return session.SessionDescriptor{}, errNoNewerSession
		}
		return descriptors[0], nil
	}
	current := -1
	for index := range descriptors {
		if descriptors[index].ID == currentID {
			current = index
			break
		}
	}
	if current < 0 {
		return session.SessionDescriptor{}, errSessionHistoryUnavailable
	}
	target := current + direction
	if target < 0 {
		return session.SessionDescriptor{}, errNoNewerSession
	}
	if target >= len(descriptors) {
		return session.SessionDescriptor{}, errNoOlderSession
	}
	return descriptors[target], nil
}

func sessionHistoryNotice(err error) string {
	switch {
	case errors.Is(err, errNoOlderSession):
		return "No older Session is available"
	case errors.Is(err, errNoNewerSession):
		return "No newer Session is available"
	case errors.Is(err, context.Canceled):
		return "Session history loading was canceled"
	default:
		return "Session history is unavailable"
	}
}

func mapRuntimeNotice(notice tui.RuntimeNotice) (message, bool) {
	switch notice.Kind() {
	case tui.RuntimeNoticeEffectPanicked,
		tui.RuntimeNoticeEffectSpawnFailed:
		if key, _, ok := notice.Task(); ok && key == sessionHistoryTaskKey {
			return message{kind: runtimeFailureMessage, err: errSessionHistoryTask}, true
		}
		return message{kind: runtimeFailureMessage}, true
	case tui.RuntimeNoticeSubscriptionStreamPanicked,
		tui.RuntimeNoticeSubscriptionSpawnFailed:
		if key, _, ok := notice.Subscription(); ok && strings.HasPrefix(key.String(), "qed-run-events-") {
			return message{kind: runtimeFailureMessage, err: errRunEventStream}, true
		}
		return message{kind: runtimeFailureMessage}, true
	default:
		return message{}, false
	}
}
