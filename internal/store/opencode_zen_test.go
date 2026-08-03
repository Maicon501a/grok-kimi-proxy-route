package store

import "testing"

func TestOpenCodeZenRoutesCanonicalAndShortFreeModels(t *testing.T) {
	base := Settings{Provider: ProviderXAI, DefaultModel: DefaultModel}
	for _, model := range []string{
		"opencode/deepseek-v4-flash-free",
		"deepseek-v4-flash-free",
		"opencode/mimo-v2.5-free",
		"opencode/big-pickle",
	} {
		got := base.WithProviderForModel(model)
		if !got.IsOpenCodeZen() {
			t.Fatalf("model %q routed to %q, want %q", model, got.NormalizedProvider(), ProviderOpenCodeZen)
		}
		if got.EffectiveUpstream() != OpenCodeZenUpstream {
			t.Fatalf("model %q upstream=%q", model, got.EffectiveUpstream())
		}
	}
}

func TestOpenCodeZenResolvesWireModelWithoutProviderPrefix(t *testing.T) {
	s := Settings{Provider: ProviderOpenCodeZen, DefaultModel: OpenCodeZenDefaultModel}
	if got := s.ResolveModelForClient("opencode/deepseek-v4-flash-free"); got != "deepseek-v4-flash-free" {
		t.Fatalf("wire model=%q", got)
	}
	if got := s.ResolveModelForClient("mimo-v2.5"); got != "mimo-v2.5-free" {
		t.Fatalf("short alias wire model=%q", got)
	}
}

func TestOpenCodeZenIsDirectKeylessProvider(t *testing.T) {
	s := Settings{Provider: "zen-free"}
	if !s.IsOpenCodeZen() || s.ProviderAuthMode() != AuthModeAPIKey {
		t.Fatalf("provider=%q auth=%q", s.NormalizedProvider(), s.ProviderAuthMode())
	}
	if s.ProviderDefaultModel() != OpenCodeZenDefaultModel {
		t.Fatalf("default model=%q", s.ProviderDefaultModel())
	}
	s.ApplyProviderDefaults("opencode zen free")
	if s.UpstreamBase != OpenCodeZenUpstream || s.APIMode != "chat" {
		t.Fatalf("defaults upstream=%q api=%q", s.UpstreamBase, s.APIMode)
	}
}

func TestProviderCatalogIncludesOpenCodeZenFree(t *testing.T) {
	for _, provider := range ProviderCatalog() {
		if provider["id"] == ProviderOpenCodeZen {
			if provider["auth_mode"] != AuthModeAPIKey {
				t.Fatalf("auth_mode=%v", provider["auth_mode"])
			}
			return
		}
	}
	t.Fatal("ProviderCatalog missing OpenCode Zen Free")
}
