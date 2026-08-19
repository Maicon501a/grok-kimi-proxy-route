package accio

// Invitation-gate probe. Gated by ACCIO_INVITE_PROBE=1.
//
// The official app ships an activation screen: "You currently have no access.
// The product is in internal testing. Enter an invitation code to try it."
// — accounts can exist WITHOUT product access. Hypothesis: our NOT_LOGIN
// accounts are unactivated, not risk-flagged. This probe asks
// /api/invitation/query for every saved pending account. An "activated:false"
// answer (instead of 401 NOT_LOGIN) proves the gate is activation and the
// bypass is an invitation code committed via /api/invitation/commit.
//
//	ACCIO_INVITE_PROBE=1 go test ./internal/accio -run TestInvitationProbe -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInvitationProbe(t *testing.T) {
	if os.Getenv("ACCIO_INVITE_PROBE") == "" {
		t.Skip("set ACCIO_INVITE_PROBE=1 to probe the invitation gate")
	}
	dataDir := filepath.Join(os.TempDir(), "accio-live-probe-data")
	c, err := New(dataDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	accounts := c.Accounts()
	if len(accounts) == 0 {
		t.Fatal("no saved accounts")
	}

	// Also fetch the entitlement config (diamondConfig) — may describe the
	// grant rules for new/invited accounts.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	call := func(endpoint, token, cookie string) {
		// invitation endpoints are POST with the token in the JSON body,
		// same contract as /api/auth/userinfo.
		body, _ := json.Marshal(map[string]string{
			"utdid": c.utdid, "version": c.appVersion, "accessToken": token,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		c.desktopHeaders(req)
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			t.Logf("%s -> %v", endpoint, err)
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1200))
		t.Logf("%s -> HTTP %d: %s", endpoint, resp.StatusCode, truncateBytes(raw, 500))
	}

	call(GatewayBase+"/entitlement/config", "", "")
	for _, a := range accounts {
		t.Logf("--- %s (%s)", a.ID, a.Email)
		call(GatewayBase+"/invitation/query", a.AccessToken, a.Cookie)
		time.Sleep(2 * time.Second)
	}
	fmt.Println("done")
}
