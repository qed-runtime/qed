// Package tuiapp contains the experimental Nagi terminal frontend for QED
package tuiapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/mayahiro/nagi-go/vt"
	tui "github.com/mayahiro/nagitui-go"
	"github.com/mayahiro/nagitui-go/widget"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/session"
)

const (
	eventBridgeCapacity      = 256
	maximumEventHistory      = 256
	maximumTranscriptHistory = 2048
	maximumComposerHistory   = 128
	maximumComposerBytes     = 64 << 10
	maximumRecentSessions    = 64
)

var (
	chatViewportID        = tui.NewNodeID("chat-history")
	sessionViewportID     = tui.NewNodeID("session-history")
	composerInputID       = tui.NewNodeID("chat-composer")
	composerViewportID    = tui.NewNodeID("chat-composer-viewport")
	composerCaretID       = tui.NewNodeID("chat-composer-caret")
	sessionHistoryTaskKey = tui.NewTaskKey("qed-session-history")
)

type messageKind uint8

const (
	runEventMessage messageKind = iota
	runResultMessage
	quitMessage
	cancelMessage
	approveMessage
	denyMessage
	composerChangedMessage
	submitMessage
	feedScrolledMessage
	toggleContextMessage
	browseOlderSessionMessage
	browseNewerSessionMessage
	returnCurrentSessionMessage
	sessionHistoryLoadedMessage
	runtimeFailureMessage
)

type message struct {
	kind              messageKind
	update            presentationUpdate
	result            agent.RunResult
	err               error
	composer          widget.ComposerState
	scroll            tui.ScrollState
	historicalScroll  bool
	runNumber         uint64
	sessionDescriptor session.SessionDescriptor
	sessionSnapshot   agent.SessionSnapshot
	historyRequest    uint64
}

// ChatOptions supplies optional stores used by long-running TUI navigation
type ChatOptions struct {
	// SessionStore enables bounded recent-Session navigation when it also
	// implements [session.Catalog]. It must be the same Store used by start.
	SessionStore agent.SessionStore
}

// RunOutcome contains one complete Run result and its ordered Events
type RunOutcome struct {
	Result agent.RunResult
	Events []agent.Event
}

// Outcome contains every Run completed in one TUI chat
//
// Result and Events identify the final Run for callers written before
// multi-turn chat support. Runs preserves every Run in start order.
type Outcome struct {
	Result agent.RunResult
	Events []agent.Event
	Runs   []RunOutcome
}

type eventBridge struct {
	messages          chan message
	done              chan struct{}
	cancel            context.CancelFunc
	close             sync.Once
	mu                sync.Mutex
	outcome           RunOutcome
	runNumber         uint64
	skipInputMessages int
}

type runView struct {
	presentation    runPresentation
	ctx             context.Context
	start           StartFunc
	baseRequest     agent.RunRequest
	persistent      bool
	messages        <-chan message
	handle          *agent.RunHandle
	bridges         []*eventBridge
	runNumber       uint64
	currentResult   agent.RunResult
	cancelRun       func()
	steerRun        func(agent.Message) error
	resolveWait     func(string, bool) error
	composer        widget.ComposerState
	composerHistory widget.ComposerHistory
	nextHistoryID   uint64
	inputNotice     string
	runErr          error
	finished        bool
	exiting         bool
	cancelRequested bool
	composerEnabled bool
	streamDone      chan struct{}
	streamClosed    *sync.Once
	options         ChatOptions
	feedAtEnd       bool
	feedUnread      bool
	showContext     bool
	historyLoading  bool
	historyRequest  uint64
	historyView     *runPresentation
	historySession  session.SessionDescriptor
}

// StartFunc starts one Run for the TUI without coupling it to a concrete Harness
type StartFunc func(context.Context, agent.RunRequest) (*agent.RunHandle, error)

// Run starts an Agent chat and displays its ordered events in a Nagi TUI
func Run(ctx context.Context, runtime *agent.Runtime, agentID, instructions, prompt string) error {
	if ctx == nil {
		return errors.New("TUI context must not be nil")
	}
	if runtime == nil {
		return errors.New("TUI runtime must not be nil")
	}
	if strings.TrimSpace(prompt) == "" {
		return errors.New("TUI prompt must not be empty")
	}

	return RunWithStarter(ctx, runtime.Run, agent.RunRequest{
		AgentID:      agentID,
		Instructions: instructions,
		Input: []agent.Message{
			{Role: agent.RoleUser, Text: prompt},
		},
	}, prompt)
}

// RunWithStarter displays a multi-turn chat started by a Runtime or orchestration Harness
func RunWithStarter(ctx context.Context, start StartFunc, request agent.RunRequest, prompt string) error {
	_, err := RunWithStarterOutcome(ctx, start, request, prompt)
	return err
}

// RunWithStarterOutcome displays a multi-turn chat and returns every consumed Run outcome
//
// A non-empty SessionID means follow-up Runs rely on the starter's configured
// Session Store. Without a SessionID, the TUI carries the previous Run messages
// into the next in-memory Run request.
func RunWithStarterOutcome(ctx context.Context, start StartFunc, request agent.RunRequest, prompt string) (Outcome, error) {
	return RunWithStarterOptions(ctx, start, request, prompt, ChatOptions{})
}

// RunWithStarterOptions displays a multi-turn chat with optional history navigation
func RunWithStarterOptions(
	ctx context.Context,
	start StartFunc,
	request agent.RunRequest,
	prompt string,
	options ChatOptions,
) (Outcome, error) {
	if ctx == nil {
		return Outcome{}, errors.New("TUI context must not be nil")
	}
	if start == nil {
		return Outcome{}, errors.New("TUI Run starter must not be nil")
	}
	if strings.TrimSpace(prompt) == "" {
		return Outcome{}, errors.New("TUI prompt must not be empty")
	}
	var priorSession *agent.SessionSnapshot
	if options.SessionStore != nil && request.SessionID != "" {
		snapshot, snapshotErr := options.SessionStore.Snapshot(ctx, request.SessionID)
		if snapshotErr == nil {
			priorSession = &snapshot
		} else if !errors.Is(snapshotErr, agent.ErrSessionNotFound) {
			return Outcome{}, fmt.Errorf("load TUI Session history: %w", snapshotErr)
		}
	}
	handle, err := start(ctx, request)
	if err != nil {
		return Outcome{}, fmt.Errorf("start run: %w", err)
	}

	bridge := newEventBridgeForRun(handle, 1, 0)
	view := newChatView(ctx, start, request, prompt, handle, bridge)
	view.options = options
	if priorSession != nil {
		view.seedCurrentSession(*priorSession, request, prompt)
	}
	terminalErr := tui.RunTerminalContextWithNoticeMapper(
		ctx,
		view,
		tui.DefaultTerminalOptions(),
		mapEvent,
		mapRuntimeNotice,
	)
	view.closeRuns()
	outcome := view.Outcome()
	if terminalErr != nil {
		return outcome, terminalErr
	}
	if view.runErr != nil {
		return outcome, view.runErr
	}
	return outcome, nil
}

func newEventBridge(handle *agent.RunHandle) *eventBridge {
	return newEventBridgeForRun(handle, 1, 0)
}

func newEventBridgeForRun(handle *agent.RunHandle, runNumber uint64, skipInputMessages int) *eventBridge {
	ctx, cancel := context.WithCancel(context.Background())
	bridge := &eventBridge{
		messages:          make(chan message, eventBridgeCapacity),
		done:              make(chan struct{}),
		cancel:            cancel,
		runNumber:         runNumber,
		skipInputMessages: skipInputMessages,
	}
	go bridge.consume(ctx, handle)
	return bridge
}

func (bridge *eventBridge) consume(ctx context.Context, handle *agent.RunHandle) {
	defer close(bridge.done)
	defer close(bridge.messages)

	events := handle.Events()
	for {
		select {
		case <-ctx.Done():
			bridge.cancelAndDrain(handle)
			return
		case event, ok := <-events:
			if !ok {
				result, err := handle.Wait()
				bridge.recordResult(result)
				bridge.send(ctx, message{
					kind:      runResultMessage,
					update:    adaptRunResult(result, err),
					result:    result,
					err:       err,
					runNumber: bridge.runNumber,
				})
				return
			}
			bridge.recordEvent(event)
			update := adaptRunEvent(event)
			if event.Type == agent.EventUserMessageAdded && bridge.skipInputMessages > 0 {
				bridge.skipInputMessages--
				update.userMessage = nil
				update.activity = nil
			}
			if !bridge.send(ctx, message{
				kind:      runEventMessage,
				update:    update,
				runNumber: bridge.runNumber,
			}) {
				bridge.cancelAndDrain(handle)
				return
			}
		}
	}
}

func (bridge *eventBridge) send(ctx context.Context, value message) bool {
	select {
	case <-ctx.Done():
		return false
	case bridge.messages <- value:
		return true
	}
}

func (bridge *eventBridge) Close() {
	bridge.close.Do(func() {
		bridge.cancel()
		<-bridge.done
	})
}

func (bridge *eventBridge) cancelAndDrain(handle *agent.RunHandle) {
	handle.Cancel()
	for event := range handle.Events() {
		bridge.recordEvent(event)
	}
	result, _ := handle.Wait()
	bridge.recordResult(result)
}

func (bridge *eventBridge) recordEvent(event agent.Event) {
	bridge.mu.Lock()
	bridge.outcome.Events = append(bridge.outcome.Events, event)
	bridge.mu.Unlock()
}

func (bridge *eventBridge) recordResult(result agent.RunResult) {
	bridge.mu.Lock()
	bridge.outcome.Result = result
	bridge.mu.Unlock()
}

// Outcome returns a copy after the bridge has stopped consuming the Run
func (bridge *eventBridge) Outcome() RunOutcome {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return RunOutcome{
		Result: bridge.outcome.Result,
		Events: append([]agent.Event(nil), bridge.outcome.Events...),
	}
}

func newChatView(
	ctx context.Context,
	start StartFunc,
	request agent.RunRequest,
	prompt string,
	handle *agent.RunHandle,
	bridge *eventBridge,
) *runView {
	view := &runView{
		presentation:    newRunPresentation(prompt, runIdentity{agentID: request.AgentID, sessionID: request.SessionID}),
		ctx:             ctx,
		start:           start,
		baseRequest:     request,
		persistent:      request.SessionID != "",
		composerEnabled: true,
		composer:        widget.NewComposerStateAtEnd(""),
		feedAtEnd:       true,
		nextHistoryID:   1,
	}
	view.recordComposerHistory(prompt)
	view.presentation.queueUserMessage(prompt)
	view.attachRun(handle, bridge)
	return view
}

func newRunView(
	prompt string,
	identity runIdentity,
	messages <-chan message,
	cancelRun func(),
	resolveWait func(string, bool) error,
) *runView {
	view := &runView{
		presentation:  newRunPresentation(prompt, identity),
		ctx:           context.Background(),
		messages:      messages,
		cancelRun:     cancelRun,
		resolveWait:   resolveWait,
		runNumber:     1,
		streamDone:    make(chan struct{}),
		streamClosed:  &sync.Once{},
		composer:      widget.NewComposerStateAtEnd(""),
		feedAtEnd:     true,
		nextHistoryID: 1,
	}
	view.recordComposerHistory(prompt)
	return view
}

func (view *runView) attachRun(handle *agent.RunHandle, bridge *eventBridge) {
	view.handle = handle
	view.messages = bridge.messages
	view.bridges = append(view.bridges, bridge)
	view.runNumber = bridge.runNumber
	view.cancelRun = handle.Cancel
	view.steerRun = handle.Steer
	view.resolveWait = func(requestID string, approved bool) error {
		response, err := approvalWaitResponse(requestID, approved)
		if err != nil {
			return err
		}
		return handle.Resume(response)
	}
	view.finished = false
	view.cancelRequested = false
	view.runErr = nil
	view.streamDone = make(chan struct{})
	view.streamClosed = &sync.Once{}
	view.presentation.status = "starting"
	view.presentation.pendingApproval = nil
	view.presentation.waitingUnsupported = false
}

func (view *runView) Init() tui.Effect[message] {
	if view.composerEnabled {
		return tui.FocusEffect[message](composerInputID)
	}
	return tui.NoneEffect[message]()
}

func (view *runView) Update(value message) tui.Effect[message] {
	if value.runNumber != 0 && value.runNumber != view.runNumber {
		return tui.NoneEffect[message]()
	}
	switch value.kind {
	case runEventMessage:
		view.presentation.apply(value.update)
		view.markFeedUpdated()
		if (view.historyView != nil || view.historyLoading) &&
			(value.update.approval != nil || value.update.waitingUnsupported) {
			view.historyRequest++
			view.historyView = nil
			view.historySession = session.SessionDescriptor{}
			view.historyLoading = false
			view.inputNotice = "Current Run requires input"
			return tui.CancelEffect[message](sessionHistoryTaskKey)
		}
	case runResultMessage:
		view.presentation.apply(value.update)
		view.presentation.reconcileMessages(value.result.Messages)
		view.markFeedUpdated()
		view.currentResult = value.result
		view.finished = true
		if view.cancelRequested && value.result.Status == agent.RunStatusCanceled {
			view.runErr = nil
		} else {
			view.runErr = value.err
		}
		view.cancelRequested = false
		return view.focusComposer()
	case quitMessage:
		view.exiting = true
		if !view.finished && view.cancelRun != nil {
			view.cancelRun()
		}
		return tui.ExitEffect[message]()
	case cancelMessage:
		if view.historyView != nil || view.historyLoading {
			view.inputNotice = "Press F7 to return before canceling the current Run"
			return tui.NoneEffect[message]()
		}
		if !view.finished && view.cancelRun != nil {
			view.cancelRequested = true
			view.presentation.status = "canceling"
			view.inputNotice = "Cancel requested"
			view.cancelRun()
		}
	case approveMessage:
		return view.resolveApproval(true)
	case denyMessage:
		return view.resolveApproval(false)
	case composerChangedMessage:
		view.composer = value.composer
		view.inputNotice = ""
	case submitMessage:
		return view.submitDraft()
	case feedScrolledMessage:
		if !value.historicalScroll {
			view.feedAtEnd = value.scroll.AtEnd
			if value.scroll.AtEnd {
				view.feedUnread = false
			}
		}
	case toggleContextMessage:
		view.showContext = !view.showContext
	case browseOlderSessionMessage:
		return view.browseSession(1)
	case browseNewerSessionMessage:
		return view.browseSession(-1)
	case returnCurrentSessionMessage:
		view.historyRequest++
		view.historyView = nil
		view.historySession = session.SessionDescriptor{}
		view.historyLoading = false
		view.inputNotice = "Returned to current Session"
		return tui.BatchEffects(
			tui.CancelEffect[message](sessionHistoryTaskKey),
			view.focusComposer(),
		)
	case sessionHistoryLoadedMessage:
		if value.historyRequest != view.historyRequest {
			return tui.NoneEffect[message]()
		}
		view.historyLoading = false
		if value.err != nil {
			view.inputNotice = sessionHistoryNotice(value.err)
			return tui.NoneEffect[message]()
		}
		presentation := presentationFromSnapshot(value.sessionSnapshot)
		view.historyView = &presentation
		view.historySession = value.sessionDescriptor
		view.inputNotice = "Viewing previous Session; F7 returns to the current chat"
	case runtimeFailureMessage:
		if errors.Is(value.err, errRunEventStream) {
			view.runErr = value.err
			view.presentation.status = "TUI event stream failed"
			view.inputNotice = "Run Event stream failed"
			view.exiting = true
			if !view.finished && view.cancelRun != nil {
				view.cancelRun()
			}
			return tui.ExitEffect[message]()
		}
		if errors.Is(value.err, errSessionHistoryTask) {
			view.historyRequest++
			view.historyLoading = false
			view.inputNotice = "Session history is unavailable"
			return tui.NoneEffect[message]()
		}
		view.inputNotice = "TUI background work failed"
	}
	return tui.NoneEffect[message]()
}

func (view *runView) Subscriptions() tui.Subscription[message] {
	if view.finished || view.exiting || view.messages == nil {
		return tui.NoneSubscription[message]()
	}
	messages := view.messages
	done := view.streamDone
	closed := view.streamClosed
	return tui.StreamSubscription(
		tui.NewSubscriptionKey(fmt.Sprintf("qed-run-events-%d", view.runNumber)),
		tui.ReliableDelivery(),
		func(ctx context.Context, sink tui.SubscriptionSink[message]) {
			defer closed.Do(func() { close(done) })
			for {
				select {
				case <-ctx.Done():
					return
				case value, ok := <-messages:
					if !ok {
						return
					}
					if !sink.Send(value) {
						return
					}
				}
			}
		},
	)
}

func (view *runView) View(context tui.ViewContext) tui.Node[message] {
	presentation := view.displayPresentation()
	content := []tui.Node[message]{
		tui.StyledText[message]("QED Runtime", vt.Style{Bold: true}).WithLength(tui.Fixed(1)),
		tui.Text[message](view.identityText(presentation)).WithLength(tui.Fixed(1)),
		tui.Text[message]("Status: " + view.statusText(presentation)).WithLength(tui.Fixed(1)),
		tui.Text[message](observabilitySummary(presentation)).WithLength(tui.Fixed(1)),
	}
	if view.showContext {
		for _, detail := range contextDetails(presentation) {
			content = append(content, tui.Text[message](detail).WithLength(tui.Fixed(1)))
		}
	}
	content = append(content, view.chatHistoryNode(presentation).WithLength(tui.Flex(1)))

	if approval := view.approvalText(); approval != "" && view.historyView == nil && !view.historyLoading {
		content = append(content, tui.Text[message](approval).WithLength(tui.Fixed(1)))
	}
	if view.composerVisible() {
		composer := view.composerWidget(context)
		content = append(content, tui.Panel(composer.Node(), "Message").WithLength(tui.Fixed(composer.VisibleRows()+2)))
	}
	if view.inputNotice != "" {
		content = append(content, tui.Text[message](view.inputNotice).WithLength(tui.Fixed(1)))
	}
	content = append(content, tui.Text[message](view.helpText()).WithLength(tui.Fixed(1)))

	return tui.Panel(tui.Column(content...), "Agent Chat")
}

func (view *runView) chatHistoryNode(presentation *runPresentation) tui.Node[message] {
	entries, items := buildFeedEntries(presentation)
	order, err := tui.NewVirtualFlowItems(items)
	if err != nil {
		return tui.Text[message]("Chat history is unavailable")
	}
	source := tui.NewVirtualFlowSource(order, func(context tui.VirtualFlowItemContext) tui.Node[message] {
		return entries[context.Index].node()
	}).Update(virtualFlowUpdate(presentation)).EstimatedHeight(
		func(context tui.VirtualFlowItemContext) uint32 {
			return entries[context.Index].estimatedHeight(context.Width)
		},
	)
	feedID := chatViewportID
	if view.historyView != nil {
		feedID = sessionViewportID
	}
	historical := view.historyView != nil
	feed := widget.NewVirtualFeed(feedID, source).
		Overscan(8).
		FollowEnd(true).
		OnScroll(func(state tui.ScrollState) message {
			return message{kind: feedScrolledMessage, scroll: state, historicalScroll: historical}
		}).
		Empty(tui.Text[message]("Waiting for messages..."))
	if view.feedUnread && view.historyView == nil {
		feed = feed.UnreadIndicator(tui.StyledText[message](" New activity below ", vt.Style{Reverse: true}))
	}
	return feed.Node()
}

func (view *runView) composerWidget(context tui.ViewContext) widget.Composer[message] {
	wrapWidth := max(int(context.Size.Width)-6, 1)
	return widget.NewComposer(
		composerInputID,
		composerViewportID,
		composerCaretID,
		view.composer,
		func(state widget.ComposerState) message {
			return message{kind: composerChangedMessage, composer: state}
		},
		func() message { return message{kind: submitMessage} },
	).
		Placeholder("Send a message").
		WidthProfile(context.WidthProfile).
		SoftWrap(wrapWidth).
		Rows(1, 6).
		MaximumUTF8Bytes(maximumComposerBytes, widget.ComposerOverflowReject).
		History(view.composerHistory)
}

func (view *runView) identityText(presentation *runPresentation) string {
	return fmt.Sprintf(
		"Agent: %s  Session: %s  Run: %s",
		displayIdentity(presentation.identity.agentID),
		displayIdentity(presentation.identity.sessionID),
		displayIdentity(presentation.identity.runID),
	)
}

func (view *runView) approvalText() string {
	if view.presentation.pendingApproval == nil {
		return ""
	}
	approval := "Approval: Tool " + view.presentation.pendingApproval.tool
	if len(view.presentation.pendingApproval.capabilities) != 0 {
		approval += " [" + strings.Join(view.presentation.pendingApproval.capabilities, ", ") + "]"
	}
	return approval
}

func (view *runView) helpText() string {
	switch {
	case view.historyView != nil || view.historyLoading:
		return "F6 older  Shift-F6 newer  F7 current  F2 context  Esc quit"
	case view.presentation.pendingApproval != nil:
		return "Approval required  Y approve  N deny  Ctrl-C cancel  F2 context  Esc quit"
	case view.presentation.waitingUnsupported:
		return "Input cannot be handled here  Ctrl-C cancel  F2 context  Esc quit"
	case !view.composerEnabled:
		if view.finished {
			return "Run finished  F2 context  Q/Esc quit"
		}
		return "Q/Esc quit  Ctrl-C cancel"
	case view.finished:
		return "Enter follow-up  F2 context  F6 Sessions  Esc quit"
	default:
		return "Enter steer  Ctrl-C cancel  F2 context  F6 Sessions  Esc quit"
	}
}

func displayIdentity(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func (view *runView) submitDraft() tui.Effect[message] {
	if view.historyView != nil || view.historyLoading {
		view.inputNotice = "Press F7 to return to the current chat before sending a message"
		return tui.NoneEffect[message]()
	}
	if !view.composerEnabled || view.presentation.pendingApproval != nil || view.presentation.waitingUnsupported {
		view.inputNotice = "Resolve the pending input before sending a message"
		return tui.NoneEffect[message]()
	}
	if strings.TrimSpace(view.draftText()) == "" {
		view.inputNotice = "Message must not be empty"
		return view.focusComposer()
	}
	if view.finished {
		return view.startFollowUp()
	}
	if view.steerRun == nil {
		view.inputNotice = "Steering is unavailable"
		return view.focusComposer()
	}
	text := view.draftText()
	if err := view.steerRun(agent.Message{Role: agent.RoleUser, Text: text}); err != nil {
		view.inputNotice = steeringNotice(err)
		return view.focusComposer()
	}
	view.presentation.queueUserMessage(text)
	view.recordComposerHistory(text)
	view.clearComposer()
	view.inputNotice = "Steering queued"
	return view.focusComposer()
}

func (view *runView) startFollowUp() tui.Effect[message] {
	if view.start == nil {
		view.inputNotice = "Follow-up is unavailable"
		return view.focusComposer()
	}
	text := view.draftText()
	request := view.baseRequest
	request.Resume = nil
	skipInputMessages := 0
	if view.persistent {
		request.Input = []agent.Message{{Role: agent.RoleUser, Text: text}}
	} else {
		history := append([]agent.Message(nil), view.currentResult.Messages...)
		skipInputMessages = len(history)
		request.Input = append(history, agent.Message{Role: agent.RoleUser, Text: text})
	}
	handle, err := view.start(view.ctx, request)
	if err != nil {
		view.inputNotice = "Follow-up could not be started"
		view.runErr = err
		return view.focusComposer()
	}
	view.presentation.queueUserMessage(text)
	view.recordComposerHistory(text)
	view.clearComposer()
	view.inputNotice = ""
	bridge := newEventBridgeForRun(handle, view.runNumber+1, skipInputMessages)
	view.attachRun(handle, bridge)
	return view.focusComposer()
}

func (view *runView) resolveApproval(approved bool) tui.Effect[message] {
	if view.presentation.pendingApproval == nil || view.resolveWait == nil {
		return tui.NoneEffect[message]()
	}
	requestID := view.presentation.pendingApproval.requestID
	if err := view.resolveWait(requestID, approved); err != nil {
		view.runErr = err
		view.presentation.status = "approval failed"
		view.inputNotice = "Approval response could not be sent"
		return tui.NoneEffect[message]()
	}
	view.runErr = nil
	view.inputNotice = ""
	view.presentation.resolveApproval(approved)
	return view.focusComposer()
}

func (view *runView) focusComposer() tui.Effect[message] {
	if !view.composerVisible() {
		return tui.NoneEffect[message]()
	}
	return tui.FocusEffect[message](composerInputID)
}

func (view *runView) closeRuns() {
	for _, bridge := range view.bridges {
		bridge.Close()
	}
}

// Outcome returns copies of all Run outcomes currently owned by the chat
func (view *runView) Outcome() Outcome {
	runs := make([]RunOutcome, 0, len(view.bridges))
	for _, bridge := range view.bridges {
		run := bridge.Outcome()
		if run.Result.RunID == "" && len(run.Events) == 0 {
			continue
		}
		runs = append(runs, run)
	}
	outcome := Outcome{Runs: runs}
	if len(runs) != 0 {
		last := runs[len(runs)-1]
		outcome.Result = last.Result
		outcome.Events = append([]agent.Event(nil), last.Events...)
	}
	return outcome
}

func steeringNotice(err error) string {
	switch {
	case errors.Is(err, agent.ErrRunWaiting):
		return "Resolve the pending input before steering"
	case errors.Is(err, agent.ErrSteeringQueueFull):
		return "Steering queue is full"
	case errors.Is(err, agent.ErrRunClosed):
		return "Run finished while steering was submitted; send it again as a follow-up"
	case errors.Is(err, agent.ErrInvalidSteeringMessage):
		return "Message is not valid steering input"
	default:
		return "Steering could not be queued"
	}
}

func mapEvent(event vt.Event) tui.EventAction[message] {
	if event.Kind == vt.EventKey && event.Key.Action == vt.KeyRelease {
		return tui.IgnoreAction[message]()
	}
	switch {
	case event.Kind == vt.EventKey && event.Key.Code == vt.KeyEscape:
		return tui.MessageAction(message{kind: quitMessage})
	case event.Kind == vt.EventKey && event.Key.Code == vt.KeyFunction && event.Key.Function == 2:
		return tui.MessageAction(message{kind: toggleContextMessage})
	case event.Kind == vt.EventKey && event.Key.Code == vt.KeyFunction && event.Key.Function == 6 && event.Key.Modifiers.Shift:
		return tui.MessageAction(message{kind: browseNewerSessionMessage})
	case event.Kind == vt.EventKey && event.Key.Code == vt.KeyFunction && event.Key.Function == 6:
		return tui.MessageAction(message{kind: browseOlderSessionMessage})
	case event.Kind == vt.EventKey && event.Key.Code == vt.KeyFunction && event.Key.Function == 7:
		return tui.MessageAction(message{kind: returnCurrentSessionMessage})
	case event.Kind == vt.EventKey && event.Key.Code == vt.KeyEnter:
		return tui.MessageAction(message{kind: submitMessage})
	case event.Kind == vt.EventKey &&
		event.Key.Code == vt.KeyCharacter &&
		event.Key.Character == 'c' &&
		event.Key.Modifiers.Control:
		return tui.MessageAction(message{kind: cancelMessage})
	case event.Kind == vt.EventText && (event.Text == "q" || event.Text == "Q"):
		return tui.MessageAction(message{kind: quitMessage})
	case event.Kind == vt.EventText && (event.Text == "y" || event.Text == "Y"):
		return tui.MessageAction(message{kind: approveMessage})
	case event.Kind == vt.EventText && (event.Text == "n" || event.Text == "N"):
		return tui.MessageAction(message{kind: denyMessage})
	default:
		return tui.IgnoreAction[message]()
	}
}

func lastAssistantMessage(messages []agent.Message) (agent.Message, bool) {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == agent.RoleAssistant {
			return messages[index], true
		}
	}
	return agent.Message{}, false
}
