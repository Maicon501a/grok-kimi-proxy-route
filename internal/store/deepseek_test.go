package store

import "testing"

// TestDeepSeekKeyVaultPersistsAcrossReopen verifies the encrypted key written
// to the secrets vault survives a full store close/reopen cycle (the scenario
// that made the key appear "lost" after app restart).
func TestDeepSeekKeyVaultPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteDeepSeekKeyFile("dpapi:test-blob"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSettings(func(s *Settings) { s.DeepSeekAPIKey = "dpapi:test-blob" }); err != nil {
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
	if !s.HasDeepSeekKey() {
		t.Fatal("key lost after reopen")
	}
	if s.DeepSeekAPIKey != "dpapi:test-blob" {
		t.Fatalf("vault not re-injected: %q", s.DeepSeekAPIKey)
	}
}

// TestDeepSeekKeyVaultSurvivesSettingsReSave simulates a legacy binary that
// re-saves settings.json without the deepseek field: the vault must restore it.
func TestDeepSeekKeyVaultSurvivesSettingsReSave(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteDeepSeekKeyFile("dpapi:survivor"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSettings(func(s *Settings) { s.DeepSeekAPIKey = "dpapi:survivor" }); err != nil {
		t.Fatal(err)
	}
	// Legacy wipe: settings JSON loses the field entirely.
	if err := st.UpdateSettings(func(s *Settings) { s.DeepSeekAPIKey = "" }); err != nil {
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
	if !st2.Settings().HasDeepSeekKey() {
		t.Fatal("vault did not survive a settings wipe")
	}
}
