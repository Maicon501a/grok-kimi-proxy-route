// Package prompt implements the tool-bridge prompt injector: it instructs the
// model to emit tool calls as executable Markdown JSON blocks that the proxy
// converts into OpenAI tool_calls.
package prompt

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"grok-desktop/internal/aistudio/converter"
	"grok-desktop/internal/aistudio/jsonx"
	"grok-desktop/internal/aistudio/models"
)

// Injector builds system and user prompt additions that enforce the tool-call
// JSON contract.
type Injector struct {
	enabled bool
}

// New creates an Injector.
func New(enabled bool) *Injector {
	return &Injector{enabled: enabled}
}

// PreparedRequest holds the result of PrepareRequest.
type PreparedRequest struct {
	Messages          []models.Message
	SystemInstruction string
}

// PrepareRequest appends a tool-bridge system instruction and suffixes the last
// user message with the format requirement. Returns the original messages and
// an empty instruction when disabled or when there are no tools.
func (i *Injector) PrepareRequest(messages []models.Message, tools []models.Tool, toolChoice json.RawMessage) PreparedRequest {
	if !i.enabled || len(tools) == 0 {
		return PreparedRequest{Messages: messages}
	}

	enhanced := make([]models.Message, len(messages))
	copy(enhanced, messages)

	toolNames := make([]string, 0, len(tools))
	for _, t := range tools {
		name := t.Function.Name
		if name == "" {
			name = "unknown"
		}
		toolNames = append(toolNames, name)
	}

	system := i.buildSystemPrompt(tools, toolNames, toolChoice)
	suffix := i.buildUserSuffix(toolNames, toolChoice)

	// Append the suffix to the last user message whose content is a plain string.
	for idx := len(enhanced) - 1; idx >= 0; idx-- {
		if enhanced[idx].Role != "user" {
			continue
		}
		if s := plainString(enhanced[idx].Content); s != "" {
			merged := s + "\n\n" + suffix
			enhanced[idx].Content = json.RawMessage(`"` + jsonEscape(merged) + `"`)
			break
		}
	}

	return PreparedRequest{Messages: enhanced, SystemInstruction: system}
}

// InjectToolInstructions returns messages with the system prompt merged into the
// first system message (or prepended if none exists).
func (i *Injector) InjectToolInstructions(messages []models.Message, tools []models.Tool, toolChoice json.RawMessage) []models.Message {
	prepared := i.PrepareRequest(messages, tools, toolChoice)
	if prepared.SystemInstruction == "" {
		return prepared.Messages
	}

	enhanced := prepared.Messages
	systemIdx := -1
	for idx, m := range enhanced {
		if m.Role == "system" {
			systemIdx = idx
			break
		}
	}
	if systemIdx >= 0 {
		existing := plainString(enhanced[systemIdx].Content)
		merged := prepared.SystemInstruction + "\n\n" + existing
		enhanced[systemIdx].Content = json.RawMessage(`"` + jsonEscape(merged) + `"`)
	} else {
		merged := []models.Message{{Role: "system", Content: json.RawMessage(`"` + jsonEscape(prepared.SystemInstruction) + `"`)}}
		enhanced = append(merged, enhanced...)
	}
	return enhanced
}

// BuildRetryPrompt produces a corrective retry prompt used when the model
// emitted an invalid tool call.
func (i *Injector) BuildRetryPrompt(originalPrompt string, tools []models.Tool, toolChoice json.RawMessage) string {
	toolNames := make([]string, 0, len(tools))
	for _, t := range tools {
		name := t.Function.Name
		if name == "" {
			name = "unknown"
		}
		toolNames = append(toolNames, name)
	}
	choiceRule := retryChoiceRule(toolChoice, toolNames)
	return "[RETRY - PREVIOUS RESPONSE FORMAT WAS INCORRECT]\n\n" +
		"Your previous response did not contain a valid executable tool call. You MUST respond with ONLY one Markdown JSON block.\n\n" +
		"User request: " + originalPrompt + "\n\n" +
		"Required format:\n```json\n{\n  \"tool_execution\": \"<one_of: " + strings.Join(toolNames, ", ") + ">\",\n  \"arguments\": { \"param\": \"value\" }\n}\n```\n\n" +
		choiceRule + "\nDo NOT include any other text. ONLY the JSON block."
}

// ExtractToolCallsFromFreeText scans free-form text for JSON tool-call blocks.
func ExtractToolCallsFromFreeText(text string) []converter.FunctionCall {
	calls := []converter.FunctionCall{}
	fenceRe := regexp.MustCompile("(?s)```json\\s*\\n?(.*?)\\n?```")
	for _, m := range fenceRe.FindAllStringSubmatch(text, -1) {
		trimmed := strings.TrimSpace(m[1])
		v := jsonx.DecodeLenient([]byte(trimmed))
		obj, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := obj["tool_execution"].(string); name != "" {
			args, _ := obj["arguments"].(map[string]any)
			calls = append(calls, converter.FunctionCall{Name: name, Arguments: args})
		} else if name, _ := obj["name"].(string); name != "" {
			args, _ := obj["arguments"].(map[string]any)
			calls = append(calls, converter.FunctionCall{Name: name, Arguments: args})
		}
	}

	// Inline tool_execution objects without fences.
	inlineRe := regexp.MustCompile(`\{\s*"tool_execution"\s*:\s*"([^"]+)"\s*,\s*"arguments"\s*:\s*(\{[\s\S]*?\})\s*\}`)
	for _, m := range inlineRe.FindAllStringSubmatch(text, -1) {
		name := m[1]
		args := map[string]any{}
		if v := jsonx.DecodeLenient([]byte(m[2])); v != nil {
			if obj, ok := v.(map[string]any); ok {
				args = obj
			}
		}
		calls = append(calls, converter.FunctionCall{Name: name, Arguments: args})
	}
	return calls
}

func (i *Injector) buildSystemPrompt(tools []models.Tool, toolNames []string, toolChoice json.RawMessage) string {
	var toolDescs []string
	for _, t := range tools {
		params := "{}"
		if len(t.Function.Parameters) > 0 {
			pretty, err := json.MarshalIndent(json.RawMessage(t.Function.Parameters), "", "  ")
			if err == nil {
				params = string(pretty)
			}
		}
		desc := t.Function.Description
		if desc == "" {
			desc = "No description"
		}
		toolDescs = append(toolDescs, "- "+t.Function.Name+": "+desc+"\nSchema: "+params)
	}

	// Build examples from up to two tools.
	examples := make([]string, 0)
	for i := 0; i < len(tools) && i < 2; i++ {
		fn := tools[i].Function
		var props map[string]any
		if len(fn.Parameters) > 0 {
			_ = json.Unmarshal(fn.Parameters, &props)
		}
		exampleArgs := map[string]any{}
		if propsMap, ok := props["properties"].(map[string]any); ok {
			for k, pRaw := range propsMap {
				p, _ := pRaw.(map[string]any)
				pt, _ := p["type"].(string)
				switch pt {
				case "number", "integer":
					exampleArgs[k] = 0
				case "boolean":
					exampleArgs[k] = false
				default:
					exampleArgs[k] = "example_" + k
				}
			}
		}
		argsJSON, _ := json.MarshalIndent(exampleArgs, "", "  ")
		examples = append(examples, "```json\n{\n  \"tool_execution\": \""+fn.Name+"\",\n  \"arguments\": "+string(argsJSON)+"\n}\n```")
	}

	forced := forcedToolName(toolChoice)
	rules := []string{
		"1. If you need a tool, output ONLY one Markdown JSON block.",
		"2. Use this exact executable shape: {\"tool_execution\":\"TOOL_NAME\",\"arguments\":{...}}.",
		"3. No prose before or after the JSON block.",
		"4. Use only available tool names and schema-defined parameter names.",
		"5. Put paths, code, shell commands, and file contents inside JSON string values.",
	}
	choiceStr := toolChoiceMode(toolChoice)
	switch {
	case choiceStr == "none":
		rules = append(rules, "6. tool_choice is none; do not emit tool calls in this response.")
	case choiceStr == "required":
		rules = append(rules, "6. tool_choice is required; if the task needs action, emit one valid tool call.")
	case forced != "":
		rules = append(rules, "6. tool_choice requires only the tool \""+forced+"\".")
	}

	return "# OPENAI-COMPATIBLE TOOL BRIDGE\n" +
		"This backend converts Markdown JSON tool execution blocks into OpenAI tool_calls.\n" +
		"Do not output OpenAI assistant wrappers, XML, or provider-native tool syntax.\n\n" +
		"Available tools:\n" + joinList(toolNames, "- ") + "\n\n" +
		"Tool schemas:\n" + strings.Join(toolDescs, "\n\n") + "\n\n" +
		"Executable format:\n```json\n{\n  \"tool_execution\": \"TOOL_NAME\",\n  \"arguments\": {\n    \"ARG_NAME\": \"ARG_VALUE\"\n  }\n}\n```\n\n" +
		"Examples:\n" + strings.Join(examples, "\n\n") + "\n\n" +
		"Rules:\n" + strings.Join(rules, "\n")
}

func (i *Injector) buildUserSuffix(toolNames []string, toolChoice json.RawMessage) string {
	forced := forcedToolName(toolChoice)
	choiceStr := toolChoiceMode(toolChoice)
	var choiceRule string
	switch {
	case choiceStr == "none":
		choiceRule = "tool_choice=none; nao chame tools."
	case choiceStr == "required":
		choiceRule = "tool_choice=required; se precisar agir, voce deve emitir uma tool call valida."
	case forced != "":
		choiceRule = "tool_choice exige apenas " + forced + "."
	default:
		choiceRule = "se chamar tool, use apenas um bloco JSON executavel."
	}

	return "[FORMAT REQUIREMENT]\n" +
		"Se precisar chamar tool, responda APENAS com este bloco JSON executavel:\n" +
		"```json\n{\n  \"tool_execution\": \"<function_name>\",\n  \"arguments\": { \"param\": \"value\" }\n}\n```\n\n" +
		"Available tool names: " + strings.Join(toolNames, ", ") + "\n" +
		choiceRule + "\n" +
		"Nao escreva nenhum texto antes ou depois do bloco JSON."
}

func retryChoiceRule(toolChoice json.RawMessage, toolNames []string) string {
	forced := forcedToolName(toolChoice)
	choiceStr := toolChoiceMode(toolChoice)
	switch {
	case choiceStr == "none":
		return "tool_choice=none; nao emita tool calls."
	case choiceStr == "required":
		return "tool_choice=required; emita uma tool call valida."
	case forced != "":
		return "tool_choice exige a tool " + forced + "."
	default:
		return "use uma tool valida se a tarefa exigir acao."
	}
}

func forcedToolName(toolChoice json.RawMessage) string {
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

func toolChoiceMode(toolChoice json.RawMessage) string {
	if len(toolChoice) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(toolChoice, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(toolChoice))
}

func plainString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func joinList(items []string, prefix string) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, prefix+it)
	}
	return strings.Join(parts, "\n")
}

func jsonEscape(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return `""`
	}
	b := bytes.TrimSpace(buf.Bytes())
	if len(b) < 2 {
		return `""`
	}
	// json.Marshal returns a quoted string; strip surrounding quotes so callers
	// can embed it directly as a JSON string token.
	return string(b[1 : len(b)-1])
}
