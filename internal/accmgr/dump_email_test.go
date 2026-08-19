package accmgr

// Email body dump probe. Skipped unless ACCIO_DUMP_EMAIL=1. Runs one real
// signup, then prints the FULL body (text + HTML) of every message in the
// disposable inbox — the site sends needMagicLink=true, so the email may
// carry a magic link that completes login with real tokens.
//
//	ACCIO_DUMP_EMAIL=1 go test ./internal/accmgr -run TestDumpEmail -v -timeout 15m

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"grok-desktop/internal/accio"
)

func TestDumpEmail(t *testing.T) {
	if os.Getenv("ACCIO_DUMP_EMAIL") == "" {
		t.Skip("set ACCIO_DUMP_EMAIL=1 to run the email dump probe")
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
	loginCtx, cancelLogin := context.WithTimeout(ctx, 8*time.Minute)
	defer cancelLogin()
	loginURL, err := acc.StartLogin(loginCtx, 0)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	execOpts := []chromedp.ExecAllocatorOption{
		chromedp.UserDataDir(t.TempDir()),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.DisableGPU,
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
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
	run(chromedp.Navigate(loginURL))
	run(chromedp.ActionFunc(func(ctx context.Context) error {
		return waitForText(ctx, []string{"Digite o endereço", "Enter your email", "email address"})
	}))
	run(chromedp.ActionFunc(func(ctx context.Context) error {
		return setReactInput(ctx, "form input", inbox.Address())
	}))
	humanDelay()
	run(chromedp.ActionFunc(func(ctx context.Context) error { return clickContinue(ctx) }))

	// Wait for the code email, then dump everything.
	code, err := inbox.WaitCode(loginCtx)
	if err != nil {
		t.Fatalf("wait code: %v", err)
	}
	t.Logf("[email code] %s", code)

	if tm, ok := inbox.(*MailTM); ok {
		var msgs []struct {
			ID string `json:"id"`
		}
		if err := getJSON(ctx, tm.http, mailTmBase+"/messages", map[string]string{
			"Authorization": "Bearer " + tm.token,
		}, &msgs); err != nil {
			t.Fatalf("list messages: %v", err)
		}
		for _, m := range msgs {
			var full map[string]any
			if err := getJSON(ctx, tm.http, mailTmBase+"/messages/"+m.ID, map[string]string{
				"Authorization": "Bearer " + tm.token,
			}, &full); err != nil {
				continue
			}
			t.Logf("[email %s] subject=%v", m.ID, full["subject"])
			if txt, ok := full["text"].(string); ok {
				t.Logf("[email text]\n%s", txt)
			}
			if html, ok := full["html"].([]any); ok {
				for _, h := range html {
					if s, ok := h.(string); ok {
						// Extract links for readability.
						for _, line := range strings.Split(s, "\"") {
							if strings.HasPrefix(line, "http") {
								t.Logf("[link] %s", line)
							}
						}
						t.Logf("[email html]\n%s", truncateStr(s, 3000))
					}
				}
			}
		}
	} else {
		t.Logf("inbox is %T, not mail.tm — cannot dump body", inbox)
	}
	fmt.Println("done")
}
