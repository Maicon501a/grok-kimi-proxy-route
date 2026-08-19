package store

import "testing"

func TestSystemPromptUsesCanonicalProviderAndModel(t *testing.T) {
	s := defaultSettings()
	s.SetSystemPrompt(ProviderKimiWork, "k3-agent-high", "  Keep answers concise.  ")

	if got, want := s.SystemPromptFor(ProviderKimiWork, "k3-agent-xhigh"), "Keep answers concise."; got != want {
		t.Fatalf("SystemPromptFor Kimi alias = %q, want %q", got, want)
	}
	if got, want := s.SystemPrompts[ProviderKimiWork]["k3-agent"], "Keep answers concise."; got != want {
		t.Fatalf("stored canonical prompt = %q, want %q", got, want)
	}
}

func TestSetSystemPromptEmptyDeletesEntry(t *testing.T) {
	s := defaultSettings()
	s.SetSystemPrompt(ProviderXAI, "grok-4.5", "Be direct")
	s.SetSystemPrompt(ProviderXAI, "grok-4.5", " ")

	if got := s.SystemPromptFor(ProviderXAI, "grok-4.5"); got != "" {
		t.Fatalf("SystemPromptFor after delete = %q, want empty", got)
	}
	if _, ok := s.SystemPrompts[ProviderXAI]; ok {
		t.Fatal("empty provider prompt map was not removed")
	}
}
