package accmgr

// Recon + driver: Alibaba.com buyer registration via the email-first unified
// flow (login.alibaba.com/newlogin/icbuLogin.htm). Iterates a small state
// machine over the screens: slider captcha, account-type picker, password
// creation, email code verification. Gated by ACCIO_ALI_REG_RECON=1.

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// aliScreen snapshots the current page: location and body text so the state
// machine can decide what to do.
func aliScreen(ctx context.Context) (loc, body string) {
	_ = chromedp.Run(ctx,
		chromedp.Location(&loc),
		chromedp.Text("body", &body, chromedp.ByQuery),
	)
	return loc, body
}

// aliClickText clicks (trusted, by coordinates) the first button/link whose
// trimmed lowercase text equals or starts with one of the needles.
func aliClickText(t *testing.T, ctx context.Context, needles ...string) bool {
	t.Helper()
	var res map[string]any
	needleList := `["` + strings.Join(needles, `","`) + `"]`
	err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
		const needles = `+needleList+`;
		const els = Array.from(document.querySelectorAll("button, a, [role=button], input[type=submit]"));
		for (const e of els) {
			const txt = (e.textContent||"").trim().toLowerCase();
			if (!txt) continue;
			for (const n of needles) {
				if (txt === n || txt.startsWith(n)) {
					const r = e.getBoundingClientRect();
					if (r.width > 0) return { x: r.left + r.width/2, y: r.top + r.height/2, txt };
				}
			}
		}
		return null;
	})()`, &res, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
	if err != nil || res == nil {
		return false
	}
	x, _ := res["x"].(float64)
	y, _ := res["y"].(float64)
	if err := chromedp.Run(ctx, chromedp.MouseClickXY(x, y)); err != nil {
		return false
	}
	t.Logf("clicked: %v", res["txt"])
	return true
}

// aliFill finds a visible input by selector and types value with pacing.
func aliFill(ctx context.Context, sel, value string) error {
	for _, ch := range value {
		if err := chromedp.Run(ctx, chromedp.SendKeys(sel, string(ch), chromedp.ByQuery)); err != nil {
			return err
		}
		time.Sleep(time.Duration(40+time.Now().UnixNano()%80) * time.Millisecond)
	}
	return nil
}

// aliCount returns how many visible inputs match the selector.
func aliCount(ctx context.Context, sel string) int {
	var n int
	js := fmt.Sprintf(`(() => {
		return Array.from(document.querySelectorAll(%q)).filter(e => e.getBoundingClientRect().width > 0).length;
	})()`, sel)
	_ = chromedp.Run(ctx, chromedp.Evaluate(js, &n, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
	return n
}

func TestAlibabaRegRecon(t *testing.T) {
	if os.Getenv("ACCIO_ALI_REG_RECON") == "" {
		t.Skip("set ACCIO_ALI_REG_RECON=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	execOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", false),
		chromedp.Flag("no-proxy-server", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.WindowSize(1440, 900),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, execOpts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()
	if err := chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(`
			Object.defineProperty(navigator, "webdriver", { get: () => undefined });
			window.chrome = window.chrome || { runtime: {} };
		`).Do(ctx)
		return err
	})); err != nil {
		t.Fatal(err)
	}

	inbox, err := NewInbox(ctx)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	email := inbox.Address()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	password := fmt.Sprintf("Zq%x9aB", b[:])
	t.Logf("identity: %s pwd=%s", email, password)

	if !alibabaRegister(t, browserCtx, inbox, email, password) {
		t.Log("registration did not complete")
	}
}

// alibabaRegister runs the full buyer registration: open the unified
// email-first page, submit the address, then drive the screen state machine.
func alibabaRegister(t *testing.T, browserCtx context.Context, inbox Inbox, email, password string) bool {
	t.Helper()
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate("https://login.alibaba.com/newlogin/icbuLogin.htm"),
		chromedp.Sleep(6*time.Second),
		chromedp.WaitVisible(`input[name="email"]`, chromedp.ByQuery),
	); err != nil {
		t.Logf("open icbuLogin: %v", err)
		return false
	}
	if err := aliFill(browserCtx, `input[name="email"]`, email); err != nil {
		t.Logf("type email: %v", err)
		return false
	}
	time.Sleep(time.Second)
	if !aliClickText(t, browserCtx, "continue", "continuar") {
		t.Log("continue button not found")
		return false
	}
	return alibabaRegisterFlow(t, browserCtx, inbox, password)
}

// alibabaRegisterFlow drives the post-email registration screens. Returns
// true once the account setup completes.
func alibabaRegisterFlow(t *testing.T, browserCtx context.Context, inbox Inbox, password string) bool {
	t.Helper()
	var code string
	codeFetched := false
	lastResubmit := -10
	sliderTries := 0
	setupFirst, setupLast := randomAliName()
	for i := 0; i < 18; i++ {
		time.Sleep(4 * time.Second)
		loc, body := aliScreen(browserCtx)
		low := strings.ToLower(body)
		var shot []byte
		if err := chromedp.Run(browserCtx, chromedp.CaptureScreenshot(&shot)); err == nil {
			_ = os.WriteFile(filepath.Join(os.TempDir(), fmt.Sprintf("accio-ali-regflow-%02d.png", i)), shot, 0o644)
		}
		t.Logf("STATE[%d] %s :: %.200s", i, loc, body)

		switch {
		case strings.Contains(low, "slide to verify") || strings.Contains(low, "deslize"):
			solveBaxiaSlider(t, browserCtx)
		case strings.Contains(low, "which account would you like"):
			// Buyer is preselected; just continue.
			aliClickText(t, browserCtx, "continue", "continuar")
		case aliCount(browserCtx, `input[name="email"]`) > 0:
			// Email screen. If the slider is up, solve it and let the
			// widget complete the pending submit by itself (an extra
			// Continue click causes a double submit and a rejection).
			// Otherwise re-submit to trigger a fresh slider.
			if sliderTries >= 4 {
				t.Log("slider rejected 4x — Baxia has hardened against this session/IP, aborting to avoid burning it further")
				return false
			}
			if solveBaxiaSlider(t, browserCtx) {
				sliderTries++
				t.Logf("slider solved on email screen at %d", i)
				// One follow-up click: the widget validates but does not
				// always re-fire the pending submit.
				time.Sleep(2 * time.Second)
				aliClickText(t, browserCtx, "continue", "continuar")
				lastResubmit = i
			} else if i-lastResubmit >= 2 {
				// Widget may be sitting in a broken "Click to retry" state —
				// reset it before giving up on this submit.
				if aliClickText(t, browserCtx, "click to retry", "recarregar") {
					t.Logf("widget retry clicked at %d", i)
					lastResubmit = i - 1 // let the slider re-render first
				} else {
					t.Logf("email screen, no slider — re-submitting")
					aliClickText(t, browserCtx, "continue", "continuar")
					lastResubmit = i
				}
			}
		case aliCount(browserCtx, `input[type="text"]`) >= 5:
			// 6-box verification code screen (one input per digit).
			if !codeFetched {
				codeFetched = true
				c, cerr := inbox.WaitCode(browserCtx)
				if cerr != nil {
					t.Logf("wait code: %v", cerr)
				} else {
					code = c
					t.Logf("got code: %s", code)
				}
			}
			if code != "" && len(code) >= 4 {
				// Tag each box with a temp id, then type one digit each
				// with trusted key events.
				_ = chromedp.Run(browserCtx, chromedp.Evaluate(`(() => {
					const ins = Array.from(document.querySelectorAll("input[type=text]")).filter(e => e.getBoundingClientRect().width > 0);
					ins.forEach((e, i) => { e.id = "__code_digit_" + i; });
					return ins.length;
				})()`, new(int), func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
				digits := strings.TrimSpace(code)
				for i := 0; i < len(digits) && i < 6; i++ {
					sel := fmt.Sprintf("#__code_digit_%d", i)
					if err := chromedp.Run(browserCtx, chromedp.SendKeys(sel, string(digits[i]), chromedp.ByQuery)); err != nil {
						t.Logf("digit %d: %v", i, err)
					}
					time.Sleep(120 * time.Millisecond)
				}
				t.Logf("code digits typed")
				time.Sleep(time.Second)
				aliClickText(t, browserCtx, "continue", "continuar", "verify", "verificar", "confirm", "confirmar")
			}
		case strings.Contains(low, "verification code") || strings.Contains(low, "digo de verifica"):
			if !codeFetched {
				codeFetched = true
				c, cerr := inbox.WaitCode(browserCtx)
				if cerr != nil {
					t.Logf("wait code: %v", cerr)
				} else {
					code = c
					t.Logf("got code: %s", code)
				}
			}
			if code != "" {
				var dump string
				_ = chromedp.Run(browserCtx, chromedp.Evaluate(`(() => {
					const ins = Array.from(document.querySelectorAll("input")).filter(e => e.getBoundingClientRect().width > 0);
					return ins.map(e => (e.name||"") + "|" + (e.placeholder||"") + "|" + (e.type||"")).join(" ;; ");
				})()`, &dump, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
				t.Logf("CODE SCREEN inputs: %s", dump)
				if aliCount(browserCtx, `input[name="code"]`) > 0 {
					_ = aliFill(browserCtx, `input[name="code"]`, code)
					aliClickText(t, browserCtx, "verify", "verificar", "continue", "continuar", "confirm", "confirmar")
				}
			}
		case strings.Contains(low, "set up your account"):
			// Final form: country (preselected), first/last name, password,
			// mandatory terms checkbox. Fill ONCE with trusted keys; on
			// later passes only correct mismatches (typed input APPENDS,
			// so re-typing unchanged values corrupts the field).
			_ = chromedp.Run(browserCtx, chromedp.Evaluate(`(() => {
				const ins = Array.from(document.querySelectorAll("input")).filter(e => e.getBoundingClientRect().width > 0);
				let firstEl = ins.find(e => /first name/i.test(e.placeholder||""));
				let lastEl = ins.find(e => /last name/i.test(e.placeholder||""));
				const texts = ins.filter(e => (e.type === "text" || !e.type) && e.name !== "email" && e.type !== "search" && !e.readOnly && e.id !== "__setup_pwd");
				if (!firstEl) firstEl = texts[0];
				if (!lastEl) lastEl = texts.find(e => e !== firstEl);
				if (firstEl) firstEl.id = "__setup_first";
				if (lastEl) lastEl.id = "__setup_last";
				const pwd = ins.find(e => e.type === "password");
				if (pwd) pwd.id = "__setup_pwd";
				return true;
			})()`, new(bool), func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
			fixField := func(sel, want string) {
				var cur string
				_ = chromedp.Run(browserCtx, chromedp.Evaluate(fmt.Sprintf(`(() => { const e = document.querySelector(%q); return e ? e.value : ""; })()`, sel), &cur, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
				if cur == want {
					return
				}
				// clear via JS, then type fresh with trusted keys
				_ = chromedp.Run(browserCtx, chromedp.Evaluate(fmt.Sprintf(`(() => {
					const e = document.querySelector(%q);
					if (!e) return false;
					const st = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, "value").set;
					e.focus();
					st.call(e, "");
					e.dispatchEvent(new Event("input", { bubbles: true }));
					return true;
				})()`, sel), new(bool), func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
				_ = aliFill(browserCtx, sel, want)
			}
			fixField(`#__setup_first`, setupFirst)
			fixField(`#__setup_last`, setupLast)
			fixField(`#__setup_pwd`, password)
			// Terms checkbox (custom widget) — click only when unchecked.
			_ = chromedp.Run(browserCtx, chromedp.Evaluate(`(() => {
				const cb = Array.from(document.querySelectorAll("[role=checkbox], [class*=checkbox], [class*=Checkbox]")).find(e => e.getBoundingClientRect().width > 0);
				if (cb && cb.getAttribute("aria-checked") !== "true" && !/checked/i.test(cb.className)) cb.click();
				return !!cb;
			})()`, new(bool), func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
			var setup string
			_ = chromedp.Run(browserCtx, chromedp.Evaluate(`(() => {
				const v = id => { const e = document.querySelector(id); return e ? e.value : null; };
				const cb = Array.from(document.querySelectorAll("[role=checkbox], [class*=checkbox], [class*=Checkbox]")).find(e => e.getBoundingClientRect().width > 0);
				return JSON.stringify({ first: v("#__setup_first"), last: v("#__setup_last"), pwdLen: (v("#__setup_pwd")||"").length, terms: cb ? (cb.getAttribute("aria-checked") || cb.className) : null });
			})()`, &setup, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
			t.Logf("SETUP FORM: %s", setup)
			time.Sleep(time.Second)
			aliClickText(t, browserCtx, "confirm", "confirmar", "continue", "continuar")
		case aliCount(browserCtx, `input[type="password"]`) > 0:
			_ = aliFill(browserCtx, `input[type="password"]`, password)
			// If a confirm-password field exists, fill it too.
			var filled bool
			_ = chromedp.Run(browserCtx, chromedp.Evaluate(fmt.Sprintf(`(() => {
				const pws = Array.from(document.querySelectorAll("input[type=password]")).filter(e => e.getBoundingClientRect().width > 0);
				if (pws.length < 2) return false;
				const el = pws[1];
				el.focus();
				const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, "value").set;
				setter.call(el, %q);
				el.dispatchEvent(new Event("input", { bubbles: true }));
				el.dispatchEvent(new Event("change", { bubbles: true }));
				return true;
			})()`, password), &filled, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
			t.Logf("password filled, confirm field: %v", filled)
			time.Sleep(time.Second)
			aliClickText(t, browserCtx, "continue", "continuar", "next", "confirm", "sign up", "create")
		case strings.Contains(low, "welcome") || strings.Contains(low, "bem-vindo") || (strings.Contains(loc, "alibaba.com") && !strings.Contains(loc, "login.alibaba.com")):
			t.Logf("REGISTRATION LOOKS COMPLETE: %s", loc)
			return true
		default:
			// The slider text lives inside a cross-frame widget; always
			// probe for the handle before declaring the screen unknown.
			if solveBaxiaSlider(t, browserCtx) {
				t.Logf("slider solved via probe at %d", i)
				continue
			}
			var dump string
			_ = chromedp.Run(browserCtx, chromedp.Evaluate(`(() => {
				const ins = Array.from(document.querySelectorAll("input, button, select")).filter(e => e.getBoundingClientRect().width > 0);
				return ins.map(e => e.tagName + ":" + (e.type||"") + ":" + (e.name||"") + ":" + (e.placeholder||"") + ":" + (e.textContent||"").trim().slice(0,30)).join(" ;; ");
			})()`, &dump, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithReturnByValue(true) }))
			t.Logf("UNKNOWN SCREEN inputs: %s", dump)
		}
	}
	t.Log("state machine exhausted iterations")
	return false
}

// randomAliName picks a plausible Brazilian-ish buyer name pair.
func randomAliName() (string, string) {
	firsts := []string{"Carlos", "Rafael", "Bruno", "Diego", "Felipe", "Rodrigo", "Marcelo", "Andre", "Lucas", "Thiago"}
	lasts := []string{"Silva", "Santos", "Oliveira", "Souza", "Pereira", "Costa", "Almeida", "Ferreira", "Ribeiro", "Carvalho"}
	var b [2]byte
	_, _ = rand.Read(b[:])
	return firsts[int(b[0])%len(firsts)], lasts[int(b[1])%len(lasts)]
}
