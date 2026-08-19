package httpserver

// live_stream.go implements the real-time SSE emitter for AI Studio
// GenerateContent streaming.
//
// Unlike the legacy buffered path (which waited for the entire upstream body
// before emitting a single SSE delta), this emitter is driven chunk-by-chunk
// by chat.Client.OnStreamChunk. It converts each decoded AI Studio chunk into
// OpenAI streaming deltas as soon as it arrives, with three delivery rules:
//
//  1. Text content (prose): streamed incrementally. Only the newly-arrived
//     suffix is emitted, so the client sees tokens appear in real time.
//  2. Reasoning content: streamed incrementally as delta.reasoning_content,
//     ahead of/interleaved with the final prose.
//  3. Tool calls:
//     - Native function-call chunks (native_first): emitted atomically as a
//       single delta.tool_calls once the chunk is complete and validated.
//     - Bridge tool calls (bridge_first, the compatibility mode): the model emits the
//       call as a fenced ```json``` block inside the text stream. The emitter
//       detects the opening fence, buffers ONLY the fenced block (not the
//       whole response), and when the fence closes it parses + validates the
//       tool call and emits it atomically as delta.tool_calls. Prose around
//       the fence continues to stream normally.
//
// A keepalive goroutine writes SSE comment frames (":\n\n") during upstream
// silence so long-thinking generations (gemini-3.1-pro) do not trip client
// idle timeouts (the root cause of OpenCode "freezing then closing the stream").

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"grok-desktop/internal/aistudio/converter"
	"grok-desktop/internal/aistudio/models"
	"grok-desktop/internal/aistudio/schema"
)

// liveEmitter converts AI Studio chunks into OpenAI SSE deltas in real time.
type liveEmitter struct {
	mu            sync.Mutex
	w             httpFlushWriter
	requestID     string
	model         string
	toolOpts      converter.ToolParseOptions
	originalTools []models.Tool
	toolSpecs     []schema.ToolSpec
	keepalive     time.Duration

	started        bool
	payloadStarted bool
	writeErr       error
	sseChunks      int
	wroteDone      bool

	emittedTextChars      int
	emittedReasoningChars int
	streamedToolIDs       map[string]bool
	finishReason          string // set on finish; "tool_calls" or "stop"
	sawNativeToolCall     bool   // a native function-call chunk was seen
	sawFenceTool          bool   // a fenced ```json``` tool block was seen
	usage                 models.Usage

	// Bridge fence state (persists across chunks because a fence can span
	// chunk boundaries).
	inFence     bool
	fenceMarker string // "```" or "~~~"
	fenceRunLen int
	blockBuf    strings.Builder
	holdTail    string // tail bytes that might be the start of a fence opener

	keepaliveStop chan struct{}
	keepaliveLast time.Time
}

// httpFlushWriter bundles an http.ResponseWriter with its Flusher so the
// emitter can push bytes to the client immediately after each delta.
type httpFlushWriter interface {
	http.ResponseWriter
	Flush()
}

// responseFlusher adapts any http.ResponseWriter into an httpFlushWriter by
// driving Flush through http.ResponseController, which transparently unwraps
// middleware (e.g. statusRecorder) to reach the real Flusher.
type responseFlusher struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func newResponseFlusher(w http.ResponseWriter) *responseFlusher {
	return &responseFlusher{ResponseWriter: w, rc: http.NewResponseController(w)}
}

func (r *responseFlusher) Flush() {
	_ = r.rc.Flush()
}

// newLiveEmitter constructs an emitter. keepalive <= 0 disables comment pings.
func newLiveEmitter(w httpFlushWriter, requestID, model string, tools []models.Tool, toolOpts converter.ToolParseOptions, keepalive time.Duration) *liveEmitter {
	e := &liveEmitter{
		w:               w,
		requestID:       requestID,
		model:           model,
		toolOpts:        toolOpts,
		originalTools:   tools,
		toolSpecs:       toToolSpecs(tools),
		streamedToolIDs: map[string]bool{},
		keepalive:       keepalive,
	}
	if keepalive > 0 {
		e.keepaliveStop = make(chan struct{})
		go e.keepaliveLoop()
	}
	return e
}

func (e *liveEmitter) keepaliveLoop() {
	ticker := time.NewTicker(e.keepalive / 2)
	defer ticker.Stop()
	for {
		select {
		case <-e.keepaliveStop:
			return
		case <-ticker.C:
			e.mu.Lock()
			if e.writeErr != nil || e.wroteDone {
				e.mu.Unlock()
				return
			}
			// Only ping if we've been silent for roughly one interval.
			if time.Since(e.keepaliveLast) >= e.keepalive {
				e.writeRawLocked([]byte(": keepalive\n\n"))
			}
			e.mu.Unlock()
		}
	}
}

// ensureHeaders writes the SSE response headers exactly once.
func (e *liveEmitter) ensureHeadersLocked() {
	if e.started {
		return
	}
	h := e.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	e.w.WriteHeader(200)
	e.started = true
	e.keepaliveLast = time.Now()
}

// writeRawLocked writes raw bytes and flushes. Caller must hold e.mu.
func (e *liveEmitter) writeRawLocked(p []byte) {
	if e.writeErr != nil {
		return
	}
	e.ensureHeadersLocked()
	if _, err := e.w.Write(p); err != nil {
		e.writeErr = err
		return
	}
	e.w.Flush()
	e.keepaliveLast = time.Now()
}

// emitDelta encodes and writes a single SSE data frame.
func (e *liveEmitter) emitDelta(delta models.Delta, finish *string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.writeErr != nil {
		return
	}
	chunk := models.StreamChunk{
		ID: "chatcmpl-" + e.requestID, Object: "chat.completion.chunk",
		Created: time.Now().Unix(), Model: e.model,
		Choices: []models.StreamChoice{{Index: 0, Delta: delta, FinishReason: finish}},
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		e.writeErr = err
		return
	}
	var buf strings.Builder
	buf.WriteString("data: ")
	buf.Write(encoded)
	buf.WriteString("\n\n")
	e.payloadStarted = true
	e.writeRawLocked([]byte(buf.String()))
	e.sseChunks++
}

// emitUsage writes the OpenAI-compatible terminal usage frame. OpenAI clients
// expect an empty choices array for this frame when stream usage is included.
func (e *liveEmitter) emitUsage() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.writeErr != nil || e.usage.TotalTokens <= 0 {
		return
	}
	usage := e.usage
	chunk := models.StreamChunk{
		ID: "chatcmpl-" + e.requestID, Object: "chat.completion.chunk",
		Created: time.Now().Unix(), Model: e.model,
		Choices: []models.StreamChoice{}, Usage: &usage,
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		e.writeErr = err
		return
	}
	e.payloadStarted = true
	e.writeRawLocked([]byte("data: " + string(encoded) + "\n\n"))
	e.sseChunks++
}

func (e *liveEmitter) setUsage(usage models.Usage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.usage = usage
}

// emitDone writes the terminal [DONE] frame.
func (e *liveEmitter) emitDone() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.writeErr != nil || e.wroteDone {
		return
	}
	e.writeRawLocked([]byte("data: [DONE]\n\n"))
	e.wroteDone = true
}

func (e *liveEmitter) hasPayload() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.payloadStarted
}

func (e *liveEmitter) emitError(message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.writeErr != nil || e.wroteDone {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": "upstream_error"},
	})
	e.payloadStarted = true
	e.writeRawLocked([]byte("data: " + string(payload) + "\n\n"))
	e.writeRawLocked([]byte("data: [DONE]\n\n"))
	e.wroteDone = true
}

// stop terminates the keepalive goroutine.
func (e *liveEmitter) stop() {
	if e.keepaliveStop != nil {
		close(e.keepaliveStop)
	}
}

// onChunk is the callback wired into chat.GenerateOptions.OnStreamChunk.
// It receives one complete AI Studio GenerateContent chunk and converts it to
// SSE deltas immediately. Text and reasoning are streamed incrementally; tool
// calls (native or bridge) are NOT emitted here — the handler emits them
// atomically from the final authoritative parse so they are complete, valid
// and never leak partial protocol text to the client.
func (e *liveEmitter) onChunk(chunk []any) error {
	if e.writeErr != nil {
		return e.writeErr
	}
	parsed := converter.ParseGenerateContentChunk(chunk, e.toolOpts)

	// 1. Reasoning first (it precedes the final answer).
	if r := strings.Join(parsed.ReasoningParts, ""); r != "" {
		if len(r) > e.emittedReasoningChars {
			suffix := r[e.emittedReasoningChars:]
			e.emitDelta(models.Delta{ReasoningContent: suffix}, nil)
			e.emittedReasoningChars = len(r)
		}
	}

	// 2. Native tool calls are noted (so the handler knows a tool call is
	//    coming) but not emitted yet — they are emitted atomically on finish.
	if len(parsed.FunctionCalls) > 0 {
		e.mu.Lock()
		e.sawNativeToolCall = true
		e.mu.Unlock()
	}

	// 3. Text: fence-aware incremental emit. Fenced bridge tool blocks are
	//    buffered (not emitted as prose) so protocol JSON never leaks.
	if t := strings.Join(parsed.TextParts, ""); t != "" {
		e.handleText(t)
	}

	return nil
}

// handleText streams text incrementally while detecting fenced tool-call
// blocks. A fence may span chunk boundaries, so partial opener bytes are held
// in holdTail and an open fence accumulates into blockBuf until closed.
func (e *liveEmitter) handleText(text string) {
	// Prepend any held tail from the previous chunk (potential fence opener).
	if e.holdTail != "" {
		text = e.holdTail + text
		e.holdTail = ""
	}

	if e.inFence {
		e.blockBuf.WriteString(text)
		e.processFenceBuffer()
		return
	}

	e.scanAndEmitText(text)
}

// scanAndEmitText handles text outside a fence: emit prose, detect opener.
func (e *liveEmitter) scanAndEmitText(text string) {
	for {
		if e.writeErr != nil {
			return
		}
		loc := fenceOpenerIndex(text)
		if loc.fenceStart < 0 {
			// No opener. Hold a trailing backtick/tilde run that could be the
			// start of a fence opener arriving in the next chunk.
			emittable, hold := splitTrailingFenceStart(text)
			e.emitTextIncremental(emittable)
			e.holdTail = hold
			return
		}
		// Emit prose before the fence.
		e.emitTextIncremental(text[:loc.fenceStart])
		// Enter fence mode.
		e.inFence = true
		e.fenceMarker = loc.marker
		e.fenceRunLen = loc.runLen
		e.blockBuf.Reset()
		e.blockBuf.WriteString(text[loc.jsonStart:])
		text = ""
		e.processFenceBuffer()
		if !e.inFence {
			// Fence closed within this chunk; loop to scan leftover (set by
			// processFenceBuffer via holdTail).
			if e.holdTail != "" {
				text = e.holdTail
				e.holdTail = ""
				continue
			}
		}
		return
	}
}

// processFenceBuffer inspects the accumulated fence buffer for a closing fence.
// If found, it parses the JSON block, validates it as a tool call, emits the
// tool_calls delta atomically, and hands any trailing text back via holdTail.
func (e *liveEmitter) processFenceBuffer() {
	buf := e.blockBuf.String()
	closeIdx, found := findFenceCloseInBuffer(buf, e.fenceMarker[0], e.fenceRunLen)
	if !found {
		// Still accumulating. Cap growth to avoid unbounded buffering of a
		// runaway fence; 256KB is well beyond any legitimate tool payload.
		if e.blockBuf.Len() > 256*1024 {
			// Treat as prose and flush.
			e.emitTextIncremental(buf)
			e.blockBuf.Reset()
			e.inFence = false
		}
		return
	}
	rest := buf[closeIdx+e.fenceRunLen:]
	// Skip the rest of the closing fence line.
	rest = skipFenceLineRest(rest)

	e.inFence = false
	e.blockBuf.Reset()

	// The fenced block is a candidate bridge tool call. Do NOT emit it as
	// prose. The handler will recover it from the final authoritative parse
	// and emit it atomically as delta.tool_calls. We only flag that a fence
	// was seen so the handler knows to expect a tool call.
	e.mu.Lock()
	e.sawFenceTool = true
	e.mu.Unlock()

	// Trailing text after the fence goes back through the scanner.
	if strings.TrimSpace(rest) != "" {
		e.holdTail = rest
	}
}

// emitTextIncremental writes only the newly-arrived text suffix.
func (e *liveEmitter) emitTextIncremental(text string) {
	if text == "" {
		return
	}
	// We don't track cumulative text across chunks here because each chunk's
	// TextParts is already incremental (extractTextFromGenerateChunk returns
	// only that chunk's text). Emit directly.
	e.emitDelta(models.Delta{Content: text}, nil)
	e.emittedTextChars += len(text)
}

// emitToolCalls writes a single delta.tool_calls carrying all calls, and records
// the finish reason. IDs are made stable so clients can correlate.
func (e *liveEmitter) emitToolCalls(calls []converter.FunctionCall) {
	openAIToolCalls := converter.BuildOpenAIToolCalls(calls, "call_"+e.requestID)
	for _, tc := range openAIToolCalls {
		if tc.ID != "" {
			e.streamedToolIDs[tc.ID] = true
		}
	}
	deltas := converter.BuildOpenAIStreamToolCallDeltas(openAIToolCalls)
	e.emitDelta(models.Delta{ToolCalls: deltas}, nil)
	e.mu.Lock()
	e.finishReason = "tool_calls"
	e.mu.Unlock()
}

// validateCalls runs JSON-Schema validation and drops invalid calls.
func (e *liveEmitter) validateCalls(calls []converter.FunctionCall) []converter.FunctionCall {
	if len(e.toolSpecs) == 0 {
		return calls
	}
	out := make([]converter.FunctionCall, 0, len(calls))
	for _, call := range calls {
		res := schema.ValidateToolCall(call.Name, call.Arguments, e.toolSpecs)
		if res.Valid {
			out = append(out, call)
		}
	}
	return out
}

// flushHeldText emits any text held in holdTail or still inside an open fence
// (treating an unclosed fence as prose on stream end). Called before finish.
func (e *liveEmitter) flushHeldText() {
	if e.holdTail != "" {
		e.emitTextIncremental(e.holdTail)
		e.holdTail = ""
	}
	if e.inFence {
		// Fence never closed: the model truncated mid-tool-call. Emit the raw
		// buffer as prose so the client sees something instead of a silent
		// truncation; the final parse will try to recover a tool call leniently.
		e.emitTextIncremental(e.blockBuf.String())
		e.blockBuf.Reset()
		e.inFence = false
	}
}

// finishWithToolCalls emits the validated tool calls as a single atomic
// delta.tool_calls, sets finish=tool_calls and writes [DONE]. Use when the
// final authoritative parse produced tool calls.
func (e *liveEmitter) finishWithToolCalls(calls []converter.FunctionCall) {
	e.flushHeldText()
	valid := e.validateCalls(calls)
	if len(valid) > 0 {
		e.emitToolCalls(valid)
	}
	finish := "tool_calls"
	if len(valid) == 0 {
		finish = "stop"
	}
	e.emitDelta(models.Delta{}, &finish)
	e.emitUsage()
	e.emitDone()
}

// finishStop emits finish=stop and [DONE]. Use when no tool calls were produced.
func (e *liveEmitter) finishStop() {
	e.flushHeldText()
	finish := "stop"
	e.emitDelta(models.Delta{}, &finish)
	e.emitUsage()
	e.emitDone()
}

// reconcileFinalText emits any trailing text present in the final authoritative
// parse that was not yet streamed (e.g. tail tokens after the last streamed
// chunk). fullText is the complete text from the final parse.
func (e *liveEmitter) reconcileFinalText(fullText string) {
	e.mu.Lock()
	emitted := e.emittedTextChars
	e.mu.Unlock()
	if len(fullText) > emitted {
		e.emitTextIncremental(fullText[emitted:])
	}
}

// sawToolActivity reports whether the emitter detected a native or fenced
// tool call during streaming.
func (e *liveEmitter) sawToolActivity() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sawNativeToolCall || e.sawFenceTool
}

// err returns the first write error encountered, if any.
func (e *liveEmitter) err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.writeErr
}

// chunkCount returns the number of SSE frames emitted.
func (e *liveEmitter) chunkCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sseChunks
}

// done reports whether [DONE] was written.
func (e *liveEmitter) done() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.wroteDone
}

// --- fence scanning helpers ---

type fenceOpener struct {
	fenceStart int
	jsonStart  int
	marker     string
	runLen     int
}

// fenceOpenerIndex finds the first ```json / ~~~json opener in text.
func fenceOpenerIndex(text string) (loc fenceOpener) {
	best := -1
	var bestMarker string
	var bestRun int
	for _, marker := range []string{"`", "~"} {
		run := 1
		for run <= 6 {
			pat := marker + strings.Repeat(marker, run-1)
			// opener: fence + optional spaces/tabs + "json" + non-newline
			idx := findFenceOpenerWithJson(text, pat)
			if idx >= 0 && (best < 0 || idx < best) {
				best = idx
				bestMarker = pat
				bestRun = run
			}
			run++
		}
	}
	if best < 0 {
		return fenceOpener{fenceStart: -1}
	}
	openerLen := len(bestMarker) // the run itself
	// Skip optional whitespace after the run, then "json", then to end of line.
	pos := best + openerLen
	for pos < len(text) && (text[pos] == ' ' || text[pos] == '\t') {
		pos++
	}
	if strings.HasPrefix(strings.ToLower(text[pos:]), "json") {
		pos += 4
	}
	// Skip to end of opener line.
	for pos < len(text) && text[pos] != '\n' {
		pos++
	}
	if pos < len(text) {
		pos++ // consume the newline; JSON starts after it
	}
	return fenceOpener{fenceStart: best, jsonStart: pos, marker: bestMarker, runLen: bestRun}
}

// findFenceOpenerWithJson locates "<run><ws?>json" at start-of-line.
func findFenceOpenerWithJson(text, run string) int {
	for i := 0; i+len(run) <= len(text); i++ {
		if text[i] != run[0] {
			continue
		}
		if text[i:i+len(run)] != run {
			continue
		}
		// Must be at start of line or start of text.
		if i > 0 && text[i-1] != '\n' {
			continue
		}
		// After the run, allow spaces/tabs then "json".
		j := i + len(run)
		for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
			j++
		}
		if strings.HasPrefix(strings.ToLower(text[j:]), "json") {
			return i
		}
	}
	return -1
}

// findFenceCloseInBuffer scans buf for a closing fence line made of markerCh
// with at least runLen chars. Returns the index just before the fence and true.
func findFenceCloseInBuffer(buf string, markerCh byte, runLen int) (int, bool) {
	for i := 0; i < len(buf); i++ {
		if buf[i] != markerCh {
			continue
		}
		// Closing fence must be at start of a line.
		if i > 0 && buf[i-1] != '\n' {
			continue
		}
		run := 0
		j := i
		for j < len(buf) && buf[j] == markerCh {
			run++
			j++
		}
		if run < runLen {
			continue
		}
		// Rest of line must be only whitespace.
		k := j
		for k < len(buf) && buf[k] != '\n' {
			if buf[k] != ' ' && buf[k] != '\t' && buf[k] != '\r' {
				break
			}
			k++
		}
		if k >= len(buf) || buf[k] == '\n' || buf[k] == '\r' {
			return i, true
		}
	}
	return 0, false
}

// skipFenceLineRest skips trailing whitespace/newline after the closing run.
func skipFenceLineRest(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	if len(s) > 0 && s[0] == '\r' {
		s = s[1:]
	}
	if len(s) > 0 && s[0] == '\n' {
		s = s[1:]
	}
	return s
}

// splitTrailingFenceStart splits off a trailing run of backticks/tilde that
// could be the beginning of a fence opener arriving in the next chunk. The
// held part is returned as the second value.
func splitTrailingFenceStart(text string) (emittable, hold string) {
	if len(text) == 0 {
		return "", ""
	}
	// Walk back from the end while on a backtick/tilde run.
	i := len(text)
	for i > 0 && (text[i-1] == '`' || text[i-1] == '~') {
		i--
	}
	if i == len(text) {
		return text, ""
	}
	tail := text[i:]
	// Only hold if the run is at a line start (otherwise it's inline code).
	if i == 0 || text[i-1] == '\n' {
		return text[:i], tail
	}
	return text, ""
}
