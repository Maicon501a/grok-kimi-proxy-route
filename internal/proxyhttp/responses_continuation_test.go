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

// TestResponsesContinuationDropsInstructions covers the xAI wire contract:
// the upstream (cli-chat-proxy.grok.com) rejects requests carrying both
// `instructions` and `previous_response_id` (HTTP 400 "Argument not supported").
// On continuation turns the proxy must drop the redundant instructions while
// keeping previous_response_id so the conversation still resumes.
func TestResponsesContinuationDropsInstructions(t *testing.T) {
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

	var wire map[string]any
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &wire)
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"cont-1\",\"model\":\"grok-4.6\"," +
				"\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"))
		return w.Result(), nil
	})

	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		acc, _ := st.GetAccount("acc-x")
		st.IncAccountRequestCount(acc.ID)
		return acc.AccessToken, acc, st.Settings().WithProvider(store.ProviderXAI), nil
	}
	s := New(st, upstream.New(), ensure)

	// Client (e.g. the new grok CLI) resumes a conversation: it sends both
	// instructions and previous_response_id. The proxy must not forward both.
	body := `{"model":"grok-4.6","input":"oi","instructions":"system rules","previous_response_id":"resp_prev_1","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.proxyUpstream(rr, req, "/responses")

	res := rr.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.StatusCode, raw)
	}
	if wire == nil {
		t.Fatal("upstream never reached")
	}
	if _, ok := wire["instructions"]; ok {
		t.Fatalf("instructions leaked upstream with previous_response_id: %#v", wire)
	}
	if got := wire["previous_response_id"]; got != "resp_prev_1" {
		t.Fatalf("previous_response_id=%v, want resp_prev_1", got)
	}
	if got := wire["input"]; got != "oi" {
		t.Fatalf("input=%v, want oi", got)
	}
}

// TestResponsesFirstTurnKeepsInstructions ensures the first turn (no
// previous_response_id) still carries instructions — the drop only applies to
// continuation turns.
func TestResponsesFirstTurnKeepsInstructions(t *testing.T) {
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

	var wire map[string]any
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &wire)
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"first-1\",\"model\":\"grok-4.6\"," +
				"\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"))
		return w.Result(), nil
	})

	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		acc, _ := st.GetAccount("acc-x")
		st.IncAccountRequestCount(acc.ID)
		return acc.AccessToken, acc, st.Settings().WithProvider(store.ProviderXAI), nil
	}
	s := New(st, upstream.New(), ensure)

	body := `{"model":"grok-4.6","input":"oi","instructions":"system rules","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.proxyUpstream(rr, req, "/responses")

	res := rr.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.StatusCode, raw)
	}
	if got := wire["instructions"]; got != "system rules" {
		t.Fatalf("instructions=%v, want system rules (first turn must keep it)", got)
	}
	if _, ok := wire["previous_response_id"]; ok {
		t.Fatalf("unexpected previous_response_id on first turn: %#v", wire)
	}
}
