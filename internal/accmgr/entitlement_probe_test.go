package accmgr

// Standalone entitlement probe. Gated by ACCIO_PROBE=1 — drives ONE real
// signup, then fires a matrix of request-shape variants at the entitlement
// endpoint with the fresh token to find which shape the server accepts:
//
//	ACCIO_PROBE=1 go test ./internal/accmgr -run TestEntitlementProbe -v -timeout 15m
//
// Context: fresh-account tokens pass /api/auth/userinfo but the entitlement
// GET answers {"success":false,"code":"401","message":"NOT_LOGIN"} for our
// desktop-headers shape, while the official app's (headerless per sniff) GET
// succeeds seconds after exchange. Something about the request shape decides.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"grok-desktop/internal/accio"
)

func TestEntitlementProbe(t *testing.T) {
	if os.Getenv("ACCIO_PROBE") == "" {
		t.Skip("set ACCIO_PROBE=1 to run the entitlement probe")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	acc, err := accio.New(dataDir)
	if err != nil {
		t.Fatalf("accio.New: %v", err)
	}
	inbox, err := NewInbox(ctx)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}

	// Retry through server-side code poisoning (invalid_code 90001) — fresh
	// inbox + fresh profile each attempt, same as signupFlow.
	var rec accio.TokenRecord
	for attempt := 1; attempt <= 5; attempt++ {
		if attempt > 1 {
			inbox, err = NewInbox(ctx)
			if err != nil {
				t.Fatalf("inbox: %v", err)
			}
			time.Sleep(5 * time.Second)
		}
		profile := signupProfileDir() // persistent: ages the WAF cookies
		rec, err = runLoginPass(ctx, acc, profile, inbox)
		if err == nil {
			break
		}
		t.Logf("attempt %d: %v", attempt, err)
	}
	if err != nil {
		t.Fatalf("signup pass: %v", err)
	}
	t.Logf("token: id=%s access=%s… cookie=%d chars", rec.ID, rec.AccessToken[:8], len(rec.Cookie))

	cna := ""
	for _, kv := range strings.Split(rec.Cookie, "; ") {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "cna" {
			cna = v
		}
	}
	t.Logf("browser cna=%q", cna)

	base := "https://phoenix-gw.alibaba.com/api/entitlement/currentSubscription"
	type variant struct {
		name    string
		headers map[string]string
	}
	variants := []variant{
		{"bare (sniff shape)", map[string]string{}},
		{"desktop no cookies", map[string]string{"x-language": "pt", "x-locale": "pt-BR", "x-platform": "pc-win", "x-app-version": "0.29.1", "x-os": "win32", "x-deploy-target": "desktop", "x-source": "ACCIO_DESKTOP"}},
		{"desktop+empty-xcna", map[string]string{"x-language": "pt", "x-locale": "pt-BR", "x-platform": "pc-win", "x-app-version": "0.29.1", "x-os": "win32", "x-deploy-target": "desktop", "x-source": "ACCIO_DESKTOP", "x-cna": ""}},
		{"desktop+cookies", map[string]string{"x-language": "pt", "x-locale": "pt-BR", "x-platform": "pc-win", "x-app-version": "0.29.1", "x-os": "win32", "x-deploy-target": "desktop", "x-source": "ACCIO_DESKTOP", "Cookie": rec.Cookie}},
		{"desktop+xcna-real", map[string]string{"x-language": "pt", "x-locale": "pt-BR", "x-platform": "pc-win", "x-app-version": "0.29.1", "x-os": "win32", "x-deploy-target": "desktop", "x-source": "ACCIO_DESKTOP", "x-cna": cna, "Cookie": rec.Cookie}},
		{"bearer only", map[string]string{"Authorization": "Bearer " + rec.AccessToken}},
		{"ua-accio", map[string]string{"User-Agent": "Accio/0.29.1"}},
	}
	for _, v := range variants {
		u := fmt.Sprintf("%s?accessToken=%s&subscripType=INDIVIDUAL&utdid=%s&version=0.29.1", base, rec.AccessToken, acc.Utdid())
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		for k, hv := range v.headers {
			req.Header.Set(k, hv)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Logf("%-20s transport error: %v", v.name, err)
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
		resp.Body.Close()
		s := string(raw)
		if len(s) > 240 {
			s = s[:240] + "…"
		}
		t.Logf("%-20s HTTP %d body=%s", v.name, resp.StatusCode, s)
		time.Sleep(300 * time.Millisecond)
	}
}
