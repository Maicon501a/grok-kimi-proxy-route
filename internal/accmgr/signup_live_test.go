package accmgr

// Live end-to-end signup probe. Skipped unless ACCIO_LIVE_SIGNUP=1 — it
// drives a real Chrome through the real Accio site with a disposable inbox
// and creates a real account. Use it to prove creation reliability and to
// measure per-creation time and granted credits:
//
//	ACCIO_LIVE_SIGNUP=1 go test ./internal/accmgr -run TestLiveSignup -v -timeout 30m
//
// ACCIO_LIVE_SIGNUP=N runs N consecutive creations and reports the success
// rate, per-creation duration and the credits each account landed with.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"grok-desktop/internal/accio"
)

func TestLiveSignup(t *testing.T) {
	if os.Getenv("ACCIO_LIVE_SIGNUP") == "" {
		t.Skip("set ACCIO_LIVE_SIGNUP=1 to run a real account creation")
	}
	runs := 1
	if v := os.Getenv("ACCIO_LIVE_SIGNUP"); v != "1" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			runs = n
		}
	}

	// Persistent probe dir (NOT t.TempDir): accounts created here survive the
	// test run so their credits can be re-checked later — a locally-failed
	// entitlement read (socket errors, transient blocks) must not throw away
	// a successfully created account.
	dataDir := filepath.Join(os.TempDir(), "accio-live-probe-data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("probe data dir: %v", err)
	}
	t.Logf("probe data dir: %s", dataDir)
	acc, err := accio.New(dataDir)
	if err != nil {
		t.Fatalf("accio.New: %v", err)
	}

	opts := signupOptions{
		warp:     os.Getenv("ACCIO_USE_WARP") == "1",
		headless: os.Getenv("ACCIO_SIGNUP_VISIBLE") != "1",
	}

	var successes int
	for i := 0; i < runs; i++ {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
		rec, err := signupFlow(ctx, acc, opts)
		cancel()
		elapsed := time.Since(start).Round(time.Second)
		if err != nil {
			t.Logf("run %d/%d: FAILED after %s: %v", i+1, runs, elapsed, err)
			continue
		}
		successes++
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		credits, cerr := acc.CreditsFor(cctx, rec)
		ccancel()
		rem, tot := 0, 0
		if cerr == nil {
			rem = int(firstValueInt(credits, "remaining"))
			tot = int(firstValueInt(credits, "total"))
		}
		t.Logf("run %d/%d: OK in %s — id=%s email=%s remaining=%d total=%d (credits err: %v)",
			i+1, runs, elapsed, rec.ID, rec.Email, rem, tot, cerr)
	}
	t.Logf("RESULT: %d/%d successful creations", successes, runs)
	if successes == 0 {
		t.Fatal("no account was created successfully")
	}
}
