package tuiapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/internal/jsonstrict"
)

const (
	maximumApprovalCapabilities = 64
	maximumApprovalPayloadBytes = 16 << 10
	maximumDiagnosticRunes      = 96
)

type activityState string

const (
	activityStateRunning   activityState = "running"
	activityStateWaiting   activityState = "waiting"
	activityStateCompleted activityState = "completed"
	activityStateFailed    activityState = "failed"
	activityStateCanceled  activityState = "canceled"
	activityStateApproved  activityState = "approved"
	activityStateDenied    activityState = "denied"
)

type runIdentity struct {
	runID     string
	agentID   string
	sessionID string
}

type runActivity struct {
	sequence uint64
	key      string
	label    string
	state    activityState
}

type approvalPrompt struct {
	requestID    string
	tool         string
	capabilities []string
}

type presentationUpdate struct {
	identity           runIdentity
	status             string
	activity           *runActivity
	resetAnswer        bool
	answerDelta        string
	answer             *string
	approval           *approvalPrompt
	clearWait          bool
	waitingUnsupported bool
	terminal           bool
}

type runPresentation struct {
	prompt             string
	identity           runIdentity
	status             string
	answer             string
	activities         []runActivity
	pendingApproval    *approvalPrompt
	waitingUnsupported bool
}

func newRunPresentation(prompt string, identity runIdentity) runPresentation {
	identity.runID = diagnosticText(identity.runID)
	identity.agentID = diagnosticText(identity.agentID)
	identity.sessionID = diagnosticText(identity.sessionID)
	return runPresentation{
		prompt:   prompt,
		identity: identity,
		status:   "starting",
	}
}

func (presentation *runPresentation) apply(update presentationUpdate) {
	if update.identity.runID != "" {
		presentation.identity.runID = update.identity.runID
	}
	if update.identity.agentID != "" {
		presentation.identity.agentID = update.identity.agentID
	}
	if update.identity.sessionID != "" {
		presentation.identity.sessionID = update.identity.sessionID
	}
	if update.status != "" {
		presentation.status = update.status
	}
	if update.resetAnswer {
		presentation.answer = ""
	}
	presentation.answer += update.answerDelta
	if update.answer != nil {
		presentation.answer = *update.answer
	}
	if update.activity != nil {
		presentation.applyActivity(*update.activity)
	}
	if update.clearWait || update.terminal {
		presentation.pendingApproval = nil
		presentation.waitingUnsupported = false
	}
	if update.approval != nil {
		approval := *update.approval
		approval.capabilities = append([]string(nil), update.approval.capabilities...)
		presentation.pendingApproval = &approval
		presentation.waitingUnsupported = false
	}
	if update.waitingUnsupported {
		presentation.pendingApproval = nil
		presentation.waitingUnsupported = true
	}
}

func (presentation *runPresentation) resolveApproval(approved bool) (string, bool) {
	if presentation.pendingApproval == nil {
		return "", false
	}
	requestID := presentation.pendingApproval.requestID
	key := approvalActivityKey(requestID)
	for index := len(presentation.activities) - 1; index >= 0; index-- {
		if presentation.activities[index].key != key {
			continue
		}
		if approved {
			presentation.activities[index].state = activityStateApproved
		} else {
			presentation.activities[index].state = activityStateDenied
		}
		break
	}
	presentation.pendingApproval = nil
	presentation.status = "resuming"
	return requestID, true
}

func (presentation *runPresentation) applyActivity(activity runActivity) {
	if activity.key != "" {
		for index := len(presentation.activities) - 1; index >= 0; index-- {
			if presentation.activities[index].key != activity.key {
				continue
			}
			presentation.activities[index].label = activity.label
			presentation.activities[index].state = activity.state
			return
		}
	}
	if len(presentation.activities) == maximumEventHistory {
		copy(presentation.activities, presentation.activities[1:])
		presentation.activities[len(presentation.activities)-1] = activity
		return
	}
	presentation.activities = append(presentation.activities, activity)
}

func adaptRunEvent(event agent.Event) presentationUpdate {
	update := presentationUpdate{identity: eventIdentity(event)}
	activity := func(label string, state activityState) {
		update.activity = &runActivity{sequence: event.Sequence, label: label, state: state}
	}

	switch event.Type {
	case agent.EventRunStarted:
		update.status = "running"
		activity("Run started", "")
	case agent.EventUserMessageAdded:
		update.status = "running"
		activity("Request added", "")
	case agent.EventContextCompacted:
		update.status = "preparing context"
		activity("Context compacted", "")
	case agent.EventProviderRateLimitWait:
		update.status = "waiting for model capacity"
		label := "Model request queued"
		if event.ProviderRateLimitWait != nil {
			switch event.ProviderRateLimitWait.Reason {
			case agent.ProviderRateLimitWaitCooldown:
				label = fmt.Sprintf(
					"Model rate limit cooldown %dms",
					event.ProviderRateLimitWait.RetryAfterMilliseconds,
				)
			case agent.ProviderRateLimitWaitConcurrency:
				label = fmt.Sprintf(
					"Waiting for model capacity (limit %d)",
					event.ProviderRateLimitWait.MaxConcurrency,
				)
			}
		}
		activity(label, activityStateWaiting)
	case agent.EventModelRequest:
		update.status = "thinking"
		activity("Model request", "")
	case agent.EventProviderRetry:
		update.status = "waiting to retry"
		label := "Model retry scheduled"
		if event.ProviderRetry != nil {
			label = fmt.Sprintf(
				"Model retry %d in %dms (%s)",
				event.ProviderRetry.NextAttempt,
				event.ProviderRetry.DelayMilliseconds,
				event.ProviderRetry.Error.Code,
			)
		}
		activity(label, activityStateWaiting)
	case agent.EventMessageStarted:
		update.status = "responding"
		update.resetAnswer = true
	case agent.EventMessageDelta:
		update.status = "responding"
		update.answerDelta = event.Delta
	case agent.EventMessageCompleted:
		update.status = "running"
		if event.Message != nil {
			answer := event.Message.Text
			update.answer = &answer
		}
		activity("Assistant response", activityStateCompleted)
	case agent.EventToolStarted:
		update.status = "running tool"
		tool := toolName(event)
		update.activity = &runActivity{
			sequence: event.Sequence,
			key:      toolActivityKey(event, tool),
			label:    "Tool " + tool,
			state:    activityStateRunning,
		}
	case agent.EventToolCompleted:
		tool := toolName(event)
		state := activityStateCompleted
		update.status = "running"
		if event.ToolResult != nil && event.ToolResult.IsError {
			state = activityStateFailed
			update.status = "tool failed"
		}
		update.activity = &runActivity{
			sequence: event.Sequence,
			key:      toolActivityKey(event, tool),
			label:    "Tool " + tool,
			state:    state,
		}
	case agent.EventRunWaiting:
		adaptWaitEvent(event, &update)
	case agent.EventRunResumed:
		update.status = "resuming"
		update.clearWait = true
		activity("Run resumed", "")
	case agent.EventRunCompleted:
		update.status = "completed"
		update.terminal = true
		activity("Run completed", activityStateCompleted)
	case agent.EventRunFailed:
		update.status = "failed"
		update.terminal = true
		activity("Run failed", activityStateFailed)
	case agent.EventRunCanceled:
		update.status = "canceled"
		update.terminal = true
		activity("Run canceled", activityStateCanceled)
	default:
		update.status = "running"
		activity("Runtime event", "")
	}
	return update
}

func adaptRunResult(result agent.RunResult, runErr error) presentationUpdate {
	update := presentationUpdate{
		identity: runIdentity{
			runID:     diagnosticText(result.RunID),
			agentID:   diagnosticText(result.AgentID),
			sessionID: diagnosticText(result.SessionID),
		},
		terminal: true,
	}
	switch result.Status {
	case agent.RunStatusCompleted:
		update.status = "completed"
	case agent.RunStatusCanceled:
		update.status = "canceled"
	case agent.RunStatusFailed:
		update.status = "failed"
	default:
		if runErr != nil {
			update.status = "failed"
		} else {
			update.status = "finished"
		}
	}
	if answer, ok := lastAssistantMessage(result.Messages); ok {
		text := answer.Text
		update.answer = &text
	}
	return update
}

func adaptWaitEvent(event agent.Event, update *presentationUpdate) {
	wait := event.WaitRequest
	if wait == nil {
		update.status = "input unavailable"
		update.waitingUnsupported = true
		update.activity = &runActivity{
			sequence: event.Sequence,
			label:    "Input request unavailable",
			state:    activityStateFailed,
		}
		return
	}
	if wait.Kind != agent.WaitKindApproval {
		update.status = "waiting for input"
		update.waitingUnsupported = true
		update.activity = &runActivity{
			sequence: event.Sequence,
			label:    "Input required",
			state:    activityStateWaiting,
		}
		return
	}
	approval, err := decodeApprovalPrompt(*wait)
	if err != nil {
		update.status = "approval unavailable"
		update.waitingUnsupported = true
		update.activity = &runActivity{
			sequence: event.Sequence,
			label:    "Approval request unavailable",
			state:    activityStateFailed,
		}
		return
	}
	update.status = "waiting for approval"
	update.approval = &approval
	update.activity = &runActivity{
		sequence: event.Sequence,
		key:      approvalActivityKey(approval.requestID),
		label:    "Approval for Tool " + approval.tool,
		state:    activityStateWaiting,
	}
}

func decodeApprovalPrompt(wait agent.WaitRequest) (approvalPrompt, error) {
	if strings.TrimSpace(wait.ID) == "" {
		return approvalPrompt{}, errors.New("approval request ID must not be empty")
	}
	var payload struct {
		Tool         string   `json:"tool"`
		Capabilities []string `json:"capabilities"`
	}
	if err := jsonstrict.Decode(wait.Payload, maximumApprovalPayloadBytes, &payload); err != nil {
		return approvalPrompt{}, fmt.Errorf("decode approval request: %w", err)
	}
	if strings.TrimSpace(payload.Tool) == "" {
		return approvalPrompt{}, errors.New("approval Tool must not be empty")
	}
	if len(payload.Capabilities) > maximumApprovalCapabilities {
		return approvalPrompt{}, fmt.Errorf("approval has %d capabilities, maximum is %d", len(payload.Capabilities), maximumApprovalCapabilities)
	}
	capabilities := make([]string, len(payload.Capabilities))
	for index, capability := range payload.Capabilities {
		if strings.TrimSpace(capability) == "" {
			return approvalPrompt{}, errors.New("approval capability must not be empty")
		}
		capabilities[index] = diagnosticText(capability)
	}
	return approvalPrompt{
		requestID:    wait.ID,
		tool:         diagnosticText(payload.Tool),
		capabilities: capabilities,
	}, nil
}

func approvalWaitResponse(requestID string, approved bool) (agent.WaitResponse, error) {
	if strings.TrimSpace(requestID) == "" {
		return agent.WaitResponse{}, errors.New("approval request ID must not be empty")
	}
	payload, err := json.Marshal(struct {
		Approved bool `json:"approved"`
	}{Approved: approved})
	if err != nil {
		return agent.WaitResponse{}, fmt.Errorf("encode approval response: %w", err)
	}
	return agent.WaitResponse{RequestID: requestID, Payload: payload}, nil
}

func eventIdentity(event agent.Event) runIdentity {
	return runIdentity{
		runID:     diagnosticText(event.RunID),
		agentID:   diagnosticText(event.AgentID),
		sessionID: diagnosticText(event.SessionID),
	}
}

func toolName(event agent.Event) string {
	if event.ToolCall != nil && strings.TrimSpace(event.ToolCall.Name) != "" {
		return diagnosticText(event.ToolCall.Name)
	}
	if event.ToolResult != nil && strings.TrimSpace(event.ToolResult.Name) != "" {
		return diagnosticText(event.ToolResult.Name)
	}
	return "unknown"
}

func toolActivityKey(event agent.Event, tool string) string {
	if event.ToolCall != nil && event.ToolCall.ID != "" {
		return "tool:" + event.ToolCall.ID
	}
	if event.ToolResult != nil && event.ToolResult.CallID != "" {
		return "tool:" + event.ToolResult.CallID
	}
	return fmt.Sprintf("tool:%s:%d", tool, event.Sequence)
}

func approvalActivityKey(requestID string) string {
	return "approval:" + requestID
}

func diagnosticText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var result strings.Builder
	runes := 0
	for _, current := range value {
		if runes == maximumDiagnosticRunes {
			break
		}
		if unicode.IsControl(current) {
			current = '?'
		}
		result.WriteRune(current)
		runes++
	}
	return result.String()
}
