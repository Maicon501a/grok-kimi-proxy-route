package httpserver

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"grok-desktop/internal/aistudio/converter"
	"grok-desktop/internal/aistudio/models"
)

// chunkText builds an AI Studio GenerateContent chunk array carrying text.
// Real shape: [contentParts, nil, meta] where meta[1]==1 marks content.
func chunkText(text string) []any {
	return []any{
		[]any{[]any{nil, text}},
		nil,
		[]any{nil, float64(1)},
	}
}

// chunkReasoning builds a reasoning chunk (meta[1]==nil).
func chunkReasoning(text string) []any {
	return []any{
		[]any{[]any{nil, text}},
		nil,
		[]any{nil, nil},
	}
}

func collectSSE(t *testing.T, e *liveEmitter) string {
	t.Helper()
	e.stop()
	// force flush of any held text
	return ""
}

func TestLiveEmitterStreamsTextIncrementally(t *testing.T) {
	rec := httptest.NewRecorder()
	toolOpts := converter.ToolParseOptions{}
	e := newLiveEmitter(newResponseFlusher(rec), "req1", "models/test", nil, toolOpts, 0)

	// Two text chunks arrive separately.
	if err := e.onChunk(chunkText("Hello ")); err != nil {
		t.Fatal(err)
	}
	if err := e.onChunk(chunkText("world")); err != nil {
		t.Fatal(err)
	}

	e.finishStop()
	body := rec.Body.String()
	// Both text fragments must appear as separate content deltas.
	if !strings.Contains(body, `"content":"Hello "`) {
		t.Fatalf("esperava delta 'Hello ', body: %s", body)
	}
	if !strings.Contains(body, `"content":"world"`) {
		t.Fatalf("esperava delta 'world', body: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("esperava [DONE], body: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("esperava finish stop, body: %s", body)
	}
}

func TestLiveEmitterStreamsReasoningThenText(t *testing.T) {
	rec := httptest.NewRecorder()
	toolOpts := converter.ToolParseOptions{}
	e := newLiveEmitter(newResponseFlusher(rec), "req1", "models/test", nil, toolOpts, 0)

	if err := e.onChunk(chunkReasoning("thinking...")); err != nil {
		t.Fatal(err)
	}
	if err := e.onChunk(chunkText("answer")); err != nil {
		t.Fatal(err)
	}
	e.finishStop()
	body := rec.Body.String()
	if !strings.Contains(body, `"reasoning_content":"thinking..."`) {
		t.Fatalf("esperava reasoning delta, body: %s", body)
	}
	if !strings.Contains(body, `"content":"answer"`) {
		t.Fatalf("esperava content delta, body: %s", body)
	}
}

func TestLiveEmitterFenceDoesNotLeakProtocolText(t *testing.T) {
	rec := httptest.NewRecorder()
	toolOpts := converter.ToolParseOptions{}
	e := newLiveEmitter(newResponseFlusher(rec), "req1", "models/test", nil, toolOpts, 0)

	// Prose, then an opening fence across chunk boundary, then the JSON, then
	// close fence, then trailing prose.
	if err := e.onChunk(chunkText("Antes do tool.\n```")); err != nil {
		t.Fatal(err)
	}
	if err := e.onChunk(chunkText("json\n{\"tool_execution\":\"ferramenta\",\"arguments\":{\"x\":1}}\n```")); err != nil {
		t.Fatal(err)
	}
	if err := e.onChunk(chunkText("\nDepois do tool.")); err != nil {
		t.Fatal(err)
	}

	// Finish with a tool call recovered by the authoritative parse (simulated).
	e.finishWithToolCalls([]converter.FunctionCall{{Name: "ferramenta", Arguments: map[string]any{"x": float64(1)}}})
	body := rec.Body.String()

	// The fenced JSON must NOT appear as content.
	if strings.Contains(body, `"content":"json`) {
		t.Fatalf("vazou opener do fence como content: %s", body)
	}
	if strings.Contains(body, `"tool_execution"`) {
		t.Fatalf("vazou JSON do tool como content: %s", body)
	}
	// Prose before and after must be streamed.
	if !strings.Contains(body, "Antes do tool.") {
		t.Fatalf("prose antes do fence nao streamada: %s", body)
	}
	if !strings.Contains(body, "Depois do tool.") {
		t.Fatalf("prose depois do fence nao streamada: %s", body)
	}
	// Tool call must be emitted.
	if !strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("esperava tool_calls delta: %s", body)
	}
	if !strings.Contains(body, `"ferramenta"`) {
		t.Fatalf("esperava nome ferramenta no tool_call: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("esperava finish tool_calls: %s", body)
	}
}

func TestLiveEmitterKeepaliveWritesCommentFrames(t *testing.T) {
	rec := httptest.NewRecorder()
	toolOpts := converter.ToolParseOptions{}
	// Very short keepalive so the ping fires during the test.
	e := newLiveEmitter(newResponseFlusher(rec), "req1", "models/test", nil, toolOpts, 20*time.Millisecond)

	// Start the stream by sending a chunk, then wait long enough for keepalive.
	if err := e.onChunk(chunkText("start")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	e.finishStop()
	e.stop()

	body := rec.Body.String()
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("esperava frame de keepalive, body: %s", body)
	}
}

func TestLiveEmitterReconcilesTrailingText(t *testing.T) {
	rec := httptest.NewRecorder()
	toolOpts := converter.ToolParseOptions{}
	e := newLiveEmitter(newResponseFlusher(rec), "req1", "models/test", nil, toolOpts, 0)

	if err := e.onChunk(chunkText("abc")); err != nil {
		t.Fatal(err)
	}
	// Simulate the final authoritative parse having a longer text (tail tokens
	// that arrived after the last streamed chunk).
	e.reconcileFinalText("abcdef")
	e.finishStop()

	body := rec.Body.String()
	if !strings.Contains(body, `"content":"abc"`) {
		t.Fatalf("esperava delta 'abc': %s", body)
	}
	if !strings.Contains(body, `"content":"def"`) {
		t.Fatalf("esperava delta reconciled 'def': %s", body)
	}
}

func TestResponseFlusherFlushes(t *testing.T) {
	rec := httptest.NewRecorder()
	rf := newResponseFlusher(rec)
	rf.Header().Set("Content-Type", "text/event-stream")
	rf.WriteHeader(200)
	if _, err := rf.Write([]byte("data: x\n\n")); err != nil {
		t.Fatal(err)
	}
	rf.Flush()
	if rec.Body.String() != "data: x\n\n" {
		t.Fatalf("flush falhou: %q", rec.Body.String())
	}
}

// Smoke-check the delta wire shape for a content chunk.
func TestLiveEmitterDeltaShape(t *testing.T) {
	rec := httptest.NewRecorder()
	toolOpts := converter.ToolParseOptions{}
	e := newLiveEmitter(newResponseFlusher(rec), "req1", "models/test", nil, toolOpts, 0)
	if err := e.onChunk(chunkText("hi")); err != nil {
		t.Fatal(err)
	}
	e.finishStop()
	body := rec.Body.String()
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Fatalf("shape de chunk SSE invalido: %s", body)
	}
	if !strings.HasPrefix(strings.TrimSpace(body), "data: ") {
		t.Fatalf("frame SSE deve comecar com 'data: ': %s", body)
	}
	// ensure models import is used
	_ = models.StreamChunk{}
}

func TestLiveEmitterEmitsTerminalUsageFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	e := newLiveEmitter(newResponseFlusher(rec), "req-usage", "models/test", nil, converter.ToolParseOptions{}, 0)
	e.setUsage(models.Usage{PromptTokens: 101, CompletionTokens: 48, TotalTokens: 149})
	e.finishStop()
	body := rec.Body.String()
	if !strings.Contains(body, `"choices":[],"usage":{"prompt_tokens":101,"completion_tokens":48,"total_tokens":149}`) {
		t.Fatalf("terminal usage frame ausente: %s", body)
	}
}
