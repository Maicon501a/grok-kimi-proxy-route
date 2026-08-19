package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"grok-desktop/internal/aistudio/chat"
	"grok-desktop/internal/aistudio/converter"
	"grok-desktop/internal/aistudio/models"
)

func TestEmitToolCallsEmitsOpenAIIndexedDelta(t *testing.T) {
	seen := map[string]bool{}
	var got models.Delta

	emitToolCalls(func(delta models.Delta, finishReason *string) {
		if finishReason != nil {
			t.Fatalf("finishReason inesperado: %v", *finishReason)
		}
		got = delta
	}, []converter.FunctionCall{{
		ID:   "call_123",
		Name: "buscar_clima",
		Arguments: map[string]any{
			"cidade": "Sao Paulo",
		},
		AistudioNativeToken:            "opaque-token",
		AistudioNativeArgumentsPayload: json.RawMessage(`[["cidade","Sao Paulo"]]`),
	}}, seen, "req_123")

	if !seen["call_123"] {
		t.Fatalf("emitToolCalls nao marcou tool_call id no mapa seen: %#v", seen)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("esperava 1 tool_call delta, obtive %d", len(got.ToolCalls))
	}
	tc := got.ToolCalls[0]
	if tc.Index != 0 {
		t.Fatalf("index inesperado: %d", tc.Index)
	}
	if tc.ID != "call_123" {
		t.Fatalf("id inesperado: %q", tc.ID)
	}
	if tc.Type != "function" {
		t.Fatalf("type inesperado: %q", tc.Type)
	}
	if tc.Function == nil || tc.Function.Name != "buscar_clima" {
		t.Fatalf("function delta inesperado: %#v", tc.Function)
	}

	wire, err := json.Marshal(models.StreamChunk{
		ID:      "chatcmpl-test",
		Object:  "chat.completion.chunk",
		Created: 0,
		Model:   "models/gemini-3.5-flash",
		Choices: []models.StreamChoice{{
			Index: 0,
			Delta: got,
		}},
	})
	if err != nil {
		t.Fatalf("falha ao serializar chunk SSE: %v", err)
	}
	wireText := string(wire)
	if !strings.Contains(wireText, `"tool_calls":[{"index":0,"id":"call_123","type":"function","function":{"name":"buscar_clima","arguments":"{\"cidade\":\"Sao Paulo\"}"}}]`) {
		t.Fatalf("shape JSON inesperado no handler SSE: %s", wireText)
	}
	if strings.Contains(wireText, "aistudio_native_token") {
		t.Fatalf("chunk SSE vazou aistudio_native_token: %s", wireText)
	}
	if strings.Contains(wireText, "aistudio_native_arguments_payload") {
		t.Fatalf("chunk SSE vazou aistudio_native_arguments_payload: %s", wireText)
	}
}

func TestShouldRetryToolResponsePropagatesValidationErrors(t *testing.T) {
	parsed := converter.ParsedResponse{
		FunctionCalls: []converter.FunctionCall{{
			Name: "buscar_clima",
			Arguments: map[string]any{
				"cidade": "Sao Paulo",
			},
		}},
	}
	tools := []models.Tool{{
		Type: "function",
		Function: models.FunctionSchema{
			Name: "buscar_clima",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{"cidade":{"type":"integer"}},
				"required":["cidade"],
				"additionalProperties":false
			}`),
		},
	}}

	if !shouldRetryToolResponse(&parsed, tools, converter.RequestOptions{}) {
		t.Fatal("esperava retry por erro de schema")
	}
	if len(parsed.ValidationErrors) == 0 {
		t.Fatal("esperava ValidationErrors propagados no parsed")
	}
	msg := toolResponseFailureMessage(parsed, converter.RequestOptions{})
	if !strings.Contains(msg, "cidade") {
		t.Fatalf("mensagem de erro inesperada: %q", msg)
	}
}

func TestEmptyParsedResponseReason(t *testing.T) {
	t.Run("empty body", func(t *testing.T) {
		got := emptyParsedResponseReason(&chat.GenerateContentResult{Status: 200, Body: ""}, converter.ParsedResponse{})
		if got == "" {
			t.Fatal("esperava erro para body vazio")
		}
	})
	t.Run("incomplete json", func(t *testing.T) {
		got := emptyParsedResponseReason(&chat.GenerateContentResult{Status: 200, Body: "[[null,\"hello\""}, converter.ParsedResponse{})
		if got == "" {
			t.Fatal("esperava erro para JSON incompleto")
		}
	})
	t.Run("has text", func(t *testing.T) {
		got := emptyParsedResponseReason(
			&chat.GenerateContentResult{Status: 200, Body: "[]"},
			converter.ParsedResponse{TextParts: []string{"ok"}},
		)
		if got != "" {
			t.Fatalf("nao esperava erro com texto: %q", got)
		}
	})
	t.Run("non-200 ignored", func(t *testing.T) {
		got := emptyParsedResponseReason(&chat.GenerateContentResult{Status: 500, Body: ""}, converter.ParsedResponse{})
		if got != "" {
			t.Fatalf("status nao-200 nao deve ser tratado como empty parse: %q", got)
		}
	})
}

func TestStatusRecorderFlushPromotesUnderlying(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapped := &statusRecorder{ResponseWriter: rec, status: 200}
	if _, ok := any(wrapped).(http.Flusher); !ok {
		t.Fatal("statusRecorder deveria implementar http.Flusher")
	}
	wrapped.Header().Set("Content-Type", "text/event-stream")
	wrapped.WriteHeader(http.StatusOK)
	if _, err := wrapped.Write([]byte("data: test\n\n")); err != nil {
		t.Fatal(err)
	}
	wrapped.Flush()
	if rec.Body.String() != "data: test\n\n" {
		t.Fatalf("body inesperado: %q", rec.Body.String())
	}
	if unwrapper, ok := any(wrapped).(interface{ Unwrap() http.ResponseWriter }); !ok {
		t.Fatal("statusRecorder deveria implementar Unwrap")
	} else if unwrapper.Unwrap() != rec {
		t.Fatal("Unwrap nao retornou o ResponseWriter subjacente")
	}
}
