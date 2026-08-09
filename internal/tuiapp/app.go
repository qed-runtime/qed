// Package tuiapp contains the experimental Nagi terminal frontend for QED
package tuiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/mayahiro/nagi-go/vt"
	tui "github.com/mayahiro/nagitui-go"

	"github.com/qed-runtime/qed/agent"
)

const (
	eventBridgeCapacity = 256
	maximumEventHistory = 256
)

var eventViewportID = tui.NewNodeID("run-events")

type messageKind uint8

const (
	runEventMessage messageKind = iota
	runResultMessage
	quitMessage
	cancelMessage
	approveMessage
	denyMessage
)

type message struct {
	kind   messageKind
	event  agent.Event
	result agent.RunResult
	err    error
}

type eventBridge struct {
	messages chan message
	done     chan struct{}
	cancel   context.CancelFunc
	close    sync.Once
	mu       sync.Mutex
	outcome  Outcome
}

type runView struct {
	prompt       string
	messages     <-chan message
	cancelRun    func()
	resumeRun    func(agent.WaitResponse) error
	events       []agent.Event
	status       string
	answer       string
	runErr       error
	finished     bool
	exiting      bool
	canceled     bool
	streamDone   chan struct{}
	streamClosed sync.Once
	pendingWait  *agent.WaitRequest
}

// StartFunc starts one Run for the TUI without coupling it to a concrete Harness
type StartFunc func(context.Context, agent.RunRequest) (*agent.RunHandle, error)

// Outcome contains the complete Run result and Events consumed by the TUI
type Outcome struct {
	Result agent.RunResult
	Events []agent.Event
}

// Run starts one Agent Run and displays its ordered events in a Nagi TUI
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

// RunWithStarter displays one Run started by a Runtime or orchestration Harness
func RunWithStarter(ctx context.Context, start StartFunc, request agent.RunRequest, prompt string) error {
	_, err := RunWithStarterOutcome(ctx, start, request, prompt)
	return err
}

// RunWithStarterOutcome displays one Run and returns the complete consumed outcome
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

	bridge := newEventBridge(handle)

	view := newRunView(prompt, bridge.messages, handle.Cancel, handle.Resume)
	terminalErr := tui.RunTerminalContext(
		ctx,
		view,
		tui.DefaultTerminalOptions(),
		mapEvent,
	)
	bridge.Close()
	outcome := bridge.Outcome()
	if terminalErr != nil {
		return outcome, terminalErr
	}
	if view.canceled {
		return outcome, context.Canceled
	}
	if view.runErr != nil {
		return outcome, view.runErr
	}
	return outcome, nil
}

func newEventBridge(handle *agent.RunHandle) *eventBridge {
	ctx, cancel := context.WithCancel(context.Background())
	bridge := &eventBridge{
		messages: make(chan message, eventBridgeCapacity),
		done:     make(chan struct{}),
		cancel:   cancel,
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
				bridge.send(ctx, message{kind: runResultMessage, result: result, err: err})
				return
			}
			bridge.recordEvent(event)
			if !bridge.send(ctx, message{kind: runEventMessage, event: event}) {
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
func (bridge *eventBridge) Outcome() Outcome {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return Outcome{
		Result: bridge.outcome.Result,
		Events: append([]agent.Event(nil), bridge.outcome.Events...),
	}
}

func newRunView(
	prompt string,
	messages <-chan message,
	cancelRun func(),
	resumeRun func(agent.WaitResponse) error,
) *runView {
	return &runView{
		prompt:     prompt,
		messages:   messages,
		cancelRun:  cancelRun,
		resumeRun:  resumeRun,
		status:     "starting",
		streamDone: make(chan struct{}),
	}
}

func (*runView) Init() tui.Effect[message] {
	return tui.NoneEffect[message]()
}

func (view *runView) Update(value message) tui.Effect[message] {
	switch value.kind {
	case runEventMessage:
		view.appendEvent(value.event)
		view.status = string(value.event.Type)
		if value.event.Type == agent.EventRunWaiting && value.event.WaitRequest != nil {
			wait := *value.event.WaitRequest
			wait.Payload = append(json.RawMessage(nil), value.event.WaitRequest.Payload...)
			view.pendingWait = &wait
		}
	case runResultMessage:
		view.finished = true
		view.runErr = value.err
		if value.err != nil {
			view.status = "failed"
		} else {
			view.status = string(value.result.Status)
			if answer, ok := lastAssistantMessage(value.result.Messages); ok {
				view.answer = answer.Text
			}
		}
	case quitMessage:
		view.exiting = true
		view.cancelRun()
		return tui.ExitEffect[message]()
	case cancelMessage:
		view.exiting = true
		view.canceled = true
		view.cancelRun()
		return tui.ExitEffect[message]()
	case approveMessage:
		view.resolveApproval(true)
	case denyMessage:
		view.resolveApproval(false)
	}
	return tui.NoneEffect[message]()
}

func (view *runView) Subscriptions() tui.Subscription[message] {
	if view.finished || view.exiting {
		return tui.NoneSubscription[message]()
	}
	return tui.StreamSubscription(
		"qed-run-events",
		tui.ReliableDelivery(),
		func(ctx context.Context, sink tui.SubscriptionSink[message]) {
			defer view.streamClosed.Do(func() { close(view.streamDone) })
			for {
				select {
				case <-ctx.Done():
					return
				case value, ok := <-view.messages:
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
	eventNodes := make([]tui.Node[message], 0, max(len(view.events), 1))
	if len(view.events) == 0 {
		eventNodes = append(eventNodes, tui.Text[message]("Waiting for Run events..."))
	} else {
		for _, event := range view.events {
			eventNodes = append(eventNodes, tui.Text[message](fmt.Sprintf(
				"%03d  %s",
				event.Sequence,
				event.Type,
			)))
		}
	}

	answer := "-"
	if view.answer != "" {
		answer = view.answer
	}
	help := "Q/Esc quit  Ctrl-C cancel"
	if view.pendingWait != nil {
		help = "Approval required  Y approve  N deny  Q/Esc quit"
	}
	if view.finished {
		help = "Run finished  Q/Esc quit"
	}

	return tui.Panel(
		tui.Column(
			tui.StyledText[message]("QED Runtime", vt.Style{Bold: true}).WithLength(tui.Fixed(1)),
			tui.Text[message]("Prompt: "+view.prompt).WithLength(tui.Fixed(1)),
			tui.Text[message]("Status: "+view.status).WithLength(tui.Fixed(1)),
			tui.Text[message]("Answer: "+answer).WithLength(tui.Fixed(1)),
			tui.ScrollViewportWithOptions(
				eventViewportID,
				tui.Column(eventNodes...),
				tui.ScrollViewportOptions[message]{
					Axis:       tui.ScrollAxisVertical,
					StickToEnd: true,
				},
			).WithLength(tui.Flex(1)),
			tui.Text[message](help).WithLength(tui.Fixed(1)),
		),
		"Agent Run",
	)
}

func (view *runView) appendEvent(event agent.Event) {
	if len(view.events) == maximumEventHistory {
		copy(view.events, view.events[1:])
		view.events[len(view.events)-1] = event
		return
	}
	view.events = append(view.events, event)
}

func (view *runView) resolveApproval(approved bool) {
	if view.pendingWait == nil || view.pendingWait.Kind != agent.WaitKindApproval || view.resumeRun == nil {
		return
	}
	payload, err := json.Marshal(struct {
		Approved bool `json:"approved"`
	}{Approved: approved})
	if err == nil {
		err = view.resumeRun(agent.WaitResponse{RequestID: view.pendingWait.ID, Payload: payload})
	}
	if err != nil {
		view.runErr = err
		view.status = "approval failed"
		return
	}
	view.pendingWait = nil
	view.status = "resuming"
}

func mapEvent(event vt.Event) tui.EventAction[message] {
	switch {
	case event.Kind == vt.EventKey && event.Key.Code == vt.KeyEscape:
		return tui.MessageAction(message{kind: quitMessage})
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
