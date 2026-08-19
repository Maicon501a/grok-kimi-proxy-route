package accmgr

// Warmed-device signup probe. Gated by ACCIO_WARM_SIGNUP=1.
//
// Sequence under test: warm the proxy's utdid with SecurityGuard-signed
// featureFlag calls (mimicking an official app launch) BEFORE the browser
// signup and code exchange, so the exchanging device is already known to the
// Baxia/SG layer when the token is issued. Then the standard flow runs and
// the entitlement is read. If the account lands entitled, device pre-warming
// is the bypass for the NOT_LOGIN gate and gets integrated into signupFlow.
//
//	ACCIO_WARM_SIGNUP=1 go test ./internal/accmgr -run TestLiveSignupWarmed -v -timeout 15m

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"grok-desktop/internal/accio"
)

func TestLiveSignupWarmed(t *testing.T) {
	if os.Getenv("ACCIO_WARM_SIGNUP") == "" {
		t.Skip("set ACCIO_WARM_SIGNUP=1 to run a warmed-device real signup")
	}
	dataDir := filepath.Join(os.TempDir(), "accio-live-probe-data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("probe data dir: %v", err)
	}
	acc, err := accio.New(dataDir)
	if err != nil {
		t.Fatalf("accio.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	// Phase 1: device warm-up (official-app launch chatter).
	warmCtx, warmCancel := context.WithTimeout(ctx, 2*time.Minute)
	acc.WarmDevice(warmCtx)
	warmCancel()

	// Phase 2: standard signup flow.
	opts := signupOptions{
		warp:     os.Getenv("ACCIO_USE_WARP") == "1",
		headless: os.Getenv("ACCIO_SIGNUP_VISIBLE") != "1",
	}
	start := time.Now()
	rec, err := signupFlow(ctx, acc, opts)
	elapsed := time.Since(start).Round(time.Second)
	if err != nil {
		t.Fatalf("warmed signup FAILED after %s: %v", elapsed, err)
	}

	// Phase 3: entitlement.
	cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
	credits, cerr := acc.CreditsFor(cctx, rec)
	ccancel()
	if cerr != nil {
		t.Fatalf("warmed signup created %s but entitlement still rejected: %v", rec.ID, cerr)
	}
	t.Logf("WARMED SIGNUP ENTITLED in %s — id=%s email=%s remaining=%v total=%v",
		elapsed, rec.ID, rec.Email, credits["remaining"], credits["total"])
}
