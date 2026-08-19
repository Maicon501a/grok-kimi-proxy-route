package kimi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Session is a standalone web session (no Desktop APPDATA).
type Session struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	UserID       string `json:"user_id"`
	Email        string `json:"email,omitempty"`
	Name         string `json:"name,omitempty"`
	DeviceID     string `json:"device_id"`
	SSID         string `json:"ssid"`
	Exp          int64  `json:"exp"`
	Source       string `json:"source"`
	CapturedAt   string `json:"captured_at"`
}

// RefreshAccessToken renews an access JWT using the current Kimi Desktop
// AuthService.RefreshToken Connect RPC. The former web GET endpoint is kept as
// a fallback for older sessions/builds.
func RefreshAccessToken(refreshToken string) (*Session, error) {
	refreshToken = strings.TrimPrefix(strings.TrimSpace(refreshToken), "Bearer ")
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh_token required")
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": refreshToken})
	req, err := http.NewRequest(http.MethodPost, "https://auth.kimi.com/api/account.gateway.v1.AuthService/RefreshToken", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setRefreshHeaders(req, refreshToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return refreshAccessTokenLegacy(client, refreshToken)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("refresh RPC HTTP %d: %s", resp.StatusCode, truncate(string(b), 240))
	}
	return parseRefreshedSession(b, refreshToken)
}

func setRefreshHeaders(req *http.Request, refreshToken string) {
	req.Header.Set("Authorization", "Bearer "+refreshToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 KimiDesktop/3.2.0 Chrome/134.0.0.0 Safari/537.36")
	req.Header.Set("Origin", DefaultKimiURL)
	req.Header.Set("Referer", DefaultKimiURL+"/")
	req.Header.Set("x-msh-platform", "windows")
	req.Header.Set("x-msh-version", "3.2.0")
	req.Header.Set("X-Language", "en-US")
	req.Header.Set("R-Timezone", "America/Sao_Paulo")
}

func parseRefreshedSession(b []byte, refreshToken string) (*Session, error) {
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	access, _ := data["access_token"].(string)
	if access == "" {
		access, _ = data["accessToken"].(string)
	}
	newRefresh, _ := data["refresh_token"].(string)
	if newRefresh == "" {
		newRefresh, _ = data["refreshToken"].(string)
	}
	if newRefresh == "" {
		newRefresh = refreshToken
	}
	if access == "" {
		return nil, fmt.Errorf("refresh response missing access_token")
	}
	p, _ := DecodeJWT(access)
	s := &Session{
		AccessToken:  access,
		RefreshToken: newRefresh,
		Source:       "refresh",
		CapturedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if p != nil {
		s.UserID = p.Sub
		s.DeviceID = DeviceIDString(p.DeviceID)
		s.SSID = p.SSID
		s.Exp = p.Exp
	}
	return s, nil
}

func refreshAccessTokenLegacy(client *http.Client, refreshToken string) (*Session, error) {
	req, err := http.NewRequest(http.MethodGet, DefaultKimiURL+"/api/auth/token/refresh", nil)
	if err != nil {
		return nil, err
	}
	setRefreshHeaders(req, refreshToken)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("legacy refresh HTTP %d: %s", resp.StatusCode, truncate(string(b), 240))
	}
	return parseRefreshedSession(b, refreshToken)
}

// BrowserLoginOptions configures standalone Playwright login.
type BrowserLoginOptions struct {
	// ProjectRoot is the GrokDesktop repo root (contains scripts/kimi-browser-login.mjs).
	ProjectRoot string
	// ProfileDir isolated browser profile (empty = auto temp under browser-data).
	ProfileDir string
	// OutPath session JSON path (empty = auto).
	OutPath string
	// Timeout seconds
	TimeoutSec int
	// Node binary
	Node string
}

// RunBrowserLogin launches a real Chrome via Playwright, user logs into kimi.com,
// returns access+refresh without reading Kimi Desktop APPDATA.
func RunBrowserLogin(opts BrowserLoginOptions) (*Session, error) {
	root := strings.TrimSpace(opts.ProjectRoot)
	if root == "" {
		return nil, fmt.Errorf("project root required")
	}
	script := filepath.Join(root, "scripts", "kimi-browser-login.mjs")
	if _, err := os.Stat(script); err != nil {
		return nil, fmt.Errorf("login script missing: %s", script)
	}
	node := opts.Node
	if node == "" {
		node = "node"
	}
	timeout := opts.TimeoutSec
	if timeout <= 0 {
		timeout = 600
	}
	out := opts.OutPath
	if out == "" {
		out = filepath.Join(os.TempDir(), fmt.Sprintf("kimi-session-%d.json", time.Now().UnixNano()))
	}
	profile := opts.ProfileDir
	if profile == "" {
		profile = filepath.Join(root, "browser-data", fmt.Sprintf("kimi-%d", time.Now().UnixNano()))
	}
	_ = os.MkdirAll(filepath.Dir(out), 0o700)
	_ = os.MkdirAll(profile, 0o700)
	_ = os.Remove(out)

	// Capture stdout/stderr so UI shows real failure reason
	var stdout, stderr strings.Builder
	cmd := exec.Command(node, script,
		"--out", out,
		"--profile", profile,
		"--timeout", fmt.Sprintf("%d", timeout),
	)
	cmd.Dir = root
	nodePath := filepath.Join(`C:\Users\maicon2\Documents\dev\proxy-kimi`, "node_modules")
	env := append([]string{}, os.Environ()...)
	if st, err := os.Stat(nodePath); err == nil && st.IsDir() {
		env = append(env, "NODE_PATH="+nodePath)
	}
	cmd.Env = env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Start()
	_ = SetupProcessJob(cmd)
	if err == nil {
		err = cmd.Wait()
	}
	outLog := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
	if err != nil {
		// still try reading out if partial
		if s, rerr := ReadSessionFile(out); rerr == nil && s.AccessToken != "" {
			return s, nil
		}
		msg := err.Error()
		if outLog != "" {
			// keep last useful lines
			lines := strings.Split(outLog, "\n")
			if len(lines) > 12 {
				lines = lines[len(lines)-12:]
			}
			msg = strings.TrimSpace(strings.Join(lines, "\n"))
		}
		return nil, fmt.Errorf("browser login failed: %s", msg)
	}
	s, rerr := ReadSessionFile(out)
	if rerr != nil {
		if outLog != "" {
			return nil, fmt.Errorf("session file: %v | log: %s", rerr, truncate(outLog, 400))
		}
		return nil, rerr
	}
	return s, nil
}

// ReadSessionFile loads session JSON written by kimi-browser-login.mjs
func ReadSessionFile(path string) (*Session, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	s.AccessToken = strings.TrimPrefix(strings.TrimSpace(s.AccessToken), "Bearer ")
	s.RefreshToken = strings.TrimPrefix(strings.TrimSpace(s.RefreshToken), "Bearer ")
	if s.AccessToken == "" {
		return nil, fmt.Errorf("session file missing access_token")
	}
	if p, err := DecodeJWT(s.AccessToken); err == nil && p != nil {
		if s.UserID == "" {
			s.UserID = p.Sub
		}
		if s.DeviceID == "" {
			s.DeviceID = DeviceIDString(p.DeviceID)
		}
		if s.SSID == "" {
			s.SSID = p.SSID
		}
		if s.Exp == 0 {
			s.Exp = p.Exp
		}
	}
	return &s, nil
}
