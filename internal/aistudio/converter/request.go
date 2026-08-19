// Package converter translates between OpenAI-compatible chat payloads and the
// MakerSuite/AI Studio nested-array RPC wire format.
//
// This file handles request-side conversion: OpenAI messages and tools into the
// array payload consumed by MakerSuiteService/GenerateContent.
package converter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/jsonx"
	"grok-desktop/internal/aistudio/models"
)

// RequestOptions carries the normalized request parameters used to build a
// GenerateContent payload.
type RequestOptions struct {
	Model             string
	Messages          []models.Message
	SystemInstruction string
	ThinkingLevel     string
	ImageAspectRatio  string
	Tools             []models.Tool
	ToolChoice        json.RawMessage
	ToolCallingMode   string
	Temperature       *float64
	TopP              *float64
	MaxTokens         *int
	Stream            bool
	SafetySettings    json.RawMessage
}

// BuildFunctionCallingSlot mirrors the Node implementation used by the
// original proxy. bridge_first/legacy mode sends tool descriptions inline in
// the description text; native_first sends the structured schema variant.
func BuildFunctionCallingSlot(tools []models.Tool, toolCallingMode string) any {
	if strings.ToLower(strings.TrimSpace(toolCallingMode)) != "native_first" {
		return buildLegacyFunctionCallingSlot(tools)
	}
	return buildNativeFunctionCallingSlot(tools)
}

// ToolsToMakerSuite converts OpenAI tools into the [[function_declarations]]
// array used at payload slot [6].
func ToolsToMakerSuite(tools []models.Tool) any {
	if len(tools) == 0 {
		return nil
	}
	declarations := make([]any, 0, len(tools))
	for _, tool := range tools {
		fn := tool.Function
		if fn.Parameters != nil {
			declarations = append(declarations, buildFunctionDeclaration(fn.Name, fn.Description, fn.Parameters))
		} else {
			declarations = append(declarations, buildFunctionDeclaration(fn.Name, fn.Description, nil))
		}
	}
	return []any{declarations}
}

func buildFunctionDeclaration(name, description string, parameters json.RawMessage) []any {
	if name == "" {
		name = "unknown"
	}
	var params map[string]any
	if len(parameters) > 0 {
		_ = json.Unmarshal(parameters, &params)
	}
	typeValue, _ := params["type"].(string)
	if typeValue == "" {
		typeValue = "OBJECT"
	}
	props, _ := params["properties"].(map[string]any)
	required, _ := params["required"].([]any)

	var propsArray any
	if len(props) > 0 {
		arr := make([]any, 0, len(props))
		for key, propRaw := range props {
			prop, _ := propRaw.(map[string]any)
			arr = append(arr, []any{
				key,
				propType(prop),
				propDescription(prop),
				containsString(required, key),
			})
		}
		propsArray = arr
	}

	return []any{name, description, []any{typeValue, propsArray}}
}

func propType(prop map[string]any) string {
	if t, ok := prop["type"].(string); ok && t != "" {
		return strings.ToUpper(t)
	}
	return "STRING"
}

func propDescription(prop map[string]any) any {
	if d, ok := prop["description"].(string); ok {
		return d
	}
	return nil
}

// NormalizedMessages is the output of NormalizeOpenAIMessages: a flat list of
// MakerSuite-role messages plus any extracted system instruction.
type NormalizedMessages struct {
	Messages          []models.Message
	SystemInstruction string
}

// NormalizeOpenAIMessages collapses OpenAI system/tool/assistant roles into the
// user/model roles AI Studio expects, optionally preserving native tool calls
// when nativeToolCalling is true.
func NormalizeOpenAIMessages(messages []models.Message, nativeToolCalling bool) NormalizedMessages {
	systemBlocks := make([]string, 0)
	result := make([]models.Message, 0, len(messages))
	toolNamesByCallID := make(map[string]string)

	for _, msg := range messages {
		if msg.Role == "" {
			continue
		}
		switch msg.Role {
		case "system":
			if t := contentToText(msg.Content); t != "" {
				systemBlocks = append(systemBlocks, t)
			}
		case "tool":
			if isTerminalToolMessage(msg) {
				continue
			}
			if nativeToolCalling {
				if native := buildNativeToolResultContent(msg, toolNamesByCallID); native != nil {
					result = append(result, models.Message{Role: "user", Content: mustMarshal(native)})
					continue
				}
			}
			result = append(result, models.Message{Role: "user", Content: mustMarshal(buildToolMessageText(msg))})
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				for _, call := range msg.ToolCalls {
					if call.ID != "" && call.Function.Name != "" {
						toolNamesByCallID[call.ID] = call.Function.Name
					}
				}
				if hasOnlyTerminalToolCalls(msg.ToolCalls) {
					continue
				}
				if nativeToolCalling {
					if calls := buildNativeAssistantToolCallContent(msg); len(calls) > 0 {
						result = append(result, models.Message{Role: "model", Content: mustMarshal(calls)})
						continue
					}
				}
				result = append(result, models.Message{Role: "model", Content: mustMarshal(buildAssistantToolCallText(msg))})
				continue
			}
			result = append(result, models.Message{Role: "model", Content: msg.Content})
		default:
			result = append(result, models.Message{Role: "user", Content: msg.Content})
		}
	}

	return NormalizedMessages{
		Messages:          result,
		SystemInstruction: strings.Join(systemBlocks, "\n\n"),
	}
}

// MessagesToMakerSuite converts normalized messages into the MakerSuite nested
// array format: [[[[null, "text"], ...], "role"], ...].
func MessagesToMakerSuite(messages []models.Message, systemPromptInjection string) []any {
	result := make([]any, 0, len(messages))

	for _, msg := range messages {
		role := msg.Role
		parts, isMultimodal := decodeMessageContent(msg.Content)

		if isMultimodal && len(parts) > 0 {
			encodedParts := make([]any, 0, len(parts))
			for _, part := range parts {
				encoded := encodeContentPart(part, role, systemPromptInjection)
				if encoded != nil {
					encodedParts = append(encodedParts, encoded)
				}
			}
			if len(encodedParts) > 0 {
				result = append(result, []any{encodedParts, role})
			}
			continue
		}

		text := textFromParts(parts)
		if role == "user" && systemPromptInjection != "" {
			text += systemPromptInjection
		}
		if text != "" {
			result = append(result, []any{[]any{[]any{nil, text}}, role})
		}
	}

	return result
}

func encodeContentPart(part models.ContentPart, role, injection string) any {
	switch part.Type {
	case "text":
		return []any{nil, part.Text}
	case "native_tool_call":
		return encodeNativeToolCallPart(part)
	case "native_tool_result":
		return encodeNativeToolResultPart(part)
	case "aistudio_file":
		if part.FileID != "" {
			return []any{nil, nil, nil, nil, nil, []any{part.FileID}}
		}
	case "aistudio_inline_image":
		if part.Data != "" {
			mime := part.MimeType
			if mime == "" {
				mime = "image/png"
			}
			return []any{nil, nil, []any{mime, part.Data}}
		}
	case "image_url":
		if part.ImageURL != nil && strings.HasPrefix(part.ImageURL.URL, "data:") {
			mime, b64 := splitDataURL(part.ImageURL.URL)
			if mime == "" {
				mime = "image/png"
			}
			return []any{nil, nil, []any{mime, b64}}
		}
	case "video_url":
		if part.VideoURL != nil && strings.HasPrefix(part.VideoURL.URL, "data:") {
			mime, b64 := splitDataURL(part.VideoURL.URL)
			if mime == "" {
				mime = "video/mp4"
			}
			return []any{nil, nil, []any{mime, b64}}
		}
	case "audio_url":
		if part.AudioURL != nil && strings.HasPrefix(part.AudioURL.URL, "data:") {
			mime, b64 := splitDataURL(part.AudioURL.URL)
			if mime == "" {
				mime = "audio/mpeg"
			}
			return []any{nil, nil, []any{mime, b64}}
		}
	}
	return nil
}

// BuildGenerateContentPayload assembles the full top-level MakerSuite array.
func BuildGenerateContentPayload(opts RequestOptions, cfg *config.Config) []any {
	model := opts.Model
	if model == "" {
		model = cfg.DefaultModel
	}
	toolsArray := ToolsToMakerSuite(opts.Tools)
	systemInjection := ""
	if len(opts.Tools) > 0 {
		systemInjection = cfg.PromptInjection.UserSuffixTools
	}

	safetyArray := buildSafetyArray(opts.SafetySettings)

	maxTokens := cfg.MaxTokensDefault
	if opts.MaxTokens != nil {
		maxTokens = *opts.MaxTokens
	}
	topP := cfg.TopPDefault
	if opts.TopP != nil {
		topP = *opts.TopP
	}
	temperature := cfg.TemperatureDefault
	if opts.Temperature != nil {
		temperature = *opts.Temperature
	}
	topK := cfg.TopKDefault

	return []any{
		model,
		MessagesToMakerSuite(opts.Messages, systemInjection),
		nil,
		[]any{
			safetyArray,
			nil,
			nil,
			maxTokens,
			topP,
			temperature,
			topK,
			nil,
			nil,
			0,
			nil,
			nil,
			nil,
			nil,
			toolsArray,
			nil,
		},
		nil,
		nil,
		toolsArraySlot(toolsArray),
		nil,
		nil,
		boolInt(opts.Stream),
		nil,
	}
}

func buildSafetyArray(safety json.RawMessage) any {
	if len(safety) == 0 {
		return safetyTuples(5)
	}
	var s string
	if err := json.Unmarshal(safety, &s); err == nil {
		switch strings.ToLower(s) {
		case "off":
			return safetyTuples(5)
		case "low":
			return safetyTuples(3)
		}
		return safetyTuples(5)
	}
	var arr []any
	if err := json.Unmarshal(safety, &arr); err == nil {
		return arr
	}
	return safetyTuples(5)
}

func safetyTuples(threshold int) []any {
	return []any{
		[]any{nil, nil, 7, threshold},
		[]any{nil, nil, 8, threshold},
		[]any{nil, nil, 9, threshold},
		[]any{nil, nil, 10, threshold},
	}
}

func buildNativeFunctionCallingSlot(tools []models.Tool) any {
	if len(tools) == 0 {
		return emptyFunctionCallingSlot()
	}

	entries := make([]any, 0, len(tools))
	for _, tool := range tools {
		def := tool.Function
		name := def.Name
		if name == "" {
			name = "unnamed_function"
		}
		entries = append(entries, []any{name, def.Description, buildNativeToolSchema(def.Parameters)})
	}
	return []any{[]any{nil, entries}}
}

func buildLegacyFunctionCallingSlot(tools []models.Tool) any {
	if len(tools) == 0 {
		return emptyFunctionCallingSlot()
	}

	entries := make([]any, 0, len(tools))
	for _, tool := range tools {
		def := tool.Function
		name := def.Name
		if name == "" {
			name = "unnamed_function"
		}
		entries = append(entries, []any{name, buildNativeToolDescription(def)})
	}
	return []any{[]any{nil, entries}}
}

func emptyFunctionCallingSlot() any {
	return []any{[]any{nil, nil, nil, []any{nil, []any{[]any{}}}}}
}

func buildNativeToolDescription(def models.FunctionSchema) string {
	description := def.Description
	if len(def.Parameters) == 0 {
		return description
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, def.Parameters); err != nil {
		return description
	}
	schema := compact.String()
	if schema == "" || schema == "{}" {
		return description
	}
	if description == "" {
		return "Arguments schema: " + schema
	}
	return description + "\nArguments schema: " + schema
}

func buildNativeToolSchema(parameters json.RawMessage) any {
	schema := map[string]any{"type": "object"}
	if len(parameters) > 0 {
		var parsed map[string]any
		if err := json.Unmarshal(parameters, &parsed); err == nil && len(parsed) > 0 {
			schema = parsed
		}
	}
	return buildNativeSchemaNode(schema)
}

func buildNativeSchemaNode(schema map[string]any) []any {
	typeName := strings.ToLower(strings.TrimSpace(asString(schema["type"])))
	if typeName == "" {
		typeName = "object"
	}
	description := optionalTrimmedString(schema["description"])

	switch typeName {
	case "string":
		return []any{1, nil, description}
	case "number":
		return []any{2, nil, description}
	case "integer":
		return []any{3, nil, description}
	case "boolean":
		return []any{4, nil, description}
	case "array":
		return []any{5, nil, description, nil, nil, buildNativeArrayItems(schema["items"])}
	default:
		properties := asMap(schema["properties"])
		required := asStringSlice(schema["required"])
		keys := make([]string, 0, len(properties))
		for key := range properties {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		propertyEntries := make([]any, 0, len(keys))
		for _, key := range keys {
			propertyEntries = append(propertyEntries, []any{key, buildNativeSchemaNode(asMap(properties[key]))})
		}

		return []any{6, nil, description, nil, nil, nil, propertyEntries, required}
	}
}

func buildNativeArrayItems(items any) []any {
	nested := buildNativeSchemaNode(asMap(items))
	if len(nested) > 0 {
		switch nested[0].(type) {
		case int, int32, int64, float32, float64:
			return []any{nested[0]}
		}
	}
	return []any{nested}
}

func asMap(value any) map[string]any {
	if m, ok := value.(map[string]any); ok && m != nil {
		return m
	}
	return map[string]any{}
}

func asString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func optionalTrimmedString(value any) any {
	s := strings.TrimSpace(asString(value))
	if s == "" {
		return nil
	}
	return s
}

func asStringSlice(value any) []string {
	arr, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s := strings.TrimSpace(asString(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func toolsArraySlot(toolsArray any) any {
	if toolsArray == nil {
		return nil
	}
	arr, _ := toolsArray.([]any)
	if len(arr) == 0 {
		return nil
	}
	return []any{[]any{nil, nil, nil, []any{nil, arr[0]}}}
}

func boolInt(b bool) any {
	if b {
		return 1
	}
	return nil
}

// --- helpers shared across the converter package ---

func contentToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []models.ContentPart
	if json.Unmarshal(raw, &parts) == nil {
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			if p.Type == "text" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

func decodeMessageContent(raw json.RawMessage) ([]models.ContentPart, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []models.ContentPart{{Type: "text", Text: s}}, false
	}
	var parts []models.ContentPart
	if json.Unmarshal(raw, &parts) == nil {
		return parts, true
	}
	return nil, false
}

func textFromParts(parts []models.ContentPart) string {
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

func containsString(list []any, target string) bool {
	for _, item := range list {
		if s, ok := item.(string); ok && s == target {
			return true
		}
	}
	return false
}

func isTerminalToolMessage(msg models.Message) bool {
	if msg.Name == "" {
		return false
	}
	return isTerminalToolName(msg.Name)
}

func hasOnlyTerminalToolCalls(calls []models.ToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	for _, c := range calls {
		if !isTerminalToolName(c.Function.Name) {
			return false
		}
	}
	return true
}

func isTerminalToolName(name string) bool {
	switch strings.ToLower(name) {
	case "end_of_turn", "end-of-turn", "memory_save", "memory-save":
		return true
	}
	return false
}

func buildToolMessageText(msg models.Message) string {
	name := msg.Name
	if name == "" {
		name = "tool"
	}
	toolCallID := ""
	if msg.ToolCallID != "" {
		toolCallID = "\nTool call id: " + msg.ToolCallID
	}
	return fmt.Sprintf("Tool result (%s):%s\n%s", name, toolCallID, contentToText(msg.Content))
}

func buildAssistantToolCallText(msg models.Message) string {
	calls := make([]any, 0, len(msg.ToolCalls))
	for i, tc := range msg.ToolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i+1)
		}
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		calls = append(calls, map[string]any{
			"id":   id,
			"type": "function",
			"function": map[string]any{
				"name":      orDefault(tc.Function.Name, "unknown_function"),
				"arguments": args,
			},
		})
	}
	payload := map[string]any{
		"role":       "assistant",
		"content":    nil,
		"tool_calls": calls,
	}
	encoded, _ := jsonx.MarshalIndent(payload, "", "  ")
	return "Assistant tool call history:\n```json\n" + string(encoded) + "\n```"
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func splitDataURL(url string) (mime, b64 string) {
	if !strings.HasPrefix(url, "data:") {
		return "", ""
	}
	comma := strings.Index(url, ",")
	if comma < 0 {
		return "", ""
	}
	header := url[5:comma]
	b64 = url[comma+1:]
	if i := strings.Index(header, ";"); i >= 0 {
		mime = header[:i]
	}
	return
}

// parseToolArguments coerces a raw arguments value into a map[string]any.
func parseToolArguments(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return map[string]any{"value": string(raw)}
	}
	switch t := v.(type) {
	case map[string]any:
		return t
	case []any:
		return map[string]any{"value": t}
	case string:
		// Nested JSON string: try to parse it.
		if nested := jsonx.DecodeLenient([]byte(t)); nested != nil {
			if obj, ok := nested.(map[string]any); ok {
				return obj
			}
		}
		return map[string]any{"value": t}
	default:
		return map[string]any{"value": t}
	}
}

func buildNativeToolResultContent(msg models.Message, toolNamesByCallID map[string]string) []models.ContentPart {
	name := strings.TrimSpace(msg.Name)
	if name == "" && msg.ToolCallID != "" {
		name = strings.TrimSpace(toolNamesByCallID[msg.ToolCallID])
	}
	if name == "" {
		return nil
	}
	return []models.ContentPart{{
		Type:       "native_tool_result",
		Name:       name,
		ToolCallID: msg.ToolCallID,
		Result:     mustMarshal(parseToolResultPayload(contentToText(msg.Content))),
	}}
}

func parseToolResultPayload(text string) any {
	text = strings.TrimSpace(text)
	if text == "" {
		return map[string]any{}
	}
	v := jsonx.DecodeLenient([]byte(text))
	if v == nil {
		return map[string]any{"result": text}
	}
	if obj, ok := v.(map[string]any); ok {
		return obj
	}
	return map[string]any{"result": v}
}

func buildNativeAssistantToolCallContent(msg models.Message) []models.ContentPart {
	parts := make([]models.ContentPart, 0, len(msg.ToolCalls))
	for i, tc := range msg.ToolCalls {
		if isTerminalToolName(tc.Function.Name) {
			continue
		}
		name := orDefault(tc.Function.Name, "unknown_function")
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i+1)
		}
		args := parseToolArguments(tcArguments(tc))
		parts = append(parts, models.ContentPart{
			Type:                           "native_tool_call",
			Name:                           name,
			Arguments:                      mustMarshal(args),
			ToolCallID:                     id,
			AistudioNativeToken:            tc.AistudioNativeToken,
			AistudioNativeArgumentsPayload: tc.AistudioNativeArgumentsPayload,
		})
	}
	return parts
}

func tcArguments(tc models.ToolCall) json.RawMessage {
	return json.RawMessage(tc.Function.Arguments)
}
