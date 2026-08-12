package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	cli "github.com/mayahiro/nagicli-go"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/evidence"
)

const (
	contextSelectorArgumentID = "context-selector"
	contextBeforeValueID      = "context-before"
	contextAfterValueID       = "context-after"
)

type contextSelector struct {
	runID         string
	eventSequence uint64
}

var errInvalidContextSelector = errors.New(
	"Context selector must be RUN_ID or RUN_ID@EVENT_SEQUENCE with a positive sequence",
)

func contextCommand() *cli.Command {
	return cli.NewCommand("context").
		About("Inspect content-free Context Compiler reports and metrics").
		RequireSubcommand().
		Subcommand(inspectContextCommand()).
		Subcommand(diffContextCommand()).
		Subcommand(explainContextCommand())
}

func inspectContextCommand() *cli.Command {
	return cli.NewCommand("inspect").
		About("Inspect one Run Context timeline and aggregate metrics").
		Argument(
			cli.Positional(runIDArgumentID).
				Parser(cli.StringParser()).
				Required().
				Help("Run ID"),
		).
		Option(evidenceStoreOption()).
		Option(contextOutputOption()).
		Handle(func(commandContext *cli.Context, invocation *cli.Invocation) (cli.Outcome, error) {
			runID, diagnostic := requiredString(invocation, runIDArgumentID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			store, diagnostic := openEvidenceStore(commandContext, invocation)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			report, err := loadContextReport(commandContext.Cancellation(), store, runID)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("load Context report: %v", err))
			}
			output, diagnostic := requiredString(invocation, outputValueID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			if err := writeContextInspect(commandContext.Stdout(), output, report); err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeIOError, fmt.Sprintf("write Context report: %v", err))
			}
			return cli.Success(), nil
		})
}

func explainContextCommand() *cli.Command {
	return cli.NewCommand("explain").
		About("Explain one content-free Context compaction decision").
		Argument(
			cli.Positional(contextSelectorArgumentID).
				Parser(cli.StringParser()).
				Required().
				Help("Run selector as RUN_ID or RUN_ID@EVENT_SEQUENCE"),
		).
		Option(evidenceStoreOption()).
		Option(contextOutputOption()).
		Handle(func(commandContext *cli.Context, invocation *cli.Invocation) (cli.Outcome, error) {
			value, diagnostic := requiredString(invocation, contextSelectorArgumentID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			selector, err := parseContextSelector(value)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeValidation, err.Error()).
					WithCategory(cli.CategoryUsage)
			}
			store, diagnostic := openEvidenceStore(commandContext, invocation)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			snapshot, err := loadContextSnapshot(commandContext.Cancellation(), store, selector)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("load Context snapshot: %v", err))
			}
			output, diagnostic := requiredString(invocation, outputValueID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			if err := writeContextExplain(commandContext.Stdout(), output, snapshot); err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeIOError, fmt.Sprintf("write Context explanation: %v", err))
			}
			return cli.Success(), nil
		})
}

func diffContextCommand() *cli.Command {
	return cli.NewCommand("diff").
		About("Compare two content-free Context compaction decisions").
		Option(
			cli.ValueOption(contextBeforeValueID).
				Long("before").
				Parser(cli.StringParser()).
				Required().
				Help("Earlier selector as RUN_ID or RUN_ID@EVENT_SEQUENCE"),
		).
		Option(
			cli.ValueOption(contextAfterValueID).
				Long("after").
				Parser(cli.StringParser()).
				Required().
				Help("Later selector as RUN_ID or RUN_ID@EVENT_SEQUENCE"),
		).
		Option(evidenceStoreOption()).
		Option(contextOutputOption()).
		Handle(func(commandContext *cli.Context, invocation *cli.Invocation) (cli.Outcome, error) {
			beforeValue, diagnostic := requiredString(invocation, contextBeforeValueID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			afterValue, diagnostic := requiredString(invocation, contextAfterValueID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			beforeSelector, err := parseContextSelector(beforeValue)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeValidation, fmt.Sprintf("invalid --before: %v", err)).
					WithCategory(cli.CategoryUsage)
			}
			afterSelector, err := parseContextSelector(afterValue)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeValidation, fmt.Sprintf("invalid --after: %v", err)).
					WithCategory(cli.CategoryUsage)
			}
			store, diagnostic := openEvidenceStore(commandContext, invocation)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			before, err := loadContextSnapshot(commandContext.Cancellation(), store, beforeSelector)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("load --before Context snapshot: %v", err))
			}
			after, err := loadContextSnapshot(commandContext.Cancellation(), store, afterSelector)
			if err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeHandlerError, fmt.Sprintf("load --after Context snapshot: %v", err))
			}
			output, diagnostic := requiredString(invocation, outputValueID)
			if diagnostic != nil {
				return cli.Outcome{}, diagnostic
			}
			diff := agent.DiffContextSnapshots(before, after)
			if err := writeContextDiff(commandContext.Stdout(), output, diff); err != nil {
				return cli.Outcome{}, cli.NewDiagnostic(cli.CodeIOError, fmt.Sprintf("write Context diff: %v", err))
			}
			return cli.Success(), nil
		})
}

func contextOutputOption() *cli.OptionSpec {
	return cli.ValueOption(outputValueID).
		Long("output").
		Parser(cli.PossibleValuesParser("text", "json")).
		Default("text").
		Help("Output format")
}

func parseContextSelector(value string) (contextSelector, error) {
	if value == "" || value != strings.TrimSpace(value) {
		return contextSelector{}, errInvalidContextSelector
	}
	separator := strings.LastIndexByte(value, '@')
	if separator < 0 {
		return contextSelector{runID: value}, nil
	}
	if separator == 0 || separator == len(value)-1 {
		return contextSelector{}, errInvalidContextSelector
	}
	sequence, err := strconv.ParseUint(value[separator+1:], 10, 64)
	if err != nil || sequence == 0 {
		return contextSelector{}, errInvalidContextSelector
	}
	return contextSelector{runID: value[:separator], eventSequence: sequence}, nil
}

func loadContextReport(ctx context.Context, store *evidence.JSONStore, runID string) (agent.ContextReport, error) {
	bundle, err := store.Load(ctx, runID)
	if err != nil {
		return agent.ContextReport{}, err
	}
	return agent.BuildContextReport(ctx, bundle.Run.ID, bundle.Events)
}

func loadContextSnapshot(
	ctx context.Context,
	store *evidence.JSONStore,
	selector contextSelector,
) (agent.ContextSnapshot, error) {
	report, err := loadContextReport(ctx, store, selector.runID)
	if err != nil {
		return agent.ContextSnapshot{}, err
	}
	return report.Snapshot(selector.eventSequence)
}

func writeContextInspect(writer io.Writer, output string, report agent.ContextReport) error {
	if output == "json" {
		return writeContextJSON(writer, report)
	}
	metrics := report.Metrics
	if _, err := fmt.Fprintf(
		writer,
		"Run: %s\nContext events: %d\nCheckpoint generations: %d\nLatest generation: %s\n",
		report.RunID,
		metrics.CompactionCount,
		metrics.CheckpointGenerationCount,
		formatContextGeneration(metrics.LatestCheckpointGeneration),
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(
		writer,
		"Compression: original=%d compiled=%d ratio=%s reduction=%s\n",
		metrics.OriginalBytes,
		metrics.CompiledBytes,
		formatContextRatio(metrics.CompressionRatio),
		formatContextReduction(metrics.CompressionRatio),
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(
		writer,
		"Rebases: %d\nRollbacks: %d\nFallbacks: %d\nExternalized: objects=%d bytes=%d\n",
		metrics.FullRebaseCount,
		metrics.RollbackCount,
		metrics.CustomFallbackCount,
		metrics.ExternalizedObjects,
		metrics.ExternalizedBytes,
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(
		writer,
		"Validation: reports=%d failures=%d unreported=%d\nPreservation: overall=%s constraints=%s modified_artifacts=%s failing_checks=%s pending_tools=%s evidence_objects=%s evidence_bytes=%s\nPost-compaction rereads: unavailable\n",
		metrics.ValidationCount,
		metrics.ValidationFailureCount,
		metrics.UnreportedValidationCount,
		formatContextMetricRate(metrics.Preservation.Overall),
		formatContextMetricRate(metrics.Preservation.ActiveConstraints),
		formatContextMetricRate(metrics.Preservation.ModifiedArtifacts),
		formatContextMetricRate(metrics.Preservation.FailingChecks),
		formatContextMetricRate(metrics.Preservation.PendingTools),
		formatContextMetricRate(metrics.Preservation.EvidenceObjects),
		formatContextMetricRate(metrics.Preservation.EvidenceBytes),
	); err != nil {
		return err
	}
	if len(report.Snapshots) == 0 {
		_, err := fmt.Fprintln(writer, "Timeline: none")
		return err
	}
	if _, err := fmt.Fprintln(writer, "Timeline:"); err != nil {
		return err
	}
	for _, snapshot := range report.Snapshots {
		validation := "unreported"
		rollback := ""
		if snapshot.Validation != nil {
			validation = "passed"
			if !snapshot.Validation.Passed {
				validation = "failed"
			}
			if snapshot.Validation.Rollback != "" {
				rollback = " rollback=" + string(snapshot.Validation.Rollback)
			}
		}
		if _, err := fmt.Fprintf(
			writer,
			"  sequence=%d effective_generation=%s candidate_generation=%s reason=%s applied=%t ratio=%s validation=%s%s\n",
			snapshot.EventSequence,
			formatContextGeneration(snapshot.CheckpointGeneration),
			formatContextGeneration(snapshot.CandidateGeneration),
			snapshot.Reason,
			snapshot.Applied,
			formatContextRatio(snapshot.CompressionRatio),
			validation,
			rollback,
		); err != nil {
			return err
		}
	}
	return nil
}

func writeContextExplain(writer io.Writer, output string, snapshot agent.ContextSnapshot) error {
	if output == "json" {
		return writeContextJSON(writer, snapshot)
	}
	if _, err := fmt.Fprintf(
		writer,
		"Run: %s\nEvent sequence: %d\nReason: %s\nApplied: %t\nPublished checkpoint: %t\nCheckpoint: effective_generation=%s candidate_generation=%s source_messages=%d recent_messages=%d\n",
		snapshot.RunID,
		snapshot.EventSequence,
		snapshot.Reason,
		snapshot.Applied,
		snapshot.PublishedCheckpoint,
		formatContextGeneration(snapshot.CheckpointGeneration),
		formatContextGeneration(snapshot.CandidateGeneration),
		snapshot.SourceMessageCount,
		snapshot.RecentMessageCount,
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(
		writer,
		"Reduction: original=%d compiled=%d removed=%d ratio=%s reduction=%s\nExternalized: objects=%d bytes=%d\n",
		snapshot.OriginalBytes,
		snapshot.CompiledBytes,
		snapshot.OriginalBytes-snapshot.CompiledBytes,
		formatContextRatio(snapshot.CompressionRatio),
		formatContextReduction(snapshot.CompressionRatio),
		snapshot.ExternalizedObjects,
		snapshot.ExternalizedBytes,
	); err != nil {
		return err
	}
	if snapshot.Validation == nil {
		if _, err := fmt.Fprintln(writer, "Validation: unreported for this Event"); err != nil {
			return err
		}
	} else {
		validation := snapshot.Validation
		if _, err := fmt.Fprintf(writer, "Validation: passed=%t\n", validation.Passed); err != nil {
			return err
		}
		if err := writeContextValidationCounts(writer, *validation); err != nil {
			return err
		}
		failures := "none"
		if len(validation.Failures) > 0 {
			values := make([]string, len(validation.Failures))
			for index, failure := range validation.Failures {
				values[index] = string(failure)
			}
			failures = strings.Join(values, ",")
		}
		rollback := "none"
		if validation.Rollback != "" {
			rollback = string(validation.Rollback)
		}
		if _, err := fmt.Fprintf(writer, "Failures: %s\nRollback: %s\n", failures, rollback); err != nil {
			return err
		}
	}
	rebase := "none"
	if snapshot.Rebased {
		rebase = string(snapshot.RebaseReason)
	}
	fallback := snapshot.Fallback
	if fallback == "" {
		fallback = "none"
	}
	_, err := fmt.Fprintf(
		writer,
		"Rebase: %s\nFallback: %s\nPost-compaction rereads: unavailable\n",
		rebase,
		fallback,
	)
	return err
}

func writeContextValidationCounts(writer io.Writer, report agent.ContextValidationReport) error {
	values := []struct {
		name  string
		count agent.ContextPreservationCount
	}{
		{"active_constraints", report.ActiveConstraints},
		{"modified_artifacts", report.ModifiedArtifacts},
		{"failing_checks", report.FailingChecks},
		{"pending_tools", report.PendingTools},
		{"evidence_objects", report.Evidence},
	}
	for _, value := range values {
		if _, err := fmt.Fprintf(
			writer,
			"Preserved %s: %d/%d\n",
			value.name,
			value.count.Preserved,
			value.count.Required,
		); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(
		writer,
		"Preserved evidence_bytes: %d/%d\n",
		report.Evidence.PreservedBytes,
		report.Evidence.RequiredBytes,
	)
	return err
}

func writeContextDiff(writer io.Writer, output string, diff agent.ContextSnapshotDiff) error {
	if output == "json" {
		return writeContextJSON(writer, diff)
	}
	if _, err := fmt.Fprintf(
		writer,
		"Before: %s@%d effective_generation=%s candidate_generation=%s reason=%s\nAfter: %s@%d effective_generation=%s candidate_generation=%s reason=%s\n",
		diff.Before.RunID,
		diff.Before.EventSequence,
		formatContextGeneration(diff.Before.CheckpointGeneration),
		formatContextGeneration(diff.Before.CandidateGeneration),
		diff.Before.Reason,
		diff.After.RunID,
		diff.After.EventSequence,
		formatContextGeneration(diff.After.CheckpointGeneration),
		formatContextGeneration(diff.After.CandidateGeneration),
		diff.After.Reason,
	); err != nil {
		return err
	}
	values := []struct {
		name  string
		delta agent.ContextValueDelta
	}{
		{"Original bytes", diff.Delta.OriginalBytes},
		{"Compiled bytes", diff.Delta.CompiledBytes},
		{"Source messages", diff.Delta.SourceMessageCount},
		{"Recent messages", diff.Delta.RecentMessageCount},
		{"Externalized objects", diff.Delta.ExternalizedObjects},
		{"Externalized bytes", diff.Delta.ExternalizedBytes},
	}
	for _, value := range values {
		if _, err := fmt.Fprintf(
			writer,
			"%s: before=%d after=%d delta=%+d\n",
			value.name,
			value.delta.Before,
			value.delta.After,
			value.delta.Delta,
		); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(
		writer,
		"Compression ratio: before=%s after=%s delta=%s\n",
		formatContextRatio(diff.Delta.CompressionRatio.Before),
		formatContextRatio(diff.Delta.CompressionRatio.After),
		formatContextRatioDelta(diff.Delta.CompressionRatio.Delta),
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(
		writer,
		"Rebase: before=%s after=%s\nRollback: before=%s after=%s\nFallback: before=%s after=%s\n",
		contextSnapshotRebase(diff.Before),
		contextSnapshotRebase(diff.After),
		contextSnapshotRollback(diff.Before),
		contextSnapshotRollback(diff.After),
		contextSnapshotFallback(diff.Before),
		contextSnapshotFallback(diff.After),
	); err != nil {
		return err
	}
	return writeContextPreservationDiff(writer, diff.Delta.Preservation)
}

func contextSnapshotRebase(snapshot agent.ContextSnapshot) string {
	if !snapshot.Rebased {
		return "none"
	}
	return string(snapshot.RebaseReason)
}

func contextSnapshotRollback(snapshot agent.ContextSnapshot) string {
	if snapshot.Validation == nil || snapshot.Validation.Rollback == "" {
		return "none"
	}
	return string(snapshot.Validation.Rollback)
}

func contextSnapshotFallback(snapshot agent.ContextSnapshot) string {
	if snapshot.Fallback == "" {
		return "none"
	}
	return snapshot.Fallback
}

func writeContextPreservationDiff(writer io.Writer, diff agent.ContextPreservationDiff) error {
	values := []struct {
		name  string
		delta agent.ContextPreservationDelta
	}{
		{"active_constraints", diff.ActiveConstraints},
		{"modified_artifacts", diff.ModifiedArtifacts},
		{"failing_checks", diff.FailingChecks},
		{"pending_tools", diff.PendingTools},
		{"evidence_objects", diff.EvidenceObjects},
		{"evidence_bytes", diff.EvidenceBytes},
	}
	for _, value := range values {
		if _, err := fmt.Fprintf(
			writer,
			"Preservation %s: required_delta=%+d preserved_delta=%+d\n",
			value.name,
			value.delta.Required,
			value.delta.Preserved,
		); err != nil {
			return err
		}
	}
	return nil
}

func writeContextJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func formatContextMetricRate(rate agent.ContextMetricRate) string {
	if rate.Rate == nil {
		return fmt.Sprintf("%d/%d (unavailable)", rate.Preserved, rate.Required)
	}
	return fmt.Sprintf("%d/%d (%.2f%%)", rate.Preserved, rate.Required, 100**rate.Rate)
}

func formatContextGeneration(generation *uint64) string {
	if generation == nil {
		return "unavailable"
	}
	if *generation == 0 {
		return "none"
	}
	return strconv.FormatUint(*generation, 10)
}

func formatContextRatio(ratio *float64) string {
	if ratio == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%.2f%%", 100**ratio)
}

func formatContextReduction(ratio *float64) string {
	if ratio == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%.2f%%", 100*(1-*ratio))
}

func formatContextRatioDelta(delta *float64) string {
	if delta == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%+.2f percentage_points", 100**delta)
}
