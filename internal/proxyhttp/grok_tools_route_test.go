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
)

func TestXAIChatForwardsOpenCodeFunctionToolsWithoutNativeSearch(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = st.UpsertAccount(store.Account{
		ID: "acc-xai", Label: "xai", AccessToken: "tok-xai",
		ClientID: store.DefaultClientID, Issuer: store.DefaultIssuer,
		Provider: store.ProviderXAI,
	})
	_ = st.SetActiveAccount("acc-xai")

	var gotPath, gotIdent, gotVer, gotSurface, gotBody string
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotIdent = r.Header.Get("x-grok-client-identifier")
		gotVer = r.Header.Get("x-grok-client-version")
		gotSurface = r.Header.Get("x-grok-client-surface")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_tools",
			"model":"grok-4.6",
			"output":[{
				"type":"function_call",
				"id":"fc_1",
				"call_id":"call_bash",
				"name":"bash",
				"arguments":"{\"command\":\"ls\"}",
				"status":"completed"
			}],
			"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}
		}`))
		return w.Result(), nil
	})

	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		acc, ok := st.ActiveAccount()
		if !ok || acc == nil {
			return "", nil, st.Settings(), context.Canceled
		}
		return acc.AccessToken, acc, st.Settings(), nil
	}
	s := New(st, nil, ensure)

	body := `{
		"model":"grok-4.6",
		"stream":false,
		"messages":[{"role":"user","content":"list files"}],
		"tools":[{
			"type":"function",
			"function":{
				"name":"bash",
				"description":"run a shell command",
				"parameters":{"type":"object","properties":{"command":{"type":"string"}}}
			}
		}]
	}`
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
	if gotPath != "/v1/responses" {
		t.Fatalf("upstream path=%q want /v1/responses", gotPath)
	}
	if gotIdent != store.DefaultClientIdentifier {
		t.Fatalf("identifier=%q want %q", gotIdent, store.DefaultClientIdentifier)
	}
	if gotVer != store.DefaultClientVersion {
		t.Fatalf("version=%q want %q", gotVer, store.DefaultClientVersion)
	}
	if gotSurface != store.DefaultClientSurface {
		t.Fatalf("surface=%q want %q", gotSurface, store.DefaultClientSurface)
	}

	var up map[string]any
	if err := json.Unmarshal([]byte(gotBody), &up); err != nil {
		t.Fatalf("upstream body: %v %s", err, gotBody)
	}
	tools, _ := up["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("want exactly 1 client function tool, got %#v body=%s", tools, gotBody)
	}
	tm := tools[0].(map[string]any)
	if tm["type"] != "function" || tm["name"] != "bash" {
		t.Fatalf("tool=%#v", tm)
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("client body: %v %s", err, raw)
	}
	ch := out["choices"].([]any)[0].(map[string]any)
	if ch["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason=%v body=%s", ch["finish_reason"], raw)
	}
	msg := ch["message"].(map[string]any)
	tcs := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls=%#v", tcs)
	}
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Fatalf("name=%v", fn["name"])
	}
}

func TestHandleModelsGrokExposesCLIBackendFields(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(st, nil, func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		return "", nil, st.Settings(), context.Canceled
	})
	rr := httptest.NewRecorder()
	s.handleModels(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rr.Code != 200 {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Data []struct {
			ID             string `json:"id"`
			APIMode        string `json:"api_mode"`
			APIBackend     string `json:"api_backend"`
			SupportedInAPI bool   `json:"supported_in_api"`
			ContextWindow  int64  `json:"context_window"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var grok *struct {
		ID             string `json:"id"`
		APIMode        string `json:"api_mode"`
		APIBackend     string `json:"api_backend"`
		SupportedInAPI bool   `json:"supported_in_api"`
		ContextWindow  int64  `json:"context_window"`
	}
	for i := range out.Data {
		if out.Data[i].ID == "grok-4.6" {
			grok = &out.Data[i]
			break
		}
	}
	if grok == nil {
		t.Fatalf("missing grok-4.6 in %s", rr.Body.String())
	}
	if grok.APIMode != "responses" || grok.APIBackend != "responses" {
		t.Fatalf("api_mode=%q api_backend=%q", grok.APIMode, grok.APIBackend)
	}
	if !grok.SupportedInAPI {
		t.Fatal("supported_in_api=false")
	}
	if grok.ContextWindow != 500000 {
		t.Fatalf("context_window=%d want 500000", grok.ContextWindow)
	}
}
