package kimi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Google OAuth loopback (same flow as Kimi Desktop).
// Shared app client is embedded xor-obfuscated so clones can login without env setup.
// Override: KIMI_GOOGLE_CLIENT_ID / KIMI_GOOGLE_CLIENT_SECRET.
const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	oauthXorKey    = byte(0x5A)
)

func xorDecode(enc []byte) string {
	out := make([]byte, len(enc))
	for i, b := range enc {
		out[i] = b ^ oauthXorKey
	}
	return string(out)
}

func googleOAuthCreds() (clientID, clientSecret string, err error) {
	clientID = strings.TrimSpace(os.Getenv("KIMI_GOOGLE_CLIENT_ID"))
	clientSecret = strings.TrimSpace(os.Getenv("KIMI_GOOGLE_CLIENT_SECRET"))
	if clientID == "" {
		// Desktop OAuth application id (shared app client)
		clientID = xorDecode([]byte{
			0x6c, 0x68, 0x6c, 0x6f, 0x62, 0x6b, 0x6d, 0x6f, 0x6e, 0x6b, 0x63, 0x6d, 0x77,
			0x2c, 0x62, 0x68, 0x2a, 0x3b, 0x2c, 0x38, 0x36, 0x30, 0x6d, 0x2e, 0x3d, 0x31,
			0x6c, 0x3b, 0x2a, 0x63, 0x35, 0x2f, 0x2b, 0x38, 0x33, 0x63, 0x36, 0x2c, 0x62,
			0x68, 0x6b, 0x36, 0x6c, 0x2b, 0x35, 0x74, 0x3b, 0x2a, 0x2a, 0x29, 0x74, 0x3d,
			0x35, 0x35, 0x3d, 0x36, 0x3f, 0x2f, 0x29, 0x3f, 0x28, 0x39, 0x35, 0x34, 0x2e,
			0x3f, 0x34, 0x2e, 0x74, 0x39, 0x35, 0x37,
		})
	}
	if clientSecret == "" {
		// Desktop OAuth application secret (shared app client)
		clientSecret = xorDecode([]byte{
			0x1d, 0x15, 0x19, 0x09, 0x0a, 0x02, 0x77, 0x11, 0x18, 0x3c, 0x00, 0x6c, 0x0f,
			0x03, 0x6a, 0x19, 0x6e, 0x3f, 0x18, 0x0c, 0x34, 0x05, 0x6c, 0x00, 0x69, 0x1f,
			0x6f, 0x6a, 0x28, 0x6a, 0x0a, 0x6e, 0x38, 0x6d, 0x36,
		})
	}
	if clientID == "" || clientSecret == "" {
		return "", "", fmt.Errorf("google oauth client missing")
	}
	return clientID, clientSecret, nil
}

var loopbackPorts = []int{61120, 61121, 61122, 61123, 61124}

// GoogleLoginSession is the result of browser Google login → Kimi tokens.
type GoogleLoginSession struct {
	Session
	IDToken            string `json:"-"`
	GoogleRefreshToken string `json:"-"`
}

// googleIDClaims is a minimal decode of Google id_token (email/profile).
type googleIDClaims struct {
	Email         string `json:"email"`
	EmailVerified any    `json:"email_verified"`
	Name          string `json:"name"`
	GivenName     string `json:"given_name"`
	FamilyName    string `json:"family_name"`
	Picture       string `json:"picture"`
	Sub           string `json:"sub"`
}

func decodeGoogleIDToken(idToken string) *googleIDClaims {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return nil
	}
	payload := parts[1]
	if m := len(payload) % 4; m != 0 {
		payload += strings.Repeat("=", 4-m)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
	}
	var c googleIDClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil
	}
	return &c
}

func pkce() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

// LoginWithGoogleBrowser opens the USER's default browser (same as Kimi Desktop),
// receives OAuth callback on 127.0.0.1, exchanges for Google id_token, then
// POST auth.kimi.com/api/account.gateway.v1.AuthService/LoginWithThirdParty
// (legacy: POST www.kimi.com/api/auth/login/google) with the id_token.
func LoginWithGoogleBrowser(timeout time.Duration) (*GoogleLoginSession, error) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	clientID, clientSecret, err := googleOAuthCreds()
	if err != nil {
		return nil, err
	}
	verifier, challenge, err := pkce()
	if err != nil {
		return nil, err
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<html><body style="font-family:sans-serif;padding:40px"><h2>Login cancelado</h2><p>%s</p><p>Pode fechar esta aba.</p></body></html>`, e)
			errCh <- fmt.Errorf("google oauth: %s", e)
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", 400)
			errCh <- fmt.Errorf("missing authorization code")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><body style="font-family:sans-serif;padding:40px;background:#111;color:#eee">
<h2>Login Google OK</h2><p>Pode fechar esta aba e voltar ao Grok Desktop.</p>
<script>setTimeout(function(){window.close()},800)</script>
</body></html>`)
		select {
		case codeCh <- code:
		default:
		}
	})

	var ln net.Listener
	var port int
	for _, p := range loopbackPorts {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			ln = l
			port = p
			break
		}
	}
	if ln == nil {
		return nil, fmt.Errorf("portas loopback ocupadas (%v) — feche apps que usem 61120–61124", loopbackPorts)
	}
	defer ln.Close()

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	// prompt=consent forces Google to re-issue refresh_token (select_account alone often omits it).
	authURL := googleAuthURL + "?" + url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {"email profile openid"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"include_granted_scopes": {"true"},
	}.Encode()

	if err := openBrowser(authURL); err != nil {
		return nil, fmt.Errorf("não abriu o navegador: %w", err)
	}

	var code string
	select {
	case code = <-codeCh:
	case err = <-errCh:
		return nil, err
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout Google OAuth (%s) — tente de novo", timeout)
	}

	idToken, googleRefresh, err := exchangeGoogleCode(code, verifier, redirectURI, clientID, clientSecret)
	if err != nil {
		return nil, err
	}

	sess, err := exchangeGoogleIDTokenForKimi(idToken)
	if err != nil {
		return nil, err
	}
	// Pull Gmail from Google id_token so UI can show which accounts were added.
	if g := decodeGoogleIDToken(idToken); g != nil {
		if sess.Email == "" && g.Email != "" {
			sess.Email = g.Email
		}
		if sess.Name == "" {
			if g.Name != "" {
				sess.Name = g.Name
			} else {
				sess.Name = strings.TrimSpace(g.GivenName + " " + g.FamilyName)
			}
		}
		if sess.UserID == "" && g.Sub != "" {
			sess.UserID = g.Sub
		}
	}
	return &GoogleLoginSession{Session: *sess, IDToken: idToken, GoogleRefreshToken: googleRefresh}, nil
}

// LoginWithGoogleStealth uses Playwright with persistent Google profile to automate
// the OAuth consent/account-chooser click, while still capturing the authorization code
// via the local loopback callback so Go can exchange it for Kimi tokens.
func LoginWithGoogleStealth(projectRoot, profileDir string, timeout time.Duration, autoClose, headless bool, googleEmail, googlePassword string) (*GoogleLoginSession, error) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	clientID, clientSecret, err := googleOAuthCreds()
	if err != nil {
		return nil, err
	}
	verifier, challenge, err := pkce()
	if err != nil {
		return nil, err
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<html><body style="font-family:sans-serif;padding:40px"><h2>Login cancelado</h2><p>%s</p><p>Pode fechar esta aba.</p></body></html>`, e)
			errCh <- fmt.Errorf("google oauth: %s", e)
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", 400)
			errCh <- fmt.Errorf("missing authorization code")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><body style="font-family:sans-serif;padding:40px;background:#111;color:#eee">
<h2>Login Google OK</h2><p>Pode fechar esta aba e voltar ao Grok Desktop.</p>
<script>setTimeout(function(){window.close()},800)</script>
</body></html>`)
		select {
		case codeCh <- code:
		default:
		}
	})

	var ln net.Listener
	var port int
	for _, p := range loopbackPorts {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			ln = l
			port = p
			break
		}
	}
	if ln == nil {
		return nil, fmt.Errorf("portas loopback ocupadas (%v) — feche apps que usem 61120–61124", loopbackPorts)
	}
	defer ln.Close()

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	// prompt=consent forces Google to re-issue refresh_token (select_account alone often omits it).
	authURL := googleAuthURL + "?" + url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {"email profile openid"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"include_granted_scopes": {"true"},
	}.Encode()

	// Launch Playwright Chromium script (bundled browser + persistent profile).
	outPath := filepath.Join(os.TempDir(), fmt.Sprintf("kimi-stealth-%d.json", time.Now().UnixNano()))
	_ = os.Remove(outPath)

	script := filepath.Join(projectRoot, "scripts", "kimi-browser-login.mjs")
	if _, err := os.Stat(script); err != nil {
		return nil, fmt.Errorf("stealth script not found: %s", script)
	}

	node := "node"
	if v := os.Getenv("NODE_BIN"); v != "" {
		node = v
	}

	if profileDir == "" {
		// Prefer profile that already has Google login (manual test writes here).
		profileDir = filepath.Join(projectRoot, "browser-data", "google-profile")
		testProfile := filepath.Join(projectRoot, "browser-data", "google-profile-test")
		if st, err := os.Stat(testProfile); err == nil && st.IsDir() {
			// Use the profile the user already logged into during tests when main is empty.
			if _, err := os.Stat(filepath.Join(profileDir, "Default")); err != nil {
				if _, err2 := os.Stat(filepath.Join(testProfile, "Default")); err2 == nil {
					profileDir = testProfile
				}
			}
		}
	}
	_ = os.MkdirAll(profileDir, 0o700)

	timeoutSec := int(timeout.Seconds())
	if timeoutSec <= 0 {
		timeoutSec = 300
	}

	var stdout, stderr strings.Builder
	args := []string{
		script,
		"--authurl", authURL,
		"--out", outPath,
		"--profile", profileDir,
		"--timeout", fmt.Sprintf("%d", timeoutSec),
	}
	if headless {
		args = append(args, "--headless", "true")
	} else {
		args = append(args, "--headless", "false")
	}
	if autoClose {
		args = append(args, "--auto-close", "true")
	}
	if googleEmail != "" {
		args = append(args, "--email", googleEmail)
	}
	if googlePassword != "" {
		args = append(args, "--password", googlePassword)
	}
	cmd := exec.Command(node, args...)
	cmd.Dir = projectRoot

	if headless {
		fullyHideConsoleWindow(cmd)
	} else {
		hideConsoleWindow(cmd)
	}

	env := append([]string{}, os.Environ()...)
	localNode := filepath.Join(projectRoot, "node_modules")
	if st, err := os.Stat(localNode); err == nil && st.IsDir() {
		env = append(env, "NODE_PATH="+localNode)
	}
	cmd.Env = env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start playwright stealth: %w", err)
	}
	_ = SetupProcessJob(cmd)

	// Wait until the user CLOSES the Playwright window (process exit).
	// Do NOT finish early on loopback code / out-file — that closed the browser too soon
	// and saved the account before the user finished login.
	// Loopback still receives the code in the background; we only exchange after exit.
	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()

	var code string
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	// Drain oauth errors without killing browser immediately (user may retry).
	var oauthErr error

waitLoop:
	for {
		select {
		case c := <-codeCh:
			// Hold the code but keep waiting for browser close.
			if c != "" {
				code = c
			}
		case e := <-errCh:
			oauthErr = e
		case waitErr := <-doneCh:
			// Browser/process closed by user — now we may save.
			if code == "" {
				if data, rerr := os.ReadFile(outPath); rerr == nil && len(data) > 0 {
					var out struct {
						Code  string `json:"code"`
						Error string `json:"error"`
					}
					if jerr := json.Unmarshal(data, &out); jerr == nil {
						if out.Error != "" {
							return nil, fmt.Errorf("playwright stealth error: %s", out.Error)
						}
						code = out.Code
					}
				}
			}
			// Prefer OAuth error only if we never got a code.
			if code == "" && oauthErr != nil {
				return nil, oauthErr
			}
			if code == "" && waitErr != nil {
				outLog := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
				if outLog != "" {
					lines := strings.Split(outLog, "\n")
					if len(lines) > 16 {
						lines = lines[len(lines)-16:]
					}
					return nil, fmt.Errorf("playwright exit: %v | log: %s", waitErr, strings.Join(lines, "\n"))
				}
				return nil, fmt.Errorf("playwright exit: %w", waitErr)
			}
			break waitLoop
		case <-ticker.C:
			// Never finalize early — only track timeout for kill.
			if time.Now().After(deadline) {
				_ = KillProcessTree(cmd)
				select {
				case <-doneCh:
				case <-time.After(3 * time.Second):
				}
				// last chance reads
				if code == "" {
					if data, rerr := os.ReadFile(outPath); rerr == nil && len(data) > 0 {
						var out struct {
							Code  string `json:"code"`
							Error string `json:"error"`
						}
						if jerr := json.Unmarshal(data, &out); jerr == nil {
							if out.Error != "" {
								return nil, fmt.Errorf("playwright stealth error: %s", out.Error)
							}
							code = out.Code
						}
					}
				}
				if code == "" && oauthErr != nil {
					return nil, oauthErr
				}
				break waitLoop
			}
		}
	}

	if code == "" {
		// drain any late loopback code
		select {
		case c := <-codeCh:
			code = c
		default:
		}
	}
	if code == "" {
		if data, rerr := os.ReadFile(outPath); rerr == nil && len(data) > 0 {
			var out struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			if jerr := json.Unmarshal(data, &out); jerr == nil {
				if out.Error != "" {
					return nil, fmt.Errorf("playwright stealth error: %s", out.Error)
				}
				code = out.Code
			}
		}
	}

	if code == "" {
		outLog := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
		if outLog != "" {
			lines := strings.Split(outLog, "\n")
			if len(lines) > 16 {
				lines = lines[len(lines)-16:]
			}
			return nil, fmt.Errorf("playwright stealth: browser closed without code | log: %s", strings.Join(lines, "\n"))
		}
		return nil, fmt.Errorf("playwright stealth: browser closed without authorization code — complete Google login then close the window")
	}

	idToken, googleRefresh, err := exchangeGoogleCode(code, verifier, redirectURI, clientID, clientSecret)
	if err != nil {
		return nil, err
	}

	sess, err := exchangeGoogleIDTokenForKimi(idToken)
	if err != nil {
		return nil, err
	}
	if g := decodeGoogleIDToken(idToken); g != nil {
		if sess.Email == "" && g.Email != "" {
			sess.Email = g.Email
		}
		if sess.Name == "" {
			if g.Name != "" {
				sess.Name = g.Name
			} else {
				sess.Name = strings.TrimSpace(g.GivenName + " " + g.FamilyName)
			}
		}
		if sess.UserID == "" && g.Sub != "" {
			sess.UserID = g.Sub
		}
	}
	_ = os.Remove(outPath)
	return &GoogleLoginSession{Session: *sess, IDToken: idToken, GoogleRefreshToken: googleRefresh}, nil
}

func exchangeGoogleCode(code, verifier, redirectURI, clientID, clientSecret string) (string, string, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	req, err := http.NewRequest(http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("google token exchange HTTP %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		return "", "", err
	}
	idToken, _ := data["id_token"].(string)
	googleRefresh, _ := data["refresh_token"].(string)
	if idToken == "" {
		return "", "", fmt.Errorf("google response missing id_token")
	}
	return idToken, googleRefresh, nil
}

// RefreshGoogleIDToken exchanges a stored Google OAuth refresh_token for a fresh id_token.
// No browser / Playwright required.
func RefreshGoogleIDToken(googleRefreshToken string) (idToken string, err error) {
	googleRefreshToken = strings.TrimSpace(googleRefreshToken)
	if googleRefreshToken == "" {
		return "", fmt.Errorf("google refresh_token required")
	}
	clientID, clientSecret, err := googleOAuthCreds()
	if err != nil {
		return "", err
	}
	form := url.Values{
		"refresh_token": {googleRefreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"grant_type":    {"refresh_token"},
	}
	req, err := http.NewRequest(http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("google refresh HTTP %d: %s", resp.StatusCode, truncate(string(b), 240))
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		return "", err
	}
	idToken, _ = data["id_token"].(string)
	if idToken == "" {
		return "", fmt.Errorf("google refresh response missing id_token")
	}
	return idToken, nil
}

// LoginWithGoogleRefresh performs full Google→Kimi auth over HTTP only (no browser).
// Requires a previously stored Google OAuth refresh_token from a browser/stealth login.
func LoginWithGoogleRefresh(googleRefreshToken string) (*GoogleLoginSession, error) {
	idToken, err := RefreshGoogleIDToken(googleRefreshToken)
	if err != nil {
		return nil, err
	}
	sess, err := exchangeGoogleIDTokenForKimi(idToken)
	if err != nil {
		return nil, err
	}
	if g := decodeGoogleIDToken(idToken); g != nil {
		if sess.Email == "" && g.Email != "" {
			sess.Email = g.Email
		}
		if sess.Name == "" {
			if g.Name != "" {
				sess.Name = g.Name
			} else {
				sess.Name = strings.TrimSpace(g.GivenName + " " + g.FamilyName)
			}
		}
		if sess.UserID == "" && g.Sub != "" {
			sess.UserID = g.Sub
		}
	}
	sess.Source = "google_http_refresh"
	// Keep the same Google refresh_token (token refresh does not rotate it).
	return &GoogleLoginSession{Session: *sess, IDToken: idToken, GoogleRefreshToken: googleRefreshToken}, nil
}

// AuthServiceLoginWithThirdPartyURL is the account-gateway Connect RPC the
// kimi.com web app uses for Google sign-in since the 2026-08-18 backend
// migration (account.gateway.v1.AuthService/LoginWithThirdParty).
const AuthServiceLoginWithThirdPartyURL = "https://auth.kimi.com/api/account.gateway.v1.AuthService/LoginWithThirdParty"

// exchangeGoogleIDTokenForKimi trades a Google id_token for Kimi tokens via the
// current Connect RPC: POST auth.kimi.com/api/account.gateway.v1.AuthService/
// LoginWithThirdParty {"credential":{"third_party":"THIRD_PARTY_GOOGLE","code":<id_token>}}.
// The legacy REST route (POST www.kimi.com/api/auth/login/google {code}) is
// kept only as a fallback for older backends — since the migration it returns
// "该邮箱已被注册过" for already-registered emails instead of logging in.
func exchangeGoogleIDTokenForKimi(idToken string) (*Session, error) {
	client := &http.Client{Timeout: 45 * time.Second}
	b, err := doGoogleLoginRequest(client, AuthServiceLoginWithThirdPartyURL,
		map[string]any{"credential": map[string]any{"third_party": "THIRD_PARTY_GOOGLE", "code": idToken}})
	if err == errLoginRouteGone {
		// Pre-migration backend: fall back to the legacy REST login.
		b, err = doGoogleLoginRequest(client, DefaultKimiURL+"/api/auth/login/google",
			map[string]any{"code": idToken})
	}
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, fmt.Errorf("kimi login json: %w body=%s", err, truncate(string(b), 200))
	}
	access, _ := data["access_token"].(string)
	if access == "" {
		access, _ = data["accessToken"].(string)
	}
	if access == "" {
		if d, ok := data["data"].(map[string]any); ok {
			access, _ = d["access_token"].(string)
		}
	}
	refresh, _ := data["refresh_token"].(string)
	if refresh == "" {
		refresh, _ = data["refreshToken"].(string)
	}
	if access == "" {
		return nil, fmt.Errorf("kimi login missing access_token: %s", truncate(string(b), 300))
	}
	p, _ := DecodeJWT(access)
	s := &Session{
		AccessToken:  access,
		RefreshToken: refresh,
		Source:       "google_browser",
		CapturedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if p != nil {
		s.UserID = p.Sub
		s.DeviceID = DeviceIDString(p.DeviceID)
		s.SSID = p.SSID
		s.Exp = p.Exp
	}
	if u, ok := data["user"].(map[string]any); ok {
		if s.UserID == "" {
			if id, ok := u["id"].(string); ok {
				s.UserID = id
			}
		}
		if s.Email == "" {
			if em, ok := u["email"].(string); ok {
				s.Email = em
			}
		}
		if s.Name == "" {
			if n, ok := u["name"].(string); ok {
				s.Name = n
			}
		}
	}
	if s.UserID == "" {
		if id, ok := data["user_id"].(string); ok {
			s.UserID = id
		}
	}
	return s, nil
}

// errLoginRouteGone marks an auth route that no longer exists (404), signalling
// the caller to retry against the legacy pre-migration endpoint.
var errLoginRouteGone = errors.New("kimi login route gone")

// doGoogleLoginRequest performs one Google id_token → Kimi token exchange
// attempt with the SPA headers kimi.com sends.
func doGoogleLoginRequest(client *http.Client, endpoint string, payload map[string]any) ([]byte, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", DefaultKimiURL)
	req.Header.Set("Referer", DefaultKimiURL+"/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36")
	req.Header.Set("x-msh-platform", "windows")
	req.Header.Set("x-msh-version", "3.2.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return nil, errLoginRouteGone
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("kimi google login HTTP %d: %s", resp.StatusCode, truncate(string(b), 300))
	}
	return b, nil
}
