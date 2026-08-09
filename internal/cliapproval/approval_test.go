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

func TestApproverAcceptsExplicitYesWithoutPrintingArguments(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	approver, err := cliapproval.New(strings.NewReader("maybe\nyes\n"), &output)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := approver.Approve(context.Background(), capability.Request{
		Tool:         "run_command",
		Capabilities: []capability.Name{capability.ProcessExecute},
		Arguments:    json.RawMessage(`{"secret":"must-not-print"}`),
	})
	if err != nil || !approved {
		t.Fatalf("Approve() = %t, %v", approved, err)
	}
	if !strings.Contains(output.String(), `Tool "run_command"`) ||
		!strings.Contains(output.String(), "process.execute") ||
		!strings.Contains(output.String(), "Please answer yes or no") {
		t.Fatalf("prompt = %q", output.String())
	}
	if strings.Contains(output.String(), "must-not-print") {
		t.Fatalf("prompt exposed Tool arguments: %q", output.String())
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
