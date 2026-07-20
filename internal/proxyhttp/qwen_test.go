package proxyhttp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
)

// resetQwenProbe clears the cached QwenBridge probe so each test controls the
// outcome via its own fake transport.
func resetQwenProbe(t *testing.T) {
	t.Helper()
	qwenProbeCache.Lock()
	qwenProbeCache.at = time.Time{}
	qwenProbeCache.key = ""
	qwenProbeCache.models = nil
	qwenProbeCache.Unlock()
	t.Cleanup(func() {
		qwenProbeCache.Lock()
		qwenProbeCache.at = time.Time{}
		qwenProbeCache.key = ""
		qwenProbeCache.models = nil
		qwenProbeCache.Unlock()
	})
}

// qwenTestStore opens a store configured for the QwenBridge provider (global
// provider stays xAI — routing is by client model id).
func qwenTestStore(t *testing.T, upstreamURL, apiKey string) *store.Store {
	t.Helper()
	isolateHome(t)
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = st.UpdateSettings(func(s *store.Settings) {
		s.QwenUpstream = upstreamURL
		s.QwenAPIKey = apiKey
	})
	return st
}

// qwenEnsure mirrors the production ensure (app.go / cmd/proxy): route-scoped
// provider from ctx, single fake bridge account, clear error when key missing.
func qwenEnsure(st *store.Store) func(ctx context.Context) (string, *store.Account, store.Settings, error) {
	return func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		settings := st.Settings()
		if rp := RouteProviderFrom(ctx); rp != "" {
			settings = settings.WithProvider(rp)
		}
		if settings.IsQwen() {
			key := strings.TrimSpace(settings.QwenAPIKey)
			if key == "" {
				return "", nil, settings, fmt.Errorf("qwen: API key do QwenBridge não configurada")
			}
			return key, &store.Account{
				ID: "qwen", Provider: store.ProviderQwen, Label: "QwenBridge",
				AccessToken: key, APIKey: key,
			}, settings, nil
		}
		return "", nil, settings, context.Canceled
	}
}

// TestProxyQwenChatCompletions: client /v1/chat/completions with a qwen model
// proxies directly to the bridge /chat/completions with Bearer auth and no
// provider-specific headers.
func TestProxyQwenChatCompletions(t *testing.T) {
	resetQwenProbe(t)
	st := qwenTestStore(t, "http://qwenbridge.test/v1", "bridge-key")

	var gotPath, gotAuth, gotBody string
	var gotXMsh, gotXGrok string
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotXMsh = r.Header.Get("X-Msh-Platform")
		gotXGrok = r.Header.Get("x-grok-client-version")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"qwen3.8",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hello qwen"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
		return w.Result(), nil
	})

	s := New(st, upstream.New(), qwenEnsure(st))
	body := `{"model":"qwen3.8","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.proxyUpstream(rr, req, "/chat/completions")

	res := rr.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "hello qwen") {
		t.Fatalf("client body missing bridge content: %s", raw)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path=%q, want /v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer bridge-key" {
		t.Fatalf("upstream auth=%q, want Bearer bridge-key", gotAuth)
	}
	if gotXMsh != "" || gotXGrok != "" {
		t.Fatalf("provider-specific headers leaked: X-Msh=%q x-grok=%q", gotXMsh, gotXGrok)
	}
	if !strings.Contains(gotBody, `"qwen3.8"`) {
		t.Fatalf("upstream body missing model: %s", gotBody)
	}
}

// TestProxyQwenResponsesConversion: client /v1/responses is converted to
// chat/completions upstream and translated back to a Responses JSON shape.
func TestProxyQwenResponsesConversion(t *testing.T) {
	resetQwenProbe(t)
	st := qwenTestStore(t, "http://qwenbridge.test/v1", "bridge-key")

	var gotPath, gotBody string
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-2","object":"chat.completion","model":"qwen3.8",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"responses ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`))
		return w.Result(), nil
	})

	s := New(st, upstream.New(), qwenEnsure(st))
	body := `{"model":"qwen3.8","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.proxyUpstream(rr, req, "/responses")

	res := rr.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, raw)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path=%q, want /v1/chat/completions (responses→chat conversion)", gotPath)
	}
	if !strings.Contains(gotBody, `"messages"`) {
		t.Fatalf("upstream body missing messages: %s", gotBody)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("client response not JSON: %v body=%s", err, raw)
	}
	if out["object"] != "response" {
		t.Fatalf("client object=%v, want response: %s", out["object"], raw)
	}
	if !strings.Contains(string(raw), "responses ok") {
		t.Fatalf("client body missing translated content: %s", raw)
	}
}

// TestHandleModelsQwenEnrichment: when the bridge answers GET /models, its
// exact ids appear in the catalog under the qwen provider.
func TestHandleModelsQwenEnrichment(t *testing.T) {
	resetQwenProbe(t)
	resetOllieProbe(t)
	st := qwenTestStore(t, "http://qwenbridge.test/v1", "bridge-key")

	var gotAuth string
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Host, "qwenbridge.test") {
			gotAuth = r.Header.Get("Authorization")
			w := httptest.NewRecorder()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen3.8"},{"id":"qwen3.7-plus"}]}`))
			return w.Result(), nil
		}
		return nil, context.DeadlineExceeded
	})

	s := New(st, upstream.New(), qwenEnsure(st))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	s.handleModels(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	seen := decodeModels(t, rr)
	if seen["qwen3.8"] != store.ProviderQwen || seen["qwen3.7-plus"] != store.ProviderQwen {
		t.Fatalf("qwen catalog missing bridge ids: %#v", seen)
	}
	if gotAuth != "Bearer bridge-key" {
		t.Fatalf("probe auth=%q, want Bearer bridge-key", gotAuth)
	}
}

// TestHandleModelsQwenUnreachable: when the bridge probe fails, qwen models
// are omitted but the rest of the catalog still responds.
func TestHandleModelsQwenUnreachable(t *testing.T) {
	resetQwenProbe(t)
	resetOllieProbe(t)
	st := qwenTestStore(t, "http://qwenbridge.test/v1", "bridge-key")
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})

	s := New(st, upstream.New(), qwenEnsure(st))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	s.handleModels(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	seen := decodeModels(t, rr)
	for id, prov := range seen {
		if prov == store.ProviderQwen {
			t.Fatalf("unreachable bridge must be omitted, found %s: %#v", id, seen)
		}
	}
	if seen["grok-4.5"] != store.ProviderXAI {
		t.Fatalf("static catalog incomplete: %#v", seen)
	}
}

// TestProxyQwenMissingKey: without a configured QwenAPIKey, ensure fails and
// the client gets a clear 401 — never a silent fallback to another provider.
func TestProxyQwenMissingKey(t *testing.T) {
	resetQwenProbe(t)
	st := qwenTestStore(t, "http://qwenbridge.test/v1", "")

	called := false
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		called = true
		w := httptest.NewRecorder()
		w.WriteHeader(500)
		return w.Result(), nil
	})

	s := New(st, upstream.New(), qwenEnsure(st))
	body := `{"model":"qwen3.8","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.proxyUpstream(rr, req, "/chat/completions")

	res := rr.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want 401", res.StatusCode, raw)
	}
	if !strings.Contains(strings.ToLower(string(raw)), "qwen") {
		t.Fatalf("error should mention qwen configuration: %s", raw)
	}
	if called {
		t.Fatal("upstream must not be called when the qwen key is missing")
	}
}

// TestProxyQwen429ReturnedToClient: single bridge account — quota errors are
// returned as-is (maxAttempts=1, no rotation).
func TestProxyQwen429ReturnedToClient(t *testing.T) {
	resetQwenProbe(t)
	st := qwenTestStore(t, "http://qwenbridge.test/v1", "bridge-key")

	hits := 0
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		hits++
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exceeded","type":"rate_limit_error"}}`))
		return w.Result(), nil
	})

	s := New(st, upstream.New(), qwenEnsure(st))
	body := `{"model":"qwen3.8","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.proxyUpstream(rr, req, "/chat/completions")

	res := rr.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s, want upstream 429 passed through", res.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "quota exceeded") {
		t.Fatalf("upstream error body not passed through: %s", raw)
	}
	if hits != 1 {
		t.Fatalf("upstream hits=%d, want exactly 1 (no rotation)", hits)
	}
}
