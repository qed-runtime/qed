package cliapproval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/internal/cliapproval"
)

func TestApproverShowsBoundPreviewWithoutPrintingRawArguments(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	approver, err := cliapproval.New(strings.NewReader("maybe\nyes\n"), &output)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := approver.Approve(context.Background(), capability.Request{
		Tool:                "run_command",
		Capabilities:        []capability.Name{capability.ProcessExecute},
		Arguments:           json.RawMessage(`{"secret":"must-not-print"}`),
		ArgumentsDigest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ExtensionID:         "qed.process",
		ExtensionGeneration: 4,
		Preview: &capability.ApprovalPreview{
			Summary: "Run workspace verification",
			Details: []capability.ApprovalPreviewDetail{
				{Label: "argv", Value: `["go","test","./..."]`},
				{Label: "cwd", Value: "."},
			},
		},
	})
	if err != nil || !approved {
		t.Fatalf("Approve() = %t, %v", approved, err)
	}
	if !strings.Contains(output.String(), `Tool "run_command"`) ||
		!strings.Contains(output.String(), "process.execute") ||
		!strings.Contains(output.String(), "Extension: qed.process generation 4") ||
		!strings.Contains(output.String(), "Action: Run workspace verification") ||
		!strings.Contains(output.String(), `argv: ["go","test","./..."]`) ||
		!strings.Contains(output.String(), "sha256:aaaaaaaa") ||
		!strings.Contains(output.String(), "Please answer yes or no") {
		t.Fatalf("prompt = %q", output.String())
	}
	if strings.Contains(output.String(), "must-not-print") {
		t.Fatalf("prompt exposed Tool arguments: %q", output.String())
	}
}

func TestApproverReportsUnavailablePreview(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	approver, err := cliapproval.New(strings.NewReader("no\n"), &output)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := approver.Approve(context.Background(), capability.Request{Tool: "external_tool"})
	if err != nil || approved {
		t.Fatalf("Approve() = %t, %v", approved, err)
	}
	if !strings.Contains(output.String(), "Details: unavailable") {
		t.Fatalf("prompt = %q", output.String())
	}
}

func TestApproverRejectsUnsafeApprovalMetadataBeforeWriting(t *testing.T) {
	t.Parallel()

	requests := []capability.Request{
		{Tool: "unsafe\x1b[31m"},
		{Tool: "unsafe\u202ereversed"},
		{Tool: "safe", Capabilities: []capability.Name{"invalid capability"}},
		{Tool: "safe", ArgumentsDigest: "sha256:not-a-digest"},
		{Tool: "safe", ExtensionID: "unsafe\nvalue"},
		{Tool: "safe", ExtensionGeneration: 2},
	}
	for _, request := range requests {
		var output bytes.Buffer
		approver, err := cliapproval.New(strings.NewReader("yes\n"), &output)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := approver.Approve(context.Background(), request); err == nil {
			t.Fatalf("Approve(%#v) succeeded", request)
		}
		if output.Len() != 0 {
			t.Fatalf("Approve(%#v) wrote %q", request, output.String())
		}
	}
}

func TestApproverRejectsNoAndEOF(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"no\n", ""} {
		var output bytes.Buffer
		approver, err := cliapproval.New(strings.NewReader(input), &output)
		if err != nil {
			t.Fatal(err)
		}
		approved, err := approver.Approve(context.Background(), capability.Request{Tool: "read_file"})
		if err != nil || approved {
			t.Fatalf("Approve(%q) = %t, %v", input, approved, err)
		}
	}
}

func TestApproverHonorsCanceledContextBeforeReading(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	approver, err := cliapproval.New(strings.NewReader("yes\n"), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = approver.Approve(ctx, capability.Request{Tool: "read_file"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Approve() error = %v, want context.Canceled", err)
	}
}
