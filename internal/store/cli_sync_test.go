package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPickAccountPrefersNonExpired(t *testing.T) {
	isolateHome(t)
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	_ = st.UpsertAccount(Account{
		ID: "dead", Label: "dead", AccessToken: "old", RefreshToken: "rt-old",
		ExpiresAt: now.Add(-2 * time.Hour),
		ClientID:  DefaultClientID, Issuer: DefaultIssuer,
	})
	_ = st.UpsertAccount(Account{
		ID: "live", Label: "live", AccessToken: "new", RefreshToken: "rt-new",
		ExpiresAt: now.Add(2 * time.Hour),
		ClientID:  DefaultClientID, Issuer: DefaultIssuer,
	})
	for i := 0; i < 8; i++ {
		got := st.PickAccountForProvider(ProviderXAI, StrategyRoundRobin)
		if got == nil {
			t.Fatal("nil pick")
		}
		if got.ID != "live" {
			t.Fatalf("iter %d: want live, got %s", i, got.ID)
		}
	}
}

func TestSyncFromGrokCLIDoesNotClobberNewerProxy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	cliDir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cliExp := now.Add(1 * time.Hour)
	cliDoc := map[string]any{
		DefaultIssuer + "::" + DefaultClientID: map[string]any{
			"key":             "cli-access",
			"refresh_token":   "cli-rt",
			"user_id":         "user-1",
			"email":           "u@example.com",
			"expires_at":      cliExp.Format(time.RFC3339Nano),
			"oidc_client_id":  DefaultClientID,
			"oidc_issuer":     DefaultIssuer,
			"auth_mode":       "oidc",
		},
	}
	b, _ := json.Marshal(cliDoc)
	if err := os.WriteFile(filepath.Join(cliDir, "auth.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Proxy already refreshed: newer expiry + different RT.
	_ = st.UpsertAccount(Account{
		ID: "user-1", Email: "u@example.com",
		AccessToken: "proxy-access", RefreshToken: "proxy-rt",
		ExpiresAt: now.Add(3 * time.Hour),
		ClientID:  DefaultClientID, Issuer: DefaultIssuer,
	})
	n, err := st.SyncFromGrokCLI()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 updates (proxy newer), got %d", n)
	}
	got, ok := st.GetAccount("user-1")
	if !ok {
		t.Fatal("missing")
	}
	if got.RefreshToken != "proxy-rt" || got.AccessToken != "proxy-access" {
		t.Fatalf("clobbered by CLI: at=%q rt=%q", got.AccessToken, got.RefreshToken)
	}
}

func TestSyncFromGrokCLIAdoptsFresherCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	cliDir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cliExp := now.Add(4 * time.Hour)
	cliDoc := map[string]any{
		DefaultIssuer + "::" + DefaultClientID: map[string]any{
			"key":            "cli-access-new",
			"refresh_token":  "cli-rt-new",
			"user_id":        "user-2",
			"email":          "v@example.com",
			"expires_at":     cliExp.Format(time.RFC3339Nano),
			"oidc_client_id": DefaultClientID,
			"oidc_issuer":    DefaultIssuer,
		},
	}
	b, _ := json.Marshal(cliDoc)
	if err := os.WriteFile(filepath.Join(cliDir, "auth.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.UpsertAccount(Account{
		ID: "user-2", Email: "v@example.com",
		AccessToken: "proxy-old", RefreshToken: "proxy-old-rt",
		ExpiresAt: now.Add(30 * time.Minute),
		ClientID:  DefaultClientID, Issuer: DefaultIssuer,
	})
	n, err := st.SyncFromGrokCLI()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 adopt, got %d", n)
	}
	got, _ := st.GetAccount("user-2")
	if got.AccessToken != "cli-access-new" || got.RefreshToken != "cli-rt-new" {
		t.Fatalf("not adopted: at=%q rt=%q", got.AccessToken, got.RefreshToken)
	}
}

func TestWriteAccountToGrokCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	cliDir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cliDir, "auth.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	exp := time.Now().UTC().Add(2 * time.Hour)
	acc := Account{
		ID: "uid-9", UserID: "uid-9", Email: "w@example.com",
		AccessToken: "at-new", RefreshToken: "rt-new",
		ExpiresAt: exp, ClientID: DefaultClientID, Issuer: DefaultIssuer,
		Provider: ProviderXAI,
	}
	if err := st.WriteAccountToGrokCLI(acc); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	key := DefaultIssuer + "::" + DefaultClientID
	entry, ok := root[key].(map[string]any)
	if !ok {
		t.Fatalf("missing entry %s in %s", key, string(raw))
	}
	if entry["key"] != "at-new" || entry["refresh_token"] != "rt-new" {
		t.Fatalf("entry=%v", entry)
	}
}
