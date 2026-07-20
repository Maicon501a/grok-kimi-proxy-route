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

	"grok-desktop/internal/store"
)

// anthropicTestEnsure mimics cmd/proxy ensureCreds: picks the first usable
// account for the pinned route provider and Inc's the in-flight counter.
func anthropicTestEnsure(st *store.Store, seenProv *[]string) func(ctx context.Context) (string, *store.Account, store.Settings, error) {
	return func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		if seenProv != nil {
			*seenProv = append(*seenProv, RouteProviderFrom(ctx))
		}
		prov := RouteProviderFrom(ctx)
		for _, a := range st.ListAccounts() {
			if prov != "" && a.NormalizedProvider() != prov {
				continue
			}
			if !a.Usable() {
				continue
			}
			cp := a
			st.IncAccountRequestCount(a.ID)
			return a.BearerToken(), &cp, st.Settings().WithProvider(prov), nil
		}
		return "", nil, st.Settings(), context.Canceled
	}
}

// TestAnthropicXAIUsesResponsesEndpoint covers BUG 1(a): an Anthropic Messages
// request for a grok model must hit the xAI /responses gateway (not
// /chat/completions) with a Responses-shaped body, and the Responses JSON must
// be translated back into an Anthropic message.
func TestAnthropicXAIUsesResponsesEndpoint(t *testing.T) {
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

	var gotPath, gotBody string
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-9","model":"grok-4.5",` +
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello from responses"}]}],` +
			`"usage":{"input_tokens":12,"output_tokens":7,"total_tokens":19}}`))
		return w.Result(), nil
	})

	s := New(st, nil, anthropicTestEnsure(st, nil))
	body := `{"model":"grok-4.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleMessages(rr, req)

	res := rr.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, raw)
	}
	if !strings.HasSuffix(gotPath, "/responses") {
		t.Fatalf("upstream path=%q — xAI route must POST /responses, not /chat/completions", gotPath)
	}
	var wire map[string]any
	if err := json.Unmarshal([]byte(gotBody), &wire); err != nil {
		t.Fatalf("wire body not json: %v (%s)", err, gotBody)
	}
	if _, ok := wire["messages"]; ok {
		t.Fatalf("responses wire must not contain chat messages: %s", gotBody)
	}
	if _, ok := wire["input"]; !ok {
		t.Fatalf("responses wire missing input: %s", gotBody)
	}
	if wire["model"] != "grok-4.5" {
		t.Fatalf("wire model=%v", wire["model"])
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("client body not json: %v (%s)", err, raw)
	}
	if out["type"] != "message" || out["role"] != "assistant" {
		t.Fatalf("not an anthropic message: %s", raw)
	}
	content, _ := out["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("empty content: %s", raw)
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "text" || !strings.Contains(asString(first["text"]), "hello from responses") {
		t.Fatalf("bad content block: %s", raw)
	}
	usage, _ := out["usage"].(map[string]any)
	if asInt(usage["input_tokens"], 0) != 12 || asInt(usage["output_tokens"], 0) != 7 {
		t.Fatalf("bad usage translation: %s", raw)
	}
	// BUG 1(c): usage must be recorded for the serving account.
	if snap := st.UsageSnapshot(); snap["acc-x"].Requests != 1 || snap["acc-x"].TotalTokens != 19 {
		t.Fatalf("usage not recorded: %+v", snap)
	}
}

// TestAnthropicXAIStreamTranslation covers BUG 1(a) streaming: Responses SSE
// events must become Anthropic SSE events (text + tool_use blocks + stop).
func TestAnthropicXAIStreamTranslation(t *testing.T) {
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

	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp-1","model":"grok-4.5"}}`,
		`data: {"type":"response.output_text.delta","delta":"Hel"}`,
		`data: {"type":"response.output_text.delta","delta":"lo"}`,
		`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","call_id":"call_1","delta":"{\"city\":"}`,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_1","call_id":"call_1","arguments":"{\"city\":\"Paris\"}"}`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"get_time","arguments":"{\"tz\":\"UTC\"}"}}`,
		`data: {"type":"response.completed","response":{"id":"resp-1","model":"grok-4.5","usage":{"input_tokens":5,"output_tokens":9,"total_tokens":14}}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	var gotPath string
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
		return w.Result(), nil
	})

	s := New(st, nil, anthropicTestEnsure(st, nil))
	body := `{"model":"grok-4.5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"weather?"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleMessages(rr, req)

	res := rr.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := string(raw)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, out)
	}
	if !strings.HasSuffix(gotPath, "/responses") {
		t.Fatalf("upstream path=%q", gotPath)
	}
	for _, want := range []string{
		"event: message_start",
		"content_block_start",
		`"type":"text"`,
		`"text":"Hel"`,
		`"text":"lo"`,
		`"type":"tool_use"`,
		`"name":"get_weather"`,
		`"partial_json":"{\"city\":"`,
		`"name":"get_time"`,
		`"partial_json":"{\"tz\":\"UTC\"}"`,
		"event: message_delta",
		`"stop_reason":"tool_use"`,
		`"output_tokens":9`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in stream:\n%s", want, out)
		}
	}
	// args done-frames must not re-send args already streamed via deltas.
	if strings.Count(out, `Paris`) != 0 {
		t.Fatalf("duplicate args from .done after deltas:\n%s", out)
	}
	// Usage from the completed event must be captured via the tee reader.
	if snap := st.UsageSnapshot(); snap["acc-x"].Requests != 1 || snap["acc-x"].TotalTokens != 14 {
		t.Fatalf("stream usage not recorded: %+v", snap)
	}
}

// TestAnthropicQuotaRotation covers BUG 1(b): a quota failure on the first
// account must rotate to a same-provider account and retry — on /v1/messages.
// It also covers BUG 4 (every ensure in the retry loop receives the pinned ctx).
func TestAnthropicQuotaRotation(t *testing.T) {
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
	var seenProv []string
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
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-2","model":"grok-4.5",` +
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"rotated ok"}]}],` +
			`"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`))
		return w.Result(), nil
	})

	s := New(st, nil, anthropicTestEnsure(st, &seenProv))
	body := `{"model":"grok-4.5","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleMessages(rr, req)

	res := rr.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "rotated ok") {
		t.Fatalf("unexpected body: %s", raw)
	}
	mu.Lock()
	if hits["Bearer tok-a"] < 1 || hits["Bearer tok-b"] < 1 {
		t.Fatalf("hits=%v want both accounts tried", hits)
	}
	mu.Unlock()
	a, _ := st.GetAccount("acc-a")
	if a == nil || !a.Exhausted() {
		t.Fatal("acc-a must be marked exhausted after quota error")
	}
	// BUG 4: every ensure call in the retry path saw the pinned xAI route.
	if len(seenProv) < 2 {
		t.Fatalf("ensure calls=%d, want >=2 (initial + rotation)", len(seenProv))
	}
	for i, p := range seenProv {
		if p != store.ProviderXAI {
			t.Fatalf("ensure call %d saw route %q, want pinned %q", i, p, store.ProviderXAI)
		}
	}
	// BUG 1(c): in-flight counters balanced after rotation + completion,
	// and usage recorded against the serving account.
	if snap := st.UsageSnapshot(); snap["acc-b"].Requests != 1 {
		t.Fatalf("usage snap=%+v", snap)
	}
}

// TestAnthropicKimiUsesChatCompletions verifies the Kimi Work route keeps the
// chat/completions wire (agent-gw has no /responses) and applies the thinking body.
func TestAnthropicKimiUsesChatCompletions(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.UpsertAccount(store.Account{
		ID: "acc-k", Label: "K", Provider: store.ProviderKimiWork,
		APIKey: "sk-kimi-test", AccessToken: "sk-kimi-test",
	})

	var gotPath, gotBody string
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"k3-agent",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"kimi says hi"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`))
		return w.Result(), nil
	})

	s := New(st, nil, anthropicTestEnsure(st, nil))
	body := `{"model":"k3-agent","max_tokens":64,"thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleMessages(rr, req)

	res := rr.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, raw)
	}
	if !strings.HasSuffix(gotPath, "/chat/completions") {
		t.Fatalf("kimi route must keep /chat/completions, got %q", gotPath)
	}
	if !strings.Contains(gotBody, `"messages"`) || strings.Contains(gotBody, `"input"`) {
		t.Fatalf("kimi wire must be chat-shaped: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"thinking"`) {
		t.Fatalf("kimi thinking object missing: %s", gotBody)
	}
	if !strings.Contains(string(raw), "kimi says hi") {
		t.Fatalf("unexpected body: %s", raw)
	}
}

// TestStreamResponsesToAnthropicQuota unit-tests the mid-stream quota contract:
// the translator emits a graceful Anthropic error event and returns an
// "sse quota error" so handleStreamQuota marks the account.
func TestStreamResponsesToAnthropicQuota(t *testing.T) {
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"data: {\"error\":{\"message\":\"usage limit exceeded for this billing cycle\"}}\n\n"
	rec := httptest.NewRecorder()
	err := streamResponsesToAnthropic(context.Background(), rec, strings.NewReader(body), "grok-4.5")
	if err == nil || !strings.Contains(err.Error(), "sse quota error") {
		t.Fatalf("want sse quota error, got %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "event: error") || !strings.Contains(out, "usage limit exceeded") {
		t.Fatalf("client missing graceful error event: %q", out)
	}
}

// TestStreamOpenAIToAnthropicQuota covers the same contract on the
// chat/completions→Anthropic translator (Kimi route).
func TestStreamOpenAIToAnthropicQuota(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"resource_exhausted: quota exceeded\"}}\n\n"
	rec := httptest.NewRecorder()
	err := streamOpenAIToAnthropic(context.Background(), rec, strings.NewReader(body), "k3-agent")
	if err == nil || !strings.Contains(err.Error(), "sse quota error") {
		t.Fatalf("want sse quota error, got %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "event: error") || !strings.Contains(out, "resource_exhausted") {
		t.Fatalf("client missing graceful error event: %q", out)
	}
}

// TestResponsesJSONToAnthropicMessage unit-tests non-stream Responses→Anthropic
// including tool_use blocks and max_tokens stop mapping.
func TestResponsesJSONToAnthropicMessage(t *testing.T) {
	raw := []byte(`{"id":"resp-1","model":"grok-4.5","output":[` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checking"}]},` +
		`{"type":"function_call","call_id":"call_7","name":"lookup","arguments":"{\"q\":1}"}` +
		`],"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14}}`)
	out, err := responsesJSONToAnthropicMessage(raw, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "message" || m["stop_reason"] != "tool_use" {
		t.Fatalf("bad envelope: %s", out)
	}
	content, _ := m["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("want text + tool_use blocks, got %s", out)
	}
	tool, _ := content[1].(map[string]any)
	if tool["type"] != "tool_use" || tool["name"] != "lookup" || tool["id"] != "call_7" {
		t.Fatalf("bad tool_use block: %s", out)
	}
}
