package proxyhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
)

// resetOllieProbe clears the cached reachability result so each test controls
// the probe outcome via its own fake transport.
func resetOllieProbe(t *testing.T) {
	t.Helper()
	ollieProbeCache.Lock()
	ollieProbeCache.at = time.Time{}
	ollieProbeCache.ok = false
	ollieProbeCache.Unlock()
	t.Cleanup(func() {
		ollieProbeCache.Lock()
		ollieProbeCache.at = time.Time{}
		ollieProbeCache.ok = false
		ollieProbeCache.Unlock()
	})
}

func decodeModels(t *testing.T, rr *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var out struct {
		Data []struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, m := range out.Data {
		seen[m.ID] = m.Provider
	}
	return seen
}

func TestHandleModelsUnifiedCatalog(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		return "", nil, st.Settings(), context.Canceled
	}
	s := New(st, upstream.New(), ensure)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	s.handleModels(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Data []struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
		} `json:"data"`
		Route string `json:"route"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Route != "model" {
		t.Fatalf("route=%q want model", out.Route)
	}
	seen := map[string]string{}
	for _, m := range out.Data {
		seen[m.ID] = m.Provider
	}
	if seen["grok-4.5"] != store.ProviderXAI {
		t.Fatalf("missing grok-4.5: %#v", seen)
	}
	if seen["k3-agent"] != store.ProviderKimiWork {
		t.Fatalf("missing k3-agent: %#v", seen)
	}
}

// TestHandleModelsListsOllieRegardlessOfUIProvider covers BUG 3: the Ollie
// catalog must be listed whenever its gateway is reachable, even when the
// global UI provider is xAI or Kimi (previously it only appeared when the
// global provider was Ollie).
func TestHandleModelsListsOllieRegardlessOfUIProvider(t *testing.T) {
	isolateHome(t)
	resetOllieProbe(t)
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = st.UpdateSettings(func(s *store.Settings) {
		s.Provider = store.ProviderKimiWork
		s.DefaultModel = store.KimiWorkDefaultModel
	})
	// Fake transport: the probe (GET {ollie}/models) answers 200 → reachable.
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5"}]}`))
		return w.Result(), nil
	})
	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		return "", nil, st.Settings(), context.Canceled
	}
	s := New(st, upstream.New(), ensure)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	s.handleModels(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	seen := decodeModels(t, rr)
	if seen["claude-sonnet-5"] != store.ProviderOllie {
		t.Fatalf("ollie catalog missing with UI provider=kimi_work: %#v", seen)
	}
	if seen["grok-4.5"] != store.ProviderXAI || seen["k3-agent"] != store.ProviderKimiWork {
		t.Fatalf("static catalog incomplete: %#v", seen)
	}
}

// TestHandleModelsSkipsUnreachableOllie: when the gateway probe fails, Ollie
// models are omitted but the rest of the catalog still responds fast.
func TestHandleModelsSkipsUnreachableOllie(t *testing.T) {
	isolateHome(t)
	resetOllieProbe(t)
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})
	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		return "", nil, st.Settings(), context.Canceled
	}
	s := New(st, upstream.New(), ensure)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	s.handleModels(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	seen := decodeModels(t, rr)
	if _, ok := seen["claude-sonnet-5"]; ok {
		t.Fatalf("unreachable ollie must be omitted: %#v", seen)
	}
	if seen["grok-4.5"] != store.ProviderXAI {
		t.Fatalf("static catalog incomplete: %#v", seen)
	}
}

// TestHandleModelsListsGeminiWhenConfigured covers BUG 3: Gemini models appear
// whenever ADC is configured (project in settings or Google env), independent
// of the global UI provider.
func TestHandleModelsListsGeminiWhenConfigured(t *testing.T) {
	isolateHome(t)
	resetOllieProbe(t)
	// Ensure no ambient Google env leaks into this test, then set project explicitly.
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GCLOUD_PROJECT", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = st.UpdateSettings(func(s *store.Settings) {
		s.Provider = store.ProviderXAI
		s.GeminiProject = "proj-test"
	})
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})
	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		return "", nil, st.Settings(), context.Canceled
	}
	s := New(st, upstream.New(), ensure)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	s.handleModels(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	seen := decodeModels(t, rr)
	if seen["gemini-2.5-pro"] != store.ProviderGemini {
		t.Fatalf("gemini catalog missing when configured with UI provider=xai: %#v", seen)
	}
}

// TestHandleModelsOmitsGeminiWhenUnconfigured: without project or env, Gemini
// stays out of the catalog.
func TestHandleModelsOmitsGeminiWhenUnconfigured(t *testing.T) {
	isolateHome(t)
	resetOllieProbe(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GCLOUD_PROJECT", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})
	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		return "", nil, st.Settings(), context.Canceled
	}
	s := New(st, upstream.New(), ensure)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	s.handleModels(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	seen := decodeModels(t, rr)
	for id := range seen {
		if seen[id] == store.ProviderGemini {
			t.Fatalf("gemini must be omitted when unconfigured: %#v", seen)
		}
	}
}

func TestWithProviderForModelRouting(t *testing.T) {
	// Start from Kimi UI global — client model must still win.
	base := store.Settings{Provider: store.ProviderKimiWork, DefaultModel: store.KimiWorkDefaultModel}
	k := base.WithProviderForModel("kimi-for-coding")
	if !k.IsKimiWork() {
		t.Fatalf("want kimi, got %s", k.NormalizedProvider())
	}
	if k.APIMode != "chat" {
		t.Fatalf("kimi api mode %q", k.APIMode)
	}
	g := base.WithProviderForModel("grok-4.5")
	if !g.IsXAI() {
		t.Fatalf("want xai when client asks grok, got %s (must ignore UI kimi global)", g.NormalizedProvider())
	}
	// Empty / alias / unknown → xAI, not leftover UI provider.
	for _, m := range []string{"", "default", "auto", "some-unknown-model"} {
		x := base.WithProviderForModel(m)
		if !x.IsXAI() {
			t.Fatalf("model %q: want xai, got %s", m, x.NormalizedProvider())
		}
	}
	// Spaced aliases → Kimi.
	for _, m := range []string{"kimi k3 max", "k3 max", "k3 agent high", "k3 agent xhigh"} {
		x := base.WithProviderForModel(m)
		if !x.IsKimiWork() {
			t.Fatalf("model %q: want kimi, got %s", m, x.NormalizedProvider())
		}
	}
}
