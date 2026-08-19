// Package httpserver exposes the OpenAI-compatible HTTP API.
//
// Endpoints:
//
//	GET  /health
//	GET  /v1/profiles
//	GET  /v1/accounts
//	GET  /v1/sessions/:sessionId
//	GET  /v1/models
//	GET  /v1/limits
//	POST /v1/audio/speech
//	POST /v1/chat/completions
//	POST /v1/chat/completions/test-tools
//	POST /v1/images/generations
//	POST /v1/images/upload
//	GET  /debug/last-response
package httpserver

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"grok-desktop/internal/aistudio/runtime"
)

// Server wires the runtime manager to an HTTP mux.
type Server struct {
	mgr    *runtime.Manager
	mux    *http.ServeMux
	images *generatedImageStore
}

// New constructs a Server.
func New(mgr *runtime.Manager) *Server {
	s := &Server{mgr: mgr, mux: http.NewServeMux(), images: newGeneratedImageStore()}
	s.routes()
	return s
}

// Handler returns the underlying mux for use with http.ListenAndServe.
func (s *Server) Handler() http.Handler {
	return s.loggingMiddleware(s.mux)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/health", s.health)
	s.mux.HandleFunc("/v1/profiles", s.profiles)
	s.mux.HandleFunc("/v1/accounts", s.accounts)
	s.mux.HandleFunc("/v1/sessions/", s.getSession)
	s.mux.HandleFunc("/v1/models", s.models)
	s.mux.HandleFunc("/v1/limits", s.limits)
	s.mux.HandleFunc("/v1/audio/speech", s.audioSpeech)
	s.mux.HandleFunc("/v1/chat/completions", s.chatCompletions)
	s.mux.HandleFunc("/v1/chat/completions/test-tools", s.testTools)
	s.mux.HandleFunc("/v1/images/generations", s.imagesGenerations)
	s.mux.HandleFunc("/v1/images/generated/", s.generatedImage)
	s.mux.HandleFunc("/v1/images/upload", s.imagesUpload)
	s.mux.HandleFunc("/debug/last-response", s.debugLastResponse)
	s.mux.HandleFunc("/", s.notFound)
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if r.URL.Path != "/health" {
			log.Printf("%s %s %d %dms", r.Method, r.URL.Path, rec.status, time.Since(start).Milliseconds())
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Flush promotes http.Flusher from the underlying ResponseWriter so SSE
// handlers keep flushing when wrapped by loggingMiddleware.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying ResponseWriter for http.ResponseController.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// writeJSON serializes v as JSON and sets the status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeError emits an OpenAI-compatible error envelope.
func writeError(w http.ResponseWriter, status int, message, errType string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    status,
		},
	})
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "Rota nao encontrada: "+r.Method+" "+r.URL.Path, "not_found")
}

// readProfileID resolves the requested profile from body/headers/query.
func (s *Server) readProfileID(r *http.Request, body map[string]any) string {
	if v, ok := body["profile_id"].(string); ok && v != "" {
		return v
	}
	if v := r.Header.Get("X-Profile-Id"); strings.TrimSpace(v) != "" {
		return v
	}
	if v := r.URL.Query().Get("profile_id"); strings.TrimSpace(v) != "" {
		return v
	}
	return ""
}
