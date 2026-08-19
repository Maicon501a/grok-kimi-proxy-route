package converter

// This file converts the decoded ParsedResponse into OpenAI-compatible
// response objects and streaming chunks.

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/models"
)

// ToOpenAIResponse builds a non-streaming chat completion response.
func ToOpenAIResponse(parsed ParsedResponse, modelName string, cfg *config.Config) models.ChatResponse {
	if modelName == "" {
		modelName = cfg.DefaultModel
	}

	message := models.ResponseMessage{Role: "assistant"}
	reasoningText := joinNonEmpty(parsed.ReasoningParts)

	toolCalls := BuildOpenAIToolCalls(parsed.FunctionCalls, "call_"+strconv.FormatInt(time.Now().UnixMilli(), 10))
	if len(parsed.FunctionCalls) > 0 {
		// When there are tool calls, visible content is emptied to avoid leaking
		// internal protocol text to the client.
		empty := ""
		message.Content = &empty
		message.ToolCalls = toolCalls
	} else if len(parsed.TextParts) > 0 {
		content := joinNonEmpty(parsed.TextParts)
		message.Content = &content
	}

	if reasoningText != "" {
		message.ReasoningContent = reasoningText
	}

	if message.Content == nil && len(toolCalls) == 0 {
		empty := ""
		message.Content = &empty
	}

	finishReason := "stop"
	if len(parsed.FunctionCalls) > 0 {
		finishReason = "tool_calls"
	}

	return models.ChatResponse{
		ID:      "chatcmpl-" + strconv.FormatInt(time.Now().UnixMilli(), 10),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   modelName,
		Choices: []models.ChatChoice{{
			Index:        0,
			Message:      message,
			FinishReason: finishReason,
		}},
		Usage: parsed.Usage,
	}
}

// ToOpenAIStreamChunk builds a single streaming chunk from a parsed slice.
func ToOpenAIStreamChunk(parsed ParsedResponse, modelName string, cfg *config.Config, chunkIndex int) models.StreamChunk {
	resp := ToOpenAIResponse(parsed, modelName, cfg)
	msg := resp.Choices[0].Message

	chunk := models.StreamChunk{
		ID:      resp.ID,
		Object:  "chat.completion.chunk",
		Created: resp.Created,
		Model:   resp.Model,
		Choices: []models.StreamChoice{{
			Index: 0,
			Delta: models.Delta{},
		}},
	}

	if msg.Content != nil && *msg.Content != "" {
		chunk.Choices[0].Delta.Content = *msg.Content
	}
	if msg.ReasoningContent != "" {
		chunk.Choices[0].Delta.ReasoningContent = msg.ReasoningContent
	}
	if len(msg.ToolCalls) > 0 {
		chunk.Choices[0].Delta.ToolCalls = BuildOpenAIStreamToolCallDeltas(msg.ToolCalls)
	}

	if chunkIndex == -1 {
		finish := resp.Choices[0].FinishReason
		chunk.Choices[0].FinishReason = &finish
	}

	return chunk
}

// BuildOpenAIToolCalls converts parsed function calls into the non-streaming
// OpenAI tool_calls shape.
func BuildOpenAIToolCalls(calls []FunctionCall, fallbackIDPrefix string) []models.ToolCall {
	toolCalls := make([]models.ToolCall, 0, len(calls))
	for i, fc := range calls {
		callID := fc.ID
		if callID == "" {
			callID = fallbackIDPrefix + "_" + strconv.Itoa(i)
		}
		argsBytes, _ := json.Marshal(fc.Arguments)
		toolCalls = append(toolCalls, models.ToolCall{
			ID:   callID,
			Type: "function",
			Function: models.FunctionCall{
				Name:      fc.Name,
				Arguments: string(argsBytes),
			},
			AistudioNativeToken:            fc.AistudioNativeToken,
			AistudioNativeArgumentsPayload: fc.AistudioNativeArgumentsPayload,
		})
	}
	return toolCalls
}

// BuildOpenAIStreamToolCallDeltas converts function calls into the streaming
// delta.tool_calls shape, including the required index field.
func BuildOpenAIStreamToolCallDeltas(toolCalls []models.ToolCall) []models.StreamToolCallDelta {
	deltas := make([]models.StreamToolCallDelta, 0, len(toolCalls))
	for i, tc := range toolCalls {
		deltas = append(deltas, models.StreamToolCallDelta{
			Index: i,
			ID:    tc.ID,
			Type:  tc.Type,
			Function: &models.StreamFunctionDelta{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}
	return deltas
}

// ToOpenAIImageResponse converts parsed inline images into an OpenAI-style
// /v1/images/generations response using b64_json payloads.
func ToOpenAIImageResponse(parsed ParsedResponse) models.ImageGenerationResponse {
	data := make([]models.GeneratedImage, 0, len(parsed.Images))
	for _, img := range parsed.Images {
		data = append(data, models.GeneratedImage{B64JSON: img.Data})
	}

	return models.ImageGenerationResponse{
		Created: time.Now().Unix(),
		Data:    data,
	}
}

// ExtractFunctionCallErrorText finds a "Malformed function call" diagnostic in raw text.
func ExtractFunctionCallErrorText(rawText string) string {
	idx := indexOfCaseInsensitive(rawText, "Malformed function call:")
	if idx < 0 {
		return ""
	}
	tail := rawText[idx:]
	end := len(tail)
	for i, r := range tail {
		if r == '"' || r == ']' {
			end = i
			break
		}
	}
	return tail[:end]
}

func indexOfCaseInsensitive(s, substr string) int {
	return indexOf(strings.ToLower(s), strings.ToLower(substr))
}

func indexOf(s, substr string) int {
	return strings.Index(s, substr)
}

func joinNonEmpty(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "")
}
