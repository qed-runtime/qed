package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

const (
	maxToolInputSchemaBytes    = 1 << 20
	maxToolInputArgumentsBytes = 8 << 20
	maxToolInputNestingDepth   = 64
	maxToolInputSchemaNodes    = 4096
	maxToolInputRequiredNames  = 4096
	maxToolInputEnumEntries    = 256
)

var (
	// ErrToolInputSchema indicates that a Tool input schema cannot be compiled
	ErrToolInputSchema = errors.New("invalid Tool input schema")
	// ErrToolInputValidation indicates that Tool arguments do not satisfy their schema
	ErrToolInputValidation = errors.New("Tool input validation failed")
)

// ToolInputValidator compiles one Tool input schema into an immutable validator
//
// CompileToolInputSchema supplies valid, duplicate-free JSON no larger than
// 1 MiB and no deeper than 64 levels. Implementations must be safe for
// concurrent Compile calls
type ToolInputValidator interface {
	Compile(schema json.RawMessage) (CompiledToolInputValidator, error)
}

// CompiledToolInputValidator validates Tool arguments against one compiled schema
//
// ValidateToolInput supplies valid, duplicate-free JSON no larger than 8 MiB
// and no deeper than 64 levels. Implementations must be safe for concurrent
// Validate calls
type CompiledToolInputValidator interface {
	Validate(arguments json.RawMessage) error
}

// JSONSchemaSubsetValidator validates the dependency-free QED JSON Schema subset
//
// The supported validation keywords are type, properties, required,
// additionalProperties, items, minItems, minimum, maximum, and enum
// Description and title are accepted as non-validating annotations
type JSONSchemaSubsetValidator struct{}

// Compile validates and compiles one Tool input schema
func (JSONSchemaSubsetValidator) Compile(schema json.RawMessage) (CompiledToolInputValidator, error) {
	if len(schema) == 0 {
		schema = defaultToolInputSchema
	}
	if len(schema) > maxToolInputSchemaBytes {
		return nil, toolSchemaError("$", "schema exceeds the 1 MiB limit")
	}
	decoded, err := decodeBoundedJSON(schema, maxToolInputNestingDepth)
	if err != nil {
		return nil, toolSchemaError("$", err.Error())
	}
	root, ok := decoded.(map[string]any)
	if !ok {
		return nil, toolSchemaError("$", "schema must be an object")
	}
	nodes := 0
	compiled, err := compileJSONSchemaSubset(root, "$", 0, &nodes)
	if err != nil {
		return nil, err
	}
	return &jsonSchemaSubsetPlan{root: compiled}, nil
}

// CompileToolInputSchema compiles a schema with validator
//
// A nil validator selects JSONSchemaSubsetValidator. An empty schema is
// normalized to QED's default object schema before Compile is called
func CompileToolInputSchema(
	validator ToolInputValidator,
	schema json.RawMessage,
) (CompiledToolInputValidator, error) {
	if len(schema) == 0 {
		schema = defaultToolInputSchema
	}
	if len(schema) > maxToolInputSchemaBytes {
		return nil, toolSchemaError("$", "schema exceeds the 1 MiB limit")
	}
	if err := enforceJSONDepth(schema, maxToolInputNestingDepth); err != nil {
		return nil, toolSchemaError("$", err.Error())
	}
	if err := rejectDuplicateJSONKeys(schema); err != nil {
		return nil, toolSchemaError("$", err.Error())
	}
	if validator == nil {
		validator = JSONSchemaSubsetValidator{}
	} else if nilInterface(validator) {
		return nil, toolSchemaError("$", "validator must not be a typed nil")
	}
	compiled, err := validator.Compile(append(json.RawMessage(nil), schema...))
	if err != nil {
		if errors.Is(err, ErrToolInputSchema) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrToolInputSchema, err)
	}
	if compiled == nil || nilInterface(compiled) {
		return nil, toolSchemaError("$", "validator returned a nil compiled validator")
	}
	return compiled, nil
}

// ValidateToolInput validates arguments with one compiled validator
//
// Empty arguments are normalized to an empty object before Validate is called
func ValidateToolInput(compiled CompiledToolInputValidator, arguments json.RawMessage) error {
	if compiled == nil || nilInterface(compiled) {
		return fmt.Errorf("%w: compiled validator is nil", ErrToolInputValidation)
	}
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if _, defaultPlan := compiled.(*jsonSchemaSubsetPlan); !defaultPlan {
		if len(arguments) > maxToolInputArgumentsBytes {
			return toolValidationError("$", "arguments exceed the 8 MiB limit")
		}
		if err := enforceJSONDepth(arguments, maxToolInputNestingDepth); err != nil {
			return toolValidationError("$", err.Error())
		}
		if err := rejectDuplicateJSONKeys(arguments); err != nil {
			return toolValidationError("$", err.Error())
		}
	}
	if err := compiled.Validate(append(json.RawMessage(nil), arguments...)); err != nil {
		if errors.Is(err, ErrToolInputValidation) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrToolInputValidation, err)
	}
	return nil
}

type jsonSchemaSubsetPlan struct {
	root *compiledJSONSchema
}

func (plan *jsonSchemaSubsetPlan) Validate(arguments json.RawMessage) error {
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if len(arguments) > maxToolInputArgumentsBytes {
		return toolValidationError("$", "arguments exceed the 8 MiB limit")
	}
	decoded, err := decodeBoundedJSON(arguments, maxToolInputNestingDepth)
	if err != nil {
		return toolValidationError("$", err.Error())
	}
	return plan.root.validate(decoded, "$", 0)
}

type compiledJSONSchema struct {
	typeName             string
	properties           map[string]*compiledJSONSchema
	propertyNames        []string
	required             []string
	additionalProperties *bool
	items                *compiledJSONSchema
	minItems             *int
	minimum              *big.Rat
	maximum              *big.Rat
	enum                 []any
	hasEnum              bool
}

func compileJSONSchemaSubset(
	raw map[string]any,
	path string,
	depth int,
	nodes *int,
) (*compiledJSONSchema, error) {
	if depth > maxToolInputNestingDepth {
		return nil, toolSchemaError(path, "schema exceeds the nesting depth limit")
	}
	(*nodes)++
	if *nodes > maxToolInputSchemaNodes {
		return nil, toolSchemaError(path, "schema exceeds the 4096 node limit")
	}
	for keyword := range raw {
		switch keyword {
		case "type", "properties", "required", "additionalProperties", "items", "minItems", "minimum", "maximum", "enum", "description", "title":
		default:
			return nil, toolSchemaError(jsonPath(path, keyword), fmt.Sprintf("keyword %q is not supported", keyword))
		}
	}

	compiled := &compiledJSONSchema{}
	if value, exists := raw["type"]; exists {
		typeName, ok := value.(string)
		if !ok || !supportedJSONType(typeName) {
			return nil, toolSchemaError(jsonPath(path, "type"), "must be one supported type string")
		}
		compiled.typeName = typeName
	}
	if value, exists := raw["description"]; exists {
		if _, ok := value.(string); !ok {
			return nil, toolSchemaError(jsonPath(path, "description"), "must be a string")
		}
	}
	if value, exists := raw["title"]; exists {
		if _, ok := value.(string); !ok {
			return nil, toolSchemaError(jsonPath(path, "title"), "must be a string")
		}
	}
	if value, exists := raw["properties"]; exists {
		properties, ok := value.(map[string]any)
		if !ok {
			return nil, toolSchemaError(jsonPath(path, "properties"), "must be an object")
		}
		compiled.properties = make(map[string]*compiledJSONSchema, len(properties))
		compiled.propertyNames = sortedMapKeys(properties)
		for _, name := range compiled.propertyNames {
			schema, ok := properties[name].(map[string]any)
			if !ok {
				return nil, toolSchemaError(jsonPath(jsonPath(path, "properties"), name), "must be a schema object")
			}
			child, err := compileJSONSchemaSubset(schema, jsonPath(path, name), depth+1, nodes)
			if err != nil {
				return nil, err
			}
			compiled.properties[name] = child
		}
	}
	if value, exists := raw["required"]; exists {
		required, ok := value.([]any)
		if !ok {
			return nil, toolSchemaError(jsonPath(path, "required"), "must be an array of unique strings")
		}
		if len(required) > maxToolInputRequiredNames {
			return nil, toolSchemaError(jsonPath(path, "required"), "exceeds the 4096 entry limit")
		}
		seen := make(map[string]struct{}, len(required))
		for index, item := range required {
			name, ok := item.(string)
			if !ok {
				return nil, toolSchemaError(jsonPath(jsonPath(path, "required"), strconv.Itoa(index)), "must be a string")
			}
			if _, duplicate := seen[name]; duplicate {
				return nil, toolSchemaError(jsonPath(path, "required"), fmt.Sprintf("contains duplicate property %q", name))
			}
			seen[name] = struct{}{}
			compiled.required = append(compiled.required, name)
		}
		sort.Strings(compiled.required)
	}
	if value, exists := raw["additionalProperties"]; exists {
		allowed, ok := value.(bool)
		if !ok {
			return nil, toolSchemaError(jsonPath(path, "additionalProperties"), "must be a boolean")
		}
		compiled.additionalProperties = &allowed
	}
	if value, exists := raw["items"]; exists {
		itemSchema, ok := value.(map[string]any)
		if !ok {
			return nil, toolSchemaError(jsonPath(path, "items"), "must be a schema object")
		}
		child, err := compileJSONSchemaSubset(itemSchema, jsonPath(path, "items"), depth+1, nodes)
		if err != nil {
			return nil, err
		}
		compiled.items = child
	}
	if value, exists := raw["minItems"]; exists {
		minimum, ok := nonnegativeJSONInteger(value)
		if !ok || !minimum.IsInt64() || minimum.Int64() > int64(^uint(0)>>1) {
			return nil, toolSchemaError(jsonPath(path, "minItems"), "must be a non-negative integer")
		}
		count := int(minimum.Int64())
		compiled.minItems = &count
	}
	if value, exists := raw["minimum"]; exists {
		minimum, ok := jsonNumberRat(value)
		if !ok {
			return nil, toolSchemaError(jsonPath(path, "minimum"), "must be a number")
		}
		compiled.minimum = minimum
	}
	if value, exists := raw["maximum"]; exists {
		maximum, ok := jsonNumberRat(value)
		if !ok {
			return nil, toolSchemaError(jsonPath(path, "maximum"), "must be a number")
		}
		compiled.maximum = maximum
	}
	if value, exists := raw["enum"]; exists {
		entries, ok := value.([]any)
		if !ok || len(entries) == 0 {
			return nil, toolSchemaError(jsonPath(path, "enum"), "must be a non-empty array")
		}
		if len(entries) > maxToolInputEnumEntries {
			return nil, toolSchemaError(jsonPath(path, "enum"), "exceeds the 256 entry limit")
		}
		for first := range entries {
			for second := 0; second < first; second++ {
				if equalJSONValues(entries[first], entries[second], 0) {
					return nil, toolSchemaError(jsonPath(path, "enum"), "must contain unique values")
				}
			}
		}
		compiled.enum = entries
		compiled.hasEnum = true
	}
	return compiled, nil
}

func (schema *compiledJSONSchema) validate(value any, path string, depth int) error {
	if depth > maxToolInputNestingDepth {
		return toolValidationError(path, "arguments exceed the nesting depth limit")
	}
	if schema.typeName != "" && !matchesJSONType(value, schema.typeName) {
		return toolValidationError(path, fmt.Sprintf("expected %s", schema.typeName))
	}
	if schema.hasEnum {
		matched := false
		for _, expected := range schema.enum {
			if equalJSONValues(value, expected, depth) {
				matched = true
				break
			}
		}
		if !matched {
			return toolValidationError(path, "value is not one of the allowed enum entries")
		}
	}
	if object, ok := value.(map[string]any); ok {
		for _, name := range schema.required {
			if _, exists := object[name]; !exists {
				return toolValidationError(jsonPath(path, name), "required property is missing")
			}
		}
		if schema.additionalProperties != nil && !*schema.additionalProperties {
			for _, name := range sortedMapKeys(object) {
				if _, known := schema.properties[name]; !known {
					return toolValidationError(jsonPath(path, name), "additional property is not allowed")
				}
			}
		}
		for _, name := range schema.propertyNames {
			property, exists := object[name]
			if !exists {
				continue
			}
			if err := schema.properties[name].validate(property, jsonPath(path, name), depth+1); err != nil {
				return err
			}
		}
	}
	if array, ok := value.([]any); ok {
		if schema.minItems != nil && len(array) < *schema.minItems {
			return toolValidationError(path, fmt.Sprintf("array must contain at least %d items", *schema.minItems))
		}
		if schema.items != nil {
			for index, item := range array {
				if err := schema.items.validate(item, jsonPath(path, strconv.Itoa(index)), depth+1); err != nil {
					return err
				}
			}
		}
	}
	if number, ok := jsonNumberRat(value); ok {
		if schema.minimum != nil && number.Cmp(schema.minimum) < 0 {
			return toolValidationError(path, "number is less than minimum")
		}
		if schema.maximum != nil && number.Cmp(schema.maximum) > 0 {
			return toolValidationError(path, "number is greater than maximum")
		}
	}
	return nil
}

func decodeBoundedJSON(value []byte, maxDepth int) (any, error) {
	if err := enforceJSONDepth(value, maxDepth); err != nil {
		return nil, err
	}
	if err := rejectDuplicateJSONKeys(value); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("JSON contains more than one value")
		}
		return nil, err
	}
	return decoded, nil
}

func enforceJSONDepth(value []byte, maximum int) error {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	depth := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			continue
		}
		switch delimiter {
		case '{', '[':
			depth++
			if depth > maximum {
				return fmt.Errorf("JSON exceeds the nesting depth limit of %d", maximum)
			}
		case '}', ']':
			depth--
		}
	}
}

func supportedJSONType(value string) bool {
	switch value {
	case "null", "boolean", "object", "array", "number", "string", "integer":
		return true
	default:
		return false
	}
}

func matchesJSONType(value any, expected string) bool {
	switch expected {
	case "null":
		return value == nil
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		number, ok := jsonNumberRat(value)
		return ok && number.IsInt()
	default:
		return false
	}
}

func nonnegativeJSONInteger(value any) (*big.Int, bool) {
	number, ok := jsonNumberRat(value)
	if !ok || !number.IsInt() || number.Sign() < 0 {
		return nil, false
	}
	return new(big.Int).Set(number.Num()), true
}

func jsonNumberRat(value any) (*big.Rat, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return nil, false
	}
	result, ok := new(big.Rat).SetString(number.String())
	return result, ok
}

func equalJSONValues(first any, second any, depth int) bool {
	if depth > maxToolInputNestingDepth {
		return false
	}
	if firstNumber, ok := jsonNumberRat(first); ok {
		secondNumber, secondOK := jsonNumberRat(second)
		return secondOK && firstNumber.Cmp(secondNumber) == 0
	}
	switch typed := first.(type) {
	case nil:
		return second == nil
	case bool:
		other, ok := second.(bool)
		return ok && typed == other
	case string:
		other, ok := second.(string)
		return ok && typed == other
	case []any:
		other, ok := second.([]any)
		if !ok || len(typed) != len(other) {
			return false
		}
		for index := range typed {
			if !equalJSONValues(typed[index], other[index], depth+1) {
				return false
			}
		}
		return true
	case map[string]any:
		other, ok := second.(map[string]any)
		if !ok || len(typed) != len(other) {
			return false
		}
		for key, value := range typed {
			otherValue, exists := other[key]
			if !exists || !equalJSONValues(value, otherValue, depth+1) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func sortedMapKeys[T any](values map[string]T) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func jsonPath(parent string, name string) string {
	escaped := strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1")
	return parent + "/" + escaped
}

func toolSchemaError(path string, message string) error {
	return fmt.Errorf("%w at %s: %s", ErrToolInputSchema, path, message)
}

func toolValidationError(path string, message string) error {
	return fmt.Errorf("%w at %s: %s", ErrToolInputValidation, path, message)
}

func nilInterface(value any) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
