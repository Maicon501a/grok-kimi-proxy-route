package config

import "testing"

func TestLoadEmbeddedAppliesDesktopRuntimePolicy(t *testing.T) {
	cfg, err := LoadEmbedded(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 0 {
		t.Fatalf("listen config = %s:%d", cfg.Host, cfg.Port)
	}
	if cfg.AIStudio.BrowserMode != BrowserHeadlessSpoof || !cfg.AIStudio.ManagedHeadless || !cfg.AIStudio.ManagedHeadlessSpoofVisible {
		t.Fatalf("browser policy not applied: %+v", cfg.AIStudio)
	}
	if cfg.Conversation.Mode != ConversationStateless {
		t.Fatalf("conversation mode = %q", cfg.Conversation.Mode)
	}
	if cfg.ToolCalling.Mode != ToolCallingNativeFirst {
		t.Fatalf("tool calling mode = %q", cfg.ToolCalling.Mode)
	}
	if cfg.Streaming.ToolMode != StreamBuffered {
		t.Fatalf("tool stream mode = %q", cfg.Streaming.ToolMode)
	}
	if cfg.EagerBoot != "default" {
		t.Fatalf("eager boot = %q", cfg.EagerBoot)
	}
}
