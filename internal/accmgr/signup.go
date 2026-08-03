package accmgr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"grok-desktop/internal/accio"
	"grok-desktop/internal/logging"
)

type signupOptions struct {
	warp     bool
	headless bool
}

// signupFlow creates one brand-new Accio account using a disposable inbox and
// the proxy's own PKCE login, so the account lands in the pool as a normal
// OAuth token.
//
// The Accio web flow has a quirk: a freshly created account is redirected
// back with *web* session tokens (which the backend rejects for OAuth use).
// The session lives on in the browser profile, so the flow runs a second
// PKCE pass on the same profile: "continue with existing account" then emits
// a real OAuth code, which the client exchanges into a working token pair.
func signupFlow(ctx context.Context, acc *accio.Client, opts signupOptions) (accio.TokenRecord, error) {
	var rec accio.TokenRecord

	inbox, err := NewInbox(ctx)
	if err != nil {
		return rec, fmt.Errorf("temp inbox: %w", err)
	}
	logging.Info("accmgr.tempmail", "address", inbox.Address())

	if opts.warp {
		if ip, err := rotateWARP(); err != nil {
			logging.Warn("accmgr.warp_failed", "err", err.Error())
		} else {
			logging.Info("accmgr.warp_rotated", "ip", ip)
		}
	}

	profile := filepath.Join(os.TempDir(), "accio-signup-"+randSuffix())
	defer os.RemoveAll(profile)

	// Pass A: register the account (email + code). The token delivered here
	// is expected to be a web-session token — validation decides. The pass can
	// also fail entirely (site redirects to the web app instead of the OAuth
	// callback, or the code exchange is rejected). Either way the web session
	// now exists in the profile, so we always proceed to the PKCE pass.
	first, errA := runLoginPass(ctx, acc, profile, inbox, "signup")
	if errA == nil {
		logging.Info("accmgr.pass_a_token", "id", first.ID, "access", first.AccessToken[:8], "refresh", first.RefreshToken[:16])
		if ok, reason := validateToken(ctx, acc, first); ok {
			logging.Info("accmgr.signup_ok", "id", first.ID, "email", first.Email)
			return first, nil
		} else {
			logging.Warn("accmgr.signup_web_token", "id", first.ID, "reason", reason)
		}
		_ = acc.RemoveAccount(first.ID)
	} else {
		logging.Warn("accmgr.pass_a_failed", "err", errA.Error())
	}

	// Pass B: PKCE re-login on the same profile (web session exists).
	second, errB := runLoginPass(ctx, acc, profile, inbox, "pkce")
	if errB != nil {
		return rec, fmt.Errorf("pass A: %v; pass B: %w", errA, errB)
	}
	if second.ID == "" {
		return rec, errors.New("no token captured during PKCE pass")
	}
	if ok, reason := validateToken(ctx, acc, second); !ok {
		_ = acc.RemoveAccount(second.ID)
		return rec, fmt.Errorf("PKCE token rejected: %s", reason)
	}
	logging.Info("accmgr.signup_ok", "id", second.ID, "email", second.Email)
	return second, nil
}

// waitLogin returns a channel fed by the client's OnLogin bridge (the same
// one the app uses), filtered to fresh accounts so an unrelated login does
// not satisfy the pipeline.
func waitLogin(acc *accio.Client, _ string) <-chan accio.TokenRecord {
	ch := make(chan accio.TokenRecord, 1)
	acc.OnLogin(func(t accio.TokenRecord) {
		select {
		case ch <- t:
		default:
		}
	})
	return ch
}

// pickLang rotates UI languages per attempt so the signup session does not
// always carry the same fingerprint.
func pickLang() string {
	langs := []string{"en-US,en;q=0.9", "pt-BR,pt;q=0.9", "es-ES,es;q=0.9", "pt-PT,pt;q=0.9"}
	return langs[rand.Intn(len(langs))]
}

// humanDelay sleeps a short random time between UI actions, mimicking a real
// user instead of a script hammering the form.
func humanDelay() {
	time.Sleep(1800*time.Millisecond + time.Duration(rand.Intn(2200))*time.Millisecond)
}

// validateToken checks that the account's refresh token actually refreshes
// (the definitive test for web-vs-OAuth tokens) and persists the rotated
// token pair — Accio rotates refresh tokens on every refresh, so an un-saved
// pair is immediately burned.
func validateToken(ctx context.Context, acc *accio.Client, rec accio.TokenRecord) (bool, string) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	access, refresh, err := acc.RefreshWith(cctx, rec.RefreshToken)
	if err != nil {
		return false, err.Error()
	}
	rec.AccessToken = access
	if refresh != "" {
		rec.RefreshToken = refresh
	}
	if err := acc.SaveAccount(rec); err != nil {
		return false, "save rotated token: " + err.Error()
	}
	return true, ""
}

// runLoginPass drives one Chrome session through the signup/PKCE flow.
// mode "signup" registers a new account with email+code; mode "pkce" reuses
// the existing web session ("continue with existing account").
func runLoginPass(ctx context.Context, acc *accio.Client, profile string, inbox Inbox, mode string) (accio.TokenRecord, error) {
	loginCtx, cancelLogin := context.WithTimeout(ctx, 7*time.Minute)
	defer cancelLogin()

	loginURL, err := acc.StartLogin(loginCtx, 0)
	if err != nil {
		return accio.TokenRecord{}, fmt.Errorf("start login: %w", err)
	}

	recCh := waitLogin(acc, inbox.Address())

	execOpts := []chromedp.ExecAllocatorOption{
		chromedp.UserDataDir(profile),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.DisableGPU,
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("disable-features", "FedCm"),
		// Vary the Accept-Language per attempt (fingerprint diversity).
		chromedp.Flag("lang", pickLang()),
	}
	execOpts = append(execOpts, signupBrowserOpts()...)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(loginCtx, execOpts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	runErr := chromedp.Run(browserCtx,
		chromedp.Navigate(loginURL),
		// wait for either the email form or the existing-account prompt
		chromedp.ActionFunc(func(ctx context.Context) error {
			return waitForText(ctx, []string{
				"Digite o endereço", "Enter your email", "endereço de e-mail", "email address",
				"conta existente", "existing account", "Você se inscreveu", "You signed up",
			})
		}),
	)

	if runErr == nil {
		if mode == "signup" {
			runErr = chromedp.Run(browserCtx,
				// fresh profile: email form → submit → code screen
				chromedp.ActionFunc(func(ctx context.Context) error {
					return setReactInput(ctx, "form input", inbox.Address())
				}),
				chromedp.ActionFunc(func(ctx context.Context) error {
					humanDelay()
					return clickContinue(ctx)
				}),
				// Capture the page state after submitting the email: the
				// code screen, or the anti-fraud block, or an error.
				chromedp.ActionFunc(func(ctx context.Context) error {
					time.Sleep(6 * time.Second)
					var text string
					_ = chromedp.Run(ctx, chromedp.Text("body", &text, chromedp.ByQuery))
					logging.Info("accmgr.page_after_email", "page", truncatePage(text))
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
				// Capture the page state shortly after submitting the code so
				// failures (risk blocks, captchas) are visible in the logs.
				chromedp.ActionFunc(func(ctx context.Context) error {
					time.Sleep(4 * time.Second)
					var text string
					_ = chromedp.Run(ctx, chromedp.Text("body", &text, chromedp.ByQuery))
					logging.Info("accmgr.page_after_code", "mode", mode, "page", truncatePage(text))
					return nil
				}),
			)
		} else {
			runErr = chromedp.Run(browserCtx,
				// existing session: "continue with existing account"
				chromedp.ActionFunc(func(ctx context.Context) error {
					return clickExistingContinue(ctx)
				}),
			)
		}
	}

	// Let the OAuth redirect + token exchange settle; the listener runs
	// inside the client regardless of CDP state.
	select {
	case rec := <-recCh:
		return rec, nil
	case <-loginCtx.Done():
		if runErr != nil {
			return accio.TokenRecord{}, runErr
		}
		return accio.TokenRecord{}, fmt.Errorf("pass %s: timeout waiting for token", mode)
	case <-time.After(3 * time.Minute):
		if runErr != nil {
			return accio.TokenRecord{}, runErr
		}
		return accio.TokenRecord{}, fmt.Errorf("pass %s: token did not arrive", mode)
	}
}

func truncatePage(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 220 {
		return s[:220] + "…"
	}
	return s
}

// clickExistingContinue clicks the "continue with existing account" prompt
// shown when the profile already has a web session.
func clickExistingContinue(ctx context.Context) error {
	js := `(() => {
		const visible = b => b.offsetParent !== null;
		const buttons = Array.from(document.querySelectorAll("button")).filter(visible);
		const textRe = /continuar|continue|iniciar sess[aã]o|sign in|entrar/i;
		const btn = buttons.find(b => textRe.test(b.textContent.trim()));
		if (!btn) return "NO_BUTTON";
		const key = Object.keys(btn).find(k => k.startsWith("__reactProps"));
		if (key && btn[key] && typeof btn[key].onClick === "function") {
			btn[key].onClick({ preventDefault() {}, stopPropagation() {}, currentTarget: btn, target: btn });
			return "REACT_CLICK:" + btn.textContent.trim();
		}
		btn.click();
		return "NATIVE_CLICK:" + btn.textContent.trim();
	})()`
	var out string
	err := chromedp.Run(ctx, chromedp.Evaluate(js, &out, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithReturnByValue(true)
	}))
	if err != nil {
		return err
	}
	if out == "NO_BUTTON" {
		return errors.New("existing-account continue button not found")
	}
	logging.Info("accmgr.click_existing", "action", out)
	time.Sleep(1500 * time.Millisecond)
	return nil
}

func waitForText(ctx context.Context, needles []string) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var text string
		err := chromedp.Run(ctx, chromedp.Text("body", &text, chromedp.ByQuery))
		if err == nil {
			lower := strings.ToLower(text)
			for _, n := range needles {
				if strings.Contains(lower, strings.ToLower(n)) {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	return errors.New("timeout waiting for page text")
}

// setReactInput writes a value into a React-controlled input the way the site
// expects: native setter + input/change events, otherwise React discards it.
func setReactInput(ctx context.Context, selector, value string) error {
	js := fmt.Sprintf(`(() => {
		const input = document.querySelector(%q);
		if (!input) return "NO_INPUT";
		const proto = window.HTMLInputElement.prototype;
		const setter = Object.getOwnPropertyDescriptor(proto, "value").set;
		setter.call(input, %q);
		input.dispatchEvent(new Event("input", { bubbles: true }));
		input.dispatchEvent(new Event("change", { bubbles: true }));
		return input.value;
	})()`, selector, value)
	var out string
	err := chromedp.Run(ctx, chromedp.Evaluate(js, &out, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithReturnByValue(true)
	}))
	if err != nil {
		return err
	}
	if out != value {
		return fmt.Errorf("input value mismatch: got %q want %q", out, value)
	}
	return nil
}

// clickContinue triggers the React submit button of the visible form.
func clickContinue(ctx context.Context) error {
	js := `(() => {
		const visible = b => b.offsetParent !== null;
		const buttons = Array.from(document.querySelectorAll("button")).filter(visible);
		const textRe = /continuar|continue|iniciar sess[aã]o|sign in|entrar|next/i;
		let btn = buttons.find(b => textRe.test(b.textContent.trim()));
		if (!btn) {
			const input = document.querySelector("form input");
			const form = input && input.closest("form");
			btn = form ? form.querySelector("button") : buttons[0];
		}
		if (!btn) return "NO_BUTTON";
		const key = Object.keys(btn).find(k => k.startsWith("__reactProps"));
		const onClick = key && btn[key] && btn[key].onClick;
		if (typeof onClick === "function") {
			onClick({ preventDefault() {}, stopPropagation() {}, currentTarget: btn, target: btn });
			return "REACT_CLICK:" + btn.textContent.trim();
		}
		btn.click();
		return "NATIVE_CLICK:" + btn.textContent.trim();
	})()`
	var out string
	err := chromedp.Run(ctx, chromedp.Evaluate(js, &out, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithReturnByValue(true)
	}))
	if err != nil {
		return err
	}
	if out == "NO_BUTTON" {
		return errors.New("continue button not found")
	}
	logging.Info("accmgr.click", "action", out)
	time.Sleep(1500 * time.Millisecond)
	return nil
}

// rotateWARP cycles the Cloudflare WARP tunnel until the public egress IP
// actually changed (disconnect+connect can return the same IP). Returns the
// new IP, or an error if it could not be rotated.
func rotateWARP() (string, error) {
	before := publicIP()
	for i := 0; i < 4; i++ {
		_ = exec.Command("warp-cli", "disconnect").Run()
		time.Sleep(2 * time.Second)
		_ = exec.Command("warp-cli", "connect").Run()
		time.Sleep(3 * time.Second)
		after := publicIP()
		if after != "" && after != before {
			return after, nil
		}
		logging.Warn("accmgr.warp_same_ip", "attempt", i+1, "ip", after)
		time.Sleep(3 * time.Second)
	}
	return publicIP(), fmt.Errorf("warp did not rotate IP (before=%q)", before)
}

// publicIP resolves the current egress IP via Cloudflare's trace endpoint.
func publicIP() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.cloudflare.com/cdn-cgi/trace", nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "ip=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "ip="))
		}
	}
	return ""
}

// signupBrowserOpts returns allocator flags; headless is preferred for
// automation (Chrome >= 112 "new" headless is hard to fingerprint), but a
// visible window can be forced for debugging with ACCIO_SIGNUP_VISIBLE=1.
func signupBrowserOpts() []chromedp.ExecAllocatorOption {
	headless := true
	if v := strings.TrimSpace(os.Getenv("ACCIO_SIGNUP_VISIBLE")); v == "1" {
		headless = false
	}
	if headless {
		return []chromedp.ExecAllocatorOption{
			chromedp.Flag("headless", "new"),
			chromedp.WindowSize(1280, 900),
		}
	}
	return []chromedp.ExecAllocatorOption{chromedp.WindowSize(1280, 900)}
}
