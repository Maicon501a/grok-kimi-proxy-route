package accio

// Device-warming live probe. Gated by ACCIO_DEVICE_WARM=1.
//
// Hypothesis: the entitlement gate (NOT_LOGIN) may require the DEVICE (utdid)
// to have passed the SecurityGuard/Baxia layer — i.e. to look like an
// initialized official app. At 15:19 the entitled account was exchanged AND
// entitlement-checked by the real app (a SG-initialized device). Our client
// never performs any SG-signed call before entitlement.
//
// This probe warms the device like an app launch — featureFlag/evaluate and
// the model catalog with pctb-x-* headers — then re-reads the entitlement of
// every pending account saved in the probe data dir. If any flips from
// NOT_LOGIN to 200, device trust is the gate and warming is the bypass.
//
//	ACCIO_DEVICE_WARM=1 go test ./internal/accio -run TestDeviceWarmLive -v

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeviceWarmLive(t *testing.T) {
	if os.Getenv("ACCIO_DEVICE_WARM") == "" {
		t.Skip("set ACCIO_DEVICE_WARM=1 to run the device-warming probe")
	}
	if securityGuardBundleDir() == "" {
		t.Skip("security-guard bundle not available")
	}
	dataDir := filepath.Join(os.TempDir(), "accio-live-probe-data")
	c, err := New(dataDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	warm := func(target string, body string) {
		sec, err := c.securityHeaders(ctx, target)
		if err != nil {
			t.Logf("warm %s: securityHeaders err: %v", target, err)
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(body))
		if err != nil {
			t.Fatalf("warm request: %v", err)
		}
		c.desktopHeaders(req)
		for k, v := range sec {
			req.Header.Set(k, v)
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			t.Logf("warm %s: %v", target, err)
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
		t.Logf("warm %s -> HTTP %d: %s", target, resp.StatusCode, truncateBytes(raw, 300))
	}

	// App-launch sequence, spaced out like real startup chatter.
	for i := 0; i < 3; i++ {
		warm(GatewayBase+"/tool/featureFlag/evaluate", "{}")
		time.Sleep(4 * time.Second)
	}
	warm(GatewayBase+"/tool/featureFlag/evaluate", `{"keys":["chat.models","signup.referral"]}`)

	// Now re-read entitlement for every saved (pending) account.
	accounts := c.Accounts()
	if len(accounts) == 0 {
		t.Fatalf("no saved accounts in %s", dataDir)
	}
	flipped := 0
	for _, a := range accounts {
		cctx, ccancel := context.WithTimeout(ctx, 30*time.Second)
		credits, err := c.CreditsFor(cctx, a)
		ccancel()
		if err != nil {
			t.Logf("%s: still rejected: %v", a.ID, truncateBytes([]byte(err.Error()), 160))
			continue
		}
		t.Logf("%s: ENTITLED! remaining=%d total=%d", a.ID, firstInt64(credits, "remaining"), firstInt64(credits, "total"))
		flipped++
	}
	t.Logf("RESULT: %d/%d accounts flipped to entitled after warming", flipped, len(accounts))
}
