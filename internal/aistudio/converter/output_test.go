package converter

import (
	"encoding/json"
	"strings"
	"testing"

	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/models"
)

func TestToOpenAIResponsePreservesUsage(t *testing.T) {
	cfg := &config.Config{DefaultModel: "models/gemini-3.7-flash"}
	parsed := ParsedResponse{
		TextParts: []string{"ok"},
		Usage: models.Usage{
			PromptTokens:     101,
			CompletionTokens: 48,
			TotalTokens:      149,
		},
	}
	resp := ToOpenAIResponse(parsed, "", cfg)
	if resp.Usage != parsed.Usage {
		t.Fatalf("usage nao foi preservado: got=%+v want=%+v", resp.Usage, parsed.Usage)
	}
}

func TestToOpenAIStreamChunkUsesIndexedToolCallDeltas(t *testing.T) {
	cfg := &config.Config{DefaultModel: "models/gemini-3.5-flash"}
	parsed := ParsedResponse{
		FunctionCalls: []FunctionCall{{
			ID:   "call_123",
			Name: "buscar_clima",
			Arguments: map[string]any{
				"cidade": "Sao Paulo",
			},
		}},
	}

	chunk := ToOpenAIStreamChunk(parsed, "", cfg, 0)
	if len(chunk.Choices) != 1 {
		t.Fatalf("esperava 1 choice, obtive %d", len(chunk.Choices))
	}
	if len(chunk.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("esperava 1 tool_call delta, obtive %d", len(chunk.Choices[0].Delta.ToolCalls))
	}

	tc := chunk.Choices[0].Delta.ToolCalls[0]
	if tc.Index != 0 {
		t.Fatalf("index inesperado: %d", tc.Index)
	}
	if tc.ID != "call_123" {
		t.Fatalf("id inesperado: %q", tc.ID)
	}
	if tc.Type != "function" {
		t.Fatalf("type inesperado: %q", tc.Type)
	}
	if tc.Function == nil {
		t.Fatal("function delta ausente")
	}
	if tc.Function.Name != "buscar_clima" {
		t.Fatalf("nome inesperado: %q", tc.Function.Name)
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments nao sao JSON valido: %v", err)
	}
	if args["cidade"] != "Sao Paulo" {
		t.Fatalf("arguments inesperados: %#v", args)
	}

	wire, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("falha ao serializar chunk: %v", err)
	}
	wireText := string(wire)
	if !strings.Contains(wireText, `"tool_calls":[{"index":0,"id":"call_123","type":"function","function":{"name":"buscar_clima","arguments":"{\"cidade\":\"Sao Paulo\"}"}}]`) {
		t.Fatalf("shape JSON inesperado: %s", wireText)
	}
	if strings.Contains(wireText, "aistudio_native_token") {
		t.Fatalf("chunk SSE vazou aistudio_native_token: %s", wireText)
	}
	if strings.Contains(wireText, "aistudio_native_arguments_payload") {
		t.Fatalf("chunk SSE vazou aistudio_native_arguments_payload: %s", wireText)
	}
}
