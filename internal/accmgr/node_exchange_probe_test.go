package accmgr

// Node-exchange probe. Gated by ACCIO_NODE_EXCHANGE=1.
//
// Hypothesis under test: the OAuth code is fine, but the SERVER gates token
// entitlement on WHO performs the exchange. Evidence: a code issued to our
// automated headless Chrome became a fully-entitled token (520 credits in 2s)
// when the OFFICIAL APP (Node TLS stack) exchanged it, while our Go client's
// byte-identical exchange yields tokens that are NOT_LOGIN at the entitlement
// API. This probe captures a real code with our own PKCE listener, then
// exchanges it via a Node child process (same TLS fingerprint as the app)
// and immediately reads the entitlement.
//
//	ACCIO_NODE_EXCHANGE=1 go test ./internal/accmgr -run TestNodeExchange -v -timeout 15m

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"grok-desktop/internal/accio"
	"grok-desktop/internal/logging"
)

// probeDeviceID returns a stable throwaway utdid for probes — never the real
// device's id (signup abuse must not be associated with it).
func probeDeviceID() string {
	p := filepath.Join(os.TempDir(), "accio-probe-utdid")
	if b, err := os.ReadFile(p); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			return v
		}
	}
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "probeUtdidFallback000000"
	}
	v := base64.StdEncoding.EncodeToString(b)
	_ = os.WriteFile(p, []byte(v), 0o600)
	return v
}

const nodeProbeScript = `
const https = require('https');
const [code, verifier, redirectUri, utdid] = process.argv.slice(2);
const body = JSON.stringify({
  utdid,
  version: "0.29.1",
  code, codeVerifier: verifier,
  clientId: "accio-work",
  redirectUri,
});
const req = https.request("https://phoenix-gw.alibaba.com/api/oauth/token", {
  method: "POST",
  headers: {
    "Content-Type": "application/json",
    "x-language": "pt",
    "x-locale": "pt-BR",
    "x-platform": "pc-win",
    "x-utdid": utdid,
    "x-app-version": "0.29.1",
    "x-os": "win32",
    "x-deploy-target": "desktop",
    "x-source": "ACCIO_DESKTOP",
    "x-cna": "",
    "x-package-region": "GLOBAL",
    "Content-Length": Buffer.byteLength(body),
  },
}, res => {
  let d = "";
  res.on("data", c => d += c);
  res.on("end", () => { console.log("HTTP", res.statusCode); console.log(d); });
});
req.on("error", e => { console.error("ERR", e.message); process.exit(1); });
req.end(body);
`

func TestNodeExchange(t *testing.T) {
	if os.Getenv("ACCIO_NODE_EXCHANGE") == "" {
		t.Skip("set ACCIO_NODE_EXCHANGE=1 to run")
	}
	ctx := context.Background()
	utdid := probeDeviceID()

	scriptPath := filepath.Join(os.TempDir(), "accio-node-exchange.cjs")
	if err := os.WriteFile(scriptPath, []byte(nodeProbeScript), 0o600); err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= 5; attempt++ {
		if attempt > 1 {
			time.Sleep(20 * time.Second)
		}
		code, verifier, redirectURI, err := captureOAuthCode(ctx, t)
		if err != nil {
			t.Logf("attempt %d capture: %v", attempt, err)
			continue
		}
		t.Logf("code captured: %s…", code[:8])

		// Exchange via Node (official-app TLS stack).
		out, err := exec.CommandContext(ctx, "node", scriptPath, code, verifier, redirectURI, utdid).CombinedOutput()
		t.Logf("node exchange: %v\n%s", err, out)
		var root struct {
			Success bool `json:"success"`
			Data    struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
			} `json:"data"`
		}
		for _, l := range strings.Split(string(out), "\n") {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "{") {
				_ = json.Unmarshal([]byte(l), &root)
			}
		}
		if root.Data.AccessToken == "" {
			continue
		}
		// Entitlement check with the Node-exchanged token (bare shape — the
		// app's own entitlement call carried no custom headers per sniff).
		u := fmt.Sprintf("https://phoenix-gw.alibaba.com/api/entitlement/currentSubscription?accessToken=%s&subscripType=INDIVIDUAL&utdid=%s&version=0.29.1", root.Data.AccessToken, utdid)
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("x-language", "pt")
		req.Header.Set("x-source", "ACCIO_DESKTOP")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("entitlement: %v", err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1200))
		resp.Body.Close()
		t.Logf("ENTITLEMENT: HTTP %d body=%s", resp.StatusCode, string(raw))
		return
	}
	t.Fatal("no working code captured")
}

// captureOAuthCode runs one browser signup pass against a bare local PKCE
// listener and returns the issued code WITHOUT exchanging it.
func captureOAuthCode(ctx context.Context, t *testing.T) (code, verifier, redirectURI string, err error) {
	inbox, err := NewInbox(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("inbox: %w", err)
	}
	logging.Info("probe.tempmail", "address", inbox.Address())

	// PKCE pair, same shape as the client.
	vb := make([]byte, 48)
	if _, err := rand.Read(vb); err != nil {
		return "", "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(vb)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return "", "", "", err
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	tb := make([]byte, 16)
	_, _ = rand.Read(tb)
	redirectURI = fmt.Sprintf("http://localhost:%d/auth/callback?login_trace_id=login_%x", port, tb)
	sb := make([]byte, 32)
	_, _ = rand.Read(sb)
	state := hex.EncodeToString(sb)

	codeCh := make(chan string, 1)
	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux}
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		c := q.Get("code")
		if c != "" {
			select {
			case codeCh <- c:
			default:
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><body><h2>OK</h2></body></html>")
	})
	go srv.Serve(listener)
	defer srv.Shutdown(context.Background())

	loginURL := fmt.Sprintf("%s/login?return_url=%s&state=%s&code_challenge=%s&code_challenge_method=S256&client_id=%s",
		accio.LoginBase, urlQueryEscape(redirectURI), state, challenge, accio.OAuthClientID)

	passCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	execOpts := []chromedp.ExecAllocatorOption{
		chromedp.UserDataDir(signupProfileDir()),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.DisableGPU,
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("disable-features", "FedCm"),
		chromedp.Flag("lang", pickLang()),
	}
	execOpts = append(execOpts, signupBrowserOpts()...)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(passCtx, execOpts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	runErr := chromedp.Run(browserCtx,
		chromedp.Navigate(loginURL),
		chromedp.ActionFunc(func(ctx context.Context) error {
			return waitForText(ctx, []string{
				"Digite o endereço", "Enter your email", "endereço de e-mail", "email address",
				"conta existente", "existing account", "Você se inscreveu", "You signed up",
			})
		}),
		chromedp.ActionFunc(func(ctx context.Context) error { return ensureEmailForm(ctx) }),
	)
	if runErr == nil {
		runErr = chromedp.Run(browserCtx,
			chromedp.ActionFunc(func(ctx context.Context) error {
				return setReactInput(ctx, "form input", inbox.Address())
			}),
			chromedp.ActionFunc(func(ctx context.Context) error {
				humanDelay()
				inbox.Reset(ctx)
				return clickContinue(ctx)
			}),
			chromedp.ActionFunc(func(ctx context.Context) error {
				if err := clickSendCodeIfPresent(ctx); err != nil {
					return err
				}
				return waitForText(ctx, []string{"verification code", "código enviado", "Insira o código", "Enter the code"})
			}),
			chromedp.ActionFunc(func(ctx context.Context) error {
				code, err := inbox.WaitCode(ctx)
				if err != nil {
					return err
				}
				humanDelay()
				return setReactInput(ctx, "form input", code)
			}),
			chromedp.ActionFunc(func(ctx context.Context) error {
				humanDelay()
				return clickContinue(ctx)
			}),
		)
	}
	select {
	case c := <-codeCh:
		return c, verifier, redirectURI, nil
	case <-time.After(60 * time.Second):
		if runErr != nil {
			return "", "", "", runErr
		}
		return "", "", "", fmt.Errorf("code did not arrive")
	}
}

func urlQueryEscape(s string) string {
	r := strings.NewReplacer(
		"%", "%25", ":", "%3A", "/", "%2F", "?", "%3F", "=", "%3D", "&", "%26",
	)
	return r.Replace(s)
}
