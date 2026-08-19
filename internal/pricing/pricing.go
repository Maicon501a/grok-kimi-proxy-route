package pricing

import (
	"strings"

	"grok-desktop/internal/store"
)

// Public list prices (USD per 1M tokens), researched 2026-08:
//   - xAI: docs.x.ai/docs/models — grok-4.5 $2 in / $0.30 cached / $6 out (<200k prompt);
//     grok-4.3 $1.25/$0.20/$2.50; grok-build-0.1 $1.00/$0.20/$2.00.
//   - Kimi: platform.kimi.com/docs/pricing — prices in RMB, converted at ~7.1 CNY/USD:
//     K3 ¥2 hit / ¥20 miss / ¥100 out → ~$0.28/$2.80/$14;
//     K2.6 ¥1.10/¥6.50/¥27 → ~$0.15/$0.92/$3.80;
//     K2.7 Code ¥1.30/¥6.50/¥27 → ~$0.18/$0.92/$3.80; HighSpeed 2x.
//   - DeepSeek: api-docs.deepseek.com/quick_start/pricing —
//     deepseek-v4-flash $0.0028 cache-hit / $0.14 miss / $0.28 out;
//     deepseek-v4-pro $0.003625 / $0.435 / $0.87. Peak hours bill 2x (not applied here).
//   - Qwen: Model Studio (Alibaba) family rates, marked as estimates (est.).
//   - Gemini: ai.google.dev pricing for 2.5 family; 3.x marked as estimates.
type Rate struct {
	InputPerM  float64 `json:"input_per_m"`
	CachedPerM float64 `json:"cached_per_m"`
	OutputPerM float64 `json:"output_per_m"`
	Label      string  `json:"label"`
}

var table = map[string]Rate{
	// ——— xAI / Grok ———
	"grok-4.6": {
		InputPerM: 2.00, CachedPerM: 0.30, OutputPerM: 6.00, Label: "Grok 4.6 (est.)",
	},
	"grok-4.5": {
		InputPerM: 2.00, CachedPerM: 0.30, OutputPerM: 6.00, Label: "Grok 4.5",
	},
	"grok-4.5-build": {
		InputPerM: 2.00, CachedPerM: 0.30, OutputPerM: 6.00, Label: "Grok 4.5",
	},
	"grok-4.3": {
		InputPerM: 1.25, CachedPerM: 0.20, OutputPerM: 2.50, Label: "Grok 4.3",
	},
	"grok-build-0.1": {
		InputPerM: 1.00, CachedPerM: 0.20, OutputPerM: 2.00, Label: "Grok Build 0.1",
	},
	"grok-composer-2.5-fast": {
		InputPerM: 2.00, CachedPerM: 0.50, OutputPerM: 6.00, Label: "Composer 2.5 Fast (est.)",
	},

	// ——— Kimi (RMB → USD @ ~7.1) ———
	"kimi-k3": {
		InputPerM: 2.80, CachedPerM: 0.28, OutputPerM: 14.00, Label: "Kimi K3",
	},
	"kimi-for-coding": {
		InputPerM: 2.80, CachedPerM: 0.28, OutputPerM: 14.00, Label: "Kimi For Coding (K3 est.)",
	},
	"k3-agent": {
		InputPerM: 2.80, CachedPerM: 0.28, OutputPerM: 14.00, Label: "K3 Max / Work (K3 rates)",
	},
	"k3-agent-low": {
		InputPerM: 2.80, CachedPerM: 0.28, OutputPerM: 14.00, Label: "K3 Max Low Think (K3 rates)",
	},
	"k3-agent-medium": {
		InputPerM: 2.80, CachedPerM: 0.28, OutputPerM: 14.00, Label: "K3 Max Medium Think (K3 rates)",
	},
	"k3-agent-high": {
		InputPerM: 2.80, CachedPerM: 0.28, OutputPerM: 14.00, Label: "K3 Max High Think (K3 rates)",
	},
	"k3-agent-xhigh": {
		InputPerM: 2.80, CachedPerM: 0.28, OutputPerM: 14.00, Label: "K3 Max XHigh Think (K3 rates)",
	},
	"k3-agent-ultra": {
		InputPerM: 2.80, CachedPerM: 0.28, OutputPerM: 14.00, Label: "K3 Swarm Max (K3 rates)",
	},
	"kimi-k2.7-code": {
		InputPerM: 0.92, CachedPerM: 0.18, OutputPerM: 3.80, Label: "Kimi K2.7 Code",
	},
	"kimi-k2.7-code-highspeed": {
		InputPerM: 1.83, CachedPerM: 0.37, OutputPerM: 7.61, Label: "Kimi K2.7 Code HighSpeed",
	},
	"kimi-k2.6": {
		InputPerM: 0.92, CachedPerM: 0.15, OutputPerM: 3.80, Label: "Kimi K2.6",
	},
	"k2d6-agent": {
		InputPerM: 0.92, CachedPerM: 0.15, OutputPerM: 3.80, Label: "K2.6 Agent (K2.6 rates)",
	},
	"k2p6": {
		InputPerM: 0.92, CachedPerM: 0.15, OutputPerM: 3.80, Label: "K2.6 (est.)",
	},
	"k2p6-agent": {
		InputPerM: 0.92, CachedPerM: 0.15, OutputPerM: 3.80, Label: "K2.6 Agent (est.)",
	},

	// ——— DeepSeek (api.deepseek.com) ———
	"deepseek-v4-flash": {
		InputPerM: 0.14, CachedPerM: 0.0028, OutputPerM: 0.28, Label: "DeepSeek V4 Flash",
	},
	"deepseek-v4-pro": {
		InputPerM: 0.435, CachedPerM: 0.003625, OutputPerM: 0.87, Label: "DeepSeek V4 Pro",
	},
	// Legacy ids kept for existing users; current docs list v4-flash/v4-pro.
	"deepseek-chat": {
		InputPerM: 0.27, CachedPerM: 0.07, OutputPerM: 1.10, Label: "DeepSeek Chat (legacy)",
	},
	"deepseek-reasoner": {
		InputPerM: 0.55, CachedPerM: 0.14, OutputPerM: 2.19, Label: "DeepSeek Reasoner (legacy)",
	},

	// ——— Qwen (Model Studio, estimates) ———
	"qwen-max": {
		InputPerM: 2.80, CachedPerM: 0.28, OutputPerM: 7.50, Label: "Qwen Max (est.)",
	},
	"qwen-plus": {
		InputPerM: 1.20, CachedPerM: 0.12, OutputPerM: 4.00, Label: "Qwen Plus (est.)",
	},
	"qwen-flash": {
		InputPerM: 0.30, CachedPerM: 0.03, OutputPerM: 1.20, Label: "Qwen Flash (est.)",
	},

	// ——— Gemini (ai.google.dev; 3.x estimates) ———
	"gemini-2.5-pro": {
		InputPerM: 1.25, CachedPerM: 0.125, OutputPerM: 10.00, Label: "Gemini 2.5 Pro",
	},
	"gemini-2.5-flash": {
		InputPerM: 0.30, CachedPerM: 0.03, OutputPerM: 2.50, Label: "Gemini 2.5 Flash",
	},
	"gemini-2.5-flash-lite": {
		InputPerM: 0.10, CachedPerM: 0.01, OutputPerM: 0.40, Label: "Gemini 2.5 Flash-Lite",
	},
	"gemini-3-pro": {
		InputPerM: 2.00, CachedPerM: 0.20, OutputPerM: 12.00, Label: "Gemini 3 Pro (est.)",
	},
	"gemini-3-flash": {
		InputPerM: 0.50, CachedPerM: 0.05, OutputPerM: 4.00, Label: "Gemini 3 Flash (est.)",
	},
}

// providerFixed pins whole-provider rates regardless of model id. Ollie is a
// free keyless gateway, so its models (including deepseek-*/qwen-* clones)
// must never be billed with another provider's rates.
var providerFixed = map[string]Rate{
	store.ProviderOllie: {
		InputPerM: 0, CachedPerM: 0, OutputPerM: 0, Label: "OllieChat · free",
	},
}

// OpenCode Zen's public free tier is not billed locally.
func init() {
	providerFixed[store.ProviderOpenCodeZen] = Rate{
		InputPerM: 0, CachedPerM: 0, OutputPerM: 0, Label: "OpenCode Zen Free",
	}
	providerFixed[store.ProviderOpenCodeGo] = Rate{
		InputPerM: 0, CachedPerM: 0, OutputPerM: 0, Label: "OpenCode Go",
	}
	providerFixed[store.ProviderCodex] = Rate{
		InputPerM: 0, CachedPerM: 0, OutputPerM: 0, Label: "OpenAI Codex · ChatGPT plan",
	}
}

// Default when unknown model + provider
var fallback = Rate{InputPerM: 2.00, CachedPerM: 0.30, OutputPerM: 6.00, Label: "Default (Grok 4.5 rates)"}

func NormalizeModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimSuffix(m, "-responses")
	m = strings.TrimSuffix(m, "@responses")
	m = strings.TrimSuffix(m, "/responses")
	m = strings.TrimSuffix(m, "-chat")
	return m
}

// RateFor resolves the rate for a model id in the context of a provider.
// Provider context matters: the same id can be free on one provider (Ollie)
// and billed on another (DeepSeek).
func RateFor(model, provider string) Rate {
	prov := strings.ToLower(strings.TrimSpace(provider))
	if r, ok := providerFixed[prov]; ok {
		return r
	}
	m := strings.ToLower(strings.TrimSpace(model))
	if r, ok := table[m]; ok {
		return r
	}
	if nm := NormalizeModel(model); nm != m {
		if r, ok := table[nm]; ok {
			return r
		}
	}
	switch prov {
	case store.ProviderDeepSeek:
		return deepSeekRateFor(m)
	case store.ProviderKimiWork:
		return kimiRateFor(m)
	case store.ProviderQwen:
		return qwenRateFor(m)
	case store.ProviderXAI:
		return xaiRateFor(m)
	case store.ProviderGemini:
		return geminiRateFor(m)
	}
	// generic prefix match (unknown provider)
	for k, r := range table {
		if strings.HasPrefix(m, k) || strings.Contains(m, k) {
			return r
		}
	}
	return fallback
}

func deepSeekRateFor(m string) Rate {
	switch {
	case strings.Contains(m, "v4-pro"):
		return table["deepseek-v4-pro"]
	case strings.Contains(m, "v4-flash"):
		return table["deepseek-v4-flash"]
	case strings.Contains(m, "reasoner"):
		return table["deepseek-reasoner"]
	default:
		return table["deepseek-chat"]
	}
}

func kimiRateFor(m string) Rate {
	switch {
	case strings.Contains(m, "k2.7-code-highspeed") || strings.Contains(m, "highspeed"):
		return table["kimi-k2.7-code-highspeed"]
	case strings.Contains(m, "k2.7") || strings.Contains(m, "code"):
		return table["kimi-k2.7-code"]
	case strings.Contains(m, "k2d6") || strings.Contains(m, "k2p6") || strings.Contains(m, "k2.6"):
		return table["kimi-k2.6"]
	default:
		return table["kimi-k3"]
	}
}

func qwenRateFor(m string) Rate {
	switch {
	case strings.Contains(m, "3.8") || strings.Contains(m, "max"):
		return table["qwen-max"]
	case strings.Contains(m, "plus"):
		return table["qwen-plus"]
	case strings.Contains(m, "flash") || strings.Contains(m, "turbo") || strings.Contains(m, "lite"):
		return table["qwen-flash"]
	default:
		return table["qwen-plus"]
	}
}

func xaiRateFor(m string) Rate {
	switch {
	case strings.Contains(m, "composer"):
		return table["grok-composer-2.5-fast"]
	case strings.Contains(m, "4.3"):
		return table["grok-4.3"]
	case strings.Contains(m, "build"):
		return table["grok-build-0.1"]
	default:
		return table["grok-4.5"]
	}
}

func geminiRateFor(m string) Rate {
	switch {
	case strings.Contains(m, "2.5-flash-lite"):
		return table["gemini-2.5-flash-lite"]
	case strings.Contains(m, "2.5-flash"):
		return table["gemini-2.5-flash"]
	case strings.Contains(m, "2.5-pro"):
		return table["gemini-2.5-pro"]
	case strings.Contains(m, "flash") || strings.Contains(m, "lite"):
		return table["gemini-3-flash"]
	default:
		return table["gemini-3-pro"]
	}
}

// CostUSD estimates billable cost. Reasoning tokens are billed as output
// (xAI / Kimi / DeepSeek thinking). Provider disambiguates same-name models.
func CostUSD(model, provider string, prompt, completion, reasoning, cached int64) float64 {
	r := RateFor(model, provider)
	in := float64(prompt-cached) * r.InputPerM / 1_000_000
	if in < 0 {
		in = float64(prompt) * r.InputPerM / 1_000_000
	}
	cache := float64(cached) * r.CachedPerM / 1_000_000
	outTokens := completion + reasoning
	out := float64(outTokens) * r.OutputPerM / 1_000_000
	return in + cache + out
}

func AllRates() map[string]Rate {
	out := map[string]Rate{}
	for k, v := range table {
		out[k] = v
	}
	return out
}
