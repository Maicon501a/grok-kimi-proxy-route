package accmgr

// Live test: drive the website signup with the OFFICIAL app's own login URL
// (obtained from its local gateway), so the app itself performs the OAuth
// token exchange through the MITM proxy. This captures the exact working
// exchange request/response contract.
//
// Requirements:
//   - Accio 0.29.1 running with --proxy-server=127.0.0.1:8888 and gateway on :4097
//   - MITM proxy running on 127.0.0.1:8888 writing %TEMP%/mitm.log
//
// Run: ACCIO_APP_SIGNUP=1 ACCIO_GW_AUTH="Basic cGhv..." go test ./internal/accmgr -run TestAppSignupExchange -v -timeout 10m
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestAppSignupExchange(t *testing.T) {
	if os.Getenv("ACCIO_APP_SIGNUP") != "1" {
		t.Skip("set ACCIO_APP_SIGNUP=1 to run")
	}
	gwAuth := os.Getenv("ACCIO_GW_AUTH")
	if gwAuth == "" {
		t.Fatal("set ACCIO_GW_AUTH to the renderer's Authorization header")
	}
	gwBase := os.Getenv("ACCIO_GW_BASE")
	if gwBase == "" {
		gwBase = "http://127.0.0.1:4097"
	}

	// 1. Ask the official app to start a login (account_switch mode).
	req, _ := http.NewRequest(http.MethodPost, gwBase+"/auth/login", strings.NewReader(`{"mode":"account_switch"}`))
	req.Header.Set("Authorization", gwAuth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-source", "ACCIO_DESKTOP")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway /auth/login: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("gateway /auth/login -> %d %s", resp.StatusCode, string(body))
	var lr struct {
		LoginURL string `json:"loginUrl"`
	}
	if err := json.Unmarshal(body, &lr); err != nil || lr.LoginURL == "" {
		t.Fatalf("no loginUrl in response: %s", string(body))
	}
	loginURL := lr.LoginURL

	// 2. Temp inbox for the new account.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	inbox, err := NewInbox(ctx)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	t.Logf("inbox: %s", inbox.Address())

	// 3. Drive the website signup in an automated Chrome.
	profile := t.TempDir()
	execOpts := []chromedp.ExecAllocatorOption{
		chromedp.UserDataDir(profile),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.DisableGPU,
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("disable-features", "FedCm"),
		chromedp.Flag("lang", "pt-BR"),
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, execOpts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	runErr := chromedp.Run(browserCtx,
		chromedp.Navigate(loginURL),
		chromedp.ActionFunc(func(ctx context.Context) error {
			return waitForText(ctx, []string{
				"Digite o endereço", "Enter your email", "endereço de e-mail", "email address",
				"conta existente", "existing account",
			})
		}),
	)
	if runErr == nil {
		runErr = chromedp.Run(browserCtx,
			chromedp.ActionFunc(func(ctx context.Context) error {
				return setReactInput(ctx, "form input", inbox.Address())
			}),
			chromedp.ActionFunc(func(ctx context.Context) error {
				humanDelay()
				return clickContinue(ctx)
			}),
			chromedp.ActionFunc(func(ctx context.Context) error {
				var text string
				_ = chromedp.Run(ctx, chromedp.Text("body", &text, chromedp.ByQuery))
				t.Logf("page after email: %s", truncatePage(text))
				return waitForText(ctx, []string{"verification code", "código enviado", "Insira o código", "Enter the code"})
			}),
			chromedp.ActionFunc(func(ctx context.Context) error {
				code, err := inbox.WaitCode(ctx)
				if err != nil {
					return err
				}
				t.Logf("got code")
				humanDelay()
				return setReactInput(ctx, "form input", code)
			}),
			chromedp.ActionFunc(func(ctx context.Context) error {
				humanDelay()
				return clickContinue(ctx)
			}),
		)
	}
	if runErr != nil {
		var text string
		_ = chromedp.Run(browserCtx, chromedp.Text("body", &text, chromedp.ByQuery))
		t.Logf("signup automation error: %v; page: %s", runErr, truncatePage(text))
	}

	// 4. The website redirects to the app gateway callback; the app then does
	// the token exchange via phoenix-gw through the MITM. Wait and watch.
	t.Log("waiting for the app to perform the token exchange (watch mitm.log)...")
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(os.TempDir() + "/mitm.log"); err == nil {
			if strings.Contains(string(data), "/api/oauth/token") {
				t.Log("EXCHANGE CAPTURED!")
				break
			}
		}
		// Also log current page URL for visibility.
		var cur string
		_ = chromedp.Run(browserCtx, chromedp.Evaluate(`location.href`, &cur))
		if cur != "" {
			t.Logf("page now at: %s", cur)
			if strings.Contains(cur, "/auth/callback") {
				t.Log("callback URL reached the gateway")
			}
		}
		time.Sleep(5 * time.Second)
	}

	// 5. Dump the captured exchange from the MITM log.
	data, _ := os.ReadFile(os.TempDir() + "/mitm.log")
	content := string(data)
	idx := strings.Index(content, "/api/oauth/token")
	if idx < 0 {
		t.Fatal("no /api/oauth/token call captured — check mitm.log")
	}
	start := strings.LastIndex(content[:idx], ">>>")
	end := strings.Index(content[idx:], "=== CONNECT")
	if end < 0 {
		end = len(content) - idx
	}
	fmt.Printf("\n===== CAPTURED EXCHANGE =====\n%s\n===== END =====\n", content[start:idx+end])
}
