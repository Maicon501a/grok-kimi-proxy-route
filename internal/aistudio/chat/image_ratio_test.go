package chat

import (
	"context"
	"encoding/json"
	"testing"

	"grok-desktop/internal/aistudio/auth"
	"grok-desktop/internal/aistudio/botguard"
	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/converter"
	"grok-desktop/internal/aistudio/models"
)

func TestBuildPayloadFromCacheAppliesImageAspectRatio(t *testing.T) {
	cfg := &config.Config{
		DefaultModel:       "models/gemini-3.1-pro-preview",
		MaxTokensDefault:   65536,
		TopPDefault:        1,
		TemperatureDefault: 0.95,
		TopKDefault:        64,
	}
	client := &Client{cfg: cfg}
	template := json.RawMessage(`[
		"models/gemini-3.1-pro-preview",
		[],
		null,
		[null,null,null,65536,1,0.95,64,null,null,null,null,null,null,1,["search"],null,[1,null,null,3]],
		null,
		null,
		["tool-slot"]
	]`)
	capture := &botguard.Capture{TemplatePayload: template}
	opts := GenerateOptions{
		RequestOptions: converter.RequestOptions{
			Model:            "models/gemini-2.5-flash-image",
			ImageAspectRatio: "16:9",
			Messages: []models.Message{
				{Role: "user", Content: mustRawJSON(t, `"gere uma imagem"`)}},
		},
	}

	payload, err := client.buildPayloadFromCache(context.Background(), opts, capture, &auth.RuntimeAuth{})
	if err != nil {
		t.Fatal(err)
	}

	configSlot, ok := payload[3].([]any)
	if !ok {
		t.Fatalf("expected config slot, got %#v", payload[3])
	}
	if len(configSlot) <= 26 {
		t.Fatalf("expected config slot length > 26, got %d", len(configSlot))
	}
	if payload[6] != nil {
		t.Fatalf("expected tool slot nil for image model, got %#v", payload[6])
	}
	if configSlot[13] != nil {
		t.Fatalf("expected search/tool config cleared, got %#v", configSlot[13])
	}
	if configSlot[16] != nil {
		t.Fatalf("expected thinking config cleared, got %#v", configSlot[16])
	}
	ratio, ok := configSlot[26].([]any)
	if !ok || len(ratio) != 1 || ratio[0] != "16:9" {
		t.Fatalf("expected aspect ratio slot [\"16:9\"], got %#v", configSlot[26])
	}
}

func TestExtractHashSourceIncludesInlineImagePayload(t *testing.T) {
	messagesField := []any{
		[]any{
			[]any{
				[]any{nil, "Descreva a imagem."},
				[]any{nil, nil, []any{"image/png", "AAA111"}},
			},
			"user",
		},
	}

	hashSource := extractHashSource(messagesField)
	if hashSource != "Descreva a imagem. AAA111" {
		t.Fatalf("expected browser-compatible hash source, got %q", hashSource)
	}
}

func TestExtractHashSourcePreservesEmptyNativePartSeparators(t *testing.T) {
	toolCall := make([]any, 15)
	toolCall[10] = []any{"get_current_temperature", []any{}, "call_1"}
	toolResult := make([]any, 12)
	toolResult[11] = []any{"get_current_temperature", []any{}, "call_1"}

	messagesField := []any{
		[]any{[]any{[]any{nil, "Check London."}}, "user"},
		[]any{[]any{toolCall}, "model"},
		[]any{[]any{toolResult}, "user"},
	}

	if got := extractHashSource(messagesField); got != "Check London.  " {
		t.Fatalf("expected native parts to contribute empty hash entries, got %q", got)
	}
}
