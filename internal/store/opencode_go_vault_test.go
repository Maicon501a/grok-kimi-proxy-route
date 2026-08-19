package store

import "testing"

// TestOpenCodeGoKeyVaultPersistsAcrossReopen verifies the encrypted key written
// to the secrets vault survives a full store close/reopen cycle (the scenario
// that made the user re-enter the key after every app restart).
func TestOpenCodeGoKeyVaultPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteOpenCodeGoKeyFile("dpapi:test-blob"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSettings(func(s *Settings) { s.OpenCodeGoAPIKey = "dpapi:test-blob" }); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	s := st2.Settings()
	if !s.HasOpenCodeGoKey() {
		t.Fatal("key lost after reopen")
	}
	if s.OpenCodeGoAPIKey != "dpapi:test-blob" {
		t.Fatalf("vault not re-injected: %q", s.OpenCodeGoAPIKey)
	}
}

// TestOpenCodeGoKeyVaultSurvivesSettingsReSave simulates a stale process that
// re-saves settings without the opencode_go field: the vault must restore it.
func TestOpenCodeGoKeyVaultSurvivesSettingsReSave(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteOpenCodeGoKeyFile("dpapi:survivor"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSettings(func(s *Settings) { s.OpenCodeGoAPIKey = "dpapi:survivor" }); err != nil {
		t.Fatal(err)
	}
	// Legacy wipe: settings loses the field entirely.
	if err := st.UpdateSettings(func(s *Settings) { s.OpenCodeGoAPIKey = "" }); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if !st2.Settings().HasOpenCodeGoKey() {
		t.Fatal("vault did not survive a settings wipe")
	}
}

// TestOpenCodeGoKeyLegacyBlobMigratesToVault: a ciphertext-only settings blob
// (older app) must be migrated into the vault on Open and stay stable.
func TestOpenCodeGoKeyLegacyBlobMigratesToVault(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSettings(func(s *Settings) { s.OpenCodeGoAPIKey = "dpapi:legacy" }); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if st2.readOpenCodeGoKeyFile() != "dpapi:legacy" {
		t.Fatalf("vault file not migrated: %q", st2.readOpenCodeGoKeyFile())
	}
}
