package proxyhttp

import (
	"context"
	"net/http"
	"strings"

	"grok-desktop/internal/store"
)

// isCodexRequest detects first-party OpenAI Codex clients. Keep this narrow:
// it is used only to disambiguate bare OpenAI model ids for subscription auth.
func isCodexRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	originator := strings.ToLower(strings.TrimSpace(r.Header.Get("Originator")))
	switch originator {
	case "codex_cli_rs", "codex_vscode", "codex_app_server", "codex-tui", "codex_exec":
		return true
	}
	clientName := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Client-Name")))
	switch clientName {
	case "codex_cli_rs", "codex_vscode", "codex_app_server", "codex-tui", "codex_exec":
		return true
	}
	userAgent := strings.ToLower(strings.TrimSpace(r.Header.Get("User-Agent")))
	return strings.HasPrefix(userAgent, "codex_cli_rs/") ||
		strings.HasPrefix(userAgent, "openai-codex/") ||
		strings.HasPrefix(userAgent, "codex/") ||
		strings.HasPrefix(userAgent, "codex-tui/") ||
		strings.HasPrefix(userAgent, "codex_exec/")
}

// tokenAccountForSettings returns credentials for the request-scoped provider
// (from client model). Never uses UI "active provider" to pick credentials.
func tokenAccountForSettings(
	s *Server,
	ctx context.Context,
	token string,
	acc *store.Account,
	settings store.Settings,
) (string, *store.Account) {
	switch {
	case settings.IsQwen():
		key := strings.TrimSpace(settings.QwenAPIKey)
		return key, &store.Account{
			ID: "qwen", Provider: store.ProviderQwen, Label: "QwenBridge",
			Email:       settings.EffectiveQwenUpstream(),
			AccessToken: key,
			APIKey:      key,
		}
	case settings.IsDeepSeek():
		key := settings.DeepSeekAPIKeyPlain()
		return key, &store.Account{
			ID: "deepseek", Provider: store.ProviderDeepSeek, Label: "DeepSeek",
			Email:       store.DeepSeekUpstream,
			AccessToken: key,
			APIKey:      key,
		}
	case settings.IsOpenCodeGo():
		key := settings.OpenCodeGoAPIKeyPlain()
		return key, &store.Account{
			ID: "opencode-go", Provider: store.ProviderOpenCodeGo, Label: "OpenCode Go",
			Email: settings.EffectiveUpstream(), AccessToken: key, APIKey: key,
		}
	case settings.IsOllie():
		return store.OllieAPIKey, &store.Account{
			ID: "ollie", Label: "OllieChat", Email: "keyless@olliechat",
			AccessToken: store.OllieAPIKey,
		}
	case settings.IsGemini():
		return store.GeminiCredMarker, &store.Account{
			ID: "gemini-adc", Label: "Gemini (ADC)", Email: settings.EffectiveGeminiProject(),
			AccessToken: store.GeminiCredMarker,
		}
	case settings.IsKimiWork():
		if acc != nil && acc.NormalizedProvider() == store.ProviderKimiWork && acc.Usable() {
			if t := acc.BearerToken(); t != "" {
				return t, acc
			}
		}
		if s != nil && s.store != nil {
			if a, ok := s.store.PreferUsableAccountForProvider(store.ProviderKimiWork); ok && a != nil {
				if t := a.BearerToken(); t != "" {
					return t, a
				}
			}
		}
		return token, acc
	case settings.IsCodex():
		if acc != nil && acc.NormalizedProvider() == store.ProviderCodex && acc.Usable() && token != "" {
			return token, acc
		}
		if s != nil && s.store != nil {
			if a, ok := s.store.PreferUsableAccountForProvider(store.ProviderCodex); ok && a != nil && a.AccessToken != "" {
				return a.AccessToken, a
			}
		}
		return token, acc
	default:
		// xAI: keep token if already an xAI account from ensure.
		if acc != nil && acc.NormalizedProvider() == store.ProviderXAI && acc.Usable() && token != "" &&
			token != store.OllieAPIKey && token != store.GeminiCredMarker && !strings.HasPrefix(token, "sk-kimi-") {
			return token, acc
		}
		if s == nil || s.store == nil {
			return token, acc
		}
		if a, ok := s.store.PreferUsableAccountForProvider(store.ProviderXAI); ok && a != nil && a.AccessToken != "" {
			return a.AccessToken, a
		}
		// ensure with xAI route override (not UI global provider).
		ctxXAI := WithRouteProvider(ctx, store.ProviderXAI)
		if tok2, acc2, _, err := s.ensure(ctxXAI); err == nil && tok2 != "" &&
			tok2 != store.OllieAPIKey && tok2 != store.GeminiCredMarker && !strings.HasPrefix(tok2, "sk-kimi-") {
			if acc2 != nil {
				store.TrackInflight(ctxXAI, acc2.ID)
			}
			return tok2, acc2
		}
		return token, acc
	}
}
