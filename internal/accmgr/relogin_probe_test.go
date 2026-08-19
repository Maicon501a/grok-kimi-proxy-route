package accmgr

// Re-login ceremony probe. Gated by ACCIO_RELOGIN=1.
//
// Hypothesis: a fresh account's NOT_LOGIN may lift after a real LOGIN event
// (not the signup's OAuth exchange) — mirroring the 15:19 success where the
// official APP performed a full login for the account. This probe:
//  1. creates an account (expected to land pending, with persisted inbox
//     credentials),
//  2. waits, then reopens the same disposable inbox,
//  3. drives a fresh browser LOGIN for the same email (code path),
//  4. re-reads the entitlement with the new token.
//
// If the account flips to entitled, the bypass is: signup → re-login ceremony
// → entitled account.
//
//	ACCIO_RELOGIN=1 go test ./internal/accmgr -run TestReloginCeremony -v -timeout 20m

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"grok-desktop/internal/accio"
)

func TestReloginCeremony(t *testing.T) {
	if os.Getenv("ACCIO_RELOGIN") == "" {
		t.Skip("set ACCIO_RELOGIN=1 to run the re-login ceremony probe")
	}
	dataDir := filepath.Join(os.TempDir(), "accio-live-probe-data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("probe data dir: %v", err)
	}
	acc, err := accio.New(dataDir)
	if err != nil {
		t.Fatalf("accio.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()

	// Phase 1: create (lands pending with inbox creds attached).
	opts := signupOptions{warp: false, headless: os.Getenv("ACCIO_SIGNUP_VISIBLE") != "1"}
	before := map[string]bool{}
	for _, a := range acc.Accounts() {
		before[a.ID] = true
	}
	_, _ = signupFlow(ctx, acc, opts) // failure expected (NOT_LOGIN); record is saved

	var pending *accio.TokenRecord
	for i, a := range acc.Accounts() {
		if !before[a.ID] && a.InboxSecret != "" {
			pending = &acc.Accounts()[i]
			break
		}
	}
	if pending == nil {
		t.Fatal("no new account with inbox credentials was saved")
	}
	t.Logf("pending account: %s email=%s provider=%s", pending.ID, pending.Email, pending.InboxProvider)

	// Phase 2: settle.
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(90 * time.Second):
	}

	// Phase 3: re-login with the same email via a fresh browser session.
	inbox, err := ReopenInbox(ctx, pending.InboxProvider, pending.Email, pending.InboxSecret)
	if err != nil {
		t.Fatalf("reopen inbox: %v", err)
	}
	profile := signupProfileDir()
	if os.Getenv("ACCIO_FRESH_PROFILE") == "1" {
		defer os.RemoveAll(profile)
	}
	rec2, err := runLoginPass(ctx, acc, profile, inbox)
	if err != nil {
		t.Fatalf("re-login pass failed: %v", err)
	}
	t.Logf("re-login token captured: id=%s", rec2.ID)

	// Phase 4: entitlement with the fresh token.
	cctx, ccancel := context.WithTimeout(ctx, 2*time.Minute)
	defer ccancel()
	rem, err := waitForCredits(cctx, acc, rec2)
	if err != nil {
		t.Fatalf("entitlement still rejected after re-login ceremony: %v", err)
	}
	t.Logf(">>> RE-LOGIN CEREMONY FLIPPED THE ACCOUNT! remaining=%d", rem)
	rec2.InboxProvider = pending.InboxProvider
	rec2.InboxSecret = pending.InboxSecret
	if err := acc.SaveAccount(rec2); err != nil {
		t.Logf("save flipped account: %v", err)
	}
}
