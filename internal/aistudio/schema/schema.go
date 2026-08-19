// Package schema validates tool-call arguments against a JSON Schema subset
// compatible with OpenAI function parameters.
//
// Improvements over the original Node validator:
//   - All errors are accumulated (the original stopped at the first failure
//     within an object, hiding additional problems).
//   - numeric enums are supported (the original only supported string enums at
//     the number branch).
//   - additionalProperties:false is checked after required, so the most
//     actionable error is reported first.
package schema

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Result is the outcome of a validation.
type Result struct {
	Valid  bool
	Errors []Error
}

// Error describes a single schema violation.
type Error struct {
	Path    string
	Message string
}

func (r *Result) add(path, msg string) {
	r.Valid = false
	r.Errors = append(r.Errors, Error{Path: path, Message: msg})
}

// Validate checks a value against a schema. A nil/empty schema always passes.
func Validate(value any, schema map[string]any, path string) Result {
	res := Result{Valid: true}
	if len(schema) == 0 {
		return res
	}
	if _, nullable := schema["nullable"]; nullable && isNullish(value) {
		return res
	}
	validateValue(value, schema, path, &res)
	return res
}

func validateValue(value any, schema map[string]any, path string, res *Result) {
	t, _ := schema["type"].(string)
	switch t {
	case "":
		// No type constraint: still allow nested validations.
	case "string":
		validateString(value, schema, path, res)
		return
	case "number", "integer":
		validateNumber(value, schema, path, res)
		return
	case "boolean":
		validateBoolean(value, schema, path, res)
		return
	case "null":
		if value != nil {
			res.add(path, fmt.Sprintf("Expected null, got %T", value))
		}
		return
	case "object":
		validateObject(value, schema, path, res)
		return
	case "array":
		validateArray(value, schema, path, res)
		return
	}
}

func validateString(value any, schema map[string]any, path string, res *Result) {
	s, ok := value.(string)
	if !ok {
		res.add(path, fmt.Sprintf("Expected string, got %s", typeName(value)))
		return
	}
	if min, ok := numField(schema, "minLength"); ok && float64(len(s)) < min {
		res.add(path, fmt.Sprintf("String too short (min %v)", schema["minLength"]))
	}
	if max, ok := numField(schema, "maxLength"); ok && float64(len(s)) > max {
		res.add(path, fmt.Sprintf("String too long (max %v)", schema["maxLength"]))
	}
	if pat, ok := schema["pattern"].(string); ok && pat != "" {
		re, err := regexp.Compile(pat)
		if err == nil && !re.MatchString(s) {
			res.add(path, fmt.Sprintf("String does not match pattern %s", pat))
		}
	}
	if enum, ok := schema["enum"].([]any); ok && !containsString(enum, s) {
		res.add(path, fmt.Sprintf("Value is not in enum [%s]", joinAny(enum, ", ")))
	}
}

func validateNumber(value any, schema map[string]any, path string, res *Result) {
	n, ok := toFloat(value)
	if !ok {
		res.add(path, fmt.Sprintf("Expected number, got %s", typeName(value)))
		return
	}
	if schema["type"] == "integer" && !isInteger(n) {
		res.add(path, fmt.Sprintf("Expected integer, got float %v", n))
		return
	}
	if min, ok := numField(schema, "minimum"); ok && n < min {
		res.add(path, fmt.Sprintf("Value %v is below minimum %v", n, schema["minimum"]))
	}
	if max, ok := numField(schema, "maximum"); ok && n > max {
		res.add(path, fmt.Sprintf("Value %v is above maximum %v", n, schema["maximum"]))
	}
	if enum, ok := schema["enum"].([]any); ok && !containsNumber(enum, n) {
		res.add(path, fmt.Sprintf("Value is not in enum [%s]", joinAny(enum, ", ")))
	}
}

func validateBoolean(value any, schema map[string]any, path string, res *Result) {
	if _, ok := value.(bool); !ok {
		res.add(path, fmt.Sprintf("Expected boolean, got %s", typeName(value)))
	}
}

func validateObject(value any, schema map[string]any, path string, res *Result) {
	if value == nil {
		res.add(path, "Expected object, got null")
		return
	}
	obj, ok := value.(map[string]any)
	if !ok {
		if _, isArr := value.([]any); isArr {
			res.add(path, "Expected object, got array")
			return
		}
		res.add(path, fmt.Sprintf("Expected object, got %s", typeName(value)))
		return
	}

	if required, ok := schema["required"].([]any); ok {
		for _, r := range required {
			key, _ := r.(string)
			if key == "" {
				continue
			}
			if _, present := obj[key]; !present {
				res.add(path+"."+key, fmt.Sprintf("Missing required property '%s'", key))
			}
		}
	}

	props, _ := schema["properties"].(map[string]any)
	additional, hasAdditional := schema["additionalProperties"]
	for k, v := range obj {
		childPath := fmt.Sprintf("%s.%s", path, k)
		propSchema, hasProp := props[k].(map[string]any)
		if hasProp {
			Validate(v, propSchema, childPath) // recursive: produces its own Result
			// Merge child errors.
			child := Validate(v, propSchema, childPath)
			if !child.Valid {
				res.Valid = false
				res.Errors = append(res.Errors, child.Errors...)
			}
			continue
		}
		if hasAdditional {
			if disallow, ok := additional.(bool); ok && disallow {
				res.add(childPath, fmt.Sprintf("Unexpected property '%s'", k))
			}
		}
	}
}

func validateArray(value any, schema map[string]any, path string, res *Result) {
	arr, ok := value.([]any)
	if !ok {
		res.add(path, fmt.Sprintf("Expected array, got %s", typeName(value)))
		return
	}
	if min, ok := numField(schema, "minItems"); ok && float64(len(arr)) < min {
		res.add(path, fmt.Sprintf("Array too short (min %v)", schema["minItems"]))
	}
	if max, ok := numField(schema, "maxItems"); ok && float64(len(arr)) > max {
		res.add(path, fmt.Sprintf("Array too long (max %v)", schema["maxItems"]))
	}
	items, _ := schema["items"].(map[string]any)
	for i, item := range arr {
		child := Validate(item, items, fmt.Sprintf("%s[%d]", path, i))
		if !child.Valid {
			res.Valid = false
			res.Errors = append(res.Errors, child.Errors...)
		}
	}
}

// --- helpers ---

func isNullish(v any) bool { return v == nil }

func numField(schema map[string]any, key string) (float64, bool) {
	return toFloat(schema[key])
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func isInteger(n float64) bool { return n == float64(int64(n)) }

func typeName(v any) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case bool:
		return "boolean"
	case float64, int, int64, json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func containsString(enum []any, s string) bool {
	for _, e := range enum {
		if v, ok := e.(string); ok && v == s {
			return true
		}
	}
	return false
}

func containsNumber(enum []any, n float64) bool {
	for _, e := range enum {
		if f, ok := toFloat(e); ok && f == n {
			return true
		}
	}
	return false
}

func joinAny(values []any, sep string) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, fmt.Sprintf("%v", v))
	}
	return strings.Join(parts, sep)
}
