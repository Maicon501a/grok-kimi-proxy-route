package proxyhttp

import (
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

// xAI Responses API accepts only these tool type variants.
var allowedToolTypes = map[string]bool{
	"function":           true,
	"web_search":         true,
	"x_search":           true,
	"collections_search": true,
	"file_search":        true,
	"code_execution":     true,
	"code_interpreter":   true,
	"mcp":                true,
	"shell":              true,
}

func nativeSearchTools() []any {
	return []any{
		map[string]any{"type": "web_search"},
		map[string]any{"type": "x_search"},
	}
}

// sanitizeResponsesTools fixes OpenCode/OpenAI tool payloads so xAI accepts them.
// Rejects unknown types like "namespace" (causes 422: unknown variant `namespace`).
// Converts nested OpenAI function tools into xAI flat function tools.
// Does not inject native web_search / x_search: IDEs (OpenCode, Kilo) send their
// own function tools and the model must call those, not server-side search.
func sanitizeResponsesTools(raw any) []any {
	return sanitizeResponsesToolsCore(raw)
}

func hasFunctionTool(tools []any) bool {
	for _, item := range tools {
		m, _ := item.(map[string]any)
		if strings.EqualFold(asString(m["type"]), "function") {
			return true
		}
	}
	return false
}

func withNativeSearch(tools []any) []any {
	out := append([]any{}, tools...)
	hasWeb, hasX := false, false
	for _, item := range out {
		m, _ := item.(map[string]any)
		switch strings.ToLower(asString(m["type"])) {
		case "web_search":
			hasWeb = true
		case "x_search":
			hasX = true
		}
	}
	if !hasWeb {
		out = append(out, map[string]any{"type": "web_search"})
	}
	if !hasX {
		out = append(out, map[string]any{"type": "x_search"})
	}
	return out
}

func sanitizeResponsesToolsCore(raw any) []any {
	list := flattenToolList(raw)
	out := make([]any, 0, len(list)+2)
	seenFn := map[string]bool{}
	hasWeb, hasX := false, false

	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// Unwrap nested groups OpenCode sometimes sends as type=namespace / type=provider
		typ := strings.ToLower(strings.TrimSpace(asString(m["type"])))
		if typ == "namespace" || typ == "provider" || typ == "group" {
			if nested, ok := m["tools"].([]any); ok {
				for _, n := range sanitizeResponsesToolsCore(nested) {
					nm, _ := n.(map[string]any)
					nt := strings.ToLower(asString(nm["type"]))
					if nt == "function" {
						name := asString(nm["name"])
						if name == "" || seenFn[name] {
							continue
						}
						seenFn[name] = true
					}
					if nt == "web_search" {
						if hasWeb {
							continue
						}
						hasWeb = true
					}
					if nt == "x_search" {
						if hasX {
							continue
						}
						hasX = true
					}
					out = append(out, n)
				}
			}
			continue
		}
		norm := normalizeOneTool(m)
		if norm == nil {
			continue
		}
		if strings.EqualFold(asString(norm["type"]), "function") {
			repairToolSchema(norm["parameters"])
		}
		nt := strings.ToLower(asString(norm["type"]))
		switch nt {
		case "web_search":
			if hasWeb {
				continue
			}
			hasWeb = true
		case "x_search":
			if hasX {
				continue
			}
			hasX = true
		case "function":
			name := asString(norm["name"])
			if name == "" || seenFn[name] {
				continue
			}
			seenFn[name] = true
		}
		out = append(out, norm)
	}
	return out
}

func flattenToolList(raw any) []any {
	switch t := raw.(type) {
	case nil:
		return nil
	case []any:
		return t
	case map[string]any:
		// Some clients send tools as object map name→def
		out := make([]any, 0, len(t))
		for name, def := range t {
			if m, ok := def.(map[string]any); ok {
				cp := cloneMap(m)
				if asString(cp["type"]) == "" {
					cp["type"] = "function"
				}
				if asString(cp["name"]) == "" {
					cp["name"] = name
				}
				// nested OpenAI shape under value
				if fn, ok := cp["function"].(map[string]any); ok {
					if asString(fn["name"]) == "" {
						fn["name"] = name
						cp["function"] = fn
					}
				}
				out = append(out, cp)
			}
		}
		return out
	default:
		return nil
	}
}

func normalizeOneTool(m map[string]any) map[string]any {
	typ := strings.ToLower(strings.TrimSpace(asString(m["type"])))

	// Nested OpenAI: {type:function, function:{name,description,parameters}}
	if fn, ok := m["function"].(map[string]any); ok {
		name := asString(fn["name"])
		if name == "" {
			name = asString(m["name"])
		}
		if name == "" {
			return nil
		}
		out := map[string]any{
			"type":        "function",
			"name":        name,
			"description": firstNonEmpty(asString(fn["description"]), asString(m["description"])),
		}
		if p := fn["parameters"]; p != nil {
			out["parameters"] = p
		} else if p := fn["input_schema"]; p != nil {
			out["parameters"] = p
		} else if p := m["parameters"]; p != nil {
			out["parameters"] = p
		} else {
			out["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		return out
	}

	// Anthropic tool shape (no type or type tool): name + input_schema
	if typ == "" || typ == "tool" {
		name := asString(m["name"])
		if name == "" {
			return nil
		}
		out := map[string]any{
			"type":        "function",
			"name":        name,
			"description": asString(m["description"]),
		}
		if p := m["input_schema"]; p != nil {
			out["parameters"] = p
		} else if p := m["parameters"]; p != nil {
			out["parameters"] = p
		} else {
			out["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		return out
	}

	if !allowedToolTypes[typ] {
		// Drop unknown variants (namespace, custom, provider-defined, etc.)
		return nil
	}

	// Built-in server tools — pass only type (+ known optional filters)
	if typ != "function" {
		out := map[string]any{"type": typ}
		// preserve safe optional knobs
		for _, k := range []string{"filters", "allowed_domains", "excluded_domains", "enable_image_understanding", "enable_image_search", "vector_store_ids", "max_num_results"} {
			if v, ok := m[k]; ok {
				out[k] = v
			}
		}
		return out
	}

	// Flat function tool (xAI style)
	name := asString(m["name"])
	if name == "" {
		return nil
	}
	out := map[string]any{
		"type":        "function",
		"name":        name,
		"description": asString(m["description"]),
	}
	if p := m["parameters"]; p != nil {
		out["parameters"] = p
	} else if p := m["input_schema"]; p != nil {
		out["parameters"] = p
	} else {
		out["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return out
}

// repairToolSchema drops entries from every "required" array that are not
// declared in the sibling "properties" object. Strict backends reject payloads
// otherwise (Kimi/Moonshot: "is not a valid moonshot flavored json schema ...
// At path 'properties.migrations.items.required': required property 'tag' is
// not defined in properties"). Only schema-bearing keywords are walked so
// example/default payloads embedded in descriptions stay untouched.
func repairToolSchema(node any) {
	switch n := node.(type) {
	case map[string]any:
		props, _ := n["properties"].(map[string]any)
		if props != nil {
			for _, sub := range props {
				repairToolSchema(sub)
			}
		}
		// A nil properties map means every required name is undefined as far
		// as strict validators are concerned.
		switch req := n["required"].(type) {
		case []any:
			kept := make([]any, 0, len(req))
			for _, r := range req {
				if _, defined := props[asString(r)]; defined {
					kept = append(kept, r)
				}
			}
			if len(kept) == 0 {
				delete(n, "required")
			} else {
				n["required"] = kept
			}
		case []string:
			kept := make([]string, 0, len(req))
			for _, r := range req {
				if _, defined := props[r]; defined {
					kept = append(kept, r)
				}
			}
			if len(kept) == 0 {
				delete(n, "required")
			} else {
				n["required"] = kept
			}
		}
		for _, key := range []string{"items", "prefixItems", "anyOf", "oneOf", "allOf"} {
			repairToolSchema(n[key])
		}
		if ap, ok := n["additionalProperties"]; ok {
			repairToolSchema(ap) // bool form is a no-op
		}
		for _, key := range []string{"$defs", "definitions"} {
			if defs, ok := n[key].(map[string]any); ok {
				for _, sub := range defs {
					repairToolSchema(sub)
				}
			}
		}
	case []any:
		for _, item := range n {
			repairToolSchema(item)
		}
	}
}

// sanitizeChatTools keeps only function tools in OpenAI nested shape for /chat/completions.
func sanitizeChatTools(raw any) []any {
	list := flattenToolList(raw)
	out := make([]any, 0, len(list))
	seen := map[string]bool{}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ := strings.ToLower(asString(m["type"]))
		if typ == "namespace" || typ == "provider" || typ == "group" {
			if nested, ok := m["tools"].([]any); ok {
				out = append(out, sanitizeChatTools(nested)...)
			}
			continue
		}
		// Convert to OpenAI nested function form
		var name, desc string
		var params any
		if fn, ok := m["function"].(map[string]any); ok {
			name = asString(fn["name"])
			desc = asString(fn["description"])
			params = fn["parameters"]
			if params == nil {
				params = fn["input_schema"]
			}
		} else if typ == "function" || typ == "" || typ == "tool" {
			name = asString(m["name"])
			desc = asString(m["description"])
			params = m["parameters"]
			if params == nil {
				params = m["input_schema"]
			}
		} else {
			// skip server-side types on chat completions (xAI rejects many)
			continue
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		repairToolSchema(params)
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": desc,
				"parameters":  params,
			},
		})
	}
	return out
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		return ""
	}
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// sanitizeToolCall normalizes a single OpenAI-style tool_call so downstream clients always get a valid shape.
// It ensures: id is non-empty, type is "function", name is non-empty, and arguments is a valid JSON string.
func sanitizeToolCall(tc map[string]any) map[string]any {
	if tc == nil {
		return nil
	}
	out := cloneMap(tc)

	// Ensure id exists
	id := asString(out["id"])
	if id == "" {
		id = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
		out["id"] = id
	}

	// Ensure type is function
	if strings.ToLower(asString(out["type"])) != "function" {
		out["type"] = "function"
	}

	fn, _ := out["function"].(map[string]any)
	if fn == nil {
		fn = map[string]any{}
		out["function"] = fn
	}

	// Ensure name is non-empty
	name := strings.TrimSpace(asString(fn["name"]))
	if name == "" {
		return nil
	}
	fn["name"] = name

	// Normalize arguments to a valid JSON string
	args := fn["arguments"]
	switch a := args.(type) {
	case string:
		if a == "" {
			fn["arguments"] = "{}"
		} else {
			var tmp any
			if err := json.Unmarshal([]byte(a), &tmp); err != nil {
				// Invalid JSON: wrap literally as a string value or default to {}
				fn["arguments"] = "{}"
			} else {
				fn["arguments"] = a
			}
		}
	case map[string]any, []any:
		b, err := json.Marshal(a)
		if err != nil {
			fn["arguments"] = "{}"
		} else {
			fn["arguments"] = string(b)
		}
	case nil:
		fn["arguments"] = "{}"
	default:
		// Try to marshal anything else
		b, err := json.Marshal(a)
		if err != nil {
			fn["arguments"] = "{}"
		} else {
			fn["arguments"] = string(b)
		}
	}

	return out
}

// sanitizeToolCallsList applies sanitizeToolCall to every element in a list, dropping nil/invalid entries.
func sanitizeToolCallsList(raw []any) []any {
	out := make([]any, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		sanitized := sanitizeToolCall(m)
		if sanitized != nil && asString(sanitized["id"]) != "" {
			out = append(out, sanitized)
		}
	}
	return out
}
