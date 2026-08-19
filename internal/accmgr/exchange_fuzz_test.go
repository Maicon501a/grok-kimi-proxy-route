package accmgr

// Exchange fuzz probe. Skipped unless ACCIO_EXCHANGE_FUZZ=1. Completes one
// real signup, then issues MULTIPLE oauth codes from the same web session
// (calling login.accio.com/api/oauth/code from the page, which carries the
// session cookies) and tries a different token-exchange variant per code, to
// discover why the exchange answers invalid_code (90001).
//
//	ACCIO_EXCHANGE_FUZZ=1 go test ./internal/accmgr -run TestExchangeFuzz -v -timeout 20m

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// pkceS256 derives the S256 code challenge for a verifier (base64url, no
// padding), matching the accio client's pkceChallengePair.
func pkceS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestExchangeFuzz(t *testing.T) {
	if os.Getenv("ACCIO_EXCHANGE_FUZZ") == "" {
		t.Skip("set ACCIO_EXCHANGE_FUZZ=1 to run the exchange fuzz probe")
	}

	ctx := context.Background()
	inbox, err := NewInbox(ctx)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	t.Logf("inbox: %s", inbox.Address())

	// Fixed PKCE pair so we can exchange codes ourselves (no client listener).
	verifier := "fuzz_verifier_0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	challenge := pkceS256(verifier)
	// The 0.29.1 app embeds a login_trace_id in the redirect_uri itself; the
	// server binds the issued code to that exact redirect_uri.
	traceID := fmt.Sprintf("login_%032x", time.Now().UnixNano())
	redirectURI := "http://localhost:45998/auth/callback?login_trace_id=" + traceID
	state := "fuzzstate0123456789abcdef0123456789abcdef"
	loginURL := fmt.Sprintf("https://www.accio.com/login?client_id=accio-work&code_challenge=%s&code_challenge_method=S256&return_url=%s&state=%s",
		url.QueryEscape(challenge), url.QueryEscape(redirectURI), state)

	loginCtx, cancelLogin := context.WithTimeout(ctx, 10*time.Minute)
	defer cancelLogin()

	execOpts := []chromedp.ExecAllocatorOption{
		chromedp.UserDataDir(t.TempDir()),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.DisableGPU,
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("headless", "new"),
		chromedp.WindowSize(1280, 900),
		chromedp.Flag("lang", "pt-BR,pt;q=0.9"),
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(loginCtx, execOpts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	run := func(actions ...chromedp.Action) {
		if err := chromedp.Run(browserCtx, actions...); err != nil {
			t.Logf("[step err] %v", err)
		}
	}

	run(network.Enable(), chromedp.Navigate(loginURL))
	run(chromedp.ActionFunc(func(ctx context.Context) error {
		return waitForText(ctx, []string{"Digite o endereço", "Enter your email", "email address"})
	}))
	run(chromedp.ActionFunc(func(ctx context.Context) error {
		return setReactInput(ctx, "form input", inbox.Address())
	}))
	humanDelay()
	run(chromedp.ActionFunc(func(ctx context.Context) error { return clickContinue(ctx) }))
	run(chromedp.ActionFunc(func(ctx context.Context) error {
		return waitForText(ctx, []string{"verification code", "código", "code"})
	}))
	code, err := inbox.WaitCode(loginCtx)
	if err != nil {
		t.Fatalf("wait code: %v", err)
	}
	t.Logf("[email code] %s", code)
	run(chromedp.ActionFunc(func(ctx context.Context) error {
		humanDelay()
		return setReactInput(ctx, "form input", code)
	}))
	run(chromedp.ActionFunc(func(ctx context.Context) error {
		humanDelay()
		return clickContinue(ctx)
	}))
	// Wait for the session to be established, then navigate back onto
	// www.accio.com so page fetches to login.accio.com run from the site's
	// own origin (the redirect to the dead localhost listener breaks fetch).
	time.Sleep(6 * time.Second)
	run(chromedp.Navigate("https://www.accio.com/work/app"))
	time.Sleep(4 * time.Second)

	// Pull the session cookies for the exchange-with-cookies variant.
	var cookies []*network.Cookie
	run(chromedp.ActionFunc(func(ctx context.Context) error {
		execCtx := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Target)
		cks, err := network.GetCookies().WithURLs([]string{
			"https://login.accio.com", "https://www.accio.com", "https://phoenix-gw.alibaba.com",
		}).Do(execCtx)
		if err != nil {
			return err
		}
		cookies = cks
		return nil
	}))
	var cookieHdr strings.Builder
	for _, ck := range cookies {
		if strings.HasSuffix(ck.Domain, "accio.com") {
			cookieHdr.WriteString(ck.Name + "=" + ck.Value + "; ")
		}
	}
	t.Logf("[cookies] %d accio cookies captured", strings.Count(cookieHdr.String(), "="))

	// The web session's own token lives in phoenix_cookie
	// (accessToken=<32hex>&expiresAt=<ts>&refreshToken=<128hex>).
	webAccess, webRefresh := "", ""
	for _, ck := range cookies {
		if ck.Name == "phoenix_cookie" {
			if v, derr := url.QueryUnescape(ck.Value); derr == nil {
				ck.Value = v
			}
			for _, kv := range strings.Split(ck.Value, "&") {
				if strings.HasPrefix(kv, "accessToken=") {
					webAccess = strings.TrimPrefix(kv, "accessToken=")
				}
				if strings.HasPrefix(kv, "refreshToken=") {
					webRefresh = strings.TrimPrefix(kv, "refreshToken=")
				}
			}
		}
	}
	if webAccess != "" {
		t.Logf("[session] web accessToken=%s… refresh=%s…", webAccess[:8], webRefresh[:min(8, len(webRefresh))])
	}

	// Web-token viability: if the non-/safe/ endpoints accept the web-session
	// tokens, the whole OAuth exchange detour becomes unnecessary.
	if webAccess != "" {
		probeWebToken(t, webAccess, webRefresh)
	}

	// The code issuance is bound to the browser session's deviceId + cna —
	// scrape both so the exchange can present them.
	deviceID, cna := "", ""
	for _, ck := range cookies {
		if ck.Name == "cna" {
			cna = ck.Value
		}
	}
	run(chromedp.ActionFunc(func(ctx context.Context) error {
		js := `(() => {
			for (let i = 0; i < localStorage.length; i++) {
				const v = localStorage.getItem(localStorage.key(i)) || "";
				const m = v.match(/accio_[0-9a-f-]{36}/);
				if (m) return m[0];
			}
			return "";
		})()`
		return chromedp.Run(ctx, chromedp.Evaluate(js, &deviceID, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithReturnByValue(true)
		}))
	}))
	t.Logf("[session] deviceId=%q cna=%q", deviceID, cna)

	// issueCode calls /api/oauth/code from the page (session cookies ride
	// along) and extracts a fresh oauth code from the JSON response.
	issueCode := func() string {
		js := fmt.Sprintf(`(async () => {
			const resp = await fetch("https://login.accio.com/api/oauth/code", {
				method: "POST",
				headers: {"Content-Type": "application/json"},
				body: JSON.stringify({
					code_challenge: %q,
					code_challenge_method: "S256",
					client_id: "accio-work",
					redirect_uri: %q,
					language: "pt_PT", country: "BR", currency: "USD",
				}),
				credentials: "include",
			});
			return await resp.text();
		})()`, challenge, redirectURI)
		var out string
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(js, &out, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithReturnByValue(true).WithAwaitPromise(true)
		})); err != nil {
			t.Logf("[issueCode] %v", err)
			return ""
		}
		t.Logf("[oauth/code resp] %s", truncateStr(out, 400))
		// The response may embed the code directly or inside a redirect URL.
		if m := strings.Index(out, "code="); m >= 0 {
			rest := out[m+5:]
			end := strings.IndexAny(rest, "&\"\\")
			if end > 0 {
				return rest[:end]
			}
			return rest
		}
		var parsed map[string]any
		if json.Unmarshal([]byte(out), &parsed) == nil {
			if s := firstDeepString(parsed, "code"); s != "" {
				return s
			}
		}
		return ""
	}

	tryExchange := func(name, endpoint string, extra map[string]string, cookie string, headers map[string]string) {
		code := issueCode()
		if code == "" {
			t.Logf("[variant %s] no code issued, skipping", name)
			return
		}
		var body map[string]string
		if extra != nil && extra["__snake"] == "1" {
			delete(extra, "__snake")
			body = map[string]string{
				"code":          code,
				"code_verifier": verifier,
				"client_id":     "accio-work",
				"redirect_uri":  redirectURI,
			}
		} else {
			body = map[string]string{
				"code":         code,
				"codeVerifier": verifier,
				"clientId":     "accio-work",
				"redirectUri":  redirectURI,
			}
		}
		for k, v := range extra {
			body[k] = v
		}
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Accio/0.27.0")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		client := &http.Client{Timeout: 20 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Logf("[variant %s] transport err: %v", name, err)
			return
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		t.Logf("[variant %s] HTTP %d: %s", name, resp.StatusCode, truncateStr(string(respBody), 400))
	}

	const phoenix = "https://phoenix-gw.alibaba.com/api/oauth/token"

	// WAF signing was already ruled out (fuzz v4) — keep the daemon out of
	// this probe; web-token viability is the open question.
	binding := map[string]string{"deviceId": deviceID, "cna": cna, "language": "pt_PT", "country": "BR", "currency": "USD"}

	tryExchange("control (current body, phoenix)", phoenix, map[string]string{"utdid": "", "version": "0.25.0"}, "", nil)
	tryExchange("binding + web accessToken", phoenix, mergeMaps(binding, map[string]string{"accessToken": webAccess}), "", nil)

	// The 0.27.5 app decorates EVERY gateway request with x-* headers via a
	// request interceptor (x-source=ACCIO_DESKTOP, x-utdid, x-cna,
	// x-app-version, x-os, x-platform, x-language, x-deploy-target) and puts
	// utdid+version into every POST body. Replicate that exactly.
	appUtdid, _ := os.ReadFile(os.ExpandEnv(`${USERPROFILE}\.accio\utdid`))
	appCna, _ := os.ReadFile(os.ExpandEnv(`${USERPROFILE}\.accio\cna`))
	appHeaders := map[string]string{
		"x-utdid":         strings.TrimSpace(string(appUtdid)),
		"x-cna":           strings.TrimSpace(string(appCna)),
		"x-app-version":   "0.27.5",
		"x-os":            "win32",
		"x-platform":      "pc-win",
		"x-language":      "pt-BR",
		"x-locale":        "pt_BR",
		"x-deploy-target": "vps",
		"x-source":        "ACCIO_DESKTOP",
	}
	appBody := map[string]string{"utdid": strings.TrimSpace(string(appUtdid)), "version": "0.27.5"}
	t.Logf("[app-style] utdid=%q cna=%q", appBody["utdid"], appHeaders["x-cna"])
	tryExchange("full app headers + body", phoenix, appBody, "", appHeaders)

	// Browser-exchange variant: run the exchange from the page itself —
	// genuine Chromium network stack (TLS fingerprint), same environment the
	// site uses.
	{
		code := issueCode()
		if code == "" {
			t.Log("[browser-exchange] no code issued, skipping")
		} else {
			bodyJSON, _ := json.Marshal(mergeMaps(appBody, map[string]string{
				"code": code, "codeVerifier": verifier, "clientId": "accio-work", "redirectUri": redirectURI,
			}))
			hdrJSON, _ := json.Marshal(appHeaders)
			js := fmt.Sprintf(`(async () => {
				try {
					const resp = await fetch("https://phoenix-gw.alibaba.com/api/oauth/token", {
						method: "POST",
						headers: Object.assign({"Content-Type": "application/json", "Accept": "application/json"}, %s),
						body: %q,
						credentials: "omit",
					});
					return "HTTP " + resp.status + ": " + (await resp.text()).slice(0, 400);
				} catch (e) { return "FETCH-ERR: " + e.message; }
			})()`, string(hdrJSON), string(bodyJSON))
			var out string
			if err := chromedp.Run(browserCtx, chromedp.Evaluate(js, &out, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
				return p.WithReturnByValue(true).WithAwaitPromise(true)
			})); err != nil {
				t.Logf("[browser-exchange] eval err: %v", err)
			} else {
				t.Logf("[browser-exchange] %s", out)
			}
		}
	}

	// Propagation hypothesis: the code is issued by login.accio.com but
	// validated by phoenix-gw — cross-service replication may lag, so an
	// immediate exchange sees invalid_code. Retry with growing delays.
	for _, delay := range []time.Duration{3 * time.Second, 10 * time.Second, 30 * time.Second} {
		code := issueCode()
		if code == "" {
			t.Logf("[delay %s] no code issued, skipping", delay)
			continue
		}
		time.Sleep(delay)
		body := mergeMaps(appBody, map[string]string{
			"code": code, "codeVerifier": verifier, "clientId": "accio-work", "redirectUri": redirectURI,
		})
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, phoenix, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Accio/0.27.5")
		for k, v := range appHeaders {
			req.Header.Set(k, v)
		}
		client := &http.Client{Timeout: 20 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Logf("[delay %s] transport err: %v", delay, err)
			continue
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		t.Logf("[delay %s] HTTP %d: %s", delay, resp.StatusCode, truncateStr(string(respBody), 300))
	}
}

func mergeMaps(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// probeWebToken tests whether the web-session token pair (from phoenix_cookie)
// is accepted by the gateway endpoints the proxy needs: credits, userinfo and
// refresh — both the /safe/ variants and the 0.27.x (non-safe) variants.
func probeWebToken(t *testing.T, access, refresh string) {
	t.Helper()
	client := &http.Client{Timeout: 20 * time.Second}
	get := func(name, rawurl string) {
		req, _ := http.NewRequest(http.MethodGet, rawurl, nil)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Accio/0.27.0")
		resp, err := client.Do(req)
		if err != nil {
			t.Logf("[web-token %s] transport err: %v", name, err)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		t.Logf("[web-token %s] HTTP %d: %s", name, resp.StatusCode, truncateStr(string(body), 300))
	}
	post := func(name, rawurl string, payload map[string]string) {
		raw, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, rawurl, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Accio/0.27.0")
		resp, err := client.Do(req)
		if err != nil {
			t.Logf("[web-token %s] transport err: %v", name, err)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		t.Logf("[web-token %s] HTTP %d: %s", name, resp.StatusCode, truncateStr(string(body), 300))
	}

	const gw = "https://phoenix-gw.alibaba.com/api"
	get("credits /safe/", gw+"/entitlement/currentSubscription?accessToken="+access+"&subscripType=INDIVIDUAL")
	post("userinfo /safe/", gw+"/auth/safe/userinfo", map[string]string{"token": access})
	post("userinfo v2", gw+"/auth/userinfo", map[string]string{"token": access})
	if refresh != "" {
		post("refresh /safe/", gw+"/auth/safe/refresh_token", map[string]string{"refreshToken": refresh})
		post("refresh v2", gw+"/auth/refresh_token", map[string]string{"refreshToken": refresh})
	}
}

func firstDeepString(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok && s != "" {
		return s
	}
	for _, v := range m {
		if child, ok := v.(map[string]any); ok {
			if s := firstDeepString(child, key); s != "" {
				return s
			}
		}
	}
	return ""
}
