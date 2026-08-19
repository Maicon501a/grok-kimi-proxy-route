package proxyhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"grok-desktop/internal/store"
)

// handleGeminiUpstream forwards the normalized OpenAI chat payload to the
// supervised AI Studio runtime. Streaming bytes are copied unchanged so native
// tool calls, opaque continuation metadata, and terminal usage frames survive.
func (s *Server) handleGeminiUpstream(w http.ResponseWriter, ctx context.Context, clientPath string, stream bool, body map[string]any, settings store.Settings) {
	_ = settings
	baseURL := s.getAIStudioBaseURL()
	if baseURL == "" {
		http.Error(w, `{"error":{"message":"AI Studio runtime is not ready","type":"server_error"}}`, http.StatusServiceUnavailable)
		return
	}
	body["stream"] = stream
	raw, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := upstreamHTTPClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		w.Header().Set("Content-Type", firstHeader(resp.Header.Get("Content-Type"), "application/json"))
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	model, _ := body["model"].(string)
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	isSSE := strings.Contains(contentType, "text/event-stream") || (stream && contentType == "")
	if clientPath == "/responses" {
		if isSSE {
			if err := pipeKimiChatSSEToResponsesContext(ctx, w, resp.Body, model); err != nil {
				return
			}
			return
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusBadGateway)
			return
		}
		out, convertErr := chatCompletionJSONToResponse(raw, model)
		if convertErr != nil {
			w.Header().Set("Content-Type", firstHeader(resp.Header.Get("Content-Type"), "application/json"))
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(raw)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	for _, name := range []string{"Content-Type", "Cache-Control", "Connection", "X-Request-Id"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if isSSE {
		_, _ = io.Copy(flushingWriter{ResponseWriter: w}, resp.Body)
		return
	}
	_, _ = io.Copy(w, resp.Body)
}

type flushingWriter struct{ http.ResponseWriter }

func (w flushingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

func firstHeader(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}
