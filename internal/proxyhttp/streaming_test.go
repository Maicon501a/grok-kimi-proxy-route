package proxyhttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
)

func TestProxyOpenCodeZenStreamingPassesSSEAndWireModel(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var gotPath, gotAuth, gotBody string
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				`data: {"id":"zen-1","model":"deepseek-v4-flash-free","choices":[{"delta":{"content":"Hel"}}]}` + "\n\n" +
					`data: {"id":"zen-1","model":"deepseek-v4-flash-free","choices":[{"delta":{"content":"lo"}}]}` + "\n\n" +
					"data: [DONE]\n\n",
			)),
		}, nil
	})

	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		settings := st.Settings()
		if rp := RouteProviderFrom(ctx); rp != "" {
			settings = settings.WithProvider(rp)
		}
		if !settings.IsOpenCodeZen() {
			return "", nil, settings, context.Canceled
		}
		return store.OpenCodeZenAPIKey, &store.Account{
			ID: "opencode-zen-free", Provider: store.ProviderOpenCodeZen,
			Label: "OpenCode Zen Free", AccessToken: store.OpenCodeZenAPIKey,
		}, settings, nil
	}

	s := New(st, upstream.New(), ensure)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"opencode/deepseek-v4-flash-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
	))
	rec := httptest.NewRecorder()
	s.proxyUpstream(rec, req, "/chat/completions")

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/zen/v1/chat/completions" || gotAuth != "Bearer public" {
		t.Fatalf("upstream path=%q auth=%q", gotPath, gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"deepseek-v4-flash-free"`) {
		t.Fatalf("Zen namespace leaked to upstream body: %s", gotBody)
	}
	if !strings.Contains(rec.Body.String(), `data: {"id":"zen-1"`) || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("client SSE incomplete: %s", rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content-type=%q", rec.Header().Get("Content-Type"))
	}
}
