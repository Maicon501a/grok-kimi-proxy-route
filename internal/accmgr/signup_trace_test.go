package accmgr

// Network-traced signup probe. Skipped unless ACCIO_TRACE_SIGNUP=1. Drives
// one real signup while capturing every API call the site makes (URLs +
// response bodies for auth-ish endpoints) plus the exact code delivered to
// the PKCE callback, so we can see why the token exchange answers
// invalid_code and whether the site offers a web→OAuth conversion endpoint
// (token.compensate, authorize, deep link).
//
//	ACCIO_TRACE_SIGNUP=1 go test ./internal/accmgr -run TestTraceSignup -v -timeout 20m

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"grok-desktop/internal/accio"
)

func traceInteresting(u string) bool {
	l := strings.ToLower(u)
	// Skip analytics/tracking noise.
	for _, skip := range []string{"aplusaccio", "fourier.", "google.", "bing.", "clarity", "googletagmanager", ".gif"} {
		if strings.Contains(l, skip) {
			return false
		}
	}
	needles := []string{
		"oauth", "token", "compensate", "authorize", "hasLogin", "sendCode",
		"captcha", "signin", "login.accio", "mtop", "auth_states", "entitlement",
	}
	for _, n := range needles {
		if strings.Contains(l, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

func TestTraceSignup(t *testing.T) {
	if os.Getenv("ACCIO_TRACE_SIGNUP") == "" {
		t.Skip("set ACCIO_TRACE_SIGNUP=1 to run the traced signup")
	}

	ctx := context.Background()
	inbox, err := NewInbox(ctx)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	t.Logf("inbox: %s", inbox.Address())

	acc, err := accio.New(t.TempDir())
	if err != nil {
		t.Fatalf("accio.New: %v", err)
	}
	loginCtx, cancelLogin := context.WithTimeout(ctx, 10*time.Minute)
	defer cancelLogin()
	loginURL, err := acc.StartLogin(loginCtx, 0)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	t.Logf("loginURL: %s", loginURL)

	profile := t.TempDir()
	execOpts := []chromedp.ExecAllocatorOption{
		chromedp.UserDataDir(profile),
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

	var mu sync.Mutex
	dump := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		line := fmt.Sprintf(format, args...)
		if len(line) > 2000 {
			line = line[:2000] + "…"
		}
		t.Log(line)
	}

	// Request URLs are remembered so the body can be fetched when the load
	// finishes (GetResponseBody right after ResponseReceived races the body
	// and comes back empty).
	pending := map[network.RequestID]string{}

	chromedp.ListenTarget(browserCtx, func(ev any) {
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			u := e.Request.URL
			if !traceInteresting(u) {
				return
			}
			if e.Request.HasPostData {
				go func(reqID network.RequestID, url, method string) {
					execCtx := cdp.WithExecutor(browserCtx, chromedp.FromContext(browserCtx).Target)
					raw, perr := network.GetRequestPostData(reqID).Do(execCtx)
					if perr != nil {
						return
					}
					pd := string(raw)
					if len(pd) > 600 {
						pd = pd[:600] + "…"
					}
					dump("[req] %s %s\n  post: %s", method, url, pd)
				}(e.RequestID, u, e.Request.Method)
			}
		case *page.EventFrameRequestedNavigation:
			if strings.Contains(e.URL, "/auth/callback") || !strings.HasPrefix(e.URL, "http") {
				dump("[reqnav] %s", e.URL)
				if u, perr := url.Parse(e.URL); perr == nil && u.Query().Get("code") != "" {
					dump("[callback-code] %s", u.Query().Get("code"))
				}
			}
		case *page.EventWindowOpen:
			dump("[window-open] %s", e.URL)
		case *network.EventResponseReceived:
			u := e.Response.URL
			if !traceInteresting(u) {
				return
			}
			mu.Lock()
			pending[e.RequestID] = u
			mu.Unlock()
		case *network.EventLoadingFinished:
			mu.Lock()
			u, ok := pending[e.RequestID]
			delete(pending, e.RequestID)
			mu.Unlock()
			if !ok {
				return
			}
			go func(reqID network.RequestID, url string) {
				execCtx := cdp.WithExecutor(browserCtx, chromedp.FromContext(browserCtx).Target)
				body, berr := network.GetResponseBody(reqID).Do(execCtx)
				if berr != nil {
					dump("[api] %s\n  body err: %v", url, berr)
					return
				}
				if len(body) > 1500 {
					body = body[:1500]
				}
				dump("[api] %s\n  body: %s", url, string(body))
			}(e.RequestID, u)
		}
	})

	if err := chromedp.Run(browserCtx,
		network.Enable(),
		chromedp.Navigate(loginURL),
		chromedp.ActionFunc(func(ctx context.Context) error {
			return waitForText(ctx, []string{
				"Digite o endereço", "Enter your email", "endereço de e-mail", "email address",
				"conta existente", "existing account", "Você se inscreveu", "You signed up",
			})
		}),
	); err != nil {
		t.Fatalf("initial page: %v", err)
	}

	run := func(actions ...chromedp.Action) {
		if err := chromedp.Run(browserCtx, actions...); err != nil {
			t.Logf("[step err] %v", err)
		}
	}

	run(chromedp.ActionFunc(func(ctx context.Context) error {
		return setReactInput(ctx, "form input", inbox.Address())
	}))
	humanDelay()
	run(chromedp.ActionFunc(func(ctx context.Context) error { return clickContinue(ctx) }))
	run(chromedp.ActionFunc(func(ctx context.Context) error {
		var text string
		_ = chromedp.Run(ctx, chromedp.Text("body", &text, chromedp.ByQuery))
		dump("[page after email] %s", truncatePage(text))
		return waitForText(ctx, []string{"verification code", "código enviado", "Insira o código", "Enter the code", "código de verificação"})
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

	// Watch the aftermath for 90s: token exchange result, deep links,
	// conversion endpoints.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var text string
		_ = chromedp.Run(browserCtx, chromedp.Text("body", &text, chromedp.ByQuery))
		dump("[page ticker] %s", truncatePage(text))
		select {
		case <-loginCtx.Done():
			t.Fatal("login ctx done")
		case <-time.After(15 * time.Second):
		}
	}
}
