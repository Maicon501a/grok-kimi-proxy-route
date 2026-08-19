package schema

import "encoding/json"

// ToolSpec is a duck-typed tool description used by ValidateToolCall. The
// HTTP layer passes the converter's tool slice, which marshals to this shape.
type ToolSpec struct {
	Function struct {
		Name       string          `json:"name"`
		Parameters json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// ValidateToolCall validates a named arguments map against the first tool whose
// function name matches. If no matching tool is found, validation passes.
func ValidateToolCall(name string, arguments map[string]any, tools []ToolSpec) Result {
	for _, t := range tools {
		if t.Function.Name != name {
			continue
		}
		if len(t.Function.Parameters) == 0 {
			return Result{Valid: true}
		}
		var params map[string]any
		if err := json.Unmarshal(t.Function.Parameters, &params); err != nil {
			return Result{Valid: true}
		}
		return Validate(arguments, params, "$."+name)
	}
	return Result{Valid: true}
}
