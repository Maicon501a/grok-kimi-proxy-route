package pricing

import (
	"testing"

	"grok-desktop/internal/store"
)

func TestRateForProviderAware(t *testing.T) {
	cases := []struct {
		model, provider string
		wantIn          float64
	}{
		// Ollie is free — even for ids that collide with paid models.
		{model: "deepseek-v4-flash-free", provider: store.ProviderOllie, wantIn: 0},
		{model: "qwen-3.7-plus", provider: store.ProviderOllie, wantIn: 0},
		{model: "claude-sonnet-5", provider: store.ProviderOllie, wantIn: 0},
		// DeepSeek official.
		{model: "deepseek-v4-flash", provider: store.ProviderDeepSeek, wantIn: 0.14},
		{model: "deepseek-v4-pro", provider: store.ProviderDeepSeek, wantIn: 0.435},
		{model: "deepseek-chat", provider: store.ProviderDeepSeek, wantIn: 0.27},
		// Kimi family.
		{model: "k3-agent", provider: store.ProviderKimiWork, wantIn: 2.80},
		{model: "k3-agent-xhigh", provider: store.ProviderKimiWork, wantIn: 2.80},
		{model: "k2d6-agent", provider: store.ProviderKimiWork, wantIn: 0.92},
		{model: "kimi-k2.7-code", provider: store.ProviderKimiWork, wantIn: 0.92},
		{model: "kimi-k2.7-code-highspeed", provider: store.ProviderKimiWork, wantIn: 1.83},
		// Qwen estimates.
		{model: "qwen3.8", provider: store.ProviderQwen, wantIn: 2.80},
		{model: "qwen3.7-plus", provider: store.ProviderQwen, wantIn: 1.20},
		{model: "qwen3.7-flash", provider: store.ProviderQwen, wantIn: 0.30},
		// xAI.
		{model: "grok-4.5", provider: store.ProviderXAI, wantIn: 2.00},
		{model: "grok-4.3", provider: store.ProviderXAI, wantIn: 1.25},
		// ChatGPT subscription usage is quota-based, not API token billing.
		{model: "codex/gpt-5.6-sol", provider: store.ProviderCodex, wantIn: 0},
	}
	for _, c := range cases {
		r := RateFor(c.model, c.provider)
		if r.InputPerM != c.wantIn {
			t.Errorf("RateFor(%q, %q): input=%.4f want %.4f (%s)", c.model, c.provider, r.InputPerM, c.wantIn, r.Label)
		}
	}
}

func TestCostUSDByProvider(t *testing.T) {
	// Same token counts: Ollie free, DeepSeek paid.
	free := CostUSD("deepseek-v4-flash", store.ProviderOllie, 1000, 200, 0, 0)
	if free != 0 {
		t.Fatalf("ollie cost should be 0, got %v", free)
	}
	paid := CostUSD("deepseek-v4-flash", store.ProviderDeepSeek, 1000, 200, 0, 0)
	if paid <= 0 || paid >= free+0.001 {
		t.Fatalf("deepseek cost=%v want > 0", paid)
	}
}
