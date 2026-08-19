// Package models defines the shared domain types used across the proxy:
// OpenAI-compatible request/response shapes, tool definitions, session and
// account metadata.
package models

import "encoding/json"

// Message represents an OpenAI chat message. Content can be a plain string or
// a slice of content parts (multimodal).
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
}

// ToolCall represents an assistant tool call in the OpenAI wire format.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`

	// Opaque replay fields preserved for AI Studio native tool calling.
	AistudioNativeToken            string          `json:"aistudio_native_token,omitempty"`
	AistudioNativeArgumentsPayload json.RawMessage `json:"aistudio_native_arguments_payload,omitempty"`
}

// FunctionCall is the function component of a tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool represents an OpenAI function tool definition.
type Tool struct {
	Type     string         `json:"type"`
	Function FunctionSchema `json:"function"`
}

// FunctionSchema describes a tool function.
type FunctionSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ContentPart represents a multimodal content part.
type ContentPart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ImageURL   *ImageURLPart   `json:"image_url,omitempty"`
	VideoURL   *MediaURLPart   `json:"video_url,omitempty"`
	AudioURL   *MediaURLPart   `json:"audio_url,omitempty"`
	URL        string          `json:"url,omitempty"`
	FileID     string          `json:"file_id,omitempty"`
	Data       string          `json:"data,omitempty"`
	MimeType   string          `json:"mime_type,omitempty"`
	Name       string          `json:"name,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`

	AistudioNativeToken            string          `json:"aistudio_native_token,omitempty"`
	AistudioNativeArgumentsPayload json.RawMessage `json:"aistudio_native_arguments_payload,omitempty"`
}

type ImageURLPart struct {
	URL string `json:"url"`
}

type MediaURLPart struct {
	URL string `json:"url"`
}

// ChatRequest is the OpenAI /v1/chat/completions request body.
type ChatRequest struct {
	Model          string          `json:"model,omitempty"`
	Messages       []Message       `json:"messages"`
	Tools          []Tool          `json:"tools,omitempty"`
	ToolChoice     json.RawMessage `json:"tool_choice,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	MaxTokens      *int            `json:"max_tokens,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	SafetySettings json.RawMessage `json:"safety_settings,omitempty"`

	SystemInstruction json.RawMessage `json:"system_instruction,omitempty"`
	ThinkingLevel     string          `json:"thinking_level,omitempty"`
	ToolCallingMode   string          `json:"tool_calling_mode,omitempty"`

	SessionID      string `json:"session_id,omitempty"`
	ProfileID      string `json:"profile_id,omitempty"`
	ChatGeneration int    `json:"chat_generation,omitempty"`
}

// ChatResponse is the OpenAI /v1/chat/completions non-streaming response.
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   Usage        `json:"usage"`
}

type ChatChoice struct {
	Index        int             `json:"index"`
	Message      ResponseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type ResponseMessage struct {
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamChunk is an OpenAI streaming chunk.
type StreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
}

type StreamChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type Delta struct {
	Content          string                `json:"content,omitempty"`
	ReasoningContent string                `json:"reasoning_content,omitempty"`
	ToolCalls        []StreamToolCallDelta `json:"tool_calls,omitempty"`
}

// StreamToolCallDelta is the OpenAI streaming shape for delta.tool_calls.
type StreamToolCallDelta struct {
	Index    int                  `json:"index"`
	ID       string               `json:"id,omitempty"`
	Type     string               `json:"type,omitempty"`
	Function *StreamFunctionDelta `json:"function,omitempty"`
}

// StreamFunctionDelta supports partial function fields in SSE tool call deltas.
type StreamFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// Session represents a persisted proxy session.
type Session struct {
	SessionID               string          `json:"session_id"`
	ProfileID               string          `json:"profile_id"`
	CreatedAt               string          `json:"created_at"`
	UpdatedAt               string          `json:"updated_at"`
	Messages                json.RawMessage `json:"messages,omitempty"`
	ConversationMode        string          `json:"conversation_mode,omitempty"`
	AistudioPromptID        string          `json:"aistudio_prompt_id,omitempty"`
	AistudioPromptURL       string          `json:"aistudio_prompt_url,omitempty"`
	AccountFingerprint      string          `json:"account_fingerprint,omitempty"`
	LastModel               string          `json:"last_model,omitempty"`
	SystemInstruction       string          `json:"system_instruction,omitempty"`
	ThinkingLevel           string          `json:"thinking_level,omitempty"`
	ToolCallingMode         string          `json:"tool_calling_mode,omitempty"`
	ClientSessionID         string          `json:"client_session_id,omitempty"`
	ProxyCodeMode           string          `json:"proxycode_mode,omitempty"`
	ProxyCodeChatGeneration int             `json:"proxycode_chat_generation,omitempty"`
	MigrationCount          int             `json:"migration_count,omitempty"`
	MigratedFromProfileID   string          `json:"migrated_from_profile_id,omitempty"`
	MigrationReason         string          `json:"migration_reason,omitempty"`
	MigratedAt              string          `json:"migrated_at,omitempty"`
}

// AccountState tracks per-account health and usage metrics.
type AccountState struct {
	ProfileID           string `json:"profile_id"`
	TotalRequests       int    `json:"total_requests"`
	RecentRequests      int    `json:"recent_requests"`
	Failures            int    `json:"failures"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	CooldownUntil       string `json:"cooldown_until,omitempty"`
	LastUsedAt          string `json:"last_used_at,omitempty"`
	LastFailureAt       string `json:"last_failure_at,omitempty"`
	LastError           string `json:"last_error,omitempty"`
	MigrationsIn        int    `json:"migrations_in"`
	MigrationsOut       int    `json:"migrations_out"`
}

// AccountSummary is the public view of an account returned by /v1/accounts.
type AccountSummary struct {
	ProfileID           string `json:"profile_id"`
	ActiveSessions      int    `json:"active_sessions"`
	TotalRequests       int    `json:"total_requests"`
	RecentRequests      int    `json:"recent_requests"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	CooldownUntil       string `json:"cooldown_until,omitempty"`
	Available           bool   `json:"available"`
	LastUsedAt          string `json:"last_used_at,omitempty"`
	LastError           string `json:"last_error,omitempty"`
}

// APIError is the standard OpenAI-compatible error envelope.
type APIError struct {
	Error APIErrorBody `json:"error"`
}

type APIErrorBody struct {
	Message string         `json:"message"`
	Type    string         `json:"type,omitempty"`
	Code    any            `json:"code,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// ChatContext carries request-scoped routing metadata through the pipeline.
type ChatContext struct {
	SessionID               string
	ClientSessionID         string
	ProxyCodeMode           string
	ProxyCodeChatGeneration int
	ProfileID               string
	ExplicitProfileLocked   bool
	MigrationTrail          []string
}

// ImageGenerationRequest is the OpenAI-style /v1/images/generations body.
type ImageGenerationRequest struct {
	Model          string `json:"model,omitempty"`
	Prompt         string `json:"prompt"`
	N              *int   `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	AspectRatio    string `json:"aspect_ratio,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	User           string `json:"user,omitempty"`
	ProfileID      string `json:"profile_id,omitempty"`
}

// ImageGenerationResponse is the OpenAI-style image generation response.
type ImageGenerationResponse struct {
	Created int64            `json:"created"`
	Data    []GeneratedImage `json:"data"`
}

// GeneratedImage carries either inline base64 or a data URL.
type GeneratedImage struct {
	B64JSON string `json:"b64_json,omitempty"`
	URL     string `json:"url,omitempty"`
}
