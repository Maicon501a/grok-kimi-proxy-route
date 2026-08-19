package converter

import (
	"encoding/json"
	"testing"

	"grok-desktop/internal/aistudio/models"
)

func TestBridgeFirstKeepsAssistantToolHistoryAndToolResultInMakerSuite(t *testing.T) {
	messages := []models.Message{
		{Role: "system", Content: rawJSON(t, `"system rule"`)},
		{Role: "user", Content: rawJSON(t, `"olha o arquivo env só."`)},
		{
			Role: "assistant",
			ToolCalls: []models.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: models.FunctionCall{
					Name:      "glob",
					Arguments: `{"pattern":"**/*env*"}`,
				},
			}},
		},
		{
			Role:       "tool",
			Name:       "glob",
			ToolCallID: "call_1",
			Content:    rawJSON(t, `"C:\\test\\.env.example"`),
		},
	}

	normalized := NormalizeOpenAIMessages(messages, false)
	if len(normalized.Messages) != 3 {
		t.Fatalf("esperava 3 mensagens normalizadas, obtive %d", len(normalized.Messages))
	}

	if contentAsString(t, normalized.Messages[1].Content) == "" {
		t.Fatal("assistant tool history nao virou string simples em bridge_first")
	}
	if contentAsString(t, normalized.Messages[2].Content) == "" {
		t.Fatal("tool result nao virou string simples em bridge_first")
	}

	converted := MessagesToMakerSuite(normalized.Messages, "")
	if len(converted) != 3 {
		t.Fatalf("esperava 3 mensagens MakerSuite preservadas, obtive %d", len(converted))
	}
}

func TestNativeFirstStillKeepsStructuredToolReplay(t *testing.T) {
	messages := []models.Message{
		{Role: "user", Content: rawJSON(t, `"teste"`)},
		{
			Role: "assistant",
			ToolCalls: []models.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: models.FunctionCall{
					Name:      "glob",
					Arguments: `{"pattern":"**/*env*"}`,
				},
			}},
		},
		{
			Role:       "tool",
			ToolCallID: "call_1",
			Content:    rawJSON(t, `{"matches":[".env.example"]}`),
		},
	}

	normalized := NormalizeOpenAIMessages(messages, true)
	if len(normalized.Messages) != 3 {
		t.Fatalf("esperava 3 mensagens normalizadas, obtive %d", len(normalized.Messages))
	}

	assistantParts := contentAsParts(t, normalized.Messages[1].Content)
	if len(assistantParts) == 0 || assistantParts[0].Type != "native_tool_call" {
		t.Fatalf("assistant native tool call ausente: %#v", assistantParts)
	}

	toolParts := contentAsParts(t, normalized.Messages[2].Content)
	if len(toolParts) == 0 || toolParts[0].Type != "native_tool_result" {
		t.Fatalf("native tool result ausente: %#v", toolParts)
	}
	if toolParts[0].Name != "glob" {
		t.Fatalf("native tool result did not infer function name: %#v", toolParts[0])
	}
}

func rawJSON(t *testing.T, raw string) json.RawMessage {
	t.Helper()
	return json.RawMessage(raw)
}

func contentAsString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("conteudo nao e string JSON valida: %v", err)
	}
	return value
}

func contentAsParts(t *testing.T, raw json.RawMessage) []models.ContentPart {
	t.Helper()
	var parts []models.ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatalf("conteudo nao e array de partes: %v", err)
	}
	return parts
}
