package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"grok-desktop/internal/secure"
)

func TestCodexProviderRoutingAndWireModel(t *testing.T) {
	s := Settings{Provider: ProviderXAI, DefaultModel: DefaultModel, UpstreamBase: DefaultUpstream}
	routed := s.WithProviderForModel("codex/gpt-5.6-sol")
	if !routed.IsCodex() {
		t.Fatalf("provider=%q, want %q", routed.NormalizedProvider(), ProviderCodex)
	}
	if got := routed.EffectiveUpstream(); got != CodexUpstream {
		t.Fatalf("upstream=%q, want %q", got, CodexUpstream)
	}
	if got := routed.ResolveModelForClient("codex/gpt-5.6-sol"); got != "gpt-5.6-sol" {
		t.Fatalf("wire model=%q, want gpt-5.6-sol", got)
	}
	if routed.ProviderAuthMode() != AuthModeSession {
		t.Fatalf("auth mode=%q, want %q", routed.ProviderAuthMode(), AuthModeSession)
	}
}

func TestCodexProviderCatalogEntry(t *testing.T) {
	for _, provider := range ProviderCatalog() {
		if provider["id"] == ProviderCodex {
			if provider["default_model"] != CodexDefaultModel || provider["default_api"] != "responses" {
				t.Fatalf("unexpected catalog entry: %#v", provider)
			}
			return
		}
	}
	t.Fatal("Codex provider missing from catalog")
}

func TestAccountRefreshLockReloadsRotatedToken(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	account := Account{
		ID: "codex-lock", Provider: ProviderCodex, Label: "Codex",
		AccessToken: "access-old", RefreshToken: "refresh-old", ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := first.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	account.AccessToken = "access-new"
	account.RefreshToken = "refresh-new"
	if err := first.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}
	stale, _ := second.GetAccount(account.ID)
	if stale.RefreshToken != "refresh-old" {
		t.Fatalf("second store unexpectedly fresh before lock: %q", stale.RefreshToken)
	}
	if err := second.WithAccountRefreshLock(context.Background(), account.ID, func() error {
		fresh, _ := second.GetAccount(account.ID)
		if fresh.RefreshToken != "refresh-new" || fresh.AccessToken != "access-new" {
			t.Fatalf("tokens not reloaded under lock: %#v", fresh)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCodexTokensAreEncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	account := Account{
		ID: "codex-encrypted", Provider: ProviderCodex, Label: "Codex",
		AccessToken: "plain-access-secret", RefreshToken: "plain-refresh-secret",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := st.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(st.accountPath(account.ID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(backup), account.AccessToken) || strings.Contains(string(backup), account.RefreshToken) {
		t.Fatalf("plaintext token found in JSON backup: %s", backup)
	}
	var storedAccess, storedRefresh string
	if err := st.db.QueryRow(`SELECT access_token, refresh_token FROM accounts WHERE id = ?`, account.ID).Scan(&storedAccess, &storedRefresh); err != nil {
		t.Fatal(err)
	}
	if !secure.HasCiphertext(storedAccess) || !secure.HasCiphertext(storedRefresh) {
		t.Fatalf("SQLite credentials are not encrypted: access=%q refresh=%q", storedAccess, storedRefresh)
	}
	inMemory, _ := st.GetAccount(account.ID)
	if inMemory.AccessToken != account.AccessToken || inMemory.RefreshToken != account.RefreshToken {
		t.Fatalf("in-memory credentials changed: %#v", inMemory)
	}
}

func TestCodexCredentialMigrationReloadsBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	account := Account{
		ID: "codex-migration", Provider: ProviderCodex, Label: "Codex",
		AccessToken: "access-old", RefreshToken: "refresh-old", ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := first.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	account.AccessToken = "access-rotated"
	account.RefreshToken = "refresh-rotated"
	if err := first.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}
	legacyBackup := account
	legacyBackup.AccessToken = "access-old"
	legacyBackup.RefreshToken = "refresh-old"
	if err := writeJSON(second.accountPath(account.ID), legacyBackup); err != nil {
		t.Fatal(err)
	}
	if err := second.migrateCodexCredentialsAtRest(account.ID); err != nil {
		t.Fatal(err)
	}
	fresh, _ := second.GetAccount(account.ID)
	if fresh.AccessToken != "access-rotated" || fresh.RefreshToken != "refresh-rotated" {
		t.Fatalf("migration restored stale credentials: %#v", fresh)
	}
}
