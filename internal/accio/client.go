package accio

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	cryptoRand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"grok-desktop/internal/logging"
)

const (
	Provider              = "accio"
	DefaultModel          = "accio/1Nexus-R36W8qJ5vB6h"
	GatewayBase           = "https://phoenix-gw.alibaba.com/api"
	GatewayLLM            = GatewayBase + "/adk/llm"
	RefreshURL            = GatewayBase + "/auth/safe/refresh_token"
	CreditsURL            = GatewayBase + "/entitlement/currentSubscription"
	CreditsQuotaURL       = GatewayBase + "/entitlement/quota"
	UserInfoURL           = GatewayBase + "/auth/safe/userinfo"
	LoginBase             = "https://www.accio.com"
	DefaultAppVersion     = "0.25.0"
	DefaultSecurityAppKey = "35336201"
	// OAuthClientID is the public client_id used by the official Accio desktop
	// app, extracted via reverse-engineering of app.asar (Hne = "accio-work").
	// It is a public OAuth client (no client_secret), using PKCE.
	OAuthClientID = "accio-work"
	// TokenExchangeURL is the PKCE token exchange endpoint discovered in the
	// official app.asar (Sc.tokenExchange = "/api/oauth/token").
	TokenExchangeURL = GatewayBase + "/oauth/token"
)

// pendingLogin holds the PKCE state for an in-flight login flow.
type pendingLogin struct {
	codeVerifier string
	state        string
	redirectURI  string
}

// pkceChallengePair generates a PKCE code_verifier (48 random bytes → base64url)
// and the corresponding code_challenge (SHA-256 of verifier → base64url, S256).
func pkceChallengePair() (verifier, challenge string, err error) {
	b := make([]byte, 48)
	if _, err = cryptoRand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64URLEncode(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64URLEncode(sum[:])
	return verifier, challenge, nil
}

// base64URLEncode encodes bytes using base64url without padding.
func base64URLEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

type TokenRecord struct {
	ID           string `json:"id,omitempty"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	Cookie       string `json:"cookie,omitempty"`
	ExpiresAt    any    `json:"expiresAt,omitempty"`
	SavedAt      int64  `json:"savedAt"`
	// Email/Label are populated from userinfo after login so multiple accounts
	// are distinguishable in the pool instead of overwriting each other.
	Email string `json:"email,omitempty"`
	Label string `json:"label,omitempty"`
}

type Model struct {
	ID                     string
	Name                   string
	Provider               string
	Description            string
	Context                int64
	Multimodal             bool
	ReasoningEfforts       []string
	DefaultReasoningEffort string
	FreeUse                bool
	Locked                 bool
}

type GatewayError struct {
	Status int
	Raw    string
	Cause  error
}

func (e *GatewayError) Error() string {
	if e == nil {
		return "accio gateway error"
	}
	if strings.TrimSpace(e.Raw) != "" {
		return fmt.Sprintf("accio gateway HTTP %d: %s", e.Status, e.Raw)
	}
	if e.Cause != nil {
		return fmt.Sprintf("accio gateway HTTP %d: %v", e.Status, e.Cause)
	}
	return fmt.Sprintf("accio gateway HTTP %d", e.Status)
}

func (e *GatewayError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func GatewayRawError(err error) string {
	var ge *GatewayError
	if errors.As(err, &ge) && ge != nil && ge.Raw != "" {
		return ge.Raw
	}
	return errString(err)
}

func GatewayStatus(err error) int {
	var ge *GatewayError
	if errors.As(err, &ge) && ge != nil {
		return ge.Status
	}
	return 0
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type Client struct {
	mu               sync.RWMutex
	refreshMu        sync.Mutex
	dataDir          string
	tokenPath        string
	httpClient       *http.Client
	refreshURL       string
	tokenExchangeURL string
	gatewayLLM       string
	modelCatalogURL  string
	modelRoutingURL  string
	appVersion       string
	utdid            string
	activeID         string
	loginMu          sync.Mutex
	loginCancel      context.CancelFunc
	// onLogin is fired when a callback captures a token. The app layer wires
	// this to runtime.EventsEmit("accio:login", ...) so the frontend can poll
	// AccioStatus / refresh models without a tight coupling to the listener.
	// The account manager appends its own listener so auto-created accounts
	// land in the proxy pool without overriding the UI bridge.
	onLogin []func(TokenRecord)
	// pendingLogin holds the PKCE state for an in-flight login flow.
	// When non-nil, the callback listener is active and waiting for a
	// redirect with ?code=...&state=... to exchange via /api/oauth/token.
	pending    pendingLogin
	catalogMu  sync.Mutex
	catalog    []Model
	catalogAt  time.Time
	catalogKey string
	catalogTTL time.Duration
}

func New(dataDir string) (*Client, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("accio data directory is required")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create accio data directory: %w", err)
	}
	// dataDir, gatewayLLM, appVersion, utdid

	utdid := ""
	if home, err := os.UserHomeDir(); err == nil {
		b, _ := os.ReadFile(filepath.Join(home, ".accio", "utdid"))
		utdid = strings.TrimSpace(string(b))
	}
	c := &Client{
		dataDir:          dataDir,
		tokenPath:        filepath.Join(dataDir, "token.json"),
		httpClient:       newChromeTLSClient(120 * time.Second),
		refreshURL:       RefreshURL,
		tokenExchangeURL: TokenExchangeURL,
		gatewayLLM:       GatewayLLM,
		modelCatalogURL:  GatewayBase + "/llm/config/v2",
		modelRoutingURL:  GatewayBase + "/tool/rlab/call",
		appVersion:       DefaultAppVersion,
		utdid:            utdid,
		catalogTTL:       45 * time.Second,
	}
	_ = c.migrateLegacyToken()
	return c, nil
}

func (c *Client) TokenPath() string { return c.tokenPath }

func (c *Client) accountPath(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		id = "default"
	}
	return filepath.Join(c.dataDir, "account-"+id+".json")
}

func (c *Client) activePath() string { return filepath.Join(c.dataDir, "active-account") }

func accountIDForToken(access string) string {
	sum := md5.Sum([]byte(strings.TrimSpace(access)))
	return "accio-" + hex.EncodeToString(sum[:])[:16]
}

func (c *Client) migrateLegacyToken() error {
	if _, err := os.Stat(c.tokenPath); err != nil {
		return nil
	}
	entries, _ := os.ReadDir(c.dataDir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "account-") && strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
	}
	b, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return err
	}
	var t TokenRecord
	if json.Unmarshal(b, &t) != nil || strings.TrimSpace(t.AccessToken) == "" {
		return nil
	}
	t.ID = accountIDForToken(t.AccessToken)
	if err := os.WriteFile(c.accountPath(t.ID), mustJSON(t), 0o600); err != nil {
		return err
	}
	c.activeID = t.ID
	return os.WriteFile(c.activePath(), []byte(t.ID), 0o600)
}

func mustJSON(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return append(b, '\n')
}

func (c *Client) Accounts() []TokenRecord {
	entries, _ := os.ReadDir(c.dataDir)
	out := make([]TokenRecord, 0)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "account-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(c.dataDir, entry.Name()))
		if err != nil {
			continue
		}
		var t TokenRecord
		if json.Unmarshal(b, &t) == nil && t.AccessToken != "" {
			out = append(out, t)
		}
	}
	return out
}

func (c *Client) CurrentAccount() (TokenRecord, error) { return c.readToken() }

func (c *Client) SetActiveAccount(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("accio account id is required")
	}
	b, err := os.ReadFile(c.accountPath(id))
	if err != nil {
		return err
	}
	var t TokenRecord
	if err := json.Unmarshal(b, &t); err != nil || t.AccessToken == "" {
		return errors.New("accio account is invalid")
	}
	c.mu.Lock()
	c.activeID = id
	c.mu.Unlock()
	return os.WriteFile(c.activePath(), []byte(id), 0o600)
}

// RemoveAccount deletes the Accio account file and repairs the active-account
// pointer. The proxy store and this account pool are separate persistence
// layers, so deleting only the store row would make the account reappear on
// the next startup when the Accio pool is synchronized again.
func (c *Client) RemoveAccount(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("accio account id is required")
	}
	// A legacy token.json may have survived an earlier migration. Remove it
	// only when its derived account ID matches the account being deleted; never
	// delete an unrelated legacy credential.
	if b, err := os.ReadFile(c.tokenPath); err == nil {
		var legacy TokenRecord
		if json.Unmarshal(b, &legacy) == nil && accountIDForToken(legacy.AccessToken) == id {
			if err := os.Remove(c.tokenPath); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	if err := os.Remove(c.accountPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	c.mu.Lock()
	active := c.activeID
	c.mu.Unlock()
	if active == "" {
		if b, err := os.ReadFile(c.activePath()); err == nil {
			active = strings.TrimSpace(string(b))
		}
	}
	if active != id {
		return nil
	}
	remaining := c.Accounts()
	if len(remaining) == 0 {
		c.mu.Lock()
		c.activeID = ""
		c.mu.Unlock()
		if err := os.Remove(c.activePath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return c.SetActiveAccount(remaining[0].ID)
}

// OnLogin registers a callback fired when a login callback captures a token.
// Used by the app layer to emit a Wails event so the UI can refresh, and by
// the account manager to receive auto-created accounts. Multiple callbacks
// are supported.
func (c *Client) OnLogin(fn func(TokenRecord)) {
	c.mu.Lock()
	c.onLogin = append(c.onLogin, fn)
	c.mu.Unlock()
}

func (c *Client) CurrentToken() (string, error) {
	t, err := c.readToken()
	if err != nil {
		return "", err
	}
	return t.AccessToken, nil
}

func (c *Client) ImportTokenFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var t TokenRecord
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	return c.writeToken(t)
}

func (c *Client) readToken() (TokenRecord, error) {
	c.mu.RLock()
	id := c.activeID
	c.mu.RUnlock()
	if id == "" {
		if b, err := os.ReadFile(c.activePath()); err == nil {
			id = strings.TrimSpace(string(b))
		}
	}
	path := c.tokenPath
	if id != "" {
		path = c.accountPath(id)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return TokenRecord{}, err
	}
	var t TokenRecord
	if err := json.Unmarshal(b, &t); err != nil {
		return TokenRecord{}, err
	}
	if strings.TrimSpace(t.AccessToken) == "" {
		return TokenRecord{}, errors.New("accio access token is empty")
	}
	if t.ID == "" {
		t.ID = id
	}
	return t, nil
}

func (c *Client) writeToken(t TokenRecord) error {
	if strings.TrimSpace(t.AccessToken) == "" {
		return errors.New("accio access token is empty")
	}
	if t.ID == "" {
		t.ID = accountIDForToken(t.AccessToken)
	}
	t.SavedAt = time.Now().UnixMilli()
	b := mustJSON(t)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activeID = t.ID
	tokenPath := c.accountPath(t.ID)
	tmp, err := os.CreateTemp(filepath.Dir(tokenPath), ".token-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, tokenPath); err != nil {
		return err
	}
	_ = os.Chmod(tokenPath, 0o600)
	_ = os.WriteFile(c.activePath(), []byte(t.ID), 0o600)
	return nil
}

func (c *Client) SetToken(access, refresh string, expires any) error {
	return c.SetTokenWithCookie(access, refresh, "", expires)
}

func (c *Client) SetTokenWithCookie(access, refresh, cookie string, expires any) error {
	return c.setTokenWithCookie(access, refresh, cookie, expires, true)
}

func (c *Client) setTokenWithCookie(access, refresh, cookie string, expires any, notify bool) error {
	rec := TokenRecord{
		ID:           accountIDForToken(access),
		AccessToken:  strings.TrimSpace(access),
		RefreshToken: strings.TrimSpace(refresh),
		Cookie:       strings.TrimSpace(cookie),
		ExpiresAt:    expires,
	}
	// Preserve email/label from a prior token so refresh doesn't wipe identity.
	if old, err := c.readToken(); err == nil {
		if rec.Email == "" && old.Email != "" && old.AccessToken == rec.AccessToken {
			rec.Email = old.Email
		}
		if rec.Label == "" && old.Label != "" && old.AccessToken == rec.AccessToken {
			rec.Label = old.Label
		}
	}
	if err := c.writeToken(rec); err != nil {
		return err
	}
	if notify {
		c.notifyLogin(rec)
	}
	return nil
}

func (c *Client) notifyLogin(rec TokenRecord) {
	c.mu.RLock()
	fns := append([]func(TokenRecord){}, c.onLogin...)
	c.mu.RUnlock()
	for _, fn := range fns {
		if fn != nil {
			go fn(rec)
		}
	}
}

func (c *Client) Status() map[string]any {
	t, err := c.readToken()
	return map[string]any{
		"authenticated": err == nil,
		"has_token":     err == nil && t.AccessToken != "",
		"has_refresh":   err == nil && strings.TrimSpace(t.RefreshToken) != "",
		"email":         t.Email,
		"label":         t.Label,
		"token_path":    c.tokenPath,
	}
}

// UserInfo fetches account identity from the gateway. Called after a login to
// enrich the TokenRecord so multiple accounts don't overwrite each other.
func (c *Client) UserInfo(ctx context.Context) (email, label string, err error) {
	t, err := c.readToken()
	if err != nil {
		return "", "", err
	}
	return c.UserInfoWithToken(ctx, t.AccessToken)
}

// UserInfoWithToken resolves the identity for an arbitrary pool token without
// touching the active-account pointer.
func (c *Client) UserInfoWithToken(ctx context.Context, access string) (email, label string, err error) {
	body, _ := json.Marshal(map[string]string{"token": access})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, UserInfoURL, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	c.authHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("accio userinfo failed: HTTP %d", resp.StatusCode)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return "", "", err
	}
	data := root
	if d, ok := root["data"].(map[string]any); ok {
		data = d
	}
	email, label = findIdentity(data)
	if email == "" || label == "" {
		jwtEmail, jwtLabel := tokenIdentity(access)
		if email == "" {
			email = jwtEmail
		}
		if label == "" {
			label = jwtLabel
		}
	}
	if label == "" {
		label = email
	}
	if label == "" {
		label = "Accio"
	}
	// Persist identity into the token file so the pool can distinguish accounts.
	if email != "" || label != "" {
		cur, err := c.readToken()
		if err == nil && cur.AccessToken == access {
			if email != "" {
				cur.Email = email
			}
			if label != "" {
				cur.Label = label
			}
			if err := c.writeToken(cur); err != nil {
				return email, label, err
			}
		}
	}
	return email, label, nil
}

func findIdentity(m map[string]any) (email, label string) {
	email = firstString(m, "email", "accountEmail", "loginEmail", "userEmail", "mail")
	label = firstString(m, "nickName", "nickname", "displayName", "name", "userName", "username")
	for _, key := range []string{"user", "account", "profile", "userInfo"} {
		if child, ok := m[key].(map[string]any); ok {
			childEmail, childLabel := findIdentity(child)
			if email == "" {
				email = childEmail
			}
			if label == "" {
				label = childLabel
			}
		}
	}
	return strings.TrimSpace(email), strings.TrimSpace(label)
}

func tokenIdentity(token string) (email, label string) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload := parts[1]
	if padding := len(payload) % 4; padding != 0 {
		payload += strings.Repeat("=", 4-padding)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(payload, "="))
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(payload)
	}
	if err != nil {
		return "", ""
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return "", ""
	}
	return findIdentity(claims)
}

// Credits queries the entitlement/quota endpoint and returns a normalized map
// for the UI. Total/remaining may be zero if the plan doesn't expose them.
func (c *Client) Credits(ctx context.Context) (map[string]any, error) {
	t, err := c.readToken()
	if err != nil {
		return nil, err
	}
	return c.CreditsFor(ctx, t)
}

// CreditsFor returns the remaining credit balance of a specific pool account
// (not only the active one). Used by the account manager to aggregate the
// whole pool without touching the active-account pointer. A refresh is only
// attempted for the active account (the refresh machinery serializes around
// the active token file); non-active accounts simply report their error.
func (c *Client) CreditsFor(ctx context.Context, t TokenRecord) (map[string]any, error) {
	result, status, err := c.creditsWithToken(ctx, t.AccessToken, CreditsURL)
	if err != nil && status != http.StatusUnauthorized && status != http.StatusForbidden {
		result, status, err = c.creditsWithToken(ctx, t.AccessToken, CreditsQuotaURL)
	}
	if (status == http.StatusUnauthorized || status == http.StatusForbidden) && err != nil && c.isActiveAccount(t) {
		if refreshed, rerr := c.refreshForToken(ctx, t.AccessToken); rerr != nil {
			return nil, rerr
		} else {
			result, _, err = c.creditsWithToken(ctx, refreshed, CreditsURL)
			if err != nil {
				result, _, err = c.creditsWithToken(ctx, refreshed, CreditsQuotaURL)
			}
		}
	}
	return result, err
}

func (c *Client) isActiveAccount(t TokenRecord) bool {
	cur, err := c.readToken()
	if err != nil {
		return false
	}
	return cur.AccessToken == t.AccessToken || cur.ID == t.ID
}

func (c *Client) creditsWithToken(ctx context.Context, token, endpoint string) (map[string]any, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	q := req.URL.Query()
	q.Set("accessToken", token)
	q.Set("subscripType", "INDIVIDUAL")
	req.URL.RawQuery = q.Encode()
	c.authHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, resp.StatusCode, fmt.Errorf("accio credits rejected: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("accio credits failed: HTTP %d", resp.StatusCode)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, resp.StatusCode, err
	}
	// The entitlement API answers HTTP 200 with {"success":false,"code":"401"}
	// for rejected sessions — treat that as an auth failure so callers can
	// trigger a refresh instead of silently counting 0 credits.
	if success, present := root["success"]; present {
		if ok, isBool := success.(bool); isBool && !ok {
			code := firstString(root, "code", "responseCode")
			return nil, http.StatusUnauthorized, fmt.Errorf("accio credits rejected: %s", code)
		}
	}
	data := root
	if d, ok := root["data"].(map[string]any); ok {
		data = d
	}
	total := toInt(firstValue(data, "total", "totalCredits", "creditTotal"))
	remaining := toInt(firstValue(data, "remaining", "remainingCredits", "creditRemaining"))
	used := toInt(firstValue(data, "used", "usedCredits"))
	if used == 0 && total > 0 {
		used = total - remaining
	}
	// The plan header only reflects the daily pool; aggregate the per-pool
	// entitlements (daily + monthly + referral) into remainingCredits so the
	// account manager sees everything actually spendable.
	aggRemaining := 0
	aggTotal := 0
	if ent, ok := data["entitlement"].(map[string]any); ok {
		for _, poolName := range []string{"daily", "monthly", "referral"} {
			if pool, ok := ent[poolName].(map[string]any); ok {
				aggTotal += toInt(firstValue(pool, "total"))
				aggRemaining += toInt(firstValue(pool, "remaining"))
			}
		}
	}
	if aggRemaining > 0 {
		remaining = aggRemaining
	}
	if aggTotal > 0 {
		total = aggTotal
	}
	planType := firstString(data, "type", "planType", "subscriptionType")
	return map[string]any{
		"total":     total,
		"remaining": remaining,
		"used":      used,
		"type":      planType,
		"raw":       data,
	}, resp.StatusCode, nil
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case string:
		i := 0
		_, _ = fmt.Sscanf(n, "%d", &i)
		return i
	}
	return 0
}

func (c *Client) authHeaders(req *http.Request) {
	c.authHeadersWithToken(req, "")
}

func (c *Client) authHeadersWithToken(req *http.Request, token string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if t, err := c.readToken(); err == nil {
		if strings.TrimSpace(token) == "" {
			token = t.AccessToken
		}
		if strings.TrimSpace(t.Cookie) != "" {
			req.Header.Set("Cookie", t.Cookie)
		}
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	// Ensure cookie2 is always present (WAF requires it; value == accessToken).
	if strings.TrimSpace(req.Header.Get("Cookie")) == "" && strings.TrimSpace(token) != "" {
		req.Header.Set("Cookie", "cookie2="+strings.TrimSpace(token))
	}
	req.Header.Set("User-Agent", "Accio/"+c.appVersion)
	req.Header.Set("version", c.appVersion)
	req.Header.Set("utdid", c.utdid)
	req.Header.Set("appKey", DefaultSecurityAppKey)
	req.Header.Set("x-deploy-target", "desktop")
	req.Header.Set("x-accio-route-region", "SG")
	req.Header.Set("x-package-region", "SG")
}

func (c *Client) Refresh(ctx context.Context) (string, error) {
	return c.refreshForToken(ctx, "")
}

// RefreshWith exchanges an arbitrary refresh token (any pool account) for a
// fresh token pair, without touching the active-account file. Accio rotates
// refresh tokens on every refresh, so callers must persist the returned pair
// (see SaveAccount) or the old refresh token becomes invalid.
func (c *Client) RefreshWith(ctx context.Context, refreshToken string) (access, refresh string, err error) {
	if strings.TrimSpace(refreshToken) == "" {
		return "", "", errors.New("accio refresh token is missing")
	}
	body, _ := json.Marshal(map[string]string{"refreshToken": refreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.refreshURL, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	c.authHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("accio refresh failed: HTTP %d", resp.StatusCode)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return "", "", fmt.Errorf("accio refresh returned invalid JSON: %w", err)
	}
	data := root
	if d, ok := root["data"].(map[string]any); ok {
		data = d
	}
	if success, present := root["success"]; present {
		if ok, isBool := success.(bool); isBool && !ok {
			return "", "", errors.New("accio refresh rejected the session")
		}
	}
	access = firstString(data, "accessToken", "access_token")
	if access == "" {
		access = firstString(root, "accessToken", "access_token")
	}
	refresh = firstString(data, "refreshToken", "refresh_token")
	if refresh == "" {
		refresh = firstString(root, "refreshToken", "refresh_token")
	}
	if access == "" {
		return "", "", errors.New("accio refresh returned no access token")
	}
	return access, refresh, nil
}

// SaveAccount persists a token record into the pool without switching the
// active account (used by the account manager to store refreshed pairs).
func (c *Client) SaveAccount(rec TokenRecord) error {
	if strings.TrimSpace(rec.AccessToken) == "" {
		return errors.New("accio access token is empty")
	}
	if rec.ID == "" {
		rec.ID = accountIDForToken(rec.AccessToken)
	}
	rec.SavedAt = time.Now().UnixMilli()
	b := mustJSON(rec)
	tokenPath := c.accountPath(rec.ID)
	tmp, err := os.CreateTemp(filepath.Dir(tokenPath), ".token-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, tokenPath); err != nil {
		return err
	}
	_ = os.Chmod(tokenPath, 0o600)
	return nil
}

// refreshForToken serializes refreshes and avoids replaying the same refresh
// token when several requests receive 401/403 at the same time. If another
// request already replaced the failed access token, its token is reused.
func (c *Client) refreshForToken(ctx context.Context, failedAccess string) (string, error) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	t, err := c.readToken()
	if err != nil {
		return "", err
	}
	if failedAccess != "" && t.AccessToken != failedAccess {
		return t.AccessToken, nil
	}
	if strings.TrimSpace(t.RefreshToken) == "" {
		return "", errors.New("accio refresh token is missing")
	}
	body, _ := json.Marshal(map[string]string{"refreshToken": t.RefreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.refreshURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	c.authHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("accio refresh failed: HTTP %d", resp.StatusCode)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return "", fmt.Errorf("accio refresh returned invalid JSON: %w", err)
	}
	data := root
	if d, ok := root["data"].(map[string]any); ok {
		data = d
	}
	if success, present := root["success"]; present {
		if ok, isBool := success.(bool); isBool && !ok {
			return "", errors.New("accio refresh rejected the session")
		}
	}
	access := firstString(data, "accessToken", "access_token")
	refresh := firstString(data, "refreshToken", "refresh_token")
	if access == "" {
		access = firstString(root, "accessToken", "access_token")
	}
	if refresh == "" {
		refresh = t.RefreshToken
	}
	if access == "" {
		return "", errors.New("accio refresh returned no access token")
	}
	// Extract cookies from the refresh response (required to bypass WAF captcha).
	cookie := extractCookiesFromRefresh(data)
	if cookie == "" {
		cookie = t.Cookie // preserve existing cookie if response has none
	}
	if err := c.setTokenWithCookie(access, refresh, cookie, firstValue(data, "expiresAt", "expires_at"), true); err != nil {
		return "", err
	}
	return access, nil
}

// extractCookiesFromRefresh builds a Cookie header string from the refresh
// response's cookies.appCookies array (same format the official app handles).
func extractCookiesFromRefresh(data map[string]any) string {
	cookiesObj, ok := data["cookies"].(map[string]any)
	if !ok {
		return ""
	}
	appCookies, ok := cookiesObj["appCookies"].([]any)
	if !ok || len(appCookies) == 0 {
		return ""
	}
	var parts []string
	for _, raw := range appCookies {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := c["name"].(string)
		value, _ := c["value"].(string)
		if name != "" && value != "" {
			parts = append(parts, name+"="+value)
		}
	}
	return strings.Join(parts, "; ")
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
func firstValue(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

func (c *Client) StartLogin(ctx context.Context, callbackPort int) (string, error) {
	c.loginMu.Lock()
	if c.loginCancel != nil {
		c.loginCancel()
	}
	loginCtx, cancel := context.WithCancel(ctx)
	c.loginCancel = cancel
	c.loginMu.Unlock()

	// Generate PKCE verifier + challenge (S256), matching the official app.
	verifier, challenge, err := pkceChallengePair()
	if err != nil {
		cancel()
		return "", fmt.Errorf("accio pkce: %w", err)
	}

	if callbackPort == 0 {
		listener, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			cancel()
			return "", fmt.Errorf("accio callback listener: %w", err)
		}
		callbackPort = listener.Addr().(*net.TCPAddr).Port
		redirect := fmt.Sprintf("http://localhost:%d/auth/callback", callbackPort)
		state, err := randomHexState()
		if err != nil {
			_ = listener.Close()
			cancel()
			return "", fmt.Errorf("accio login state: %w", err)
		}
		loginURL := buildPKCELoginURL(redirect, challenge, state)

		c.mu.Lock()
		c.pending = pendingLogin{codeVerifier: verifier, state: state, redirectURI: redirect}
		c.mu.Unlock()

		go c.loginCallbackWithListener(loginCtx, listener)
		return loginURL, nil
	}

	redirect := fmt.Sprintf("http://localhost:%d/auth/callback", callbackPort)
	state, err := randomHexState()
	if err != nil {
		cancel()
		return "", fmt.Errorf("accio login state: %w", err)
	}
	loginURL := buildPKCELoginURL(redirect, challenge, state)

	c.mu.Lock()
	c.pending = pendingLogin{codeVerifier: verifier, state: state, redirectURI: redirect}
	c.mu.Unlock()

	go func() {
		listener, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", callbackPort))
		if err != nil {
			return
		}
		c.loginCallbackWithListener(loginCtx, listener)
	}()
	return loginURL, nil
}

// randomHexState generates a 32-byte random hex state for CSRF protection,
// matching the official app's crypto.randomBytes(32).toString("hex").
func randomHexState() (string, error) {
	b := make([]byte, 32)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// buildPKCELoginURL constructs the login URL with PKCE parameters, matching
// the official app's startLoginWithCallbackMode:
//
//	${loginBaseUrl}/login?return_url=${redirect}&state=${state}&code_challenge=${challenge}&code_challenge_method=S256&client_id=accio-work
func buildPKCELoginURL(redirect, challenge, state string) string {
	u, _ := url.Parse(LoginBase + "/login")
	q := u.Query()
	q.Set("return_url", redirect)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("client_id", OAuthClientID)
	u.RawQuery = q.Encode()
	return u.String()
}

func firstQuery(q url.Values, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(q.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

const callbackRelayHTML = `<!doctype html><meta charset="utf-8"><title>Accio callback</title><p id="status">Finalizando login no app…</p><script>
(async()=>{
  const status=document.getElementById("status");
  const raw=location.hash.replace(/^#/,"");
  const params=new URLSearchParams(raw.startsWith("/") ? raw.slice(1) : raw);
  const keys=["accessToken","access_token","token","access-token"];
  const body={};
  for(const key of keys){const value=params.get(key);if(value){body[key]=value;break;}}
  for(const key of ["refreshToken","refresh_token","refresh-token","expiresAt","cookie"]){const value=params.get(key);if(value)body[key]=value;}
  if(!Object.keys(body).some(k=>keys.includes(k))){status.textContent="O login terminou, mas não entregou uma sessão ao app.";return;}
  try{
    const response=await fetch(location.pathname,{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(body)});
    status.textContent=response.ok?"Login conectado ao app. Você pode fechar esta aba.":"Falha ao registrar a sessão no app.";
    if(response.ok) setTimeout(()=>window.close(),400);
  }catch(_){status.textContent="Não foi possível conectar ao app local.";}
})();
</script>`

func (c *Client) loginCallbackWithListener(ctx context.Context, listener net.Listener) {
	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux}
	// The Accio web flow can hit the callback more than once (e.g. a PKCE
	// code redirect followed by a legacy accessToken redirect). Only the
	// first exchange is authoritative — later requests would overwrite a
	// good OAuth token with web-session tokens.
	var handledMu sync.Mutex
	handled := false
	markHandled := func() bool {
		handledMu.Lock()
		defer handledMu.Unlock()
		if handled {
			return false
		}
		handled = true
		return true
	}
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		if !markHandled() {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, callbackSuccessHTML)
			return
		}
		q := r.URL.Query()

		// PKCE flow: the callback arrives with ?code=...&state=...
		// We exchange the code for tokens via /api/oauth/token.
		code := strings.TrimSpace(firstQuery(q, "code"))
		state := strings.TrimSpace(q.Get("state"))
		if code != "" {
			c.mu.RLock()
			pending := c.pending
			c.mu.RUnlock()

			if pending.state == "" || state == "" || state != pending.state {
				http.Error(w, "State mismatch — possible CSRF, aborting.", http.StatusBadRequest)
				return
			}
			if pending.codeVerifier == "" {
				http.Error(w, "No pending login — start login first.", http.StatusBadRequest)
				return
			}

			token, err := c.exchangeCodeForToken(ctx, code, pending)
			if err != nil {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, callbackErrorHTML(err.Error()))
				return
			}

			// Clear pending state before saving.
			c.mu.Lock()
			c.pending = pendingLogin{}
			c.mu.Unlock()

			if err := c.setTokenWithCookie(token.AccessToken, token.RefreshToken, "", token.ExpiresAt, false); err != nil {
				http.Error(w, "Unable to save token", http.StatusInternalServerError)
				return
			}
			// Enrich before notifying the app so the UI receives the real identity.
			ctx2, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, _, _ = c.UserInfo(ctx2)
			cancel()
			if enriched, readErr := c.CurrentAccount(); readErr == nil {
				c.notifyLogin(enriched)
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, callbackSuccessHTML)
			go srv.Shutdown(context.Background())
			return
		}

		// Legacy flow: some older redirect shapes may deliver accessToken directly.
		access, refresh, cookie, expires := extractCallbackToken(r)
		if access == "" {
			// OAuth-style fragments are not sent in the HTTP request. Return a
			// tiny relay page that reads #accessToken=... in the browser and
			// posts it back to this listener.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, callbackRelayHTML)
			return
		}
		if err := c.setTokenWithCookie(access, refresh, cookie, expires, false); err != nil {
			http.Error(w, "Unable to save token", http.StatusInternalServerError)
			return
		}
		// Enrich before notifying the app so the UI receives the real identity.
		ctx2, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _, _ = c.UserInfo(ctx2)
		cancel()
		if enriched, readErr := c.CurrentAccount(); readErr == nil {
			c.notifyLogin(enriched)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<html><body><h2>Accio login completed</h2><p>Token captured securely. You can close this window.</p><script>window.close()</script></body></html>`)
		go srv.Shutdown(context.Background())
	})
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	_ = srv.Serve(listener)
}

// exchangeCodeForToken performs the PKCE token exchange at /api/oauth/token,
// matching the official app's qc(Sc.tokenExchange, {code, codeVerifier, clientId, redirectUri}).
func (c *Client) exchangeCodeForToken(ctx context.Context, code string, pending pendingLogin) (*TokenRecord, error) {
	body, _ := json.Marshal(map[string]string{
		"code":         code,
		"codeVerifier": pending.codeVerifier,
		"clientId":     OAuthClientID,
		"redirectUri":  pending.redirectURI,
		"utdid":        c.utdid,
		"version":      c.appVersion,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenExchangeURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.authHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("accio token exchange failed: HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("accio token exchange: invalid JSON: %w", err)
	}
	data := root
	if d, ok := root["data"].(map[string]any); ok {
		data = d
	}
	access := firstString(data, "accessToken", "access_token")
	refresh := firstString(data, "refreshToken", "refresh_token")
	if access == "" {
		return nil, fmt.Errorf("accio token exchange: no accessToken in response")
	}
	var expiresAt any
	if v, ok := data["expiresAt"]; ok {
		expiresAt = v
	} else if v, ok := data["expires_at"]; ok {
		expiresAt = v
	}
	// If expiresAt is in seconds (number), convert to milliseconds.
	if n, ok := expiresAt.(float64); ok && n > 0 && n < 1e12 {
		expiresAt = int64(n * 1000)
	}
	cookie := extractCookiesFromRefresh(data)
	return &TokenRecord{AccessToken: access, RefreshToken: refresh, Cookie: cookie, ExpiresAt: expiresAt}, nil
}

const callbackSuccessHTML = `<!doctype html><meta charset="utf-8"><title>Accio login</title>
<style>body{font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;background:#1a1a2e;color:#e0e0e0}h2{color:#4e9af1}</style>
<h2>Login concluído</h2><p>Token capturado com segurança. Você pode fechar esta aba.</p><script>setTimeout(()=>window.close(),2000)</script>`

func callbackErrorHTML(reason string) string {
	return `<!doctype html><meta charset="utf-8"><title>Accio login error</title>
<style>body{font-family:system-ui,sans-serif;display:flex;flex-direction:column;align-items:center;justify-content:center;height:100vh;margin:0;background:#1a1a2e;color:#e0e0e0}h2{color:#f44}pre{background:#2a2a4e;padding:12px;border-radius:8px;max-width:80%;overflow:auto;font-size:13px}</style>
<h2>Falha no login</h2><pre>` + html.EscapeString(reason) + `</pre>
<p>Verifique se o Accio oficial não está aberto simultaneamente — apenas um pode registrar o protocolo de callback.</p>`
}

// extractCallbackToken pulls access/refresh tokens from query, form body, or JSON.
func extractCallbackToken(r *http.Request) (access, refresh, cookie, expires string) {
	q := r.URL.Query()
	access = firstQuery(q, "accessToken", "access_token", "token", "access-token")
	refresh = firstQuery(q, "refreshToken", "refresh_token", "refresh-token")
	cookie = q.Get("cookie")
	expires = q.Get("expiresAt")
	if access != "" {
		return
	}
	// POST form
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		if access == "" {
			access = strings.TrimSpace(r.FormValue("accessToken"))
		}
		if access == "" {
			access = strings.TrimSpace(r.FormValue("access_token"))
		}
		if access == "" {
			access = strings.TrimSpace(r.FormValue("token"))
		}
		if refresh == "" {
			refresh = strings.TrimSpace(r.FormValue("refreshToken"))
		}
		if refresh == "" {
			refresh = strings.TrimSpace(r.FormValue("refresh_token"))
		}
		if cookie == "" {
			cookie = strings.TrimSpace(r.FormValue("cookie"))
		}
		if expires == "" {
			expires = strings.TrimSpace(r.FormValue("expiresAt"))
		}
	}
	// JSON body (application/json or fragment payload)
	if access == "" {
		if b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err == nil && len(b) > 0 {
			var m map[string]any
			if json.Unmarshal(b, &m) == nil {
				if v, ok := m["accessToken"].(string); ok && access == "" {
					access = strings.TrimSpace(v)
				}
				if v, ok := m["access_token"].(string); ok && access == "" {
					access = strings.TrimSpace(v)
				}
				if v, ok := m["token"].(string); ok && access == "" {
					access = strings.TrimSpace(v)
				}
				if v, ok := m["refreshToken"].(string); ok && refresh == "" {
					refresh = strings.TrimSpace(v)
				}
				if v, ok := m["refresh_token"].(string); ok && refresh == "" {
					refresh = strings.TrimSpace(v)
				}
				if v, ok := m["cookie"].(string); ok && cookie == "" {
					cookie = strings.TrimSpace(v)
				}
				if v, ok := m["expiresAt"].(string); ok && expires == "" {
					expires = strings.TrimSpace(v)
				}
				// Nested data object
				if d, ok := m["data"].(map[string]any); ok {
					if v, ok := d["accessToken"].(string); ok && access == "" {
						access = strings.TrimSpace(v)
					}
					if v, ok := d["access_token"].(string); ok && access == "" {
						access = strings.TrimSpace(v)
					}
					if v, ok := d["refreshToken"].(string); ok && refresh == "" {
						refresh = strings.TrimSpace(v)
					}
				}
			}
		}
	}
	return
}

func (c *Client) Models(ctx context.Context) ([]Model, error) {
	c.catalogMu.Lock()
	key := c.activeAccountKey()
	if key != "" && c.catalogKey == key && len(c.catalog) > 0 && time.Since(c.catalogAt) < c.catalogTTL {
		cached := append([]Model(nil), c.catalog...)
		c.catalogMu.Unlock()
		return cached, nil
	}
	c.catalogMu.Unlock()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
	}
	t, err := c.readToken()
	if err != nil {
		return nil, err
	}
	models, status, err := c.modelsWithToken(ctx, t.AccessToken)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		if _, rerr := c.refreshForToken(ctx, t.AccessToken); rerr != nil {
			return nil, rerr
		}
		t, err = c.readToken()
		if err != nil {
			return nil, err
		}
		models, _, err = c.modelsWithToken(ctx, t.AccessToken)
	}
	// The current production gateway no longer exposes the renderer's
	// /models/config route remotely. Ask the authenticated routing service for
	// the model it would actually assign instead of returning the old, fake
	// Nexus entry. This is dynamic and uses the same account token as chat.
	if err != nil || len(models) == 0 {
		if routed, routeErr := c.modelRoutingWithToken(ctx, t.AccessToken); routeErr == nil {
			models = []Model{routed}
			err = nil
		}
	}
	if err == nil && len(models) > 0 {
		c.catalogMu.Lock()
		c.catalogKey = c.activeAccountKey()
		c.catalog = append([]Model(nil), models...)
		c.catalogAt = time.Now()
		c.catalogMu.Unlock()
	}
	return models, err
}

func (c *Client) activeAccountKey() string {
	t, err := c.readToken()
	if err != nil {
		return ""
	}
	if t.ID != "" {
		return t.ID
	}
	return accountIDForToken(t.AccessToken)
}

func (c *Client) modelsWithToken(ctx context.Context, token string) ([]Model, int, error) {
	body, _ := json.Marshal(map[string]string{"token": token})
	// The official Accio client syncs the complete catalog with POST
	// /api/llm/config/v2. It returns providers[].modelList[]; the older
	// /models/config and /adk/llm/config/v2 guesses do not expose this route.
	catalogURL := c.modelCatalogURL
	if strings.TrimSpace(catalogURL) == "" {
		catalogURL = GatewayBase + "/llm/config/v2"
	}
	endpoints := []struct {
		method string
		url    string
	}{
		{http.MethodPost, catalogURL},
		{http.MethodGet, GatewayBase + "/models/config"},
		{http.MethodGet, GatewayBase + "/models"},
	}
	var lastStatus int
	var lastErr error
	for _, endpoint := range endpoints {
		var reader io.Reader
		if endpoint.method == http.MethodPost {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, endpoint.method, endpoint.url, reader)
		if err != nil {
			lastErr = err
			continue
		}
		c.authHeadersWithToken(req, token)
		// A failed legacy route must not consume the entire catalog deadline.
		// Give each candidate its own short child timeout while preserving the
		// caller's overall context and cancellation.
		requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req = req.WithContext(requestCtx)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		cancel()
		lastStatus = resp.StatusCode
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("accio models failed: HTTP %d", resp.StatusCode)
			continue
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			lastErr = err
			continue
		}
		if encoded, ok := value.(string); ok {
			var nested any
			if json.Unmarshal([]byte(encoded), &nested) == nil {
				value = nested
			}
		}
		if root, ok := value.(map[string]any); ok {
			if success, present := root["success"]; present {
				if ok, isBool := success.(bool); isBool && !ok {
					lastStatus = http.StatusUnauthorized
					lastErr = errors.New("accio model catalog rejected the session")
					continue
				}
			}
		}
		models := parseModelCatalogValue(value)
		if len(models) > 0 {
			return models, resp.StatusCode, nil
		}
		lastErr = errors.New("accio model catalog returned no visible models")
	}
	if lastErr == nil {
		lastErr = errors.New("accio model catalog unavailable")
	}
	return nil, lastStatus, lastErr
}

// modelRoutingWithToken asks the production routing service for the model that
// the current Accio account can actually use. The renderer's local /models
// catalog is protected by an IPC password and is intentionally not reused by
// this proxy; model_routing is the authenticated remote contract available to
// the proxy account itself.
func (c *Client) modelRoutingWithToken(ctx context.Context, token string) (Model, error) {
	payload := map[string]any{
		"conversationId": "proxy-model-catalog",
		"messageId":      "proxy-model-catalog",
		"requestId":      "proxy-model-catalog",
		"model":          "auto",
	}
	body := map[string]any{
		"function":  "model_routing",
		"payload":   payload,
		"token":     token,
		"messageId": "proxy-model-catalog",
		"requestId": "proxy-model-catalog",
	}
	b, _ := json.Marshal(body)
	endpoint := c.modelRoutingURL
	if strings.TrimSpace(endpoint) == "" {
		endpoint = GatewayBase + "/tool/rlab/call"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return Model{}, err
	}
	c.authHeadersWithToken(req, token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Model{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Model{}, fmt.Errorf("accio model routing failed: HTTP %d", resp.StatusCode)
	}
	var root struct {
		Success bool `json:"success"`
		Data    struct {
			Payload struct {
				ModelCode string `json:"modelCode"`
				ModelName string `json:"modelName"`
			} `json:"payload"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return Model{}, fmt.Errorf("accio model routing returned invalid JSON: %w", err)
	}
	id := strings.TrimSpace(root.Data.Payload.ModelCode)
	name := strings.TrimSpace(root.Data.Payload.ModelName)
	if id == "" {
		return Model{}, errors.New("accio model routing returned no model code")
	}
	if name == "" {
		name = id
	}
	modelInfo := catalogModelInfo{id: id, name: name, modelName: name}
	return Model{
		ID:          "accio/" + strings.TrimPrefix(id, "accio/"),
		Name:        normalizeAccioModelDisplayName(modelInfo, nil),
		Provider:    Provider,
		Description: "Accio · dynamic routing",
	}, nil
}

// parseModelCatalog accepts the catalog shapes used by different gateway
// deployments. The official response has changed between a provider array
// containing modelList and nested data/result/models objects; walking the
// response avoids silently falling back to the single hard-coded UI model.
func parseModelCatalog(root map[string]any) []Model {
	return parseModelCatalogValue(root)
}

func parseModelCatalogValue(root any) []Model {
	out := make([]Model, 0)
	seen := make(map[string]bool)
	displayLabels := collectAccioDisplayLabels(root)
	var walk func(any, string)
	walk = func(value any, providerName string) {
		switch v := value.(type) {
		case []any:
			for _, item := range v {
				walk(item, providerName)
			}
		case map[string]any:
			if p := firstString(v, "providerDisplayName", "providerName", "provider"); p != "" {
				providerName = p
			}
			if model, ok := catalogModel(v); ok {
				visible, hasVisible := v["visible"].(bool)
				if !hasVisible || visible {
					id := "accio/" + model.id
					if !seen[id] {
						seen[id] = true
						out = append(out, Model{
							ID:                     id,
							Name:                   normalizeAccioModelDisplayName(model, displayLabels),
							Provider:               Provider,
							Description:            providerName,
							Context:                model.context,
							Multimodal:             model.multimodal,
							ReasoningEfforts:       model.reasoningEfforts,
							DefaultReasoningEffort: model.defaultReasoningEffort,
							FreeUse:                model.freeUse,
							Locked:                 model.locked,
						})
					}
				}
			}
			for key, child := range v {
				if key == "visible" {
					continue
				}
				walk(child, providerName)
			}
		}
	}
	walk(root, "")
	return out
}

type catalogModelInfo struct {
	id                     string
	name                   string
	modelName              string
	description            string
	context                int64
	multimodal             bool
	reasoningEfforts       []string
	defaultReasoningEffort string
	freeUse                bool
	locked                 bool
}

func catalogModel(v map[string]any) (catalogModelInfo, bool) {
	id := firstString(v, "modelCode", "modelId", "model_id", "code")
	modelName := firstString(v, "modelName")
	name := firstString(v, "modelDisplayName", "displayName", "name", "label")
	description := firstString(v, "modelDesc", "hoverDesc", "usageDesc")
	if name == "" {
		name = modelName
	}
	if name == "" {
		name = description
	}
	// modelName is also used as the ID by some catalog versions.
	if id == "" {
		id = modelName
	}
	if id == "" {
		return catalogModelInfo{}, false
	}
	return catalogModelInfo{
		id:                     strings.TrimPrefix(id, "accio/"),
		name:                   name,
		modelName:              modelName,
		description:            description,
		context:                firstInt64(v, "contextWindow", "context_window", "contextLength", "context_length"),
		multimodal:             firstBool(v, "multimodal", "isMultimodal", "supportsMultimodal"),
		reasoningEfforts:       stringList(v["reasoningEfforts"]),
		defaultReasoningEffort: firstString(v, "defaultReasoningEffort", "default_reasoning_effort"),
		freeUse:                firstBool(v, "freeUse", "free_use"),
		locked:                 firstBool(v, "locked"),
	}, true
}

// collectAccioDisplayLabels mirrors the renderer's labelList mapping. Accio
// can return an opaque modelCode/modelName pair while putting the human name
// in ext.labelList[]. The code remains the request ID; only the visible name
// is replaced here.
func collectAccioDisplayLabels(root any) map[string]string {
	labels := make(map[string]string)
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case []any:
			for _, item := range v {
				walk(item)
			}
		case map[string]any:
			if list, ok := v["labelList"]; ok {
				if items, ok := list.([]any); ok {
					for _, item := range items {
						label, ok := item.(map[string]any)
						if !ok || strings.EqualFold(firstString(label, "labelKey"), "auto") {
							continue
						}
						target := accioModelCodeKey(firstString(label, "targetModelCode", "modelCode"))
						name := firstString(label, "displayName", "name", "label")
						if target != "" && name != "" {
							labels[target] = name
						}
					}
				}
			}
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(root)
	return labels
}

func normalizeAccioModelDisplayName(model catalogModelInfo, labels map[string]string) string {
	if label := labels[accioModelCodeKey(model.id)]; label != "" {
		return label
	}
	for _, candidate := range []string{model.name, model.modelName} {
		if candidate != "" && !isOpaqueAccioModelName(candidate, model.id) {
			return candidate
		}
	}
	if model.description != "" && !isOpaqueAccioModelName(model.description, model.id) {
		return model.description
	}
	return friendlyAccioModelName(model.id)
}

func accioModelCodeKey(value string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(value), "accio/"))
}

func isOpaqueAccioModelName(name, id string) bool {
	name = strings.TrimPrefix(strings.TrimSpace(name), "accio/")
	id = strings.TrimPrefix(strings.TrimSpace(id), "accio/")
	if name == "" || id == "" || !strings.EqualFold(name, id) {
		return false
	}
	prefix, suffix, ok := strings.Cut(name, "-")
	return ok && strings.HasPrefix(prefix, "1") && len(suffix) >= 8
}

func friendlyAccioModelName(id string) string {
	raw := strings.TrimPrefix(strings.TrimSpace(id), "accio/")
	prefix, suffix, found := strings.Cut(raw, "-")
	family := strings.TrimPrefix(prefix, "1")
	if family == "" {
		return "Accio model"
	}
	family = titleAccioModelToken(family)
	if found && suffix != "" {
		return family + " - " + suffix
	}
	return family
}

func titleAccioModelToken(value string) string {
	if value == "" {
		return value
	}
	if strings.EqualFold(value, "gpt") {
		return "GPT"
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func firstBool(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k].(bool); ok {
			return v
		}
	}
	return false
}

func firstInt64(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case int:
			return int64(v)
		case int64:
			return v
		case json.Number:
			n, _ := v.Int64()
			return n
		}
	}
	return 0
}

func stringList(v any) []string {
	var items []any
	switch typed := v.(type) {
	case []any:
		items = typed
	case []string:
		items = make([]any, len(typed))
		for i, item := range typed {
			items[i] = item
		}
	default:
		return nil
	}
	out := make([]string, 0, len(items))
	seen := make(map[string]bool)
	for _, item := range items {
		s, ok := item.(string)
		s = strings.TrimSpace(s)
		if !ok || s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func (c *Client) Stream(ctx context.Context, request map[string]any, emit func(text, reasoning string, call map[string]any, done bool)) error {
	t, err := c.readToken()
	if err != nil {
		return err
	}
	return c.StreamWithToken(ctx, request, t.AccessToken, emit)
}

func (c *Client) StreamWithToken(ctx context.Context, request map[string]any, token string, emit func(text, reasoning string, call map[string]any, done bool)) error {
	request["stream"] = true
	logging.Info("accio.stream.start", "local_available", localGatewayAvailable(), "model", firstString(request, "model"))
	// When the Accio desktop is running, prefer its authenticated local
	// gateway: the official transport used by the app. It bypasses the WAF
	// entirely (the desktop app has already passed the Baxia challenge and
	// the WebSocket is authorised via the per-process basic-auth password).
	// This is the only path that reliably works for brand-new accounts that
	// have not yet been observed by the Alibaba WAF stateful layer.
	if localGatewayAvailable() {
		err := c.streamLocal(ctx, request, emit)
		if err == nil {
			logging.Info("accio.stream.local_ok")
			return nil
		}
		// Fall through to the remote endpoint on any error: it works for
		// accounts whose WAF fingerprint has already been registered.
		logging.Warn("accio.stream.local_fail", "err", err.Error())
	}
	resp, status, err := c.chatWithToken(ctx, request, token)
	logging.Info("accio.stream.remote", "status", status, "err", errString(err))
	if err != nil {
		return err
	}
	if resp == nil {
		return &GatewayError{Status: status, Raw: "session rejected before response"}
	}
	defer resp.Body.Close()
	if err := c.readFrames(resp.Body, emit); err != nil {
		// Surface a more actionable message when the gateway returned the
		// Baxia/WAF challenge HTML: the proxy has already emitted all SG
		// signed headers, so this means the account's device fingerprint
		// has not yet been seen by the Alibaba stateful WAF. Logging into
		// the account once via the Accio desktop app resolves it.
		if _, ok := err.(*GatewayError); ok && strings.Contains(err.Error(), "HTML challenge") {
			return &GatewayError{Status: http.StatusBadGateway, Raw: "Accio WAF/Baxia challenge for this account. Open the Accio desktop app and log in with this account once so the device fingerprint is registered; afterwards the proxy remote path will also work."}
		}
		return err
	}
	return nil
}

func (c *Client) Chat(ctx context.Context, request map[string]any, w http.ResponseWriter) error {
	t, err := c.readToken()
	if err != nil {
		return err
	}
	return c.ChatWithToken(ctx, request, t.AccessToken, w)
}

func (c *Client) ChatWithToken(ctx context.Context, request map[string]any, token string, w http.ResponseWriter) error {
	resp, _, err := c.chatWithToken(ctx, request, token)
	if err != nil {
		return err
	}
	if resp == nil {
		return &GatewayError{Status: http.StatusUnauthorized, Raw: "session rejected before response"}
	}
	return c.writeChatResponse(resp, request, w)
}

func (c *Client) chatWithToken(ctx context.Context, request map[string]any, token string) (*http.Response, int, error) {
	body := buildRequest(request, token)
	b, _ := json.Marshal(body)
	id := firstString(request, "request_id")
	if id == "" {
		id = "req-" + randomID()
	}
	// The official Accio ADK transport (ct.streamGenerateContent from
	// @ali/accio-adk-ts) POSTs to `${gatewayLLM}/generateContent?sg_k=md5(reqId)`
	// with Content-Type: application/json and the protobuf-JSON body. The
	// `/api/adk/llm` base without the `/generateContent` sub-path returns
	// HTTP 404 from the Spring gateway — verified by direct comparison with
	// the desktop app's wire format.
	h := md5.Sum([]byte(id))
	endpoint := c.gatewayLLM + "/generateContent?sg_k=" + hex.EncodeToString(h[:])
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	c.applyChatHeaders(req, token)
	security, securityErr := c.securityHeaders(ctx, endpoint)
	if securityErr != nil {
		return nil, 0, securityErr
	}
	for key, value := range security {
		req.Header.Set(key, value)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		return nil, resp.StatusCode, &GatewayError{Status: resp.StatusCode, Raw: string(raw)}
	}
	return resp, resp.StatusCode, nil
}

// applyChatHeaders mirrors the EXACT header set the official Accio desktop
// client applies to /api/adk/llm/generateContent (verified against the app's
// httpClient interceptor + ct transport). Verified bits:
//
//   - Auth cookie: bc() returns `cookie2=${accessToken}` (getAccioCookie),
//     so the WAF sees a Cookie: cookie2=<token> on EVERY chat POST. We
//     MUST send `Cookie: cookie2=<accessToken>` — removing it breaks the
//     WAF correlation (this was the regression that gave us captcha).
//   - x-cna: Wc(bc()) extracts `cna=([^;]+)` from the cookie string. Since
//     our cookie is `cookie2=...` (no cna entry), Wc returns null and the
//     app sends x-cna: "" (empty). We must do the same — minting a fake
//     UUID here is what made the WAF lock us out for new accounts.
//   - Auth is NOT in an Authorization header — it lives only in the JSON
//     body as `token`. Sending Authorization: Bearer <...> on chat makes
//     the WAF classify the request as a different auth context.
//   - x-*: x-utdid, x-app-version, x-os, x-source, x-language, x-cna,
//     x-deploy-target, x-package-region are added by the httpClient
//     interceptor in the main process. utdid/version/appKey (lowercase)
//     are added by the ADK transport's z() helper.
func (c *Client) applyChatHeaders(req *http.Request, token string) {
	if t, err := c.readToken(); err == nil && strings.TrimSpace(token) == "" {
		token = t.AccessToken
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	// Header set verified against the official @ali/accio-adk-ts transport of
	// the Accio 0.26.0 desktop app (extracted from app.asar and executed
	// locally against the live gateway). The LLM request goes through a plain
	// fetch: only utdid/version/appKey (SDK initParams) plus the
	// transportInterceptor trio are sent. Everything the old httpClient
	// interceptor added (Cookie: cookie2, x-source, x-os, x-utdid, x-cna,
	// Electron UA, …) is NOT present on this route — sending it makes the
	// Baxia WAF punish the request with the sufei-punish captcha page.
	// Verified: SDK-style headers pass, cookie/Electron-UA variants get
	// punished on the same account/token.
	if v := strings.TrimSpace(c.utdid); v != "" {
		req.Header.Set("utdid", v)
	}
	if v := strings.TrimSpace(c.appVersion); v != "" {
		req.Header.Set("version", v)
	}
	if v := strings.TrimSpace(DefaultSecurityAppKey); v != "" {
		req.Header.Set("appKey", v)
	}
	req.Header.Set("x-deploy-target", "desktop")
	req.Header.Set("x-accio-route-region", "SG")
	req.Header.Set("x-package-region", "SG")
	// The SDK runs inside Node/Electron where fetch sends "node" (or the
	// Chromium UA with the exact Electron version). A bare "Accio/<version>"
	// or a guessed Electron UA is classified as a bot and punished; "node"
	// is accepted (verified against the live gateway).
	req.Header.Set("User-Agent", "node")
}

// chatLanguage returns the UI language used in x-language/x-locale headers.
// The desktop app derives these from its UI settings (pt-BR in this build);
// allow an override for other locales.
func (c *Client) chatLanguage() string {
	if v := strings.TrimSpace(os.Getenv("ACCIO_LANGUAGE")); v != "" {
		return v
	}
	return "pt-BR"
}

// extractCnaFromToken replicates Wc(bc()) from the desktop app: scan the
// persisted token's cookie blob for `cna=<value>` and return it. Returns
// empty string when not present (which matches what the desktop app sends
// for accounts whose login cookie never included a `cna` entry).
func (c *Client) extractCnaFromToken() string {
	t, err := c.readToken()
	if err != nil {
		return ""
	}
	cookie := strings.TrimSpace(t.Cookie)
	if cookie == "" {
		return ""
	}
	for _, part := range strings.Split(cookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && strings.TrimSpace(kv[0]) == "cna" {
			return strings.TrimSpace(kv[1])
		}
	}
	return ""
}

func randomID() string {
	b := make([]byte, 12)
	_, _ = cryptoRand.Read(b)
	return hex.EncodeToString(b)
}

func buildRequest(in map[string]any, token string) map[string]any {
	model, _ := in["model"].(string)
	model = strings.TrimPrefix(model, "accio/")
	contents := make([]map[string]any, 0)
	system := ""
	if msgs, ok := in["messages"].([]any); ok {
		for _, raw := range msgs {
			m, _ := raw.(map[string]any)
			role, _ := m["role"].(string)
			parts := messageParts(m)
			if role == "system" {
				text := textContent(m["content"])
				if system != "" && text != "" {
					system += "\n\n"
				}
				system += text
				continue
			}
			if role == "assistant" {
				role = "model"
			}
			if role == "tool" {
				role = "user"
			}
			if len(parts) > 0 {
				contents = append(contents, map[string]any{"role": role, "parts": parts})
			}
		}
	}
	id := firstString(in, "request_id")
	if id == "" {
		id = "req-" + randomID()
	}
	out := map[string]any{
		"model":          model,
		"incremental":    true,
		"requestId":      id,
		"userRequestId":  id,
		"messageId":      "msg-" + randomID(),
		"conversationId": "conv-" + randomID(),
		"sessionKey":     "sess-" + randomID(),
		"contents":       contents,
		"properties":     map[string]any{"normalized_response": "true"},
		"token":          token,
		// Required by the gateway's tenant/ADK routing layer. The official
		// desktop client always sends these three (defaulting to empty
		// empid/tenant and "phoenix-desktop" iaiTag). Without them the
		// gateway returns HTTP 404 {"error":"Not Found","path":"/api/adk/llm"}.
		"empid":  "",
		"tenant": "",
		"iaiTag": "phoenix-desktop",
	}
	if system != "" {
		out["systemInstruction"] = system
	}
	// The official Accio ADK wire contract uses camelCase. The gateway may
	// accept the OpenAI-shaped snake_case names at the HTTP boundary, but it
	// does not reliably interpret them once they are passed to the ADK layer.
	for src, dst := range map[string]string{
		"temperature":      "temperature",
		"top_p":            "topP",
		"stop_sequences":   "stopSequences",
		"tool_choice":      "toolChoice",
		"response_format":  "responseFormat",
		"thinking_budget":  "thinkingBudget",
		"thinking_level":   "thinkingLevel",
		"reasoning_effort": "reasoningEffort",
	} {
		if v, ok := in[src]; ok {
			out[dst] = v
		}
	}
	// ChatRequest is a Go value type, so the Wails UI path serializes an
	// omitted max_tokens field as numeric zero.  Zero is not "no limit" for
	// the Accio gateway: it means maxOutputTokens=0, which completes with
	// MAX_TOKENS before emitting any content.  Only forward a positive limit;
	// an omitted/zero limit lets the gateway apply its normal default.
	if v, ok := in["max_tokens"]; ok && positiveNumber(v) {
		out["maxOutputTokens"] = v
	}
	if v, ok := in["tools"]; ok {
		out["tools"] = convertTools(v)
	}
	return out
}

func positiveNumber(v any) bool {
	switch n := v.(type) {
	case int:
		return n > 0
	case int8:
		return n > 0
	case int16:
		return n > 0
	case int32:
		return n > 0
	case int64:
		return n > 0
	case uint:
		return n > 0
	case uint8:
		return n > 0
	case uint16:
		return n > 0
	case uint32:
		return n > 0
	case uint64:
		return n > 0
	case float32:
		return n > 0
	case float64:
		return n > 0
	case json.Number:
		value, err := n.Int64()
		return err == nil && value > 0
	default:
		return false
	}
}

func messageParts(m map[string]any) []any {
	parts := make([]any, 0)
	if reasoning, ok := m["reasoning_content"].(string); ok && strings.TrimSpace(reasoning) != "" {
		parts = append(parts, map[string]any{"text": reasoning, "thought": true})
	}
	content := m["content"]
	if list, ok := content.([]any); ok {
		for _, raw := range list {
			p, _ := raw.(map[string]any)
			switch p["type"] {
			case "text":
				if text, _ := p["text"].(string); text != "" {
					parts = append(parts, map[string]any{"text": text, "thought": false})
				}
			case "image_url":
				if image, ok := p["image_url"].(map[string]any); ok {
					if u, _ := image["url"].(string); strings.HasPrefix(u, "data:") {
						if mime, data, ok := decodeDataURL(u); ok {
							parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": data}, "thought": false})
						}
					}
				}
			}
		}
	} else if text := textContent(content); text != "" {
		parts = append(parts, map[string]any{"text": text, "thought": false})
	}
	if calls, ok := m["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			name, _ := fn["name"].(string)
			args, _ := fn["arguments"].(string)
			parts = append(parts, map[string]any{"functionCall": map[string]any{"id": call["id"], "name": name, "argsJson": args}})
		}
	}
	if m["role"] == "tool" {
		parts = append(parts, map[string]any{"functionResponse": map[string]any{"id": m["tool_call_id"], "name": m["name"], "responseJson": fmt.Sprintf(`{"result":%q}`, textContent(content))}})
	}
	return parts
}

func decodeDataURL(value string) (string, string, bool) {
	parts := strings.SplitN(value, ",", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "data:") || !strings.Contains(parts[0], ";base64") {
		return "", "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(parts[0], "data:"), ";base64"), parts[1], true
}

func convertTools(v any) any {
	list, ok := v.([]any)
	if !ok {
		return v
	}
	out := make([]any, 0, len(list))
	for _, raw := range list {
		t, _ := raw.(map[string]any)
		fn, _ := t["function"].(map[string]any)
		if len(fn) == 0 {
			out = append(out, raw)
			continue
		}
		params, _ := json.Marshal(fn["parameters"])
		out = append(out, map[string]any{"name": fn["name"], "description": fn["description"], "parameters_json": string(params)})
	}
	return out
}

func textContent(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if list, ok := v.([]any); ok {
		var b strings.Builder
		for _, x := range list {
			if m, ok := x.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

type framePart struct {
	Text         string `json:"text"`
	Thought      bool   `json:"thought"`
	FunctionCall *struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		ArgsJSON string `json:"argsJson"`
	} `json:"functionCall"`
	FunctionCallSnake *struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		ArgsJSON string `json:"args_json"`
	} `json:"function_call"`
}

type frameContent struct {
	Parts []framePart `json:"parts"`
}

type frameEnvelope struct {
	Content *frameContent `json:"content"`
}

type frame struct {
	Content           *frameContent  `json:"content"`
	Candidate         *frameEnvelope `json:"candidate"`
	Payload           *frameEnvelope `json:"payload"`
	TurnComplete      bool           `json:"turnComplete"`
	TurnCompleteSnake bool           `json:"turn_complete"`
	FinishReason      string         `json:"finishReason"`
	FinishReasonSnake string         `json:"finish_reason"`
	Usage             *struct {
		Prompt     int `json:"promptTokenCount"`
		Completion int `json:"candidatesTokenCount"`
		Total      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	UsageSnake *struct {
		Prompt     int `json:"prompt_token_count"`
		Completion int `json:"candidates_token_count"`
		Total      int `json:"total_token_count"`
	} `json:"usage_metadata"`
	Error           any            `json:"error"`
	GatewayError    any            `json:"gatewayError"`
	GatewayPayload  map[string]any `json:"gatewayPayload"`
	GatewayEnvelope map[string]any `json:"gatewayEnvelope"`
}

func (c *Client) writeChatResponse(resp *http.Response, request map[string]any, w http.ResponseWriter) error {
	defer resp.Body.Close()
	model, _ := request["model"].(string)
	stream, _ := request["stream"].(bool)
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		id := "chatcmpl-" + randomID()
		writeSSE(w, map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}})
		if fl != nil {
			fl.Flush()
		}
		return c.readFrames(resp.Body, func(text, reasoning string, call map[string]any, done bool) {
			delta := map[string]any{}
			if text != "" {
				delta["content"] = text
			}
			if reasoning != "" {
				delta["reasoning_content"] = reasoning
			}
			if call != nil {
				delta["tool_calls"] = []any{call}
			}
			if len(delta) > 0 {
				writeSSE(w, map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}})
			}
			if done {
				writeSSE(w, map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}
			if fl != nil {
				fl.Flush()
			}
		})
	}
	var text, reasoning string
	var calls []map[string]any
	err := c.readFrames(resp.Body, func(t, r string, call map[string]any, _ bool) {
		text += t
		reasoning += r
		if call != nil {
			calls = append(calls, call)
		}
	})
	if err != nil {
		return err
	}
	msg := map[string]any{"role": "assistant", "content": text}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	return json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-" + randomID(), "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}})
}

func writeSSE(w io.Writer, v any) { b, _ := json.Marshal(v); _, _ = fmt.Fprintf(w, "data: %s\n\n", b) }
func (c *Client) readFrames(r io.Reader, emit func(string, string, map[string]any, bool)) error {
	var buf bytes.Buffer
	tee := io.TeeReader(r, &buf)
	sc := bufio.NewScanner(tee)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	completed := false
	frames := 0
	textChars := 0
	reasonChars := 0
	calls := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		lowerLine := strings.ToLower(line)
		if strings.HasPrefix(lowerLine, "<!doctype html") || strings.HasPrefix(lowerLine, "<html") {
			return &GatewayError{Status: http.StatusBadGateway, Raw: "Accio gateway returned an HTML challenge/captcha page instead of SSE"}
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" {
			continue
		}
		lowerRaw := strings.ToLower(raw)
		if strings.HasPrefix(lowerRaw, "<!doctype html") || strings.HasPrefix(lowerRaw, "<html") {
			return &GatewayError{Status: http.StatusBadGateway, Raw: "Accio gateway returned an HTML challenge/captcha page instead of SSE"}
		}
		if raw == "[DONE]" {
			if !completed {
				completed = true
				emit("", "", nil, true)
			}
			continue
		}
		var f frame
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			continue
		}
		frames++
		if err := frameError(f); err != nil {
			return &GatewayError{Status: http.StatusBadGateway, Raw: raw, Cause: err}
		}
		content := f.Content
		if content == nil && f.Candidate != nil {
			content = f.Candidate.Content
		}
		if content == nil && f.Payload != nil {
			content = f.Payload.Content
		}
		if content != nil {
			for _, p := range content.Parts {
				callID, callName, callArgs := "", "", ""
				if p.FunctionCall != nil {
					callID, callName, callArgs = p.FunctionCall.ID, p.FunctionCall.Name, p.FunctionCall.ArgsJSON
				} else if p.FunctionCallSnake != nil {
					callID, callName, callArgs = p.FunctionCallSnake.ID, p.FunctionCallSnake.Name, p.FunctionCallSnake.ArgsJSON
				}
				if callName != "" {
					calls++
					emit("", "", map[string]any{"index": 0, "id": callID, "type": "function", "function": map[string]any{"name": callName, "arguments": callArgs}}, false)
				} else if p.Thought {
					reasonChars += len(p.Text)
					emit("", p.Text, nil, false)
				} else {
					textChars += len(p.Text)
					emit(p.Text, "", nil, false)
				}
			}
		}
		done := f.TurnComplete || f.TurnCompleteSnake ||
			strings.EqualFold(strings.TrimSpace(f.FinishReason), "stop") ||
			strings.EqualFold(strings.TrimSpace(f.FinishReasonSnake), "stop")
		if done && !completed {
			completed = true
			emit("", "", nil, true)
		}
	}
	if err := sc.Err(); err != nil {
		logging.Warn("accio.stream.read_err", "err", err.Error(), "bytes", buf.Len())
		return err
	}
	// Some production deployments close a successful SSE response without a
	// final turnComplete frame. EOF is still a terminal upstream signal; emit
	// the OpenAI-compatible completion marker instead of leaving clients stuck.
	if !completed {
		emit("", "", nil, true)
	}
	if textChars == 0 && reasonChars == 0 && calls == 0 {
		logging.Warn("accio.stream.empty", "frames", frames, "bytes", buf.Len(), "completed", completed, "preview", truncateBytes(buf.Bytes(), 200))
	} else {
		logging.Info("accio.stream.done", "frames", frames, "text_chars", textChars, "reason_chars", reasonChars, "calls", calls, "completed", completed)
	}
	return nil
}

func truncateBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return strings.TrimSpace(string(b))
}

func frameError(f frame) error {
	if f.GatewayPayload != nil {
		code := firstString(f.GatewayPayload, "error_code", "errorCode", "code")
		msg := firstString(f.GatewayPayload, "error_message", "errorMessage", "message")
		if code != "" || msg != "" {
			return fmt.Errorf("accio gateway %s: %s", code, msg)
		}
	}
	if f.GatewayEnvelope != nil {
		if code, ok := f.GatewayEnvelope["code"].(float64); ok && code >= 400 {
			return fmt.Errorf("accio gateway HTTP %.0f: %s", code, firstString(f.GatewayEnvelope, "messageRaw", "message"))
		}
	}
	for _, raw := range []any{f.Error, f.GatewayError} {
		if raw == nil {
			continue
		}
		if m, ok := raw.(map[string]any); ok {
			if msg := firstString(m, "message", "errorMessage", "error"); msg != "" {
				return errors.New("accio gateway: " + msg)
			}
		}
		if s, ok := raw.(string); ok && strings.TrimSpace(s) != "" {
			return errors.New("accio gateway: " + s)
		}
	}
	return nil
}
