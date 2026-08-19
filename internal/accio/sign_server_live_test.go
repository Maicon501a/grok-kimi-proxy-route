package accio

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestSignServerLive sends one real request to the phoenix gateway using a
// Go-generated pctb-x-sign (session body captured from the native addon via
// the daemon) while keeping every other header from the addon (umt,
// mini-wua, etc). The URL is a low-risk feature-flag endpoint.
//
// Expected outcomes:
//   - 2xx / 4xx business response  → the WAF accepted the Go sign.
//   - 403 / sufei-punish captcha   → the server validates something the
//     generator does not replicate yet.
//
// Skipped when the native addon is unavailable.
func TestSignServerLive(t *testing.T) {
	if securityGuardBundleDir() == "" {
		t.Skip("security-guard bundle not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	dataDir := t.TempDir()
	c, err := New(dataDir)
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	target := GatewayBase + "/tool/featureFlag/evaluate"
	if c.gatewayLLM == "" {
		c.gatewayLLM = GatewayLLM
	}

	// Real headers from the addon.
	security, err := c.securityHeaders(ctx, target)
	if err != nil {
		t.Fatalf("securityHeaders: %v", err)
	}
	realSignEnc := security["pctb-x-sign"]
	realSign, uerr := url.QueryUnescape(realSignEnc)
	if uerr != nil {
		realSign = realSignEnc
	}
	if !strings.HasPrefix(realSign, signMarker) {
		t.Fatalf("unexpected sign: %q", realSign)
	}

	// Go-generated sign with the same session body.
	body, err := ExtractSignBody(realSign)
	if err != nil {
		t.Fatalf("extract body: %v", err)
	}
	g, err := NewSignGenerator(body)
	if err != nil {
		t.Fatalf("generator: %v", err)
	}
	goSign, err := g.Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	security["pctb-x-sign"] = url.QueryEscape(goSign)

	// Build the request exactly like the chat path (context headers only).
	send := func(sign string) (int, string) {
		sec := map[string]string{}
		for k, v := range security {
			sec[k] = v
		}
		sec["pctb-x-sign"] = url.QueryEscape(sign)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		c.applyChatHeaders(req, "")
		for key, value := range sec {
			req.Header.Set(key, value)
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		bodyStr := string(raw)
		if len(bodyStr) > 400 {
			bodyStr = bodyStr[:400]
		}
		return resp.StatusCode, bodyStr
	}

	// Control: real addon sign on the same URL.
	realStatus, realBody := send(realSign)
	t.Logf("CONTROL real sign -> status %d body %s", realStatus, realBody)

	// Experiment: Go-generated sign.
	goStatus, goBody := send(goSign)
	t.Logf("TEST    go sign   -> status %d body %s", goStatus, goBody)

	punished := func(status int, body string) bool {
		return status == http.StatusForbidden || strings.Contains(body, "sufei-punish")
	}
	if punished(realStatus, realBody) {
		t.Fatalf("control request was punished; environment issue, not sign")
	}
	if punished(goStatus, goBody) {
		t.Fatalf("WAF punished the Go-generated sign")
	}
	if goStatus >= 200 && goStatus < 300 {
		t.Logf("server accepted Go-generated sign (status %d)", goStatus)
	}
}
