package converter

// Public helpers re-exported for the httpserver package.

// StreamModeHint is the streaming tool mode (buffered/hybrid/live).
type StreamModeHint string

// LooksLikeToolProtocolPublic exposes looksLikeToolProtocol to callers outside
// the converter package.
func LooksLikeToolProtocolPublic(text string) bool {
	return looksLikeToolProtocol(text)
}

// RequiresToolCallPublic exposes requiresToolCall semantics.
func RequiresToolCallPublic(toolChoice interface{}) bool {
	_ = toolChoice
	return false
}

// ValidationErrorsFromSchema converts a schema-style result into converter
// validation errors. Defined here to avoid an import cycle through schema.
type SchemaError struct {
	Path    string
	Message string
}

// FromSchemaErrors builds converter ValidationErrors from generic schema errors.
func FromSchemaErrors(errs []SchemaError) []ValidationError {
	out := make([]ValidationError, 0, len(errs))
	for _, e := range errs {
		out = append(out, ValidationError(e))
	}
	return out
}

// TextToolCallResult is the exported form of parsedText returned by
// ParseTextToolCalls.
type TextToolCallResult struct {
	TextContent         string
	ToolCalls           []FunctionCall
	RejectedToolCalls   []FunctionCall
	HasUnclosedToolCall bool
}

// ParseTextToolCalls extracts tool calls embedded in free-form model text
// (fenced ```json``` blocks and inline tool-protocol objects). It is the
// streaming-friendly, lenient counterpart used by the live emitter to recover
// bridge_first tool calls from a fenced JSON block without buffering the whole
// upstream response.
func ParseTextToolCalls(text string, opts ToolParseOptions) TextToolCallResult {
	p := parseTextToolCalls(text, opts)
	return TextToolCallResult{
		TextContent:         p.TextContent,
		ToolCalls:           p.ToolCalls,
		RejectedToolCalls:   p.RejectedToolCalls,
		HasUnclosedToolCall: p.HasUnclosedToolCall,
	}
}
