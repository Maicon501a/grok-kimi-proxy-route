package proxyhttp

import (
	"context"
	"net/http"
	"strings"

	"grok-desktop/internal/store"
)

// isCodexRequest detects OpenAI Codex CLI / Codex VS Code clients.
// Detection only — model is still whatever the client sent (no global force).
func isCodexRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	checks := []string{
		r.Header.Get("User-Agent"),
		r.Header.Get("user-agent"),
		r.Header.Get("Originator"),
		r.Header.Get("originator"),
		r.Header.Get("X-Client-Name"),
		r.Header.Get("x-client-name"),
		r.Header.Get("X-Title"),
		r.Header.Get("x-title"),
		r.Header.Get("OpenAI-Project"),
		r.Header.Get("openai-project"),
	}
	for _, h := range checks {
		low := strings.ToLower(h)
		if strings.Contains(low, "codex") {
			return true
		}
	}
	// Some builds send a dedicated header.
	if v := strings.TrimSpace(r.Header.Get("X-Codex")); v != "" && v != "0" && !strings.EqualFold(v, "false") {
		return true
	}
	return false
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
