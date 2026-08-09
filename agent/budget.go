package agent

import (
	"errors"
	"sync"
	"time"
)

var (
	// ErrBudgetProviderCalls indicates that a shared Run budget exhausted Provider calls
	ErrBudgetProviderCalls = errors.New("run budget provider call limit reached")
	// ErrBudgetToolCalls indicates that a shared Run budget exhausted Tool calls
	ErrBudgetToolCalls = errors.New("run budget tool call limit reached")
	// ErrBudgetInputTokens indicates that reported input usage exceeded a Run budget
	ErrBudgetInputTokens = errors.New("run budget input token limit reached")
	// ErrBudgetOutputTokens indicates that reported output usage exceeded a Run budget
	ErrBudgetOutputTokens = errors.New("run budget output token limit reached")
	// ErrBudgetCost indicates that reported cost exceeded a Run budget
	ErrBudgetCost = errors.New("run budget cost limit reached")
)

// BudgetLimits configures a concurrency-safe budget shared by one or more Runs
type BudgetLimits struct {
	MaxDuration      time.Duration `json:"max_duration,omitempty"`
	MaxProviderCalls int           `json:"max_provider_calls,omitempty"`
	MaxToolCalls     int           `json:"max_tool_calls,omitempty"`
	MaxInputTokens   int64         `json:"max_input_tokens,omitempty"`
	MaxOutputTokens  int64         `json:"max_output_tokens,omitempty"`
	MaxCostMicros    int64         `json:"max_cost_micros,omitempty"`
}

// BudgetSnapshot reports consumed resources without exposing mutable state
type BudgetSnapshot struct {
	StartedAt     time.Time `json:"started_at"`
	ProviderCalls int       `json:"provider_calls"`
	ToolCalls     int       `json:"tool_calls"`
	InputTokens   int64     `json:"input_tokens"`
	OutputTokens  int64     `json:"output_tokens"`
	CostMicros    int64     `json:"cost_micros,omitempty"`
}

// Budget enforces resource limits across concurrent Agent Runs
//
// A Budget may be shared by an orchestration. Limits are consumed atomically.
// Token and cost limits are evaluated after Providers report Usage, so a single
// response can exceed a limit before the Runtime terminates.
type Budget struct {
	limits BudgetLimits

	mu            sync.Mutex
	startedAt     time.Time
	providerCalls int
	toolCalls     int
	inputTokens   int64
	outputTokens  int64
	costMicros    int64
}

// NewBudget validates limits and starts a shared budget clock
func NewBudget(limits BudgetLimits) (*Budget, error) {
	if limits.MaxDuration < 0 || limits.MaxProviderCalls < 0 || limits.MaxToolCalls < 0 ||
		limits.MaxInputTokens < 0 || limits.MaxOutputTokens < 0 || limits.MaxCostMicros < 0 {
		return nil, errors.New("run budget limits must not be negative")
	}
	return &Budget{limits: limits, startedAt: time.Now().UTC()}, nil
}

// Limits returns an immutable copy of the configured budget limits
func (budget *Budget) Limits() BudgetLimits {
	if budget == nil {
		return BudgetLimits{}
	}
	return budget.limits
}

// Deadline returns the shared duration deadline and whether one is configured
func (budget *Budget) Deadline() (time.Time, bool) {
	if budget == nil || budget.limits.MaxDuration == 0 {
		return time.Time{}, false
	}
	return budget.startedAt.Add(budget.limits.MaxDuration), true
}

// Snapshot returns the current shared budget consumption
func (budget *Budget) Snapshot() BudgetSnapshot {
	if budget == nil {
		return BudgetSnapshot{}
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return BudgetSnapshot{
		StartedAt:     budget.startedAt,
		ProviderCalls: budget.providerCalls,
		ToolCalls:     budget.toolCalls,
		InputTokens:   budget.inputTokens,
		OutputTokens:  budget.outputTokens,
		CostMicros:    budget.costMicros,
	}
}

func (budget *Budget) consumeProviderCall() error {
	if budget == nil {
		return nil
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.limits.MaxProviderCalls > 0 && budget.providerCalls >= budget.limits.MaxProviderCalls {
		return ErrBudgetProviderCalls
	}
	budget.providerCalls++
	return nil
}

func (budget *Budget) consumeToolCalls(count int) error {
	if budget == nil {
		return nil
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.limits.MaxToolCalls > 0 && budget.toolCalls+count > budget.limits.MaxToolCalls {
		return ErrBudgetToolCalls
	}
	budget.toolCalls += count
	return nil
}

func (budget *Budget) recordUsage(usage *Usage) error {
	if budget == nil || usage == nil {
		return nil
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	budget.inputTokens += usage.InputTokens
	budget.outputTokens += usage.OutputTokens
	budget.costMicros += usage.CostMicros
	if budget.limits.MaxInputTokens > 0 && budget.inputTokens > budget.limits.MaxInputTokens {
		return ErrBudgetInputTokens
	}
	if budget.limits.MaxOutputTokens > 0 && budget.outputTokens > budget.limits.MaxOutputTokens {
		return ErrBudgetOutputTokens
	}
	if budget.limits.MaxCostMicros > 0 && budget.costMicros > budget.limits.MaxCostMicros {
		return ErrBudgetCost
	}
	return nil
}
