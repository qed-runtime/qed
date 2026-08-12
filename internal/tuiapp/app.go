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

	"github.com/qed-runtime/qed/agent"
)

const (
	eventBridgeCapacity      = 256
	maximumEventHistory      = 256
	maximumTranscriptHistory = 256
)

var (
	chatViewportID  = tui.NewNodeID("chat-history")
	composerInputID = tui.NewNodeID("chat-composer")
)

type messageKind uint8

const (
	runEventMessage messageKind = iota
	runResultMessage
	quitMessage
	cancelMessage
	approveMessage
	denyMessage
	draftChangedMessage
	submitMessage
)

type message struct {
	kind      messageKind
	update    presentationUpdate
	result    agent.RunResult
	err       error
	draft     string
	runNumber uint64
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
	draft           string
	inputNotice     string
	runErr          error
	finished        bool
	exiting         bool
	cancelRequested bool
	composerEnabled bool
	streamDone      chan struct{}
	streamClosed    *sync.Once
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
	if ctx == nil {
		return Outcome{}, errors.New("TUI context must not be nil")
	}
	if start == nil {
		return Outcome{}, errors.New("TUI Run starter must not be nil")
	}
	if strings.TrimSpace(prompt) == "" {
		return Outcome{}, errors.New("TUI prompt must not be empty")
	}
	handle, err := start(ctx, request)
	if err != nil {
		return Outcome{}, fmt.Errorf("start run: %w", err)
	}

	bridge := newEventBridgeForRun(handle, 1, 0)
	view := newChatView(ctx, start, request, prompt, handle, bridge)
	terminalErr := tui.RunTerminalContext(
		ctx,
		view,
		tui.DefaultTerminalOptions(),
		mapEvent,
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
	}
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
	return &runView{
		presentation: newRunPresentation(prompt, identity),
		ctx:          context.Background(),
		messages:     messages,
		cancelRun:    cancelRun,
		resolveWait:  resolveWait,
		runNumber:    1,
		streamDone:   make(chan struct{}),
		streamClosed: &sync.Once{},
	}
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
	case runResultMessage:
		view.presentation.apply(value.update)
		view.presentation.reconcileMessages(value.result.Messages)
		view.trimTranscript()
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
	case draftChangedMessage:
		view.draft = value.draft
		view.inputNotice = ""
	case submitMessage:
		return view.submitDraft()
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

func (view *runView) View(_ tui.ViewContext) tui.Node[message] {
	content := []tui.Node[message]{
		tui.StyledText[message]("QED Runtime", vt.Style{Bold: true}).WithLength(tui.Fixed(1)),
		tui.Text[message](view.identityText()).WithLength(tui.Fixed(1)),
		tui.Text[message]("Status: " + view.presentation.status).WithLength(tui.Fixed(1)),
		view.chatHistoryNode().WithLength(tui.Flex(1)),
	}

	if approval := view.approvalText(); approval != "" {
		content = append(content, tui.Text[message](approval).WithLength(tui.Fixed(1)))
	}
	if view.composerEnabled && view.presentation.pendingApproval == nil && !view.presentation.waitingUnsupported {
		content = append(content, view.composerNode().WithLength(tui.Fixed(3)))
	}
	if view.inputNotice != "" {
		content = append(content, tui.Text[message](view.inputNotice).WithLength(tui.Fixed(1)))
	}
	content = append(content, tui.Text[message](view.helpText()).WithLength(tui.Fixed(1)))

	return tui.Panel(tui.Column(content...), "Agent Chat")
}

func (view *runView) chatHistoryNode() tui.Node[message] {
	nodes := make([]tui.Node[message], 0, len(view.presentation.transcript)+len(view.presentation.activities)+2)
	if len(view.presentation.transcript) == 0 {
		nodes = append(nodes, tui.Text[message]("Waiting for messages..."))
	} else {
		for _, entry := range view.presentation.transcript {
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
			nodes = append(nodes, tui.Paragraph[message]([]tui.TextSpan{
				tui.NewTextSpan(role+": ", style),
				tui.NewTextSpan(entry.text+state, vt.Style{}),
			}, tui.DefaultParagraphOptions()))
		}
	}
	nodes = append(nodes, tui.StyledText[message]("Activity", vt.Style{Bold: true}))
	if len(view.presentation.activities) == 0 {
		nodes = append(nodes, tui.Text[message]("Waiting for Run activity..."))
	} else {
		for _, activity := range view.presentation.activities {
			label := activity.label
			if activity.state != "" {
				label += " [" + string(activity.state) + "]"
			}
			nodes = append(nodes, tui.Text[message](fmt.Sprintf("%03d  %s", activity.sequence, label)))
		}
	}
	return tui.ScrollViewportWithOptions(
		chatViewportID,
		tui.Column(nodes...),
		tui.ScrollViewportOptions[message]{Axis: tui.ScrollAxisVertical, StickToEnd: true},
	)
}

func (view *runView) composerNode() tui.Node[message] {
	input := tui.StyledTextInput(
		composerInputID,
		view.draft,
		"Send a message",
		vt.Style{},
		vt.Style{Dim: true},
		func(value string) message {
			return message{kind: draftChangedMessage, draft: value}
		},
	)
	return tui.Panel(input, "Message")
}

func (view *runView) identityText() string {
	return fmt.Sprintf(
		"Agent: %s  Session: %s  Run: %s",
		displayIdentity(view.presentation.identity.agentID),
		displayIdentity(view.presentation.identity.sessionID),
		displayIdentity(view.presentation.identity.runID),
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
	case view.presentation.pendingApproval != nil:
		return "Approval required  Y approve  N deny  Ctrl-C cancel  Esc quit"
	case view.presentation.waitingUnsupported:
		return "Input cannot be handled here  Ctrl-C cancel  Esc quit"
	case !view.composerEnabled:
		if view.finished {
			return "Run finished  Q/Esc quit"
		}
		return "Q/Esc quit  Ctrl-C cancel"
	case view.finished:
		return "Enter follow-up  Esc quit"
	default:
		return "Enter steer  Ctrl-C cancel  Esc quit"
	}
}

func displayIdentity(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func (view *runView) submitDraft() tui.Effect[message] {
	if !view.composerEnabled || view.presentation.pendingApproval != nil || view.presentation.waitingUnsupported {
		view.inputNotice = "Resolve the pending input before sending a message"
		return tui.NoneEffect[message]()
	}
	if strings.TrimSpace(view.draft) == "" {
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
	text := view.draft
	if err := view.steerRun(agent.Message{Role: agent.RoleUser, Text: text}); err != nil {
		view.inputNotice = steeringNotice(err)
		return view.focusComposer()
	}
	view.presentation.queueUserMessage(text)
	view.trimTranscript()
	view.draft = ""
	view.inputNotice = "Steering queued"
	return view.focusComposer()
}

func (view *runView) startFollowUp() tui.Effect[message] {
	if view.start == nil {
		view.inputNotice = "Follow-up is unavailable"
		return view.focusComposer()
	}
	text := view.draft
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
	view.trimTranscript()
	view.draft = ""
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
	if !view.composerEnabled || view.presentation.pendingApproval != nil || view.presentation.waitingUnsupported {
		return tui.NoneEffect[message]()
	}
	return tui.FocusEffect[message](composerInputID)
}

func (view *runView) trimTranscript() {
	if len(view.presentation.transcript) <= maximumTranscriptHistory {
		return
	}
	extra := len(view.presentation.transcript) - maximumTranscriptHistory
	copy(view.presentation.transcript, view.presentation.transcript[extra:])
	view.presentation.transcript = view.presentation.transcript[:maximumTranscriptHistory]
	if view.presentation.streamingEntry >= 0 {
		view.presentation.streamingEntry -= extra
		if view.presentation.streamingEntry < 0 {
			view.presentation.streamingEntry = -1
		}
	}
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
