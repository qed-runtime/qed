package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// ContextOperationKind identifies one host-observed operation relevant to safe context cuts
type ContextOperationKind string

// Context operation kinds supported by the compacting Context Compiler
const (
	// ContextOperationMutation reports an operation that may have changed durable state
	ContextOperationMutation ContextOperationKind = "mutation"
	// ContextOperationVerification reports an operation that checked state after a mutation
	ContextOperationVerification ContextOperationKind = "verification"
	// ContextOperationCommit reports an operation that attempted to publish one durable revision
	ContextOperationCommit ContextOperationKind = "commit"
	// ContextOperationSubagent reports a delegated Agent transaction
	ContextOperationSubagent ContextOperationKind = "subagent"
)

// ContextOperation is content-free host metadata used to protect semantic transactions
//
// Extensions may return this metadata with a ToolResult. Runtime validates it,
// persists it in the Tool completion Event, and never sends it to the model.
type ContextOperation struct {
	// Kind identifies the observed operation
	Kind ContextOperationKind `json:"kind"`
}

type contextProtectedRange struct {
	start int
	end   int
}

type contextToolTransaction struct {
	start     int
	result    int
	operation *ContextOperation
}

type contextSafeCutPlan struct {
	messages  int
	protected []contextProtectedRange
}

// ValidateContextOperation rejects unknown operation kinds at host boundaries
func ValidateContextOperation(operation *ContextOperation) error {
	if operation == nil {
		return nil
	}
	switch operation.Kind {
	case ContextOperationMutation, ContextOperationVerification,
		ContextOperationCommit, ContextOperationSubagent:
		return nil
	default:
		return fmt.Errorf("unsupported Context operation kind %q", operation.Kind)
	}
}

func cloneContextOperation(operation *ContextOperation) *ContextOperation {
	if operation == nil {
		return nil
	}
	cloned := *operation
	return &cloned
}

func buildContextSafeCutPlan(
	ctx context.Context,
	messages []Message,
	events []Event,
	ledger *ContextLedger,
) (contextSafeCutPlan, error) {
	if ctx == nil {
		return contextSafeCutPlan{}, errors.New("safe cut context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return contextSafeCutPlan{}, err
	}
	transactions, err := contextToolTransactions(ctx, messages)
	if err != nil {
		return contextSafeCutPlan{}, err
	}
	plan := contextSafeCutPlan{messages: len(messages)}
	for _, transaction := range transactions {
		plan.protected = append(plan.protected, contextProtectedRange{
			start: transaction.start,
			end:   transaction.result,
		})
	}
	if len(events) == 0 {
		plan.normalize()
		return plan, nil
	}
	if ledger == nil {
		return contextSafeCutPlan{}, errors.New("safe cut Events require a Context Ledger")
	}
	if err := ValidateContextLedger(ctx, *ledger, events); err != nil {
		return contextSafeCutPlan{}, fmt.Errorf("validate safe cut Event prefix: %w", err)
	}
	operations, err := contextOperationsByMessage(ctx, messages, events)
	if err != nil {
		return contextSafeCutPlan{}, err
	}
	for index := range transactions {
		transactions[index].operation = cloneContextOperation(operations[transactions[index].result])
	}
	if err := appendSemanticContextRanges(ctx, &plan, messages, transactions); err != nil {
		return contextSafeCutPlan{}, err
	}
	plan.normalize()
	return plan, nil
}

func (plan contextSafeCutPlan) safe(cut int) bool {
	if cut <= 0 || cut >= plan.messages {
		return false
	}
	index := sort.Search(len(plan.protected), func(index int) bool {
		return plan.protected[index].end >= cut
	})
	if index < len(plan.protected) && plan.protected[index].start < cut {
		return false
	}
	return true
}

func (plan *contextSafeCutPlan) normalize() {
	if len(plan.protected) < 2 {
		return
	}
	sort.Slice(plan.protected, func(first, second int) bool {
		if plan.protected[first].start == plan.protected[second].start {
			return plan.protected[first].end < plan.protected[second].end
		}
		return plan.protected[first].start < plan.protected[second].start
	})
	merged := plan.protected[:1]
	for _, candidate := range plan.protected[1:] {
		last := &merged[len(merged)-1]
		if candidate.start <= last.end {
			if candidate.end > last.end {
				last.end = candidate.end
			}
			continue
		}
		merged = append(merged, candidate)
	}
	plan.protected = merged
}

func contextToolTransactions(ctx context.Context, messages []Message) ([]contextToolTransaction, error) {
	type pendingTool struct {
		start int
	}
	pending := make(map[string]pendingTool)
	transactions := make([]contextToolTransaction, 0)
	for index, message := range messages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch message.Role {
		case RoleAssistant:
			if len(pending) != 0 {
				return nil, errors.New("assistant Message overlaps pending Tool results")
			}
			for _, call := range message.ToolCalls {
				if call.ID == "" {
					return nil, errors.New("assistant Tool Call ID is required for a safe cut")
				}
				if _, duplicate := pending[call.ID]; duplicate {
					return nil, fmt.Errorf("assistant Tool Call ID %q is duplicated", call.ID)
				}
				pending[call.ID] = pendingTool{start: index}
			}
		case RoleTool:
			tool, exists := pending[message.ToolCallID]
			if !exists {
				return nil, fmt.Errorf("Tool result %q has no pending Tool Call", message.ToolCallID)
			}
			transactions = append(transactions, contextToolTransaction{
				start:  tool.start,
				result: index,
			})
			delete(pending, message.ToolCallID)
		case RoleUser:
			if len(pending) != 0 {
				return nil, errors.New("user Message overlaps pending Tool results")
			}
		default:
			return nil, fmt.Errorf("Message %d has unsupported Role %q", index, message.Role)
		}
	}
	if len(pending) != 0 {
		return nil, errors.New("context ends with pending Tool results")
	}
	return transactions, nil
}

func contextOperationsByMessage(
	ctx context.Context,
	messages []Message,
	events []Event,
) (map[int]*ContextOperation, error) {
	operations := make(map[int]*ContextOperation)
	observed := make([]Message, 0, len(messages))
	for eventIndex, event := range events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if event.Type == EventToolCompleted && event.ToolResult != nil {
			if err := ValidateContextOperation(event.ToolResult.ContextOperation); err != nil {
				return nil, fmt.Errorf("safe cut Event %d: %w", eventIndex, err)
			}
		}
		switch event.Type {
		case EventUserMessageAdded, EventMessageCompleted, EventToolCompleted:
			if event.Message == nil {
				continue
			}
			messageIndex := len(observed)
			observed = append(observed, cloneMessage(*event.Message))
			if event.Type == EventToolCompleted && event.ToolResult != nil && event.ToolResult.ContextOperation != nil {
				operations[messageIndex] = cloneContextOperation(event.ToolResult.ContextOperation)
			}
		}
	}
	if len(observed) != len(messages) || checkpointSourceHash(observed) != checkpointSourceHash(messages) {
		return nil, errors.New("safe cut Events do not reconstruct the compiled raw Messages")
	}
	return operations, nil
}

func appendSemanticContextRanges(
	ctx context.Context,
	plan *contextSafeCutPlan,
	messages []Message,
	transactions []contextToolTransaction,
) error {
	transactionIndex := 0
	openMutation := -1
	for messageIndex, message := range messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		if message.Role == RoleUser && openMutation >= 0 {
			plan.protected = append(plan.protected, contextProtectedRange{
				start: openMutation,
				end:   messageIndex - 1,
			})
			openMutation = -1
		}
		for transactionIndex < len(transactions) && transactions[transactionIndex].result == messageIndex {
			transaction := transactions[transactionIndex]
			transactionIndex++
			if transaction.operation == nil {
				continue
			}
			switch transaction.operation.Kind {
			case ContextOperationMutation:
				if openMutation < 0 {
					openMutation = transaction.start
				}
			case ContextOperationVerification, ContextOperationCommit:
				if openMutation >= 0 {
					plan.protected = append(plan.protected, contextProtectedRange{
						start: openMutation,
						end:   transaction.result,
					})
					openMutation = -1
				}
			case ContextOperationSubagent:
				// The enclosing Tool transaction already protects start through result
			}
		}
	}
	if openMutation >= 0 {
		plan.protected = append(plan.protected, contextProtectedRange{
			start: openMutation,
			end:   len(messages) - 1,
		})
	}
	return nil
}
