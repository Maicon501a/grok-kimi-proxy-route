package proxyhttp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
)

// TestSearchIgnoresGlobalOllieProvider covers BUG 2(b): with the GLOBAL UI
// provider set to Ollie, a search request carrying a grok model must still run
// (the old code returned 501 for everyone). BUG 2(a): the pinned route seen by
// ensure must come from the client model, not the global provider.
func TestSearchIgnoresGlobalOllieProvider(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.UpsertAccount(store.Account{
		ID: "acc-x", Label: "X", AccessToken: "tok-x",
		ClientID: store.DefaultClientID, Issuer: store.DefaultIssuer,
	})
	_ = st.SetActiveAccount("acc-x")
	// Global UI provider = Ollie with a Kimi leftover default model — the
	// request must not care.
	_ = st.UpdateSettings(func(s *store.Settings) {
		s.Provider = store.ProviderOllie
		s.DefaultModel = store.KimiWorkDefaultModel
	})

	var wireModel string
	up := upstream.New()
	up.HTTP = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		wireModel, _ = m["model"].(string)
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"sr-1\",\"model\":\"grok-4.5\"," +
				"\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"))
		return w.Result(), nil
	})}

	var seenRoute string
	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		seenRoute = RouteProviderFrom(ctx)
		acc, _ := st.GetAccount("acc-x")
		st.IncAccountRequestCount(acc.ID)
		return acc.AccessToken, acc, st.Settings().WithProvider(store.ProviderXAI), nil
	}
	s := New(st, up, ensure)

	body := `{"query":"grok 4.5 news","model":"grok-4.5"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleSearch(rr, req)

	res := rr.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode == http.StatusNotImplemented {
		t.Fatalf("501 must be gone (global UI provider must not gate search): %s", raw)
	}
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, raw)
	}
	if seenRoute != store.ProviderXAI {
		t.Fatalf("ensure saw route %q, want pinned %q", seenRoute, store.ProviderXAI)
	}
	// BUG 2(a): the client-sent grok model reaches the wire untouched — never
	// substituted by the global (Kimi) default model.
	if wireModel != "grok-4.5" {
		t.Fatalf("wire model=%q — client model must be trusted, not rewritten to the global default", wireModel)
	}
}

// TestSearchRejectsNonGrokModels covers BUG 2(a): models routing to other
// providers get a clear 400 instead of a silent rewrite to the default model.
func TestSearchRejectsNonGrokModels(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.UpsertAccount(store.Account{
		ID: "acc-x", Label: "X", AccessToken: "tok-x",
		ClientID: store.DefaultClientID, Issuer: store.DefaultIssuer,
	})
	ensureCalls := 0
	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		ensureCalls++
		acc, _ := st.GetAccount("acc-x")
		return acc.AccessToken, acc, st.Settings(), nil
	}
	s := New(st, upstream.New(), ensure)

	for _, model := range []string{"k3-agent", "kimi-for-coding", "claude-sonnet-5", "gemini-3.1-pro-preview"} {
		body := `{"query":"x","model":"` + model + `"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(body))
		rr := httptest.NewRecorder()
		s.handleSearch(rr, req)
		res := rr.Result()
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("model %q: status=%d body=%s, want 400", model, res.StatusCode, raw)
		}
		if !strings.Contains(string(raw), "search_requires_xai_model") {
			t.Fatalf("model %q: want search_requires_xai_model, got %s", model, raw)
		}
	}
	if ensureCalls != 0 {
		t.Fatalf("ensure must not run for rejected routes (calls=%d)", ensureCalls)
	}
}

// TestSearchEmptyModelResolvesToGrokDefault: an empty model is an alias, so it
// resolves to the routed provider default — which on the xAI route is always a
// grok model even when the global DefaultModel belongs to Kimi.
func TestSearchEmptyModelResolvesToGrokDefault(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.UpsertAccount(store.Account{
		ID: "acc-x", Label: "X", AccessToken: "tok-x",
		ClientID: store.DefaultClientID, Issuer: store.DefaultIssuer,
	})
	_ = st.UpdateSettings(func(s *store.Settings) {
		s.Provider = store.ProviderKimiWork
		s.DefaultModel = store.KimiWorkDefaultModel
	})

	var wireModel string
	up := upstream.New()
	up.HTTP = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		wireModel, _ = m["model"].(string)
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"sr-2\"}}\n\n"))
		return w.Result(), nil
	})}
	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		acc, _ := st.GetAccount("acc-x")
		st.IncAccountRequestCount(acc.ID)
		return acc.AccessToken, acc, st.Settings().WithProvider(store.ProviderXAI), nil
	}
	s := New(st, up, ensure)

	req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"query":"x"}`))
	rr := httptest.NewRecorder()
	s.handleSearch(rr, req)
	res := rr.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, raw)
	}
	if !strings.Contains(wireModel, "grok") {
		t.Fatalf("empty model must resolve to a grok default on the xAI route, got %q", wireModel)
	}
}
