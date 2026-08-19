package chat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"grok-desktop/internal/aistudio/auth"
	"grok-desktop/internal/aistudio/botguard"
	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/converter"
	"grok-desktop/internal/aistudio/models"
)

func TestDebugWritePayloadSample(t *testing.T) {
	if os.Getenv("GO_DEBUG_WRITE_PAYLOAD_SAMPLE") != "1" {
		t.Skip("debug helper")
	}

	capturePath := os.Getenv("AISTUDIO_DEBUG_CAPTURE")
	if capturePath == "" {
		t.Skip("set AISTUDIO_DEBUG_CAPTURE to a captured bootstrap payload")
	}
	raw, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}

	var capture struct {
		TemplatePayload json.RawMessage `json:"templatePayload"`
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		DefaultModel:       "models/gemini-3.1-pro-preview",
		MaxTokensDefault:   65536,
		TopPDefault:        1,
		TemperatureDefault: 0.95,
		TopKDefault:        64,
		GenerateTimeout:    120000,
	}

	client := &Client{cfg: cfg}
	bgCapture := &botguard.Capture{TemplatePayload: capture.TemplatePayload}
	opts := GenerateOptions{
		RequestOptions: converter.RequestOptions{
			Model: "models/gemini-3.5-flash",
			Messages: []models.Message{
				{Role: "user", Content: mustRawJSON(t, `"Responda chamando exatamente uma ferramenta. Use tool_1 com {\"value\":\"ok\"}."`)},
			},
			Tools:           makeSampleTools(135),
			ToolChoice:      mustRawJSON(t, `"required"`),
			ToolCallingMode: "bridge_first",
		},
	}

	payload, err := client.buildPayloadFromCache(context.Background(), opts, bgCapture, &auth.RuntimeAuth{})
	if err != nil {
		t.Fatal(err)
	}

	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(t.TempDir(), "go-payload-sample.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func makeSampleTools(count int) []models.Tool {
	tools := make([]models.Tool, 0, count)
	for i := 1; i <= count; i++ {
		tools = append(tools, makeSampleTool(i))
	}
	return tools
}

func makeSampleTool(index int) models.Tool {
	return models.Tool{
		Type: "function",
		Function: models.FunctionSchema{
			Name:        "tool_" + itoa(index),
			Description: "Ferramenta " + itoa(index),
			Parameters:  json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`),
		},
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

func mustRawJSON(t *testing.T, raw string) json.RawMessage {
	t.Helper()
	return json.RawMessage(raw)
}
