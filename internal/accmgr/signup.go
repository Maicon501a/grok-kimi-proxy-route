package accmgr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/storage"
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
// The session lives on in the browser profile, so each attempt runs a second
// PKCE pass on the same profile: "continue with existing account" then emits
// a real OAuth code, which the client exchanges into a working token pair.
//
// signupMaxAttempts bounds how many full signup passes we run before giving
// up. The Accio server poisons codes at issuance based on session/IP
// reputation: from a cold session the exchange fails with invalid_code
// (90001) or yields a token that is NOT_LOGIN for the entitlement API. A
// persistent, slowly-aged browser profile (like the official app's own
// partition) plus spaced retries is what gets a fully-entitled token.
var signupMaxAttempts = 4

// signupProfileDir is the PERSISTENT browser profile used for account
// creation. Reusing it across attempts and runs lets the Alibaba WAF cookies
// (cna, _m_h5_tk) age — a brand-new profile per attempt makes every signup
// look like a new device and gets the issued OAuth code poisoned by risk
// control. This mirrors the official desktop app, which signs up accounts
// from its own long-lived browser partition. Do not delete it on failure.
// signupProfileDir returns the browser profile for account creation.
//
// Default: a PERSISTENT profile (%TEMP%/accio-signup-profile) so Alibaba's
// device cookies (cna, _m_h5_tk) age like a real install. BUT those cookies
// are exactly what the risk engine fingerprints: once a profile's cna is
// flagged, EVERY account created through it is born limited (NOT_LOGIN),
// regardless of the client IP. ACCIO_FRESH_PROFILE=1 switches to a
// brand-new profile per creation — untainted device cookies.
func signupProfileDir() string {
	if os.Getenv("ACCIO_FRESH_PROFILE") == "1" {
		return filepath.Join(os.TempDir(), fmt.Sprintf("accio-signup-profile-%d", time.Now().UnixNano()))
	}
	return filepath.Join(os.TempDir(), "accio-signup-profile")
}

func signupFlow(ctx context.Context, acc *accio.Client, opts signupOptions) (accio.TokenRecord, error) {
	var rec accio.TokenRecord

	profile := signupProfileDir()
	if os.Getenv("ACCIO_FRESH_PROFILE") == "1" {
		// Fresh profiles are single-use: never let a possibly-flagged cna
		// leak into the next creation.
		defer os.RemoveAll(profile)
	}
	var attemptErrs []string
	for attempt := 1; attempt <= signupMaxAttempts; attempt++ {
		// Fresh inbox per attempt: each retry is a fully independent signup,
		// and no stale verification code from a previous attempt can leak in.
		inbox, err := NewInbox(ctx)
		if err != nil {
			return rec, fmt.Errorf("temp inbox: %w", err)
		}
		logging.Info("accmgr.tempmail", "address", inbox.Address(), "attempt", attempt)

		if opts.warp {
			if ip, err := rotateWARP(); err != nil {
				logging.Warn("accmgr.warp_failed", "err", err.Error())
			} else {
				logging.Info("accmgr.warp_rotated", "ip", ip)
			}
		}

		rec, err := signupAttempt(ctx, acc, profile, inbox)
		if err == nil {
			return rec, nil
		}
		logging.Warn("accmgr.signup_attempt_failed", "attempt", attempt, "max", signupMaxAttempts, "err", err.Error())
		attemptErrs = append(attemptErrs, fmt.Sprintf("attempt %d: %v", attempt, err))
		if errors.Is(err, errCreditsNotApproved) {
			// The account exists and the token is valid; only the
			// server-side credit grant is missing. Retrying would just
			// pile up creditless accounts on the same inbox.
			break
		}
		// Space retries out: hammering the signup endpoint back-to-back is
		// what heats up the IP/session risk score in the first place.
		backoff := time.Duration(15+attempt*10) * time.Second
		select {
		case <-ctx.Done():
			return rec, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return rec, fmt.Errorf("signup failed after %d attempt(s): %s", len(attemptErrs), strings.Join(attemptErrs, "; "))
}

// errCreditsNotApproved marks the terminal "account created but the server
// never granted credits" outcome so the retry loop does not spawn duplicates.
var errCreditsNotApproved = errors.New("credits not approved")

// signupAttempt runs one full signup on a single browser profile: register
// the account (email + code), exchange the OAuth code, then accept only when
// the entitlement API confirms the token AND a positive credit balance.
//
// Validation note: the refresh endpoint rejects brand-new accounts with
// "auth not pass" (502) — the official app's own refresh fails the same way
// for minutes after signup (captured live), while the access token already
// works against /entitlement/currentSubscription. So the entitlement read
// doubles as token validation; a refresh litmus test would reject every
// fresh account.
func signupAttempt(ctx context.Context, acc *accio.Client, profile string, inbox Inbox) (accio.TokenRecord, error) {
	var rec accio.TokenRecord

	first, err := runLoginPass(ctx, acc, profile, inbox)
	if err != nil {
		logging.Warn("accmgr.pass_a_failed", "err", err.Error())
		return rec, err
	}
	if first.ID == "" {
		return rec, errors.New("no token captured during signup pass")
	}
	logging.Info("accmgr.pass_a_token", "id", first.ID, "access", first.AccessToken[:8], "refresh", first.RefreshToken[:16])
	// Persist the inbox credential on the record: a pending account can then
	// be LOGGED IN again later (same email, fresh code) — both to re-check
	// activation and to test whether a second login ceremony flips NOT_LOGIN.
	first.InboxProvider = inbox.Provider()
	first.InboxSecret = inbox.Secret()
	if first.Email == "" {
		// userinfo can fail or return only masked addresses for fresh
		// accounts; the inbox address IS the account's email.
		first.Email = inbox.Address()
	}
	rem, creditErr := waitForCredits(ctx, acc, first)
	if creditErr != nil {
		// Do NOT delete the account: a NOT_LOGIN limitation can be a
		// temporary server-side review. The account is invisible to the
		// pool cap (balance reads error out) and the aggregate loop
		// re-checks it every cycle — if it activates later, its credits
		// join the pool automatically.
		if serr := acc.SaveAccount(first); serr != nil {
			logging.Warn("accmgr.pending_save_failed", "id", first.ID, "err", serr.Error())
		} else {
			logging.Info("accmgr.account_pending", "id", first.ID, "email", first.Email)
		}
		logging.Warn("accmgr.signup_no_credits", "id", first.ID, "err", creditErr.Error())
		return rec, fmt.Errorf("account created but %w: %v", errCreditsNotApproved, creditErr)
	}
	if err := acc.SaveAccount(first); err != nil {
		_ = acc.RemoveAccount(first.ID)
		return rec, fmt.Errorf("persist account: %w", err)
	}
	logging.Info("accmgr.signup_ok", "id", first.ID, "email", first.Email, "credits", rem)
	return first, nil
}

// Credit-gate timings are package vars so tests can shrink them.
var (
	creditWaitTotal = 2 * time.Minute
	creditPollEvery = 8 * time.Second
)

// waitForCredits polls the fresh account's balance until the server-side
// entitlement approval lands. New accounts can sit at 0 credits for a while
// after signup; accepting one would fill the pool with dead weight (the
// proxy would burn a chat attempt on an account that cannot answer). The
// account only joins the pool once it has credits.
func waitForCredits(ctx context.Context, acc *accio.Client, rec accio.TokenRecord) (int, error) {
	return pollCredits(ctx, func(c context.Context) (map[string]any, error) {
		return acc.CreditsFor(c, rec)
	})
}

func pollCredits(ctx context.Context, fetch func(context.Context) (map[string]any, error)) (int, error) {
	deadline := time.Now().Add(creditWaitTotal)
	var lastErr error
	for {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		credits, err := fetch(cctx)
		cancel()
		if err == nil {
			if rem := int(firstValueInt(credits, "remaining")); rem > 0 {
				return rem, nil
			}
			lastErr = nil
		} else {
			lastErr = err
		}
		if time.Now().Add(creditPollEvery).After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(creditPollEvery):
		}
	}
	if lastErr != nil {
		return 0, fmt.Errorf("still 0 after %s (last read error: %v)", creditWaitTotal, lastErr)
	}
	return 0, fmt.Errorf("still 0 after %s", creditWaitTotal)
}

// waitLogin returns a channel fed by the client's OnLogin bridge (the same
// one the app uses), filtered to the signup inbox address so an unrelated
// login (e.g. the user logging in manually mid-creation) cannot satisfy the
// pipeline with a foreign account.
func waitLogin(acc *accio.Client, wantEmail string) <-chan accio.TokenRecord {
	ch := make(chan accio.TokenRecord, 1)
	want := strings.ToLower(strings.TrimSpace(wantEmail))
	acc.OnLogin(func(t accio.TokenRecord) {
		if want != "" {
			email := strings.ToLower(strings.TrimSpace(t.Email))
			if email == "" {
				// Identity is not always resolved when the bridge fires;
				// resolve it now to decide whether this token is ours. The
				// legacy /safe/ endpoint is dead, so resolve via the v2
				// endpoint (email comes back server-masked, and may be empty
				// while the account is still being provisioned — retry).
				var masked string
				var rerr error
				for attempt := 0; attempt < 3; attempt++ {
					ictx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					masked, _, rerr = acc.UserInfoV2WithToken(ictx, t.AccessToken)
					cancel()
					if rerr == nil && strings.TrimSpace(masked) != "" {
						break
					}
					time.Sleep(time.Duration(400+attempt*400) * time.Millisecond)
				}
				switch {
				case rerr != nil || strings.TrimSpace(masked) == "":
					// Identity unverifiable — during automated creation the
					// login event is almost certainly ours. Dropping the
					// token here would kill the whole signup.
					logging.Warn("accmgr.waitlogin_accept_unverified", "err", fmt.Sprint(rerr), "masked", masked)
				case !maskMatch(masked, want):
					logging.Warn("accmgr.waitlogin_skip_foreign", "got", masked, "want", want)
					return
				default:
					// Store the real inbox email, not the masked form.
					t.Email = wantEmail
				}
			} else if email != want {
				logging.Warn("accmgr.waitlogin_skip_foreign", "got", email, "want", want)
				return
			}
		}
		select {
		case ch <- t:
		default:
		}
	})
	return ch
}

// maskMatch reports whether a server-masked email ("a*************0@emalupe.com")
// plausibly corresponds to the real address: same domain, same first and last
// visible local-part characters.
func maskMatch(masked, real string) bool {
	m := strings.SplitN(strings.ToLower(strings.TrimSpace(masked)), "@", 2)
	r := strings.SplitN(strings.ToLower(strings.TrimSpace(real)), "@", 2)
	if len(m) != 2 || len(r) != 2 || m[1] != r[1] {
		return false
	}
	ml, rl := m[0], r[0]
	if ml == "" || rl == "" {
		return false
	}
	if ml[0] != '*' && ml[0] != rl[0] {
		return false
	}
	if last := ml[len(ml)-1]; last != '*' && last != rl[len(rl)-1] {
		return false
	}
	return true
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
	// Human pacing: nobody reads a page, finds the field and acts in 700ms.
	// The whole signup used to complete in ~15s — inhumanly consistent.
	time.Sleep(1800*time.Millisecond + time.Duration(rand.Intn(2200))*time.Millisecond)
}

// runLoginPass drives one Chrome session through the signup flow: email form,
// verification code, OAuth redirect, token capture.
func runLoginPass(ctx context.Context, acc *accio.Client, profile string, inbox Inbox) (accio.TokenRecord, error) {
	loginCtx, cancelLogin := context.WithTimeout(ctx, 7*time.Minute)
	defer cancelLogin()

	loginURL, err := acc.StartLogin(loginCtx, 0)
	if err != nil {
		return accio.TokenRecord{}, fmt.Errorf("start login: %w", err)
	}

	recCh := waitLogin(acc, inbox.Address())

	// A previous attempt may have left a chrome holding the profile lock —
	// kill it before allocating, otherwise this attempt fails at startup.
	killChromeForProfile(profile)

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
	scrW, scrH := signupScreenRes()
	execOpts = append(execOpts, chromedp.WindowSize(int(scrW), int(scrH)))

	// Optional residential proxy for THIS browser session (see proxy.go): each
	// creation leaves through a fresh exit IP so signup velocity never heats a
	// single address.
	proxyURL := signupProxy()
	if proxyURL != nil {
		execOpts = append(execOpts, chromedp.Flag("proxy-server", proxyServerFlag(proxyURL)))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(loginCtx, execOpts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	// Optional network tracing (ACCIO_TRACE=1): logs OAuth-relevant requests
	// and redirects so exchange failures are diagnosable from the test log.
	var preRun []chromedp.Action

	// Proxy authentication: Chrome ignores credentials embedded in the
	// --proxy-server flag, so answer the auth challenge via the Fetch domain.
	if proxyURL != nil {
		if puser, ppass, ok := proxyCredentials(proxyURL); ok {
			preRun = append(preRun, fetch.Enable().WithHandleAuthRequests(true))
			logging.Info("accmgr.signup_proxy", "server", proxyURL.Host, "auth", "user:pass")
			chromedp.ListenTarget(browserCtx, func(ev any) {
				switch e := ev.(type) {
				case *fetch.EventAuthRequired:
					go func(reqID fetch.RequestID) {
						_ = chromedp.Run(browserCtx, fetch.ContinueWithAuth(reqID, &fetch.AuthChallengeResponse{
							Response: fetch.AuthChallengeResponseResponseProvideCredentials,
							Username: puser,
							Password: ppass,
						}))
					}(e.RequestID)
				case *fetch.EventRequestPaused:
					go func(reqID fetch.RequestID) {
						_ = chromedp.Run(browserCtx, fetch.ContinueRequest(reqID))
					}(e.RequestID)
				}
			})
		} else {
			logging.Info("accmgr.signup_proxy", "server", proxyURL.Host, "auth", "none")
		}
	}

	if os.Getenv("ACCIO_TRACE") == "1" {
		preRun = append(preRun, network.Enable())
		var urlMu sync.Mutex
		urlByReq := map[network.RequestID]string{}
		interestingReq := func(u string) bool {
			return strings.Contains(u, "oauth") || strings.Contains(u, "callback") || strings.Contains(u, "login.accio")
		}
		chromedp.ListenTarget(browserCtx, func(ev any) {
			switch e := ev.(type) {
			case *network.EventRequestWillBeSent:
				u := e.Request.URL
				if interestingReq(u) {
					urlMu.Lock()
					urlByReq[e.RequestID] = u
					urlMu.Unlock()
					logging.Info("accmgr.trace.req", "method", e.Request.Method, "url", u)
					if e.RedirectResponse != nil {
						logging.Info("accmgr.trace.redirect", "from", u, "status", e.RedirectResponse.Status, "location", e.RedirectResponse.Headers["Location"])
					}
					if e.Request.HasPostData {
						reqID := e.RequestID
						go func() {
							// Post data must be read from the browser target.
							var data []byte
							err := chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
								var err error
								data, err = network.GetRequestPostData(reqID).Do(cdp.WithExecutor(ctx, chromedp.FromContext(browserCtx).Target))
								return err
							}))
							if err == nil && len(data) > 0 {
								logging.Info("accmgr.trace.post", "url", u, "post", string(data))
							}
						}()
					}
				}
			case *network.EventResponseReceived:
				u := e.Response.URL
				if interestingReq(u) {
					logging.Info("accmgr.trace.resp", "status", e.Response.Status, "url", u)
				}
			case *network.EventLoadingFinished:
				urlMu.Lock()
				u, ok := urlByReq[e.RequestID]
				if ok {
					delete(urlByReq, e.RequestID)
				}
				urlMu.Unlock()
				if !ok || !strings.Contains(u, "oauth/code") {
					return
				}
				// Capture the issuance response body — a poisoned/flagged
				// code is only visible here.
				go func(reqID network.RequestID) {
					var body []byte
					err := chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
						var err error
						body, err = network.GetResponseBody(reqID).Do(cdp.WithExecutor(ctx, chromedp.FromContext(browserCtx).Target))
						return err
					}))
					if err != nil {
						logging.Warn("accmgr.trace.issuance_err", "url", u, "err", err.Error())
						return
					}
					if len(body) > 0 {
						if len(body) > 1200 {
							body = body[:1200]
						}
						logging.Info("accmgr.trace.issuance", "url", u, "body", string(body))
					}
				}(e.RequestID)
			}
		})
	}

	runErr := chromedp.Run(browserCtx,
		append(preRun,
			chromedp.ActionFunc(func(ctx context.Context) error {
				return emulation.SetDeviceMetricsOverride(scrW, scrH, 1, false).
					WithScreenWidth(scrW).
					WithScreenHeight(scrH).
					Do(cdp.WithExecutor(ctx, chromedp.FromContext(browserCtx).Target))
			}),
			chromedp.Navigate(loginURL),
			// Wait for either the email form or the existing-account prompt;
			// with a persistent profile the latter appears on every run
			// after the first — switch back to the email form then.
			chromedp.ActionFunc(func(ctx context.Context) error {
				return waitForText(ctx, []string{
					"Digite o endereço", "Enter your email", "endereço de e-mail", "email address",
					"conta existente", "existing account", "Você se inscreveu", "You signed up",
				})
			}),
			chromedp.ActionFunc(func(ctx context.Context) error {
				return ensureEmailForm(ctx)
			}),
		)...)

	if runErr == nil {
		runErr = chromedp.Run(browserCtx,
			// fresh profile: email form → submit → code screen
			chromedp.ActionFunc(func(ctx context.Context) error {
				return setReactInput(ctx, "form input", inbox.Address())
			}),
			chromedp.ActionFunc(func(ctx context.Context) error {
				humanDelay()
				// Mark the mailbox before the code email is sent so
				// WaitCode cannot pick up a stale code from an
				// earlier pass/attempt.
				inbox.Reset(ctx)
				return clickContinue(ctx)
			}),
			// Capture the page state after submitting the email: the
			// code screen, or the anti-fraud block, or an error. The
			// waitForText poll doubles as the "page settled" wait — no
			// fixed sleep needed.
			chromedp.ActionFunc(func(ctx context.Context) error {
				var text string
				_ = chromedp.Run(ctx, chromedp.Text("body", &text, chromedp.ByQuery))
				logging.Info("accmgr.page_after_email", "page", truncatePage(text))
				// The site sometimes presents a choice screen ("Entrar com
				// senha" | "Enviar código") instead of going straight to the
				// code form — pick the code path.
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
			// Capture the page state shortly after submitting the code so
			// failures (risk blocks, captchas) are visible in the logs.
			chromedp.ActionFunc(func(ctx context.Context) error {
				time.Sleep(1500 * time.Millisecond)
				var text string
				_ = chromedp.Run(ctx, chromedp.Text("body", &text, chromedp.ByQuery))
				logging.Info("accmgr.page_after_code", "page", truncatePage(text))
				return nil
			}),
		)
	}

	// Let the OAuth redirect + token exchange settle; the listener runs
	// inside the client regardless of CDP state. A rejected code (invalid_code
	// from server-side risk control) arrives on LoginErrors within seconds —
	// fail fast so the retry loop can start a fresh attempt immediately.
	if runErr != nil {
		// The flow died mid-page: dump what the page is actually showing
		// (captcha? risk block? stuck spinner?) plus a screenshot — blind
		// "timeout waiting for page text" is undiagnosable otherwise.
		dumpSignupFailure(browserCtx, runErr)
	}
	loginErrs := acc.LoginErrors()
	select {
	case rec := <-recCh:
		// Capture the browser session cookies (cna, _m_h5_tk, …) onto the
		// token record: downstream API calls (entitlement, refresh) are
		// WAF-checked against the session that issued the token.
		rec.Cookie = collectAccioCookies(browserCtx)
		return rec, nil
	case lerr := <-loginErrs:
		return accio.TokenRecord{}, fmt.Errorf("code exchange rejected: %w", lerr)
	case <-loginCtx.Done():
		if runErr != nil {
			return accio.TokenRecord{}, runErr
		}
		return accio.TokenRecord{}, errors.New("timeout waiting for token")
	case <-time.After(3 * time.Minute):
		if runErr != nil {
			return accio.TokenRecord{}, runErr
		}
		return accio.TokenRecord{}, errors.New("token did not arrive")
	}
}

// dumpSignupFailure logs the visible page text and saves a screenshot to the
// temp dir so a dead-end page (captcha, risk block, broken flow) can be
// diagnosed after the fact. Best-effort: the browser may already be gone.
func dumpSignupFailure(browserCtx context.Context, cause error) {
	var text string
	_ = chromedp.Run(browserCtx, chromedp.Text("body", &text, chromedp.ByQuery))
	var shot []byte
	_ = chromedp.Run(browserCtx, chromedp.CaptureScreenshot(&shot))
	shotPath := ""
	if len(shot) > 0 {
		shotPath = filepath.Join(os.TempDir(), fmt.Sprintf("accio-signup-fail-%d.png", time.Now().Unix()))
		if err := os.WriteFile(shotPath, shot, 0o600); err != nil {
			shotPath = ""
		}
	}
	logging.Warn("accmgr.signup_fail_page", "cause", cause.Error(), "page", truncatePage(text), "screenshot", shotPath)
}

// killChromeForProfile force-kills leftover chrome processes still bound to
// the persistent signup profile. After a timed-out attempt the browser can
// outlive its context and hold the profile lock, which makes the NEXT attempt
// fail with "chrome failed to start". Matches on the unique profile dir name
// so the user's real Chrome is never touched.
func killChromeForProfile(profile string) {
	name := filepath.Base(profile)
	if goruntime.GOOS == "windows" {
		ps := `Get-CimInstance Win32_Process -Filter "Name='chrome.exe'" | Where-Object { $_.CommandLine -like '*` + name + `*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`
		_ = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).Run()
		return
	}
	_ = exec.Command("pkill", "-f", "user-data-dir=.*"+name).Run()
}

// collectAccioCookies reads the browser's accio/alibaba session cookies into
// a Cookie-header string. With ACCIO_TRACE=1 the (truncated) set is logged so
// exchange failures stay diagnosable from the test log.
func collectAccioCookies(browserCtx context.Context) string {
	var cookies []*network.Cookie
	err := chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		cookies, err = storage.GetCookies().Do(cdp.WithExecutor(ctx, chromedp.FromContext(browserCtx).Target))
		return err
	}))
	if err != nil {
		logging.Warn("accmgr.cookies_err", "err", err.Error())
		return ""
	}
	var parts, traced []string
	for _, c := range cookies {
		if !strings.Contains(c.Domain, "accio") && !strings.Contains(c.Domain, "alibaba") {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
		if os.Getenv("ACCIO_TRACE") == "1" {
			v := c.Value
			if len(v) > 24 {
				v = v[:24] + "…"
			}
			traced = append(traced, fmt.Sprintf("%s[%s]=%s", c.Name, c.Domain, v))
		}
	}
	if os.Getenv("ACCIO_TRACE") == "1" {
		logging.Info("accmgr.trace.cookies", "cookies", strings.Join(traced, " | "))
	}
	return strings.Join(parts, "; ")
}

func truncatePage(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 220 {
		return s[:220] + "…"
	}
	return s
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
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errors.New("timeout waiting for page text")
}

// clickSendCodeIfPresent handles the "Entrar com senha | Enviar código"
// choice screen the site shows for emails it already knows: picks the code
// path with a real mouse click. No-op when the code form is already up or no
// such choice exists.
func clickSendCodeIfPresent(ctx context.Context) error {
	js := `(() => {
		const visible = e => e && e.offsetParent !== null;
		// Code form already visible? Nothing to do.
		const input = document.querySelector("form input");
		const labelRe = /c[oó]digo|code/i;
		if (visible(input)) {
			const lbl = input.closest("form") ? input.closest("form").textContent : "";
			if (/insira|enter|digite/i.test(lbl)) return "FORM_ALREADY";
		}
		const els = Array.from(document.querySelectorAll("button, a, span, div[role=button], [role=tab]")).filter(e => {
			if (!visible(e)) return false;
			const t = (e.textContent || "").trim();
			if (!t || t.length > 40) return false;
			if (!/enviar c[oó]digo|send( me)?( a)?( verification)? code/i.test(t)) return false;
			return !Array.from(e.querySelectorAll("button, a, span, [role=tab]")).some(c => /enviar c[oó]digo|send( me)?( a)?( verification)? code/i.test((c.textContent || "").trim()));
		});
		els.sort((a, b) => (a.textContent || "").trim().length - (b.textContent || "").trim().length);
		const el = els[0];
		if (!el) return "NO_SENDCODE";
		el.scrollIntoView({ block: "center" });
		const r = el.getBoundingClientRect();
		return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2, label: el.textContent.trim() });
	})()`
	var out string
	err := chromedp.Run(ctx, chromedp.Evaluate(js, &out, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithReturnByValue(true)
	}))
	if err != nil {
		return err
	}
	if out == "FORM_ALREADY" || out == "NO_SENDCODE" {
		return nil
	}
	var target struct {
		X     float64 `json:"x"`
		Y     float64 `json:"y"`
		Label string  `json:"label"`
	}
	if err := json.Unmarshal([]byte(out), &target); err != nil {
		return fmt.Errorf("parse send-code position: %w", err)
	}
	logging.Info("accmgr.click_sendcode", "action", "MOUSE_CLICK:"+target.Label)
	if err := chromedp.Run(ctx, chromedp.MouseClickXY(target.X, target.Y)); err != nil {
		return err
	}
	time.Sleep(800 * time.Millisecond)
	return nil
}

// ensureEmailForm detects the "continue with existing account?" prompt that a
// persistent profile triggers and clicks the "use another account" affordance
// to get back to the plain email form. No-op when the form is already shown.
func ensureEmailForm(ctx context.Context) error {
	// Detection runs in a short retry loop: the prompt renders asynchronously
	// and the first probe can land before the link exists.
	js := `(() => {
		const visible = e => e && e.offsetParent !== null;
		const input = document.querySelector("form input");
		if (visible(input)) return "FORM_OK";
		const re = /(use another|sign in with another|another account|outra conta|trocar de conta|mudar de conta|switch account|different account|entrar com outra|usar outra|not you|não é você)/i;
		// Match only leaf-ish elements: containers concatenate the prompt and
		// the link text ("ContinuarOu, Entre agora com outra conta"), and
		// clicking the container triggers the DEFAULT continue action instead
		// of the switch. Prefer the deepest element with the shortest text.
		const els = Array.from(document.querySelectorAll("a, button, span, div, p")).filter(e => {
			if (!visible(e)) return false;
			const t = (e.textContent || "").trim();
			if (!t || t.length > 80 || !re.test(t)) return false;
			// no matching descendant → this is the innermost carrier
			return !Array.from(e.querySelectorAll("a, button, span, div, p")).some(c => re.test((c.textContent || "").trim()));
		});
		els.sort((a, b) => (a.textContent || "").trim().length - (b.textContent || "").trim().length);
		const el = els[0];
		if (!el) return "NO_SWITCH";
		el.scrollIntoView({ block: "center" });
		const r = el.getBoundingClientRect();
		return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2, label: el.textContent.trim() });
	})()`
	var out string
	deadline := time.Now().Add(8 * time.Second)
	for {
		out = ""
		if err := chromedp.Run(ctx, chromedp.Evaluate(js, &out, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithReturnByValue(true)
		})); err != nil {
			return err
		}
		if out != "NO_SWITCH" || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	if out == "FORM_OK" {
		return nil
	}
	if out == "NO_SWITCH" {
		// The prompt never rendered a switch link, or the site changed shape;
		// let the email-form wait below produce the diagnostic.
		logging.Warn("accmgr.ensure_email_form_no_switch")
	} else {
		// Trusted mouse click (anti-bot entropy): a synthetic el.click() or
		// direct React onClick call carries no input-event trust signals.
		var target struct {
			X     float64 `json:"x"`
			Y     float64 `json:"y"`
			Label string  `json:"label"`
		}
		if err := json.Unmarshal([]byte(out), &target); err != nil {
			return fmt.Errorf("parse switch-account position: %w", err)
		}
		logging.Info("accmgr.ensure_email_form", "action", "MOUSE_CLICK:"+target.Label)
		if err := chromedp.Run(ctx, chromedp.MouseClickXY(target.X, target.Y)); err != nil {
			return err
		}
	}
	// Wait until the email input is actually visible.
	formDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(formDeadline) {
		var ok bool
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
			const i = document.querySelector("form input");
			return !!i && i.offsetParent !== null;
		})()`, &ok, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithReturnByValue(true)
		}))
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errors.New("email form did not appear")
}

// setReactInput types a value into the form input using TRUSTED input events
// (real click to focus, real per-key dispatch). Plain JS value-setters produce
// zero keyboard/mouse entropy and the Alibaba WAF scores the session as a
// bot, which gets the issued OAuth code poisoned (invalid_code / NOT_LOGIN
// tokens). Slower than JS injection, but the session stays human-looking.
func setReactInput(ctx context.Context, selector, value string) error {
	// Type like a human: focus with a real click, then one trusted key event
	// per character with 60-180ms randomized intervals. A whole-string
	// SendKeys bursts all keys in <10ms — a timing signature no human has.
	if err := chromedp.Run(ctx,
		chromedp.Click(selector, chromedp.NodeVisible),
		chromedp.Sleep(150*time.Millisecond),
	); err != nil {
		return fmt.Errorf("focus %s: %w", selector, err)
	}
	for _, ch := range value {
		if err := chromedp.Run(ctx, chromedp.SendKeys(selector, string(ch), chromedp.NodeVisible)); err != nil {
			return fmt.Errorf("type into %s: %w", selector, err)
		}
		time.Sleep(60*time.Millisecond + time.Duration(rand.Intn(120))*time.Millisecond)
	}
	// Verify the value actually landed (React keeps the input controlled).
	var out string
	js := fmt.Sprintf(`(() => { const i = document.querySelector(%q); return i ? i.value : ""; })()`, selector)
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &out, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithReturnByValue(true)
	})); err != nil {
		return err
	}
	if out != value {
		return fmt.Errorf("input value mismatch: got %q want %q", out, value)
	}
	return nil
}

// clickContinue triggers the form's primary button with a real mouse click
// (move + press + release at the button's coordinates) instead of calling the
// React handler directly — same anti-bot reasoning as setReactInput.
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
		const r = btn.getBoundingClientRect();
		btn.scrollIntoView({ block: "center" });
		const r2 = btn.getBoundingClientRect();
		return JSON.stringify({ x: r2.x + r2.width / 2, y: r2.y + r2.height / 2, label: btn.textContent.trim() });
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
	var target struct {
		X     float64 `json:"x"`
		Y     float64 `json:"y"`
		Label string  `json:"label"`
	}
	if err := json.Unmarshal([]byte(out), &target); err != nil {
		return fmt.Errorf("parse button position: %w", err)
	}
	logging.Info("accmgr.click", "action", "MOUSE_CLICK:"+target.Label)
	// Move the mouse over the button first (hover), then press/release.
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := input.DispatchMouseEvent(input.MouseMoved, target.X-40, target.Y-20).Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Target)); err != nil {
			return err
		}
		time.Sleep(120 * time.Millisecond)
		return input.DispatchMouseEvent(input.MouseMoved, target.X, target.Y).Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Target))
	})); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	if err := chromedp.Run(ctx, chromedp.MouseClickXY(target.X, target.Y)); err != nil {
		return err
	}
	time.Sleep(800 * time.Millisecond)
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
// signupScreenRes picks a realistic desktop resolution per attempt. Headless
// Chrome's virtual screen defaults to 800x600 — a resolution no real user has
// and a loud bot signal (it shows up in GA collect as sr=800x600, which the
// risk engine sees). The window flag alone does NOT change screen.width in
// headless; the emulation override below does.
func signupScreenRes() (int64, int64) {
	res := [][2]int64{{1920, 1080}, {1366, 768}, {1536, 864}, {1440, 900}, {1600, 900}}
	r := res[rand.Intn(len(res))]
	return r[0], r[1]
}

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
