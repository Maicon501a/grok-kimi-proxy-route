package accountmenu

import (
	"path/filepath"
	"testing"

	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/profile"
	runtimepkg "grok-desktop/internal/aistudio/runtime"
)

func TestSetDefaultProfileUpdatesActiveRouting(t *testing.T) {
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
	mgr := runtimepkg.New(cfg)
	defer mgr.Close()
	if err := mgr.Profiles().SaveProfiles(profiles, "one"); err != nil {
		t.Fatal(err)
	}
	menu := New(mgr)
	if err := menu.SetDefaultProfile("two"); err != nil {
		t.Fatal(err)
	}
	if got := mgr.Profiles().DefaultID(); got != "two" {
		t.Fatalf("registry default = %q, want two", got)
	}
	active, err := mgr.GetActiveChatProfile(nil)
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != "two" {
		t.Fatalf("active route = %q, want two", active.ID)
	}
}
