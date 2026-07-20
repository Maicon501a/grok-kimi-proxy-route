package proxyhttp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// flushWriter wraps ResponseWriter and flushes after every write when possible.
type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func newFlushWriter(w http.ResponseWriter) *flushWriter {
	f, _ := w.(http.Flusher)
	return &flushWriter{w: w, f: f}
}

func (f *flushWriter) Write(p []byte) (int, error) {
	// Client disconnect mid-stream is normal (Codex cancel / app focus). Never panic.
	defer func() { _ = recover() }()
	n, err := f.w.Write(p)
	if err == nil && f.f != nil {
		func() {
			defer func() { _ = recover() }()
			f.f.Flush()
		}()
	}
	return n, err
}

func setSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

func writeSSE(w io.Writer, event string, data string) error {
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteString("\n")
	}
	// multi-line data support
	for _, line := range strings.Split(data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func writeSSEJSON(w io.Writer, event string, payload any, marshal func(any) ([]byte, error)) error {
	b, err := marshal(payload)
	if err != nil {
		return err
	}
	return writeSSE(w, event, string(b))
}

func isQuotaPayload(payload map[string]any) (bool, string) {
	errObj, ok := payload["error"].(map[string]any)
	if !ok {
		if s, ok := payload["error"].(string); ok && s != "" {
			errObj = map[string]any{"message": s}
		} else {
			return false, ""
		}
	}
	msg := ""
	if m, ok := errObj["message"].(string); ok {
		msg = m
	} else if m, ok := payload["message"].(string); ok {
		msg = m
	}
	if msg == "" {
		return false, ""
	}
	low := strings.ToLower(msg)
	if strings.Contains(low, "usage limit") ||
		strings.Contains(low, "billing cycle") ||
		strings.Contains(low, "resource_exhausted") ||
		strings.Contains(low, "access_terminated") ||
		strings.Contains(low, "balance exhausted") ||
		strings.Contains(low, "quota exceeded") ||
		strings.Contains(low, "upgrade to get more") ||
		(strings.Contains(low, "rate limit exceeded") && (strings.Contains(low, "quota") || strings.Contains(low, "usage") || strings.Contains(low, "billing"))) {
		return true, msg
	}
	return false, ""
}

// pipeSSE copies an upstream SSE body to client, flushing each event.
// Uses a large scanner limit so fat Grok tool/reasoning frames do not abort the stream.
func pipeSSE(ctx context.Context, dst http.ResponseWriter, src io.Reader) error {
	setSSEHeaders(dst)
	dst.WriteHeader(http.StatusOK)
	fw := newFlushWriter(dst)
	// 16MiB max token — Grok/Codex can emit very large single SSE data lines.
	const maxLine = 16 << 20
	sc := bufio.NewScanner(newContextReader(ctx, src))
	sc.Buffer(make([]byte, 0, 256*1024), maxLine)
	var block strings.Builder
	flushBlock := func() error {
		if block.Len() == 0 {
			return nil
		}
		_, err := io.WriteString(fw, block.String())
		if !strings.HasSuffix(block.String(), "\n\n") {
			_, _ = io.WriteString(fw, "\n")
		}
		block.Reset()
		return err
	}
	for sc.Scan() {
		line := sc.Text()
		block.WriteString(line)
		block.WriteByte('\n')
		if line == "" {
			// Detect mid-stream quota error inside SSE data lines BEFORE flushing
			// the block (flushBlock resets it, so scanning afterwards was dead code).
			quotaMsg := ""
			for _, ln := range strings.Split(block.String(), "\n") {
				if strings.HasPrefix(ln, "data: ") {
					payload := strings.TrimSpace(strings.TrimPrefix(ln, "data: "))
					if payload == "" || payload == "[DONE]" {
						continue
					}
					var m map[string]any
					if json.Unmarshal([]byte(payload), &m) == nil {
						if isQuota, qmsg := isQuotaPayload(m); isQuota {
							quotaMsg = qmsg
							break
						}
					}
				}
			}
			if err := flushBlock(); err != nil {
				// Client gone — stop quietly; do not surface as process death.
				return nil
			}
			if quotaMsg != "" {
				// Emit a graceful SSE error to the client so the stream ends cleanly
				// instead of a raw connection drop.
				errPayload, _ := json.Marshal(map[string]any{
					"error": map[string]any{
						"message": quotaMsg,
						"type":    "upstream_error",
						"code":    "quota_exhausted",
					},
				})
				_, _ = io.WriteString(fw, fmt.Sprintf("data: %s\n\n", errPayload))
				_, _ = io.WriteString(fw, "data: [DONE]\n\n")
				return fmt.Errorf("sse quota error: %s", quotaMsg)
			}
		}
	}
	_ = flushBlock()
	if err := sc.Err(); err != nil {
		// Token too long / network blip: log-friendly error, no panic.
		return fmt.Errorf("sse scan: %w", err)
	}
	return nil
}
