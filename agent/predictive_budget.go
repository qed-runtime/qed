package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// PredictiveBudgetLevel identifies the highest configured threshold reached by
// one logical Provider request before any prepared candidate is adopted
type PredictiveBudgetLevel string

const (
	// PredictiveBudgetWithin means the original request is below the soft threshold
	PredictiveBudgetWithin PredictiveBudgetLevel = "within"
	// PredictiveBudgetSoft means Runtime may prepare a candidate but can safely use the original request
	PredictiveBudgetSoft PredictiveBudgetLevel = "soft"
	// PredictiveBudgetHard means the original request must not reach the Provider
	PredictiveBudgetHard PredictiveBudgetLevel = "hard"
)

// PredictiveBudgetAction identifies the Runtime decision taken for one
// predictive preflight
type PredictiveBudgetAction string

const (
	// PredictiveBudgetActionNone means Runtime prepared or adopted no candidate
	PredictiveBudgetActionNone PredictiveBudgetAction = "none"
	// PredictiveBudgetActionPrepare means a validated inactive candidate is ready
	PredictiveBudgetActionPrepare PredictiveBudgetAction = "prepare"
	// PredictiveBudgetActionAdopt means a validated candidate replaced the hard-limit request
	PredictiveBudgetActionAdopt PredictiveBudgetAction = "adopt"
)

// PredictiveBudgetPolicy reserves model context before each Provider request
//
// ContextWindowTokens, OutputReserveTokens, and SoftThresholdTokens must be
// positive. SafetyMarginTokens and PredictedToolOutputTokens may be zero.
// SoftThresholdTokens must be below ContextWindowTokens and above the fixed
// reserve plus predicted Tool output. Model limits are operator-supplied facts
// and should be updated when the selected Provider or model changes.
type PredictiveBudgetPolicy struct {
	// ContextWindowTokens is the model's complete input and output context limit
	ContextWindowTokens int64 `json:"context_window_tokens"`
	// OutputReserveTokens reserves space for the next model response
	OutputReserveTokens int64 `json:"output_reserve_tokens"`
	// SafetyMarginTokens is an alternate minimum reserve for estimation error
	SafetyMarginTokens int64 `json:"safety_margin_tokens,omitempty"`
	// PredictedToolOutputTokens reserves space for the next likely Tool result
	PredictedToolOutputTokens int64 `json:"predicted_tool_output_tokens,omitempty"`
	// SoftThresholdTokens starts validated candidate preparation before the hard limit
	SoftThresholdTokens int64 `json:"soft_threshold_tokens"`
}

// PredictiveBudgetPlan is the content-free preflight result for one logical
// Provider request
type PredictiveBudgetPlan struct {
	// Level identifies the threshold reached by the original compiled input
	Level PredictiveBudgetLevel `json:"level"`
	// Action identifies whether Runtime prepared or adopted a validated candidate
	Action PredictiveBudgetAction `json:"action"`
	// ContextWindowTokens is copied from the effective policy
	ContextWindowTokens int64 `json:"context_window_tokens"`
	// SoftThresholdTokens is copied from the effective policy
	SoftThresholdTokens int64 `json:"soft_threshold_tokens"`
	// OutputReserveTokens is copied from the effective policy
	OutputReserveTokens int64 `json:"output_reserve_tokens"`
	// SafetyMarginTokens is copied from the effective policy
	SafetyMarginTokens int64 `json:"safety_margin_tokens,omitempty"`
	// RequiredReserveTokens is max(OutputReserveTokens, SafetyMarginTokens)
	RequiredReserveTokens int64 `json:"required_reserve_tokens"`
	// PredictedToolOutputTokens is copied from the effective policy
	PredictedToolOutputTokens int64 `json:"predicted_tool_output_tokens,omitempty"`
	// MaxInputTokens is the hard input allowance after fixed reservations
	MaxInputTokens int64 `json:"max_input_tokens"`
	// InputTokenEstimate is the original compiled input estimate
	InputTokenEstimate int64 `json:"input_token_estimate"`
	// CandidateInputTokenEstimate is the candidate estimate, or the original
	// estimate when no candidate exists
	CandidateInputTokenEstimate int64 `json:"candidate_input_token_estimate"`
	// ProviderInputTokenEstimate is the selected Provider input estimate
	//
	// No Provider request occurs when a hard plan remains unadopted.
	ProviderInputTokenEstimate int64 `json:"provider_input_token_estimate"`
	// PredictedTotalTokens is the original input plus Tool and required reserves
	PredictedTotalTokens int64 `json:"predicted_total_tokens"`
	// CandidatePredictedTotalTokens is the candidate total, or the original total
	// when no candidate exists
	CandidatePredictedTotalTokens int64 `json:"candidate_predicted_total_tokens"`
	// ProviderPredictedTotalTokens is the selected Provider input plus fixed reserves
	ProviderPredictedTotalTokens int64 `json:"provider_predicted_total_tokens"`
	// TokenEstimateKind identifies the estimator used for both input values
	TokenEstimateKind string `json:"token_estimate_kind"`
	// CandidateGeneration identifies the prepared or adopted Checkpoint when present
	CandidateGeneration uint64 `json:"candidate_generation,omitempty"`
}

// PreparedContextCandidate is a validated but inactive Checkpoint prepared at a
// soft Predictive Budget threshold
type PreparedContextCandidate struct {
	// Checkpoint is the inactive candidate that may be adopted at a hard threshold
	Checkpoint ContextCheckpoint `json:"checkpoint"`
	// Compaction describes how the candidate was built and validated
	Compaction ContextCompactionReport `json:"compaction"`
	// Budget is the soft-threshold plan that caused preparation
	Budget PredictiveBudgetPlan `json:"budget"`
}

// PredictiveContextCompiler can build a validated view below one estimated
// input-token limit
//
// CompileToTokenLimit must preserve the same identity and validation guarantees
// as ContextCompiler. Runtime independently verifies the returned estimate and
// refuses a hard-limit Provider call when the result does not fit.
type PredictiveContextCompiler interface {
	ContextCompiler
	CompileToTokenLimit(
		ctx context.Context,
		request ContextCompileRequest,
		maxInputTokens int64,
	) (CompiledContext, error)
}

// BuildPredictiveBudgetPlan validates a policy and evaluates one input estimate
func BuildPredictiveBudgetPlan(
	policy PredictiveBudgetPolicy,
	inputTokenEstimate int64,
	tokenEstimateKind string,
) (PredictiveBudgetPlan, error) {
	normalized, err := normalizePredictiveBudgetPolicy(policy)
	if err != nil {
		return PredictiveBudgetPlan{}, err
	}
	if inputTokenEstimate < 0 {
		return PredictiveBudgetPlan{}, errors.New("Predictive Budget input estimate must not be negative")
	}
	if !validTokenEstimateKind(tokenEstimateKind) {
		return PredictiveBudgetPlan{}, errors.New("Predictive Budget Token Estimate Kind is invalid")
	}
	requiredReserve := max(normalized.OutputReserveTokens, normalized.SafetyMarginTokens)
	fixed, err := predictiveBudgetAdd(requiredReserve, normalized.PredictedToolOutputTokens)
	if err != nil {
		return PredictiveBudgetPlan{}, err
	}
	predicted, err := predictiveBudgetAdd(inputTokenEstimate, fixed)
	if err != nil {
		return PredictiveBudgetPlan{}, err
	}
	level := PredictiveBudgetWithin
	if predicted > normalized.ContextWindowTokens {
		level = PredictiveBudgetHard
	} else if predicted >= normalized.SoftThresholdTokens {
		level = PredictiveBudgetSoft
	}
	return PredictiveBudgetPlan{
		Level:                         level,
		Action:                        PredictiveBudgetActionNone,
		ContextWindowTokens:           normalized.ContextWindowTokens,
		SoftThresholdTokens:           normalized.SoftThresholdTokens,
		OutputReserveTokens:           normalized.OutputReserveTokens,
		SafetyMarginTokens:            normalized.SafetyMarginTokens,
		RequiredReserveTokens:         requiredReserve,
		PredictedToolOutputTokens:     normalized.PredictedToolOutputTokens,
		MaxInputTokens:                normalized.ContextWindowTokens - fixed,
		InputTokenEstimate:            inputTokenEstimate,
		CandidateInputTokenEstimate:   inputTokenEstimate,
		ProviderInputTokenEstimate:    inputTokenEstimate,
		PredictedTotalTokens:          predicted,
		CandidatePredictedTotalTokens: predicted,
		ProviderPredictedTotalTokens:  predicted,
		TokenEstimateKind:             tokenEstimateKind,
	}, nil
}

func normalizePredictiveBudgetPolicy(policy PredictiveBudgetPolicy) (*PredictiveBudgetPolicy, error) {
	if policy.ContextWindowTokens <= 0 {
		return nil, errors.New("Predictive Budget context window must be positive")
	}
	if policy.OutputReserveTokens <= 0 {
		return nil, errors.New("Predictive Budget output reserve must be positive")
	}
	if policy.SafetyMarginTokens < 0 || policy.PredictedToolOutputTokens < 0 {
		return nil, errors.New("Predictive Budget safety margin and predicted Tool output must not be negative")
	}
	if policy.SoftThresholdTokens <= 0 || policy.SoftThresholdTokens >= policy.ContextWindowTokens {
		return nil, errors.New("Predictive Budget soft threshold must be positive and below the context window")
	}
	requiredReserve := max(policy.OutputReserveTokens, policy.SafetyMarginTokens)
	fixed, err := predictiveBudgetAdd(requiredReserve, policy.PredictedToolOutputTokens)
	if err != nil {
		return nil, err
	}
	if fixed >= policy.ContextWindowTokens {
		return nil, errors.New("Predictive Budget reserves leave no model input capacity")
	}
	if policy.SoftThresholdTokens <= fixed {
		return nil, errors.New("Predictive Budget soft threshold must exceed fixed reserves")
	}
	normalized := policy
	return &normalized, nil
}

func normalizePredictiveBudgetPolicyOption(policy *PredictiveBudgetPolicy) (*PredictiveBudgetPolicy, error) {
	if policy == nil {
		return nil, nil
	}
	return normalizePredictiveBudgetPolicy(*policy)
}

func validatePredictiveBudgetPlan(plan PredictiveBudgetPlan) error {
	policy := PredictiveBudgetPolicy{
		ContextWindowTokens:       plan.ContextWindowTokens,
		OutputReserveTokens:       plan.OutputReserveTokens,
		SafetyMarginTokens:        plan.SafetyMarginTokens,
		PredictedToolOutputTokens: plan.PredictedToolOutputTokens,
		SoftThresholdTokens:       plan.SoftThresholdTokens,
	}
	want, err := BuildPredictiveBudgetPlan(policy, plan.InputTokenEstimate, plan.TokenEstimateKind)
	if err != nil {
		return err
	}
	if plan.Level != want.Level || plan.RequiredReserveTokens != want.RequiredReserveTokens ||
		plan.MaxInputTokens != want.MaxInputTokens || plan.PredictedTotalTokens != want.PredictedTotalTokens {
		return errors.New("Predictive Budget plan does not match its policy and input estimate")
	}
	switch plan.Action {
	case PredictiveBudgetActionNone, PredictiveBudgetActionPrepare, PredictiveBudgetActionAdopt:
	default:
		return fmt.Errorf("Predictive Budget action %q is invalid", plan.Action)
	}
	if plan.CandidateInputTokenEstimate < 0 || plan.ProviderInputTokenEstimate < 0 {
		return errors.New("Predictive Budget candidate and Provider input estimates must not be negative")
	}
	fixed := plan.RequiredReserveTokens + plan.PredictedToolOutputTokens
	candidateTotal, err := predictiveBudgetAdd(plan.CandidateInputTokenEstimate, fixed)
	if err != nil {
		return err
	}
	providerTotal, err := predictiveBudgetAdd(plan.ProviderInputTokenEstimate, fixed)
	if err != nil {
		return err
	}
	if plan.CandidatePredictedTotalTokens != candidateTotal ||
		plan.ProviderPredictedTotalTokens != providerTotal {
		return errors.New("Predictive Budget candidate or Provider total does not match its inputs")
	}
	if plan.Action == PredictiveBudgetActionPrepare {
		if plan.Level != PredictiveBudgetSoft || plan.CandidateGeneration == 0 ||
			plan.CandidatePredictedTotalTokens >= plan.SoftThresholdTokens ||
			plan.ProviderInputTokenEstimate != plan.InputTokenEstimate {
			return errors.New("Predictive Budget preparation requires a soft threshold and candidate")
		}
	}
	if plan.Action == PredictiveBudgetActionAdopt {
		if plan.Level != PredictiveBudgetHard || plan.CandidateGeneration == 0 ||
			plan.CandidateInputTokenEstimate > plan.MaxInputTokens ||
			plan.ProviderInputTokenEstimate != plan.CandidateInputTokenEstimate {
			return errors.New("Predictive Budget adoption requires a fitting hard-threshold candidate")
		}
	}
	if plan.Action == PredictiveBudgetActionNone && (plan.CandidateGeneration != 0 ||
		plan.CandidateInputTokenEstimate != plan.InputTokenEstimate ||
		plan.ProviderInputTokenEstimate != plan.InputTokenEstimate) {
		return errors.New("Predictive Budget plan without an action must retain the original input")
	}
	return nil
}

func validatePredictiveBudgetEvent(event Event) error {
	if event.PredictiveBudget == nil {
		if event.Type == EventContextCompactionPrepared {
			return errors.New("context.compaction.prepared requires a Predictive Budget plan")
		}
		return nil
	}
	if err := validatePredictiveBudgetPlan(*event.PredictiveBudget); err != nil {
		return err
	}
	switch event.Type {
	case EventModelRequest:
		if event.CachePlan == nil ||
			event.CachePlan.InputTokenEstimate != event.PredictiveBudget.ProviderInputTokenEstimate ||
			event.CachePlan.TokenEstimateKind != event.PredictiveBudget.TokenEstimateKind {
			return errors.New("model.request.started Predictive Budget does not match its Cache Plan")
		}
		if event.PredictiveBudget.Level == PredictiveBudgetHard &&
			event.PredictiveBudget.Action != PredictiveBudgetActionAdopt {
			return errors.New("model.request.started must not use an unadopted hard Predictive Budget")
		}
		return nil
	case EventContextCompactionPrepared:
		if event.PredictiveBudget.Action != PredictiveBudgetActionPrepare ||
			event.ContextCheckpoint == nil || event.ContextCompaction == nil ||
			event.ContextCompaction.Reason != "predictive_budget_prepare" ||
			event.ContextCheckpoint.Generation != event.PredictiveBudget.CandidateGeneration {
			return errors.New("context.compaction.prepared has an invalid Predictive Budget transition")
		}
		return nil
	case EventContextCompacted:
		if event.PredictiveBudget.Action != PredictiveBudgetActionAdopt ||
			event.ContextCheckpoint == nil || event.ContextCompaction == nil ||
			event.ContextCompaction.Reason != "predictive_budget_adopt" ||
			event.ContextCheckpoint.Generation != event.PredictiveBudget.CandidateGeneration {
			return errors.New("context.compacted has an invalid Predictive Budget adoption")
		}
		return nil
	default:
		return fmt.Errorf("Event %q must not contain a Predictive Budget plan", event.Type)
	}
}

func predictiveBudgetWithDecision(
	plan PredictiveBudgetPlan,
	action PredictiveBudgetAction,
	compiledInput int64,
	candidateGeneration uint64,
) (PredictiveBudgetPlan, error) {
	plan.Action = action
	plan.CandidateInputTokenEstimate = compiledInput
	plan.CandidateGeneration = candidateGeneration
	total, err := predictiveBudgetAdd(
		compiledInput,
		plan.RequiredReserveTokens+plan.PredictedToolOutputTokens,
	)
	if err != nil {
		return PredictiveBudgetPlan{}, err
	}
	plan.CandidatePredictedTotalTokens = total
	if action == PredictiveBudgetActionAdopt {
		plan.ProviderInputTokenEstimate = compiledInput
		plan.ProviderPredictedTotalTokens = total
	}
	if err := validatePredictiveBudgetPlan(plan); err != nil {
		return PredictiveBudgetPlan{}, err
	}
	return plan, nil
}

func predictiveBudgetAdd(first, second int64) (int64, error) {
	if first < 0 || second < 0 || second > math.MaxInt64-first {
		return 0, errors.New("Predictive Budget value overflow")
	}
	return first + second, nil
}

func clonePredictiveBudgetPlan(plan *PredictiveBudgetPlan) *PredictiveBudgetPlan {
	if plan == nil {
		return nil
	}
	cloned := *plan
	return &cloned
}

func clonePreparedContextCandidate(candidate *PreparedContextCandidate) *PreparedContextCandidate {
	if candidate == nil {
		return nil
	}
	return &PreparedContextCandidate{
		Checkpoint: *cloneContextCheckpointPointer(&candidate.Checkpoint),
		Compaction: *cloneContextCompactionReport(&candidate.Compaction),
		Budget:     candidate.Budget,
	}
}
