package codexauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"grok-desktop/internal/store"
)

const (
	DefaultIssuer   = "https://auth.openai.com"
	DefaultClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultScope    = "openid profile email offline_access api.connectors.read api.connectors.invoke"
)

type Client struct {
	HTTP     *http.Client
	Issuer   string
	ClientID string
}

type DeviceStart struct {
	DeviceAuthID    string `json:"device_auth_id"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

type TokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

type TokenError struct {
	Operation string
	Status    int
	Detail    string
}

func (e *TokenError) Error() string {
	return fmt.Sprintf("%s HTTP %d: %s", e.Operation, e.Status, e.Detail)
}

type Claims struct {
	Email     string
	UserID    string
	AccountID string
	PlanType  string
	FedRAMP   bool
	ExpiresAt time.Time
}

func New() *Client {
	return &Client{
		HTTP:     &http.Client{Timeout: 60 * time.Second},
		Issuer:   DefaultIssuer,
		ClientID: DefaultClientID,
	}
}

func (c *Client) StartDevice(ctx context.Context) (*DeviceStart, error) {
	payload, _ := json.Marshal(map[string]string{"client_id": c.clientID()})
	b, status, err := c.do(ctx, http.MethodPost, c.endpoint("/api/accounts/deviceauth/usercode"), "application/json", payload)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("codex device login HTTP %d: %s", status, safeBody(b))
	}
	var raw struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		UserCodeAlt  string `json:"usercode"`
		Interval     any    `json:"interval"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("codex device login: %w", err)
	}
	if raw.UserCode == "" {
		raw.UserCode = raw.UserCodeAlt
	}
	interval := 5
	switch value := raw.Interval.(type) {
	case string:
		if n, err := time.ParseDuration(value + "s"); err == nil && n > 0 {
			interval = int(n / time.Second)
		}
	case float64:
		if value > 0 {
			interval = int(value)
		}
	}
	if raw.DeviceAuthID == "" || raw.UserCode == "" {
		return nil, fmt.Errorf("codex device login returned an incomplete response")
	}
	return &DeviceStart{
		DeviceAuthID:    raw.DeviceAuthID,
		UserCode:        raw.UserCode,
		VerificationURL: c.endpoint("/codex/device"),
		Interval:        interval,
		ExpiresIn:       15 * 60,
	}, nil
}

func (c *Client) PollDevice(ctx context.Context, start DeviceStart) (*TokenResponse, error) {
	interval := start.Interval
	if interval <= 0 {
		interval = 5
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(interval) * time.Second):
		}

		payload, _ := json.Marshal(map[string]string{
			"device_auth_id": start.DeviceAuthID,
			"user_code":      start.UserCode,
		})
		b, status, err := c.do(ctx, http.MethodPost, c.endpoint("/api/accounts/deviceauth/token"), "application/json", payload)
		if err != nil {
			return nil, err
		}
		if status == http.StatusForbidden || status == http.StatusNotFound {
			continue
		}
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("codex device authorization HTTP %d: %s", status, safeBody(b))
		}
		var code struct {
			AuthorizationCode string `json:"authorization_code"`
			CodeVerifier      string `json:"code_verifier"`
		}
		if err := json.Unmarshal(b, &code); err != nil {
			return nil, fmt.Errorf("codex device authorization: %w", err)
		}
		if code.AuthorizationCode == "" || code.CodeVerifier == "" {
			return nil, fmt.Errorf("codex device authorization returned an incomplete response")
		}
		return c.exchange(ctx, code.AuthorizationCode, code.CodeVerifier)
	}
}

func (c *Client) exchange(ctx context.Context, code, verifier string) (*TokenResponse, error) {
	return c.exchangeWithRedirect(ctx, code, verifier, c.endpoint("/deviceauth/callback"))
}

func (c *Client) exchangeWithRedirect(ctx context.Context, code, verifier, redirectURI string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", c.clientID())
	form.Set("code_verifier", verifier)
	b, status, err := c.do(ctx, http.MethodPost, c.endpoint("/oauth/token"), "application/x-www-form-urlencoded", []byte(form.Encode()))
	if err != nil {
		return nil, err
	}
	return parseTokenResponse(b, status, "codex token exchange")
}

func (c *Client) Refresh(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	payload, _ := json.Marshal(map[string]string{
		"client_id":     c.clientID(),
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
	b, status, err := c.do(ctx, http.MethodPost, c.endpoint("/oauth/token"), "application/json", payload)
	if err != nil {
		return nil, err
	}
	tok, err := parseTokenResponse(b, status, "codex token refresh")
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	return tok, nil
}

func (c *Client) do(ctx context.Context, method, endpoint, contentType string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "codex_cli_rs/0.144.0")
	req.Header.Set("originator", "codex_cli_rs")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, err
}

func parseTokenResponse(b []byte, status int, operation string) (*TokenResponse, error) {
	var tok TokenResponse
	_ = json.Unmarshal(b, &tok)
	if status < 200 || status >= 300 || tok.AccessToken == "" {
		detail := firstNonEmpty(tok.ErrorDesc, tok.Error)
		if detail == "" {
			detail = "token endpoint returned an invalid response"
		}
		return nil, &TokenError{Operation: operation, Status: status, Detail: detail}
	}
	return &tok, nil
}

func ParseClaims(jwt string) Claims {
	var out Claims
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return out
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return out
	}
	var payload struct {
		Email            string `json:"email"`
		Expires          int64  `json:"exp"`
		ChatGPTUserID    string `json:"chatgpt_user_id"`
		UserID           string `json:"user_id"`
		ChatGPTAccountID string `json:"chatgpt_account_id"`
		PlanType         string `json:"chatgpt_plan_type"`
		FedRAMP          bool   `json:"chatgpt_account_is_fedramp"`
		Profile          struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
		Auth struct {
			ChatGPTUserID    string `json:"chatgpt_user_id"`
			UserID           string `json:"user_id"`
			ChatGPTAccountID string `json:"chatgpt_account_id"`
			PlanType         string `json:"chatgpt_plan_type"`
			FedRAMP          bool   `json:"chatgpt_account_is_fedramp"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return out
	}
	out.Email = firstNonEmpty(payload.Email, payload.Profile.Email)
	out.UserID = firstNonEmpty(payload.Auth.ChatGPTUserID, payload.Auth.UserID, payload.ChatGPTUserID, payload.UserID)
	out.AccountID = firstNonEmpty(payload.Auth.ChatGPTAccountID, payload.ChatGPTAccountID)
	out.PlanType = firstNonEmpty(payload.Auth.PlanType, payload.PlanType)
	out.FedRAMP = payload.Auth.FedRAMP || payload.FedRAMP
	if payload.Expires > 0 {
		out.ExpiresAt = time.Unix(payload.Expires, 0).UTC()
	}
	return out
}

func AccountFromToken(tok *TokenResponse) store.Account {
	claims := ParseClaims(tok.IDToken)
	accessClaims := ParseClaims(tok.AccessToken)
	claims.Email = firstNonEmpty(claims.Email, accessClaims.Email)
	claims.UserID = firstNonEmpty(claims.UserID, accessClaims.UserID)
	claims.AccountID = firstNonEmpty(claims.AccountID, accessClaims.AccountID)
	claims.PlanType = firstNonEmpty(claims.PlanType, accessClaims.PlanType)
	claims.FedRAMP = claims.FedRAMP || accessClaims.FedRAMP
	if claims.ExpiresAt.IsZero() {
		claims.ExpiresAt = accessClaims.ExpiresAt
	}
	idPart := firstNonEmpty(claims.AccountID, claims.UserID)
	if idPart == "" {
		idPart = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	label := firstNonEmpty(claims.Email, "ChatGPT Codex")
	now := time.Now().UTC()
	source := "codex_oauth"
	if claims.FedRAMP {
		source = "codex_oauth_fedramp"
	}
	return store.Account{
		ID:           "codex-" + idPart,
		Provider:     store.ProviderCodex,
		Label:        label,
		Email:        claims.Email,
		TeamID:       claims.AccountID,
		UserID:       claims.UserID,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    claims.ExpiresAt,
		ClientID:     DefaultClientID,
		Issuer:       DefaultIssuer,
		Scope:        DefaultScope,
		Source:       source,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

func IsInvalidGrant(err error) bool {
	if err == nil {
		return false
	}
	var tokenErr *TokenError
	if errors.As(err, &tokenErr) && tokenErr.Status == http.StatusUnauthorized {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "invalid_grant") ||
		strings.Contains(s, "refresh_token_expired") ||
		strings.Contains(s, "refresh_token_reused") ||
		strings.Contains(s, "refresh_token_invalidated") ||
		strings.Contains(s, "refresh token") && (strings.Contains(s, "expired") || strings.Contains(s, "revoked"))
}

func (c *Client) endpoint(path string) string {
	issuer := strings.TrimRight(strings.TrimSpace(c.Issuer), "/")
	if issuer == "" {
		issuer = DefaultIssuer
	}
	return issuer + path
}

func (c *Client) clientID() string {
	if id := strings.TrimSpace(c.ClientID); id != "" {
		return id
	}
	return DefaultClientID
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func safeBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
