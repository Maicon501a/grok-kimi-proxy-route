package proxyhttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"grok-desktop/internal/pricing"
	"grok-desktop/internal/store"
)

func TestRecordUsageViaServer(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.UpsertAccount(store.Account{
		ID: "acc", Label: "L", AccessToken: "t",
		ClientID: store.DefaultClientID, Issuer: store.DefaultIssuer,
	})
	s := &Server{store: st}
	raw := []byte(`{"id":"chatcmpl-1","model":"` + store.DefaultModel + `",` +
		`"usage":{"prompt_tokens":1000,"completion_tokens":200,"total_tokens":1200}}`)
	s.recordUsageFromJSONBody(raw, "acc", store.ProviderXAI, store.DefaultModel, 12)
	snap := st.UsageSnapshot()
	if snap["acc"].Requests != 1 {
		t.Fatalf("requests=%d", snap["acc"].Requests)
	}
	if snap["acc"].CostUSD <= 0 {
		t.Fatalf("expected cost > 0, got %v", snap["acc"].CostUSD)
	}
	// pricing sanity
	cost := pricing.CostUSD(store.DefaultModel, store.ProviderXAI, 1000, 200, 0, 0)
	if cost <= 0 {
		t.Fatal("pricing zero")
	}
}

func TestRecordUsageFromSSECapture(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.UpsertAccount(store.Account{
		ID: "acc", Label: "L", AccessToken: "t",
		ClientID: store.DefaultClientID, Issuer: store.DefaultIssuer,
	})
	s := &Server{store: st}
	// Responses-shaped final event: usage nested under response.usage
	raw := []byte(`{"type":"response.completed","response":{"model":"` + store.DefaultModel + `",` +
		`"usage":{"input_tokens":500,"output_tokens":100,"total_tokens":600}}}`)
	s.recordUsageFromSSECapture(raw, "acc", store.ProviderXAI, store.DefaultModel, 30)
	snap := st.UsageSnapshot()
	if snap["acc"].Requests != 1 {
		t.Fatalf("requests=%d", snap["acc"].Requests)
	}
	if snap["acc"].TotalTokens != 600 {
		t.Fatalf("total=%d", snap["acc"].TotalTokens)
	}
}

// TestPipeSSEQuotaMidStream covers the fix for the dead quota scan: the block
// must be inspected BEFORE flushBlock resets it, or quota errors inside SSE
// data frames on the passthrough path are never detected.
func TestPipeSSEQuotaMidStream(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"usage limit exceeded for this billing cycle\"}}\n\n"
	rec := httptest.NewRecorder()
	err := pipeSSE(context.Background(), rec, strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("want quota error, got %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "quota_exhausted") {
		t.Fatalf("client missing graceful error frame: %q", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("client missing DONE: %q", out)
	}
}

// TestCrossProviderRotationRefused covers C1: when a quota retry's ensure
// returns an account whose provider differs from the pinned route provider,
// the request must fail instead of sending a cross-provider retry upstream.
func TestCrossProviderRotationRefused(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var hits int
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		hits++
		w := httptest.NewRecorder()
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"subscription:free-usage-exhausted"}}`))
		return w.Result(), nil
	})
	_ = st.UpdateSettings(func(s *store.Settings) {
		s.ProxyEnabled = false
	})
	// ensure always returns a KIMI account even though the model routes to xAI.
	kimiAcc := &store.Account{
		ID: "kimi-1", Provider: store.ProviderKimiWork,
		APIKey: "sk-kimi-x", AccessToken: "sk-kimi-x",
	}
	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		return kimiAcc.APIKey, kimiAcc, st.Settings(), nil
	}
	s := New(st, nil, ensure)
	s.SetQuotaHandler(func(accountID, reason string) bool { return true })

	body := `{"model":"grok-4.5","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.proxyUpstream(rr, req, "/chat/completions")

	res := rr.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", res.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "cross_provider_rotation") {
		t.Fatalf("want cross_provider_rotation error, got %s", raw)
	}
	if hits != 1 {
		t.Fatalf("upstream hits=%d — cross-provider retry must not be sent", hits)
	}
}
