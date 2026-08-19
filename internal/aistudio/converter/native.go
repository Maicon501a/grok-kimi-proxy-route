package converter

import (
	"encoding/json"
	"fmt"

	"grok-desktop/internal/aistudio/jsonx"
	"grok-desktop/internal/aistudio/models"
)

// encodeNativeToolCallPart builds the AI Studio nested-array payload for an
// assistant native tool call part.
func encodeNativeToolCallPart(part models.ContentPart) any {
	if part.Type != "native_tool_call" {
		return nil
	}
	name := part.Name
	if name == "" {
		return nil
	}

	var preservedPayload []any
	if len(part.AistudioNativeArgumentsPayload) > 0 {
		if v, err := jsonx.Decode(part.AistudioNativeArgumentsPayload); err == nil && v != nil {
			if arr, ok := v.([]any); ok {
				preservedPayload = arr
			}
		}
	}

	var entries []any
	if preservedPayload != nil {
		entries = preservedPayload
	} else {
		args := parseToolArguments(part.Arguments)
		entries = make([]any, 0, len(args))
		for k, v := range args {
			entries = append(entries, []any{[]any{k, encodeNativeStructuredValue(v)}})
		}
	}

	toolCallID := part.ToolCallID
	if toolCallID == "" {
		toolCallID = fmt.Sprintf("call_%d", 0)
	}
	tuple := []any{name, entries, toolCallID}

	payload := make([]any, 15)
	for i := range payload {
		payload[i] = nil
	}
	payload[10] = tuple
	if part.AistudioNativeToken != "" {
		payload[14] = part.AistudioNativeToken
	}
	return payload
}

// encodeNativeToolResultPart builds the AI Studio nested-array payload for a
// tool result part.
func encodeNativeToolResultPart(part models.ContentPart) any {
	if part.Type != "native_tool_result" {
		return nil
	}
	name := part.Name
	if name == "" {
		return nil
	}

	var raw map[string]any
	if len(part.Result) > 0 {
		_ = json.Unmarshal(part.Result, &raw)
	}
	if raw == nil {
		raw = map[string]any{}
	}

	entries := make([]any, 0, len(raw))
	for k, v := range raw {
		entries = append(entries, []any{k, encodeNativeStructuredValue(v)})
	}

	toolCallID := part.ToolCallID
	if toolCallID == "" {
		toolCallID = fmt.Sprintf("call_%d", 0)
	}
	tuple := []any{name, []any{entries}, toolCallID}

	payload := make([]any, 12)
	for i := range payload {
		payload[i] = nil
	}
	payload[11] = tuple
	return payload
}

// encodeNativeStructuredValue mirrors the structured-value encoder of the
// original JS implementation, producing the nested tuple form AI Studio uses
// for function arguments.
func encodeNativeStructuredValue(value any) any {
	switch v := value.(type) {
	case nil:
		return []any{nil}
	case string:
		return []any{nil, nil, v}
	case float64:
		if isFloatInteger(v) {
			return []any{nil, v}
		}
		return []any{nil, v}
	case int:
		return []any{nil, float64(v)}
	case int64:
		return []any{nil, float64(v)}
	case json.Number:
		f, err := v.Float64()
		if err == nil {
			return []any{nil, f}
		}
		return []any{nil, nil, v.String()}
	case bool:
		// google.protobuf.Value field order is null, number, string, bool,
		// struct, list. Putting a boolean in slot zero makes protobuf decode it
		// as the NullValue enum and AI Studio rejects false/true as invalid.
		return []any{nil, nil, nil, v}
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, []any{encodeNativeStructuredValue(item)})
		}
		return []any{nil, nil, nil, nil, nil, out}
	case map[string]any:
		out := make([]any, 0, len(v))
		for k, nested := range v {
			out = append(out, []any{k, encodeNativeStructuredValue(nested)})
		}
		return out
	}
	return []any{nil, nil, fmt.Sprintf("%v", value)}
}

func isFloatInteger(f float64) bool {
	return f == float64(int64(f))
}
