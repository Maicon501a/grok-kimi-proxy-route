package accmgr

// Credits re-check probe. Gated by ACCIO_RECHECK=1 — does NOT create anything:
// reads every account saved in the persistent probe dir and reports its
// current entitlement. Use it after a live run whose credits read failed
// locally, or to watch pending (NOT_LOGIN) accounts get approved later:
//
//	ACCIO_RECHECK=1 go test ./internal/accmgr -run TestLiveRecheck -v

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"grok-desktop/internal/accio"
)

func TestLiveRecheck(t *testing.T) {
	if os.Getenv("ACCIO_RECHECK") == "" {
		t.Skip("set ACCIO_RECHECK=1 to re-check saved probe accounts")
	}
	dataDir := filepath.Join(os.TempDir(), "accio-live-probe-data")
	acc, err := accio.New(dataDir)
	if err != nil {
		t.Fatalf("accio.New: %v", err)
	}
	accounts := acc.Accounts()
	if len(accounts) == 0 {
		t.Fatalf("no saved accounts in %s", dataDir)
	}
	approved := 0
	for _, a := range accounts {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		credits, err := acc.CreditsFor(ctx, a)
		cancel()
		if err != nil {
			t.Logf("%s (%s): credits err: %v", a.ID, a.Email, err)
			continue
		}
		rem := int(firstValueInt(credits, "remaining"))
		tot := int(firstValueInt(credits, "total"))
		t.Logf("%s (%s): remaining=%d total=%d", a.ID, a.Email, rem, tot)
		if rem > 0 {
			approved++
		}
	}
	t.Logf("RESULT: %d/%d accounts with credits", approved, len(accounts))
}
