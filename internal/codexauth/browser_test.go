package codexauth

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestBrowserAuthorizeURLMatchesOfficialContract(t *testing.T) {
	c := New()
	c.Issuer = "https://auth.example.test"
	raw := c.buildAuthorizeURL("http://localhost:1455/auth/callback", "challenge", "state-value")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	checks := map[string]string{
		"response_type":              "code",
		"client_id":                  DefaultClientID,
		"redirect_uri":               "http://localhost:1455/auth/callback",
		"scope":                      DefaultScope,
		"code_challenge":             "challenge",
		"code_challenge_method":      "S256",
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
		"state":                      "state-value",
		"originator":                 "codex_cli_rs",
	}
	for key, want := range checks {
		if got := query.Get(key); got != want {
			t.Fatalf("%s=%q, want %q", key, got, want)
		}
	}
}

func TestBrowserCallbackExchangesCode(t *testing.T) {
	var exchangeForm url.Values
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		exchangeForm = r.PostForm
		_ = json.NewEncoder(w).Encode(TokenResponse{
			IDToken: "id-token", AccessToken: "access-token", RefreshToken: "refresh-token",
		})
	}))
	defer tokenServer.Close()
	c := New()
	c.Issuer = tokenServer.URL
	c.HTTP = tokenServer.Client()
	login, err := c.startBrowser([]int{0})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan browserLoginResult, 1)
	go func() {
		tokens, waitErr := login.Wait(ctx)
		result <- browserLoginResult{tokens: tokens, err: waitErr}
	}()
	callbackURL := login.RedirectURI + "?" + url.Values{
		"code":  {"authorization-code"},
		"state": {login.state},
	}.Encode()
	response, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Get(callbackURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	completed := <-result
	if completed.err != nil {
		t.Fatal(completed.err)
	}
	if completed.tokens == nil || completed.tokens.AccessToken != "access-token" || completed.tokens.RefreshToken != "refresh-token" {
		t.Fatalf("tokens=%#v", completed.tokens)
	}
	if exchangeForm.Get("grant_type") != "authorization_code" ||
		exchangeForm.Get("code") != "authorization-code" ||
		exchangeForm.Get("redirect_uri") != login.RedirectURI ||
		exchangeForm.Get("client_id") != DefaultClientID ||
		exchangeForm.Get("code_verifier") != login.codeVerifier {
		t.Fatalf("exchange form=%#v", exchangeForm)
	}
}

func TestBrowserLoginRetriesRecentlyReleasedPort(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := occupied.Addr().(*net.TCPAddr).Port
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = occupied.Close()
	}()
	login, err := New().startBrowser([]int{port})
	if err != nil {
		t.Fatal(err)
	}
	_ = login.listener.Close()
}
