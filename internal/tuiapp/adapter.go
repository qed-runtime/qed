package tuiapp

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/internal/jsonstrict"
)

const (
	maximumApprovalCapabilities = 64
	maximumApprovalPayloadBytes = 64 << 10
	maximumApprovalPreviewRunes = 512
	maximumDiagnosticRunes      = 96
	maximumSummaryOutputBytes   = 1 << 20
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
	id            uint64
	sequence      uint64
	key           string
	label         string
	state         activityState
	tool          string
	failure       string
	visibleInChat bool
}

type runWorkSummary struct {
	patchedFiles      int
	checksPassed      int
	checksFailed      int
	unverifiedActions int
	observed          bool
}

type approvalPrompt struct {
	requestID           string
	activityKey         string
	tool                string
	capabilities        []string
	argumentsDigest     string
	extensionID         string
	extensionGeneration uint64
	summary             string
	details             []approvalDetail
}

type approvalDetail struct {
	label string
	value string
}

type presentationUpdate struct {
	identity           runIdentity
	status             string
	activity           *runActivity
	userMessage        *string
	userMessageOrigin  agent.UserMessageOrigin
	assistantStarted   bool
	assistantCompleted bool
	resetAnswer        bool
	answerDelta        string
	answer             *string
	approval           *approvalPrompt
	clearWait          bool
	waitingUnsupported bool
	terminal           bool
	context            *contextObservation
	cache              *cacheObservation
	usage              *agent.Usage
	resetWork          bool
	patchedFiles       int
	checksPassed       int
	checksFailed       int
	unverifiedActions  int
	resetUnverified    bool
	observedWork       bool
}

type runPresentation struct {
	identity           runIdentity
	status             string
	answer             string
	transcript         []transcriptEntry
	streamingEntry     int
	activities         []runActivity
	pendingApproval    *approvalPrompt
	waitingUnsupported bool
	context            contextPresentation
	cache              cachePresentation
	nextEntryID        uint64
	revision           uint64
	feedKey            string
	feedPrevious       uint64
	feedPartial        bool
	work               runWorkSummary
	failureCounts      map[string]int
}

type contextObservation struct {
	hasCompaction     bool
	hasPredictive     bool
	compacted         bool
	prepared          bool
	applied           bool
	reason            string
	originalBytes     int64
	compiledBytes     int64
	sourceMessages    int
	recentMessages    int
	generation        uint64
	generationKnown   bool
	externalized      int
	externalizedBytes int64
	predictiveLevel   agent.PredictiveBudgetLevel
	predictiveAction  agent.PredictiveBudgetAction
	predictedInput    int64
	maximumInput      int64
}

type contextPresentation struct {
	compactions        int
	preparedCandidates int
	last               contextObservation
	externalized       int
	externalizedBytes  int64
}

type cacheObservation struct {
	mode               agent.CacheMode
	ttl                agent.CacheTTL
	breakpoints        int
	expectedReuse      int
	inputTokenEstimate int64
	tokenEstimateKind  string
	fallbackReason     string
}

type cachePresentation struct {
	available           bool
	latest              cacheObservation
	providerInputTokens int64
	usageReported       bool
	cacheReadTokens     int64
	cacheWriteTokens    int64
	detailsReported     bool
}

type transcriptState string

const (
	transcriptStateQueued   transcriptState = "queued"
	transcriptStateApplied  transcriptState = "applied"
	transcriptStateCanceled transcriptState = "canceled"
)

type transcriptEntry struct {
	id     uint64
	role   agent.Role
	text   string
	state  transcriptState
	origin agent.UserMessageOrigin
}

func newRunPresentation(_ string, identity runIdentity) runPresentation {
	feedKey := presentationKey("session", identity.sessionID)
	identity.runID = diagnosticText(identity.runID)
	identity.agentID = diagnosticText(identity.agentID)
	identity.sessionID = diagnosticText(identity.sessionID)
	return runPresentation{
		identity:       identity,
		status:         "starting",
		streamingEntry: -1,
		nextEntryID:    1,
		feedKey:        feedKey,
	}
}

func (presentation *runPresentation) apply(update presentationUpdate) {
	streamingEntry := presentation.streamingEntry
	partialDelta := update.answerDelta != "" && streamingEntry >= 0 &&
		update.activity == nil && update.userMessage == nil && !update.assistantStarted &&
		!update.assistantCompleted && update.answer == nil && update.approval == nil &&
		!update.clearWait && !update.waitingUnsupported && !update.terminal &&
		update.context == nil && update.cache == nil && update.usage == nil
	presentation.invalidateFeed()
	if partialDelta {
		presentation.feedPartial = true
	}
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
		if update.status != "failed" || !strings.HasPrefix(presentation.status, "failed: ") {
			presentation.status = update.status
		}
	}
	if update.resetWork {
		presentation.work = runWorkSummary{}
		presentation.failureCounts = nil
	}
	presentation.work.patchedFiles = saturatingAddInt(
		presentation.work.patchedFiles,
		update.patchedFiles,
	)
	presentation.work.checksPassed = saturatingAddInt(
		presentation.work.checksPassed,
		update.checksPassed,
	)
	presentation.work.checksFailed = saturatingAddInt(
		presentation.work.checksFailed,
		update.checksFailed,
	)
	if update.resetUnverified {
		presentation.work.unverifiedActions = 0
	}
	presentation.work.unverifiedActions = saturatingAddInt(
		presentation.work.unverifiedActions,
		update.unverifiedActions,
	)
	presentation.work.observed = presentation.work.observed || update.observedWork
	if update.userMessage != nil {
		presentation.applyUserMessage(*update.userMessage, update.userMessageOrigin)
	}
	if update.assistantStarted {
		presentation.startAssistantMessage()
	}
	if update.resetAnswer {
		presentation.answer = ""
	}
	presentation.answer += update.answerDelta
	if update.answerDelta != "" && presentation.streamingEntry >= 0 {
		presentation.transcript[presentation.streamingEntry].text += update.answerDelta
	}
	if update.answer != nil {
		presentation.answer = *update.answer
		if presentation.streamingEntry >= 0 {
			presentation.transcript[presentation.streamingEntry].text = *update.answer
		}
	}
	if update.assistantCompleted {
		presentation.completeAssistantMessage()
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
	if update.context != nil {
		presentation.applyContext(*update.context)
	}
	if update.cache != nil {
		presentation.applyCache(*update.cache)
	}
	if update.usage != nil {
		presentation.applyUsage(*update.usage)
	}
}

func (presentation *runPresentation) queueUserMessage(text string) {
	presentation.invalidateFeed()
	presentation.transcript = append(presentation.transcript, transcriptEntry{
		id:    presentation.allocateEntryID(),
		role:  agent.RoleUser,
		text:  text,
		state: transcriptStateQueued,
	})
	presentation.trimTranscript()
}

func (presentation *runPresentation) applyUserMessage(text string, origin agent.UserMessageOrigin) {
	for index := range presentation.transcript {
		entry := &presentation.transcript[index]
		if entry.role != agent.RoleUser || entry.state != transcriptStateQueued || entry.text != text {
			continue
		}
		entry.state = transcriptStateApplied
		entry.origin = origin
		return
	}
	presentation.transcript = append(presentation.transcript, transcriptEntry{
		id:     presentation.allocateEntryID(),
		role:   agent.RoleUser,
		text:   text,
		state:  transcriptStateApplied,
		origin: origin,
	})
	presentation.trimTranscript()
}

func (presentation *runPresentation) startAssistantMessage() {
	if presentation.streamingEntry >= 0 {
		presentation.completeAssistantMessage()
	}
	presentation.transcript = append(presentation.transcript, transcriptEntry{
		id:    presentation.allocateEntryID(),
		role:  agent.RoleAssistant,
		state: transcriptStateApplied,
	})
	presentation.streamingEntry = len(presentation.transcript) - 1
	presentation.trimTranscript()
}

func (presentation *runPresentation) completeAssistantMessage() {
	if presentation.streamingEntry < 0 || presentation.streamingEntry >= len(presentation.transcript) {
		presentation.streamingEntry = -1
		return
	}
	if presentation.transcript[presentation.streamingEntry].text == "" {
		presentation.transcript = append(
			presentation.transcript[:presentation.streamingEntry],
			presentation.transcript[presentation.streamingEntry+1:]...,
		)
	}
	presentation.streamingEntry = -1
}

func (presentation *runPresentation) reconcileMessages(messages []agent.Message) {
	presentation.invalidateFeed()
	pending := make([]transcriptEntry, 0)
	for _, entry := range presentation.transcript {
		if entry.role == agent.RoleUser && entry.state == transcriptStateQueued {
			entry.state = transcriptStateCanceled
			pending = append(pending, entry)
		}
	}
	visibleMessages := messages
	if len(visibleMessages) > maximumTranscriptHistory {
		visibleMessages = visibleMessages[len(visibleMessages)-maximumTranscriptHistory:]
	}
	transcript := make([]transcriptEntry, 0, min(len(visibleMessages)+len(pending), maximumTranscriptHistory))
	searchFrom := 0
	for _, message := range visibleMessages {
		if message.Role != agent.RoleUser && message.Role != agent.RoleAssistant {
			continue
		}
		if message.Role == agent.RoleAssistant && message.Text == "" {
			continue
		}
		entryID := uint64(0)
		for index := searchFrom; index < len(presentation.transcript); index++ {
			entry := presentation.transcript[index]
			if entry.role != message.Role || entry.text != message.Text || entry.state == transcriptStateQueued {
				continue
			}
			entryID = entry.id
			searchFrom = index + 1
			break
		}
		if entryID == 0 {
			entryID = presentation.allocateEntryID()
		}
		transcript = append(transcript, transcriptEntry{
			id:    entryID,
			role:  message.Role,
			text:  message.Text,
			state: transcriptStateApplied,
		})
	}
	presentation.transcript = append(transcript, pending...)
	presentation.streamingEntry = -1
	presentation.trimTranscript()
}

func (presentation *runPresentation) resolveApproval(approved bool) (string, bool) {
	if presentation.pendingApproval == nil {
		return "", false
	}
	requestID := presentation.pendingApproval.requestID
	key := presentation.pendingApproval.activityKey
	for index := len(presentation.activities) - 1; index >= 0; index-- {
		if presentation.activities[index].key != key {
			continue
		}
		if approved {
			presentation.activities[index].state = activityStateApproved
		} else {
			presentation.activities[index].state = activityStateDenied
		}
		presentation.invalidateFeed()
		break
	}
	presentation.pendingApproval = nil
	presentation.status = "resuming"
	return requestID, true
}

func (presentation *runPresentation) applyActivity(activity runActivity) {
	if activity.tool != "" {
		if activity.failure == "" {
			for key := range presentation.failureCounts {
				if strings.HasPrefix(key, activity.tool+"\x00") {
					delete(presentation.failureCounts, key)
				}
			}
		} else {
			if presentation.failureCounts == nil {
				presentation.failureCounts = make(map[string]int)
			}
			key := activity.tool + "\x00" + activity.failure
			presentation.failureCounts[key]++
			if count := presentation.failureCounts[key]; count > 1 {
				activity.label += fmt.Sprintf(" (repeat %d)", count)
			}
		}
	}
	if activity.key != "" {
		for index := len(presentation.activities) - 1; index >= 0; index-- {
			if presentation.activities[index].key != activity.key {
				continue
			}
			activity.id = presentation.activities[index].id
			presentation.activities[index].label = activity.label
			presentation.activities[index].state = activity.state
			presentation.activities[index].visibleInChat = activity.visibleInChat
			return
		}
	}
	if activity.id == 0 {
		activity.id = presentation.allocateEntryID()
	}
	if len(presentation.activities) == maximumEventHistory {
		copy(presentation.activities, presentation.activities[1:])
		presentation.activities[len(presentation.activities)-1] = activity
		return
	}
	presentation.activities = append(presentation.activities, activity)
}

func (presentation *runPresentation) allocateEntryID() uint64 {
	id := presentation.nextEntryID
	presentation.nextEntryID++
	return id
}

func (presentation *runPresentation) invalidateFeed() {
	presentation.feedPrevious = presentation.revision
	presentation.revision++
	presentation.feedPartial = false
}

func (presentation *runPresentation) trimTranscript() {
	if len(presentation.transcript) <= maximumTranscriptHistory {
		return
	}
	extra := len(presentation.transcript) - maximumTranscriptHistory
	copy(presentation.transcript, presentation.transcript[extra:])
	presentation.transcript = presentation.transcript[:maximumTranscriptHistory]
	if presentation.streamingEntry >= 0 {
		presentation.streamingEntry -= extra
		if presentation.streamingEntry < 0 {
			presentation.streamingEntry = -1
		}
	}
}

func (presentation *runPresentation) applyContext(observation contextObservation) {
	predictive := observation
	if observation.compacted {
		presentation.context.compactions = saturatingAddInt(presentation.context.compactions, 1)
	}
	if observation.prepared {
		presentation.context.preparedCandidates = saturatingAddInt(presentation.context.preparedCandidates, 1)
	}
	presentation.context.externalized = saturatingAddInt(
		presentation.context.externalized,
		observation.externalized,
	)
	presentation.context.externalizedBytes = saturatingAddInt64(
		presentation.context.externalizedBytes,
		observation.externalizedBytes,
	)
	if observation.hasCompaction {
		last := presentation.context.last
		if !observation.generationKnown && last.generationKnown {
			observation.generation = last.generation
			observation.generationKnown = true
		}
		observation.hasPredictive = observation.hasPredictive || last.hasPredictive
		observation.predictiveLevel = last.predictiveLevel
		observation.predictiveAction = last.predictiveAction
		observation.predictedInput = last.predictedInput
		observation.maximumInput = last.maximumInput
		presentation.context.last = observation
	}
	if predictive.hasPredictive {
		presentation.context.last.hasPredictive = true
		presentation.context.last.predictiveLevel = predictive.predictiveLevel
		presentation.context.last.predictiveAction = predictive.predictiveAction
		presentation.context.last.predictedInput = predictive.predictedInput
		presentation.context.last.maximumInput = predictive.maximumInput
	}
}

func (presentation *runPresentation) applyCache(observation cacheObservation) {
	presentation.cache.available = true
	presentation.cache.latest = observation
	presentation.cache.providerInputTokens = 0
	presentation.cache.usageReported = false
	presentation.cache.cacheReadTokens = 0
	presentation.cache.cacheWriteTokens = 0
	presentation.cache.detailsReported = false
}

func (presentation *runPresentation) applyUsage(usage agent.Usage) {
	if !presentation.cache.available {
		return
	}
	presentation.cache.providerInputTokens = usage.InputTokens
	presentation.cache.usageReported = true
	presentation.cache.cacheReadTokens = usage.CacheReadInputTokens
	presentation.cache.cacheWriteTokens = usage.CacheWriteInputTokens
	presentation.cache.detailsReported = usage.InputTokenDetailsReported
}

func adaptRunEvent(event agent.Event) presentationUpdate {
	update := presentationUpdate{identity: eventIdentity(event)}
	activity := func(label string, state activityState) {
		update.activity = &runActivity{sequence: event.Sequence, label: label, state: state}
	}

	switch event.Type {
	case agent.EventRunStarted:
		update.status = "running"
		update.resetWork = true
		activity("Run started", "")
	case agent.EventUserMessageAdded:
		update.status = "running"
		label := "Request added"
		if event.UserMessageOrigin == agent.UserMessageOriginSteering {
			label = "Steering added"
		}
		if event.Message != nil && event.Message.Role == agent.RoleUser {
			text := event.Message.Text
			update.userMessage = &text
			update.userMessageOrigin = event.UserMessageOrigin
		}
		activity(label, "")
	case agent.EventContextCompacted:
		update.status = "preparing context"
		update.context = contextObservationFromEvent(event, true, false)
		activity("Context compacted", "")
	case agent.EventContextCompactionPrepared:
		update.status = "preparing context"
		update.context = contextObservationFromEvent(event, false, true)
		activity("Context candidate prepared", "")
	case agent.EventCurrentWorldStateCaptured:
		update.status = "preparing context"
		activity("Current state captured", "")
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
		if event.CachePlan != nil {
			observation := cacheObservationFromPlan(*event.CachePlan)
			update.cache = &observation
		}
		if event.PredictiveBudget != nil {
			observation := contextObservationFromBudget(*event.PredictiveBudget)
			update.context = &observation
		}
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
		update.assistantStarted = true
		update.resetAnswer = true
	case agent.EventMessageDelta:
		update.status = "responding"
		update.answerDelta = event.Delta
	case agent.EventMessageCompleted:
		update.status = "running"
		if event.Message != nil {
			answer := event.Message.Text
			update.answer = &answer
			if event.Message.Usage != nil {
				usage := *event.Message.Usage
				update.usage = &usage
			}
		}
		update.assistantCompleted = true
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
		label := "Tool " + tool
		update.status = "running"
		if event.ToolResult != nil && event.ToolResult.IsError {
			state = activityStateFailed
			reason := safeToolFailureReason(tool, event.ToolResult)
			label += ": " + reason
			update.status = "tool failed: " + reason
			update.activity = &runActivity{
				sequence: event.Sequence,
				key:      toolActivityKey(event, tool),
				label:    label,
				state:    state,
				tool:     tool,
				failure:  reason,
			}
		} else {
			summary := safeToolSuccessSummary(tool, event.ToolResult)
			if summary != "" {
				label += ": " + summary
			}
			update.activity = &runActivity{
				sequence: event.Sequence,
				key:      toolActivityKey(event, tool),
				label:    label,
				state:    state,
				tool:     tool,
			}
		}
		applyToolWorkSummary(&update, tool, event.ToolResult)
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
		reason := safeRunFailureReason(event.Error, event.ProviderError)
		update.status = "failed"
		label := "Run failed"
		if reason != "" {
			update.status += ": " + reason
			label += ": " + reason
		}
		update.terminal = true
		activity(label, activityStateFailed)
	case agent.EventRunCanceled:
		update.status = "canceled"
		update.terminal = true
		activity("Run canceled", activityStateCanceled)
	default:
		update.status = "running"
		activity("Runtime event", "")
	}
	if update.activity != nil {
		update.activity.visibleInChat = activityVisibleInChat(event.Type)
	}
	return update
}

func activityVisibleInChat(eventType agent.EventType) bool {
	switch eventType {
	case agent.EventContextCompacted,
		agent.EventProviderRateLimitWait,
		agent.EventProviderRetry,
		agent.EventToolStarted,
		agent.EventToolCompleted,
		agent.EventRunWaiting,
		agent.EventRunCompleted,
		agent.EventRunFailed,
		agent.EventRunCanceled:
		return true
	default:
		return false
	}
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
		if runErr != nil {
			if reason := safeRunFailureReason(runErr.Error(), nil); reason != "" {
				update.status += ": " + reason
			}
		}
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

func contextObservationFromEvent(
	event agent.Event,
	compacted bool,
	prepared bool,
) *contextObservation {
	report := event.ContextCompaction
	if report == nil {
		return &contextObservation{hasCompaction: true, compacted: compacted, prepared: prepared}
	}
	var externalizedBytes int64
	for _, object := range report.Externalized {
		externalizedBytes = saturatingAddInt64(externalizedBytes, object.Bytes)
	}
	observation := &contextObservation{
		hasCompaction:     true,
		compacted:         compacted,
		prepared:          prepared,
		applied:           report.Applied,
		reason:            contextReason(report.Reason),
		originalBytes:     max(report.OriginalBytes, 0),
		compiledBytes:     max(report.CompiledBytes, 0),
		sourceMessages:    max(report.SourceMessageCount, 0),
		recentMessages:    max(report.RecentMessageCount, 0),
		externalized:      len(report.Externalized),
		externalizedBytes: externalizedBytes,
	}
	if event.ContextCheckpoint != nil {
		observation.generation = event.ContextCheckpoint.Generation
		observation.generationKnown = true
	} else if report.Validation != nil {
		observation.generation = report.Validation.CandidateGeneration
		observation.generationKnown = true
	}
	if event.PredictiveBudget != nil {
		budget := contextObservationFromBudget(*event.PredictiveBudget)
		observation.hasPredictive = true
		observation.predictiveLevel = budget.predictiveLevel
		observation.predictiveAction = budget.predictiveAction
		observation.predictedInput = budget.predictedInput
		observation.maximumInput = budget.maximumInput
	}
	return observation
}

func contextObservationFromBudget(plan agent.PredictiveBudgetPlan) contextObservation {
	return contextObservation{
		hasPredictive:    true,
		predictiveLevel:  predictiveBudgetLevel(plan.Level),
		predictiveAction: predictiveBudgetAction(plan.Action),
		predictedInput:   max(plan.ProviderInputTokenEstimate, 0),
		maximumInput:     max(plan.MaxInputTokens, 0),
	}
}

func cacheObservationFromPlan(plan agent.CachePlan) cacheObservation {
	return cacheObservation{
		mode:               cacheMode(plan.Mode),
		ttl:                cacheTTL(plan.TTL),
		breakpoints:        len(plan.Breakpoints),
		expectedReuse:      max(plan.ExpectedReuse, 0),
		inputTokenEstimate: max(plan.InputTokenEstimate, 0),
		tokenEstimateKind:  diagnosticText(plan.TokenEstimateKind),
		fallbackReason:     cacheFallbackReason(plan.FallbackReason),
	}
}

func predictiveBudgetLevel(value agent.PredictiveBudgetLevel) agent.PredictiveBudgetLevel {
	switch value {
	case agent.PredictiveBudgetWithin, agent.PredictiveBudgetSoft, agent.PredictiveBudgetHard:
		return value
	default:
		return agent.PredictiveBudgetLevel(agent.ContextReportUnrecognizedLabel)
	}
}

func predictiveBudgetAction(value agent.PredictiveBudgetAction) agent.PredictiveBudgetAction {
	switch value {
	case agent.PredictiveBudgetActionNone, agent.PredictiveBudgetActionPrepare, agent.PredictiveBudgetActionAdopt:
		return value
	default:
		return agent.PredictiveBudgetAction(agent.ContextReportUnrecognizedLabel)
	}
}

func cacheMode(value agent.CacheMode) agent.CacheMode {
	switch value {
	case agent.CacheModeDisabled, agent.CacheModeAdaptive, agent.CacheModeAutomatic, agent.CacheModeExplicit:
		return value
	default:
		return agent.CacheMode(agent.ContextReportUnrecognizedLabel)
	}
}

func cacheTTL(value agent.CacheTTL) agent.CacheTTL {
	switch value {
	case "", agent.CacheTTLFiveMinutes, agent.CacheTTLThirtyMinutes, agent.CacheTTLOneHour,
		agent.CacheTTLTwentyFourHours:
		return value
	default:
		return agent.CacheTTL(agent.ContextReportUnrecognizedLabel)
	}
}

func contextReason(value string) string {
	switch value {
	case "checkpoint", "externalize_evidence", "input_limit", "raw_event_rebase",
		"reuse_checkpoint", "validation_rollback", "predictive_budget_prepare",
		"predictive_budget_adopt":
		return value
	default:
		return agent.ContextReportUnrecognizedLabel
	}
}

func cacheFallbackReason(value string) string {
	switch value {
	case "":
		return ""
	case "explicit_cache_unsupported", "automatic_cache_unsupported", "cache_ttl_fallback",
		"no_eligible_cache_breakpoint", "explicit_cache_not_economical":
		return value
	default:
		return agent.ContextReportUnrecognizedLabel
	}
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
	approval.activityKey = approvalActivityKey(event.RunID, approval.requestID)
	update.approval = &approval
	label := "Approval for Tool " + approval.tool
	if len(approval.details) != 0 {
		label += ": " + diagnosticText(approval.details[0].label+" "+approval.details[0].value)
		if len(approval.details) > 1 {
			label += fmt.Sprintf(" (+%d detail(s))", len(approval.details)-1)
		}
	} else if approval.summary != "" {
		label += ": " + diagnosticText(approval.summary)
	}
	update.activity = &runActivity{
		sequence: event.Sequence,
		key:      approval.activityKey,
		label:    label,
		state:    activityStateWaiting,
	}
}

func safeToolFailureReason(tool string, result *agent.ToolResult) string {
	if result == nil || !result.IsError {
		return "execution error"
	}
	output := result.Output
	if strings.Contains(output, "approval was rejected") {
		return "approval denied"
	}
	if result.Policy != nil && result.Policy.Outcome == string(capability.OutcomeDeny) {
		return "permission denied"
	}
	switch {
	case strings.Contains(output, "context deadline exceeded"):
		return "operation timed out"
	case strings.Contains(output, "context canceled"):
		return "operation canceled"
	case strings.Contains(output, "input validation"),
		strings.Contains(output, "arguments are not valid JSON"):
		return "invalid arguments"
	}
	switch tool {
	case "apply_patch":
		switch {
		case strings.Contains(output, "parse apply_patch patch:"),
			strings.Contains(output, "apply_patch patch is required"),
			strings.Contains(output, "apply_patch patch exceeds"):
			return "invalid patch"
		case strings.Contains(output, "does not match the current file"):
			return "file changed since it was read"
		case strings.Contains(output, "precondition"):
			return "invalid file precondition"
		case strings.Contains(output, "apply patch to "):
			return "patch no longer applies"
		case strings.Contains(output, "decode apply_patch arguments:"):
			return "invalid arguments"
		}
	case "run_command":
		switch {
		case strings.Contains(output, `"timed_out":true`):
			return "command timed out"
		case strings.HasPrefix(strings.TrimSpace(output), `{"argv":`):
			return "command exited unsuccessfully"
		case strings.Contains(output, "decode run_command arguments:"),
			strings.Contains(output, "run_command argv"),
			strings.Contains(output, "run_command timeout_ms"):
			return "invalid arguments"
		}
	}
	if strings.Contains(output, "Extension RPC error") {
		return "Extension error"
	}
	return "execution error"
}

func safeToolSuccessSummary(tool string, result *agent.ToolResult) string {
	if result == nil || result.IsError {
		return ""
	}
	switch tool {
	case "apply_patch":
		if count, ok := safePatchChangeCount(result.Output); ok {
			return fmt.Sprintf("changed %d file(s)", count)
		}
	case "run_command":
		if result.ContextOperation != nil {
			switch result.ContextOperation.Kind {
			case agent.ContextOperationVerification:
				return "check passed"
			case agent.ContextOperationCommit:
				return "commit completed"
			case agent.ContextOperationMutation:
				return "command completed"
			}
		}
	case "git_status":
		if count, ok := safeGitStatusEntryCount(result.Output); ok {
			return fmt.Sprintf("observed %d change(s)", count)
		}
	case "git_diff":
		if truncated, ok := safeGitDiffSummary(result.Output); ok {
			if truncated {
				return "captured truncated diff"
			}
			return "captured diff"
		}
	}
	return ""
}

func applyToolWorkSummary(update *presentationUpdate, tool string, result *agent.ToolResult) {
	if update == nil {
		return
	}
	switch tool {
	case "apply_patch", "run_command", "git_status", "git_diff":
		update.observedWork = true
	}
	if result == nil {
		return
	}
	if tool == "apply_patch" && !result.IsError {
		if count, ok := safePatchChangeCount(result.Output); ok {
			update.patchedFiles = count
		}
	}
	if result.ContextOperation == nil {
		return
	}
	switch result.ContextOperation.Kind {
	case agent.ContextOperationVerification:
		if result.IsError {
			update.checksFailed = 1
		} else {
			update.checksPassed = 1
			update.resetUnverified = true
		}
	case agent.ContextOperationMutation:
		update.unverifiedActions = 1
	case agent.ContextOperationCommit:
		update.unverifiedActions = 1
	}
}

func safePatchChangeCount(output string) (int, bool) {
	if len(output) == 0 || len(output) > maximumSummaryOutputBytes {
		return 0, false
	}
	var response struct {
		Changes []struct {
			Kind string `json:"kind"`
		} `json:"changes"`
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil || len(response.Changes) == 0 || len(response.Changes) > 64 {
		return 0, false
	}
	for _, change := range response.Changes {
		switch change.Kind {
		case "add", "update", "delete":
		default:
			return 0, false
		}
	}
	return len(response.Changes), true
}

func safeGitStatusEntryCount(output string) (int, bool) {
	if len(output) == 0 || len(output) > maximumSummaryOutputBytes {
		return 0, false
	}
	var response struct {
		Entries []struct{} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil || len(response.Entries) > 1024 {
		return 0, false
	}
	return len(response.Entries), true
}

func safeGitDiffSummary(output string) (bool, bool) {
	if len(output) == 0 || len(output) > maximumSummaryOutputBytes {
		return false, false
	}
	var response struct {
		Scope     string `json:"scope"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil || response.Scope == "" {
		return false, false
	}
	return response.Truncated, true
}

func safeRunFailureReason(raw string, providerError *agent.ProviderErrorInfo) string {
	for _, known := range []struct {
		needle string
		label  string
	}{
		{agent.ErrRepeatedToolFailureLimit.Error(), "repeated Tool failures"},
		{agent.ErrProviderCallLimit.Error(), "provider call limit reached"},
		{agent.ErrToolCallLimit.Error(), "Tool call limit reached"},
		{agent.ErrBudgetProviderCalls.Error(), "shared provider call budget reached"},
		{agent.ErrBudgetToolCalls.Error(), "shared Tool call budget reached"},
		{agent.ErrBudgetInputTokens.Error(), "input token budget reached"},
		{agent.ErrBudgetOutputTokens.Error(), "output token budget reached"},
		{agent.ErrBudgetCost.Error(), "cost budget reached"},
	} {
		if strings.Contains(raw, known.needle) {
			return known.label
		}
	}
	if providerError == nil {
		return ""
	}
	switch string(providerError.Code) {
	case "rate_limited":
		return "provider rate limit"
	case "authentication":
		return "provider authentication failed"
	case "invalid_request":
		return "provider rejected the request"
	case "retryable":
		return "provider temporarily unavailable"
	case "terminal":
		return "provider request failed"
	default:
		return ""
	}
}

func decodeApprovalPrompt(wait agent.WaitRequest) (approvalPrompt, error) {
	if strings.TrimSpace(wait.ID) == "" {
		return approvalPrompt{}, errors.New("approval request ID must not be empty")
	}
	var payload struct {
		Tool                string                      `json:"tool"`
		Capabilities        []string                    `json:"capabilities"`
		ArgumentsDigest     string                      `json:"arguments_digest"`
		ExtensionID         string                      `json:"extension_id,omitempty"`
		ExtensionGeneration uint64                      `json:"extension_generation,omitempty"`
		Preview             *capability.ApprovalPreview `json:"preview,omitempty"`
	}
	if err := jsonstrict.Decode(wait.Payload, maximumApprovalPayloadBytes, &payload); err != nil {
		return approvalPrompt{}, fmt.Errorf("decode approval request: %w", err)
	}
	if strings.TrimSpace(payload.Tool) == "" {
		return approvalPrompt{}, errors.New("approval Tool must not be empty")
	}
	if err := capability.ValidateApprovalArgumentsDigest(payload.ArgumentsDigest); err != nil {
		return approvalPrompt{}, err
	}
	if payload.ExtensionGeneration != 0 && strings.TrimSpace(payload.ExtensionID) == "" {
		return approvalPrompt{}, errors.New("approval Extension generation requires an Extension ID")
	}
	if err := capability.ValidateApprovalPreview(payload.Preview); err != nil {
		return approvalPrompt{}, fmt.Errorf("validate approval preview: %w", err)
	}
	if len(payload.Capabilities) > maximumApprovalCapabilities {
		return approvalPrompt{}, fmt.Errorf("approval has %d capabilities, maximum is %d", len(payload.Capabilities), maximumApprovalCapabilities)
	}
	capabilities := make([]string, len(payload.Capabilities))
	for index, value := range payload.Capabilities {
		if err := capability.ValidateName(capability.Name(value)); err != nil {
			return approvalPrompt{}, fmt.Errorf("approval capability: %w", err)
		}
		capabilities[index] = diagnosticText(value)
	}
	approval := approvalPrompt{
		requestID:           wait.ID,
		tool:                diagnosticText(payload.Tool),
		capabilities:        capabilities,
		argumentsDigest:     payload.ArgumentsDigest,
		extensionID:         diagnosticText(payload.ExtensionID),
		extensionGeneration: payload.ExtensionGeneration,
	}
	if payload.Preview != nil {
		approval.summary = approvalPreviewText(payload.Preview.Summary)
		approval.details = make([]approvalDetail, len(payload.Preview.Details))
		for index, detail := range payload.Preview.Details {
			approval.details[index] = approvalDetail{
				label: approvalPreviewText(detail.Label),
				value: approvalPreviewText(detail.Value),
			}
		}
	}
	return approval, nil
}

func approvalPreviewText(value string) string {
	value = strings.TrimSpace(value)
	var result strings.Builder
	runes := 0
	truncated := false
	for _, current := range value {
		if runes == maximumApprovalPreviewRunes {
			truncated = true
			break
		}
		if unsafeDiagnosticRune(current) {
			current = '?'
		}
		result.WriteRune(current)
		runes++
	}
	if truncated {
		result.WriteString("...")
	}
	return result.String()
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
	callID := ""
	if event.ToolCall != nil && event.ToolCall.ID != "" {
		callID = event.ToolCall.ID
	}
	if callID == "" && event.ToolResult != nil && event.ToolResult.CallID != "" {
		callID = event.ToolResult.CallID
	}
	if callID == "" {
		callID = fmt.Sprintf("%s:%d", tool, event.Sequence)
	}
	return presentationKey("tool", event.RunID, callID)
}

func approvalActivityKey(runID, requestID string) string {
	return presentationKey("approval", runID, requestID)
}

func presentationKey(domain string, values ...string) string {
	var input strings.Builder
	input.WriteString(domain)
	input.WriteByte(0)
	for _, value := range values {
		input.WriteString(strconv.Itoa(len(value)))
		input.WriteByte(':')
		input.WriteString(value)
	}
	digest := sha256.Sum256([]byte(input.String()))
	return fmt.Sprintf("%s:%x", domain, digest)
}

func saturatingAddInt(total, value int) int {
	if value <= 0 {
		return total
	}
	if total > math.MaxInt-value {
		return math.MaxInt
	}
	return total + value
}

func saturatingAddInt64(total, value int64) int64 {
	if value <= 0 {
		return total
	}
	if total > math.MaxInt64-value {
		return math.MaxInt64
	}
	return total + value
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
		if unsafeDiagnosticRune(current) {
			current = '?'
		}
		result.WriteRune(current)
		runes++
	}
	return result.String()
}

func unsafeDiagnosticRune(value rune) bool {
	return unicode.IsControl(value) || unicode.In(value, unicode.Cf)
}
