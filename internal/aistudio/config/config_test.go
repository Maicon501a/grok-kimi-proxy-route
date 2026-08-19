package config

import "testing"

func TestToolCallingDefaultsToNativeFirst(t *testing.T) {
	t.Setenv("AISTUDIO_TOOL_CALLING_MODE", "")
	cfg, err := Load(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ToolCalling.Mode != ToolCallingNativeFirst {
		t.Fatalf("expected native_first default, got %q", cfg.ToolCalling.Mode)
	}
}

func TestToolCallingBridgeFirstRemainsAvailable(t *testing.T) {
	t.Setenv("AISTUDIO_TOOL_CALLING_MODE", "")
	cfg, err := Load(t.TempDir(), []string{"--tool-calling-mode=bridge_first"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ToolCalling.Mode != ToolCallingBridgeFirst {
		t.Fatalf("expected bridge_first CLI override, got %q", cfg.ToolCalling.Mode)
	}
}
