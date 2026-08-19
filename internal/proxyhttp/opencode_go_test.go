package proxyhttp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"grok-desktop/internal/secure"
	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
)

func TestOpenCodeGoModelsDynamicFetch(t *testing.T) {
	isolateHome(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enc, err := secure.Encrypt("go-key-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSettings(func(s *store.Settings) { s.OpenCodeGoAPIKey = enc }); err != nil {
		t.Fatal(err)
	}

	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/zen/go/v1/models" {
			t.Fatalf("path=%s, want /zen/go/v1/models", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer go-key-123" {
			t.Fatalf("auth=%q, want Bearer go-key-123", auth)
		}
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"deepseek-v4-pro"},{"id":"deepseek-v4-flash"},{"id":"gpt-5.6-luna"}]}`))
		return w.Result(), nil
	})

	settings := st.Settings().WithProvider(store.ProviderOpenCodeGo)
	got := openCodeGoModels(settings)
	want := map[string]bool{
		"opencode-go/deepseek-v4-pro":   true,
		"opencode-go/deepseek-v4-flash": true,
		"opencode-go/gpt-5.6-luna":      true,
	}
	if len(got) != len(want) {
		t.Fatalf("models=%+v, want %d entries", got, len(want))
	}
	for _, m := range got {
		if !want[m.ID] {
			t.Fatalf("unexpected id %q (junk from zen-wide catalog must not leak into opencode-go)", m.ID)
		}
		if m.APIMode != "chat" {
			t.Fatalf("id=%q api_mode=%q", m.ID, m.APIMode)
		}
	}
}

func TestOpenCodeGoModelsFallsBackToStatic(t *testing.T) {
	isolateHome(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enc, err := secure.Encrypt("go-key-456")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSettings(func(s *store.Settings) { s.OpenCodeGoAPIKey = enc }); err != nil {
		t.Fatal(err)
	}

	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
		return w.Result(), nil
	})

	settings := st.Settings().WithProvider(store.ProviderOpenCodeGo)
	got := openCodeGoModels(settings)
	found := false
	for _, m := range got {
		if m.ID == "opencode-go/deepseek-v4-flash" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fallback missing static entries: %+v", got)
	}
}

func TestOpenCodeGoRoutesEveryModelToGoGateway(t *testing.T) {
	isolateHome(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enc, err := secure.Encrypt("go-key-789")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSettings(func(s *store.Settings) { s.OpenCodeGoAPIKey = enc }); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var urls []string
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		urls = append(urls, r.URL.String())
		mu.Unlock()
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"deepseek-v4-pro","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		return w.Result(), nil
	})

	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		settings := st.Settings().WithProvider(RouteProviderFrom(ctx))
		return "go-key", &store.Account{ID: "opencode-go", Provider: store.ProviderOpenCodeGo, AccessToken: "go-key"}, settings, nil
	}
	srv := New(st, upstream.New(), ensure)

	for _, tc := range []struct {
		model string
		want  string
	}{
		{"opencode-go/deepseek-v4-pro", "https://opencode.ai/zen/go/v1/chat/completions"},
		{"opencode-go/gpt-5.6-luna", "https://opencode.ai/zen/go/v1/chat/completions"},
		{"opencode-go/big-pickle", "https://opencode.ai/zen/go/v1/chat/completions"},
		{"opencode-go/deepseek-v4-flash-free", "https://opencode.ai/zen/go/v1/chat/completions"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+tc.model+`","messages":[{"role":"user","content":"oi"}]}`))
		rr := httptest.NewRecorder()
		srv.proxyUpstream(rr, req, "/chat/completions")
		if rr.Code != http.StatusOK {
			t.Fatalf("model=%s status=%d body=%s", tc.model, rr.Code, rr.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	wantURLs := []string{
		"https://opencode.ai/zen/go/v1/chat/completions",
		"https://opencode.ai/zen/go/v1/chat/completions",
		"https://opencode.ai/zen/go/v1/chat/completions",
		"https://opencode.ai/zen/go/v1/chat/completions",
	}
	if len(urls) != len(wantURLs) {
		t.Fatalf("urls=%v", urls)
	}
	for i, u := range urls {
		if u != wantURLs[i] {
			t.Fatalf("request %d url=%s, want %s", i, u, wantURLs[i])
		}
	}
}

func TestOpenCodeGoRetriesWithoutThinkingWhenHistoryIsIncomplete(t *testing.T) {
	isolateHome(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.UpdateSettings(func(s *store.Settings) { s.OpenCodeGoAPIKey = "dpapi:test" }); err != nil {
		t.Fatal(err)
	}

	var bodies []map[string]any
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("invalid body: %v", err)
		}
		bodies = append(bodies, parsed)
		w := httptest.NewRecorder()
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"The reasoning_content in the thinking mode must be passed back to the API."}}`))
			return w.Result(), nil
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"deepseek-v4-flash-free","choices":[{"message":{"role":"assistant","content":"continued"},"finish_reason":"stop"}]}`))
		return w.Result(), nil
	})

	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		settings := st.Settings().WithProvider(RouteProviderFrom(ctx))
		return "go-key", &store.Account{ID: "opencode-go", Provider: store.ProviderOpenCodeGo, AccessToken: "go-key"}, settings, nil
	}
	srv := New(st, upstream.New(), ensure)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"opencode-go/deepseek-v4-flash-free","reasoning_effort":"max","messages":[{"role":"assistant","content":"prior answer"},{"role":"user","content":"continue"}]}`))
	rr := httptest.NewRecorder()
	srv.proxyUpstream(rr, req, "/chat/completions")

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(bodies) != 2 {
		t.Fatalf("attempts=%d, want 2", len(bodies))
	}
	if bodies[0]["reasoning_effort"] != "max" {
		t.Fatalf("first effort=%v", bodies[0]["reasoning_effort"])
	}
	if _, ok := bodies[1]["reasoning_effort"]; ok {
		t.Fatalf("retry must omit reasoning_effort: %#v", bodies[1])
	}
}
