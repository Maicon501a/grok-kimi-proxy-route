package main

import (
	"context"
	"strings"
	"testing"

	"grok-desktop/internal/store"
)

// isolateHome points the OS home dir at an empty temp dir so store.Open's
// SyncFromGrokCLI does not import real ~/.grok/auth.json accounts into tests.
func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
}

// TestEnsureCredsQwenMissingKey: no silent fallback — a clear error naming qwen.
func TestEnsureCredsQwenMissingKey(t *testing.T) {
	isolateHome(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = st.UpdateSettings(func(s *store.Settings) {
		s.Provider = store.ProviderQwen
		s.QwenAPIKey = ""
	})
	tok, acc, _, err := ensureCreds(context.Background(), st, nil, nil, "", false)
	if err == nil {
		t.Fatalf("want error when qwen key missing, got tok=%q acc=%v", tok, acc)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "qwen") {
		t.Fatalf("error should mention qwen: %v", err)
	}
	if tok != "" || acc != nil {
		t.Fatalf("no credentials should be returned: tok=%q acc=%v", tok, acc)
	}
}

// TestEnsureCredsQwenOK: single fake bridge account carrying the bridge key.
func TestEnsureCredsQwenOK(t *testing.T) {
	isolateHome(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = st.UpdateSettings(func(s *store.Settings) {
		s.Provider = store.ProviderQwen
		s.QwenAPIKey = "bridge-key"
		s.QwenUpstream = "http://127.0.0.1:3000"
	})
	tok, acc, settings, err := ensureCreds(context.Background(), st, nil, nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "bridge-key" {
		t.Fatalf("token=%q, want bridge-key", tok)
	}
	if acc == nil || acc.NormalizedProvider() != store.ProviderQwen {
		t.Fatalf("acc=%v, want qwen provider", acc)
	}
	if got := settings.EffectiveUpstream(); got != "http://127.0.0.1:3000/v1" {
		t.Fatalf("upstream=%q, want http://127.0.0.1:3000/v1", got)
	}
}
