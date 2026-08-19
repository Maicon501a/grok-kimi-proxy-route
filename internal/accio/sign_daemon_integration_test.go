package accio

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestSignGeneratorWithLiveDaemon validates the Go sign generator against a
// real SecurityGuard session. It starts the Node sidecar (sg_daemon.js),
// captures one real pctb-x-sign, extracts its 44-byte session body, then
// generates 10 signs in pure Go and checks every structural invariant plus
// body round-trip.
//
// Skipped when the native addon is not available (CI / non-Windows).
func TestSignGeneratorWithLiveDaemon(t *testing.T) {
	if securityGuardBundleDir() == "" {
		t.Skip("security-guard bundle not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	daemon, err := getSGDaemon(ctx)
	if err != nil {
		t.Fatalf("getSGDaemon: %v", err)
	}
	url2 := "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=ffffffffffffffffffffffffffffffff"
	headers, err := daemon.headers(ctx, url2)
	if err != nil {
		t.Fatalf("daemon.headers: %v", err)
	}
	realSign, ok := headers["pctb-x-sign"]
	if !ok || !strings.HasPrefix(realSign, signMarker) {
		t.Fatalf("no valid sign from daemon: %q", realSign)
	}
	// The daemon URI-encodes header values (encodeURIComponent), so decode.
	if unescaped, err := url.QueryUnescape(realSign); err == nil {
		realSign = unescaped
	}

	body, err := ExtractSignBody(realSign)
	if err != nil {
		t.Fatalf("extract body: %v", err)
	}
	t.Logf("real sign OK, session body=%x", body)

	g, err := NewSignGenerator(body)
	if err != nil {
		t.Fatalf("generator: %v", err)
	}
	for i := 0; i < 10; i++ {
		sign, err := g.Generate()
		if err != nil {
			t.Fatalf("generate %d: %v", i, err)
		}
		payload := decodePayload(t, sign)
		if err := ValidateStructure(payload); err != nil {
			t.Fatalf("generated %d invalid: %v", i, err)
		}
		got, _ := ExtractSignBody(sign)
		for j := range body {
			if got[j] != body[j] {
				t.Fatalf("body drift at %d", j)
			}
		}
	}
	t.Log("10 Go-generated signs structurally valid against live session")
}
