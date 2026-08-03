package store

import "testing"

func TestProviderAvailability(t *testing.T) {
	for _, provider := range []string{ProviderOllie, "olliechat", ProviderGemini, "google", "vertex", ProviderQwen, "qwenbridge"} {
		if got := ProviderAvailability(provider); got != "disabled" {
			t.Fatalf("ProviderAvailability(%q) = %q, want disabled", provider, got)
		}
		if ProviderAvailabilityMessage(provider) == "" {
			t.Fatalf("ProviderAvailabilityMessage(%q) is empty", provider)
		}
	}
	for _, provider := range []string{ProviderAccio, "accio-work", "phoenix"} {
		if got := ProviderAvailability(provider); got != "maintenance" {
			t.Fatalf("ProviderAvailability(%q) = %q, want maintenance", provider, got)
		}
	}
	for _, provider := range []string{ProviderXAI, ProviderKimiWork, ProviderDeepSeek, ProviderOpenCodeZen} {
		if got := ProviderAvailability(provider); got != "" {
			t.Fatalf("ProviderAvailability(%q) = %q, want available", provider, got)
		}
	}
}
