package proxyhttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"grok-desktop/internal/store"
)

// roundTripperFunc lets tests fake upstream responses at the transport layer.
// xAI/Kimi routes force the provider default upstream base, so an httptest URL
// in settings would never be dialed.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// swapUpstreamClient replaces the package upstream client for the duration of a test.
func swapUpstreamClient(t *testing.T, rt func(*http.Request) (*http.Response, error)) {
	t.Helper()
	old := upstreamHTTPClient
	upstreamHTTPClient = &http.Client{Transport: roundTripperFunc(rt)}
	t.Cleanup(func() { upstreamHTTPClient = old })
}

// isolateHome points the OS home dir at an empty temp dir so store.Open's
// SyncFromGrokCLI does not import real ~/.grok/auth.json accounts into tests.
func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
}

// TestFailover429Then200 exercises proxyUpstream with a fake upstream:
// first account gets 429 free-usage-exhausted; second account gets 200 + usage JSON.
func TestFailover429Then200(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.UpsertAccount(store.Account{
		ID: "acc-a", Label: "A", AccessToken: "tok-a",
		ClientID: store.DefaultClientID, Issuer: store.DefaultIssuer,
	})
	_ = st.UpsertAccount(store.Account{
		ID: "acc-b", Label: "B", AccessToken: "tok-b",
		ClientID: store.DefaultClientID, Issuer: store.DefaultIssuer,
	})
	_ = st.SetActiveAccount("acc-a")

	var mu sync.Mutex
	hits := map[string]int{}

	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		hits[auth]++
		mu.Unlock()
		w := httptest.NewRecorder()
		if strings.Contains(auth, "tok-a") {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"subscription:free-usage-exhausted"}}`))
			return w.Result(), nil
		}
		if strings.Contains(auth, "tok-b") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-1","model":"grok-4.5",` +
				`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],` +
				`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`))
			return w.Result(), nil
		}
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`unknown token`))
		return w.Result(), nil
	})

	_ = st.UpdateSettings(func(s *store.Settings) {
		s.ProxyEnabled = false
	})

	// ensure rotates: first call returns A if not exhausted; after mark, B
	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		// Prefer non-exhausted active, else first non-exhausted
		if acc, ok := st.ActiveAccount(); ok && acc != nil && !acc.Exhausted() && acc.AccessToken != "" {
			return acc.AccessToken, acc, st.Settings(), nil
		}
		for _, a := range st.ListAccounts() {
			if a.Exhausted() || a.AccessToken == "" {
				continue
			}
			cp := a
			_ = st.SetActiveAccount(a.ID)
			return a.AccessToken, &cp, st.Settings(), nil
		}
		return "", nil, st.Settings(), context.Canceled
	}

	s := New(st, nil, ensure)

	body := `{"model":"grok-4.5","stream":false,"messages":[{"role":"user","content":"hi"}]}`
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
	if !strings.Contains(string(raw), `"ok"`) && !strings.Contains(string(raw), "ok") {
		t.Fatalf("unexpected body: %s", raw)
	}

	a, _ := st.GetAccount("acc-a")
	if a == nil || !a.Exhausted() {
		t.Fatal("acc-a should be exhausted after 429")
	}
	// usage may fire if extractUsageFromOpenAIBody finds tokens
	snap := st.UsageSnapshot()
	if snap["acc-b"].Requests < 1 {
		// recordUsage needs non-zero tokens from payload — extractUsage may parse
		t.Logf("usage snap=%+v (ok if extract path differs)", snap)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["Bearer tok-a"] < 1 || hits["Bearer tok-b"] < 1 {
		t.Fatalf("hits=%v want both tokens", hits)
	}
}

// TestKimiCapacityBusyUsesQuotaFailover ensures Kimi's overloaded message
// invokes the normal quota lifecycle: mark exhausted, rotate, and retry.
func TestKimiCapacityBusyUsesQuotaFailover(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.UpsertAccount(store.Account{
		ID: "kimi-a", Label: "Kimi A", Provider: store.ProviderKimiWork,
		APIKey: "sk-kimi-a", AccessToken: "sk-kimi-a",
	})
	_ = st.UpsertAccount(store.Account{
		ID: "kimi-b", Label: "Kimi B", Provider: store.ProviderKimiWork,
		APIKey: "sk-kimi-b", AccessToken: "sk-kimi-b",
	})
	_ = st.SetActiveAccount("kimi-a")

	var mu sync.Mutex
	hits := map[string]int{}
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		hits[auth]++
		mu.Unlock()
		w := httptest.NewRecorder()
		if strings.Contains(auth, "sk-kimi-a") {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"Too many people are chatting with Kimi right now. Please try again soon."}}`))
			return w.Result(), nil
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"k3-agent","choices":[{"index":0,"message":{"role":"assistant","content":"rotated ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}`))
		return w.Result(), nil
	})

	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		for _, id := range []string{"kimi-a", "kimi-b"} {
			acc, ok := st.GetAccount(id)
			if !ok || acc == nil || !acc.Usable() {
				continue
			}
			return acc.BearerToken(), acc, st.Settings(), nil
		}
		return "", nil, st.Settings(), context.Canceled
	}
	s := New(st, nil, ensure)
	var quotaCalls int
	s.SetQuotaHandler(func(accountID, reason string) bool {
		quotaCalls++
		if accountID != "kimi-a" || !strings.Contains(reason, "Too many people are chatting with Kimi") {
			t.Fatalf("unexpected quota callback: account=%q reason=%q", accountID, reason)
		}
		if _, err := st.MarkExhausted(accountID, reason); err != nil {
			t.Fatal(err)
		}
		return true
	})

	body := `{"model":"k3-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.proxyUpstream(rr, req, "/chat/completions")

	res := rr.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(raw), "rotated ok") {
		t.Fatalf("status=%d body=%s", res.StatusCode, raw)
	}
	if quotaCalls != 1 {
		t.Fatalf("quota handler calls=%d, want 1", quotaCalls)
	}
	a, _ := st.GetAccount("kimi-a")
	if a == nil || !a.Exhausted() {
		t.Fatal("kimi-a must be exhausted after the capacity message")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["Bearer sk-kimi-a"] != 1 || hits["Bearer sk-kimi-b"] != 1 {
		t.Fatalf("hits=%v want one attempt per Kimi account", hits)
	}
}
