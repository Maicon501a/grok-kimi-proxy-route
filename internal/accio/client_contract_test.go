package accio

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildRequestUsesOfficialADKFieldNames(t *testing.T) {
	request := buildRequest(map[string]any{
		"model": "accio/1Nexus-R36W8qJ5vB6h",
		"messages": []any{
			map[string]any{"role": "system", "content": "system"},
			map[string]any{"role": "user", "content": "hello"},
		},
		"temperature":      0.2,
		"top_p":            0.8,
		"max_tokens":       123,
		"reasoning_effort": "high",
	}, "test-token")

	if request["model"] != "1Nexus-R36W8qJ5vB6h" {
		t.Fatalf("model = %#v", request["model"])
	}
	for _, key := range []string{"requestId", "userRequestId", "messageId", "conversationId", "sessionKey", "systemInstruction", "maxOutputTokens", "reasoningEffort"} {
		if _, ok := request[key]; !ok {
			t.Fatalf("missing ADK field %q in %#v", key, request)
		}
	}
	for _, key := range []string{"request_id", "user_request_id", "message_id", "conversation_id", "session_key", "system_instruction", "max_output_tokens", "reasoning_effort"} {
		if _, ok := request[key]; ok {
			t.Fatalf("unexpected snake_case ADK field %q in %#v", key, request)
		}
	}

	contents, ok := request["contents"].([]map[string]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("contents = %#v", request["contents"])
	}
	if contents[0]["role"] != "user" {
		t.Fatalf("user role = %#v", contents[0]["role"])
	}
	parts, ok := contents[0]["parts"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("parts = %#v", contents[0]["parts"])
	}
	part, ok := parts[0].(map[string]any)
	if !ok || part["text"] != "hello" {
		t.Fatalf("part = %#v", parts[0])
	}

	if _, err := json.Marshal(request); err != nil {
		t.Fatalf("marshal request: %v", err)
	}
}

func TestBuildRequestOmitsZeroMaxTokens(t *testing.T) {
	request := buildRequest(map[string]any{
		"model": "accio/1Nova-Q3xM8vJ1rH6z",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
		// This is what json.Marshal(ChatRequest{}) produces for an omitted
		// max_tokens value on the desktop UI path.
		"max_tokens": float64(0),
	}, "test-token")

	if _, ok := request["maxOutputTokens"]; ok {
		t.Fatalf("zero max_tokens must not become maxOutputTokens=0: %#v", request)
	}
}

func TestCallbackRelayExistsForBrowserFragments(t *testing.T) {
	if !strings.Contains(callbackRelayHTML, "location.hash") || !strings.Contains(callbackRelayHTML, "fetch(location.pathname") {
		t.Fatal("callback relay does not forward browser fragment to the local listener")
	}
	r := httptest.NewRequest("GET", "http://localhost:1234/auth/callback#accessToken=fragment-token", nil)
	access, _, _, _ := extractCallbackToken(r)
	if access != "" {
		t.Fatal("server must not receive URL fragments directly")
	}
}

func TestPKCEChallengeAndLoginURL(t *testing.T) {
	verifier, challenge, err := pkceChallengePair()
	if err != nil {
		t.Fatal(err)
	}
	if len(verifier) < 43 || len(verifier) > 128 {
		t.Fatalf("invalid verifier length: %d", len(verifier))
	}
	sum := sha256.Sum256([]byte(verifier))
	expected := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != expected {
		t.Fatalf("challenge mismatch: got %q want %q", challenge, expected)
	}
	login := buildPKCELoginURL("http://localhost:3456/auth/callback", challenge, "state-123")
	u, err := url.Parse(login)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for key, want := range map[string]string{
		"return_url":            "http://localhost:3456/auth/callback",
		"state":                 "state-123",
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"client_id":             OAuthClientID,
	} {
		if q.Get(key) != want {
			t.Fatalf("%s = %q, want %q", key, q.Get(key), want)
		}
	}
}

func TestParseModelCatalogAcceptsNestedGatewayShapes(t *testing.T) {
	got := parseModelCatalog(map[string]any{
		"data": map[string]any{
			"providers": []any{
				map[string]any{
					"providerDisplayName": "Available models",
					"models": []any{
						map[string]any{
							"modelCode": "gpt-4o", "modelDisplayName": "GPT-4o",
							"contextWindow":          float64(100000),
							"reasoningEfforts":       []any{"low", "medium", "high", "high", ""},
							"defaultReasoningEffort": "medium", "freeUse": true, "locked": false,
						},
						map[string]any{"modelId": "claude-3-7", "displayName": "Claude 3.7"},
						map[string]any{"modelCode": "hidden", "modelDisplayName": "Hidden", "visible": false},
					},
				},
			},
		},
		"models": []any{
			map[string]any{"modelName": "1Nexus-R36W8qJ5vB6h", "name": "Accio Nexus"},
		},
	})
	if len(got) != 3 {
		t.Fatalf("got %d models: %#v", len(got), got)
	}
	wantIDs := map[string]bool{
		"accio/gpt-4o":              true,
		"accio/claude-3-7":          true,
		"accio/1Nexus-R36W8qJ5vB6h": true,
	}
	for _, model := range got {
		if !wantIDs[model.ID] {
			t.Fatalf("unexpected model: %#v", model)
		}
		if model.ID == "accio/gpt-4o" {
			if model.Context != 100000 || model.DefaultReasoningEffort != "medium" || !model.FreeUse || model.Locked {
				t.Fatalf("metadata = %#v", model)
			}
			if len(model.ReasoningEfforts) != 3 || model.ReasoningEfforts[0] != "low" || model.ReasoningEfforts[2] != "high" {
				t.Fatalf("reasoning efforts = %#v", model.ReasoningEfforts)
			}
		}
	}
}

func TestBuildRequestEnablesIncrementalStreaming(t *testing.T) {
	out := buildRequest(map[string]any{
		"model":    "accio/1Nova-Q3xM8vJ1rH6z",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, "test-token")
	if out["incremental"] != true {
		t.Fatalf("incremental = %#v", out["incremental"])
	}
}

func TestReadFramesTerminatesOnDoneMarker(t *testing.T) {
	var done int
	var text string
	raw := "data: {\"content\":{\"parts\":[{\"text\":\"hello\"}]}}\n\ndata: [DONE]\n\n"
	c := &Client{}
	if err := c.readFrames(strings.NewReader(raw), func(t, _ string, _ map[string]any, finished bool) {
		text += t
		if finished {
			done++
		}
	}); err != nil {
		t.Fatal(err)
	}
	if text != "hello" || done != 1 {
		t.Fatalf("text=%q done=%d", text, done)
	}
}

func TestReadFramesTerminatesOnEOF(t *testing.T) {
	done := 0
	c := &Client{}
	raw := "data: {\"content\":{\"parts\":[{\"text\":\"hello\"}]}}\n"
	if err := c.readFrames(strings.NewReader(raw), func(_, _ string, _ map[string]any, finished bool) {
		if finished {
			done++
		}
	}); err != nil {
		t.Fatal(err)
	}
	if done != 1 {
		t.Fatalf("done=%d", done)
	}
}

func TestReadFramesParsesNestedCandidateContent(t *testing.T) {
	var text string
	c := &Client{}
	raw := "data: {\"candidate\":{\"content\":{\"parts\":[{\"text\":\"nested\"}]}}}\n"
	if err := c.readFrames(strings.NewReader(raw), func(t, _ string, _ map[string]any, _ bool) {
		text += t
	}); err != nil {
		t.Fatal(err)
	}
	if text != "nested" {
		t.Fatalf("text=%q", text)
	}
}

func TestReadFramesRejectsHTMLChallenge(t *testing.T) {
	c := &Client{}
	raw := "<!doctype html><html><body>challenge</body></html>\n"
	if err := c.readFrames(strings.NewReader(raw), func(_, _ string, _ map[string]any, _ bool) {}); err == nil || !strings.Contains(err.Error(), "HTML challenge") {
		t.Fatalf("err=%v", err)
	}
}

func TestModelsWithTokenUsesOfficialCatalogRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/llm/config/v2" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["token"] != "test-token" {
			t.Fatalf("token payload missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"providers":[{"provider":"accio","providerDisplayName":"Accio","modelList":[{"modelCode":"gpt-5-6-luna","modelName":"GPT 5.6 Luna","visible":true},{"modelCode":"hidden","modelName":"Hidden","visible":false}]}]}`))
	}))
	defer srv.Close()
	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.httpClient = srv.Client()
	c.modelCatalogURL = srv.URL + "/llm/config/v2"
	models, status, err := c.modelsWithToken(context.Background(), "test-token")
	if err != nil || status != http.StatusOK {
		t.Fatalf("models = %#v, status=%d, err=%v", models, status, err)
	}
	if len(models) != 1 || models[0].ID != "accio/gpt-5-6-luna" || models[0].Name != "GPT 5.6 Luna" {
		t.Fatalf("models = %#v", models)
	}
}

func TestModelRoutingWithTokenParsesDynamicModel(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/tool/rlab/call" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"payload":{"modelCode":"1Nova-bW7yT4kL9pN2","modelName":"deepseek-v4-pro"}}}`))
	}))
	defer srv.Close()

	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.httpClient = srv.Client()
	c.modelRoutingURL = srv.URL + "/tool/rlab/call"
	model, err := c.modelRoutingWithToken(context.Background(), "test-token")
	if err != nil {
		t.Fatal(err)
	}
	if model.ID != "accio/1Nova-bW7yT4kL9pN2" || model.Name != "deepseek-v4-pro" {
		t.Fatalf("model = %#v", model)
	}
	if got["function"] != "model_routing" {
		t.Fatalf("body = %#v", got)
	}
}

func TestRemoveAccountDeletesFileAndRepairsActivePointer(t *testing.T) {
	dir := t.TempDir()
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.writeToken(TokenRecord{ID: "accio-a", AccessToken: "access-a"}); err != nil {
		t.Fatal(err)
	}
	if err := c.writeToken(TokenRecord{ID: "accio-b", AccessToken: "access-b"}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetActiveAccount("accio-a"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveAccount("accio-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.accountPath("accio-a")); !os.IsNotExist(err) {
		t.Fatalf("removed account file still exists: %v", err)
	}
	active, err := os.ReadFile(c.activePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(active) != "accio-b" {
		t.Fatalf("active account = %q, want accio-b", string(active))
	}
}

func TestExchangeCodeForTokenUsesPKCEContract(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if gotErr := json.NewDecoder(r.Body).Decode(&got); gotErr != nil {
			t.Errorf("decode request: %v", gotErr)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"accessToken":"access-1","refreshToken":"refresh-1","expiresAt":123}}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.tokenExchangeURL = srv.URL
	c.httpClient = srv.Client()
	pending := pendingLogin{codeVerifier: "verifier-1", state: "state-1", redirectURI: "http://localhost:1234/auth/callback"}
	token, err := c.exchangeCodeForToken(context.Background(), "code-1", pending)
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "access-1" || token.RefreshToken != "refresh-1" {
		t.Fatalf("token = %#v", token)
	}
	for key, want := range map[string]string{
		"code":         "code-1",
		"codeVerifier": "verifier-1",
		"clientId":     OAuthClientID,
		"redirectUri":  pending.redirectURI,
	} {
		if got[key] != want {
			t.Fatalf("%s = %#v, want %q", key, got[key], want)
		}
	}
	if got["utdid"] == nil || got["version"] == nil {
		t.Fatalf("device metadata missing: %#v", got)
	}
}

func TestLocalCallbackRejectsWrongStateAndCompletesPKCELogin(t *testing.T) {
	var exchange map[string]any
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&exchange); err != nil {
			t.Errorf("decode exchange: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"accessToken":"callback-access","refreshToken":"callback-refresh"}}`))
	}))
	defer tokenServer.Close()

	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.tokenExchangeURL = tokenServer.URL
	c.httpClient = tokenServer.Client()
	loginURL, err := c.StartLogin(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	login, err := url.Parse(loginURL)
	if err != nil {
		t.Fatal(err)
	}
	redirect, err := url.Parse(login.Query().Get("return_url"))
	if err != nil {
		t.Fatal(err)
	}
	wrong := *redirect
	wrongQuery := wrong.Query()
	wrongQuery.Set("code", "should-not-exchange")
	wrongQuery.Set("state", "wrong-state")
	wrong.RawQuery = wrongQuery.Encode()
	wrongResp, wrongErr := waitForCallback(t, wrong.String(), http.StatusBadRequest)
	if wrongErr != nil {
		t.Fatal(wrongErr)
	}
	_ = wrongResp.Body.Close()
	if exchange != nil {
		t.Fatalf("wrong state reached token endpoint: %#v", exchange)
	}

	correct := *redirect
	correctQuery := correct.Query()
	correctQuery.Set("code", "callback-code")
	correctQuery.Set("state", login.Query().Get("state"))
	correct.RawQuery = correctQuery.Encode()
	resp, err := waitForCallback(t, correct.String(), http.StatusOK)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if exchange["code"] != "callback-code" || exchange["codeVerifier"] == "" {
		t.Fatalf("invalid exchange payload: %#v", exchange)
	}
	got, err := c.CurrentAccount()
	if err != nil || got.AccessToken != "callback-access" {
		t.Fatalf("current account = %#v, err = %v", got, err)
	}
}

func waitForCallback(t *testing.T, callbackURL string, wantStatus int) (*http.Response, error) {
	t.Helper()
	var lastErr error
	for i := 0; i < 40; i++ {
		resp, err := http.Get(callbackURL)
		if err == nil {
			if resp.StatusCode != wantStatus {
				_ = resp.Body.Close()
				return nil, fmt.Errorf("callback status = %d, want %d", resp.StatusCode, wantStatus)
			}
			return resp, nil
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	return nil, lastErr
}

func TestSetTokenFiresLoginAndPersistsAccount(t *testing.T) {
	dir := t.TempDir()
	c, err := New(filepath.Join(dir, "accio"))
	if err != nil {
		t.Fatal(err)
	}
	called := make(chan TokenRecord, 1)
	c.OnLogin(func(t TokenRecord) { called <- t })
	if err := c.SetToken("access-event", "refresh-event", time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-called:
		if got.AccessToken != "access-event" {
			t.Fatalf("callback token = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("login callback not fired")
	}
	if _, err := os.Stat(c.accountPath(accountIDForToken("access-event"))); err != nil {
		t.Fatalf("account was not persisted: %v", err)
	}
	if got, err := c.CurrentAccount(); err != nil || got.AccessToken != "access-event" {
		t.Fatalf("current account = %#v, err = %v", got, err)
	}
}

func TestMessagePartsUseOfficialADKNestedNames(t *testing.T) {
	parts := messageParts(map[string]any{
		"role":    "assistant",
		"content": "answer",
		"tool_calls": []any{
			map[string]any{
				"id": "call-1",
				"function": map[string]any{
					"name":      "lookup",
					"arguments": `{"q":"x"}`,
				},
			},
		},
	})
	if len(parts) != 2 {
		t.Fatalf("parts = %#v", parts)
	}
	call, ok := parts[1].(map[string]any)["functionCall"].(map[string]any)
	if !ok {
		t.Fatalf("functionCall missing: %#v", parts[1])
	}
	if call["argsJson"] != `{"q":"x"}` {
		t.Fatalf("argsJson = %#v", call["argsJson"])
	}
	if _, ok := parts[1].(map[string]any)["function_call"]; ok {
		t.Fatal("snake_case function_call must not be emitted")
	}
}

// TestReadGatewayCLIConfigFallback verifies that readGatewayCLIConfig finds
// the gateway token + URL from ~/.accio/accounts/<id>/.accio/runtime/gateway-cli.json
// even when ACCIO_GATEWAY_TOKEN is not in the environment (the normal case for
// the standalone GrokDesktop.exe process, which does not inherit the Accio
// Desktop parent's env). Without this fallback the local gateway is skipped
// and every request hits the captcha-protected remote endpoint.
func TestReadGatewayCLIConfigFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ACCIO_GATEWAY_TOKEN", "")
	t.Setenv("ACCIO_LOCAL_GATEWAY_URL", "")

	// Older entry — must NOT be picked (newer one wins).
	oldDir := filepath.Join(home, ".accio", "accounts", "111", ".accio", "runtime")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldCfg := `{"url":"http://localhost:4097","wsUrl":"","authMode":"basic","username":"phoenix","password":"old-token","pid":1,"createdAt":"2026-07-20T00:00:00.000Z"}`
	if err := os.WriteFile(filepath.Join(oldDir, "gateway-cli.json"), []byte(oldCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// Newer entry — must be picked.
	newDir := filepath.Join(home, ".accio", "accounts", "222", ".accio", "runtime")
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		t.Fatal(err)
	}
	newCfg := `{"url":"http://localhost:4097","wsUrl":"ws://localhost:4097/websocket/connect","authMode":"basic","username":"phoenix","password":"fresh-token","pid":2,"createdAt":"2026-07-27T13:40:31.179Z"}`
	if err := os.WriteFile(filepath.Join(newDir, "gateway-cli.json"), []byte(newCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	token, gwURL := readGatewayCLIConfig()
	if token != "fresh-token" {
		t.Fatalf("token = %q, want fresh-token", token)
	}
	if gwURL != "http://localhost:4097" {
		t.Fatalf("url = %q, want http://localhost:4097", gwURL)
	}

	// localGatewayConfig must also return the disk fallback.
	gotToken, gotURL := localGatewayConfig()
	if gotToken != "fresh-token" {
		t.Fatalf("localGatewayConfig token = %q", gotToken)
	}
	if gotURL != "http://localhost:4097" {
		t.Fatalf("localGatewayConfig url = %q", gotURL)
	}
	if !localGatewayAvailable() {
		t.Fatal("localGatewayAvailable should be true with disk fallback")
	}
}
