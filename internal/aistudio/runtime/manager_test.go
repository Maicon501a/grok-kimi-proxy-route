package runtime

import (
	"path/filepath"
	"testing"

	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/profile"
)

func TestPersistedDefaultIsActiveAfterRuntimeRestart(t *testing.T) {
	root := t.TempDir()
	cfg, err := config.LoadEmbedded(root)
	if err != nil {
		t.Fatal(err)
	}
	valid := true
	profiles := []profile.Profile{
		{ID: "one", Label: "One", UserDataDir: filepath.Join(root, "one"), IsValid: &valid},
		{ID: "two", Label: "Two", UserDataDir: filepath.Join(root, "two"), IsValid: &valid},
	}
	first := New(cfg)
	if err := first.Profiles().SaveProfiles(profiles, "two"); err != nil {
		t.Fatal(err)
	}
	first.Close()

	restarted := New(cfg)
	defer restarted.Close()
	active, err := restarted.GetActiveChatProfile(nil)
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != "two" {
		t.Fatalf("active profile after restart=%q, want persisted default two", active.ID)
	}
}
