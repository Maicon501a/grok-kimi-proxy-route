package accmgr

// End-to-end: create a NEW Alibaba.com buyer account (unified email-first
// flow), then log into Accio via the "Continue with Alibaba.com" SSO popup
// and measure whether the resulting Accio account is born ACTIVATED with
// credits — i.e. whether the Alibaba channel bypasses the Accio-side risk
// engine that gates our direct signups with NOT_LOGIN.
// Gated by ACCIO_ALI_E2E=1. NEVER touches the real device profile.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"grok-desktop/internal/accio"
)

func TestAlibabaToAccioE2E(t *testing.T) {
	if os.Getenv("ACCIO_ALI_E2E") == "" {
		t.Skip("set ACCIO_ALI_E2E=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Match the EXACT allocator shape of the working recon: fresh temp
	// profile (no UserDataDir — the persistent one wedges the SSO popup's
	// renderer), new headless, same window size. Trade-off: without a
	// persistent profile the SSO shows the full login form (email+password)
	// instead of the one-click "continuar como" — we fill it.
	headless := any(false)
	if os.Getenv("ACCIO_ALI_HEADFUL") == "" {
		headless = "new" // old boolean headless wedges popup renderers
	}
	execOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", headless),
		chromedp.Flag("no-proxy-server", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.WindowSize(1440, 900),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, execOpts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()
	// NOTE: no stealth script here — a window.chrome stub or webdriver
	// override on every new document wedges the login.alibaba.com renderer
	// (its bootstrap spins, CDP evaluates then time out). The registration
	// flow needed stealth for the Baxia widget, but the SSO popup does not.
	_ = page.AddScriptToEvaluateOnNewDocument

	email := ""
	password := ""
	if os.Getenv("ACCIO_ALI_REUSE") != "" {
		// Reuse the identity persisted by an earlier run — skips the whole
		// Baxia-gated registration and goes straight to the SSO phase.
		raw, rerr := os.ReadFile(filepath.Join(os.TempDir(), "accio-ali-e2e-identity.json"))
		if rerr != nil {
			t.Fatalf("reuse: %v", rerr)
		}
		var id struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if json.Unmarshal(raw, &id) != nil || id.Email == "" {
			t.Fatal("reuse: bad identity file")
		}
		email, password = id.Email, id.Password
		t.Logf("E2E identity (reused): %s", email)
	} else {
		inbox, err := NewInbox(ctx)
		if err != nil {
			t.Fatalf("inbox: %v", err)
		}
		email = inbox.Address()
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			t.Fatal(err)
		}
		password = fmt.Sprintf("Zq%x9aB", b[:])
		t.Logf("E2E identity: %s", email)

		// Phase 1: Alibaba buyer registration.
		if !alibabaRegister(t, browserCtx, inbox, email, password) {
			t.Fatal("alibaba registration failed")
		}
		t.Log("ALIBABA ACCOUNT CREATED")
		// Give the brand-new account a moment to propagate through Alibaba's
		// login backends before the SSO tries to authenticate with it.
		time.Sleep(45 * time.Second)
	}
	// Persist the Alibaba identity so a later run can retry just the SSO.
	if os.Getenv("ACCIO_ALI_REUSE") == "" {
		_ = os.WriteFile(filepath.Join(os.TempDir(), "accio-ali-e2e-identity.json"),
			[]byte(fmt.Sprintf(`{"email":%q,"password":%q}`, email, password)), 0o600)
	}

	// Phase 2: Accio login via the Alibaba SSO popup.
	dataDir := filepath.Join(os.TempDir(), "accio-live-probe-data")
	acc, err := accio.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	loginURL, err := acc.StartLogin(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	loginCh := waitLogin(acc, email)
	chromedp.ListenTarget(browserCtx, func(ev any) {
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			u := e.Request.URL
			if strings.Contains(u, "localhost") || strings.Contains(u, "alibaba_sign") || strings.Contains(u, "oauth") {
				t.Logf("NET-NAV %s", u[:min(len(u), 180)])
			}
		}
	})

	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(loginURL),
		chromedp.ActionFunc(func(ctx context.Context) error {
			return waitForText(ctx, []string{"Alibaba", "alibaba"})
		}),
	); err != nil {
		t.Fatalf("open accio login: %v", err)
	}
	// Fire the real React handler (opens the Alibaba SSO popup).
	_ = chromedp.Run(browserCtx, chromedp.Evaluate(`(() => {
		const els = Array.from(document.querySelectorAll("div, button, a, span"));
		const el = els.find(e => /alibaba\.com/i.test((e.textContent||"").trim()) && (e.textContent||"").trim().length < 40);
		if (!el) return "NOT_FOUND";
		let n = el;
		for (let d = 0; n && d < 6; d++, n = n.parentElement) {
			const k = Object.keys(n).find(k => k.startsWith("__reactProps"));
			if (k && typeof n[k].onClick === "function") {
				n[k].onClick({ preventDefault() {}, stopPropagation() {}, currentTarget: n, target: n });
				return "CLICKED";
			}
		}
		return "NO_HANDLER";
	})()`, new(string), func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))

	// Find the SSO popup.
	var aliID target.ID
	for i := 0; i < 30 && aliID == ""; i++ {
		time.Sleep(500 * time.Millisecond)
		targets, _ := chromedp.Targets(browserCtx)
		for _, tg := range targets {
			if tg.Type == "page" && strings.Contains(tg.URL, "login.alibaba.com") {
				aliID = tg.TargetID
				break
			}
		}
	}
	if aliID == "" {
		t.Fatal("SSO popup never appeared")
	}
	aliCtx, cancelAli := chromedp.NewContext(browserCtx, chromedp.WithTargetID(aliID))
	defer cancelAli()

	// The fresh Alibaba session may already be authenticated in this
	// browser (cookies from registration) — the popup might skip straight
	// to consent/redirect. Otherwise fill the login form.
	time.Sleep(5 * time.Second)
	filled := false
	sliderTries := 0
	consentDone := false
	tick := 0
	loginErrCh := acc.LoginErrors()
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case rec := <-loginCh:
			raw, _ := json.Marshal(rec)
			t.Logf("ACCIO LOGIN COMPLETED: %s", raw)
			for wait := 0; wait < 12; wait++ {
				creds, cerr := acc.CreditsFor(ctx, rec)
				t.Logf("CREDITS: %v err=%v", creds, cerr)
				if cerr == nil && creds != nil {
					return
				}
				time.Sleep(10 * time.Second)
			}
			return
		case lerr := <-loginErrCh:
			t.Logf("LOGIN EXCHANGE ERROR: %v", lerr)
		case <-time.After(3 * time.Second):
		}
		if consentDone {
			continue // just wait for the callback now
		}
		// Is the popup still alive? After consent it navigates to the local
		// callback and closes — poking a dead target can hang.
		alive := false
		targets, _ := chromedp.Targets(browserCtx)
		for _, tg := range targets {
			if tg.TargetID == aliID {
				alive = true
				break
			}
		}
		if !alive {
			t.Log("SSO popup closed — waiting for callback")
			consentDone = true
			continue
		}
		var loc string
		locCtx, locCancel := context.WithTimeout(aliCtx, 5*time.Second)
		locErr := chromedp.Run(locCtx, chromedp.Location(&loc))
		locCancel()
		tick++
		if tick%10 == 1 {
			var dom string
			dCtx, dCancel := context.WithTimeout(aliCtx, 5*time.Second)
			domErr := chromedp.Run(dCtx, chromedp.Evaluate(`(() => {
				const ins = Array.from(document.querySelectorAll("input, button, a")).filter(e => e.getBoundingClientRect().width > 0).map(e => e.tagName + ":" + (e.type||"") + ":" + (e.name||"") + ":" + (e.textContent||"").trim().slice(0,30));
				return "WIN:" + window.innerWidth + "x" + window.innerHeight + "@" + window.screenX + "," + window.screenY + " ;; " + ins.join(" ;; ");
			})()`, &dom, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
			if domErr != nil {
				t.Logf("SSO LOOP domErr: %v", domErr)
			}
			dCancel()
			t.Logf("SSO LOOP loc=%.90s locErr=%v dom=%q", loc, locErr, dom)
			var ps []byte
			sCtx, sCancel := context.WithTimeout(aliCtx, 5*time.Second)
			if err := chromedp.Run(sCtx, chromedp.CaptureScreenshot(&ps)); err == nil {
				_ = os.WriteFile(filepath.Join(os.TempDir(), fmt.Sprintf("accio-ali-sso-loop-%02d.png", tick)), ps, 0o644)
			}
			sCancel()
		}
		if strings.HasPrefix(loc, "http://localhost") || strings.HasPrefix(loc, "https://localhost") || strings.Contains(loc, "login.accio.com/newlogin/alibaba_sign") {
			t.Logf("SSO redirecting via %s", loc[:min(len(loc), 140)])
			consentDone = true
			continue
		}
		// Every action against the popup gets a hard timeout — once consent
		// is given the target closes and a bare Run would hang forever.
		act := func(d time.Duration) (context.Context, context.CancelFunc) {
			return context.WithTimeout(aliCtx, d)
		}
		if strings.Contains(loc, "login.alibaba.com") && !filled {
			c1, k1 := act(5 * time.Second)
			hasForm := aliCount(c1, `input[name="account"]`) > 0
			k1()
			if hasForm {
				c2, k2 := act(20 * time.Second)
				if err := aliFill(c2, `input[name="account"]`, email); err == nil {
					_ = aliFill(c2, `input[name="password"]`, password)
					time.Sleep(time.Second)
					aliClickText(t, c2, "continuar", "continue", "sign in", "entrar")
					filled = true
					t.Log("SSO login form submitted")
				}
				k2()
			}
		}
		c3, k3 := act(25 * time.Second)
		if sliderTries < 4 && solveBaxiaSlider(t, c3) {
			sliderTries++
			t.Logf("SSO slider solved (%d)", sliderTries)
		}
		k3()
		c4, k4 := act(6 * time.Second)
		if aliClickText(t, c4, "sim", "yes", "authorize", "autorizar", "allow", "permitir", "agree", "concordo", "accept", "aceitar") {
			t.Log("consent clicked")
			consentDone = true
		}
		k4()
	}
	t.Fatal("SSO did not complete within watch window")
}
