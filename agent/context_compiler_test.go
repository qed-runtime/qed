package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
)

func TestDefaultContextCompilerCanonicalizesToolABIAndManifest(t *testing.T) {
	t.Parallel()

	firstRequest := agent.ModelRequest{
		Instructions: "keep this instruction stable",
		Metadata:     map[string]string{"z": "last", "a": "first"},
		Messages:     []agent.Message{{Role: agent.RoleUser, Text: "private request content"}},
		Tools: []agent.ToolDefinition{
			{
				Name:        "zeta",
				Description: "Zeta tool",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"b":{"type":"number"},"a":{"type":"string"}}}`),
			},
			{
				Name:        "alpha",
				Description: "Alpha tool",
				InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
	}
	secondRequest := agent.ModelRequest{
		Instructions: firstRequest.Instructions,
		Metadata:     map[string]string{"a": "first", "z": "last"},
		Messages:     []agent.Message{{Role: agent.RoleUser, Text: "private request content"}},
		Tools: []agent.ToolDefinition{
			{
				Name:        "alpha",
				Description: "Alpha tool",
				InputSchema: json.RawMessage(`{"properties":{},"type":"object"}`),
			},
			{
				Name:        "zeta",
				Description: "Zeta tool",
				InputSchema: json.RawMessage(`{"properties":{"a":{"type":"string"},"b":{"type":"number"}},"type":"object"}`),
			},
		},
	}

	compiler := agent.DefaultContextCompiler{}
	first, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{ModelRequest: firstRequest})
	if err != nil {
		t.Fatal(err)
	}
	second, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{ModelRequest: secondRequest})
	if err != nil {
		t.Fatal(err)
	}
	if firstRequest.Tools[0].Name != "zeta" {
		t.Fatal("Compile mutated its input request")
	}
	if len(first.ModelRequest.Tools) != 2 || first.ModelRequest.Tools[0].Name != "alpha" || first.ModelRequest.Tools[1].Name != "zeta" {
		t.Fatalf("canonical Tool order = %#v", first.ModelRequest.Tools)
	}
	wantSchema := `{"properties":{"a":{"type":"string"},"b":{"type":"number"}},"type":"object"}`
	if got := string(first.ModelRequest.Tools[1].InputSchema); got != wantSchema {
		t.Fatalf("canonical Tool schema = %s, want %s", got, wantSchema)
	}

	options := agent.PrefixManifestOptions{Provider: "test/responses", Model: "test-model"}
	firstManifest, err := agent.BuildPrefixManifest(options, first.Segments)
	if err != nil {
		t.Fatal(err)
	}
	secondManifest, err := agent.BuildPrefixManifest(options, second.Segments)
	if err != nil {
		t.Fatal(err)
	}
	if firstManifest.Epoch != secondManifest.Epoch {
		t.Fatalf("equivalent Context epochs = %q/%q", firstManifest.Epoch, secondManifest.Epoch)
	}
	if len(firstManifest.Segments) != 4 {
		t.Fatalf("Segment count = %d, want 4", len(firstManifest.Segments))
	}
	if firstManifest.Segments[0].Kind != agent.SegmentKindInstructions ||
		firstManifest.Segments[1].Kind != agent.SegmentKindToolABI ||
		firstManifest.Segments[2].Kind != agent.SegmentKindMessage ||
		firstManifest.Segments[3].Kind != agent.SegmentKindMetadata {
		t.Fatalf("Segment order = %#v", firstManifest.Segments)
	}
	encoded, err := json.Marshal(firstManifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private request content") || strings.Contains(string(encoded), firstRequest.Instructions) {
		t.Fatalf("Prefix Manifest exposed Context content: %s", encoded)
	}
}

func TestDefaultContextCompilerLocalizesMessageChanges(t *testing.T) {
	t.Parallel()

	compiler := agent.DefaultContextCompiler{}
	compile := func(text string) agent.PrefixManifest {
		t.Helper()
		compiled, err := compiler.Compile(context.Background(), agent.ContextCompileRequest{
			ModelRequest: agent.ModelRequest{
				Instructions: "stable",
				Messages:     []agent.Message{{Role: agent.RoleUser, Text: text}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := agent.BuildPrefixManifest(
			agent.PrefixManifestOptions{Provider: "test", Model: "model"},
			compiled.Segments,
		)
		if err != nil {
			t.Fatal(err)
		}
		return manifest
	}

	before := compile("before")
	after := compile("after")
	if before.Epoch == after.Epoch {
		t.Fatal("message change did not change Prefix epoch")
	}
	if before.Segments[0] != after.Segments[0] || before.Segments[1] != after.Segments[1] {
		t.Fatalf("stable Segment changed: %#v / %#v", before.Segments, after.Segments)
	}
	if before.Segments[2].ContentHash == after.Segments[2].ContentHash {
		t.Fatal("message Segment hash did not change")
	}
}

func TestDefaultContextCompilerRejectsAmbiguousToolSchema(t *testing.T) {
	t.Parallel()

	_, err := (agent.DefaultContextCompiler{}).Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{
			Messages: []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
			Tools: []agent.ToolDefinition{{
				Name:        "ambiguous",
				InputSchema: json.RawMessage(`{"type":"object","type":"array"}`),
			}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate JSON key") {
		t.Fatalf("Compile error = %v", err)
	}
}

func TestPrefixEpochIgnoresObservationalTokenEstimate(t *testing.T) {
	t.Parallel()

	compiled, err := (agent.DefaultContextCompiler{}).Compile(context.Background(), agent.ContextCompileRequest{
		ModelRequest: agent.ModelRequest{Messages: []agent.Message{{Role: agent.RoleUser, Text: "hello"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	options := agent.PrefixManifestOptions{Provider: "test", Model: "model"}
	before, err := agent.BuildPrefixManifest(options, compiled.Segments)
	if err != nil {
		t.Fatal(err)
	}
	compiled.Segments[0].TokenEstimate = 42
	after, err := agent.BuildPrefixManifest(options, compiled.Segments)
	if err != nil {
		t.Fatal(err)
	}
	if before.Epoch != after.Epoch {
		t.Fatalf("observational token estimate changed Prefix epoch: %q/%q", before.Epoch, after.Epoch)
	}
}
