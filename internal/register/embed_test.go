package register_test

import (
	"os"
	"path/filepath"
	"testing"

	"grok-desktop/internal/register"
)

func TestExtractEmbeddedBot(t *testing.T) {
	if !register.HasEmbeddedBot() {
		t.Fatal("expected embedded bot")
	}
	dir := t.TempDir()
	dest, err := register.ExtractEmbeddedBot(dir)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dest, "grok_signup.py")
	if st, err := os.Stat(script); err != nil || st.IsDir() {
		t.Fatalf("missing script: %v", err)
	}
	// second call should reuse
	dest2, err := register.ExtractEmbeddedBot(dir)
	if err != nil || dest2 != dest {
		t.Fatalf("reuse: %v %q vs %q", err, dest2, dest)
	}
	if _, err := os.Stat(filepath.Join(dest, "UPSTREAM.md")); err != nil {
		t.Fatalf("missing provenance metadata: %v", err)
	}
	t.Log("ver", register.BotEmbedVersion(), "dest", dest)
}
