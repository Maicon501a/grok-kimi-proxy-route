package kimi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"grok-desktop/internal/store"
)

const (
	DefaultKimiURL = "https://www.kimi.com"
	KimiWorkBaseURL = "https://agent-gw.kimi.com/coding/v1"
	CreateAPIKeyRPC = "/apiv2/kimi.gateway.credentials.v1.APIKeyService/CreateAPIKey"
	FeatureWORK     = 9
	FeatureCODING   = 4
)

var jwtRE = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

type JWTPayload struct {
	Sub        string `json:"sub"`
	SSID       string `json:"ssid"`
	DeviceID   any    `json:"device_id"`
	Typ        string `json:"typ"`
	Exp        int64  `json:"exp"`
	Membership struct {
		Level int `json:"level"`
	} `json:"membership"`
}

type MintResult struct {
	APIKey    string
	UserID    string
	DeviceID  string
	SSID      string
	Raw       map[string]any
	Name      string
	Scope     int
	ExpiresAt time.Time // access token exp (not the sk)
}

func DecodeJWT(token string) (*JWTPayload, error) {
	token = strings.TrimPrefix(strings.TrimSpace(token), "Bearer ")
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid jwt")
	}
	payload := parts[1]
	if m := len(payload) % 4; m != 0 {
		payload += strings.Repeat("=", 4-m)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, err
		}
	}
	var p JWTPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func DeviceIDString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%.0f", t)
	case json.Number:
		return t.String()
	default:
		return fmt.Sprint(v)
	}
}

// ExtractAccessFromDesktop scans Kimi Desktop Local Storage for the best access JWT.
func ExtractAccessFromDesktop() (token string, payload *JWTPayload, err error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		home, _ := os.UserHomeDir()
		appData = filepath.Join(home, "AppData", "Roaming")
	}
	ld := filepath.Join(appData, "kimi-desktop", "Local Storage", "leveldb")
	entries, err := os.ReadDir(ld)
	if err != nil {
		return "", nil, fmt.Errorf("kimi desktop local storage not found: %w", err)
	}
	type cand struct {
		tok string
		p   *JWTPayload
	}
	var list []cand
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(ld, e.Name()))
		if err != nil {
			continue
		}
		// latin1-ish scan for embedded JWTs
		matches := jwtRE.FindAll(b, -1)
		for _, m := range matches {
			tok := string(m)
			if seen[tok] {
				continue
			}
			seen[tok] = true
			p, err := DecodeJWT(tok)
			if err != nil || p == nil || p.Typ != "access" {
				continue
			}
			list = append(list, cand{tok: tok, p: p})
		}
	}
	if len(list) == 0 {
		return "", nil, fmt.Errorf("no access_token in Kimi Desktop (login no app)")
	}
	sort.Slice(list, func(i, j int) bool {
		si, sj := 0, 0
		if list[i].p.Sub != "" {
			si = 1
		}
		if list[j].p.Sub != "" {
			sj = 1
		}
		if si != sj {
			return si > sj
		}
		return list[i].p.Exp > list[j].p.Exp
	})
	best := list[0]
	return best.tok, best.p, nil
}

// MintWorkAPIKey calls CreateAPIKey with scope WORK (same as Desktop).
// The gateway reuses one active WORK key per user.
func MintWorkAPIKey(accessToken, name string) (*MintResult, error) {
	accessToken = strings.TrimPrefix(strings.TrimSpace(accessToken), "Bearer ")
	if accessToken == "" {
		return nil, fmt.Errorf("access token required")
	}
	p, err := DecodeJWT(accessToken)
	if err != nil {
		return nil, err
	}
	if p.Exp > 0 && time.Now().Unix() > p.Exp {
		return nil, fmt.Errorf("access_token expired")
	}
	if name == "" {
		name = "kimi-proxy-desktop"
	}
	deviceID := DeviceIDString(p.DeviceID)
	headers := func(ct string) http.Header {
		h := make(http.Header)
		h.Set("Authorization", "Bearer "+accessToken)
		h.Set("Content-Type", ct)
		h.Set("Accept", "application/json")
		h.Set("Connect-Protocol-Version", "1")
		h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) KimiDesktop/3.1.0 Chrome/134.0.0.0 Safari/537.36")
		h.Set("x-msh-platform", "windows")
		h.Set("x-msh-version", "3.1.0")
		h.Set("x-language", "en-US")
		h.Set("R-Timezone", "America/Sao_Paulo")
		h.Set("Origin", DefaultKimiURL)
		h.Set("Referer", DefaultKimiURL+"/")
		if deviceID != "" {
			h.Set("x-msh-device-id", deviceID)
		}
		if p.SSID != "" {
			h.Set("x-msh-session-id", p.SSID)
		}
		if p.Sub != "" {
			h.Set("x-traffic-id", p.Sub)
		}
		return h
	}

	type attempt struct {
		ct   string
		body any
	}
	attempts := []attempt{
		{"application/json", map[string]any{"apiKey": map[string]any{"name": name, "scope": []int{FeatureWORK}}}},
		{"application/json", map[string]any{"api_key": map[string]any{"name": name, "scope": []int{FeatureWORK}}}},
		{"application/connect+json", map[string]any{"apiKey": map[string]any{"name": name, "scope": []int{FeatureWORK}}}},
	}

	url := DefaultKimiURL + CreateAPIKeyRPC
	client := &http.Client{Timeout: 45 * time.Second}
	var lastErr error
	for _, a := range attempts {
		raw, _ := json.Marshal(a.body)
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(raw)))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header = headers(a.ct)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		var parsed map[string]any
		_ = json.Unmarshal(b, &parsed)
		key := extractKey(parsed)
		if resp.StatusCode < 400 && key != "" {
			out := &MintResult{
				APIKey:   key,
				UserID:   p.Sub,
				DeviceID: deviceID,
				SSID:     p.SSID,
				Raw:      parsed,
				Name:     name,
				Scope:    FeatureWORK,
			}
			if p.Exp > 0 {
				out.ExpiresAt = time.Unix(p.Exp, 0).UTC()
			}
			return out, nil
		}
		lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(b), 240))
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("CreateAPIKey failed")
	}
	return nil, lastErr
}

func extractKey(m map[string]any) string {
	if m == nil {
		return ""
	}
	if k, ok := m["key"].(string); ok && strings.HasPrefix(k, "sk-kimi-") {
		return k
	}
	for _, nest := range []string{"apiKey", "api_key"} {
		if sub, ok := m[nest].(map[string]any); ok {
			if k, ok := sub["key"].(string); ok && k != "" {
				return k
			}
			if k, ok := sub["apiKey"].(string); ok && k != "" {
				return k
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestAPIKey checks whether a sk-kimi WORK key is accepted by the upstream.
func TestAPIKey(apiKey string) error {
	apiKey = strings.TrimPrefix(strings.TrimSpace(apiKey), "Bearer ")
	if apiKey == "" {
		return fmt.Errorf("api key required")
	}
	req, err := http.NewRequest(http.MethodGet, KimiWorkBaseURL+"/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) KimiDesktop/3.1.0 Chrome/134.0.0.0 Safari/537.36")
	req.Header.Set("x-msh-platform", "kimi-code-cli")
	req.Header.Set("x-msh-version", "0.23.5")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(b), 240))
	}
	return nil
}

// TestRefreshToken checks whether a Kimi refresh token is valid.
func TestRefreshToken(refreshToken string) error {
	_, err := RefreshAccessToken(refreshToken)
	return err
}

// ResolveKimiModel maps Desktop / OpenCode aliases to agent-gw wire id.
// Real wire models from official Kimi Desktop: k3-agent, k2d6-agent, k2p6, k3-agent-swarm.
func ResolveKimiModel(requested string) string {
	return store.Settings{Provider: store.ProviderKimiWork}.ResolveModelForClient(requested)
}

// StaticModels for ListModels when provider is kimi_work.
// Wire ids match agent-gw / Kimi Desktop (not public moonshot kimi-k3 ids).
func StaticModels() []map[string]string {
	return []map[string]string{
		{"id": "k3-agent", "name": "K3 Max (Work)", "api_mode": "responses"},
		{"id": "k3-agent-low", "name": "K3 Max — Low Think", "api_mode": "responses"},
		{"id": "k3-agent-medium", "name": "K3 Max — Medium Think", "api_mode": "responses"},
		{"id": "k3-agent-high", "name": "K3 Max — High Think", "api_mode": "responses"},
		{"id": "k3-agent-xhigh", "name": "K3 Max — Extra High Think", "api_mode": "responses"},
		{"id": "k2d6-agent", "name": "K2.6 Agent (Work)", "api_mode": "responses"},
		{"id": "k2d6-agent-low", "name": "K2.6 Agent — Low Think", "api_mode": "responses"},
		{"id": "k2d6-agent-medium", "name": "K2.6 Agent — Medium Think", "api_mode": "responses"},
		{"id": "k2d6-agent-high", "name": "K2.6 Agent — High Think", "api_mode": "responses"},
		{"id": "k2d6-agent-xhigh", "name": "K2.6 Agent — Extra High Think", "api_mode": "responses"},
	}
}
