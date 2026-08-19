package store

import "testing"

func TestSanitizeModelForProvider_XAIClearsOllieModel(t *testing.T) {
	s := Settings{
		Provider:     ProviderXAI,
		DefaultModel: "accounts/euromodels/models/claude-sonnet-5",
		UpstreamBase: DefaultUpstream,
	}
	s.SanitizeModelForProvider()
	if s.DefaultModel != DefaultModel {
		t.Fatalf("got %q want %q", s.DefaultModel, DefaultModel)
	}
}

func TestApplyProviderDefaults_SwitchBackToXAI(t *testing.T) {
	s := Settings{
		Provider:     ProviderOllie,
		DefaultModel: "accounts/euromodels/models/claude-fable-5",
		UpstreamBase: OllieUpstream,
	}
	s.ApplyProviderDefaults("xai")
	if s.Provider != ProviderXAI {
		t.Fatalf("provider=%s", s.Provider)
	}
	if s.DefaultModel != DefaultModel {
		t.Fatalf("model=%s", s.DefaultModel)
	}
	if s.UpstreamBase != DefaultUpstream {
		t.Fatalf("upstream=%s", s.UpstreamBase)
	}
}

func TestApplyProviderDefaults_Gemini(t *testing.T) {
	s := Settings{Provider: ProviderXAI, DefaultModel: "grok-4.5"}
	s.ApplyProviderDefaults("gemini")
	if s.Provider != ProviderGemini {
		t.Fatalf("provider=%s", s.Provider)
	}
	if s.DefaultModel != GeminiDefaultModel {
		t.Fatalf("model=%s", s.DefaultModel)
	}
}

func TestSanitizeModelForProviderMigratesGrok45To46(t *testing.T) {
	s := Settings{Provider: ProviderXAI, DefaultModel: "grok-4.5", UpstreamBase: DefaultUpstream}
	s.SanitizeModelForProvider()
	if s.DefaultModel != DefaultModel {
		t.Fatalf("model=%q want=%q", s.DefaultModel, DefaultModel)
	}
}
