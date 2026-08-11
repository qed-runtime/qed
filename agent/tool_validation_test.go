package agent_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
)

func TestJSONSchemaSubsetValidatorValidatesSupportedKeywords(t *testing.T) {
	t.Parallel()

	compiled, err := agent.CompileToolInputSchema(nil, json.RawMessage(`{
		"type":"object",
		"properties":{
			"name":{"type":"string","enum":["alpha","beta"]},
			"count":{"type":"integer","minimum":1,"maximum":3},
			"tags":{"type":"array","minItems":1,"items":{"type":"string"}},
			"entries":{"type":"array","items":{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"],"additionalProperties":false}}
		},
		"required":["name","count","tags","entries"],
		"additionalProperties":false
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.ValidateToolInput(compiled, json.RawMessage(`{"name":"alpha","count":1.0,"tags":["one"],"entries":[{"enabled":true}]}`)); err != nil {
		t.Fatalf("ValidateToolInput(valid) error = %v", err)
	}

	tests := []struct {
		name      string
		arguments string
		wantPath  string
	}{
		{name: "invalid JSON", arguments: `{`, wantPath: `$`},
		{name: "duplicate key", arguments: `{"name":"alpha","name":"beta","count":1,"tags":["one"],"entries":[]}`, wantPath: `name`},
		{name: "wrong root type", arguments: `[]`, wantPath: `$`},
		{name: "missing required", arguments: `{"count":1,"tags":["one"],"entries":[]}`, wantPath: `$/name`},
		{name: "additional property", arguments: `{"name":"alpha","count":1,"tags":["one"],"entries":[],"extra":true}`, wantPath: `$/extra`},
		{name: "enum", arguments: `{"name":"gamma","count":1,"tags":["one"],"entries":[]}`, wantPath: `$/name`},
		{name: "fractional integer", arguments: `{"name":"alpha","count":1.5,"tags":["one"],"entries":[]}`, wantPath: `$/count`},
		{name: "minimum", arguments: `{"name":"alpha","count":0,"tags":["one"],"entries":[]}`, wantPath: `$/count`},
		{name: "maximum", arguments: `{"name":"alpha","count":4,"tags":["one"],"entries":[]}`, wantPath: `$/count`},
		{name: "min items", arguments: `{"name":"alpha","count":1,"tags":[],"entries":[]}`, wantPath: `$/tags`},
		{name: "nested item", arguments: `{"name":"alpha","count":1,"tags":["one"],"entries":[{}]}`, wantPath: `$/entries/0/enabled`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := agent.ValidateToolInput(compiled, json.RawMessage(test.arguments))
			if !errors.Is(err, agent.ErrToolInputValidation) || !strings.Contains(err.Error(), test.wantPath) {
				t.Fatalf("ValidateToolInput() error = %v, want validation error containing %q", err, test.wantPath)
			}
		})
	}
}

func TestJSONSchemaSubsetValidatorUsesJSONNumericEquality(t *testing.T) {
	t.Parallel()

	compiled, err := agent.CompileToolInputSchema(nil, json.RawMessage(`{"enum":[1e2]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.ValidateToolInput(compiled, json.RawMessage(`100.0`)); err != nil {
		t.Fatalf("ValidateToolInput(100.0) error = %v", err)
	}
}

func TestJSONSchemaSubsetValidatorRejectsInvalidOrUnsupportedSchemas(t *testing.T) {
	t.Parallel()

	tests := []string{
		`{`,
		`true`,
		`{"type":"object","type":"array"}`,
		`{"oneOf":[{"type":"string"}]}`,
		`{"type":["string","null"]}`,
		`{"required":["value","value"]}`,
		`{"minItems":-1}`,
		`{"enum":[1,1.0]}`,
		`{"additionalProperties":{}}`,
	}
	for _, schema := range tests {
		_, err := agent.CompileToolInputSchema(nil, json.RawMessage(schema))
		if !errors.Is(err, agent.ErrToolInputSchema) {
			t.Errorf("CompileToolInputSchema(%s) error = %v", schema, err)
		}
	}
}

func TestJSONSchemaSubsetValidatorRejectsOversizedAndDeepInputs(t *testing.T) {
	t.Parallel()

	compiled, err := agent.CompileToolInputSchema(nil, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	oversized := json.RawMessage(`"` + strings.Repeat("x", 8<<20) + `"`)
	if err := agent.ValidateToolInput(compiled, oversized); !errors.Is(err, agent.ErrToolInputValidation) {
		t.Fatalf("oversized input error = %v", err)
	}
	deep := json.RawMessage(strings.Repeat("[", 65) + strings.Repeat("]", 65))
	if err := agent.ValidateToolInput(compiled, deep); !errors.Is(err, agent.ErrToolInputValidation) {
		t.Fatalf("deep input error = %v", err)
	}
}

func TestCompileToolInputSchemaSupportsCustomValidator(t *testing.T) {
	t.Parallel()

	_, err := agent.CompileToolInputSchema(rejectingToolInputValidator{}, json.RawMessage(`{`))
	if !errors.Is(err, agent.ErrToolInputSchema) {
		t.Fatalf("CompileToolInputSchema(malformed) error = %v", err)
	}
	compiled, err := agent.CompileToolInputSchema(rejectingToolInputValidator{}, json.RawMessage(`{"custom":true}`))
	if err != nil {
		t.Fatal(err)
	}
	err = agent.ValidateToolInput(compiled, json.RawMessage(`{`))
	if !errors.Is(err, agent.ErrToolInputValidation) || strings.Contains(err.Error(), "custom rejection") {
		t.Fatalf("ValidateToolInput(malformed) error = %v", err)
	}
	err = agent.ValidateToolInput(compiled, json.RawMessage(`{"value":true}`))
	if !errors.Is(err, agent.ErrToolInputValidation) || !strings.Contains(err.Error(), "custom rejection") {
		t.Fatalf("ValidateToolInput() error = %v", err)
	}
}

type rejectingToolInputValidator struct{}

func (rejectingToolInputValidator) Compile(json.RawMessage) (agent.CompiledToolInputValidator, error) {
	return rejectingCompiledToolInputValidator{}, nil
}

type rejectingCompiledToolInputValidator struct{}

func (rejectingCompiledToolInputValidator) Validate(json.RawMessage) error {
	return errors.New("custom rejection")
}
