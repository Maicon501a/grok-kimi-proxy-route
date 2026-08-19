package proxyhttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
)

func TestSetUpstreamAuthHeadersCodex(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, store.CodexUpstream+"/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	settings := store.Settings{Provider: store.ProviderCodex, CodexAccountID: "account-123", CodexFedRAMP: true}
	setUpstreamAuthHeaders(req, "access-token", settings)
	checks := map[string]string{
		"Authorization":      "Bearer access-token",
		"ChatGPT-Account-ID": "account-123",
		"originator":         "codex_cli_rs",
		"version":            store.CodexClientVersion,
		"X-OpenAI-Fedramp":   "true",
	}
	for name, want := range checks {
		if got := req.Header.Get(name); got != want {
			t.Fatalf("%s=%q, want %q", name, got, want)
		}
	}
}

func TestCodexClientDetectionIsNarrow(t *testing.T) {
	for _, headers := range []map[string]string{
		{"User-Agent": "codex_cli_rs/0.144.0"},
		{"Originator": "codex_vscode"},
		{"User-Agent": "codex-tui/0.144.0"},
		{"Originator": "codex_exec"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		if !isCodexRequest(req) {
			t.Fatalf("official headers not detected: %#v", headers)
		}
	}
	for _, headers := range []map[string]string{
		{"User-Agent": "third-party-codex-proxy"},
		{"OpenAI-Project": "my-codex-project"},
		{"X-Codex": "true"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		if isCodexRequest(req) {
			t.Fatalf("third-party headers misdetected: %#v", headers)
		}
	}
}

func TestCodexNativeResponsesUsesStatelessSSEContract(t *testing.T) {
	isolateHome(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	account := store.Account{
		ID: "codex-account", Provider: store.ProviderCodex, Label: "Codex",
		AccessToken: "codex-token", RefreshToken: "codex-refresh", TeamID: "workspace-1",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := st.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}

	var wire map[string]any
	var accept string
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		accept = r.Header.Get("Accept")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatalf("decode wire body: %v", err)
		}
		response := httptest.NewRecorder()
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.WriteString("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		_, _ = response.WriteString("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-codex\",\"model\":\"gpt-5.6-sol\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
		return response.Result(), nil
	})

	var route string
	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		route = RouteProviderFrom(ctx)
		acc, _ := st.GetAccount(account.ID)
		st.IncAccountRequestCount(account.ID)
		settings := st.Settings().WithProvider(route)
		settings.CodexAccountID = acc.TeamID
		return acc.AccessToken, acc, settings, nil
	}
	server := New(st, upstream.New(), ensure)
	body := `{"model":"gpt-5.6-sol","stream":false,"store":true,"previous_response_id":"resp-old","include":["file_search_call.results"],"reasoning":{"effort":"high"},"input":[{"type":"reasoning","id":"rs-1","encrypted_content":"ciphertext"},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	request.Header.Set("User-Agent", "codex_cli_rs/0.144.0")
	recorder := httptest.NewRecorder()
	server.proxyUpstream(recorder, request, "/responses")

	result := recorder.Result()
	defer result.Body.Close()
	raw, _ := io.ReadAll(result.Body)
	if result.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"id":"resp-codex"`) {
		t.Fatalf("status=%d body=%s", result.StatusCode, raw)
	}
	if route != store.ProviderCodex {
		t.Fatalf("route=%q, want %q", route, store.ProviderCodex)
	}
	if accept != "text/event-stream" || wire["stream"] != true || wire["store"] != false {
		t.Fatalf("accept=%q stream=%#v store=%#v", accept, wire["stream"], wire["store"])
	}
	if _, exists := wire["previous_response_id"]; exists {
		t.Fatalf("previous_response_id leaked upstream: %#v", wire)
	}
	if got := asString(wire["model"]); got != "gpt-5.6-sol" {
		t.Fatalf("wire model=%q", got)
	}
	inputs, _ := wire["input"].([]any)
	if len(inputs) != 2 || asString(inputs[0].(map[string]any)["encrypted_content"]) != "ciphertext" {
		t.Fatalf("reasoning history was not preserved: %#v", wire["input"])
	}
	includes, _ := wire["include"].([]any)
	if len(includes) != 2 || includes[0] != "file_search_call.results" || includes[1] != "reasoning.encrypted_content" {
		t.Fatalf("include=%#v", wire["include"])
	}
	reasoning, _ := wire["reasoning"].(map[string]any)
	if reasoning["context"] != "all_turns" || wire["parallel_tool_calls"] != false {
		t.Fatalf("gpt-5.6 options missing: reasoning=%#v parallel=%#v", reasoning, wire["parallel_tool_calls"])
	}
}

func TestNormalizeCodexReasoningEffort(t *testing.T) {
	for input, want := range map[string]string{
		"xhigh":      "max",
		"extra-high": "max",
		"ultra":      "max",
		"high":       "high",
	} {
		if got := normalizeCodexReasoningEffort(input); got != want {
			t.Fatalf("normalizeCodexReasoningEffort(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizeCodexResponsesInput(t *testing.T) {
	got := normalizeCodexResponsesInput("hello")
	items, ok := got.([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("input = %#v, want one typed item", got)
	}
	item := items[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "user" {
		t.Fatalf("item = %#v", item)
	}
	content := item["content"].([]any)
	if content[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("content = %#v", content)
	}

	alreadyTyped := []any{map[string]any{"type": "message"}}
	if got := normalizeCodexResponsesInput(alreadyTyped); len(got.([]any)) != 1 {
		t.Fatalf("typed input changed: %#v", got)
	}
}

func TestCodexTransientRefreshFailureDoesNotPoisonAccount(t *testing.T) {
	isolateHome(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	account := store.Account{
		ID: "codex-transient", Provider: store.ProviderCodex, Label: "Codex",
		AccessToken: "expired-token", RefreshToken: "refresh-token", TeamID: "workspace-1",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := st.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}
	swapUpstreamClient(t, func(*http.Request) (*http.Response, error) {
		response := httptest.NewRecorder()
		response.WriteHeader(http.StatusUnauthorized)
		_, _ = response.WriteString(`{"error":{"message":"token expired"}}`)
		return response.Result(), nil
	})
	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		acc, _ := st.GetAccount(account.ID)
		st.IncAccountRequestCount(account.ID)
		return acc.AccessToken, acc, st.Settings().WithProvider(RouteProviderFrom(ctx)), nil
	}
	server := New(st, upstream.New(), ensure)
	server.SetForceRefresh(func(context.Context, string) (string, *store.Account, store.Settings, error) {
		return "", nil, st.Settings().WithProvider(store.ProviderCodex), errors.New("token endpoint timeout")
	})
	authFailed := false
	server.SetAuthFailHandler(func(string, string) bool {
		authFailed = true
		return false
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"codex/gpt-5.6-sol","input":"hi"}`))
	recorder := httptest.NewRecorder()
	server.proxyUpstream(recorder, request, "/responses")

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if authFailed {
		t.Fatal("transient Codex refresh failure invoked permanent auth-fail handler")
	}
	fresh, _ := st.GetAccount(account.ID)
	if fresh.AuthDenied() {
		t.Fatalf("account was poisoned: %s", fresh.AuthDeniedReason)
	}
}

func TestCodexAnthropicNonStreamAggregatesSSE(t *testing.T) {
	isolateHome(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	account := store.Account{
		ID: "codex-anthropic", Provider: store.ProviderCodex, Label: "Codex",
		AccessToken: "codex-token", RefreshToken: "codex-refresh", TeamID: "workspace-1",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := st.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	swapUpstreamClient(t, func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatalf("decode wire body: %v", err)
		}
		response := httptest.NewRecorder()
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.WriteString("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-anthropic\",\"model\":\"gpt-5.6-sol\",\"output\":[{\"type\":\"reasoning\",\"id\":\"rs-1\",\"encrypted_content\":\"ciphertext\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"summary\"}]},{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
		return response.Result(), nil
	})
	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		acc, _ := st.GetAccount(account.ID)
		st.IncAccountRequestCount(account.ID)
		settings := st.Settings().WithProvider(RouteProviderFrom(ctx))
		settings.CodexAccountID = acc.TeamID
		return acc.AccessToken, acc, settings, nil
	}
	server := New(st, upstream.New(), ensure)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.6-sol","stream":false,"max_tokens":100,"thinking":{"type":"enabled","budget_tokens":100},"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("User-Agent", "codex_cli_rs/0.144.0")
	recorder := httptest.NewRecorder()
	server.handleMessages(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"type":"thinking"`) || !strings.Contains(recorder.Body.String(), `"text":"answer"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if wire["stream"] != true {
		t.Fatalf("Codex Anthropic upstream was not forced to SSE: %#v", wire)
	}
	if _, exists := wire["reasoning_effort"]; exists {
		t.Fatalf("unsupported reasoning_effort leaked: %#v", wire)
	}
}

func TestChatCompletionsToCodexResponsesStripsNamespace(t *testing.T) {
	settings := store.Settings{Provider: store.ProviderCodex, DefaultModel: store.CodexDefaultModel}
	body := chatCompletionsBodyToResponses(map[string]any{
		"model":            "codex/gpt-5.6-sol",
		"stream":           true,
		"reasoning_effort": "high",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}, settings)
	if got := asString(body["model"]); got != "gpt-5.6-sol" {
		t.Fatalf("model=%q, want gpt-5.6-sol", got)
	}
	if _, ok := body["input"]; !ok {
		t.Fatalf("responses input missing: %#v", body)
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatalf("unsupported top-level reasoning_effort leaked: %#v", body)
	}
}

func TestAnthropicReasoningSignatureRoundTrip(t *testing.T) {
	item := map[string]any{
		"type": "reasoning", "id": "rs-1", "encrypted_content": "ciphertext",
		"summary": []any{map[string]any{"type": "summary_text", "text": "summary"}},
	}
	signature := codexReasoningSignature(item)
	decoded, ok := reasoningItemFromSignature(signature)
	if !ok || asString(decoded["id"]) != "rs-1" || asString(decoded["encrypted_content"]) != "ciphertext" {
		t.Fatalf("decoded=%#v ok=%v", decoded, ok)
	}
	messages := anthropicToOpenAIMessages(map[string]any{
		"messages": []any{map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "thinking", "thinking": "summary", "signature": signature}},
		}},
	})
	if len(messages) != 1 {
		t.Fatalf("messages=%#v", messages)
	}
	converted := chatCompletionsBodyToResponses(map[string]any{"model": "codex/gpt-5.6-sol", "messages": messages}, store.Settings{Provider: store.ProviderCodex})
	input, _ := converted["input"].([]any)
	if len(input) < 1 || asString(input[0].(map[string]any)["encrypted_content"]) != "ciphertext" {
		t.Fatalf("reasoning item lost: %#v", converted["input"])
	}
	rawResponse, _ := json.Marshal(map[string]any{
		"id": "resp-1", "model": "gpt-5.6-sol",
		"output": []any{item, map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "answer"}},
		}},
	})
	anthropicRaw, err := responsesJSONToAnthropicMessage(rawResponse, "gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	var anthropicMessage map[string]any
	if json.Unmarshal(anthropicRaw, &anthropicMessage) != nil {
		t.Fatalf("invalid Anthropic response: %s", anthropicRaw)
	}
	blocks, _ := anthropicMessage["content"].([]any)
	if len(blocks) < 2 || asString(blocks[0].(map[string]any)["type"]) != "thinking" {
		t.Fatalf("Anthropic reasoning block missing: %s", anthropicRaw)
	}
	if _, ok := reasoningItemFromSignature(asString(blocks[0].(map[string]any)["signature"])); !ok {
		t.Fatalf("Anthropic reasoning signature is not round-trippable: %s", anthropicRaw)
	}
}
