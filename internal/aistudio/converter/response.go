package converter

// This file decodes AI Studio GenerateContent responses. The wire format is a
// nested JSON array whose structure is opaque, so the decoder walks the tree
// generically to extract text, reasoning parts and function calls.

import (
	"encoding/json"
	"regexp"
	"strings"

	"grok-desktop/internal/aistudio/jsonx"
	"grok-desktop/internal/aistudio/models"
)

// ToolParseOptions carries the tools/tool-choice context needed to validate
// parsed function calls.
type ToolParseOptions struct {
	Tools      []models.Tool
	ToolChoice json.RawMessage
}

// ParsedResponse is the structured view of an AI Studio GenerateContent body.
type ParsedResponse struct {
	TextParts           []string
	ReasoningParts      []string
	FunctionCalls       []FunctionCall
	RejectedToolCalls   []FunctionCall
	Images              []ParsedImage
	HasUnclosedToolCall bool
	ValidationErrors    []ValidationError
	Usage               models.Usage
	Raw                 string
}

// ParsedImage is an inline image extracted from an AI Studio response.
type ParsedImage struct {
	MimeType string
	Data     string
}

// FunctionCall is a normalized tool call.
type FunctionCall struct {
	Name      string
	Arguments map[string]any
	ID        string

	AistudioNativeToken            string
	AistudioNativeArgumentsPayload json.RawMessage
	Reason                         string `json:",omitempty"`
}

// ValidationError is produced when a parsed tool call fails schema validation.
type ValidationError struct {
	Path    string
	Message string
}

// ParseGenerateContentResponse decodes a full GenerateContent body.
func ParseGenerateContentResponse(body string, opts ToolParseOptions) ParsedResponse {
	res := ParsedResponse{Raw: body}

	v, err := jsonx.Decode([]byte(body))
	if err != nil || v == nil {
		return res
	}
	arr, err := jsonx.AsArray(v)
	if err != nil {
		return res
	}

	if chunks := unwrapGenerateContentChunks(arr); chunks != nil {
		for _, chunk := range chunks {
			text := extractTextFromGenerateChunk(chunk)
			if text != "" {
				if classifyGenerateChunk(chunk) == "reasoning" {
					res.ReasoningParts = append(res.ReasoningParts, text)
				} else {
					res.TextParts = append(res.TextParts, text)
				}
			}
			extractFunctionCallsFromArray(chunk, &res.FunctionCalls)
			extractImagesFromArray(chunk, &res.Images)
			updateUsageFromGenerateChunk(chunk, &res.Usage)
		}

		native := normalizeAndValidateFunctionCalls(res.FunctionCalls, opts)
		res.FunctionCalls = native.calls
		res.RejectedToolCalls = append(res.RejectedToolCalls, native.rejected...)

		parsedText := parseTextToolCalls(strings.Join(res.TextParts, ""), opts)
		if len(res.FunctionCalls) == 0 && len(parsedText.ToolCalls) > 0 {
			res.FunctionCalls = parsedText.ToolCalls
		}
		res.RejectedToolCalls = append(res.RejectedToolCalls, parsedText.RejectedToolCalls...)
		res.HasUnclosedToolCall = parsedText.HasUnclosedToolCall
		if len(parsedText.ToolCalls) > 0 || len(parsedText.RejectedToolCalls) > 0 {
			if parsedText.TextContent != "" {
				res.TextParts = []string{parsedText.TextContent}
			} else {
				res.TextParts = nil
			}
		}
		return res
	}

	extractFromArray(arr, &res)

	native := normalizeAndValidateFunctionCalls(res.FunctionCalls, opts)
	res.FunctionCalls = native.calls
	res.RejectedToolCalls = append(res.RejectedToolCalls, native.rejected...)

	parsedText := parseTextToolCalls(strings.Join(res.TextParts, ""), opts)
	if len(res.FunctionCalls) == 0 && len(parsedText.ToolCalls) > 0 {
		res.FunctionCalls = parsedText.ToolCalls
	}
	res.RejectedToolCalls = append(res.RejectedToolCalls, parsedText.RejectedToolCalls...)
	res.HasUnclosedToolCall = parsedText.HasUnclosedToolCall
	if len(parsedText.ToolCalls) > 0 || len(parsedText.RejectedToolCalls) > 0 {
		if parsedText.TextContent != "" {
			res.TextParts = []string{parsedText.TextContent}
		} else {
			res.TextParts = nil
		}
	}
	return res
}

// ParseGenerateContentChunk decodes a single streaming chunk.
func ParseGenerateContentChunk(chunk any, opts ToolParseOptions) ParsedResponse {
	res := ParsedResponse{}

	arr, ok := chunk.([]any)
	if !ok {
		return res
	}

	if text := extractTextFromGenerateChunk(arr); text != "" {
		if classifyGenerateChunk(arr) == "reasoning" {
			res.ReasoningParts = append(res.ReasoningParts, text)
		} else {
			res.TextParts = append(res.TextParts, text)
		}
	}

	extractFunctionCallsFromArray(arr, &res.FunctionCalls)
	extractImagesFromArray(arr, &res.Images)
	updateUsageFromGenerateChunk(arr, &res.Usage)
	native := normalizeAndValidateFunctionCalls(res.FunctionCalls, opts)
	res.FunctionCalls = native.calls
	res.RejectedToolCalls = native.rejected

	if len(res.TextParts) > 0 {
		parsedText := parseTextToolCalls(strings.Join(res.TextParts, ""), opts)
		if len(res.FunctionCalls) == 0 && len(parsedText.ToolCalls) > 0 {
			res.FunctionCalls = parsedText.ToolCalls
		}
		res.RejectedToolCalls = append(res.RejectedToolCalls, parsedText.RejectedToolCalls...)
		if parsedText.HasUnclosedToolCall {
			res.HasUnclosedToolCall = true
		}
		if len(parsedText.ToolCalls) > 0 || len(parsedText.RejectedToolCalls) > 0 {
			if parsedText.TextContent != "" {
				res.TextParts = []string{parsedText.TextContent}
			} else {
				res.TextParts = nil
			}
		}
	}

	if len(res.ReasoningParts) > 0 {
		parsedReasoning := parseTextToolCalls(strings.Join(res.ReasoningParts, ""), opts)
		if len(res.FunctionCalls) == 0 && len(parsedReasoning.ToolCalls) > 0 {
			res.FunctionCalls = parsedReasoning.ToolCalls
		}
		res.RejectedToolCalls = append(res.RejectedToolCalls, parsedReasoning.RejectedToolCalls...)
		if parsedReasoning.HasUnclosedToolCall {
			res.HasUnclosedToolCall = true
		}
		if len(parsedReasoning.ToolCalls) > 0 || len(parsedReasoning.RejectedToolCalls) > 0 {
			if parsedReasoning.TextContent != "" {
				res.ReasoningParts = []string{parsedReasoning.TextContent}
			} else {
				res.ReasoningParts = nil
			}
		}
	}

	return res
}

// updateUsageFromGenerateChunk decodes Gemini UsageMetadata from chunk[2].
// The array fields mirror the protobuf message: prompt, candidates, total,
// cached/details..., thoughts at index 9. OpenAI's completion_tokens includes
// reasoning tokens, so total-prompt is preferred over candidates when present.
func updateUsageFromGenerateChunk(chunk []any, usage *models.Usage) {
	if usage == nil || len(chunk) <= 2 {
		return
	}
	meta, ok := chunk[2].([]any)
	if !ok || len(meta) < 3 {
		return
	}
	prompt, promptOK := usageTokenCount(meta[0])
	candidates, candidatesOK := usageTokenCount(meta[1])
	total, totalOK := usageTokenCount(meta[2])
	if !totalOK || total <= 0 || (!promptOK && !candidatesOK) {
		return
	}
	completion := candidates
	if promptOK && total >= prompt {
		completion = total - prompt
	}
	if total >= usage.TotalTokens {
		usage.PromptTokens = prompt
		usage.CompletionTokens = completion
		usage.TotalTokens = total
	}
}

func usageTokenCount(value any) (int, bool) {
	n, ok := jsonx.AsNumber(value)
	if !ok {
		return 0, false
	}
	v, err := n.Int64()
	if err != nil || v < 0 {
		return 0, false
	}
	return int(v), true
}

// extractFromArray walks a generic AI Studio tree looking for text leaves and
// function calls.
func extractFromArray(arr []any, res *ParsedResponse) {
	for _, item := range arr {
		switch v := item.(type) {
		case []any:
			probeNode(v, res)
		case string:
			// Top-level strings other than role markers are text.
			if v != "user" && v != "model" && v != "" {
				res.TextParts = append(res.TextParts, v)
			}
		}
	}
}

func probeNode(arr []any, res *ParsedResponse) {
	if len(arr) == 2 {
		if arr[0] == nil {
			if s, ok := arr[1].(string); ok {
				if s != "user" && s != "model" && s != "" {
					res.TextParts = append(res.TextParts, s)
				}
				return
			}
		}
		if name, ok := arr[0].(string); ok && name != "user" && name != "model" {
			if _, ok := arr[1].(map[string]any); ok {
				res.FunctionCalls = append(res.FunctionCalls, FunctionCall{
					Name:      name,
					Arguments: map[string]any{},
				})
				return
			}
			if _, ok := arr[1].([]any); ok {
				res.FunctionCalls = append(res.FunctionCalls, FunctionCall{
					Name:      name,
					Arguments: map[string]any{},
				})
				return
			}
		}
	}

	if c := tryParseNativeFunctionCallEnvelope(arr); c != nil {
		res.FunctionCalls = append(res.FunctionCalls, *c)
		return
	}
	if c := tryParseNativeFunctionCall(arr); c != nil {
		res.FunctionCalls = append(res.FunctionCalls, *c)
		return
	}

	for _, item := range arr {
		if nested, ok := item.([]any); ok {
			probeNode(nested, res)
		}
	}
}

// tryParseNativeFunctionCall decodes a [name, argumentsNode] tuple.
func tryParseNativeFunctionCall(arr []any) *FunctionCall {
	if len(arr) < 2 {
		return nil
	}
	name, ok := arr[0].(string)
	if !ok || name == "user" || name == "model" || name == "" {
		return nil
	}
	argsNode, ok := arr[1].([]any)
	if !ok {
		return nil
	}
	decoded := decodeNativeFunctionArguments(argsNode)
	if decoded == nil {
		return nil
	}
	call := &FunctionCall{
		Name:      name,
		Arguments: map[string]any{},
	}
	if obj, ok := decoded.(map[string]any); ok {
		call.Arguments = obj
	} else {
		call.Arguments = map[string]any{"value": decoded}
	}
	if id, ok := arr[2].(string); ok {
		call.ID = id
	}
	if raw, err := jsonx.Marshal(argsNode); err == nil {
		call.AistudioNativeArgumentsPayload = raw
	}
	return call
}

// tryParseNativeFunctionCallEnvelope handles the [.., .., .., ..., tuple at 10, .., token at 14] envelope.
func tryParseNativeFunctionCallEnvelope(arr []any) *FunctionCall {
	if len(arr) <= 10 {
		return nil
	}
	inner, ok := arr[10].([]any)
	if !ok {
		return nil
	}
	parsed := tryParseNativeFunctionCall(inner)
	if parsed == nil {
		return nil
	}
	if len(arr) > 14 {
		if token, ok := arr[14].(string); ok && token != "" {
			parsed.AistudioNativeToken = token
		}
	}
	return parsed
}

// decodeNativeFunctionArguments mirrors the recursive decoder of the original.
func decodeNativeFunctionArguments(node []any) any {
	if len(node) == 0 {
		return nil
	}

	// [key, value] -> object
	if len(node) == 2 {
		if key, ok := node[0].(string); ok {
			if nested, ok := node[1].([]any); ok {
				decoded := decodeNativeFunctionArguments(nested)
				if decoded == nil {
					return map[string]any{key: node[1]}
				}
				return map[string]any{key: decoded}
			}
			return map[string]any{key: node[1]}
		}
		if node[0] == nil {
			if n, ok := jsonx.AsNumber(node[1]); ok {
				if f, err := n.Float64(); err == nil {
					return f
				}
			}
		}
	}
	// [bool]
	if len(node) == 1 {
		if _, ok := node[0].(bool); ok {
			return node[0]
		}
		if nested, ok := node[0].([]any); ok {
			if len(nested) == 2 {
				if key, ok := nested[0].(string); ok {
					decoded := decodeNativeFunctionArguments(nested[1].([]any))
					if decoded == nil {
						return map[string]any{key: nested[1]}
					}
					return map[string]any{key: decoded}
				}
			}
			decoded := decodeNativeFunctionArguments(nested)
			if decoded != nil {
				return decoded
			}
			return nested
		}
		return node[0]
	}
	// [null, null, string]
	if len(node) >= 3 && node[0] == nil && node[1] == nil {
		return node[2]
	}
	// array form [null, null, null, null, null, [...]]
	if len(node) >= 6 && node[0] == nil && node[1] == nil && node[2] == nil && node[3] == nil && node[4] == nil {
		if items, ok := node[5].([]any); ok {
			out := make([]any, 0, len(items))
			for _, item := range items {
				if arr, ok := item.([]any); ok {
					decoded := decodeNativeFunctionArguments(arr)
					if decoded == nil {
						out = append(out, item)
					} else {
						out = append(out, decoded)
					}
				} else {
					out = append(out, item)
				}
			}
			return out
		}
	}

	// object entries
	obj := map[string]any{}
	hasEntries := false
	for _, item := range node {
		entry, ok := item.([]any)
		if !ok || len(entry) != 2 {
			continue
		}
		key, ok := entry[0].(string)
		if !ok {
			continue
		}
		if nested, ok := entry[1].([]any); ok {
			decoded := decodeNativeFunctionArguments(nested)
			if decoded == nil {
				obj[key] = entry[1]
			} else {
				obj[key] = decoded
			}
		} else {
			obj[key] = entry[1]
		}
		hasEntries = true
	}
	if hasEntries {
		return obj
	}
	return nil
}

// extractFunctionCallsFromArray scans a chunk for function calls.
func extractFunctionCallsFromArray(arr []any, out *[]FunctionCall) {
	walkForCalls(arr, out)
}

func walkForCalls(arr []any, out *[]FunctionCall) {
	if len(arr) == 2 {
		if name, ok := arr[0].(string); ok && name != "user" && name != "model" {
			if _, ok := arr[1].(map[string]any); ok {
				*out = append(*out, FunctionCall{Name: name, Arguments: arr[1].(map[string]any)})
				return
			}
		}
	}
	if c := tryParseNativeFunctionCallEnvelope(arr); c != nil {
		*out = append(*out, *c)
		return
	}
	if c := tryParseNativeFunctionCall(arr); c != nil {
		*out = append(*out, *c)
		return
	}
	for _, item := range arr {
		if nested, ok := item.([]any); ok {
			walkForCalls(nested, out)
		}
	}
}

func extractImagesFromArray(arr []any, out *[]ParsedImage) {
	walkForImages(arr, out)
}

func walkForImages(arr []any, out *[]ParsedImage) {
	if img := tryParseInlineImage(arr); img != nil {
		*out = append(*out, *img)
		return
	}
	for _, item := range arr {
		if nested, ok := item.([]any); ok {
			walkForImages(nested, out)
		}
	}
}

func tryParseInlineImage(arr []any) *ParsedImage {
	if len(arr) < 3 || arr[0] != nil || arr[1] != nil {
		return nil
	}
	payload, ok := arr[2].([]any)
	if !ok || len(payload) < 2 {
		return nil
	}
	mimeType, ok1 := payload[0].(string)
	data, ok2 := payload[1].(string)
	if !ok1 || !ok2 || mimeType == "" || data == "" {
		return nil
	}
	if !strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		return nil
	}
	return &ParsedImage{MimeType: mimeType, Data: data}
}

// --- chunk structure helpers ---

func unwrapGenerateContentChunks(node []any) [][]any {
	if len(node) == 0 {
		return nil
	}
	if len(node) == 1 {
		if inner, ok := node[0].([]any); ok {
			chunks := filterChunks(inner)
			if len(chunks) > 0 {
				return chunks
			}
		}
	}
	chunks := filterChunks(node)
	if len(chunks) > 0 {
		return chunks
	}
	return nil
}

func filterChunks(node []any) [][]any {
	out := make([][]any, 0)
	for _, item := range node {
		if isGenerateContentChunk(item) {
			out = append(out, item.([]any))
		}
	}
	return out
}

func isGenerateContentChunk(node any) bool {
	arr, ok := node.([]any)
	if !ok {
		return false
	}
	if len(arr) < 3 {
		return false
	}
	if _, ok := arr[0].([]any); !ok {
		return false
	}
	_, isArr := arr[2].([]any)
	return isArr || arr[2] == nil
}

func classifyGenerateChunk(chunk []any) string {
	meta, ok := chunk[2].([]any)
	if !ok || len(meta) <= 1 {
		return "content"
	}
	switch meta[1] {
	case nil:
		return "reasoning"
	case float64(1):
		return "content"
	}
	return "content"
}

func extractTextFromGenerateChunk(chunk []any) string {
	first, ok := chunk[0].([]any)
	if !ok {
		return ""
	}
	texts := make([]string, 0)
	collectTextLeaves(first, &texts)
	if len(texts) == 0 {
		return ""
	}
	return strings.Join(texts, "")
}

func collectTextLeaves(node []any, out *[]string) {
	if len(node) >= 2 && node[0] == nil {
		if s, ok := node[1].(string); ok {
			*out = append(*out, s)
			return
		}
	}
	for _, item := range node {
		if nested, ok := item.([]any); ok {
			collectTextLeaves(nested, out)
		}
	}
}

// --- validation/normalization ---

type validatedCalls struct {
	calls    []FunctionCall
	rejected []FunctionCall
}

func normalizeAndValidateFunctionCalls(calls []FunctionCall, opts ToolParseOptions) validatedCalls {
	out := validatedCalls{}
	if len(calls) == 0 {
		return out
	}
	for i := range calls {
		normalized := calls[i]
		if normalized.ID == "" {
			normalized.ID = toolCallID(i)
		}
		reason := validateToolCallName(normalized.Name, opts)
		if reason != "" {
			normalized.Reason = reason
			out.rejected = append(out.rejected, normalized)
		} else {
			out.calls = append(out.calls, normalized)
		}
	}
	return out
}

// toolCallID builds a stable-ish id for tool calls lacking one.
func toolCallID(offset int) string {
	return "call_" + offsetToString(offset)
}

func offsetToString(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	out := []byte{}
	for n > 0 {
		out = append([]byte{digits[n%10]}, out...)
		n /= 10
	}
	return string(out)
}

func validateToolCallName(name string, opts ToolParseOptions) string {
	toolChoice := string(opts.ToolChoice)
	if toolChoice == "none" {
		return "tool_choice forbids tool calls"
	}
	allowed := getAllowedToolNames(opts.Tools)
	if len(allowed) > 0 && !allowed[name] {
		return "tool \"" + name + "\" is not in request tools"
	}
	if forced := getForcedToolName(opts.ToolChoice); forced != "" && forced != name {
		return "tool_choice requires \"" + forced + "\""
	}
	return ""
}

func getAllowedToolNames(tools []models.Tool) map[string]bool {
	out := make(map[string]bool, len(tools))
	for _, t := range tools {
		out[t.Function.Name] = true
	}
	return out
}

func getForcedToolName(toolChoice json.RawMessage) string {
	if len(toolChoice) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(toolChoice, &s) == nil {
		switch s {
		case "", "auto", "required", "none":
			return ""
		}
		return s
	}
	var obj map[string]any
	if json.Unmarshal(toolChoice, &obj) == nil {
		if fn, ok := obj["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return name
			}
		}
		if name, ok := obj["name"].(string); ok {
			return name
		}
	}
	return ""
}

// import jsonx for Marshal used above
var _ = jsonx.Marshal

// toolProtocolRe matches markers that suggest the model emitted tool-call
// protocol text instead of plain prose.
var toolProtocolRe = regexp.MustCompile(`"tool_execution"|"tool_calls"|"function_call"` + "|" + "```json" + "|" + "~~~json")

// looksLikeToolProtocol returns true if the text contains tool-protocol markers.
func looksLikeToolProtocol(text string) bool {
	return toolProtocolRe.MatchString(text)
}
