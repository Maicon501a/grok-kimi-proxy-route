package codexauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func TestParseClaimsAndAccount(t *testing.T) {
	exp := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	idToken := jwt(t, map[string]any{
		"email": "dev@example.com",
		"exp":   exp.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":    "user-1",
			"chatgpt_account_id": "account-1",
			"chatgpt_plan_type":  "plus",
		},
	})
	claims := ParseClaims(idToken)
	if claims.Email != "dev@example.com" || claims.UserID != "user-1" || claims.AccountID != "account-1" || claims.PlanType != "plus" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	acc := AccountFromToken(&TokenResponse{IDToken: idToken, AccessToken: jwt(t, map[string]any{"exp": exp.Unix()}), RefreshToken: "rt"})
	if acc.ID != "codex-account-1" || acc.TeamID != "account-1" || acc.Email != "dev@example.com" || !acc.ExpiresAt.Equal(exp) {
		t.Fatalf("unexpected account: %+v", acc)
	}
}

func TestRefreshUsesOfficialJSONShapeAndRotatesToken(t *testing.T) {
	var got map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected request: %s %s %s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(TokenResponse{AccessToken: "new-at", RefreshToken: "new-rt"})
	}))
	defer server.Close()
	c := New()
	c.Issuer = server.URL
	tok, err := c.Refresh(context.Background(), "old-rt")
	if err != nil {
		t.Fatal(err)
	}
	if got["client_id"] != DefaultClientID || got["grant_type"] != "refresh_token" || got["refresh_token"] != "old-rt" {
		t.Fatalf("unexpected refresh payload: %#v", got)
	}
	if tok.AccessToken != "new-at" || tok.RefreshToken != "new-rt" {
		t.Fatalf("unexpected tokens: %+v", tok)
	}
}

func TestStartDeviceUsesOfficialEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/accounts/deviceauth/usercode" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["client_id"] != DefaultClientID {
			t.Fatalf("client_id=%q", body["client_id"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_auth_id": "device-1",
			"user_code":      "ABCD-EFGH",
			"interval":       "3",
		})
	}))
	defer server.Close()
	c := New()
	c.Issuer = server.URL
	start, err := c.StartDevice(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if start.DeviceAuthID != "device-1" || start.UserCode != "ABCD-EFGH" || start.Interval != 3 || start.VerificationURL != server.URL+"/codex/device" {
		t.Fatalf("unexpected device start: %+v", start)
	}
}

func TestRefreshUnauthorizedIsInvalidGrant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer server.Close()
	c := New()
	c.Issuer = server.URL
	_, err := c.Refresh(context.Background(), "revoked-token")
	if err == nil || !IsInvalidGrant(err) {
		t.Fatalf("err=%v, want permanent invalid grant", err)
	}
}

func TestTokenErrorDoesNotExposePartialTokens(t *testing.T) {
	_, err := parseTokenResponse([]byte(`{"refresh_token":"secret-refresh","id_token":"secret-id"}`), http.StatusOK, "codex token exchange")
	if err == nil {
		t.Fatal("expected incomplete token response error")
	}
	if strings.Contains(err.Error(), "secret-refresh") || strings.Contains(err.Error(), "secret-id") {
		t.Fatalf("token leaked in error: %v", err)
	}
}
