package accmgr

// SSO completion on the EXACT allocator/sequence of the working recon:
// fresh profile, new headless, single Run for nav+click, attach popup after
// it loaded. Then: fill the Alibaba login form (or click the one-click
// "sim"), wait for the OAuth callback, measure credits.
// Gated by ACCIO_ALI_SSOC=1. Reuses the identity persisted by the E2E run.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"grok-desktop/internal/accio"
)

func TestAlibabaSSOComplete(t *testing.T) {
	if os.Getenv("ACCIO_ALI_SSOC") == "" {
		t.Skip("set ACCIO_ALI_SSOC=1")
	}
	raw, err := os.ReadFile(filepath.Join(os.TempDir(), "accio-ali-e2e-identity.json"))
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	var id struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if json.Unmarshal(raw, &id) != nil || id.Email == "" {
		t.Fatal("bad identity file")
	}
	t.Logf("identity: %s", id.Email)

	dataDir := filepath.Join(os.TempDir(), "accio-live-probe-data")
	acc, err := accio.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	loginURL, err := acc.StartLogin(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	loginCh := waitLogin(acc, id.Email)
	loginErrCh := acc.LoginErrors()

	// Headful by default: the Baxia slider only accepts the REAL cursor
	// (Win32 SendInput), which requires a visible on-screen window — in
	// headless the hardware events never reach the page. ACCIO_ALI_SSOC_HEADLESS=1
	// forces headless for pure recon runs without a slider.
	headless := any(false)
	if os.Getenv("ACCIO_ALI_SSOC_HEADLESS") != "" {
		headless = "new"
	}
	execOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", headless),
		chromedp.Flag("no-proxy-server", true),
		chromedp.WindowSize(1440, 900),
	)
	// PERSISTENT profile by default: it holds cookies from an ACCEPTED Baxia
	// verification and likely a live Alibaba session from the registration —
	// the SSO popup then skips the login form AND the slider entirely
	// ("Continuar como <email>?" one-click). Fresh fingerprints are exactly
	// what Baxia hardened against. ACCIO_ALI_SSOC_FRESH=1 opts out.
	if os.Getenv("ACCIO_ALI_SSOC_FRESH") == "" {
		execOpts = append(execOpts,
			chromedp.UserDataDir(filepath.Join(os.TempDir(), "accio-ali-browser-profile")))
	}
	// Baxia hardened server-side against THIS machine's hardware fingerprint
	// (WebGL/canvas/renderer hash) — trajectory changes do not matter once the
	// device is marked. SwiftShader swaps the WebGL renderer string
	// ("ANGLE (...)" → "WebKit WebGL"/SwiftShader), changing the fingerprint
	// hash without new hardware. ACCIO_ALI_SSOC_SWIFTSHADER=1 to enable.
	if os.Getenv("ACCIO_ALI_SSOC_SWIFTSHADER") != "" {
		execOpts = append(execOpts,
			chromedp.Flag("use-angle", "swiftshader"),
			chromedp.Flag("enable-unsafe-swiftshader", true),
		)
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, execOpts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	// One Run like the recon: navigate, wait, fire the React handler.
	err = chromedp.Run(browserCtx,
		chromedp.Navigate(loginURL),
		chromedp.ActionFunc(func(ctx context.Context) error {
			return waitForText(ctx, []string{"Continue com Alibaba.com", "Continue with Alibaba.com"})
		}),
	)
	if err != nil {
		// Dump what the login page actually rendered before dying.
		var btns string
		_ = chromedp.Run(browserCtx, chromedp.Evaluate(`(() => {
			return Array.from(document.querySelectorAll("div, button, a, span"))
				.map(e => (e.textContent || "").trim())
				.filter(s => s.length > 3 && s.length < 60)
				.slice(0, 40).join(" | ");
		})()`, &btns, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
		t.Logf("login page texts: %s", btns)
		t.Fatalf("open: %v", err)
	}
	fireSSO := func(ctx context.Context) string {
		var res string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
			const els2 = Array.from(document.querySelectorAll("div, button, a, span"));
			const el2 = els2.find(e => { const s = (e.textContent || "").trim(); return s === "Continue com Alibaba.com" || s === "Continue with Alibaba.com"; });
			if (!el2) return "button not found";
			let n2 = el2;
			for (let d = 0; n2 && d < 6; d++, n2 = n2.parentElement) {
				const k2 = Object.keys(n2).find(k => k.startsWith("__reactProps"));
				if (k2 && typeof n2[k2].onClick === "function") {
					n2[k2].onClick({ preventDefault() {}, stopPropagation() {}, currentTarget: n2, target: n2 });
					return "fired";
				}
			}
			return "no react handler";
		})()`, &res, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
		return res
	}
	t.Logf("sso click: %s", fireSSO(browserCtx))
	// Attach to the popup AFTER it loaded (recon timing) — but poll: with a
	// LIVE Alibaba session (persistent profile) the popup can race through
	// login.alibaba.com → login.accio.com → localhost callback in seconds,
	// or never match the original URL filter. Watch the login channel while
	// hunting so a fast auto-consent is not missed.
	time.Sleep(4 * time.Second)
	var aliID target.ID
	huntDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(huntDeadline) && aliID == "" {
		select {
		case rec := <-loginCh:
			raw2, _ := json.Marshal(rec)
			t.Logf("ACCIO LOGIN COMPLETED (fast auto-consent): %s", raw2)
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
		default:
		}
		targets, terr := chromedp.Targets(browserCtx)
		if terr != nil {
			t.Fatal(terr)
		}
		for _, tg := range targets {
			if tg.Type == "page" && (strings.Contains(tg.URL, "login.alibaba.com") ||
				strings.Contains(tg.URL, "login.accio.com") ||
				strings.HasPrefix(tg.URL, "http://localhost")) {
				aliID = tg.TargetID
				break
			}
		}
		if aliID == "" {
			// The first fire can race the page's hydration or get popup-
			// blocked; re-fire every poll while no popup shows.
			t.Logf("sso re-click: %s", fireSSO(browserCtx))
			time.Sleep(3 * time.Second)
		}
	}
	if aliID == "" {
		targets, _ := chromedp.Targets(browserCtx)
		for _, tg := range targets {
			t.Logf("TARGET %s %s %.100s", tg.Type, tg.TargetID, tg.URL)
		}
		t.Fatal("no alibaba popup found")
	}
	aliCtx, cancelAli := chromedp.NewContext(browserCtx, chromedp.WithTargetID(aliID))
	defer cancelAli()
	time.Sleep(5 * time.Second)

	// Same dump as the recon — must answer if the renderer is healthy.
	var aliLoc string
	var aliEls []map[string]any
	if err := chromedp.Run(aliCtx,
		chromedp.Location(&aliLoc),
		chromedp.Evaluate(`(() => {
			return Array.from(document.querySelectorAll("input, button, a, [role=button]")).map(e => ({
				tag: e.tagName, type: e.type || "", name: e.name || "", id: e.id || "",
				placeholder: e.placeholder || "", text: (e.textContent||"").trim().slice(0,50),
				href: e.href || ""
			})).filter(e => e.text || e.placeholder || e.tag === "INPUT");
		})()`, &aliEls, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }),
	); err != nil {
		t.Fatalf("ali dump: %v", err)
	}
	t.Logf("ALI location: %s", aliLoc)
	for _, e := range aliEls {
		t.Logf("ALI EL: %v", e)
	}

	// Drive the popup: fill form if present, click primary buttons, watch
	// for the callback.
	deadline := time.Now().Add(3 * time.Minute)
	filled := false
	tick := 0
	sliderTries := 0
	for time.Now().Before(deadline) {
		select {
		case rec := <-loginCh:
			raw2, _ := json.Marshal(rec)
			t.Logf("ACCIO LOGIN COMPLETED: %s", raw2)
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
		alive := false
		if ts, terr := chromedp.Targets(browserCtx); terr == nil {
			for _, tg := range ts {
				if tg.TargetID == aliID {
					alive = true
					break
				}
			}
		}
		if !alive {
			t.Log("popup closed — waiting for callback")
			continue
		}
		c1, k1 := context.WithTimeout(aliCtx, 5*time.Second)
		var loc string
		_ = chromedp.Run(c1, chromedp.Location(&loc))
		k1()
		if strings.HasPrefix(loc, "http://localhost") || strings.Contains(loc, "login.accio.com/newlogin/alibaba_sign") {
			t.Logf("redirecting: %.120s", loc)
			continue
		}
		if !filled {
			c2, k2 := context.WithTimeout(aliCtx, 25*time.Second)
			if aliCount(c2, `input[name="account"]`) > 0 {
				if err := aliFill(c2, `input[name="account"]`, id.Email); err == nil {
					_ = aliFill(c2, `input[name="password"]`, id.Password)
					time.Sleep(time.Second)
					aliClickText(t, c2, "continuar", "continue", "sign in", "entrar")
					filled = true
					t.Log("login form submitted")
				}
			}
			k2()
		}
		c3, k3 := context.WithTimeout(aliCtx, 6*time.Second)
		if aliClickText(t, c3, "sim", "yes", "authorize", "autorizar", "allow", "permitir") {
			t.Log("consent clicked")
		}
		k3()
		// Baxia slider can gate the login submit too.
		if sliderTries < 3 {
			c4, k4 := context.WithTimeout(aliCtx, 25*time.Second)
			if solveBaxiaSlider(t, c4) {
				sliderTries++
				t.Logf("slider attempt %d done", sliderTries)
			}
			k4()
		}
		// Periodic visibility into what the popup shows after submit.
		tick++
		if tick%8 == 1 {
			c5, k5 := context.WithTimeout(aliCtx, 6*time.Second)
			var dom string
			_ = chromedp.Run(c5, chromedp.Evaluate(`(() => {
				const els = Array.from(document.querySelectorAll("input, button, a, [class*=error], [role=alert], iframe")).map(e => e.tagName + ":" + (e.type||e.name||e.id||e.className&&e.className.slice?e.className.slice(0,30):""||"") + ":" + (e.textContent||"").trim().slice(0,40));
				return els.join(" ;; ").slice(0, 400);
			})()`, &dom, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
			var ps []byte
			if err := chromedp.Run(c5, chromedp.CaptureScreenshot(&ps)); err == nil {
				_ = os.WriteFile(filepath.Join(os.TempDir(), fmt.Sprintf("accio-ali-ssoc-%02d.png", tick)), ps, 0o644)
			}
			k5()
			t.Logf("POPUP STATE[%d] loc=%.80s dom=%s", tick, loc, dom)
		}
	}
	t.Fatal("SSO did not complete")
}
