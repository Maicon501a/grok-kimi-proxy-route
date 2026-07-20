package store

import "testing"

func TestWithProviderForModel_Qwen(t *testing.T) {
	// Start from Kimi UI global — client model must still win.
	base := Settings{Provider: ProviderKimiWork, DefaultModel: KimiWorkDefaultModel}
	for _, m := range []string{"qwen3.8", "qwen3.7-plus", "Qwen3.8", "qwen-3.7-plus"} {
		q := base.WithProviderForModel(m)
		if !q.IsQwen() {
			t.Fatalf("model %q: want qwen, got %s", m, q.NormalizedProvider())
		}
		if q.APIMode != "chat" {
			t.Fatalf("model %q: qwen api mode %q, want chat", m, q.APIMode)
		}
		if got := q.EffectiveUpstream(); got != QwenDefaultUpstream {
			t.Fatalf("model %q: upstream %q, want %q", m, got, QwenDefaultUpstream)
		}
	}
	// Empty / alias / unknown still route to xAI (never the qwen UI leftover).
	qwenBase := Settings{Provider: ProviderQwen, DefaultModel: QwenDefaultModel}
	for _, m := range []string{"", "default", "auto", "some-unknown-model"} {
		x := qwenBase.WithProviderForModel(m)
		if !x.IsXAI() {
			t.Fatalf("model %q: want xai, got %s", m, x.NormalizedProvider())
		}
	}
	// Other providers unaffected.
	if g := qwenBase.WithProviderForModel("grok-4.5"); !g.IsXAI() {
		t.Fatalf("grok-4.5: want xai, got %s", g.NormalizedProvider())
	}
	if k := qwenBase.WithProviderForModel("k3-agent"); !k.IsKimiWork() {
		t.Fatalf("k3-agent: want kimi, got %s", k.NormalizedProvider())
	}
}

func TestWithProvider_QwenHonorsCustomBridgeUpstream(t *testing.T) {
	s := Settings{
		Provider:     ProviderXAI,
		QwenUpstream: "http://192.168.1.10:4000",
		QwenAPIKey:   "k",
	}
	q := s.WithProvider(ProviderQwen)
	if got := q.EffectiveUpstream(); got != "http://192.168.1.10:4000/v1" {
		t.Fatalf("upstream %q, want http://192.168.1.10:4000/v1", got)
	}
	if q.UpstreamBase != "http://192.168.1.10:4000/v1" {
		t.Fatalf("UpstreamBase %q", q.UpstreamBase)
	}
}

func TestApplyProviderDefaults_Qwen(t *testing.T) {
	s := Settings{Provider: ProviderXAI, DefaultModel: "grok-4.5"}
	s.ApplyProviderDefaults("qwen")
	if s.Provider != ProviderQwen {
		t.Fatalf("provider=%s", s.Provider)
	}
	if s.DefaultModel != QwenDefaultModel {
		t.Fatalf("model=%s", s.DefaultModel)
	}
	if s.APIMode != "chat" {
		t.Fatalf("api_mode=%s", s.APIMode)
	}
	if s.UpstreamBase != QwenDefaultUpstream {
		t.Fatalf("upstream=%s", s.UpstreamBase)
	}
}

func TestEffectiveQwenUpstream(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", QwenDefaultUpstream},
		{"http://127.0.0.1:3000", "http://127.0.0.1:3000/v1"},
		{"http://127.0.0.1:3000/", "http://127.0.0.1:3000/v1"},
		{"http://127.0.0.1:3000/v1", "http://127.0.0.1:3000/v1"},
		{"http://bridge.local:8080/v1/", "http://bridge.local:8080/v1"},
	}
	for _, c := range cases {
		s := Settings{QwenUpstream: c.in}
		if got := s.EffectiveQwenUpstream(); got != c.want {
			t.Fatalf("QwenUpstream %q: got %q want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeModelForProvider_QwenKeepsDashIds(t *testing.T) {
	// "qwen-3.7-plus" trips Ollie's "qwen-" catalog hint; qwen sanitize must keep it.
	s := Settings{Provider: ProviderQwen, DefaultModel: "qwen-3.7-plus"}
	s.SanitizeModelForProvider()
	if s.DefaultModel != "qwen-3.7-plus" {
		t.Fatalf("model=%s, want qwen-3.7-plus kept", s.DefaultModel)
	}
	// Cross-provider leftovers still reset.
	s2 := Settings{Provider: ProviderQwen, DefaultModel: "grok-4.5"}
	s2.SanitizeModelForProvider()
	if s2.DefaultModel != QwenDefaultModel {
		t.Fatalf("model=%s, want %s", s2.DefaultModel, QwenDefaultModel)
	}
	if s2.APIMode != "chat" {
		t.Fatalf("api_mode=%s", s2.APIMode)
	}
}

func TestProviderCatalogIncludesQwen(t *testing.T) {
	found := false
	for _, p := range ProviderCatalog() {
		if p["id"] == ProviderQwen {
			found = true
			if p["auth_mode"] != AuthModeAPIKey {
				t.Fatalf("qwen auth_mode=%v", p["auth_mode"])
			}
			if p["default_model"] != QwenDefaultModel {
				t.Fatalf("qwen default_model=%v", p["default_model"])
			}
		}
	}
	if !found {
		t.Fatal("ProviderCatalog missing qwen")
	}
}

func TestQwenNoAccountPool(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if got := st.PickAccountForProvider(ProviderQwen, StrategyRoundRobin); got != nil {
		t.Fatalf("PickAccountForProvider(qwen)=%v, want nil", got)
	}
	if st.HasUsableAccountForProvider(ProviderQwen) {
		t.Fatal("HasUsableAccountForProvider(qwen)=true, want false")
	}
	if got := st.PublicAccountsForProvider(ProviderQwen); len(got) != 0 {
		t.Fatalf("PublicAccountsForProvider(qwen)=%v, want empty", got)
	}
}
