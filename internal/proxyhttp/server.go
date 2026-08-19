package proxyhttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"grok-desktop/internal/accio"
	"grok-desktop/internal/codexauth"
	"grok-desktop/internal/gemini"
	"grok-desktop/internal/logging"
	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
	"grok-desktop/internal/warp"
)

// Server is a local OpenAI + Anthropic compatible reverse proxy.

// Supported:

//

//	GET  /health

//	GET  /v1/models

//	POST /v1/chat/completions   (OpenAI)

//	POST /v1/responses          (OpenAI Responses)

//	POST /v1/messages           (Anthropic Messages)

//	POST /v1/search             (native xAI web_search + x_search)

//

// Not supported (explicit 404): /v1/completions (legacy).

//

// Multi-route: HTTP clients pick provider by model id on the same base URL

// (e.g. grok-4.6 → xAI, kimi-for-coding → Kimi Work). Global UI "active provider"

// is only a fallback when model is empty/alias.

type Server struct {
	mu sync.Mutex

	store *store.Store

	upstream *upstream.Client
	warp     *warp.Manager

	ensure func(ctx context.Context) (token string, account *store.Account, settings store.Settings, err error)

	// forceRefresh re-syncs CLI tokens and forces OAuth refresh for a specific account.

	forceRefresh func(ctx context.Context, accountID string) (token string, account *store.Account, settings store.Settings, err error)
	accio        *accio.Client

	// onQuota is optional: called when upstream returns 402 / balance exhausted so the app can

	// mark the account and rotate active. Return true if a different usable account is now active.

	onQuota func(accountID, reason string) (rotated bool)

	// onAuthFail is optional: called when upstream returns 401/403 permission-denied after a

	// failed force-refresh. Should mark auth-denied and rotate. Return true if another account is active.

	onAuthFail func(accountID, reason string) (rotated bool)

	srv *http.Server

	ln net.Listener

	addr string

	aiStudioBaseURL string
}

// maxProxyBodyBytes caps inbound request bodies (32 MiB) so a rogue client
// cannot exhaust memory with an unbounded io.ReadAll.
const maxProxyBodyBytes = 32 << 20

// upstreamHTTPClient is used for upstream provider calls. Total Timeout stays
// 0 (SSE streams must stay open for minutes); ResponseHeaderTimeout bounds
// how long we wait for the upstream to start responding.
var upstreamHTTPClient = &http.Client{
	Timeout: 0,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		ResponseHeaderTimeout: 120 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
	},
}

type ctxKey int

const routeProviderKey ctxKey = 1

// WithRouteProvider pins request-scoped provider for ensureCreds (xai|kimi_work|…).
func WithRouteProvider(ctx context.Context, provider string) context.Context {
	p := strings.TrimSpace(provider)
	if p == "" {
		return ctx
	}
	return context.WithValue(ctx, routeProviderKey, p)
}

// RouteProviderFrom returns the request-scoped provider override, if any.
func RouteProviderFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(routeProviderKey).(string)
	return strings.TrimSpace(v)
}

// contextReader wraps an io.Reader so that reads respect context cancellation.
// This prevents bufio.Scanner from blocking forever when the context is cancelled.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := c.r.Read(p)
		done <- result{n, err}
	}()
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	case res := <-done:
		return res.n, res.err
	}
}

func newContextReader(ctx context.Context, r io.Reader) io.Reader {
	return &contextReader{ctx: ctx, r: r}
}

func New(

	st *store.Store,

	up *upstream.Client,

	ensure func(ctx context.Context) (string, *store.Account, store.Settings, error),

) *Server {

	return &Server{store: st, upstream: up, ensure: ensure, warp: warp.Default()}

}

// SetQuotaHandler registers a callback invoked on quota exhaustion (402 / balance exhausted).

func (s *Server) SetQuotaHandler(fn func(accountID, reason string) (rotated bool)) {

	s.mu.Lock()

	defer s.mu.Unlock()

	s.onQuota = fn

}

// SetAuthFailHandler registers a callback for chat auth denial (403 permission-denied / 401).

func (s *Server) SetAuthFailHandler(fn func(accountID, reason string) (rotated bool)) {

	s.mu.Lock()

	defer s.mu.Unlock()

	s.onAuthFail = fn

}

// SetAIStudioBaseURL configures the supervised local AI Studio runtime used by
// Gemini routes. The child binds exclusively to loopback.
func (s *Server) SetAIStudioBaseURL(baseURL string) {
	s.mu.Lock()
	s.aiStudioBaseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	s.mu.Unlock()
}

func (s *Server) getAIStudioBaseURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aiStudioBaseURL
}

// SetForceRefresh registers force OAuth refresh (used before marking auth-denied).

func (s *Server) SetAccio(client *accio.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accio = client
}

func (s *Server) accioClient() *accio.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accio
}

func (s *Server) emitAccioError(ctx context.Context, accountID, model string, attempt int, err error) {
	logging.Error("accio.gateway.error", "account_id", accountID, "model", model, "attempt", attempt, "status", accio.GatewayStatus(err), "raw", accio.GatewayRawError(err))
}

func (s *Server) SetForceRefresh(fn func(ctx context.Context, accountID string) (string, *store.Account, store.Settings, error)) {

	s.mu.Lock()

	defer s.mu.Unlock()

	s.forceRefresh = fn

}

func (s *Server) quotaHandler() func(accountID, reason string) (rotated bool) {

	s.mu.Lock()

	defer s.mu.Unlock()

	return s.onQuota

}

// handleStreamQuota reports a quota error detected INSIDE an SSE stream (the
// upstream sent a 200 then an error payload mid-stream). The client already got
// a graceful error frame, so there is nothing to retry — but the account must be
// marked exhausted so the NEXT request rotates, and a background re-login must
// be kicked or the pool silently shrinks. Async: the quota handler may block.
func (s *Server) handleStreamQuota(accountID string, pipeErr error) {

	if accountID == "" || pipeErr == nil || !strings.Contains(pipeErr.Error(), "sse quota error") {

		return

	}

	if fn := s.quotaHandler(); fn != nil {

		go fn(accountID, pipeErr.Error())

		return

	}

	_, _ = s.store.MarkExhausted(accountID, pipeErr.Error())

}

func (s *Server) authFailHandler() func(accountID, reason string) (rotated bool) {

	s.mu.Lock()

	defer s.mu.Unlock()

	return s.onAuthFail

}

func (s *Server) forceRefreshFn() func(ctx context.Context, accountID string) (string, *store.Account, store.Settings, error) {

	s.mu.Lock()

	defer s.mu.Unlock()

	return s.forceRefresh

}

func (s *Server) Addr() string {

	s.mu.Lock()

	defer s.mu.Unlock()

	return s.addr

}

func (s *Server) Start(listen string) error {

	s.mu.Lock()

	defer s.mu.Unlock()

	if s.srv != nil {

		return nil

	}

	mux := http.NewServeMux()

	mux.HandleFunc("/health", s.handleHealth)

	mux.HandleFunc("/v1/models", s.handleModels)

	mux.HandleFunc("/models", s.handleModels)

	// OpenAI

	mux.HandleFunc("/v1/chat/completions", s.handleChat)

	mux.HandleFunc("/chat/completions", s.handleChat)

	mux.HandleFunc("/v1/responses", s.handleResponses)

	mux.HandleFunc("/responses", s.handleResponses)

	// Anthropic

	mux.HandleFunc("/v1/messages", s.handleMessages)

	mux.HandleFunc("/messages", s.handleMessages)

	// Native xAI search (OpenAI-compatible helper; reuses proxy auth)

	mux.HandleFunc("/v1/search", s.handleSearch)

	mux.HandleFunc("/search", s.handleSearch)

	mux.HandleFunc("/v1/web_search", s.handleSearch)

	mux.HandleFunc("/v1/x_search", s.handleSearch)

	// Explicitly reject legacy completions

	mux.HandleFunc("/v1/completions", s.handleLegacyCompletions)

	mux.HandleFunc("/completions", s.handleLegacyCompletions)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {

		if r.URL.Path == "/" {

			w.Header().Set("Content-Type", "application/json")

			_ = json.NewEncoder(w).Encode(map[string]any{

				"name": "grok-proxy-plus",

				"endpoints": []string{

					"/v1/models",

					"/v1/chat/completions",

					"/v1/responses",

					"/v1/messages",

					"/v1/search",
				},

				"not_supported": []string{"/v1/completions"},
			})

			return

		}

		http.NotFound(w, r)

	})

	ln, err := net.Listen("tcp", listen)

	if err != nil {

		return err

	}

	s.ln = ln

	s.addr = ln.Addr().String()

	// Recover panics per-request. If headers were already sent (SSE mid-stream),

	// writing another status panics again — nested recover swallows that so the

	// Wails process does not die while Codex is streaming Grok 4.5.

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		defer func() {

			if rec := recover(); rec != nil {

				log.Printf("proxyhttp panic on %s %s: %v", r.Method, r.URL.Path, rec)

				func() {

					defer func() { _ = recover() }()

					w.Header().Set("Content-Type", "application/json")

					w.WriteHeader(http.StatusInternalServerError)

					_, _ = w.Write([]byte(`{"error":{"message":"internal proxy panic","type":"server_error"}}`))

				}()

			}

		}()

		mux.ServeHTTP(w, r)

	})

	s.srv = &http.Server{

		Handler: handler,

		ReadHeaderTimeout: 30 * time.Second,

		// IdleTimeout bounds keep-alive connections waiting for the NEXT request;
		// an active SSE stream is not idle, so long-lived Codex streams are not cut.

		IdleTimeout: 120 * time.Second,

		// WriteTimeout 0: Grok 4.5 long reasoning streams can exceed minutes.

		ErrorLog: log.Default(),
	}
	srv := s.srv

	go func() {

		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {

			log.Printf("proxyhttp serve error (listener stopped): %v", err)

		}

	}()

	log.Printf("proxyhttp: listening on http://%s", s.addr)

	return nil

}

func (s *Server) Stop(ctx context.Context) error {

	s.mu.Lock()
	srv := s.srv
	if srv == nil {
		s.mu.Unlock()

		return nil

	}
	s.srv = nil
	s.ln = nil
	s.addr = ""
	s.mu.Unlock()
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	return err

}

func (s *Server) handleLegacyCompletions(w http.ResponseWriter, r *http.Request) {

	w.Header().Set("Content-Type", "application/json")

	w.WriteHeader(http.StatusNotFound)

	_ = json.NewEncoder(w).Encode(map[string]any{

		"error": map[string]any{

			"message": "Legacy /v1/completions is not supported. Use /v1/chat/completions (OpenAI), /v1/responses (OpenAI), or /v1/messages (Anthropic).",

			"type": "invalid_request_error",

			"code": "endpoint_not_supported",
		},
	})

}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {

	w.Header().Set("Content-Type", "application/json")

	settings := s.store.Settings()

	email := ""

	exhausted := false

	switch {

	case settings.IsOllie():

		email = "OllieChat (keyless)"

	case settings.IsGemini():

		email = "Gemini ADC · " + settings.EffectiveGeminiProject()

	case settings.IsKimiWork():

		acc, ok := s.store.PreferUsableAccount()

		if !ok {

			acc, ok = s.store.ActiveAccount()

		}

		if ok && acc != nil {

			email = acc.Label

			if email == "" {

				email = acc.Email

			}

			exhausted = acc.Exhausted()

		} else {

			email = "Kimi Work (no account)"

		}

	default:

		acc, ok := s.store.PreferUsableAccount()

		if !ok {

			acc, ok = s.store.ActiveAccount()

		}

		if ok && acc != nil {

			email = acc.Email

			exhausted = acc.Exhausted()

		}

	}

	warpStatus := map[string]any{"enabled": false}
	if s.warp != nil {
		warpStatus = s.warp.Status()
	}
	_ = json.NewEncoder(w).Encode(map[string]any{

		"status": "ok",

		"addr": s.Addr(),

		"account": email,

		"exhausted": exhausted,

		"provider": settings.NormalizedProvider(),

		"auth_mode": settings.ProviderAuthMode(),

		"upstream": settings.EffectiveUpstream(),

		"model": settings.ResolveModel("default"),
		"warp":  warpStatus,
	})

}

func (s *Server) gate(r *http.Request) bool {

	key := s.store.Settings().ProxyAPIKey

	if key == "" {

		return true

	}

	auth := r.Header.Get("Authorization")

	if strings.HasPrefix(strings.ToLower(auth), "bearer ") && strings.TrimSpace(auth[7:]) == key {

		return true

	}

	return r.Header.Get("X-API-Key") == key || r.Header.Get("x-api-key") == key

}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !s.gate(r) {
		http.Error(w, `{"error":{"message":"unauthorized","type":"invalid_request_error"}}`, http.StatusUnauthorized)
		return
	}
	// Unified catalog: same base URL lists Grok + Kimi (and optional others).
	// Clients pick provider by model id — no "active provider" gate on listing.
	base := s.store.Settings()

	var data []map[string]any

	// --- Grok / xAI (static; avoid ensure side-effects that flip active account) ---
	xaiModels := []upstream.ModelInfo{
		{ID: "grok-4.6", Name: "Grok 4.6", Description: "xAI Build · /v1/responses", APIMode: "responses"},
	}
	for _, m := range xaiModels {
		data = append(data, enrichModelMeta(m, store.ProviderXAI))
	}

	// --- OpenAI Codex / ChatGPT subscription ---
	for _, m := range upstream.CodexModels() {
		data = append(data, enrichModelMeta(m, store.ProviderCodex))
	}

	// --- OpenCode Zen Free (native/keyless) ---
	// Keep this catalog on the Grok proxy itself: clients only need this one
	// base URL and never need a separate `opencode serve` terminal process.
	for _, m := range upstream.OpenCodeZenFreeModels() {
		data = append(data, enrichModelMeta(m, store.ProviderOpenCodeZen))
	}
	if base.HasOpenCodeGoKey() {
		for _, m := range openCodeGoModels(base) {
			data = append(data, enrichModelMeta(m, store.ProviderOpenCodeGo))
		}
	}

	// --- Accio ---
	// Use the authenticated native client. Keep the fallback only when the
	// catalog is unavailable; a real catalog is authoritative and may omit it.
	if client := s.accioClient(); client != nil {
		models, err := client.Models(r.Context())
		if err != nil || len(models) == 0 {
			data = append(data, enrichModelMeta(upstream.ModelInfo{ID: accio.DefaultModel, Name: "Accio Nexus", Description: "Accio · login required", APIMode: "chat"}, store.ProviderAccio))
		} else {
			accioSeen := make(map[string]bool, len(models))
			for _, m := range models {
				if accioSeen[m.ID] {
					continue
				}
				accioSeen[m.ID] = true
				freeUse, locked := m.FreeUse, m.Locked
				data = append(data, enrichModelMeta(upstream.ModelInfo{
					ID: m.ID, Name: m.Name, Description: m.Description, APIMode: "chat",
					ContextWindow: m.Context, ReasoningEfforts: m.ReasoningEfforts,
					DefaultReasoningEffort: m.DefaultReasoningEffort, FreeUse: &freeUse, Locked: &locked,
				}, store.ProviderAccio))
			}
		}
	}

	// --- Kimi Work ---
	kimiModels := []upstream.ModelInfo{
		{ID: "k3-agent", Name: "K3 Max (Work)", Description: "Kimi Work · responses", APIMode: "responses"},
		{ID: "k3-agent-low", Name: "K3 Max — Low Think", Description: "Kimi Work · low reasoning", APIMode: "responses"},
		{ID: "k3-agent-medium", Name: "K3 Max — Medium Think", Description: "Kimi Work · medium reasoning", APIMode: "responses"},
		{ID: "k3-agent-high", Name: "K3 Max — High Think", Description: "Kimi Work · high reasoning", APIMode: "responses"},
		{ID: "k3-agent-xhigh", Name: "K3 Max — Extra High Think", Description: "Kimi Work · xhigh reasoning", APIMode: "responses"},
		{ID: "k2d6-agent", Name: "K2.6 Agent (Work)", Description: "Kimi Work · responses", APIMode: "responses"},
		{ID: "k2d6-agent-low", Name: "K2.6 Agent — Low Think", Description: "Kimi Work · low reasoning", APIMode: "responses"},
		{ID: "k2d6-agent-medium", Name: "K2.6 Agent — Medium Think", Description: "Kimi Work · medium reasoning", APIMode: "responses"},
		{ID: "k2d6-agent-high", Name: "K2.6 Agent — High Think", Description: "Kimi Work · high reasoning", APIMode: "responses"},
		{ID: "k2d6-agent-xhigh", Name: "K2.6 Agent — Extra High Think", Description: "Kimi Work · xhigh reasoning", APIMode: "responses"},
	}
	for _, m := range kimiModels {
		data = append(data, enrichModelMeta(m, store.ProviderKimiWork))
	}

	// Ollie / Gemini are listed whenever configured — independent of the global
	// UI provider, since HTTP clients route per-request by model id.
	// The Ollie probe is cached with a hard short timeout so a dead gateway
	// never blocks the catalog.
	if ollieUpstreamReachable() {
		for _, m := range []upstream.ModelInfo{
			{ID: "claude-sonnet-5", Name: "claude-sonnet-5", Description: "OllieChat", APIMode: "chat"},
			{ID: "claude-fable-5", Name: "claude-fable-5", Description: "OllieChat", APIMode: "chat"},
			{ID: "claude-opus-4-8", Name: "claude-opus-4-8", Description: "OllieChat", APIMode: "chat"},
			{ID: "deepseek-v4-flash-free", Name: "deepseek-v4-flash-free", Description: "OllieChat", APIMode: "chat"},
		} {
			data = append(data, enrichModelMeta(m, store.ProviderOllie))
		}
	}
	if s.getAIStudioBaseURL() != "" || geminiConfigured(base) {
		for _, id := range gemini.ListModels(r.Context(), base.WithProvider(store.ProviderGemini)) {
			data = append(data, enrichModelMeta(upstream.ModelInfo{ID: id, Name: id, Description: "Google AI Studio · local account pool", APIMode: "chat"}, store.ProviderGemini))
		}
	}

	// QwenBridge: dynamic model list from the local bridge. Omitted entirely
	// when the bridge is unreachable or no API key is configured; when
	// reachable, exactly the ids the bridge returns are listed.
	for _, id := range qwenBridgeModels(base) {
		data = append(data, enrichModelMeta(upstream.ModelInfo{ID: id, Name: id, Description: "QwenBridge · local", APIMode: "chat"}, store.ProviderQwen))
	}

	// DeepSeek: official API — listed whenever a key is configured (cached probe).
	for _, id := range deepSeekModels(base) {
		data = append(data, enrichModelMeta(upstream.ModelInfo{ID: id, Name: id, Description: "DeepSeek API · chat/completions", APIMode: "chat"}, store.ProviderDeepSeek))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
		"route":  "model",
		"api_policy": map[string]string{
			"xai":          "responses",
			"openai_codex": "responses",
			"kimi_work":    "chat",
			"deepseek":     "chat",
			"opencode_zen": "chat",
			"opencode_go":  "chat",
		},
		"note": "Pick model on the client; same baseURL routes Grok vs Kimi automatically.",
	})
}

// ollieProbeCache memoizes the Ollie gateway reachability probe so /v1/models
// never pays more than one short network check per TTL window.
var ollieProbeCache = struct {
	sync.Mutex
	at time.Time
	ok bool
}{}

const ollieProbeTTL = 60 * time.Second

// ollieUpstreamReachable pings the keyless OllieChat gateway with a hard short
// timeout. Any HTTP response below 500 means the gateway is up (even a 4xx);
// network errors and 5xx count as unreachable. Result is cached ollieProbeTTL.
func ollieUpstreamReachable() bool {
	ollieProbeCache.Lock()
	if time.Since(ollieProbeCache.at) < ollieProbeTTL {
		ok := ollieProbeCache.ok
		ollieProbeCache.Unlock()
		return ok
	}
	ollieProbeCache.Unlock()

	ok := false
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(store.OllieUpstream, "/")+"/models", nil)
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+store.OllieAPIKey)
		resp, derr := upstreamHTTPClient.Do(req)
		if derr == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
			ok = resp.StatusCode < 500
		}
	}

	ollieProbeCache.Lock()
	ollieProbeCache.at = time.Now()
	ollieProbeCache.ok = ok
	ollieProbeCache.Unlock()
	return ok
}

// qwenProbeCache memoizes the QwenBridge model-list probe so /v1/models never
// pays more than one short network check per TTL window. Keyed by upstream+key
// so a settings change invalidates immediately.
var qwenProbeCache = struct {
	sync.Mutex
	at     time.Time
	key    string
	models []string
}{}

const qwenProbeTTL = 60 * time.Second

// qwenBridgeModels fetches the dynamic model list from the local QwenBridge
// (GET {base}/models with Bearer). Returns nil when the key is missing, the
// bridge is unreachable (network error or 5xx), or the payload cannot be
// parsed — qwen is then omitted from the catalog. Result cached qwenProbeTTL.
func qwenBridgeModels(settings store.Settings) []string {
	base := strings.TrimRight(settings.EffectiveQwenUpstream(), "/")
	key := strings.TrimSpace(settings.QwenAPIKey)
	if base == "" || key == "" {
		return nil
	}
	cacheKey := base + "|" + key
	qwenProbeCache.Lock()
	if qwenProbeCache.key == cacheKey && time.Since(qwenProbeCache.at) < qwenProbeTTL {
		models := qwenProbeCache.models
		qwenProbeCache.Unlock()
		return models
	}
	qwenProbeCache.Unlock()

	var models []string
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+key)
		resp, derr := upstreamHTTPClient.Do(req)
		if derr == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				var parsed struct {
					Data []struct {
						ID string `json:"id"`
					} `json:"data"`
				}
				if json.Unmarshal(body, &parsed) == nil {
					for _, m := range parsed.Data {
						if id := strings.TrimSpace(m.ID); id != "" {
							models = append(models, id)
						}
					}
				}
			}
		}
	}

	qwenProbeCache.Lock()
	qwenProbeCache.at = time.Now()
	qwenProbeCache.key = cacheKey
	qwenProbeCache.models = models
	qwenProbeCache.Unlock()
	return models
}

// deepSeekProbeCache memoizes the DeepSeek model-list probe so /v1/models
// never pays more than one short network check per TTL window.
var deepSeekProbeCache = struct {
	sync.Mutex
	at     time.Time
	key    string
	models []string
}{}

const deepSeekProbeTTL = 60 * time.Second

// deepSeekModels fetches the official DeepSeek model list (GET /models with
// the decrypted API key). Returns nil when no key is configured, the API is
// unreachable, or the payload cannot be parsed — DeepSeek is then omitted
// from the catalog. Result cached deepSeekProbeTTL.
func deepSeekModels(settings store.Settings) []string {
	key := settings.DeepSeekAPIKeyPlain()
	if key == "" {
		return nil
	}
	cacheKey := "deepseek|" + key
	deepSeekProbeCache.Lock()
	if deepSeekProbeCache.key == cacheKey && time.Since(deepSeekProbeCache.at) < deepSeekProbeTTL {
		models := deepSeekProbeCache.models
		deepSeekProbeCache.Unlock()
		return models
	}
	deepSeekProbeCache.Unlock()

	base := strings.TrimRight(settings.WithProvider(store.ProviderDeepSeek).EffectiveUpstream(), "/")
	var models []string
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+key)
		resp, derr := upstreamHTTPClient.Do(req)
		if derr == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				var parsed struct {
					Data []struct {
						ID string `json:"id"`
					} `json:"data"`
				}
				if json.Unmarshal(body, &parsed) == nil {
					for _, m := range parsed.Data {
						if id := strings.TrimSpace(m.ID); id != "" {
							models = append(models, id)
						}
					}
				}
			}
		}
	}

	deepSeekProbeCache.Lock()
	deepSeekProbeCache.at = time.Now()
	deepSeekProbeCache.key = cacheKey
	deepSeekProbeCache.models = models
	deepSeekProbeCache.Unlock()
	return models
}

// openCodeGoProbeCache memoizes the OpenCode Go model-list probe so /v1/models
// never pays more than one short network check per TTL window.
var openCodeGoProbeCache = struct {
	sync.Mutex
	at     time.Time
	key    string
	models []upstream.ModelInfo
}{}

const openCodeGoProbeTTL = 5 * time.Minute

// openCodeGoModels fetches the OpenCode Go model list from the dedicated go
// gateway (GET zen/go/v1/models with the user key), the same catalog the
// opencode CLI surfaces.
// Falls back to the static catalog when the fetch fails or returns nothing,
// so the namespace never disappears from /v1/models. Cached openCodeGoProbeTTL.
func openCodeGoModels(settings store.Settings) []upstream.ModelInfo {
	key := settings.OpenCodeGoAPIKeyPlain()
	if key == "" {
		return upstream.OpenCodeGoModels()
	}
	cacheKey := "opencode-go|" + key
	openCodeGoProbeCache.Lock()
	if openCodeGoProbeCache.key == cacheKey && time.Since(openCodeGoProbeCache.at) < openCodeGoProbeTTL {
		models := openCodeGoProbeCache.models
		openCodeGoProbeCache.Unlock()
		return models
	}
	openCodeGoProbeCache.Unlock()

	models := upstream.OpenCodeGoModels()
	ctx, cancel := context.WithTimeout(context.Background(), 3000*time.Millisecond)
	defer cancel()
	if ids, err := upstream.FetchOpenCodeGoModelIDs(ctx, upstreamHTTPClient, key); err == nil && len(ids) > 0 {
		dynamic := make([]upstream.ModelInfo, 0, len(ids))
		for _, id := range ids {
			dynamic = append(dynamic, upstream.ModelInfo{
				ID: "opencode-go/" + id, Name: id, Description: "OpenCode Go · API key · chat/completions", APIMode: "chat",
			})
		}
		models = dynamic
	}

	openCodeGoProbeCache.Lock()
	openCodeGoProbeCache.at = time.Now()
	openCodeGoProbeCache.key = cacheKey
	openCodeGoProbeCache.models = models
	openCodeGoProbeCache.Unlock()
	return models
}

// geminiConfigured reports whether Vertex/ADC credentials are plausibly set up
// (explicit project in settings, or standard Google env vars), so /v1/models
// can list Gemini models without depending on the global UI provider switch.
func geminiConfigured(s store.Settings) bool {
	if strings.TrimSpace(s.GeminiProject) != "" {
		return true
	}
	for _, env := range []string{"GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT", "GOOGLE_APPLICATION_CREDENTIALS"} {
		if strings.TrimSpace(os.Getenv(env)) != "" {
			return true
		}
	}
	return false
}

func enrichModelMeta(m upstream.ModelInfo, provider string) map[string]any {
	ctxWindow := 256000
	owner := "xAI"
	prov := strings.ToLower(strings.TrimSpace(provider))
	switch prov {
	case store.ProviderOllie, "olliechat":
		owner = "OllieChat"
		prov = store.ProviderOllie
		ctxWindow = 128000
	case store.ProviderGemini, "google", "vertex":
		owner = "Google"
		prov = store.ProviderGemini
		ctxWindow = 1048576
	case store.ProviderQwen, "qwenbridge":
		owner = "Qwen"
		prov = store.ProviderQwen
		ctxWindow = 131072
	case store.ProviderDeepSeek, "deep-seek", "ds":
		owner = "DeepSeek"
		prov = store.ProviderDeepSeek
		ctxWindow = 131072
	case store.ProviderOpenCodeZen, "opencode-zen", "opencode", "zen", "zen-free":
		owner = "OpenCode Zen Free"
		prov = store.ProviderOpenCodeZen
		ctxWindow = 131072
	case store.ProviderOpenCodeGo, "opencode-go":
		owner = "OpenCode Go"
		prov = store.ProviderOpenCodeGo
		ctxWindow = 131072
	case store.ProviderCodex, "codex", "openai-codex", "chatgpt":
		owner = "OpenAI"
		prov = store.ProviderCodex
		ctxWindow = 272000
	case store.ProviderAccio, "accio-work", "phoenix":
		owner = "Accio"
		prov = store.ProviderAccio
		ctxWindow = 131072
	case store.ProviderKimiWork, "kimi", "kimi-work":
		owner = "Kimi"
		prov = store.ProviderKimiWork
		ctxWindow = 1048576
		if strings.Contains(strings.ToLower(m.ID), "k2") {
			ctxWindow = 262144
		}
	default:
		owner = "xAI"
		prov = store.ProviderXAI
		id := strings.ToLower(m.ID)
		if strings.Contains(id, "4.5") || strings.Contains(id, "4.6") {
			ctxWindow = 500000
		}
	}
	desc := m.Description
	if desc == "" {
		desc = owner
	}
	apiMode := m.APIMode
	if apiMode == "" {
		if prov == store.ProviderXAI {
			apiMode = "responses"
		} else {
			apiMode = "chat"
		}
	}
	apiBackend := "chat_completions"
	switch strings.ToLower(strings.TrimSpace(apiMode)) {
	case "responses":
		apiBackend = "responses"
	case "messages":
		apiBackend = "messages"
	}
	out := map[string]any{
		"id":                       m.ID,
		"object":                   "model",
		"created":                  time.Now().Unix(),
		"owned_by":                 owner,
		"provider":                 prov,
		"name":                     firstNonEmpty(m.Name, m.ID),
		"description":              desc,
		"api_mode":                 apiMode,
		"api_backend":              apiBackend,
		"supported_in_api":         true,
		"root":                     m.Root,
		"context_window":           firstPositiveInt64(m.ContextWindow, int64(ctxWindow)),
		"context_length":           firstPositiveInt64(m.ContextWindow, int64(ctxWindow)),
		"max_tokens":               firstPositiveInt64(m.ContextWindow, int64(ctxWindow)),
		"reasoning_efforts":        m.ReasoningEfforts,
		"default_reasoning_effort": m.DefaultReasoningEffort,
	}
	if m.FreeUse != nil {
		out["free_use"] = *m.FreeUse
	}
	if m.Locked != nil {
		out["locked"] = *m.Locked
	}
	return out
}

func firstPositiveInt64(value, fallback int64) int64 {
	if value > 0 {
		return value
	}
	return fallback
}

func clientSetReasoningEffort(body map[string]any) bool {
	if body == nil {
		return false
	}
	if effort, _ := body["reasoning_effort"].(string); strings.TrimSpace(effort) != "" {
		return true
	}
	if effort, _ := body["reasoningEffort"].(string); strings.TrimSpace(effort) != "" {
		return true
	}
	if reasoning, _ := body["reasoning"].(map[string]any); reasoning != nil {
		if effort, _ := reasoning["effort"].(string); strings.TrimSpace(effort) != "" {
			return true
		}
	}
	return false
}

// applyAccioDefaultReasoningEffort injects the strongest effort advertised by
// the matched Accio model only when the external client omitted an effort.
// Catalog effort lists are ordered from lowest to highest.
func applyAccioDefaultReasoningEffort(body map[string]any, modelValue any, models []accio.Model) bool {
	if clientSetReasoningEffort(body) {
		return false
	}
	modelID, _ := modelValue.(string)
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return false
	}
	for _, model := range models {
		if model.ID != modelID && strings.TrimPrefix(model.ID, "accio/") != strings.TrimPrefix(modelID, "accio/") {
			continue
		}
		for i := len(model.ReasoningEfforts) - 1; i >= 0; i-- {
			if effort := strings.TrimSpace(model.ReasoningEfforts[i]); effort != "" {
				body["reasoning_effort"] = effort
				return true
			}
		}
		return false
	}
	return false
}

// defaultAccioReasoningEffort loads the per-account Accio catalog and applies
// its highest supported effort. If the catalog is unavailable, preserve the
// prior behavior and let the gateway choose its own default.
func defaultAccioReasoningEffort(ctx context.Context, client *accio.Client, modelValue any, body map[string]any) {
	if clientSetReasoningEffort(body) {
		return
	}
	models, err := client.Models(ctx)
	if err != nil {
		return
	}
	applyAccioDefaultReasoningEffort(body, modelValue, models)
}

func validateAccioReasoning(ctx context.Context, client *accio.Client, modelValue any, body map[string]any) error {
	modelID, _ := modelValue.(string)
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	effort, _ := body["reasoning_effort"].(string)
	if strings.TrimSpace(effort) == "" {
		if compat, ok := body["reasoningEffort"].(string); ok && strings.TrimSpace(compat) != "" {
			effort = strings.TrimSpace(compat)
			body["reasoning_effort"] = effort
		}
	}
	if strings.TrimSpace(effort) == "" {
		return nil
	}
	models, err := client.Models(ctx)
	if err != nil {
		return nil // Preserve upstream behavior when metadata is temporarily unavailable.
	}
	for _, model := range models {
		if model.ID != modelID && strings.TrimPrefix(model.ID, "accio/") != strings.TrimPrefix(modelID, "accio/") {
			continue
		}
		if len(model.ReasoningEfforts) == 0 {
			return nil
		}
		for _, allowed := range model.ReasoningEfforts {
			if effort == allowed {
				return nil
			}
		}
		return fmt.Errorf("reasoning_effort %q is not supported by model %q; allowed values: %s", effort, modelID, strings.Join(model.ReasoningEfforts, ", "))
	}
	return nil
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {

	s.proxyUpstream(w, r, "/chat/completions")

}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {

	s.proxyUpstream(w, r, "/responses")

}

// injectTemporalIntoMessages prepends a system line with today's date/year (e.g. 2026).

func injectTemporalIntoMessages(msgs []any) []any {

	now := time.Now()

	line := "Temporal context: today is " + now.Format("Monday, January 2, 2006") +

		". The current year is " + strconv.Itoa(now.Year()) +

		". Treat this as ground truth for \"today\", \"this year\", recency, and time-sensitive answers — do not assume you are stuck in 2023–2024."

	if len(msgs) > 0 {

		if first, ok := msgs[0].(map[string]any); ok {

			role, _ := first["role"].(string)

			if role == "system" {

				content := contentToString(first["content"])

				if !strings.Contains(content, "Temporal context:") {

					first["content"] = line + "\n\n" + content

					msgs[0] = first

				}

				return msgs

			}

		}

	}

	sys := map[string]any{"role": "system", "content": line}

	return append([]any{sys}, msgs...)

}

// injectManagedSystemPromptIntoMessages prepends the proxy-managed prompt as a
// regular OpenAI system message. The caller owns the prompt lookup; this helper
// only shapes an already-authorized local setting for the selected request.
func injectManagedSystemPromptIntoMessages(msgs []any, prompt string) []any {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return msgs
	}
	sys := map[string]any{"role": "system", "content": prompt}
	return append([]any{sys}, msgs...)
}

// injectManagedSystemPromptIntoResponses appends the local prompt to Responses
// instructions without replacing client-provided instructions.
func injectManagedSystemPromptIntoResponses(body map[string]any, prompt string) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return
	}
	current := strings.TrimSpace(contentToString(body["instructions"]))
	if current == "" {
		body["instructions"] = prompt
		return
	}
	body["instructions"] = prompt + "\n\n" + current
}

func contentToString(c any) string {

	switch t := c.(type) {

	case string:

		return t

	case []any:

		var b strings.Builder

		for _, p := range t {

			if m, ok := p.(map[string]any); ok {

				if s := asString(m["text"]); s != "" {

					b.WriteString(s)

				}

			} else if s, ok := p.(string); ok {

				b.WriteString(s)

			}

		}

		return b.String()

	default:

		return ""

	}

}

func (s *Server) proxyUpstream(w http.ResponseWriter, r *http.Request, path string) {

	if r.Method != http.MethodPost {

		http.Error(w, `{"error":{"message":"method not allowed"}}`, http.StatusMethodNotAllowed)

		return

	}

	if !s.gate(r) {

		http.Error(w, `{"error":{"message":"unauthorized"}}`, http.StatusUnauthorized)

		return

	}

	// Read body first so we can route provider from client model id (same baseURL).
	// Cap the body so a rogue client cannot exhaust memory.
	r.Body = http.MaxBytesReader(w, r.Body, maxProxyBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	stream := false
	clientPath := path // original path before any rewrite
	codexClient := isCodexRequest(r)

	var m map[string]any
	reqModel := ""
	if json.Unmarshal(body, &m) == nil {
		if v, ok := m["stream"].(bool); ok {
			stream = v
		}
		reqModel, _ = m["model"].(string)
	} else {
		m = nil
	}

	// Route ONLY by client model (never UI global provider).
	// accio/* → Accio, grok-* → xAI, k3-agent/kimi-* → Kimi, qwen* → QwenBridge, empty/alias/unknown → xAI.
	baseSettings := s.store.Settings()
	routeSettings := baseSettings.WithProviderForModel(reqModel)
	if codexClient && store.IsBareCodexModel(reqModel) {
		routeSettings = baseSettings.WithProvider(store.ProviderCodex)
	}
	routeProv := routeSettings.NormalizedProvider()
	if message := store.ProviderAvailabilityMessage(routeProv); message != "" && store.ProviderAvailabilityBlocksRequests(routeProv) {
		typ := "provider_disabled"
		if store.ProviderAvailability(routeProv) == "maintenance" {
			typ = "provider_maintenance"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message":  message,
				"type":     typ,
				"provider": routeProv,
			},
		})
		return
	}
	ctx := WithRouteProvider(r.Context(), routeProv)
	ctx = logging.WithRequestID(ctx, uuid.NewString()[:8])
	reqLog := logging.FromContext(ctx).With("provider", routeProv, "path", clientPath, "stream", stream)
	// Track every account Inc'd by ensure (initial pick + rotation retries) so
	// in-flight counters are decremented exactly once per Inc on any exit path.
	ctx, inflight := store.WithInflightTracker(ctx)
	defer inflight.DecAll(s.store)

	token, acc, settings, err := s.ensure(ctx)
	if err != nil {
		reqLog.Error("proxy.request.error", "model", reqModel, "status", 401, "reason", err.Error())
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	accountID := ""
	if acc != nil {
		store.TrackInflight(ctx, acc.ID)
		accountID = acc.ID
	}
	resolvedModel := settings.ResolveModelForClient(reqModel)
	reqLog.Info("proxy.request.start", "model", reqModel, "resolved_model", resolvedModel, "account_id", accountID)
	// ensure may return store settings; force route from client model again.
	settings = routeSettings
	if settings.IsCodex() && acc != nil {
		settings.CodexAccountID = acc.TeamID
		settings.CodexFedRAMP = strings.Contains(acc.Source, "fedramp")
	}
	var clientStatus int
	if routeProv == store.ProviderAccio {
		client := s.accioClient()
		if client == nil {
			http.Error(w, `{"error":{"message":"Accio provider is not initialized","type":"server_error"}}`, http.StatusServiceUnavailable)
			return
		}
		// Accio uses native Phoenix requests, but still participates in the same
		// account failover loop as the generic providers.
		if m == nil {
			_ = json.Unmarshal(body, &m)
		}
		if m == nil {
			m = map[string]any{}
		}
		m["model"] = settings.ResolveModelForClient(reqModel)
		resolvedAccioModel, _ := m["model"].(string)
		if msgs, ok := m["messages"].([]any); ok {
			m["messages"] = injectManagedSystemPromptIntoMessages(msgs, settings.SystemPromptFor(routeProv, resolvedAccioModel))
		}
		// External clients own an explicit effort. When they omit it, use the
		// highest level advertised by this Accio model instead of the desktop UI
		// setting or a generic hard-coded value.
		defaultAccioReasoningEffort(r.Context(), client, m["model"], m)
		if err := validateAccioReasoning(r.Context(), client, m["model"], m); err != nil {
			clientStatus = http.StatusBadRequest
			http.Error(w, fmt.Sprintf(`{"error":{"message":%q,"type":"invalid_request_error"}}`, err.Error()), clientStatus)
			return
		}
		for attempt := 0; attempt < 3; attempt++ {
			err := client.ChatWithToken(r.Context(), m, token, w)
			if err == nil {
				clientStatus = http.StatusOK
				return
			}
			rawErr := accio.GatewayRawError(err)
			reqLog.Error("accio.gateway.error", "account_id", accountID, "model", reqModel, "attempt", attempt+1, "status", accio.GatewayStatus(err), "raw", rawErr)
			s.emitAccioError(r.Context(), accountID, reqModel, attempt+1, err)
			if !isQuotaStatus(accio.GatewayStatus(err), []byte(rawErr)) && !isAuthStatus(accio.GatewayStatus(err), []byte(rawErr)) {
				clientStatus = http.StatusBadGateway
				http.Error(w, rawErr, clientStatus)
				return
			}
			rotated := false
			if fn := s.quotaHandler(); isQuotaStatus(accio.GatewayStatus(err), []byte(rawErr)) && fn != nil {
				rotated = fn(accountID, rawErr)
			}
			if fn := s.authFailHandler(); isAuthStatus(accio.GatewayStatus(err), []byte(rawErr)) && !isQuotaStatus(accio.GatewayStatus(err), []byte(rawErr)) && fn != nil {
				rotated = fn(accountID, rawErr)
			}
			if !rotated {
				_, _ = s.store.MarkExhausted(accountID, rawErr)
				if isAuthStatus(accio.GatewayStatus(err), []byte(rawErr)) {
					_, _ = s.store.MarkAuthDenied(accountID, rawErr)
				}
			}
			next := s.store.NextUsableAccountID(accountID)
			if next == "" {
				clientStatus = http.StatusBadGateway
				http.Error(w, rawErr, clientStatus)
				return
			}
			_ = s.store.SetActiveAccount(next)
			tok2, acc2, _, err2 := s.ensure(r.Context())
			if err2 != nil || acc2 == nil {
				clientStatus = http.StatusBadGateway
				http.Error(w, rawErr, clientStatus)
				return
			}
			prev := accountID
			token, acc, accountID = tok2, acc2, acc2.ID
			reqLog.Warn("accio.account.rotated", "from_account", prev, "to_account", accountID, "reason", "gateway_error")
		}
		clientStatus = http.StatusBadGateway
		http.Error(w, "Accio gateway retries exhausted", clientStatus)
		return
	}
	if routeProv == store.ProviderKimiWork {
		settings.UpstreamBase = store.KimiWorkUpstream
		settings.APIMode = "chat"
	} else if routeProv == store.ProviderQwen {
		settings.UpstreamBase = settings.EffectiveQwenUpstream()
		settings.APIMode = "chat"
	} else if routeProv == store.ProviderDeepSeek {
		settings.UpstreamBase = store.DeepSeekUpstream
		settings.APIMode = "chat"
	} else if routeProv == store.ProviderOpenCodeZen {
		settings.UpstreamBase = store.OpenCodeZenUpstream
		settings.APIMode = "chat"
	} else if routeProv == store.ProviderOpenCodeGo {
		settings.UpstreamBase = store.OpenCodeGoUpstream
		settings.APIMode = "chat"
	} else if routeProv == store.ProviderCodex {
		settings.UpstreamBase = store.CodexUpstream
		settings.APIMode = "responses"
	} else if routeProv == store.ProviderXAI {
		settings.UpstreamBase = store.DefaultUpstream
		// Client often uses /chat/completions; xAI wire still goes /responses.
		settings.APIMode = "chat"
	}

	// Endpoint rewrite (same baseURL, model decides provider):
	//   Grok default: client /chat/completions → upstream /responses → client chat again
	//   Grok optional: client /responses stays /responses end-to-end
	//   Kimi: /responses → /chat/completions
	wantClientChat := path == "/chat/completions"
	wantClientResponses := path == "/responses"

	startedAt := time.Now()
	resolvedModel = settings.ResolveModelForClient(reqModel)
	defer func() {
		dur := time.Since(startedAt).Milliseconds()
		if clientStatus >= 400 {
			reqLog.Error("proxy.request.error", "model", resolvedModel, "account_id", accountID, "status", clientStatus, "duration_ms", dur)
		} else if clientStatus > 0 {
			reqLog.Info("proxy.request.done", "model", resolvedModel, "account_id", accountID, "duration_ms", dur, "status", clientStatus)
		}
	}()

	if m != nil {
		// Do NOT inject settings.ReasoningEffort here.
		// Global effort is only for the desktop app's own chat UI (app.go).
		// HTTP clients (OpenCode, etc.) must send reasoning_effort themselves
		// or via model alias (e.g. k3-agent-high). Omitted = upstream default.
		// Honor client model; only empty/alias → default of ROUTEd provider.
		m["model"] = settings.ResolveModelForClient(reqModel)
		resolvedModel, _ = m["model"].(string)

		// alias last_response_id
		if prev, ok := m["last_response_id"].(string); ok && prev != "" {
			m["previous_response_id"] = prev
			delete(m, "last_response_id")
		}

		// Kimi / Ollie / Gemini / Qwen / DeepSeek / OpenCode: /responses → chat/completions
		if (settings.IsOllie() || settings.IsGemini() || settings.IsKimiWork() || settings.IsQwen() || settings.IsDeepSeek() || settings.IsOpenCodeZen() || settings.IsOpenCodeGo()) && path == "/responses" {
			path = "/chat/completions"
			wantClientChat = true
			wantClientResponses = false
			m = responsesBodyToChatCompletions(m, settings)
			if mid, ok := m["model"].(string); ok && mid != "" {
				resolvedModel = mid
			}
		}

		// Enforce Kimi hard context-window limits (K2.6 = 262k, K3 = 1M).
		if settings.IsKimiWork() {
			m = clampKimiContextLimits(m, resolvedModel)
		}

		// Grok default path: client chat/completions → xAI /responses (response translated back later)
		if (settings.IsXAI() || settings.IsCodex()) && path == "/chat/completions" {
			path = "/responses"
			m = chatCompletionsBodyToResponses(m, settings)
			if mid, ok := m["model"].(string); ok && mid != "" {
				resolvedModel = mid
			}
		}

		if path == "/chat/completions" {

			if msgs, ok := m["messages"].([]any); ok {

				msgs = injectManagedSystemPromptIntoMessages(msgs, settings.SystemPromptFor(routeProv, resolvedModel))
				m["messages"] = injectTemporalIntoMessages(msgs)

			}

			// Sanitize tools — drop namespace / invalid types (prevents 422)

			if _, ok := m["tools"]; ok {

				tools := sanitizeChatTools(m["tools"])

				if len(tools) == 0 {

					delete(m, "tools")

					delete(m, "tool_choice")

				} else {

					m["tools"] = tools

				}

			}

			if stream {

				if _, ok := m["stream_options"]; !ok {

					m["stream_options"] = map[string]any{"include_usage": true}

				}

			}

			if settings.IsOllie() {

				sanitizeOllieChatBody(m)

			}

			// Kimi agent-gw: real wire model + thinking object from client only.
			if settings.IsKimiWork() {
				sanitizeKimiWorkChatBody(m)
			}

			if settings.IsGemini() {

				// Vertex path ignores OpenAI-only junk.

				delete(m, "store")

				delete(m, "previous_response_id")

				// Proxy Plus understands OpenAI tools, tool_choice, and stream
				// usage options natively. Keep them intact for AI Studio.

			}

		}

		if path == "/responses" {
			injectManagedSystemPromptIntoResponses(m, settings.SystemPromptFor(routeProv, resolvedModel))
			if settings.IsCodex() {
				// The subscription-backed Codex endpoint is stateless and mirrors the
				// official CLI request contract.
				m["store"] = false
				m["stream"] = true
				delete(m, "previous_response_id")
				delete(m, "temperature")
				delete(m, "top_p")
				if input, ok := m["input"]; ok {
					m["input"] = normalizeCodexResponsesInput(input)
				}
				ensureResponsesInclude(m, "reasoning.encrypted_content")
				if _, ok := m["instructions"]; !ok {
					m["instructions"] = ""
				}
				if reasoning, ok := m["reasoning"].(map[string]any); ok {
					reasoning["effort"] = normalizeCodexReasoningEffort(asString(reasoning["effort"]))
				}
				if effort, ok := m["reasoning_effort"].(string); ok {
					m["reasoning_effort"] = normalizeCodexReasoningEffort(effort)
				}
			}

			if settings.StoreResponses {

				if _, ok := m["store"]; !ok {

					m["store"] = true

				}

			}

			// Only map client-provided effort → Responses reasoning object.
			// Never invent from global settings (app UI only).
			if _, ok := m["reasoning"]; !ok {

				if eff, _ := m["reasoning_effort"].(string); eff != "" {

					m["reasoning"] = map[string]any{"effort": eff, "summary": "auto"}

				}

			}
			if settings.IsCodex() {
				if reasoning, ok := m["reasoning"].(map[string]any); ok && strings.HasPrefix(strings.ToLower(resolvedModel), "gpt-5.6-") {
					reasoning["context"] = "all_turns"
				}
				delete(m, "reasoning_effort")
				if strings.HasPrefix(strings.ToLower(resolvedModel), "gpt-5.6-") {
					m["parallel_tool_calls"] = false
				}
			}

			// Sanitize tools (fixes OpenCode 422 unknown variant `namespace`).
			// When the client already sent function tools (OpenCode/Kilo), keep
			// only those — do not inject web_search/x_search or the model
			// spends the turn on server-side search instead of calling bash.
			if raw, ok := m["tools"]; ok && !settings.IsCodex() {
				tools := sanitizeResponsesTools(raw)
				if settings.IsXAI() && !hasFunctionTool(tools) {
					tools = withNativeSearch(tools)
				}
				if len(tools) == 0 {
					delete(m, "tools")
					delete(m, "tool_choice")
				} else {
					m["tools"] = tools
				}
			} else if settings.IsXAI() {
				m["tools"] = nativeSearchTools()
			}

			if _, ok := m["tool_choice"]; !ok {

				if _, hasTools := m["tools"]; hasTools {

					m["tool_choice"] = "auto"

				}

			}

			// CRITICAL: sanitize input (fixes Codex 422 untagged enum ModelInput)

			if raw, ok := m["input"]; ok && !settings.IsCodex() {

				m["input"] = sanitizeResponsesInput(raw)

			}

			if input, ok := m["input"].([]any); ok && !settings.IsCodex() {

				m["input"] = injectTemporalIntoResponsesInput(input)

			}

			// xAI wire contract: a continuation request carrying both
			// instructions and previous_response_id is rejected with 400
			// ("Argument not supported"). On resume the stored conversation
			// already holds the instructions — keep previous_response_id and
			// drop instructions.
			if settings.IsXAI() && asString(m["previous_response_id"]) != "" {

				delete(m, "instructions")

			}

		}

		body, _ = json.Marshal(m)

	}

	// Gemini: handle entirely via Vertex REST + ADC (not reverse-proxy to UpstreamBase).

	if settings.IsGemini() {

		if m == nil {

			_ = json.Unmarshal(body, &m)

			if m == nil {

				m = map[string]any{}

			}

			reqModel, _ := m["model"].(string)

			if codexClient {

				m["model"] = settings.ResolveModelForCodex(reqModel)

			} else {

				m["model"] = settings.ResolveModelForClient(reqModel)

			}

			if clientPath == "/responses" {

				m = responsesBodyToChatCompletions(m, settings)

			}

			if v, ok := m["stream"].(bool); ok {

				stream = v

			}

		}

		clientStatus = 200
		s.handleGeminiUpstream(w, r.Context(), clientPath, stream, m, settings)

		return

	}

	// Up to 3 attempts: original → force-refresh same account (auth) → rotated account.

	maxAttempts := 3

	if settings.IsOllie() {

		maxAttempts = 1

		accountID = ""

	}

	// QwenBridge / DeepSeek: single account — no rotation; upstream 429/401
	// goes straight back to the client.
	if settings.IsQwen() || settings.IsDeepSeek() {

		maxAttempts = 1

		accountID = ""

	}

	warpTried := false
	viaWarp := false
	if settings.IsOpenCodeZen() {
		accountID = ""
		if s.warp != nil && s.warp.Enabled() {
			viaWarp = s.warp.IsActive()
			warpTried = viaWarp
			if !viaWarp {
				maxAttempts = 2
			}
		} else {
			maxAttempts = 1
		}
	}

	authRetried := false
	codexRefreshTransient := false
	openCodeThinkingRetried := false

	// rotateCreds adopts credentials from a retry ensure/forceRefresh.
	// Settings stay pinned to routeSettings; only token/acc swap. Returns false
	// when the rotated account belongs to another provider — the request must
	// fail instead of sending a Kimi-shaped body to the xAI upstream (or vice
	// versa) with a mismatched token.
	rotateCreds := func(tok2 string, acc2 *store.Account) bool {
		if acc2 != nil && acc2.NormalizedProvider() != routeProv {
			return false
		}
		if acc2 != nil && acc2.ID != accountID {
			// Dec the previous account now that the next one is Inc'd.
			if n := inflight.Remove(accountID); n > 0 {
				for i := 0; i < n; i++ {
					s.store.DecAccountRequestCount(accountID)
				}
			}
		}
		if acc2 != nil {
			store.TrackInflight(ctx, acc2.ID)
			if settings.IsCodex() {
				settings.CodexAccountID = acc2.TeamID
				settings.CodexFedRAMP = strings.Contains(acc2.Source, "fedramp")
			}
		}
		token, acc = tok2, acc2
		if acc2 != nil {
			accountID = acc2.ID
		}
		return true
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {

		url := strings.TrimRight(settings.EffectiveUpstream(), "/") + path
		if settings.IsOpenCodeGo() {
			url = strings.TrimRight(store.OpenCodeGoGateway, "/") + path
		}

		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))

		if err != nil {

			http.Error(w, err.Error(), 500)

			return

		}

		setUpstreamAuthHeaders(req, token, settings)

		if settings.IsCodex() {
			req.Header.Set("Accept", "text/event-stream")
		} else if v := r.Header.Get("Accept"); v != "" {

			req.Header.Set("Accept", v)

		} else if stream {

			req.Header.Set("Accept", "text/event-stream")

		} else {

			req.Header.Set("Accept", "application/json")

		}

		upstreamSentAt := time.Now()
		reqLog.Debug("proxy.upstream.request", "attempt", attempt+1, "url", url, "account_id", accountID)
		client := upstreamHTTPClient
		if settings.IsOpenCodeZen() && viaWarp && s.warp != nil {
			client = s.warp.HTTPClient(upstreamHTTPClient)
			reqLog.Debug("proxy.warp.route", "attempt", attempt+1, "socks", s.warp.Status()["socks"])
		}
		resp, err := client.Do(req)

		if err != nil {
			reqLog.Error("proxy.upstream.error", "attempt", attempt+1, "err", err.Error(), "latency_ms", time.Since(upstreamSentAt).Milliseconds())
			if settings.IsOpenCodeZen() && viaWarp && s.warp != nil && s.warp.Enabled() && warp.IsNetworkFailure(err) {
				s.warp.MarkInactive(err.Error())
				viaWarp = false
				maxAttempts = attempt + 2
				reqLog.Warn("proxy.warp.stale_state", "reason", "network")
				continue
			}
			if settings.IsOpenCodeZen() && !warpTried && s.warp != nil && s.warp.Enabled() && warp.IsNetworkFailure(err) {
				warpTried = true
				reqLog.Warn("proxy.warp.failover.trigger", "reason", "network")
				if activateErr := s.warp.Activate(r.Context()); activateErr == nil {
					viaWarp = true
					continue
				}
				reqLog.Warn("proxy.warp.failover.activation_failed", "reason", "network")
			}
			http.Error(w, err.Error(), http.StatusBadGateway)

			return

		}

		reqLog.Info("proxy.upstream.response", "attempt", attempt+1, "status", resp.StatusCode, "latency_ms", time.Since(upstreamSentAt).Milliseconds())
		// Read error body only when status is bad; keep stream body for SSE success path.

		if resp.StatusCode >= 400 {

			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

			_ = resp.Body.Close()

			reason := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(errBody))
			log.Printf("[AUDIT] upstream error raw: status=%d body=%q", resp.StatusCode, string(errBody))

			// OpenCode Go requires historical reasoning_content when thinking is
			// enabled. Some OpenAI-compatible clients retain the assistant text but
			// omit that internal field on a follow-up. It cannot be reconstructed,
			// so retry once with thinking disabled while preserving the conversation.
			if settings.IsOpenCodeGo() && !openCodeThinkingRetried && openCodeRequiresReasoningHistory(errBody) && m != nil {
				if _, configured := m["reasoning_effort"]; configured {
					delete(m, "reasoning_effort")
					delete(m, "reasoning")
					body, _ = json.Marshal(m)
					openCodeThinkingRetried = true
					reqLog.Warn("proxy.opencode_go.thinking_disabled", "reason", "missing_reasoning_history")
					continue
				}
			}

			if settings.IsOpenCodeZen() && !warpTried && s.warp != nil && s.warp.Enabled() && warp.ShouldFailover(resp.StatusCode, errBody) {
				warpTried = true
				reqLog.Warn("proxy.warp.failover.trigger", "reason", "upstream", "status", resp.StatusCode)
				if activateErr := s.warp.Activate(r.Context()); activateErr == nil {
					viaWarp = true
					reqLog.Info("proxy.warp.failover.retry", "status", resp.StatusCode)
					continue
				}
				reqLog.Warn("proxy.warp.failover.activation_failed", "status", resp.StatusCode)
			}

			// Quota first (Kimi billing 403 must not be treated as auth-denied).
			// Handler may block while re-login refills a Kimi account (HTTP or clean profile).
			quotaHit := isQuotaStatus(resp.StatusCode, errBody)
			log.Printf("[AUDIT] quota check: status=%d quotaHit=%v", resp.StatusCode, quotaHit)
			if quotaHit {
				if fn := s.quotaHandler(); fn != nil && accountID != "" {
					if rotated := fn(accountID, reason); rotated {
						tok2, acc2, _, err2 := s.ensure(ctx)
						// Retry on new account OR same id refilled (re-login / remint).
						if err2 == nil && tok2 != "" && (acc2 == nil || acc2.Usable()) {
							prev := accountID
							if !rotateCreds(tok2, acc2) {
								writeCrossProviderError(w, routeProv, acc2)
								return
							}
							reqLog.Warn("proxy.rotate", "from_account", prev, "to_account", accountID, "reason", "quota")
							continue
						}
					}
				} else if accountID != "" {
					_, _ = s.store.MarkExhausted(accountID, reason)
					if next := s.store.NextUsableAccountID(accountID); next != "" {
						_ = s.store.SetActiveAccount(next)
						tok2, acc2, _, err2 := s.ensure(ctx)
						if err2 == nil {
							if !rotateCreds(tok2, acc2) {
								writeCrossProviderError(w, routeProv, acc2)
								return
							}
							continue
						}
					}
				}
			}

			// Auth denial: force-refresh same account once, then rotate.
			// Skip permanent auth-denied when the error is clearly from the wrong
			// upstream (e.g. sk-kimi hit cli-chat-proxy / xAI JWT hit agent-gw).
			if isAuthStatus(resp.StatusCode, errBody) && accountID != "" && !isQuotaStatus(resp.StatusCode, errBody) {
				if isCrossProviderAuthMismatch(acc, settings, errBody) {
					reqLog.Warn("proxy.auth.cross_provider_mismatch", "account_id", accountID, "detail", truncateForLog(string(errBody), 180))
					// Fall through to return the upstream error without poisoning the account.
				} else {
					if !authRetried {
						authRetried = true
						if fr := s.forceRefreshFn(); fr != nil {
							tok2, acc2, _, err2 := fr(ctx, accountID)
							if settings.IsCodex() && err2 != nil && !codexauth.IsInvalidGrant(err2) {
								codexRefreshTransient = true
							}
							if err2 == nil && tok2 != "" && tok2 != token {
								if !rotateCreds(tok2, acc2) {
									writeCrossProviderError(w, routeProv, acc2)
									return
								}
								reqLog.Warn("proxy.auth.force_refresh", "account_id", accountID)
								continue
							}
						}
						// Also re-run ensure (pulls CLI sync) in case refresh path was unavailable.
						tok2, acc2, _, err2 := s.ensure(ctx)
						if err2 == nil && tok2 != "" && tok2 != token {
							if !rotateCreds(tok2, acc2) {
								writeCrossProviderError(w, routeProv, acc2)
								return
							}
							reqLog.Warn("proxy.auth.token_changed")
							continue
						}
					}

					// A transient token-endpoint failure must not permanently poison a
					// rotating Codex session. Return the original upstream auth error.
					if codexRefreshTransient {
						reqLog.Warn("codex.refresh.transient", "account_id", accountID)
					} else if fn := s.authFailHandler(); fn != nil {
						if rotated := fn(accountID, reason); rotated {
							tok2, acc2, _, err2 := s.ensure(ctx)
							if err2 == nil && (acc2 == nil || acc2.ID != accountID) {
								prev := accountID
								if !rotateCreds(tok2, acc2) {
									writeCrossProviderError(w, routeProv, acc2)
									return
								}
								reqLog.Warn("proxy.rotate", "from_account", prev, "to_account", accountID, "reason", "auth")
								continue
							}
						}
					} else {
						_, _ = s.store.MarkAuthDenied(accountID, reason)
						if next := s.store.NextUsableAccountID(accountID); next != "" {
							_ = s.store.SetActiveAccount(next)
							tok2, acc2, _, err2 := s.ensure(ctx)
							if err2 == nil {
								if !rotateCreds(tok2, acc2) {
									writeCrossProviderError(w, routeProv, acc2)
									return
								}
								continue
							}
						}
					}
				}
			}

			// Final error to client

			for k, vv := range resp.Header {

				if strings.EqualFold(k, "Content-Length") {

					continue

				}

				for _, v := range vv {

					w.Header().Add(k, v)

				}

			}

			clientStatus = resp.StatusCode
			w.Header().Set("Content-Type", "application/json")

			w.WriteHeader(resp.StatusCode)

			_, _ = w.Write(errBody)

			return

		}

		ct := strings.ToLower(resp.Header.Get("Content-Type"))

		// A JSON fallback from an upstream that ignored stream=true must not be
		// fed to the SSE pipe (it would silently discard the whole answer).
		// Only assume SSE when the provider omitted Content-Type entirely; a
		// declared application/json response is a normal non-stream fallback.
		isSSE := strings.Contains(ct, "text/event-stream") || ((stream || settings.IsCodex()) && ct == "")

		// Codex always returns SSE. Aggregate the terminal response object when
		// the downstream OpenAI client requested a regular JSON response.
		if settings.IsCodex() && isSSE && !stream {
			raw, err := completedResponseFromSSE(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				clientStatus = http.StatusBadGateway
				writeOpenAIError(w, http.StatusBadGateway, err.Error())
				return
			}
			accountID := ""
			if acc != nil {
				accountID = acc.ID
			}
			s.recordUsageFromJSONBody(raw, accountID, routeProv, resolvedModel, time.Since(startedAt).Milliseconds())
			w.Header().Set("Content-Type", "application/json")
			if wantClientChat && !wantClientResponses {
				out, convErr := responsesJSONToChatCompletion(raw, resolvedModel)
				if convErr != nil {
					clientStatus = http.StatusBadGateway
					writeOpenAIError(w, http.StatusBadGateway, convErr.Error())
					return
				}
				clientStatus = http.StatusOK
				_ = json.NewEncoder(w).Encode(out)
				return
			}
			clientStatus = http.StatusOK
			_, _ = w.Write(raw)
			return
		}

		// Ollie/Gemini: client asked for Responses but we hit chat/completions — translate wire format.
		if settings.IsOllie() && clientPath == "/responses" {
			if isSSE {
				if err := pipeOllieChatSSEToResponsesContext(r.Context(), w, resp.Body, resolvedModel); err != nil {
					log.Printf("proxyhttp ollie responses sse: %v", err)
				}
				_ = resp.Body.Close()
				return
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			_ = resp.Body.Close()
			out, err := chatCompletionJSONToResponse(raw, resolvedModel)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(resp.StatusCode)
				_, _ = w.Write(raw)
				return
			}
			accountID := ""
			if acc != nil {
				accountID = acc.ID
			}
			s.recordUsageFromJSONBody(raw, accountID, routeProv, resolvedModel, time.Since(startedAt).Milliseconds())
			clientStatus = 200
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(out)
			return
		}

		// Qwen (QwenBridge) / DeepSeek: client asked for Responses but we hit
		// chat/completions — translate wire format.
		if (settings.IsQwen() || settings.IsDeepSeek() || settings.IsOpenCodeZen()) && clientPath == "/responses" {
			accountID := ""
			if acc != nil {
				accountID = acc.ID
			}
			if isSSE {
				if err := pipeKimiChatSSEToResponsesContext(r.Context(), w, resp.Body, resolvedModel); err != nil {
					log.Printf("proxyhttp qwen responses sse: %v", err)
					s.handleStreamQuota(accountID, err)
				}
				_ = resp.Body.Close()
				return
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
			_ = resp.Body.Close()
			out, err := chatCompletionJSONToResponse(raw, resolvedModel)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(resp.StatusCode)
				_, _ = w.Write(raw)
				return
			}
			s.recordUsageFromJSONBody(raw, accountID, routeProv, resolvedModel, time.Since(startedAt).Milliseconds())
			clientStatus = 200
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(out)
			return
		}

		// Kimi Work: client asked for Responses but we hit chat/completions — translate wire format fully (reasoning + tools).
		if settings.IsKimiWork() && clientPath == "/responses" {
			accountID := ""
			if acc != nil {
				accountID = acc.ID
			}
			if isSSE {
				if err := pipeKimiChatSSEToResponsesContext(r.Context(), w, resp.Body, resolvedModel); err != nil {
					log.Printf("proxyhttp kimi responses sse: %v", err)
					s.handleStreamQuota(accountID, err)
				}
				_ = resp.Body.Close()
				return
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
			_ = resp.Body.Close()
			out, err := chatCompletionJSONToResponse(raw, resolvedModel)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(resp.StatusCode)
				_, _ = w.Write(raw)
				return
			}
			s.recordUsageFromJSONBody(raw, accountID, routeProv, resolvedModel, time.Since(startedAt).Milliseconds())
			clientStatus = 200
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(out)
			return
		}

		// Grok default: client used /chat/completions, upstream was /responses → translate back to chat.
		if (settings.IsXAI() || settings.IsCodex()) && wantClientChat && !wantClientResponses {
			accountID := ""
			if acc != nil {
				accountID = acc.ID
			}
			if isSSE {
				tee := newUsageTeeReader(resp.Body)
				if err := pipeResponsesSSEToChat(r.Context(), w, tee, resolvedModel); err != nil {
					log.Printf("proxyhttp grok responses→chat sse: %v", err)
					s.handleStreamQuota(accountID, err)
				}
				_ = resp.Body.Close()
				if len(tee.lastJSON) > 0 {
					s.recordUsageFromSSECapture(tee.lastJSON, accountID, routeProv, resolvedModel, time.Since(startedAt).Milliseconds())
				}
				return
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
			_ = resp.Body.Close()
			s.recordUsageFromJSONBody(raw, accountID, routeProv, resolvedModel, time.Since(startedAt).Milliseconds())
			out, err := responsesJSONToChatCompletion(raw, resolvedModel)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(resp.StatusCode)
				_, _ = w.Write(raw)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(out)
			return
		}

		if isSSE {

			for k, vv := range resp.Header {

				if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Type") {

					continue

				}

				for _, v := range vv {

					w.Header().Add(k, v)

				}

			}

			tee := newUsageTeeReader(resp.Body)

			if err := pipeSSE(r.Context(), w, tee); err != nil {

				log.Printf("proxyhttp sse: %v", err)

				streamAccountID := ""

				if acc != nil {

					streamAccountID = acc.ID

				}

				s.handleStreamQuota(streamAccountID, err)

			}

			_ = resp.Body.Close()

			accountID := ""

			if acc != nil {

				accountID = acc.ID

			}

			if len(tee.lastJSON) > 0 {

				s.recordUsageFromSSECapture(tee.lastJSON, accountID, routeProv, resolvedModel, time.Since(startedAt).Milliseconds())

			}

			clientStatus = 200

			return

		}

		// JSON success

		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))

		_ = resp.Body.Close()

		accountID := ""

		if acc != nil {

			accountID = acc.ID

		}

		s.recordUsageFromJSONBody(raw, accountID, routeProv, resolvedModel, time.Since(startedAt).Milliseconds())

		clientStatus = 200

		for k, vv := range resp.Header {

			if strings.EqualFold(k, "Content-Length") {

				continue

			}

			for _, v := range vv {

				w.Header().Add(k, v)

			}

		}

		w.WriteHeader(resp.StatusCode)

		_, _ = w.Write(raw)

		return

	}

}

// normalizeCodexReasoningEffort translates client UI labels to the Codex
// Responses API vocabulary. ZCode exposes "Extra high" as xhigh, while Codex
// accepts max for that tier (and uses the same value for legacy ultra).
func normalizeCodexReasoningEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "xhigh", "extra_high", "extra-high", "ultra":
		return "max"
	default:
		return effort
	}
}

// normalizeCodexResponsesInput handles clients that send the valid generic
// Responses shorthand `input: "..."`. The ChatGPT Codex upstream accepts only
// typed input-item lists, so turn that shorthand into its equivalent message.
func normalizeCodexResponsesInput(input any) any {
	text, ok := input.(string)
	if !ok {
		return input
	}
	return []any{map[string]any{
		"type": "message",
		"role": "user",
		"content": []any{map[string]any{
			"type": "input_text",
			"text": text,
		}},
	}}
}

// sanitizeKimiWorkChatBody rewrites HTTP chat bodies for agent-gw:
//   - real wire model ids (k3-agent / k2d6-agent / …), never force kimi-for-coding
//   - client thinking / reasoning_effort only (no global settings)
//   - Desktop shape: thinking: { type, effort?, keep? }
func sanitizeKimiWorkChatBody(m map[string]any) {
	if m == nil {
		return
	}
	orig, _ := m["model"].(string)
	// Resolve wire model from client id (strips effort suffix for model field).
	wire := store.Settings{Provider: store.ProviderKimiWork}.ResolveModelForClient(orig)
	if wire != "" {
		m["model"] = wire
	}

	// Prefer explicit client thinking object.
	if th, ok := m["thinking"].(map[string]any); ok && th != nil {
		delete(m, "reasoning_effort")
		return
	}

	// Effort from: reasoning_effort | reasoning.effort | model alias suffix.
	effort := ""
	if v, ok := m["reasoning_effort"].(string); ok {
		effort = strings.TrimSpace(v)
	}
	if effort == "" {
		if r, ok := m["reasoning"].(map[string]any); ok {
			if v, ok := r["effort"].(string); ok {
				effort = strings.TrimSpace(v)
			}
		}
	}
	if effort == "" {
		_, effort = store.ExtractKimiWorkEffort(orig)
	}

	eff := strings.ToLower(strings.TrimSpace(effort))
	delete(m, "reasoning_effort")
	delete(m, "reasoning") // Responses-style object invalid on chat/completions

	switch eff {
	case "":
		// Client omitted — do not invent settings.ReasoningEffort.
		return
	case "off", "none", "disabled":
		m["thinking"] = map[string]any{"type": "disabled"}
	case "low", "medium", "high", "xhigh", "max", "minimal", "extra_high", "extra-high":
		if eff == "xhigh" || eff == "extra_high" || eff == "extra-high" {
			eff = "max"
		}
		if eff == "minimal" {
			eff = "low"
		}
		m["thinking"] = map[string]any{
			"type":   "enabled",
			"effort": eff,
			"keep":   "all",
		}
	default:
		m["thinking"] = map[string]any{"type": "enabled", "keep": "all"}
	}
}

// clampKimiContextLimits enforces the upstream token ceiling Kimi actually accepts.

// Both K3 and K2.6 are 262_144 tokens. We clamp max_tokens to leave headroom

// for the system prompt / tool history already in the messages.

func clampKimiContextLimits(m map[string]any, model string) map[string]any {

	if m == nil {

		return m

	}

	// Kimi hard ceiling: both K3 and K2.6 are 262_144 tokens.

	// Leave 10 % headroom for prompts, tools, reasoning.

	ceiling := 65536

	cur := 0

	switch v := m["max_tokens"].(type) {

	case float64:

		cur = int(v)

	case int:

		cur = v

	case int64:

		cur = int(v)

	case json.Number:

		i, _ := v.Int64()

		cur = int(i)

	}

	if cur == 0 || cur > ceiling {

		m["max_tokens"] = ceiling

	}

	return m

}

// isKimiCapacityBusy detects Kimi Work / Desktop overload that cannot be fixed by rotating accounts.

// Example: "Too many people are chatting with Kimi right now. Please try again soon."

func isKimiCapacityBusy(code int, body []byte) bool {

	low := strings.ToLower(string(body))
	result := false

	if strings.Contains(low, "too many people are chatting with kimi") {
		result = true
	}

	if !result && strings.Contains(low, "too many people are chatting") && strings.Contains(low, "kimi") {
		result = true
	}

	if !result && strings.Contains(low, "please try again soon") && strings.Contains(low, "kimi") {
		result = true
	}

	// Some gateways return 429/503 with generic capacity wording.

	if !result && (code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable) &&
		(strings.Contains(low, "too many people") ||
			strings.Contains(low, "server is busy") ||
			strings.Contains(low, "capacity") && strings.Contains(low, "kimi") ||
			strings.Contains(low, "overloaded")) {
		result = true
	}

	log.Printf("[AUDIT] isKimiCapacityBusy: code=%d result=%v bodySnippet=%q", code, result, truncateForLog(string(body), 300))
	return result

}

func writeKimiCapacityError(w http.ResponseWriter, upstreamBody []byte) {

	msg := "Too many people are chatting with Kimi right now. Please try again soon."

	// Prefer upstream message when present.

	if m, ok := extractUpstreamMessage(upstreamBody); ok && m != "" {

		msg = m

	}

	log.Printf("[AUDIT] writeKimiCapacityError: returning 503 to client with upstreamMsg=%q", msg)

	w.Header().Set("Content-Type", "application/json")

	w.Header().Set("Retry-After", "30")

	w.WriteHeader(http.StatusServiceUnavailable)

	_ = json.NewEncoder(w).Encode(map[string]any{

		"error": map[string]any{

			"message": msg,

			"type": "kimi_capacity_error",

			"code": "kimi_server_busy",

			"retryable": true,

			// Explicit: not an auth/account problem — do not rotate keys or re-mint.

			"hint": "Kimi upstream servers are overloaded. Wait and retry; rotating accounts will not help.",
		},
	})

}

func extractUpstreamMessage(body []byte) (string, bool) {

	var m map[string]any

	if json.Unmarshal(body, &m) != nil {

		// plain text body

		s := strings.TrimSpace(string(body))

		if s != "" && len(s) < 500 {

			return s, true

		}

		return "", false

	}

	if errObj, ok := m["error"].(map[string]any); ok {

		if msg, ok := errObj["message"].(string); ok && msg != "" {

			return msg, true

		}

	}

	if msg, ok := m["message"].(string); ok && msg != "" {

		return msg, true

	}

	return "", false

}

func isQuotaStatus(code int, body []byte) bool {
	result := false

	if code == http.StatusPaymentRequired { // 402
		result = true
	}

	low := strings.ToLower(string(body))
	// Kimi capacity is handled through the same account lifecycle as quota:
	// remote logoff, pool rotation, and automatic re-login.
	if strings.Contains(low, "too many people are chatting with kimi") {
		result = true
	}

	if !result && (strings.Contains(low, "usage balance exhausted") || strings.Contains(low, "balance exhausted") || strings.Contains(low, "credit exhausted") || strings.Contains(low, "no remaining quota") || strings.Contains(low, "diamond") && strings.Contains(low, "insufficient")) {
		result = true
	}

	// Kimi Work billing / plan cap (often HTTP 403 with access_terminated / resource_exhausted).
	if !result && (strings.Contains(low, "usage limit") ||
		strings.Contains(low, "billing cycle") ||
		strings.Contains(low, "resource_exhausted") ||
		strings.Contains(low, "access_terminated") ||
		strings.Contains(low, "rate limit exceeded") && (strings.Contains(low, "quota") || strings.Contains(low, "usage") || strings.Contains(low, "billing")) ||
		strings.Contains(low, "upgrade to get more")) {
		result = true
	}

	if !result && code == http.StatusTooManyRequests && (strings.Contains(low, "exhausted") || strings.Contains(low, "quota") || strings.Contains(low, "balance") || strings.Contains(low, "rate limit")) {
		result = true
	}

	// Kimi sometimes returns 403 (not 429) for plan exhaustion.
	if !result && code == http.StatusForbidden && (strings.Contains(low, "usage limit") ||
		strings.Contains(low, "resource_exhausted") ||
		strings.Contains(low, "access_terminated") ||
		strings.Contains(low, "billing cycle") ||
		strings.Contains(low, "credit") || strings.Contains(low, "quota") || strings.Contains(low, "diamond")) {
		result = true
	}

	log.Printf("[AUDIT] isQuotaStatus: code=%d result=%v bodySnippet=%q", code, result, truncateForLog(string(body), 300))
	return result
}

func isQuotaErrorPayload(payload map[string]any) (bool, string) {
	errObj, ok := payload["error"].(map[string]any)
	if !ok {
		// Some providers emit a top-level error as a string
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
	if strings.Contains(low, "too many people are chatting with kimi") ||
		strings.Contains(low, "usage limit") ||
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

// isAuthStatus detects upstream auth failures that should force-refresh / rotate accounts.

func isAuthStatus(code int, body []byte) bool {

	if code == http.StatusUnauthorized {

		return true

	}

	low := strings.ToLower(string(body))

	if code == http.StatusForbidden {

		if strings.Contains(low, "permission-denied") ||

			strings.Contains(low, "access to the chat endpoint is denied") ||

			strings.Contains(low, "invalid or expired credentials") ||

			strings.Contains(low, "no auth context") {

			return true

		}

		// Generic 403 from cli-chat-proxy with structured JSON code field.

		if strings.Contains(low, `"code":"permission-denied"`) || strings.Contains(low, `"code": "permission-denied"`) {

			return true

		}

	}

	if strings.Contains(low, "invalid or expired credentials") || strings.Contains(low, "no auth context") {

		return true

	}

	return false

}

func openCodeRequiresReasoningHistory(body []byte) bool {
	message := strings.ToLower(string(body))
	return strings.Contains(message, "reasoning_content") &&
		(strings.Contains(message, "must be passed back") || strings.Contains(message, "thinking mode"))
}

// isCrossProviderAuthMismatch reports that an auth error is almost certainly from
// sending the wrong credential type to the wrong upstream (must not MarkAuthDenied).
// Example we hit: Kimi sk-kimi against cli-chat-proxy.grok.com →
//
//	"x_xai_token_auth=none ... no auth context"
func isCrossProviderAuthMismatch(acc *store.Account, settings store.Settings, body []byte) bool {
	low := strings.ToLower(string(body))
	up := strings.ToLower(settings.EffectiveUpstream())
	provAcc := ""
	if acc != nil {
		provAcc = acc.NormalizedProvider()
	}
	// xAI gateway rejecting non-JWT / non-xAI bearer
	xaiishErr := strings.Contains(low, "x_xai_token_auth") ||
		strings.Contains(low, "xai_token") ||
		(strings.Contains(low, "no auth context") && strings.Contains(low, "auth_kind=bearer"))
	kimiCred := provAcc == store.ProviderKimiWork ||
		(acc != nil && (strings.HasPrefix(acc.BearerToken(), "sk-kimi-") || strings.HasPrefix(acc.APIKey, "sk-kimi-")))
	xaiUpstream := strings.Contains(up, "cli-chat-proxy") || strings.Contains(up, "grok.com") || settings.IsXAI()
	if kimiCred && xaiUpstream && xaiishErr {
		return true
	}
	// Kimi gateway rejecting an xAI JWT
	if provAcc == store.ProviderXAI && (settings.IsKimiWork() || strings.Contains(up, "agent-gw") || strings.Contains(up, "kimi.com")) {
		if strings.Contains(low, "invalid") || strings.Contains(low, "unauthorized") || strings.Contains(low, "unauthenticated") {
			return true
		}
	}
	return false
}

// writeCrossProviderError fails the request when account rotation landed on a
// different provider than the model route pinned (a Kimi-shaped body would go
// to the xAI upstream with an xAI token, or vice versa).
func writeCrossProviderError(w http.ResponseWriter, routeProv string, acc *store.Account) {
	got := "nil"
	if acc != nil {
		got = acc.NormalizedProvider()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": "account rotation returned provider " + got + " for route " + routeProv + " — refusing cross-provider retry",
			"type":    "server_error",
			"code":    "cross_provider_rotation",
		},
	})
}

func setUpstreamAuthHeaders(req *http.Request, token string, settings store.Settings) {

	version := settings.ClientVersion

	if version == "" {

		version = store.DefaultClientVersion

	}

	if token == "" && settings.IsOllie() {

		token = store.OllieAPIKey

	}

	if token == "" && settings.IsQwen() {

		token = strings.TrimSpace(settings.QwenAPIKey)

	}

	if token == "" && settings.IsDeepSeek() {

		token = settings.DeepSeekAPIKeyPlain()

	}

	if token == "" && settings.IsOpenCodeZen() {

		token = store.OpenCodeZenAPIKey

	}

	if token == "" && settings.IsOpenCodeGo() {

		token = settings.OpenCodeGoAPIKeyPlain()

	}

	req.Header.Set("Authorization", "Bearer "+token)

	req.Header.Set("Content-Type", "application/json")
	if settings.IsCodex() {
		req.Header.Set("ChatGPT-Account-ID", settings.CodexAccountID)
		req.Header.Set("originator", "codex_cli_rs")
		req.Header.Set("version", store.CodexClientVersion)
		req.Header.Set("User-Agent", "codex_cli_rs/"+store.CodexClientVersion)
		if settings.CodexFedRAMP {
			req.Header.Set("X-OpenAI-Fedramp", "true")
		}
		return
	}

	// QwenBridge / DeepSeek: Authorization + Content-Type only — no provider-specific headers.
	if settings.IsQwen() || settings.IsDeepSeek() || settings.IsOpenCodeZen() || settings.IsOpenCodeGo() {

		return

	}

	if settings.IsOllie() {

		req.Header.Set("User-Agent", "grok-desktop-ollie/"+version)

		return

	}

	if settings.IsKimiWork() {

		req.Header.Set("User-Agent", store.KimiWorkUserAgent)

		req.Header.Set("X-Msh-Platform", "kimi-code-cli")

		req.Header.Set("X-Msh-Version", "0.23.5")

		return

	}

	req.Header.Set("x-grok-client-version", version)
	req.Header.Set("x-grok-client-identifier", store.DefaultClientIdentifier)
	req.Header.Set("x-grok-client-surface", store.DefaultClientSurface)
	req.Header.Set("User-Agent", "grok/"+version)
}

// chatCompletionsBodyToResponses converts OpenAI chat/completions body into /v1/responses
// so OpenCode/Kilo (which only call chat/completions) work with Grok/xAI.
func chatCompletionsBodyToResponses(m map[string]any, settings store.Settings) map[string]any {
	out := map[string]any{}
	if model, ok := m["model"].(string); ok && model != "" {
		out["model"] = settings.ResolveModelForClient(model)
	} else {
		out["model"] = settings.ResolveModelForClient("")
	}
	if v, ok := m["stream"].(bool); ok {
		out["stream"] = v
	}
	if v, ok := m["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := m["top_p"]; ok {
		out["top_p"] = v
	}
	if v, ok := m["max_tokens"]; ok {
		out["max_output_tokens"] = v
	} else if v, ok := m["max_completion_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	// Client-provided effort only — no global settings fallback on HTTP path.
	if v, ok := m["reasoning_effort"].(string); ok && strings.TrimSpace(v) != "" {
		out["reasoning"] = map[string]any{"effort": v, "summary": "auto"}
	}
	// tools: chat function tools → responses tools when possible
	if tools, ok := m["tools"]; ok {
		if sanitized := sanitizeResponsesTools(tools); len(sanitized) > 0 {
			out["tools"] = sanitized
			if tc, ok := m["tool_choice"]; ok {
				out["tool_choice"] = tc
			} else {
				out["tool_choice"] = "auto"
			}
		}
	}
	// messages → input (Responses)
	if msgs, ok := m["messages"].([]any); ok && len(msgs) > 0 {
		input := make([]any, 0, len(msgs))
		var instructions strings.Builder
		for _, raw := range msgs {
			msg, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			content := flattenResponsesContent(msg["content"])
			if s, ok := msg["content"].(string); ok && content == "" {
				content = s
			}
			role = strings.ToLower(strings.TrimSpace(role))
			if role == "system" || role == "developer" {
				if instructions.Len() > 0 {
					instructions.WriteString("\n\n")
				}
				instructions.WriteString(content)
				continue
			}
			if role == "" {
				role = "user"
			}
			// tool result
			if role == "tool" {
				item := map[string]any{
					"type":   "function_call_output",
					"output": content,
				}
				if id, ok := msg["tool_call_id"].(string); ok && id != "" {
					item["call_id"] = id
				}
				input = append(input, item)
				continue
			}
			if role == "assistant" {
				if reasoningItems, ok := msg["reasoning_items"].([]any); ok {
					for _, rawItem := range reasoningItems {
						if item, ok := rawItem.(map[string]any); ok && asString(item["type"]) == "reasoning" && asString(item["encrypted_content"]) != "" {
							input = append(input, item)
						}
					}
				}
			}
			// assistant with tool_calls
			if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
				for _, tc := range tcs {
					tcm, ok := tc.(map[string]any)
					if !ok {
						continue
					}
					sanitized := sanitizeToolCall(tcm)
					if sanitized == nil {
						continue
					}
					fn, _ := sanitized["function"].(map[string]any)
					name := asString(fn["name"])
					args := asString(fn["arguments"])
					callID := asString(sanitized["id"])
					item := map[string]any{
						"type":      "function_call",
						"name":      name,
						"arguments": args,
					}
					if callID != "" {
						item["call_id"] = callID
					}
					input = append(input, item)
				}
				if content != "" {
					input = append(input, map[string]any{
						"type": "message",
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "output_text", "text": content},
						},
					})
				}
				continue
			}
			ctype := "input_text"
			if role == "assistant" {
				ctype = "output_text"
			}
			input = append(input, map[string]any{
				"type": "message",
				"role": role,
				"content": []any{
					map[string]any{"type": ctype, "text": content},
				},
			})
		}
		if instructions.Len() > 0 {
			out["instructions"] = instructions.String()
		}
		if len(input) == 0 {
			out["input"] = "Hello"
		} else {
			out["input"] = input
		}
	} else if s, ok := m["input"].(string); ok && s != "" {
		out["input"] = s
	} else {
		out["input"] = "Hello"
	}
	if settings.StoreResponses {
		out["store"] = true
	}
	return out
}

// responsesBodyToChatCompletions converts an OpenAI Responses body into chat/completions

// so clients that only speak /v1/responses still work against OllieChat / Gemini.

// Model is assumed already resolved by the caller (Codex vs client policy).

func responsesBodyToChatCompletions(m map[string]any, settings store.Settings) map[string]any {

	out := map[string]any{}

	if model, ok := m["model"].(string); ok && model != "" {

		// Prefer already-resolved id; only normalize aliases/suffixes without re-forcing.

		out["model"] = settings.ResolveModelForClient(model)

	} else {

		out["model"] = settings.ResolveModelForClient("")

	}

	if v, ok := m["stream"].(bool); ok {

		out["stream"] = v

	}

	if v, ok := m["temperature"]; ok {

		out["temperature"] = v

	}

	if v, ok := m["top_p"]; ok {

		out["top_p"] = v

	}

	if v, ok := m["max_output_tokens"]; ok {

		out["max_tokens"] = v

	} else if v, ok := m["max_tokens"]; ok {

		out["max_tokens"] = v

	}

	// Only forward client-set effort (never invent global xhigh — empties free-model answers).

	if v, ok := m["reasoning_effort"].(string); ok && strings.TrimSpace(v) != "" {

		out["reasoning_effort"] = v

	} else if r, ok := m["reasoning"].(map[string]any); ok {

		// Codex sends reasoning: { effort, summary } on /v1/responses.

		if v, ok := r["effort"].(string); ok && strings.TrimSpace(v) != "" {

			out["reasoning_effort"] = v

		}

	}

	// tools: Responses-style tools → chat tools when possible; drop native xAI search types.

	if tools, ok := m["tools"]; ok {

		if sanitized := sanitizeChatTools(tools); len(sanitized) > 0 {

			out["tools"] = sanitized

			if tc, ok := m["tool_choice"]; ok {

				out["tool_choice"] = tc

			}

		}

	}

	msgs := responsesInputToChatMessages(m["input"])

	// Codex system prompt lives in top-level instructions — must become a system message.

	if instr, ok := m["instructions"].(string); ok && strings.TrimSpace(instr) != "" {

		content := strings.TrimSpace(instr)

		// When tools are available, append a concise nudge that encourages the model

		// to actually USE them rather than reasoning indefinitely.  Fable-5 / Claude

		// reasoning models on OllieChat tend to think at length without acting; this

		// helps break the reasoning-only loop in Codex agent sessions.

		if _, hasTools := out["tools"]; hasTools {

			content += "\n\nIMPORTANT: You have tools available. Use them directly to accomplish tasks. " +

				"Keep your reasoning concise and act promptly. Do not reason at length without taking action. " +

				"If you know which tool to call, call it immediately rather than planning extensively."

		}

		sys := map[string]any{"role": "system", "content": content}

		msgs = append([]any{sys}, msgs...)

	}

	if len(msgs) == 0 {

		msgs = []any{map[string]any{"role": "user", "content": "Hello"}}

	}

	out["messages"] = msgs

	if settings.IsOllie() {

		sanitizeOllieChatBody(out)

	}

	return out

}

// sanitizeOllieChatBody makes chat/completions bodies safe for the free OllieChat gateway.

//

// We do NOT clamp reasoning effort or impose a low max_tokens ceiling.  The reasoning

// loop was caused by (1) reasoning text leaking into message content, (2) finish_reason

// "length" being reported as "completed", and (3) max_tokens floors being too low

// (1024-2048).  All three are now fixed elsewhere.  Here we just:

//   - strip Responses-only fields that chat/completions rejects

//   - clamp xhigh→high (gateway doesn't support xhigh)

//   - set a high default max_tokens ONLY when the client sent nothing at all

//

// If the client (Codex) sends its own max_tokens, we respect it as-is — no floor,

// no ceiling.  The model thinks as much as it wants.

func sanitizeOllieChatBody(m map[string]any) {

	if m == nil {

		return

	}

	delete(m, "store")

	delete(m, "previous_response_id")

	delete(m, "last_response_id")

	delete(m, "reasoning") // Responses-style object; not valid on chat/completions

	// Clamp xhigh → high (the gateway rejects xhigh). Everything else stays as-is.

	if re, ok := m["reasoning_effort"].(string); ok {

		switch strings.ToLower(strings.TrimSpace(re)) {

		case "xhigh", "extra_high", "extra-high", "max", "maximum":

			m["reasoning_effort"] = "high"

		case "", "none", "off", "minimal":

			delete(m, "reasoning_effort")

		}

	}

	// Only set a default when the client sent NOTHING.  If the client sent a value

	// (high or low) we respect it — no floor, no ceiling, no interference.

	if _, ok := m["max_tokens"]; !ok {

		if _, ok2 := m["max_completion_tokens"]; !ok2 {

			m["max_tokens"] = 65536

		}

	}

}

func ensureMinMaxTokens(m map[string]any, min int) {

	if min <= 0 {

		return

	}

	cur := 0

	switch v := m["max_tokens"].(type) {

	case float64:

		cur = int(v)

	case int:

		cur = v

	case int64:

		cur = int(v)

	case json.Number:

		i, _ := v.Int64()

		cur = int(i)

	}

	if cur < min {

		m["max_tokens"] = min

	}

}

func responsesInputToChatMessages(input any) []any {

	switch v := input.(type) {

	case string:

		if strings.TrimSpace(v) == "" {

			return nil

		}

		return []any{map[string]any{"role": "user", "content": v}}

	case []any:

		out := make([]any, 0, len(v))

		for _, raw := range v {

			item, ok := raw.(map[string]any)

			if !ok {

				continue

			}

			// Already a chat message shape (possibly with tool_calls).

			if role, _ := item["role"].(string); role != "" {

				role = strings.ToLower(role)

				msg := map[string]any{"role": role}

				content := flattenResponsesContent(item["content"])

				if content == "" {

					if s, ok := item["content"].(string); ok {

						content = s

					}

				}

				// Assistant tool_calls-only turns have empty content — still keep them.

				if tcs, ok := item["tool_calls"]; ok {

					if rawList, ok := tcs.([]any); ok {

						msg["tool_calls"] = sanitizeToolCallsList(rawList)

					} else {

						msg["tool_calls"] = tcs

					}

					if content != "" {

						msg["content"] = content

					} else {

						msg["content"] = nil

					}

					out = append(out, msg)

					continue

				}

				if role == "tool" {

					if id, ok := item["tool_call_id"].(string); ok && id != "" {

						msg["tool_call_id"] = id

					}

					msg["content"] = content

					out = append(out, msg)

					continue

				}

				if content == "" {

					continue

				}

				if role == "system" || role == "developer" || role == "assistant" {

					// keep

				} else {

					role = "user"

					msg["role"] = role

				}

				msg["content"] = content

				out = append(out, msg)

				continue

			}

			// Responses typed items (Codex multi-turn / tools).

			typ, _ := item["type"].(string)

			switch typ {

			case "message", "":

				role, _ := item["role"].(string)

				if role == "" {

					role = "user"

				}

				content := flattenResponsesContent(item["content"])

				if content == "" {

					continue

				}

				out = append(out, map[string]any{"role": role, "content": content})

			case "function_call", "custom_tool_call":

				// → assistant message with tool_calls

				callID, _ := item["call_id"].(string)

				if callID == "" {

					callID, _ = item["id"].(string)

				}

				name, _ := item["name"].(string)

				args := ""

				switch a := item["arguments"].(type) {

				case string:

					args = a

				default:

					if item["arguments"] != nil {

						b, _ := json.Marshal(item["arguments"])

						args = string(b)

					}

				}

				if callID == "" {

					callID = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]

				}

				out = append(out, map[string]any{

					"role": "assistant",

					"content": nil,

					"tool_calls": sanitizeToolCallsList([]any{

						map[string]any{

							"id": callID,

							"type": "function",

							"function": map[string]any{

								"name": name,

								"arguments": args,
							},
						},
					}),
				})

			case "function_call_output", "custom_tool_call_output":

				callID, _ := item["call_id"].(string)

				if callID == "" {

					callID, _ = item["id"].(string)

				}

				outText := ""

				switch o := item["output"].(type) {

				case string:

					outText = o

				default:

					if item["output"] != nil {

						b, _ := json.Marshal(item["output"])

						outText = string(b)

					}

				}

				if outText == "" {

					outText = flattenResponsesContent(item["content"])

				}

				msg := map[string]any{"role": "tool", "content": outText}

				if callID != "" {

					msg["tool_call_id"] = callID

				}

				out = append(out, msg)

			case "reasoning":

				// Skip encrypted/internal reasoning history for chat backends.

				continue

			}

		}

		return out

	case map[string]any:

		// single object input

		return responsesInputToChatMessages([]any{v})

	default:

		return nil

	}

}

func flattenResponsesContent(content any) string {

	switch c := content.(type) {

	case string:

		return c

	case []any:

		var b strings.Builder

		for _, p := range c {

			part, ok := p.(map[string]any)

			if !ok {

				if s, ok := p.(string); ok {

					b.WriteString(s)

				}

				continue

			}

			if t, ok := part["text"].(string); ok {

				b.WriteString(t)

				continue

			}

			// input_text / output_text

			if t, ok := part["type"].(string); ok && (t == "input_text" || t == "output_text" || t == "text") {

				if tx, ok := part["text"].(string); ok {

					b.WriteString(tx)

				}

			}

		}

		return b.String()

	default:

		return ""

	}

}

func injectTemporalIntoResponsesInput(input []any) []any {

	// Prepend a system-like user note if nothing temporal yet

	line := "Temporal context: today is " + time.Now().Format("Monday, January 2, 2006") +

		". The current year is " + strconv.Itoa(time.Now().Year()) + "."

	// Check first item

	if len(input) > 0 {

		if m, ok := input[0].(map[string]any); ok {

			blob, _ := json.Marshal(m)

			if strings.Contains(string(blob), "Temporal context:") {

				return input

			}

		}

	}

	// Use full message shape + input_text (xAI ModelInput-safe)

	sys := map[string]any{

		"type": "message",

		"role": "system",

		"content": []any{

			map[string]any{"type": "input_text", "text": line},
		},
	}

	return append([]any{sys}, input...)

}

// chatCompletionJSONToResponse maps a non-stream chat.completion JSON body to a Responses-like object.
func chatCompletionJSONToResponse(raw []byte, model string) (map[string]any, error) {
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	text := ""
	var toolCalls []any
	if choices, ok := in["choices"].([]any); ok && len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			if msg, ok := ch["message"].(map[string]any); ok {
				if c, ok := msg["content"].(string); ok {
					text = c
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					toolCalls = tcs
				}
			}
		}
	}
	if model == "" {
		if m, ok := in["model"].(string); ok {
			model = m
		}
	}
	output := make([]any, 0, 1+len(toolCalls))
	if text != "" || len(toolCalls) == 0 {
		output = append(output, map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": text},
			},
		})
	}
	for _, rawTC := range toolCalls {
		tcm, ok := rawTC.(map[string]any)
		if !ok {
			continue
		}
		sanitized := sanitizeToolCall(tcm)
		if sanitized == nil {
			continue
		}
		fn, _ := sanitized["function"].(map[string]any)
		name := asString(fn["name"])
		if name == "" {
			continue
		}
		args := asString(fn["arguments"])
		if args == "" {
			args = "{}"
		}
		callID := asString(sanitized["id"])
		if callID == "" {
			callID = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
		}
		output = append(output, map[string]any{
			"type":      "function_call",
			"call_id":   callID,
			"name":      name,
			"arguments": args,
			"status":    "completed",
		})
	}
	status := "completed"
	out := map[string]any{
		"id":     in["id"],
		"object": "response",
		"model":  model,
		"status": status,
		"output": output,
	}
	if u, ok := in["usage"]; ok {
		out["usage"] = normalizeUsageToResponses(u)
	}
	return out, nil
}

// responsesJSONToChatCompletion maps xAI /responses JSON → OpenAI chat.completion.
func responsesJSONToChatCompletion(raw []byte, model string) (map[string]any, error) {
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	if model == "" {
		if m, ok := in["model"].(string); ok {
			model = m
		}
	}
	id, _ := in["id"].(string)
	if id == "" {
		id = "chatcmpl-" + uuid.NewString()
	}
	text := extractResponsesOutputText(in)
	toolCalls := extractResponsesFunctionCalls(in)
	created := time.Now().Unix()
	if c, ok := in["created_at"].(float64); ok {
		created = int64(c)
	}
	msg := map[string]any{
		"role": "assistant",
	}
	if reasoningItems := responsesReasoningItems(in); len(reasoningItems) > 0 {
		msg["reasoning_items"] = reasoningItems
	}
	finish := "stop"
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		// OpenAI: content is null when the turn is tool-only; keep text if present.
		if text == "" {
			msg["content"] = nil
		} else {
			msg["content"] = text
		}
		finish = "tool_calls"
	} else {
		msg["content"] = text
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       msg,
				"finish_reason": finish,
			},
		},
	}
	if u, ok := in["usage"]; ok {
		out["usage"] = normalizeUsageToChat(u)
	}
	return out, nil
}

func extractResponsesOutputText(in map[string]any) string {
	var b strings.Builder
	// output_text shortcut
	if t, ok := in["output_text"].(string); ok && t != "" {
		return t
	}
	out, _ := in["output"].([]any)
	for _, raw := range out {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := item["type"].(string)
		if typ == "message" || typ == "" {
			if content, ok := item["content"].([]any); ok {
				for _, c := range content {
					part, ok := c.(map[string]any)
					if !ok {
						continue
					}
					pt, _ := part["type"].(string)
					if pt == "output_text" || pt == "text" {
						if t, ok := part["text"].(string); ok {
							b.WriteString(t)
						}
					}
				}
			}
			if t, ok := item["text"].(string); ok {
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

// extractResponsesFunctionCalls pulls client function_call items from Responses output.
// Server-side tools (web_search, x_search, reasoning) are ignored.
func extractResponsesFunctionCalls(in map[string]any) []any {
	out, _ := in["output"].([]any)
	if len(out) == 0 {
		return nil
	}
	calls := make([]any, 0)
	for _, raw := range out {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if tc := responsesItemToChatToolCall(item); tc != nil {
			calls = append(calls, tc)
		}
	}
	return calls
}

func responsesItemToChatToolCall(item map[string]any) map[string]any {
	typ := strings.ToLower(strings.TrimSpace(asString(item["type"])))
	if typ != "function_call" && typ != "custom_tool_call" {
		return nil
	}
	name := asString(item["name"])
	if name == "" {
		return nil
	}
	callID := asString(item["call_id"])
	if callID == "" {
		callID = asString(item["id"])
	}
	if callID == "" {
		callID = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	}
	args := ""
	if a, ok := item["arguments"].(string); ok {
		args = a
	} else if item["arguments"] != nil {
		b, _ := json.Marshal(item["arguments"])
		args = string(b)
	} else if in, ok := item["input"].(string); ok {
		args = in
	} else if item["input"] != nil {
		b, _ := json.Marshal(item["input"])
		args = string(b)
	}
	if args == "" {
		args = "{}"
	}
	return sanitizeToolCall(map[string]any{
		"id":   callID,
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": args,
		},
	})
}

func normalizeUsageToChat(u any) map[string]any {
	m, ok := u.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	out := map[string]any{}
	// Responses uses input_tokens / output_tokens; chat uses prompt/completion.
	if v, ok := m["prompt_tokens"]; ok {
		out["prompt_tokens"] = v
	} else if v, ok := m["input_tokens"]; ok {
		out["prompt_tokens"] = v
	}
	if v, ok := m["completion_tokens"]; ok {
		out["completion_tokens"] = v
	} else if v, ok := m["output_tokens"]; ok {
		out["completion_tokens"] = v
	}
	if v, ok := m["total_tokens"]; ok {
		out["total_tokens"] = v
	} else {
		pt, _ := out["prompt_tokens"].(float64)
		ct, _ := out["completion_tokens"].(float64)
		if pt == 0 {
			if i, ok := out["prompt_tokens"].(int); ok {
				pt = float64(i)
			}
		}
		if ct == 0 {
			if i, ok := out["completion_tokens"].(int); ok {
				ct = float64(i)
			}
		}
		out["total_tokens"] = pt + ct
	}
	return out
}

// pipeResponsesSSEToChat translates xAI Responses SSE into OpenAI chat.completion.chunk SSE.
// Forwards output_text and client function_call items as OpenAI tool_calls so agents (Kilo, etc.) execute tools.
func pipeResponsesSSEToChat(ctx context.Context, w http.ResponseWriter, body io.Reader, model string) error {
	flusher, _ := w.(http.Flusher)
	setSSEHeaders(w)
	w.WriteHeader(http.StatusOK)

	id := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	writeChatDelta := func(delta map[string]any, finish any) {
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{
				map[string]any{
					"index":         0,
					"delta":         delta,
					"finish_reason": finish,
				},
			},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	writeChatDelta(map[string]any{"role": "assistant"}, nil)

	toolIndexByKey := map[string]int{}
	argDeltaSeen := map[string]bool{}
	seenReasoning := map[string]bool{}
	nextToolIdx := 0
	sawToolCall := false
	emitReasoning := func(item map[string]any) {
		if asString(item["type"]) != "reasoning" || asString(item["encrypted_content"]) == "" {
			return
		}
		key := firstNonEmpty(asString(item["id"]), asString(item["encrypted_content"]))
		if seenReasoning[key] {
			return
		}
		seenReasoning[key] = true
		writeChatDelta(map[string]any{"reasoning_items": []any{item}}, nil)
	}

	resolveToolIdx := func(keys ...string) (int, bool) {
		for _, k := range keys {
			if k == "" {
				continue
			}
			if i, ok := toolIndexByKey[k]; ok {
				return i, true
			}
		}
		return -1, false
	}
	markToolKeys := func(idx int, keys ...string) {
		for _, k := range keys {
			if k != "" {
				toolIndexByKey[k] = idx
			}
		}
	}
	markArgSeen := func(keys ...string) {
		for _, k := range keys {
			if k != "" {
				argDeltaSeen[k] = true
			}
		}
	}
	anyArgSeen := func(keys ...string) bool {
		for _, k := range keys {
			if k != "" && argDeltaSeen[k] {
				return true
			}
		}
		return false
	}

	// allowFullArgs: true on output_item.done / completed harvest; false on .added (args often empty yet).
	emitToolCallStart := func(item map[string]any, allowFullArgs bool) {
		tc := responsesItemToChatToolCall(item)
		if tc == nil {
			return
		}
		callID := asString(tc["id"])
		itemID := asString(item["id"])
		fn, _ := tc["function"].(map[string]any)
		name := asString(fn["name"])
		args := asString(fn["arguments"])
		if !allowFullArgs {
			args = ""
		} else if args == "{}" {
			// keep "{}" only when this is the sole source of args
		}
		if idx, ok := resolveToolIdx(itemID, callID); ok {
			if allowFullArgs && args != "" && !anyArgSeen(itemID, callID) {
				writeChatDelta(map[string]any{
					"tool_calls": []any{
						map[string]any{
							"index":    idx,
							"function": map[string]any{"arguments": args},
						},
					},
				}, nil)
				markArgSeen(itemID, callID)
			}
			return
		}
		idx := nextToolIdx
		nextToolIdx++
		markToolKeys(idx, itemID, callID)
		if itemID == "" && callID == "" {
			markToolKeys(idx, "idx_"+strconv.Itoa(idx))
		}
		sawToolCall = true
		writeChatDelta(map[string]any{
			"tool_calls": []any{
				map[string]any{
					"index": idx,
					"id":    callID,
					"type":  "function",
					"function": map[string]any{
						"name":      name,
						"arguments": args,
					},
				},
			},
		}, nil)
		if args != "" {
			markArgSeen(itemID, callID)
		}
	}

	emitToolArgsDelta := func(itemID, callID, delta string) {
		if delta == "" {
			return
		}
		idx, ok := resolveToolIdx(itemID, callID)
		if !ok {
			idx = nextToolIdx
			nextToolIdx++
			markToolKeys(idx, itemID, callID)
			sawToolCall = true
			writeChatDelta(map[string]any{
				"tool_calls": []any{
					map[string]any{
						"index": idx,
						"id":    callID,
						"type":  "function",
						"function": map[string]any{
							"name":      "",
							"arguments": delta,
						},
					},
				},
			}, nil)
		} else {
			sawToolCall = true
			writeChatDelta(map[string]any{
				"tool_calls": []any{
					map[string]any{
						"index":    idx,
						"function": map[string]any{"arguments": delta},
					},
				},
			}, nil)
		}
		markArgSeen(itemID, callID)
	}

	finishStream := func(usage any) {
		fr := "stop"
		if sawToolCall {
			fr = "tool_calls"
		}
		chunk := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": fr}},
		}
		if usage != nil {
			chunk["usage"] = normalizeUsageToChat(usage)
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}

	sc := bufio.NewScanner(newContextReader(ctx, body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		// Detect mid-stream quota error (some providers send error inside SSE).
		if isQuota, qmsg := isQuotaErrorPayload(ev); isQuota {
			// Emit a graceful SSE error chunk so the client gets a clean termination.
			writeChatDelta(map[string]any{"content": "\n\n[Erro: quota esgotada — " + qmsg + "]"}, "stop")
			b, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message": qmsg,
					"type":    "upstream_error",
					"code":    "quota_exhausted",
				},
			})
			fmt.Fprintf(w, "data: %s\n\n", b)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return fmt.Errorf("sse quota error: %s", qmsg)
		}
		typ, _ := ev["type"].(string)
		switch typ {
		case "response.output_text.delta":
			if d, ok := ev["delta"].(string); ok && d != "" {
				writeChatDelta(map[string]any{"content": d}, nil)
			}
		case "response.output_item.added":
			if item, ok := ev["item"].(map[string]any); ok {
				emitToolCallStart(item, false)
			}
		case "response.output_item.done":
			if item, ok := ev["item"].(map[string]any); ok {
				emitReasoning(item)
				emitToolCallStart(item, true)
			}
		case "response.function_call_arguments.delta":
			d, _ := ev["delta"].(string)
			if d == "" {
				d, _ = ev["arguments"].(string)
			}
			emitToolArgsDelta(asString(ev["item_id"]), asString(ev["call_id"]), d)
		case "response.function_call_arguments.done":
			itemID := asString(ev["item_id"])
			callID := asString(ev["call_id"])
			args, _ := ev["arguments"].(string)
			if args == "" || anyArgSeen(itemID, callID) {
				continue
			}
			emitToolArgsDelta(itemID, callID, args)
		case "response.completed", "response.done":
			if resp, ok := ev["response"].(map[string]any); ok {
				for _, rawReasoning := range responsesReasoningItems(resp) {
					if item, ok := rawReasoning.(map[string]any); ok {
						emitReasoning(item)
					}
				}
				for _, raw := range extractResponsesFunctionCalls(resp) {
					if tcm, ok := raw.(map[string]any); ok {
						fn, _ := tcm["function"].(map[string]any)
						item := map[string]any{
							"type":      "function_call",
							"call_id":   asString(tcm["id"]),
							"id":        asString(tcm["id"]),
							"name":      asString(fn["name"]),
							"arguments": asString(fn["arguments"]),
						}
						emitToolCallStart(item, true)
					}
				}
				var usage any
				if u, ok := resp["usage"]; ok {
					usage = u
				}
				finishStream(usage)
				return nil
			}
			finishStream(nil)
			return nil
		}
	}
	finishStream(nil)
	return sc.Err()
}

func truncateForLog(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
