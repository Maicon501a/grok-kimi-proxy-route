package chat

// Stream-aware upstream body reading: idle-read watchdog + incremental JSON
// array decoder for AI Studio GenerateContent responses.
//
// The watchdog replaces the old total http.Client.Timeout so a long but
// healthy generation (gemini-3.1-pro with high thinking) is not killed
// mid-stream. Instead the request is aborted only when the upstream stops
// sending frames for longer than GenerateIdleReadMs, or when the hard ceiling
// (GenerateTimeout) is reached.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// idleReadError is returned when the watchdog aborts the request due to upstream
// silence. Callers can type-assert to distinguish it from transport errors.
type idleReadError struct{ reason string }

func (e *idleReadError) Error() string { return "chat: upstream idle read: " + e.reason }

// readWatchdog carrega o estado do watchdog. Err() retorna nil enquanto o
// watchdog NAO disparou. Nunca tratar o ponteiro do watchdog em si como erro:
// ele existe do inicio ao fim do request.
type readWatchdog struct {
	mu    sync.Mutex
	fired *idleReadError
}

func (w *readWatchdog) fire(reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired == nil {
		w.fired = &idleReadError{reason: reason}
	}
}

// Err retorna o erro de idle read quando o watchdog disparou, ou nil.
func (w *readWatchdog) Err() *idleReadError {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}

// startReadWatchdog derives a request context from ctx that is cancelled when:
//   - the parent ctx is cancelled (client disconnect),
//   - the hard ceiling GenerateTimeout elapses, or
//   - no bytes arrive from the upstream for GenerateIdleReadMs.
//
// It returns the derived context, a cancel function, and the watchdog handle;
// consulte watchdog.Err() para saber se o disparo foi por silencio do upstream.
func (c *Client) startReadWatchdog(ctx context.Context) (context.Context, context.CancelFunc, *readWatchdog) {
	cfg := c.cfg
	hardTimeout := time.Duration(cfg.GenerateTimeout) * time.Millisecond
	if hardTimeout <= 0 {
		hardTimeout = 10 * time.Minute
	}
	idleTimeout := time.Duration(cfg.GenerateIdleReadMs) * time.Millisecond
	if idleTimeout <= 0 {
		idleTimeout = 45 * time.Second
	}

	ceiledCtx, ceilCancel := context.WithTimeout(ctx, hardTimeout)
	idleCtx, idleCancel := context.WithCancel(ceiledCtx)

	watchdog := &readWatchdog{}

	activity := make(chan struct{}, 1)
	notifyActivity := func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}

	watchdogCtx, watchdogCancel := context.WithCancel(context.Background())
	go func() {
		timer := time.NewTimer(idleTimeout)
		defer timer.Stop()
		for {
			select {
			case <-watchdogCtx.Done():
				return
			case <-activity:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idleTimeout)
			case <-timer.C:
				watchdog.fire(fmt.Sprintf("nenhum frame em %s", idleTimeout))
				idleCancel()
				return
			}
		}
	}()

	// Attach the activity notifier to the context so the read loop / tee writer
	// can reset the watchdog via pingWatchdog(ctx) without exposing channels.
	idleCtx = context.WithValue(idleCtx, watchdogActivityKey{}, notifyActivity)

	cancel := func() {
		idleCancel()
		ceilCancel()
		watchdogCancel()
	}
	return idleCtx, cancel, watchdog
}

type watchdogActivityKey struct{}

// pingWatchdog signals the idle-read watchdog that bytes arrived. It is a no-op
// when the context was not created with startReadWatchdog.
func pingWatchdog(ctx context.Context) {
	if fn, ok := ctx.Value(watchdogActivityKey{}).(func()); ok && fn != nil {
		fn()
	}
}

// streamGenerateContentArray reads the upstream body as a streaming JSON array
// and invokes onChunk for every complete GenerateContent chunk element. The
// full body is mirrored into full so the caller can run a final authoritative
// parse. Returns the number of chunks delivered and an error (io.EOF is not
// reported as an error).
//
// The AI Studio wire format is:
//
//	["models/<name>", [chunk1, chunk2, ...], ...]
//
// where each chunkN is a complete JSON array value. We stream the inner array
// element-by-element with json.Decoder so chunks are delivered as soon as they
// are complete, without waiting for the closing brackets.
func streamGenerateContentArray(body io.ReadCloser, full *strings.Builder, onChunk func([]any) error, ctx context.Context) (int, error) {
	tee := io.TeeReader(body, &fullWriter{sb: full, ctx: ctx})
	dec := json.NewDecoder(tee)
	dec.UseNumber()

	chunks := 0
	// Opening top-level '['.
	tok, err := dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return 0, nil
		}
		return 0, err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '[' {
		// Not an array: let the caller fall back to full-body parse.
		return 0, nil
	}
	if !dec.More() {
		return 0, nil
	}

	// Element 0 may be the model string (shape A) or the chunks wrapper array
	// (shape B). Decode it; if it is an array we already have the chunks.
	var first any
	if err := dec.Decode(&first); err != nil {
		return 0, err
	}
	if firstArr, ok := first.([]any); ok {
		for _, item := range firstArr {
			if c, ok := item.([]any); ok {
				pingWatchdog(ctx)
				if err := onChunk(c); err != nil {
					return chunks, err
				}
				chunks++
			}
		}
		return chunks, nil
	}

	// Shape A: first was the model string; next token should be '[' opening the
	// chunks array. Stream its elements incrementally.
	if !dec.More() {
		return 0, nil
	}
	tok2, err := dec.Token()
	if err != nil {
		return chunks, err
	}
	d2, ok := tok2.(json.Delim)
	if !ok || d2 != '[' {
		// Unexpected shape; let the caller parse the full body.
		return chunks, nil
	}
	for dec.More() {
		if err := ctx.Err(); err != nil {
			return chunks, err
		}
		var chunk []any
		if err := dec.Decode(&chunk); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return chunks, err
		}
		pingWatchdog(ctx)
		if err := onChunk(chunk); err != nil {
			return chunks, err
		}
		chunks++
	}
	return chunks, nil
}

// fullWriter adapts a *strings.Builder to io.Writer for TeeReader. Every Write
// pings the idle-read watchdog so a healthy stream of bytes resets the stall
// timer, even when the decoder is mid-value.
type fullWriter struct {
	sb  *strings.Builder
	ctx context.Context
}

func (w *fullWriter) Write(p []byte) (int, error) {
	n, err := w.sb.Write(p)
	pingWatchdog(w.ctx)
	return n, err
}
