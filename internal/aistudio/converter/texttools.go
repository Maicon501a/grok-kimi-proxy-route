package converter

// This file extracts tool calls embedded in free-form model text: fenced JSON
// blocks (```json / ~~~json) and inline objects containing tool_execution /
// function_call markers. It mirrors parseTextToolCalls from the original Node
// implementation, with corrected fence-aware marker scanning and balanced
// brace detection that correctly ignores braces inside strings.

import (
	"encoding/json"
	"regexp"
	"strings"

	"grok-desktop/internal/aistudio/jsonx"
	"grok-desktop/internal/aistudio/models"
)

// parsedText is the outcome of parseTextToolCalls.
type parsedText struct {
	TextContent         string
	ToolCalls           []FunctionCall
	RejectedToolCalls   []FunctionCall
	HasUnclosedToolCall bool
}

// fenceOpenerRe matches the opening line of a ```json / ~~~json fenced block.
// RE2 (Go's regexp) does not support backreferences, so we match opening fences
// and scan for the matching closing fence manually via findFenceClose.
var fenceOpenerRe = regexp.MustCompile(`(?m)(?:^|\n)(` + "`{3,}" + `|~{3,})[ \t]*json[^\n]*\n`)

// markerRe locates tool-protocol markers within the text.
var markerRe = regexp.MustCompile(`"tool_execution"|"tool_calls"|"function_call"`)

// parseTextToolCalls scans text for embedded tool-call JSON and returns the
// cleaned text plus accepted/rejected calls.
func parseTextToolCalls(text string, opts ToolParseOptions) parsedText {
	result := parsedText{}
	source := text

	lastIndex := 0
	for {
		loc := fenceOpenerRe.FindStringSubmatchIndex(source[lastIndex:])
		if loc == nil {
			break
		}
		base := lastIndex + loc[0]
		prefixEnd := base
		if source[base] == '\n' {
			prefixEnd = base + 1
		} else if base > 0 && source[base-1] == '\n' {
			prefixEnd = base
		}
		markerStart := lastIndex + loc[2]
		markerEnd := lastIndex + loc[3]
		marker := source[markerStart:markerEnd]
		markerCh := marker[0]
		jsonStart := lastIndex + loc[1]

		jsonEnd, found := findFenceClose(source, jsonStart, markerCh, len(marker))
		if !found {
			// Unclosed fence: mark suspicious and stop.
			result.HasUnclosedToolCall = true
			result.TextContent += source[lastIndex:prefixEnd]
			lastIndex = len(source)
			break
		}

		jsonSource := source[jsonStart:jsonEnd]
		parsed := parseToolCallJson(jsonSource, len(result.ToolCalls), opts)
		if !parsed.shouldSuppress() {
			lastIndex = jsonEnd + len(marker)
			continue
		}

		result.TextContent += source[lastIndex:prefixEnd]
		result.ToolCalls = append(result.ToolCalls, parsed.calls...)
		result.RejectedToolCalls = append(result.RejectedToolCalls, parsed.rejected...)
		if parsed.parseFailed || (parsed.suspicious && len(parsed.calls) == 0 && len(parsed.rejected) == 0) {
			result.HasUnclosedToolCall = true
		}
		// Advance past the closing fence (to end of its line).
		afterClose := jsonEnd + len(marker)
		// Skip trailing spaces and a single newline.
		for afterClose < len(source) && (source[afterClose] == ' ' || source[afterClose] == '\t') {
			afterClose++
		}
		if afterClose < len(source) && source[afterClose] == '\r' {
			afterClose++
		}
		if afterClose < len(source) && source[afterClose] == '\n' {
			afterClose++
		}
		lastIndex = afterClose
	}
	result.TextContent += source[lastIndex:]

	// Marker pass: scan remaining text for inline tool_protocol objects.
	result.TextContent = scanInlineMarkers(result.TextContent, opts, &result)

	if !result.HasUnclosedToolCall {
		lower := strings.ToLower(source)
		lastFence := lastIndexOf(lower, "```json", "~~~json")
		if lastFence >= 0 {
			tail := source[lastFence:]
			closeIdx := strings.Index(tail[3:], "```")
			closeTildeIdx := strings.Index(tail[3:], "~~~")
			hasClose := false
			if strings.HasPrefix(tail, "```json") {
				hasClose = closeIdx >= 0
			} else {
				hasClose = closeTildeIdx >= 0
			}
			if !hasClose && looksLikeToolProtocol(tail) {
				result.HasUnclosedToolCall = true
			}
		}
	}

	result.TextContent = strings.TrimSpace(result.TextContent)
	return result
}

// findFenceClose scans forward from start looking for a fence line made of the
// same marker character with at least markerLen characters. Returns the index
// just before the closing fence, and true if found.
func findFenceClose(source string, start int, markerCh byte, markerLen int) (int, bool) {
	i := start
	for i < len(source) {
		// A closing fence must be at the start of a line.
		if i > start && source[i-1] != '\n' {
			i++
			continue
		}
		if source[i] != markerCh {
			i++
			continue
		}
		// Count run length of markerCh.
		run := 0
		j := i
		for j < len(source) && source[j] == markerCh {
			run++
			j++
		}
		if run >= markerLen {
			// Ensure rest of line is only whitespace.
			k := j
			for k < len(source) && source[k] != '\n' {
				if source[k] != ' ' && source[k] != '\t' && source[k] != '\r' {
					break
				}
				k++
			}
			if k >= len(source) || source[k] == '\n' || source[k] == '\r' {
				// Trim trailing whitespace/newline before the fence from JSON.
				end := i
				for end > start && (source[end-1] == '\n' || source[end-1] == '\r') {
					end--
				}
				return end, true
			}
		}
		i++
	}
	return 0, false
}

func scanInlineMarkers(working string, opts ToolParseOptions, result *parsedText) string {
	indices := markerRe.FindAllStringIndex(working, -1)
	for i := 0; i < len(indices); i++ {
		markerStart := indices[i][0]
		openBrace := strings.LastIndex(working[:markerStart], "{")
		if openBrace < 0 {
			continue
		}
		if isInsideMarkdownCodeBlock(working, openBrace, 0) {
			continue
		}
		jsonEnd := findBalancedJsonObjectEnd(working, openBrace)
		if jsonEnd < 0 {
			result.HasUnclosedToolCall = true
			working = strings.TrimRight(working[:openBrace], " \t\r\n")
			break
		}
		parsed := parseToolCallJson(working[openBrace:jsonEnd], len(result.ToolCalls), opts)
		if !parsed.shouldSuppress() {
			continue
		}
		result.ToolCalls = append(result.ToolCalls, parsed.calls...)
		result.RejectedToolCalls = append(result.RejectedToolCalls, parsed.rejected...)
		if parsed.parseFailed || (parsed.suspicious && len(parsed.calls) == 0 && len(parsed.rejected) == 0) {
			result.HasUnclosedToolCall = true
		}
		working = working[:openBrace] + working[jsonEnd:]
		// Restart scanning since indices shifted.
		indices = markerRe.FindAllStringIndex(working, -1)
		i = -1
	}
	return working
}

func lastIndexOf(s string, needles ...string) int {
	last := -1
	for _, n := range needles {
		if idx := strings.LastIndex(s, n); idx > last {
			last = idx
		}
	}
	return last
}

// parsedJson is the outcome of parseToolCallJson.
type parsedJson struct {
	calls       []FunctionCall
	rejected    []FunctionCall
	suspicious  bool
	parseFailed bool
}

func (p parsedJson) shouldSuppress() bool {
	return len(p.calls) > 0 || len(p.rejected) > 0 || p.parseFailed || p.suspicious
}

func parseToolCallJson(jsonSource string, offset int, opts ToolParseOptions) parsedJson {
	parsed := jsonx.DecodeLenient([]byte(jsonSource))
	if parsed == nil {
		suspicious := looksLikeToolProtocol(jsonSource)
		return parsedJson{suspicious: suspicious, parseFailed: suspicious}
	}

	normalized := normalizeToolCallObject(parsed, offset, opts)
	calls := make([]FunctionCall, 0, len(normalized))
	rejected := make([]FunctionCall, 0)
	for _, call := range normalized {
		reason := validateToolCallName(call.Name, opts)
		if reason != "" {
			call.Reason = reason
			rejected = append(rejected, call)
		} else {
			calls = append(calls, call)
		}
	}
	return parsedJson{
		calls:      calls,
		rejected:   rejected,
		suspicious: len(normalized) > 0 || looksLikeToolProtocol(jsonSource),
	}
}

func normalizeToolCallObject(obj any, offset int, opts ToolParseOptions) []FunctionCall {
	m, ok := obj.(map[string]any)
	if !ok {
		return nil
	}

	if list, ok := m["tool_calls"].([]any); ok {
		out := make([]FunctionCall, 0, len(list))
		for i, raw := range list {
			if call := normalizeOpenAIToolCall(raw, offset+i); call != nil {
				out = append(out, *call)
			}
		}
		return out
	}

	if fc, ok := m["function_call"].(map[string]any); ok {
		name, _ := fc["name"].(string)
		id, _ := fc["id"].(string)
		if id == "" {
			if objID, ok := m["id"].(string); ok {
				id = objID
			}
		}
		if p := normalizeOpenAIFunctionPayload(name, fc["arguments"], id, nil); p != nil {
			return []FunctionCall{*p}
		}
	}

	if toolExec, ok := m["tool_execution"].(string); ok && toolExec != "" {
		args, _ := m["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		id, _ := m["id"].(string)
		return []FunctionCall{{Name: toolExec, Arguments: args, ID: orDefaultString(id, toolCallID(offset))}}
	}

	if name, ok := m["name"].(string); ok && name != "" {
		if argsRaw, exists := m["arguments"]; exists {
			args := coerceArguments(argsRaw)
			id, _ := m["id"].(string)
			return []FunctionCall{{Name: name, Arguments: args, ID: orDefaultString(id, toolCallID(offset))}}
		}
	}

	if _, hasName := m["name"]; !hasName {
		if _, hasExec := m["tool_execution"]; !hasExec {
			if args, ok := m["arguments"].(map[string]any); ok && len(args) > 0 {
				if inferred := inferToolNameFromParameters(args, opts.Tools); inferred != "" {
					id, _ := m["id"].(string)
					return []FunctionCall{{Name: inferred, Arguments: args, ID: orDefaultString(id, toolCallID(offset))}}
				}
			}
		}
	}

	return nil
}

func normalizeOpenAIToolCall(raw any, index int) *FunctionCall {
	call, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	if t, _ := call["type"].(string); t != "" && t != "function" {
		return nil
	}
	fn, _ := call["function"].(map[string]any)
	if fn == nil {
		return nil
	}
	name, _ := fn["name"].(string)
	args := fn["arguments"]
	id, _ := call["id"].(string)
	if id == "" {
		id = toolCallID(index)
	}
	return normalizeOpenAIFunctionPayload(name, args, id, fn)
}

func normalizeOpenAIFunctionPayload(name string, argsValue any, id string, extras map[string]any) *FunctionCall {
	if name == "" {
		return nil
	}
	args := coerceArguments(argsValue)
	call := &FunctionCall{
		Name:      name,
		Arguments: args,
		ID:        id,
	}
	if token, ok := extras["aistudio_native_token"].(string); ok && token != "" {
		call.AistudioNativeToken = token
	}
	if payload, ok := extras["aistudio_native_arguments_payload"]; ok && payload != nil {
		if raw, err := jsonx.Marshal(payload); err == nil {
			call.AistudioNativeArgumentsPayload = raw
		}
	}
	return call
}

func coerceArguments(value any) map[string]any {
	switch v := value.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		return v
	case string:
		if decoded := jsonx.DecodeLenient([]byte(v)); decoded != nil {
			if obj, ok := decoded.(map[string]any); ok {
				return obj
			}
			return map[string]any{"value": decoded}
		}
		return map[string]any{"value": v}
	default:
		return map[string]any{"value": v}
	}
}

func orDefaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func inferToolNameFromParameters(args map[string]any, tools []models.Tool) string {
	if len(args) == 0 || len(tools) == 0 {
		return ""
	}
	argKeys := make([]string, 0, len(args))
	for k := range args {
		argKeys = append(argKeys, k)
	}
	matches := make([]models.Tool, 0)
	for _, tool := range tools {
		var params map[string]any
		if len(tool.Function.Parameters) > 0 {
			_ = json.Unmarshal(tool.Function.Parameters, &params)
		}
		props, _ := params["properties"].(map[string]any)
		if containsAll(props, argKeys) {
			matches = append(matches, tool)
		}
	}
	if len(matches) == 1 {
		return matches[0].Function.Name
	}
	return ""
}

func containsAll(props map[string]any, keys []string) bool {
	for _, k := range keys {
		if _, ok := props[k]; !ok {
			return false
		}
	}
	return true
}

// --- fence / brace helpers ---

func advanceMarkdownCodeState(text string, delimiterLen int) int {
	for i := 0; i < len(text); {
		ch := text[i]
		if ch != '`' && ch != '~' {
			i++
			continue
		}
		marker := ch
		run := 1
		for i+run < len(text) && text[i+run] == marker {
			run++
		}
		if delimiterLen == 0 {
			delimiterLen = run
		} else if run >= delimiterLen {
			delimiterLen = 0
		}
		i += run
	}
	return delimiterLen
}

func isInsideMarkdownCodeBlock(source string, position, initialDelimiterLen int) bool {
	return advanceMarkdownCodeState(source[:position], initialDelimiterLen) != 0
}

func findBalancedJsonObjectEnd(source string, openBrace int) int {
	if openBrace < 0 || openBrace >= len(source) || source[openBrace] != '{' {
		return -1
	}
	depth := 0
	inString := false
	escaped := false
	for i := openBrace; i < len(source); i++ {
		ch := source[i]
		if inString {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}
