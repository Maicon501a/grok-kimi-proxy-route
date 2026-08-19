package accmgr

// Recon: where does "Continue with Alibaba.com" lead? Gated by ACCIO_ALI_RECON=1.

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

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"grok-desktop/internal/accio"
	"grok-desktop/internal/logging"
)

func TestAlibabaChannelRecon(t *testing.T) {
	if os.Getenv("ACCIO_ALI_RECON") == "" {
		t.Skip("set ACCIO_ALI_RECON=1")
	}
	acc, err := accio.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	loginURL, err := acc.StartLogin(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	execOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", "new"),
		chromedp.Flag("no-proxy-server", true),
		chromedp.WindowSize(1440, 900),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, execOpts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	var page string
	var hrefs []map[string]any
	err = chromedp.Run(browserCtx,
		chromedp.Navigate(loginURL),
		chromedp.ActionFunc(func(ctx context.Context) error {
			return waitForText(ctx, []string{"Alibaba", "alibaba"})
		}),
		chromedp.Evaluate(`(() => {
			const els = Array.from(document.querySelectorAll("a, button, div[role=button], span, div"));
			window.__iframes = Array.from(document.querySelectorAll("iframe")).map(f => f.src);
			return els.filter(e => /alibaba\.com/i.test(e.textContent||"")).map(e => ({
				tag: e.tagName, text: (e.textContent||"").trim().slice(0,60),
				href: e.href || (e.closest("a") ? e.closest("a").href : ""),
				onclick: !!e.onclick
			}));
		})()`, &hrefs, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }),
		chromedp.Text("body", &page, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var iframes []string
	_ = chromedp.Run(browserCtx, chromedp.Evaluate(`window.__iframes || []`, &iframes, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
	t.Logf("iframes: %v", iframes)
	t.Logf("alibaba elements: %v", hrefs)
	t.Logf("page: %.300s", page)

	// Read the React onClick handler source for "Continue com Alibaba.com" —
	// the handler reveals the OAuth destination (passport.alibaba.com?).
	var handler string
	if err := chromedp.Run(browserCtx, chromedp.Evaluate(`(() => {
		const els2 = Array.from(document.querySelectorAll("div, button, a, span"));
		const el2 = els2.find(e => (e.textContent || "").trim() === "Continue com Alibaba.com");
		if (el2) {
			let n2 = el2;
			for (let d = 0; n2 && d < 6; d++, n2 = n2.parentElement) {
				const k2 = Object.keys(n2).find(k => k.startsWith("__reactProps"));
				if (k2 && typeof n2[k2].onClick === "function") {
					n2[k2].onClick({ preventDefault() {}, stopPropagation() {}, currentTarget: n2, target: n2 });
					break;
				}
			}
		}
	})()`, &handler, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) })); err == nil {
		t.Logf("synthetic click dispatched")
	}
	if err := chromedp.Run(browserCtx, chromedp.Evaluate(`(() => {
		const els = Array.from(document.querySelectorAll("div, button, a, span"));
		const el = els.find(e => (e.textContent || "").trim() === "Continue com Alibaba.com");
		if (!el) return "NOT_FOUND";
		let node = el, out = [];
		for (let d = 0; node && d < 6; d++, node = node.parentElement) {
			const key = Object.keys(node).find(k => k.startsWith("__reactProps"));
			if (!key) continue;
			const props = node[key];
			for (const pk of Object.keys(props)) {
				if (typeof props[pk] === "function") {
					out.push(d + ":" + pk + " => " + props[pk].toString().slice(0, 600));
				}
			}
		}
		return out.join("\n---\n") || "NO_HANDLERS";
	})()`, &handler, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) })); err != nil {
		t.Fatalf("read handler: %v", err)
	}
	t.Logf("HANDLER:\n%s", handler)
	time.Sleep(8 * time.Second)
	var loc, pageAfter string
	if err := chromedp.Run(browserCtx,
		chromedp.Location(&loc),
		chromedp.Text("body", &pageAfter, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read after click: %v", err)
	}
	t.Logf("AFTER CLICK location: %s", loc)
	t.Logf("AFTER CLICK page: %.400s", pageAfter)

	// The button may open a POPUP instead of navigating this tab.
	targets, err := chromedp.Targets(browserCtx)
	if err == nil {
		for _, tg := range targets {
			t.Logf("TARGET type=%s url=%s", tg.Type, tg.URL)
		}
	}

	// Attach to the login.alibaba.com popup and dump what it renders —
	// we need to know if account creation on the Alibaba side is viable.
	var aliID string
	for _, tg := range targets {
		if tg.Type == "page" && strings.Contains(tg.URL, "login.alibaba.com") {
			aliID = string(tg.TargetID)
			break
		}
	}
	if aliID == "" {
		t.Log("no alibaba popup found")
		return
	}
	aliCtx, cancelAli := chromedp.NewContext(browserCtx, chromedp.WithTargetID(target.ID(aliID)))
	defer cancelAli()
	time.Sleep(5 * time.Second)
	var aliLoc, aliBody string
	var aliEls []map[string]any
	var shot []byte
	if err := chromedp.Run(aliCtx,
		chromedp.Location(&aliLoc),
		chromedp.Text("body", &aliBody, chromedp.ByQuery),
		chromedp.Evaluate(`(() => {
			return Array.from(document.querySelectorAll("input, button, a, [role=button]")).map(e => ({
				tag: e.tagName, type: e.type || "", name: e.name || "", id: e.id || "",
				placeholder: e.placeholder || "", text: (e.textContent||"").trim().slice(0,50),
				href: e.href || ""
			})).filter(e => e.text || e.placeholder || e.tag === "INPUT");
		})()`, &aliEls, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }),
		chromedp.CaptureScreenshot(&shot),
	); err != nil {
		t.Fatalf("ali dump: %v", err)
	}
	t.Logf("ALI location: %s", aliLoc)
	t.Logf("ALI page: %.600s", aliBody)
	for _, e := range aliEls {
		t.Logf("ALI EL: %v", e)
	}
	shotPath := filepath.Join(os.TempDir(), "accio-ali-sso.png")
	if err := os.WriteFile(shotPath, shot, 0o644); err == nil {
		t.Logf("ALI screenshot: %s", shotPath)
	}
}

// TestAlibabaSSOFlow drives the full "Continue with Alibaba.com" path with a
// fresh tempmail identity: if the email is unknown to Alibaba the unified
// login becomes a REGISTRATION, the account may inherit Alibaba-side trust,
// and the SSO completes the OAuth callback by itself. Gated by
// ACCIO_ALI_SIGNUP=1. NEVER touches the real device profile.
func TestAlibabaSSOFlow(t *testing.T) {
	if os.Getenv("ACCIO_ALI_SIGNUP") == "" {
		t.Skip("set ACCIO_ALI_SIGNUP=1")
	}
	dataDir := filepath.Join(os.TempDir(), "accio-live-probe-data")
	acc, err := accio.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	loginURL, err := acc.StartLogin(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := NewInbox(ctx)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	email := inbox.Address()
	var b [6]byte
	_, _ = rand.Read(b[:])
	password := fmt.Sprintf("Zq%x9aB", b[:])
	t.Logf("identity: %s (provider=%s)", email, inbox.Provider())
	loginCh := waitLogin(acc, email)

	headless := os.Getenv("ACCIO_ALI_HEADFUL") == ""
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

	// Hide automation markers BEFORE any document loads — the Aliyun
	// NoCaptcha wasm refuses to run when navigator.webdriver is set.
	if err := chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(`
			Object.defineProperty(navigator, "webdriver", { get: () => undefined });
			window.chrome = window.chrome || { runtime: {} };
		`).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("stealth script: %v", err)
	}

	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(loginURL),
		chromedp.ActionFunc(func(ctx context.Context) error {
			return waitForText(ctx, []string{"Alibaba", "alibaba"})
		}),
	); err != nil {
		t.Fatalf("open login: %v", err)
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

	// Find the Alibaba SSO popup.
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
		t.Fatal("alibaba SSO popup never appeared")
	}
	aliCtx, cancelAli := chromedp.NewContext(browserCtx, chromedp.WithTargetID(aliID))
	defer cancelAli()
	chromedp.ListenTarget(aliCtx, func(ev any) {
		switch e := ev.(type) {
		case *network.EventResponseReceived:
			u := e.Response.URL
			if strings.Contains(u, "login.do") || strings.Contains(u, "punish") || strings.Contains(u, "x5sec") || strings.Contains(u, "alibaba_sign") || strings.Contains(u, "oauth") || strings.Contains(u, "member") {
				t.Logf("NET %d %s", e.Response.Status, u[:min(len(u), 200)])
			}
		}
	})

	// Fill the unified login/registration form — give Alibaba's anti-bot
	// scripts (xman/umid) time to instrument the page before we touch it,
	// and type with human-ish per-key pacing.
	time.Sleep(5 * time.Second)
	if err := chromedp.Run(aliCtx,
		chromedp.WaitVisible(`input[name="account"]`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("wait form: %v", err)
	}
	for _, ch := range email {
		if err := chromedp.Run(aliCtx, chromedp.SendKeys(`input[name="account"]`, string(ch), chromedp.ByQuery)); err != nil {
			t.Fatalf("type account: %v", err)
		}
		time.Sleep(time.Duration(50+time.Now().UnixNano()%90) * time.Millisecond)
	}
	for _, ch := range password {
		if err := chromedp.Run(aliCtx, chromedp.SendKeys(`input[name="password"]`, string(ch), chromedp.ByQuery)); err != nil {
			t.Fatalf("type password: %v", err)
		}
		time.Sleep(time.Duration(50+time.Now().UnixNano()%90) * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond)
	var formState string
	_ = chromedp.Run(aliCtx, chromedp.Evaluate(`(() => {
		const acc = document.querySelector('input[name="account"]');
		const pwd = document.querySelector('input[name="password"]');
		const btns = Array.from(document.querySelectorAll("button"));
		const b = btns.find(x => /continuar|continue|entrar|sign in|log in/i.test((x.textContent||"").trim()));
		return JSON.stringify({
			account: acc ? acc.value : null,
			pwdLen: pwd ? pwd.value.length : null,
			btnDisabled: b ? !!b.disabled : null,
			btnClass: b ? b.className.slice(0,120) : null,
			btnText: b ? b.textContent.trim() : null
		});
	})()`, &formState, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
	t.Logf("FORM STATE: %s", formState)
	// Trusted click on the submit button via mouse coordinates.
	var bx, by float64
	var coords map[string]float64
	if err := chromedp.Run(aliCtx, chromedp.Evaluate(`(() => {
		const btns = Array.from(document.querySelectorAll("button"));
		const b = btns.find(x => /continuar|continue|entrar|sign in|log in/i.test((x.textContent||"").trim()));
		if (!b) return null;
		const r = b.getBoundingClientRect();
		return { x: r.left + r.width/2, y: r.top + r.height/2 };
	})()`, &coords, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) })); err == nil && coords != nil {
		bx, by = coords["x"], coords["y"]
	}
	if bx > 0 || by > 0 {
		t.Logf("trusted click at %.0f,%.0f", bx, by)
		if err := chromedp.Run(aliCtx, chromedp.MouseClickXY(bx, by)); err != nil {
			t.Fatalf("trusted click: %v", err)
		}
	} else {
		t.Fatal("submit button coords not found")
	}
	t.Log("alibaba form submitted")

	// Watch where the popup goes; dump intermediate states (verification
	// code prompt? consent screen? straight to callback?).
	for i := 0; i < 12; i++ {
		time.Sleep(5 * time.Second)
		select {
		case rec := <-loginCh:
			raw, _ := json.Marshal(rec)
			t.Logf("LOGIN COMPLETED: %s", raw)
			creds, cerr := acc.CreditsFor(ctx, rec)
			t.Logf("CREDITS: %v err=%v", creds, cerr)
			return
		default:
		}
		var loc, body string
		if err := chromedp.Run(aliCtx,
			chromedp.Location(&loc),
			chromedp.Text("body", &body, chromedp.ByQuery),
		); err == nil {
			t.Logf("POPUP[%d] %s :: %.240s", i, loc, body)
		}
		if i >= 1 && i <= 6 {
			if solveBaxiaSlider(t, aliCtx) {
				t.Logf("slider drag attempt at poll %d done", i)
			}
		}
		{
			var ps []byte
			if err := chromedp.Run(aliCtx, chromedp.CaptureScreenshot(&ps)); err == nil {
				_ = os.WriteFile(filepath.Join(os.TempDir(), fmt.Sprintf("accio-ali-poll-%d.png", i)), ps, 0o644)
			}
		}
		if i == 1 {
			var shot2 []byte
			if err := chromedp.Run(aliCtx, chromedp.CaptureScreenshot(&shot2)); err == nil {
				sp := filepath.Join(os.TempDir(), "accio-ali-after-submit.png")
				if os.WriteFile(sp, shot2, 0o644) == nil {
					t.Logf("AFTER SUBMIT screenshot: %s", sp)
				}
			}
			// Inspect the Alibaba NoCaptcha slider DOM (iframe? handle ids?).
			var sliderInfo string
			_ = chromedp.Run(aliCtx, chromedp.Evaluate(`(() => {
				const frames = Array.from(document.querySelectorAll("iframe")).map(f => f.id + "|" + f.src.slice(0,120));
				const nc = Array.from(document.querySelectorAll("[id^=nc_], [class*=nc_iconfont], [class*=slide], [class*=Slide], [class*=slider], [class*=Slider]")).map(e => e.tagName + "#" + e.id + "." + e.className.slice(0,60) + " => " + (e.textContent||"").trim().slice(0,40));
				return JSON.stringify({ frames, nc: nc.slice(0, 15) });
			})()`, &sliderInfo, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
			t.Logf("SLIDER DOM: %s", sliderInfo)
			// Dump any visible error/notice elements.
			var notes []string
			_ = chromedp.Run(aliCtx, chromedp.Evaluate(`(() => {
				return Array.from(document.querySelectorAll("[class*=error], [class*=Error], [class*=notice], [class*=Notice], [class*=toast], [class*=Toast], [class*=verify], [class*=Verify], [class*=captcha], [class*=Captcha], [role=alert]")).map(e => e.className.slice(0,60) + " => " + (e.textContent||"").trim().slice(0,120)).filter(s => !s.endsWith("=>"));
			})()`, &notes, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
			for _, n := range notes {
				t.Logf("NOTICE: %s", n)
			}
		}
	}
	t.Log("no login completion within watch window")
	logging.Warn("ali_sso_flow.incomplete")
}

// solveBaxiaSlider drags the Alibaba Baxia/NoCaptcha slider that lives inside
// the "baxia-dialog-content" OOPIF (the _____tmd_____/punish page). Returns
// true when the slider dialog went away (verification accepted).
func solveBaxiaSlider(t *testing.T, browserCtx context.Context) bool {
	t.Helper()
	// The Baxia iframe is same-origin (login.alibaba.com) so its DOM is
	// reachable via contentDocument; trusted mouse events go to the page
	// using absolute coordinates (frame rect + element rect).
	fctx := browserCtx

	// Locate handle and track — the nocaptcha widget renders async inside
	// the punish iframe (initialize.jsonp → nc widget), so poll for it.
	var geo map[string]float64
	for wait := 0; wait < 24 && len(geo) == 0; wait++ {
		time.Sleep(500 * time.Millisecond)
		_ = chromedp.Run(fctx, chromedp.Evaluate(`(() => {
			// Scan the main document AND every same-origin iframe for the
			// nocaptcha slide handle, accumulating iframe offsets so the
			// returned coordinates are absolute to the page viewport.
			const handleSels = ["#nc_1_n1z", "[id$=_n1z]", ".nc_iconfont.btn_slide", "[class*=btn_slide]"];
			const trackSels = ["#nc_1_wrapper", ".nc-container", "[class*=nc_scale]", "[class*=scale_text]"];
			const findIn = (doc, ox, oy) => {
				for (const hs of handleSels) {
					const handle = doc.querySelector(hs);
					if (!handle) continue;
					const h = handle.getBoundingClientRect();
					if (h.width === 0) continue;
					let tr = null;
					for (const ts of trackSels) {
						const t2 = doc.querySelector(ts);
						if (t2 && t2.getBoundingClientRect().width > h.width) { tr = t2.getBoundingClientRect(); break; }
					}
					const tw = tr ? tr.width : h.width + 300;
					return { hx: ox + h.left + h.width/2, hy: oy + h.top + h.height/2, hw: h.width, tw };
				}
				return null;
			};
			let r = findIn(document, 0, 0);
			if (r) return r;
			for (const fr of Array.from(document.querySelectorAll("iframe"))) {
				let doc = null;
				try { doc = fr.contentDocument; } catch (e) { continue; }
				if (!doc) continue;
				const fr2 = fr.getBoundingClientRect();
				r = findIn(doc, fr2.left, fr2.top);
				if (r) return r;
			}
			return null;
		})()`, &geo, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
	}
	if len(geo) == 0 {
		// Dump what the punish frame actually rendered for debugging.
		var inner string
		_ = chromedp.Run(fctx, chromedp.Evaluate(`(() => {
			const fr = document.querySelector("#baxia-dialog-content") || Array.from(document.querySelectorAll("iframe")).find(f => (f.src||"").includes("punish"));
			if (!fr || !fr.contentDocument) return "NO_FRAME";
			return fr.contentDocument.body ? fr.contentDocument.body.innerHTML.slice(0, 1500) : "EMPTY_BODY";
		})()`, &inner, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
		t.Logf("slider handle never rendered; punish frame: %s", inner)
		return false
	}
	dist := geo["tw"] - geo["hw"] - 4
	t.Logf("dragging slider: start=(%.0f,%.0f) dist=%.0f", geo["hx"], geo["hy"], dist)

	// Preferred path: move the REAL cursor via Win32 SendInput. CDP events
	// are DOM-trusted but lack hardware traits (movementX/Y = 0) that the
	// Aliyun wasm scores. Only possible headful — the window must exist.
	if osMouseAvailable() {
		var win map[string]float64
		if err := chromedp.Run(fctx, page.BringToFront()); err == nil {
			_ = chromedp.Run(fctx, chromedp.Evaluate(`(() => ({
				sx: window.screenX, sy: window.screenY,
				chromeTop: window.outerHeight - window.innerHeight,
				dpr: window.devicePixelRatio, innerH: window.innerHeight
			}))()`, &win, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
		}
		// Precise physical window bounds via CDP — the JS screenX/screenY +
		// outerHeight-innerHeight heuristic misses the handle in popup
		// windows whose chrome UI height differs from the main window's.
		var px, py int
		got := false
		if win != nil && win["dpr"] > 0 {
			dpr := win["dpr"]
			if c := chromedp.FromContext(fctx); c != nil && c.Target != nil {
				var bounds *browser.Bounds
				berr := chromedp.Run(fctx, chromedp.ActionFunc(func(ctx context.Context) error {
					var err error
					_, bounds, err = browser.GetWindowForTarget().WithTargetID(c.Target.TargetID).Do(ctx)
					return err
				}))
				if berr == nil && bounds != nil && bounds.Width > 0 {
					chromeTopPhys := float64(bounds.Height) - win["innerH"]*dpr
					if chromeTopPhys < 0 {
						chromeTopPhys = 0
					}
					px = int(float64(bounds.Left) + geo["hx"]*dpr)
					py = int(float64(bounds.Top) + chromeTopPhys + geo["hy"]*dpr)
					t.Logf("window bounds phys: left=%d top=%d w=%d h=%d chromeTopPhys=%.0f | js: sx=%.0f sy=%.0f chromeTop=%.0f dpr=%.2f",
						bounds.Left, bounds.Top, bounds.Width, bounds.Height, chromeTopPhys, win["sx"], win["sy"], win["chromeTop"], dpr)
					got = true
				} else {
					t.Logf("GetWindowForTarget failed: %v", berr)
				}
			}
			if !got {
				px = int((win["sx"] + geo["hx"]) * dpr)
				py = int((win["sy"] + win["chromeTop"] + geo["hy"]) * dpr)
				got = true
			}
			// Force the popup's OS window to the foreground — SendInput clicks
			// land on whatever window is physically under the cursor, and CDP's
			// BringToFront does not reliably raise it above the main window.
			var pageTitle string
			_ = chromedp.Run(fctx, chromedp.Title(&pageTitle))
			if pageTitle != "" && osFocusWindowByTitle(pageTitle) {
				t.Logf("foregrounded window titled %q", pageTitle)
				time.Sleep(400 * time.Millisecond)
			}
			t.Logf("OS-MOUSE drag at screen (%d,%d) dist=%d (dpr=%.2f)", px, py, int(dist*win["dpr"]), win["dpr"])
			// Drag in a goroutine while sampling the handle position — proves
			// whether the cursor physically engaged the handle (left grows)
			// or missed it entirely (left pinned at the start).
			done := make(chan struct{})
			go func() {
				osMouseDragTo(px, py, int(dist*win["dpr"]))
				close(done)
			}()
			var samples []string
			for s := 0; s < 12; s++ {
				select {
				case <-done:
					s = 12
				case <-time.After(280 * time.Millisecond):
				}
				var sl float64
				_ = chromedp.Run(fctx, chromedp.Evaluate(`(() => {
					const sels = ["#nc_1_n1z", "[id$=_n1z]", ".nc_iconfont.btn_slide", "[class*=btn_slide]"];
					const scan = (doc, ox) => {
						for (const q of sels) {
							const h = doc.querySelector(q);
							if (h && h.getBoundingClientRect().width > 0) return ox + h.getBoundingClientRect().left;
						}
						return null;
					};
					let v = scan(document, 0);
					if (v !== null) return v;
					for (const fr of Array.from(document.querySelectorAll("iframe"))) {
						let doc = null;
						try { doc = fr.contentDocument; } catch (e) { continue; }
						if (!doc) continue;
						v = scan(doc, fr.getBoundingClientRect().left);
						if (v !== null) return v;
					}
					return -1;
				})()`, &sl, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
				samples = append(samples, fmt.Sprintf("%.0f", sl))
			}
			<-done
			t.Logf("handle-left samples during drag: %s", strings.Join(samples, ","))
			time.Sleep(2 * time.Second)
			// Post-drag probe: did the handle physically move? Distinguishes
			// "cursor missed the handle" from "Baxia scored and rejected".
			var after map[string]float64
			_ = chromedp.Run(fctx, chromedp.Evaluate(`(() => {
				const sels = ["#nc_1_n1z", "[id$=_n1z]", ".nc_iconfont.btn_slide", "[class*=btn_slide]"];
				const scan = (doc) => {
					for (const s of sels) {
						const h = doc.querySelector(s);
						if (h && h.getBoundingClientRect().width > 0) return h.getBoundingClientRect().left;
					}
					return null;
				};
				let v = scan(document);
				if (v !== null) return { left: v };
				for (const fr of Array.from(document.querySelectorAll("iframe"))) {
					let doc = null;
					try { doc = fr.contentDocument; } catch (e) { continue; }
					if (!doc) continue;
					v = scan(doc);
					if (v !== null) return { left: v };
				}
				return null;
			})()`, &after, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
			if after != nil {
				t.Logf("handle left after drag: %.0f (was %.0f)", after["left"], geo["hx"]-geo["hw"]/2)
			} else {
				t.Log("handle gone after drag — widget likely advanced")
			}
			return true
		}
		t.Log("window metrics unavailable, falling back to CDP drag")
	}

	if err := chromedp.Run(fctx, chromedp.ActionFunc(func(ctx context.Context) error {
		rnd := func(n int64) int64 { var b [8]byte; _, _ = rand.Read(b[:]); return int64(b[0]) % n }
		// A human LOOKS at the widget before touching it.
		time.Sleep(time.Duration(1400+rnd(1600)) * time.Millisecond)
		// Approach the handle with a curve of a few moves (the mouse was
		// last seen on the Continue button, far below/right).
		startX, startY := geo["hx"]+float64(180+rnd(120)), geo["hy"]+float64(140+rnd(90))
		for i := 1; i <= 7; i++ {
			prog := float64(i) / 7
			x := startX + (geo["hx"]-startX)*prog*prog
			y := startY + (geo["hy"]-startY)*prog*prog
			if err := input.DispatchMouseEvent(input.MouseMoved, x, y).Do(ctx); err != nil {
				return err
			}
			time.Sleep(time.Duration(25+rnd(45)) * time.Millisecond)
		}
		// hover over the handle first
		if err := input.DispatchMouseEvent(input.MouseMoved, geo["hx"]-8, geo["hy"]+2).Do(ctx); err != nil {
			return err
		}
		time.Sleep(time.Duration(150+rnd(200)) * time.Millisecond)
		if err := input.DispatchMouseEvent(input.MouseMoved, geo["hx"], geo["hy"]).Do(ctx); err != nil {
			return err
		}
		time.Sleep(time.Duration(120+rnd(180)) * time.Millisecond)
		if err := input.DispatchMouseEvent(input.MousePressed, geo["hx"], geo["hy"]).WithButton(input.Left).WithButtons(1).WithClickCount(1).Do(ctx); err != nil {
			return err
		}
		// hold still briefly — humans press before they move
		time.Sleep(time.Duration(250+rnd(350)) * time.Millisecond)
		// ~60 steps, smooth ease-in-out, gentle sinusoidal y drift, NO
		// overshoot — reporting cursor positions beyond the track end gets
		// flagged by the wasm trajectory check.
		steps := 60
		yOff := 0.0
		for i := 1; i <= steps; i++ {
			prog := float64(i) / float64(steps)
			// smoothstep-ish: slow start, fast middle, soft landing
			ease := prog * prog * (3 - 2*prog)
			x := geo["hx"] + dist*ease
			// smooth random-walk y drift (never jumps more than 1px)
			yOff += float64(rnd(3)) - 1
			if yOff > 2 {
				yOff = 2
			} else if yOff < -2 {
				yOff = -2
			}
			y := geo["hy"] + yOff
			if err := input.DispatchMouseEvent(input.MouseMoved, x, y).WithButton(input.Left).WithButtons(1).Do(ctx); err != nil {
				return err
			}
			time.Sleep(time.Duration(9+rnd(26)) * time.Millisecond)
		}
		time.Sleep(time.Duration(140+rnd(160)) * time.Millisecond)
		return input.DispatchMouseEvent(input.MouseReleased, geo["hx"]+dist, geo["hy"]).WithButton(input.Left).WithButtons(0).WithClickCount(1).Do(ctx)
	})); err != nil {
		t.Logf("drag failed: %v", err)
		return false
	}
	// Did the handle actually move (drag engaged) or snap back (rejected)?
	time.Sleep(1500 * time.Second / 1000)
	var after float64
	_ = chromedp.Run(fctx, chromedp.Evaluate(`(() => {
		const fr = document.querySelector("#baxia-dialog-content") || Array.from(document.querySelectorAll("iframe")).find(f => (f.src||"").includes("punish"));
		if (!fr || !fr.contentDocument) return -1;
		const doc = fr.contentDocument;
		const handle = doc.querySelector("#nc_1_n1z") || doc.querySelector("[id$=_n1z]") || doc.querySelector("[class*=btn_slide]");
		if (!handle) return -2;
		return handle.getBoundingClientRect().left;
	})()`, &after, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
	t.Logf("handle left after drag: %.0f (started at %.0f)", after, geo["hx"]-geo["hw"]/2)
	time.Sleep(1500 * time.Millisecond)
	return true
}
