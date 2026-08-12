package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
)

const (
	// TokenUsageReportVersion is the content-free estimate comparison schema
	TokenUsageReportVersion uint32 = 1
)

// TokenUsageOutcome identifies how one Provider attempt ended
type TokenUsageOutcome string

// Token Usage outcomes
const (
	// TokenUsageCompleted identifies a completed assistant response
	TokenUsageCompleted TokenUsageOutcome = "completed"
	// TokenUsageRetry identifies a failed attempt followed by a scheduled retry
	TokenUsageRetry TokenUsageOutcome = "retry"
	// TokenUsageFailed identifies a terminal failed Provider attempt
	TokenUsageFailed TokenUsageOutcome = "failed"
	// TokenUsageCanceled identifies a canceled Provider attempt
	TokenUsageCanceled TokenUsageOutcome = "canceled"
)

// TokenUsageObservation compares one request estimate with Provider Usage
type TokenUsageObservation struct {
	// RunID identifies the observed Run
	RunID string `json:"run_id"`
	// ProviderCall identifies the one-based attempt within the Run
	ProviderCall int `json:"provider_call"`
	// ProviderAttempt identifies the attempt within one logical model request
	ProviderAttempt int `json:"provider_attempt"`
	// RequestEventSequence identifies model.request.started
	RequestEventSequence uint64 `json:"request_event_sequence"`
	// CompletionEventSequence identifies the closing Event
	CompletionEventSequence uint64 `json:"completion_event_sequence"`
	// Provider is copied from the content-free Prefix Manifest
	Provider string `json:"provider"`
	// Model is copied from the content-free Prefix Manifest
	Model string `json:"model,omitempty"`
	// Outcome identifies completion, retry, failure, or cancellation
	Outcome TokenUsageOutcome `json:"outcome"`
	// EstimatedInputTokens is the pre-request logical Context estimate
	EstimatedInputTokens int64 `json:"estimated_input_tokens"`
	// TokenEstimateKind identifies the tokenizer or canonical fallback
	TokenEstimateKind string `json:"token_estimate_kind"`
	// ProviderInputTokens is present only when Provider Usage reported input
	ProviderInputTokens *int64 `json:"provider_input_tokens,omitempty"`
	// DifferenceTokens is ProviderInputTokens minus EstimatedInputTokens
	DifferenceTokens *int64 `json:"difference_tokens,omitempty"`
}

// TokenUsageMetrics aggregates only content-free request and comparison counts
type TokenUsageMetrics struct {
	// RequestCount is the number of estimated Provider attempts
	RequestCount int64 `json:"request_count"`
	// ProviderUsageReportedCount is the number of comparable attempts
	ProviderUsageReportedCount int64 `json:"provider_usage_reported_count"`
	// ProviderUsageMissingCount is the number without reported input Usage
	ProviderUsageMissingCount int64 `json:"provider_usage_missing_count"`
	// ComparableEstimatedInputTokens sums estimates only for reported attempts
	ComparableEstimatedInputTokens int64 `json:"comparable_estimated_input_tokens"`
	// ProviderInputTokens sums reported input Usage
	ProviderInputTokens int64 `json:"provider_input_tokens"`
	// DifferenceTokens is ProviderInputTokens minus comparable estimates
	DifferenceTokens int64 `json:"difference_tokens"`
}

// TokenUsageReport contains per-attempt estimate comparisons for one Run
type TokenUsageReport struct {
	// Version identifies the report schema
	Version uint32 `json:"version"`
	// RunID identifies the selected Run
	RunID string `json:"run_id"`
	// Observations preserve Provider attempt order
	Observations []TokenUsageObservation `json:"observations"`
	// Metrics aggregates comparable estimates and Provider Usage
	Metrics TokenUsageMetrics `json:"metrics"`
}

// BuildTokenUsageReport reconstructs token estimate accuracy from public Events
//
// The report contains no prompt, message, Tool, or metadata content. Events for
// the selected Run must start with run.started, retain their complete contiguous
// sequence, and contain no Events after a terminal Event. A Provider attempt
// without Usage remains an explicit unreported observation
func BuildTokenUsageReport(
	ctx context.Context,
	runID string,
	events []Event,
) (TokenUsageReport, error) {
	if ctx == nil {
		return TokenUsageReport{}, errors.New("Token Usage report context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return TokenUsageReport{}, err
	}
	if strings.TrimSpace(runID) == "" {
		return TokenUsageReport{}, errors.New("Token Usage report Run ID is required")
	}
	report := TokenUsageReport{
		Version:      TokenUsageReportVersion,
		RunID:        runID,
		Observations: make([]TokenUsageObservation, 0),
	}
	expectedSequence := uint64(0)
	expectedProviderCall := 0
	expectedProviderAttempt := 1
	selectedEventCount := 0
	terminal := false
	pendingOpen := false
	pendingProviderCall := 0
	pendingProviderAttempt := 0
	pendingObservation := -1
	for index := range events {
		if err := ctx.Err(); err != nil {
			return TokenUsageReport{}, err
		}
		event := events[index]
		if event.RunID != runID {
			continue
		}
		if terminal {
			return TokenUsageReport{}, errors.New("Token Usage report contains an Event after Run termination")
		}
		selectedEventCount++
		if expectedSequence == math.MaxUint64 || event.Sequence != expectedSequence+1 {
			return TokenUsageReport{}, fmt.Errorf(
				"Token Usage report Run %q Event sequence = %d, want %d",
				runID,
				event.Sequence,
				expectedSequence+1,
			)
		}
		expectedSequence = event.Sequence
		if selectedEventCount == 1 && event.Type != EventRunStarted {
			return TokenUsageReport{}, errors.New("Token Usage report must start with run.started")
		}
		switch event.Type {
		case EventRunStarted:
			if selectedEventCount != 1 {
				return TokenUsageReport{}, errors.New("Token Usage report contains duplicate run.started")
			}

		case EventModelRequest:
			if pendingOpen {
				return TokenUsageReport{}, errors.New("Token Usage report contains overlapping Provider attempts")
			}
			if event.ProviderCall != expectedProviderCall+1 ||
				event.ProviderAttempt != expectedProviderAttempt ||
				event.PrefixManifest == nil ||
				strings.TrimSpace(event.PrefixManifest.Provider) != event.PrefixManifest.Provider ||
				event.PrefixManifest.Provider == "" ||
				strings.TrimSpace(event.PrefixManifest.Model) != event.PrefixManifest.Model {
				return TokenUsageReport{}, errors.New("Token Usage report model request is incomplete")
			}
			pendingOpen = true
			pendingProviderCall = event.ProviderCall
			pendingProviderAttempt = event.ProviderAttempt
			pendingObservation = -1
			expectedProviderCall = event.ProviderCall
			expectedProviderAttempt = 0
			if event.CachePlan == nil {
				continue
			}
			if event.CachePlan.InputTokenEstimate < 0 || !validTokenEstimateKind(event.CachePlan.TokenEstimateKind) {
				return TokenUsageReport{}, errors.New("Token Usage report model request has an invalid estimate")
			}
			report.Observations = append(report.Observations, TokenUsageObservation{
				RunID: runID, ProviderCall: event.ProviderCall, ProviderAttempt: event.ProviderAttempt,
				RequestEventSequence: event.Sequence, Provider: event.PrefixManifest.Provider,
				Model: event.PrefixManifest.Model, EstimatedInputTokens: event.CachePlan.InputTokenEstimate,
				TokenEstimateKind: event.CachePlan.TokenEstimateKind,
			})
			pendingObservation = len(report.Observations) - 1

		case EventProviderRetry:
			if !pendingOpen || event.ProviderCall != pendingProviderCall ||
				event.ProviderAttempt != pendingProviderAttempt || event.ProviderRetry == nil ||
				event.ProviderRetry.NextAttempt != pendingProviderAttempt+1 ||
				event.ProviderRetry.DelayMilliseconds < 0 {
				return TokenUsageReport{}, errors.New("Token Usage report retry has no matching Provider attempt")
			}
			if pendingObservation >= 0 {
				report.Observations[pendingObservation].Outcome = TokenUsageRetry
				report.Observations[pendingObservation].CompletionEventSequence = event.Sequence
			}
			pendingOpen = false
			pendingProviderAttempt = 0
			pendingObservation = -1
			expectedProviderAttempt = event.ProviderRetry.NextAttempt

		case EventMessageCompleted:
			if !pendingOpen || event.Message == nil || event.Message.Role != RoleAssistant {
				return TokenUsageReport{}, errors.New("Token Usage report completion has no matching Provider attempt")
			}
			if pendingObservation >= 0 {
				observation := &report.Observations[pendingObservation]
				observation.Outcome = TokenUsageCompleted
				observation.CompletionEventSequence = event.Sequence
				if event.Message.Usage != nil {
					if err := validateUsage(event.Message.Usage); err != nil {
						return TokenUsageReport{}, fmt.Errorf("Token Usage report Provider Usage: %w", err)
					}
					actual := event.Message.Usage.InputTokens
					difference := actual - observation.EstimatedInputTokens
					observation.ProviderInputTokens = &actual
					observation.DifferenceTokens = &difference
				}
			}
			pendingOpen = false
			pendingProviderAttempt = 0
			pendingObservation = -1
			expectedProviderAttempt = 1

		case EventRunCompleted:
			if pendingOpen {
				return TokenUsageReport{}, errors.New("Token Usage report completed Run has a pending Provider attempt")
			}
			terminal = true

		case EventRunFailed, EventRunCanceled:
			if pendingOpen && pendingObservation >= 0 {
				outcome := TokenUsageFailed
				if event.Type == EventRunCanceled {
					outcome = TokenUsageCanceled
				}
				report.Observations[pendingObservation].Outcome = outcome
				report.Observations[pendingObservation].CompletionEventSequence = event.Sequence
			}
			pendingOpen = false
			pendingProviderAttempt = 0
			pendingObservation = -1
			terminal = true
		}
	}
	if selectedEventCount == 0 {
		return TokenUsageReport{}, fmt.Errorf("Token Usage report Run %q was not found", runID)
	}
	if pendingOpen {
		return TokenUsageReport{}, errors.New("Token Usage report ends with a pending Provider attempt")
	}
	if err := aggregateTokenUsageReport(&report); err != nil {
		return TokenUsageReport{}, err
	}
	return report, nil
}

// Latest returns the final Provider attempt observation
func (report TokenUsageReport) Latest() (TokenUsageObservation, bool) {
	if len(report.Observations) == 0 {
		return TokenUsageObservation{}, false
	}
	return report.Observations[len(report.Observations)-1], true
}

func aggregateTokenUsageReport(report *TokenUsageReport) error {
	report.Metrics.RequestCount = int64(len(report.Observations))
	for index := range report.Observations {
		observation := &report.Observations[index]
		if observation.Outcome == "" || observation.CompletionEventSequence == 0 {
			return errors.New("Token Usage report contains an incomplete observation")
		}
		if observation.ProviderInputTokens == nil {
			report.Metrics.ProviderUsageMissingCount++
			continue
		}
		if observation.DifferenceTokens == nil ||
			*observation.DifferenceTokens != *observation.ProviderInputTokens-observation.EstimatedInputTokens {
			return errors.New("Token Usage report contains an invalid difference")
		}
		report.Metrics.ProviderUsageReportedCount++
		var err error
		report.Metrics.ComparableEstimatedInputTokens, err = addTokenMetric(
			report.Metrics.ComparableEstimatedInputTokens,
			observation.EstimatedInputTokens,
		)
		if err != nil {
			return err
		}
		report.Metrics.ProviderInputTokens, err = addTokenMetric(
			report.Metrics.ProviderInputTokens,
			*observation.ProviderInputTokens,
		)
		if err != nil {
			return err
		}
	}
	report.Metrics.DifferenceTokens =
		report.Metrics.ProviderInputTokens - report.Metrics.ComparableEstimatedInputTokens
	return nil
}

func addTokenMetric(total, value int64) (int64, error) {
	if value < 0 || total > math.MaxInt64-value {
		return 0, errors.New("Token Usage report metric overflows int64")
	}
	return total + value, nil
}
