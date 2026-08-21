package proxyhttp

import (
	"encoding/json"
	"testing"
)

func TestSanitizeResponsesTools_DropsNamespace(t *testing.T) {
	raw := []any{
		map[string]any{
			"type": "namespace",
			"tools": []any{
				map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        "read_file",
						"description": "read a file",
						"parameters":  map[string]any{"type": "object"},
					},
				},
			},
		},
		map[string]any{"type": "function", "name": "bash", "description": "run", "parameters": map[string]any{"type": "object"}},
	}
	out := sanitizeResponsesTools(raw)
	b, _ := json.Marshal(out)
	s := string(b)
	if contains(s, `"namespace"`) {
		t.Fatalf("namespace leaked: %s", s)
	}
	hasWeb, hasX, hasRead, hasBash := false, false, false, false
	for _, item := range out {
		m := item.(map[string]any)
		switch m["type"] {
		case "web_search":
			hasWeb = true
		case "x_search":
			hasX = true
		case "function":
			switch m["name"] {
			case "read_file":
				hasRead = true
			case "bash":
				hasBash = true
			}
		}
	}
	if hasWeb || hasX {
		t.Fatalf("native search must not be injected when the client sent function tools: %s", s)
	}
	if !hasRead || !hasBash {
		t.Fatalf("missing function tools: %s", s)
	}
}

func TestSanitizeResponsesTools_EmptyKeepsNoSearch(t *testing.T) {
	out := sanitizeResponsesTools([]any{})
	if len(out) != 0 {
		t.Fatalf("empty tools should stay empty, got %#v", out)
	}
}

func TestWithNativeSearch_OnlyWhenNoFunctions(t *testing.T) {
	fns := sanitizeResponsesTools([]any{
		map[string]any{"type": "function", "name": "bash", "parameters": map[string]any{"type": "object"}},
	})
	if hasFunctionTool(fns) && len(withNativeSearch(fns)) < 3 {
		t.Fatal("withNativeSearch should still append search when asked")
	}
	plain := withNativeSearch(nil)
	if len(plain) != 2 {
		t.Fatalf("expected web+x search, got %#v", plain)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

func TestSanitizeResponsesInput_TextToInputText(t *testing.T) {
	raw := []any{
		map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "hi"},
			},
		},
	}
	out := sanitizeResponsesInput(raw).([]any)
	b, _ := json.Marshal(out)
	s := string(b)
	if contains(s, `"type":"text"`) || contains(s, `"type": "text"`) {
		// loose check — also accept spaced JSON
	}
	if !contains(s, "input_text") {
		t.Fatalf("expected input_text normalization, got %s", s)
	}
	if contains(s, `"type":"text"`) {
		t.Fatalf("raw text type leaked: %s", s)
	}
}

func TestSanitizeResponsesInput_SingleObjectWrap(t *testing.T) {
	raw := map[string]any{"role": "user", "content": "hi"}
	out := sanitizeResponsesInput(raw)
	arr, ok := out.([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("expected array of 1, got %#v", out)
	}
}

func TestSanitizeResponsesInput_LocalShellCallCollapsed(t *testing.T) {
	raw := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{
			"type":    "local_shell_call",
			"call_id": "c1",
			"status":  "completed",
			"action":  map[string]any{"type": "exec", "command": []any{"echo", "1"}},
		},
	}
	out := sanitizeResponsesInput(raw).([]any)
	for _, item := range out {
		m := item.(map[string]any)
		if asString(m["type"]) == "local_shell_call" {
			t.Fatalf("local_shell_call not collapsed: %#v", m)
		}
	}
}

func TestSanitizeToolCall_NormalizesMissingFields(t *testing.T) {
	raw := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":      "bash",
			"arguments": `{"cmd":"ls"}`,
		},
	}
	sanitized := sanitizeToolCall(raw)
	if sanitized == nil {
		t.Fatal("expected non-nil")
	}
	if asString(sanitized["id"]) == "" {
		t.Fatal("expected id to be generated")
	}
	if asString(sanitized["type"]) != "function" {
		t.Fatalf("expected type=function, got %s", asString(sanitized["type"]))
	}
	fn := sanitized["function"].(map[string]any)
	if asString(fn["name"]) != "bash" {
		t.Fatalf("expected name=bash, got %s", asString(fn["name"]))
	}
	if asString(fn["arguments"]) != `{"cmd":"ls"}` {
		t.Fatalf("expected arguments={\"cmd\":\"ls\"}, got %s", asString(fn["arguments"]))
	}
}

func TestSanitizeToolCall_FixesInvalidArguments(t *testing.T) {
	raw := map[string]any{
		"id":   "call_1",
		"type": "function",
		"function": map[string]any{
			"name":      "bash",
			"arguments": `not-json`,
		},
	}
	sanitized := sanitizeToolCall(raw)
	fn := sanitized["function"].(map[string]any)
	if asString(fn["arguments"]) != "{}" {
		t.Fatalf("expected {} for invalid JSON, got %s", asString(fn["arguments"]))
	}
}

func TestSanitizeToolCall_NormalizesMapArguments(t *testing.T) {
	raw := map[string]any{
		"id":   "call_1",
		"type": "function",
		"function": map[string]any{
			"name":      "bash",
			"arguments": map[string]any{"cmd": "ls"},
		},
	}
	sanitized := sanitizeToolCall(raw)
	fn := sanitized["function"].(map[string]any)
	if asString(fn["arguments"]) != `{"cmd":"ls"}` {
		t.Fatalf("expected serialized JSON, got %s", asString(fn["arguments"]))
	}
}

func TestSanitizeToolCallsList_DropsInvalid(t *testing.T) {
	raw := []any{
		map[string]any{
			"id":   "call_1",
			"type": "function",
			"function": map[string]any{
				"name":      "bash",
				"arguments": `{"cmd":"ls"}`,
			},
		},
		"not-a-map",
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":      "",
				"arguments": `{}`,
			},
		},
	}
	out := sanitizeToolCallsList(raw)
	if len(out) != 1 {
		t.Fatalf("expected 1 valid tool call, got %d", len(out))
	}
	fn := out[0].(map[string]any)["function"].(map[string]any)
	if asString(fn["name"]) != "bash" {
		t.Fatalf("expected bash, got %s", asString(fn["name"]))
	}
}

func TestRepairToolSchema_DropsRequiredNotInProperties(t *testing.T) {
	// Exact shape from the field report: Moonshot rejects
	// "At path 'properties.migrations.items.required': required property
	// 'tag' is not defined in properties".
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"migrations": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":     "object",
					"required": []any{"tag"},
					"properties": map[string]any{
						"name": map[string]any{"type": "string"},
					},
				},
			},
		},
		"required": []any{"migrations"},
	}
	raw := []any{map[string]any{
		"type":       "function",
		"name":       "apply_migrations",
		"parameters": params,
	}}

	out := sanitizeChatTools(raw)
	fn := out[0].(map[string]any)["function"].(map[string]any)
	fixed := fn["parameters"].(map[string]any)

	items := fixed["properties"].(map[string]any)["migrations"].(map[string]any)["items"].(map[string]any)
	if _, ok := items["required"]; ok {
		b, _ := json.Marshal(fixed)
		t.Fatalf("nested orphan required survived: %s", b)
	}
	req, ok := fixed["required"].([]any)
	if !ok || len(req) != 1 || asString(req[0]) != "migrations" {
		b, _ := json.Marshal(fixed)
		t.Fatalf("valid top-level required was damaged: %s", b)
	}
}

func TestSanitizeResponsesTools_RepairsRequiredInSchema(t *testing.T) {
	raw := []any{map[string]any{
		"type": "function",
		"name": "apply_migrations",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
				"opts": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string"},
					},
					"required": []any{"name", "tag"},
				},
			},
		},
	}}
	out := sanitizeResponsesTools(raw)
	params := out[0].(map[string]any)["parameters"].(map[string]any)
	opts := params["properties"].(map[string]any)["opts"].(map[string]any)
	req, ok := opts["required"].([]any)
	if !ok || len(req) != 1 || asString(req[0]) != "name" {
		b, _ := json.Marshal(params)
		t.Fatalf("expected [name] kept and orphan tag dropped: %s", b)
	}

	// No properties at all: every required name counts as undefined for a
	// strict validator, so the whole key must go.
	raw2 := []any{map[string]any{
		"type":       "function",
		"name":       "no_props",
		"parameters": map[string]any{"type": "object", "required": []any{"ghost"}},
	}}
	out2 := sanitizeResponsesTools(raw2)
	params2 := out2[0].(map[string]any)["parameters"].(map[string]any)
	if _, ok := params2["required"]; ok {
		b, _ := json.Marshal(params2)
		t.Fatalf("orphan-only required should be removed: %s", b)
	}
}

func TestRepairToolSchema_StringRequiredAndEmptyRemoval(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []string{"ghost"},
	}
	repairToolSchema(schema)
	if _, ok := schema["required"]; ok {
		t.Fatalf("orphan-only required should be removed entirely")
	}

	schema2 := map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "string"}},
		"required":   []string{"a"},
	}
	repairToolSchema(schema2)
	req, ok := schema2["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "a" {
		b, _ := json.Marshal(schema2)
		t.Fatalf("valid string-required damaged: %s", b)
	}
}
