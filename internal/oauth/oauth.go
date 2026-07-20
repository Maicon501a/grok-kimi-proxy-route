package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"grok-desktop/internal/store"
)

type DeviceStart struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

type Client struct {
	HTTP       *http.Client
	Issuer     string
	ClientID   string
	Scopes     string
	CLIVersion string
}

func New() *Client {
	return &Client{
		HTTP: &http.Client{
			Timeout: 60 * time.Second,
		},
		Issuer:     store.DefaultIssuer,
		ClientID:   store.DefaultClientID,
		Scopes:     store.DefaultScopes,
		CLIVersion: store.DefaultClientVersion,
	}
}

func (c *Client) headers() http.Header {
	h := make(http.Header)
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	h.Set("User-Agent", "grok-desktop/"+c.CLIVersion)
	h.Set("x-grok-client-version", c.CLIVersion)
	h.Set("x-grok-client-surface", "grok-desktop")
	return h
}

func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header = c.headers()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

func (c *Client) StartDevice(ctx context.Context) (*DeviceStart, error) {
	form := url.Values{}
	form.Set("client_id", c.ClientID)
	form.Set("scope", c.Scopes)
	ep := strings.TrimRight(c.Issuer, "/") + "/oauth2/device/code"
	b, code, err := c.postForm(ctx, ep, form)
	if err != nil {
		return nil, err
	}
	if code >= 400 {
		return nil, fmt.Errorf("device code HTTP %d: %s", code, string(b))
	}
	var out DeviceStart
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return nil, fmt.Errorf("invalid device response: %s", string(b))
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	return &out, nil
}

// PollDevice blocks until authorized, denied, expired, or ctx done.
func (c *Client) PollDevice(ctx context.Context, deviceCode string, intervalSec int) (*TokenResponse, error) {
	if intervalSec <= 0 {
		intervalSec = 5
	}
	ep := strings.TrimRight(c.Issuer, "/") + "/oauth2/token"
	pendingLogs := 0
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(intervalSec) * time.Second):
		}
		form := url.Values{}
		form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
		form.Set("device_code", deviceCode)
		form.Set("client_id", c.ClientID)
		b, code, err := c.postForm(ctx, ep, form)
		if err != nil {
			log.Printf("PollDevice: HTTP error: %v", err)
			return nil, err
		}
		var tok TokenResponse
		_ = json.Unmarshal(b, &tok)
		if tok.Error != "" {
			switch tok.Error {
			case "authorization_pending":
				pendingLogs++
				// spam control: first, then every 6th (~30s at 5s interval)
				if pendingLogs == 1 || pendingLogs%6 == 0 {
					log.Printf("PollDevice: still pending (n=%d) %s", pendingLogs, tok.ErrorDesc)
				}
				continue
			case "slow_down":
				intervalSec += 5
				log.Printf("PollDevice: slow_down → interval=%ds", intervalSec)
				continue
			default:
				log.Printf("PollDevice: error=%s desc=%s", tok.Error, tok.ErrorDesc)
				return nil, fmt.Errorf("%s: %s", tok.Error, tok.ErrorDesc)
			}
		}
		if code >= 400 {
			// try parse soft errors
			if strings.Contains(string(b), "authorization_pending") {
				pendingLogs++
				if pendingLogs == 1 || pendingLogs%6 == 0 {
					log.Printf("PollDevice: still pending HTTP (n=%d)", pendingLogs)
				}
				continue
			}
			return nil, fmt.Errorf("token HTTP %d: %s", code, string(b))
		}
		if tok.AccessToken == "" {
			continue
		}
		if tok.ExpiresIn <= 0 {
			tok.ExpiresIn = 21600
		}
		log.Printf("PollDevice: success token_len=%d", len(tok.AccessToken))
		return &tok, nil
	}
}

func (c *Client) Refresh(ctx context.Context, refreshToken, clientID, issuer string) (*TokenResponse, error) {
	if clientID == "" {
		clientID = c.ClientID
	}
	if issuer == "" {
		issuer = c.Issuer
	}
	ep := strings.TrimRight(issuer, "/") + "/oauth2/token"
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	b, code, err := c.postForm(ctx, ep, form)
	if err != nil {
		return nil, err
	}
	var tok TokenResponse
	_ = json.Unmarshal(b, &tok)
	if tok.Error != "" {
		return nil, fmt.Errorf("%s: %s", tok.Error, tok.ErrorDesc)
	}
	if code >= 400 || tok.AccessToken == "" {
		return nil, fmt.Errorf("refresh HTTP %d: %s", code, string(b))
	}
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 21600
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	return &tok, nil
}

// IsInvalidGrant reports OAuth invalid_grant / revoked refresh-token errors.
// These are permanent for the stored refresh_token: mark the account auth-denied
// and rotate, or re-sync from the official Grok CLI (~/.grok/auth.json).
func IsInvalidGrant(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "invalid_grant") ||
		strings.Contains(s, "refresh token has been revoked") ||
		strings.Contains(s, "token has been revoked") ||
		strings.Contains(s, "refresh_token is invalid")
}

func (c *Client) UserInfo(ctx context.Context, accessToken, issuer string) (email, userID string) {
	if issuer == "" {
		issuer = c.Issuer
	}
	ep := strings.TrimRight(issuer, "/") + "/oauth2/userinfo"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep, nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return "", ""
	}
	if e, ok := m["email"].(string); ok {
		email = e
	}
	if s, ok := m["sub"].(string); ok {
		userID = s
	}
	return email, userID
}

// AccessClaims are the JWT claims we care about for routing / abuse detection.
type AccessClaims struct {
	TeamID  string
	UserID  string
	Tier    int
	BotFlag int
	Exp     time.Time
	Scope   string
}

func ClaimsFromAccess(access string) (teamID, userID string) {
	c := ParseAccessClaims(access)
	return c.TeamID, c.UserID
}

// ParseAccessClaims decodes the JWT payload without verifying the signature
// (tokens are only used as opaque bearer credentials against xAI).
func ParseAccessClaims(access string) AccessClaims {
	var out AccessClaims
	parts := strings.Split(access, ".")
	if len(parts) < 2 {
		return out
	}
	payload := parts[1]
	if m := len(payload) % 4; m != 0 {
		payload += strings.Repeat("=", 4-m)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return out
		}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return out
	}
	if v, ok := m["team_id"].(string); ok {
		out.TeamID = v
	}
	if v, ok := m["sub"].(string); ok {
		out.UserID = v
	}
	if v, ok := m["scope"].(string); ok {
		out.Scope = v
	}
	out.Tier = claimInt(m["tier"])
	out.BotFlag = claimInt(m["bot_flag_source"])
	if exp := claimInt64(m["exp"]); exp > 0 {
		out.Exp = time.Unix(exp, 0).UTC()
	}
	return out
}

// BotFlagged reports whether the access token carries an anti-abuse bot flag.
// Such tokens often still work on /models but get 403 on chat/responses.
func BotFlagged(access string) bool {
	return ParseAccessClaims(access).BotFlag != 0
}

func claimInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

func claimInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

func AccountFromToken(tok *TokenResponse, clientID, issuer string) store.Account {
	if clientID == "" {
		clientID = store.DefaultClientID
	}
	if issuer == "" {
		issuer = store.DefaultIssuer
	}
	claims := ParseAccessClaims(tok.AccessToken)
	id := claims.UserID
	if id == "" {
		id = fmt.Sprintf("acc_%d", time.Now().UnixNano())
	}
	exp := time.Now().UTC().Add(time.Duration(tok.ExpiresIn) * time.Second)
	if !claims.Exp.IsZero() {
		// Prefer JWT exp when present (authoritative server-side lifetime).
		exp = claims.Exp
	}
	return store.Account{
		ID:           id,
		Label:        "Grok account",
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    exp,
		ClientID:     clientID,
		Issuer:       issuer,
		Scope:        tok.Scope,
		TeamID:       claims.TeamID,
		UserID:       claims.UserID,
	}
}
