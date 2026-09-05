package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	b64 "encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"grok-desktop/internal/logging"
	"grok-desktop/internal/secure"
)

const (
	AppName         = "GrokDesktop"
	DefaultUpstream = "https://cli-chat-proxy.grok.com/v1"
	// Wire identity of the official Grok CLI (currently 1.0.13). cli-chat-proxy
	// gates function-tool emission on these headers; older 0.2.x / 1.0.4 / 1.0.5 values look
	// like a chat-only client and the model never returns tool_calls.
	DefaultClientVersion    = "1.0.13"
	DefaultClientIdentifier = "grok-cli"
	DefaultClientSurface    = "grok-cli"
	DefaultModel            = "grok-4.6"
	DefaultEffort           = "high"
	DefaultClientID         = "b1a00492-073a-47ea-816f-4c329264a828"
	DefaultIssuer           = "https://auth.x.ai"
	// Scope matching the official Grok CLI device grant. cli-chat-proxy
	// rejects /v1/responses when the token lacks conversations/workspaces,
	// so the full set is requested up front (auth.x.ai accepts it).
	DefaultScopes = "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write workspaces:read workspaces:write"

	// Upstream providers (local proxy can fan-out to any of these).
	ProviderXAI         = "xai"
	ProviderKimiWork    = "kimi_work"
	ProviderOllie       = "ollie"
	ProviderGemini      = "gemini"
	ProviderQwen        = "qwen"
	ProviderDeepSeek    = "deepseek"
	ProviderAccio       = "accio"
	ProviderOpenCodeZen = "opencode_zen"
	ProviderOpenCodeGo  = "opencode_go"
	ProviderCodex       = "openai_codex"

	// AuthMode: how credentials are obtained for a provider.
	AuthModeSession = "auth"    // multi-account session flow (xAI OAuth, Kimi Work mint)
	AuthModeAPIKey  = "api_key" // direct API key / ADC (Ollie, Gemini, …)

	// OllieChat — free keyless OpenAI/Anthropic-compatible API (WeirdMM).
	OllieUpstream     = "https://olliechat.vercel.app/v1"
	OllieAPIKey       = "ollie" // any non-empty string is accepted; ignored by server
	OllieDefaultModel = "claude-sonnet-5"

	// Gemini via Vertex AI Agent Platform (ADC — no API keys).
	GeminiDefaultModel    = "gemini-3.1-pro-preview"
	GeminiDefaultLocation = "global"
	// Placeholder "token" for local ensureCreds; real access token loaded from ADC at request time.
	GeminiCredMarker = "adc:google"

	// Kimi Work / Kimi Code (Desktop) — coding gateway with sk-kimi keys.
	KimiWorkUpstream = "https://agent-gw.kimi.com/coding/v1"
	// Wire model ids observed from official Kimi Desktop (agent-gw): k3-agent, k2d6-agent, k2p6.
	// "kimi-for-coding" is product branding / legacy alias, not the chat model field.
	KimiWorkDefaultModel = "k3-agent"
	KimiWorkUserAgent    = "Desktop Kimi Work"

	// QwenBridge — local OpenAI-compatible bridge (multi-account internally).
	// The grok proxy only needs the bridge base URL + its API_KEY; model list
	// is discovered dynamically via GET {base}/models.
	QwenDefaultUpstream = "http://127.0.0.1:3000/v1"
	QwenDefaultModel    = "qwen3.8"

	// DeepSeek — official OpenAI-compatible API (api.deepseek.com).
	// The API key is stored encrypted (DPAPI on Windows) inside Settings.
	DeepSeekUpstream     = "https://api.deepseek.com/v1"
	DeepSeekDefaultModel = "deepseek-v4-flash"
	// DeepSeekProModel is the flagship reasoning model (deepseek-v4-pro);
	// both ids are exposed in catalogs and the proxy forwards whatever the
	// client sends.
	DeepSeekProModel = "deepseek-v4-pro"
	// DeepSeekReasonerModel is the legacy reasoning id, kept for compat.
	DeepSeekReasonerModel = "deepseek-reasoner"

	AccioDefaultModel = "accio/1Nexus-R36W8qJ5vB6h"

	// OpenCode Zen Free — direct OpenAI-compatible gateway. The public bearer
	// token unlocks Zen's free tier; no local opencode process is required.
	OpenCodeZenUpstream     = "https://opencode.ai/zen/v1"
	OpenCodeZenAPIKey       = "public"
	OpenCodeZenDefaultModel = "opencode/deepseek-v4-flash-free"

	// OpenCode Go uses the authenticated OpenCode gateway with a user API key.
	// It is distinct from OpenCode Zen Free and every opencode-go model must use
	// the dedicated Go gateway.
	OpenCodeGoUpstream     = "https://opencode.ai/zen/go/v1"
	OpenCodeGoGateway      = "https://opencode.ai/zen/go/v1"
	OpenCodeGoDefaultModel = "opencode-go/deepseek-v4-flash"

	// OpenAI Codex via the user's ChatGPT subscription (official Codex OAuth).
	CodexUpstream      = "https://chatgpt.com/backend-api/codex"
	CodexDefaultModel  = "codex/gpt-5.6-sol"
	CodexClientVersion = "0.144.0"
)

// ProviderAvailability returns the product-level availability gate used by
// both the desktop chat and the embedded HTTP proxy. Empty means available.
func ProviderAvailability(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case ProviderOllie, "olliechat", ProviderQwen, "qwenbridge":
		return "disabled"
	case ProviderAccio, "accio-work", "phoenix":
		return "maintenance"
	default:
		return ""
	}
}

func ProviderAvailabilityMessage(provider string) string {
	switch ProviderAvailability(provider) {
	case "disabled":
		return "Este provedor está desativado no momento. Tente outro provedor."
	case "maintenance":
		return "Este provedor está em manutenção. O uso continua liberado, mas podem ocorrer problemas e erros."
	default:
		return ""
	}
}

// ProviderAvailabilityBlocksRequests keeps maintenance as an advisory state.
// Disabled providers remain blocked, while Accio can continue serving traffic
// during maintenance so the user can test the route and see its real errors.
func ProviderAvailabilityBlocksRequests(provider string) bool {
	return ProviderAvailability(provider) == "disabled"
}

type LoadBalancerStrategy string

const (
	StrategyActive     LoadBalancerStrategy = "active"
	StrategyRoundRobin LoadBalancerStrategy = "round_robin"
	StrategyLeastUsed  LoadBalancerStrategy = "least_used"
	StrategyRandom     LoadBalancerStrategy = "random"
)

func (s LoadBalancerStrategy) IsValid() bool {
	switch s {
	case StrategyActive, StrategyRoundRobin, StrategyLeastUsed, StrategyRandom:
		return true
	}
	return false
}

// ProviderState tracks per-provider load balancer state (round-robin index, etc).
type ProviderState struct {
	RRIndex int // round-robin cursor
}

type Account struct {
	ID string `json:"id"`
	// Provider: xai | kimi_work | … Empty means xai (legacy accounts).
	Provider string `json:"provider,omitempty"`
	Label    string `json:"label"`
	Email    string `json:"email,omitempty"`
	TeamID   string `json:"team_id,omitempty"`
	UserID   string `json:"user_id,omitempty"`
	// xAI OAuth (and generic bearer sessions)
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	// Kimi Work coding key (sk-kimi-…). Also used if a provider stores a static API key on the account.
	APIKey   string `json:"api_key,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
	Source   string `json:"source,omitempty"` // oauth | desktop_mint | paste_key | paste_jwt | …
	// GoogleRefreshToken stores the Google OAuth refresh token (for VM re-login without browser).
	GoogleRefreshToken string `json:"google_refresh_token,omitempty"`
	// GoogleEmail and GooglePassword are stored per-account for Playwright stealth re-login.
	GoogleEmail    string `json:"google_email,omitempty"`
	GooglePassword string `json:"google_password,omitempty"`
	// ExhaustedAt marks when usage quota (402 / balance exhausted) was observed.
	// Zero means the account is still usable for quota purposes.
	ExhaustedAt   time.Time `json:"exhausted_at,omitempty"`
	ExhaustReason string    `json:"exhaust_reason,omitempty"`
	// AuthDeniedAt marks permanent-ish auth failures (403 permission-denied, invalid grant).
	// Distinct from quota so UI/logs can tell them apart; both make Usable() false.
	AuthDeniedAt     time.Time `json:"auth_denied_at,omitempty"`
	AuthDeniedReason string    `json:"auth_denied_reason,omitempty"`
	ClientID         string    `json:"client_id"`
	Issuer           string    `json:"issuer"`
	Scope            string    `json:"scope,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	// RequestCount is incremented atomically by the load balancer for least-used strategy.
	// Not persisted; resets on app restart.
	requestCount int64 `json:"-"`
}

func (a *Account) Expired() bool {
	if a.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().UTC().After(a.ExpiresAt)
}

func (a *Account) ExpiresSoon(skew time.Duration) bool {
	if a.ExpiresAt.IsZero() {
		// Unknown lifetime: treat as needing refresh if we have a refresh token.
		return a != nil && a.RefreshToken != ""
	}
	return time.Now().UTC().After(a.ExpiresAt.Add(-skew))
}

// Exhausted reports whether this account hit usage quota and should be rotated away.
func (a *Account) Exhausted() bool {
	return a != nil && !a.ExhaustedAt.IsZero()
}

// AuthDenied reports whether chat auth was rejected for this account.
func (a *Account) AuthDenied() bool {
	return a != nil && !a.AuthDeniedAt.IsZero()
}

// NormalizedProvider returns the provider id for this account (legacy empty → xai).
func (a *Account) NormalizedProvider() string {
	if a == nil {
		return ProviderXAI
	}
	p := strings.ToLower(strings.TrimSpace(a.Provider))
	switch p {
	case ProviderKimiWork, "kimi", "kimi-work", "kimiwork", "moonshot-work":
		return ProviderKimiWork
	case ProviderOllie, "olliechat":
		return ProviderOllie
	case ProviderGemini, "google", "vertex":
		return ProviderGemini
	case ProviderQwen, "qwenbridge", "qwen-bridge":
		return ProviderQwen
	case ProviderDeepSeek, "deep-seek", "ds":
		return ProviderDeepSeek
	case ProviderAccio, "accio-work", "phoenix":
		return ProviderAccio
	case ProviderOpenCodeZen, "opencode-zen", "opencode", "zen", "zen-free", "opencode-zen-free", "opencode zen", "opencode zen free":
		return ProviderOpenCodeZen
	case ProviderOpenCodeGo, "opencode-go", "opencode go":
		return ProviderOpenCodeGo
	case ProviderCodex, "codex", "openai-codex", "openai codex", "chatgpt":
		return ProviderCodex
	case "", ProviderXAI, "grok", "x.ai":
		return ProviderXAI
	default:
		return p
	}
}

// BearerToken returns the credential string used as Authorization bearer for this account.
func (a *Account) BearerToken() string {
	if a == nil {
		return ""
	}
	if a.NormalizedProvider() == ProviderKimiWork {
		if k := strings.TrimSpace(a.APIKey); k != "" {
			return k
		}
	}
	if t := strings.TrimSpace(a.AccessToken); t != "" {
		return t
	}
	return strings.TrimSpace(a.APIKey)
}

// Usable is true when the account still has credentials and is not marked
// quota-exhausted or auth-denied. For xAI, bot-flagged JWTs are rejected.
// Token expiry alone does not make it unusable if a refresh token exists
// (ensureCreds will refresh).
func (a *Account) Usable() bool {
	if a == nil || a.Exhausted() || a.AuthDenied() {
		return false
	}
	switch a.NormalizedProvider() {
	case ProviderKimiWork:
		return strings.TrimSpace(a.APIKey) != "" || strings.HasPrefix(strings.TrimSpace(a.AccessToken), "sk-kimi-")
	default:
		if a.AccessToken == "" {
			return false
		}
		// cli-chat-proxy returns 403 permission-denied for bot-flagged JWTs on chat.
		if TokenBotFlagged(a.AccessToken) {
			return false
		}
		return true
	}
}

// TokenBotFlagged reports bot_flag_source != 0 in the access-token JWT payload.
// Signature is not verified; the token is only used as an opaque bearer upstream.
func TokenBotFlagged(access string) bool {
	parts := strings.Split(access, ".")
	if len(parts) < 2 {
		return false
	}
	payload := parts[1]
	if m := len(payload) % 4; m != 0 {
		payload += strings.Repeat("=", 4-m)
	}
	raw, err := decodeB64URL(payload)
	if err != nil {
		return false
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return false
	}
	switch n := claims["bot_flag_source"].(type) {
	case float64:
		return n != 0
	case int:
		return n != 0
	case json.Number:
		i, _ := n.Int64()
		return i != 0
	default:
		return false
	}
}

func decodeB64URL(s string) ([]byte, error) {
	// std encoding with padding first, then raw.
	if b, err := b64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return b64.RawURLEncoding.DecodeString(s)
}

type Settings struct {
	ActiveAccountID string `json:"active_account_id"`
	// Provider: xai | kimi_work | ollie | gemini | qwen | deepseek | opencode_zen | opencode_go
	Provider        string `json:"provider,omitempty"`
	DefaultModel    string `json:"default_model"`
	ReasoningEffort string `json:"reasoning_effort"`
	APIMode         string `json:"api_mode"`
	UpstreamBase    string `json:"upstream_base"`
	ClientVersion   string `json:"client_version"`
	ProxyListen     string `json:"proxy_listen"`
	ProxyEnabled    bool   `json:"proxy_enabled"`
	ProxyAPIKey     string `json:"proxy_api_key,omitempty"`
	StoreResponses  bool   `json:"store_responses"`
	// ForceDefaultModel when true (default) always routes client requests to DefaultModel.
	ForceDefaultModel *bool `json:"force_default_model,omitempty"`
	// Gemini / Vertex AI (Application Default Credentials).
	GeminiProject  string `json:"gemini_project,omitempty"`
	GeminiLocation string `json:"gemini_location,omitempty"`
	// QwenBridge local bridge: base URL (with or without /v1) + its API_KEY.
	QwenUpstream string `json:"qwen_upstream,omitempty"`
	QwenAPIKey   string `json:"qwen_api_key,omitempty"`
	// DeepSeekAPIKey stores the DeepSeek API key as an ENCRYPTED blob
	// (internal/secure — DPAPI on Windows). Never plaintext on disk; the
	// frontend only ever sees a masked sentinel.
	DeepSeekAPIKey   string `json:"deepseek_api_key,omitempty"`
	OpenCodeGoAPIKey string `json:"opencode_go_api_key,omitempty"`
	// CodexAccountID is request-scoped and never persisted. The ChatGPT backend
	// requires it alongside the OAuth bearer token.
	CodexAccountID      string `json:"-"`
	CodexFedRAMP        bool   `json:"-"`
	ThemeAccent         string `json:"theme_accent,omitempty"`
	KimiStealthHeadless bool   `json:"kimi_stealth_headless"`
	GoogleEmail         string `json:"google_email,omitempty"`
	GooglePassword      string `json:"google_password,omitempty"`
	// LoadBalancerStrategies maps provider → strategy (active | round_robin | least_used | random).
	LoadBalancerStrategies map[string]string `json:"load_balancer_strategies,omitempty"`
	// SystemPrompts stores a user-managed prompt per routed provider and canonical
	// upstream model id. It is intentionally separate from request payloads so the
	// local proxy can apply it consistently to desktop and HTTP clients.
	SystemPrompts map[string]map[string]string `json:"system_prompts,omitempty"`
	// AutoCreateOnExhausted enables the xAI pool top-up: when the number of
	// usable xAI accounts drops below AutoCreateMinAccounts, the app runs the
	// grok-register signup flow until the pool is back at the floor.
	AutoCreateOnExhausted bool `json:"auto_create_on_exhausted"`
	// AutoCreateMinAccounts is the target pool size (default 3 when enabled).
	AutoCreateMinAccounts int `json:"auto_create_min_accounts,omitempty"`
}

// SystemPromptFor returns the prompt configured for a provider/model pair. Both
// values are normalized through the same routing/model-resolution path used by
// the proxy, which keeps aliases such as k3-agent-high and codex/... stable.
func (s Settings) SystemPromptFor(provider, model string) string {
	routed := s.WithProvider(provider)
	providerKey := routed.NormalizedProvider()
	modelKey := strings.TrimSpace(routed.ResolveModelForClient(model))
	if modelKey == "" {
		modelKey = strings.TrimSpace(routed.DefaultModel)
	}
	if prompts := s.SystemPrompts[providerKey]; prompts != nil {
		return strings.TrimSpace(prompts[modelKey])
	}
	return ""
}

// SetSystemPrompt updates one provider/model prompt. An empty prompt removes the
// entry, making deletion from the UI and API explicit and idempotent.
func (s *Settings) SetSystemPrompt(provider, model, prompt string) {
	routed := s.WithProvider(provider)
	providerKey := routed.NormalizedProvider()
	modelKey := strings.TrimSpace(routed.ResolveModelForClient(model))
	if modelKey == "" {
		modelKey = strings.TrimSpace(routed.DefaultModel)
	}
	if providerKey == "" || modelKey == "" {
		return
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		if s.SystemPrompts == nil || s.SystemPrompts[providerKey] == nil {
			return
		}
		delete(s.SystemPrompts[providerKey], modelKey)
		if len(s.SystemPrompts[providerKey]) == 0 {
			delete(s.SystemPrompts, providerKey)
		}
		return
	}
	if s.SystemPrompts == nil {
		s.SystemPrompts = make(map[string]map[string]string)
	}
	if s.SystemPrompts[providerKey] == nil {
		s.SystemPrompts[providerKey] = make(map[string]string)
	}
	s.SystemPrompts[providerKey][modelKey] = prompt
}

// ForceModel reports whether the proxy should ignore the client's model field.
// Default is FALSE — HTTP clients keep the model they send.
// Desktop chat UI selects model independently (not via this force flag).
func (s Settings) ForceModel() bool {
	if s.ForceDefaultModel == nil {
		return false
	}
	return *s.ForceDefaultModel
}

// ResolveModel picks the upstream model id for a client request.
// Aliases: "", "default", "proxy", "auto", "grok-desktop" → DefaultModel.
// When ForceModel() is true, unknown / cross-provider ids still map to DefaultModel,
// but explicit provider-native ids are honored (desktop / legacy callers).
// Prefer ResolveModelForClient / ResolveModelForCodex on the HTTP proxy.
func (s Settings) ResolveModel(requested string) string {
	return s.resolveModel(requested, s.ForceModel())
}

// ResolveModelForClient honors the model id chosen by Kilo / OpenCode / SDKs.
// Only aliases (default/proxy/…) map to the global DefaultModel — force_default is ignored.
func (s Settings) ResolveModelForClient(requested string) string {
	return s.resolveModel(requested, false)
}

// ResolveModelForCodex always honors the client model (same as ResolveModelForClient).
// Global force_default / UI provider must not rewrite what the client sent.
func (s Settings) ResolveModelForCodex(requested string) string {
	return s.resolveModel(requested, false)
}

func (s Settings) resolveModel(requested string, force bool) string {
	req := strings.TrimSpace(requested)
	low := strings.ToLower(req)
	alias := req == "" ||
		low == "default" || low == "proxy" || low == "auto" ||
		low == "grok-desktop" || low == "global" || low == "current"
	def := strings.TrimSpace(s.DefaultModel)
	if def == "" {
		def = s.ProviderDefaultModel()
	}
	if alias {
		req = def
	} else if force {
		// Force rewrites junk / wrong-provider models to the global default,
		// but does not swallow an explicit model that belongs to the active provider.
		if !s.isNativeModelForProvider(req) {
			req = def
		}
	}
	// strip publisher prefix if present
	if i := strings.LastIndex(req, "/models/"); i >= 0 {
		req = req[i+len("/models/"):]
	}
	// strip -responses / @responses for upstream id
	low = strings.ToLower(req)
	switch {
	case strings.HasSuffix(low, "-responses"):
		req = req[:len(req)-len("-responses")]
	case strings.HasSuffix(low, "@responses"):
		req = req[:len(req)-len("@responses")]
	case strings.HasSuffix(low, "/responses"):
		req = req[:len(req)-len("/responses")]
	}
	// Friendly Ollie aliases
	if s.IsOllie() {
		req = normalizeOllieModelAlias(req)
	}
	// Zen's provider prefix is a client-facing namespace. The gateway expects
	// the bare model id (e.g. deepseek-v4-flash-free).
	if s.IsOpenCodeZen() {
		req = resolveOpenCodeZenModel(req)
	}
	if s.IsOpenCodeGo() {
		req = resolveOpenCodeGoModel(req)
	}
	if s.IsCodex() {
		req = resolveCodexModel(req)
	}
	// Kimi Work: Desktop aliases → gateway wire id
	if s.IsKimiWork() {
		req = resolveKimiWorkModel(req)
	}
	return req
}

func resolveKimiWorkModel(requested string) string {
	// Preserve real agent-gw wire ids. Only map empty/legacy aliases.
	// Effort suffixes (k3-agent-high) are stripped for the model field; callers
	// may still read effort via ExtractKimiWorkEffort on the original id.
	raw := strings.TrimSpace(requested)
	m := strings.ToLower(raw)
	m = NormalizeKimiModelAlias(m)
	base, _ := ExtractKimiWorkEffort(m)
	if base == "" {
		base = m
	}
	switch base {
	case "", "default", "proxy", "auto", "kimi-work", "kimi-code", "kimi-for-coding",
		"kimi-for-coding-chat", "k3", "k3-max", "k3-agent-ultra":
		return KimiWorkDefaultModel
	case "k3-swarm", "k3-agent-swarm":
		return "k3-agent-swarm"
	case "k3-agent":
		return "k3-agent"
	case "k2d6-agent", "k2p6-agent", "k2p6", "k2d6":
		if base == "k2d6" {
			return "k2d6-agent"
		}
		return base
	default:
		// Pass through native ids (k3-agent, k2d6-agent, k2p6, …).
		if looksLikeKimiWorkModel(base) {
			return base
		}
		return raw
	}
}

// WithProviderForModel returns a copy of settings with Provider/Upstream switched
// to match the client-requested model id. Does NOT fall back to the UI "active"
// provider: empty/alias/unknown models route to xAI. Does not mutate the store.
func (s Settings) WithProviderForModel(requested string) Settings {
	req := strings.TrimSpace(requested)
	low := strings.ToLower(req)
	// No model or generic alias → xAI (never inherit UI global provider).
	if req == "" || low == "default" || low == "proxy" || low == "auto" ||
		low == "global" || low == "current" || low == "grok-desktop" {
		return s.WithProvider(ProviderXAI)
	}
	id := req
	if i := strings.LastIndex(id, "/models/"); i >= 0 {
		id = id[i+len("/models/"):]
	}
	id = normalizeOllieModelAlias(id)
	id = NormalizeKimiModelAlias(id)
	switch {
	case looksLikeCodexModel(id):
		return s.WithProvider(ProviderCodex)
	case looksLikeAccioModel(id):
		return s.WithProvider(ProviderAccio)
	case looksLikeKimiWorkModel(id):
		return s.WithProvider(ProviderKimiWork)
	case looksLikeGeminiModel(id):
		return s.WithProvider(ProviderGemini)
	case looksLikeQwenModel(id):
		// Before Ollie: its catalog hints contain "qwen-".
		return s.WithProvider(ProviderQwen)
	case looksLikeOpenCodeGoModel(id):
		return s.WithProvider(ProviderOpenCodeGo)
	case looksLikeOpenCodeZenModel(id):
		return s.WithProvider(ProviderOpenCodeZen)
	case looksLikeDeepSeekModel(id):
		// Before Ollie: its catalog hints contain "deepseek-".
		return s.WithProvider(ProviderDeepSeek)
	case looksLikeOllieModel(id):
		return s.WithProvider(ProviderOllie)
	case looksLikeXAIModel(id):
		return s.WithProvider(ProviderXAI)
	default:
		// Unknown id: treat as xAI so a leftover kimi_work UI setting cannot steal the request.
		return s.WithProvider(ProviderXAI)
	}
}

// WithProvider returns a copy of settings forced to provider (request-scoped routing).
// Also aligns DefaultModel when the previous default belongs to another provider.
func (s Settings) WithProvider(provider string) Settings {
	out := s
	p := strings.ToLower(strings.TrimSpace(provider))
	switch p {
	case ProviderKimiWork, "kimi", "kimi-work", "kimiwork", "moonshot-work":
		out.Provider = ProviderKimiWork
		out.UpstreamBase = KimiWorkUpstream
		out.APIMode = "chat"
		if out.DefaultModel == "" || !looksLikeKimiWorkModel(out.DefaultModel) {
			out.DefaultModel = KimiWorkDefaultModel
		}
	case ProviderOllie:
		out.Provider = ProviderOllie
		out.UpstreamBase = OllieUpstream
		out.APIMode = "chat"
		if out.DefaultModel == "" || looksLikeXAIModel(out.DefaultModel) || looksLikeKimiWorkModel(out.DefaultModel) || looksLikeGeminiModel(out.DefaultModel) {
			out.DefaultModel = OllieDefaultModel
		}
	case ProviderGemini:
		out.Provider = ProviderGemini
		if strings.TrimSpace(out.GeminiLocation) == "" {
			out.GeminiLocation = GeminiDefaultLocation
		}
		if out.DefaultModel == "" || looksLikeXAIModel(out.DefaultModel) || looksLikeKimiWorkModel(out.DefaultModel) || looksLikeOllieModel(out.DefaultModel) {
			out.DefaultModel = GeminiDefaultModel
		}
	case ProviderQwen, "qwenbridge", "qwen-bridge":
		out.Provider = ProviderQwen
		out.UpstreamBase = out.EffectiveQwenUpstream()
		out.APIMode = "chat"
		if out.DefaultModel == "" || looksLikeXAIModel(out.DefaultModel) || looksLikeKimiWorkModel(out.DefaultModel) || looksLikeGeminiModel(out.DefaultModel) {
			out.DefaultModel = QwenDefaultModel
		}
	case ProviderDeepSeek, "deep-seek", "ds":
		out.Provider = ProviderDeepSeek
		out.UpstreamBase = DeepSeekUpstream
		out.APIMode = "chat"
		if out.DefaultModel == "" || !looksLikeDeepSeekModel(out.DefaultModel) {
			out.DefaultModel = DeepSeekDefaultModel
		}
	case ProviderAccio, "accio-work", "phoenix":
		out.Provider = ProviderAccio
		out.UpstreamBase = "https://phoenix-gw.alibaba.com/api/adk/llm"
		out.APIMode = "chat"
		if out.DefaultModel == "" || !looksLikeAccioModel(out.DefaultModel) {
			out.DefaultModel = AccioDefaultModel
		}
	case ProviderOpenCodeZen, "opencode-zen", "opencode", "zen", "zen-free", "opencode-zen-free", "opencode zen", "opencode zen free":
		out.Provider = ProviderOpenCodeZen
		out.UpstreamBase = OpenCodeZenUpstream
		out.APIMode = "chat"
		if out.DefaultModel == "" || !looksLikeOpenCodeZenModel(out.DefaultModel) {
			out.DefaultModel = OpenCodeZenDefaultModel
		}
	case ProviderOpenCodeGo, "opencode-go", "opencode go":
		out.Provider = ProviderOpenCodeGo
		out.UpstreamBase = OpenCodeGoUpstream
		out.APIMode = "chat"
		if out.DefaultModel == "" || !looksLikeOpenCodeGoModel(out.DefaultModel) {
			out.DefaultModel = OpenCodeGoDefaultModel
		}
	case ProviderCodex, "codex", "openai-codex", "openai codex", "chatgpt":
		out.Provider = ProviderCodex
		out.UpstreamBase = CodexUpstream
		out.APIMode = "responses"
		if out.DefaultModel == "" || !looksLikeCodexModel(out.DefaultModel) {
			out.DefaultModel = CodexDefaultModel
		}
	case ProviderXAI, "grok", "x.ai":

		out.Provider = ProviderXAI
		out.UpstreamBase = DefaultUpstream
		out.APIMode = "responses"
		if out.DefaultModel == "" || looksLikeKimiWorkModel(out.DefaultModel) || looksLikeOllieModel(out.DefaultModel) || looksLikeGeminiModel(out.DefaultModel) {
			out.DefaultModel = DefaultModel
		}
	}
	return out
}

// isNativeModelForProvider reports whether requested looks like it belongs
// to the active provider (so force_default should not rewrite it).
func (s Settings) isNativeModelForProvider(requested string) bool {
	req := strings.TrimSpace(requested)
	if req == "" {
		return false
	}
	// Strip path/suffix for classification only.
	id := req
	if i := strings.LastIndex(id, "/models/"); i >= 0 {
		id = id[i+len("/models/"):]
	}
	low := strings.ToLower(id)
	switch {
	case strings.HasSuffix(low, "-responses"):
		id = id[:len(id)-len("-responses")]
	case strings.HasSuffix(low, "@responses"):
		id = id[:len(id)-len("@responses")]
	}
	switch s.NormalizedProvider() {
	case ProviderOllie:
		return looksLikeOllieModel(id) || looksLikeOllieModel(normalizeOllieModelAlias(id))
	case ProviderGemini:
		return looksLikeGeminiModel(id)
	case ProviderQwen:
		return looksLikeQwenModel(id)
	case ProviderDeepSeek:
		return looksLikeDeepSeekModel(id)
	case ProviderOpenCodeZen:
		return looksLikeOpenCodeZenModel(id)
	case ProviderOpenCodeGo:
		return looksLikeOpenCodeGoModel(id)
	case ProviderCodex:
		return looksLikeCodexModel(id)
	case ProviderKimiWork:
		return looksLikeKimiWorkModel(id) || looksLikeKimiWorkModel(NormalizeKimiModelAlias(id))
	default:
		return looksLikeXAIModel(id)
	}
}

// normalizeOllieModelAlias maps common short names to catalog ids.
func normalizeOllieModelAlias(id string) string {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "fable", "fable-5", "fable5", "fable 5", "claude-fable", "claude fable 5":
		return "claude-fable-5"
	case "sonnet", "sonnet-5", "claude-sonnet", "claude sonnet 5":
		return "claude-sonnet-5"
	case "opus", "opus-4", "opus-4-8", "claude-opus", "claude opus":
		return "claude-opus-4-8"
	default:
		return strings.TrimSpace(id)
	}
}

// NormalizeKimiModelAlias maps common friendly names (including spaced variants) to Kimi catalog ids.
func NormalizeKimiModelAlias(id string) string {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "kimi k3 max", "k3 max", "k3 agent", "k3 max (work)", "k3":
		return "k3-agent"
	case "k3 agent low", "k3 max low", "k3 low think":
		return "k3-agent-low"
	case "k3 agent medium", "k3 max medium", "k3 medium think":
		return "k3-agent-medium"
	case "k3 agent high", "k3 max high", "k3 high think":
		return "k3-agent-high"
	case "k3 agent xhigh", "k3 max xhigh", "k3 extra high", "k3 extra high think", "k3 xhigh think":
		return "k3-agent-xhigh"
	case "kimi k2", "k2 agent", "k2d6 agent", "k2p6 agent":
		return "k2p6-agent"
	default:
		return strings.TrimSpace(id)
	}
}

func looksLikeAccioModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(m, "accio/") || strings.HasPrefix(m, "accio-") || strings.Contains(m, "nexus-") || strings.Contains(m, "phoenix")
}

func (s Settings) ProviderDefaultModel() string {
	switch s.NormalizedProvider() {
	case ProviderOllie:
		return OllieDefaultModel
	case ProviderGemini:
		return GeminiDefaultModel
	case ProviderQwen:
		return QwenDefaultModel
	case ProviderDeepSeek:
		return DeepSeekDefaultModel
	case ProviderAccio:
		return AccioDefaultModel
	case ProviderOpenCodeZen:
		return OpenCodeZenDefaultModel
	case ProviderOpenCodeGo:
		return OpenCodeGoDefaultModel
	case ProviderCodex:
		return CodexDefaultModel
	case ProviderKimiWork:
		return KimiWorkDefaultModel
	default:
		return DefaultModel
	}
}

// ProviderAuthMode returns "auth" (session/account pool) or "api_key" (direct key/ADC).
func (s Settings) ProviderAuthMode() string {
	switch s.NormalizedProvider() {
	case ProviderXAI, ProviderKimiWork, ProviderAccio, ProviderCodex, ProviderGemini:
		return AuthModeSession
	default:
		return AuthModeAPIKey
	}
}

// NormalizedProvider returns xai|kimi_work|ollie|gemini|qwen.
func (s Settings) NormalizedProvider() string {
	p := strings.ToLower(strings.TrimSpace(s.Provider))
	switch p {
	case ProviderOllie, "olliechat", "ollie-chat":
		return ProviderOllie
	case ProviderGemini, "google", "vertex", "vertexai", "adc":
		return ProviderGemini
	case ProviderKimiWork, "kimi", "kimi-work", "kimiwork", "moonshot-work", "kimi-code":
		return ProviderKimiWork
	case ProviderQwen, "qwenbridge", "qwen-bridge":
		return ProviderQwen
	case ProviderDeepSeek, "deep-seek", "ds":
		return ProviderDeepSeek
	case ProviderAccio, "accio-work", "phoenix":
		return ProviderAccio
	case ProviderOpenCodeZen, "opencode-zen", "opencode", "zen", "zen-free", "opencode-zen-free", "opencode zen", "opencode zen free":
		return ProviderOpenCodeZen
	case ProviderOpenCodeGo, "opencode-go", "opencode go":
		return ProviderOpenCodeGo
	case ProviderCodex, "codex", "openai-codex", "openai codex", "chatgpt":
		return ProviderCodex
	case "", ProviderXAI, "grok", "x.ai", "cli":
		return ProviderXAI
	default:
		if strings.Contains(strings.ToLower(s.UpstreamBase), "olliechat") {
			return ProviderOllie
		}
		if strings.Contains(strings.ToLower(s.UpstreamBase), "agent-gw.kimi.com") ||
			strings.Contains(strings.ToLower(s.UpstreamBase), "kimi.com/coding") {
			return ProviderKimiWork
		}
		if strings.Contains(strings.ToLower(s.UpstreamBase), "aiplatform") ||
			strings.Contains(strings.ToLower(s.UpstreamBase), "generativelanguage") {
			return ProviderGemini
		}
		if strings.Contains(strings.ToLower(s.UpstreamBase), "opencode.ai/zen") {
			return ProviderOpenCodeZen
		}
		if strings.Contains(strings.ToLower(s.UpstreamBase), "chatgpt.com/backend-api/codex") {
			return ProviderCodex
		}
		return ProviderXAI
	}
}

func (s Settings) IsOllie() bool       { return s.NormalizedProvider() == ProviderOllie }
func (s Settings) IsXAI() bool         { return s.NormalizedProvider() == ProviderXAI }
func (s Settings) IsGemini() bool      { return s.NormalizedProvider() == ProviderGemini }
func (s Settings) IsKimiWork() bool    { return s.NormalizedProvider() == ProviderKimiWork }
func (s Settings) IsQwen() bool        { return s.NormalizedProvider() == ProviderQwen }
func (s Settings) IsDeepSeek() bool    { return s.NormalizedProvider() == ProviderDeepSeek }
func (s Settings) IsAccio() bool       { return s.NormalizedProvider() == ProviderAccio }
func (s Settings) IsOpenCodeZen() bool { return s.NormalizedProvider() == ProviderOpenCodeZen }
func (s Settings) IsOpenCodeGo() bool  { return s.NormalizedProvider() == ProviderOpenCodeGo }
func (s Settings) IsCodex() bool       { return s.NormalizedProvider() == ProviderCodex }
func (s Settings) IsSessionAuth() bool { return s.ProviderAuthMode() == AuthModeSession }

// HasDeepSeekKey reports whether a DeepSeek API key is configured. The stored
// value is an encrypted blob (or legacy plaintext) — never echo it out.
func (s Settings) HasDeepSeekKey() bool {
	return strings.TrimSpace(s.DeepSeekAPIKey) != ""
}

// DeepSeekAPIKeyPlain decrypts the stored DeepSeek API key. Returns "" when
// nothing is stored or the blob cannot be decrypted (different user/machine).
func (s Settings) DeepSeekAPIKeyPlain() string {
	if !s.HasDeepSeekKey() {
		return ""
	}
	out, err := secure.Decrypt(s.DeepSeekAPIKey)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (s Settings) HasOpenCodeGoKey() bool {
	return strings.TrimSpace(s.OpenCodeGoAPIKey) != ""
}

func (s Settings) OpenCodeGoAPIKeyPlain() string {
	if !s.HasOpenCodeGoKey() {
		return ""
	}
	out, err := secure.Decrypt(s.OpenCodeGoAPIKey)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// EffectiveUpstream returns the base URL including /v1 used for OpenAI-style paths.
// Gemini does not use this HTTP reverse-proxy base (it uses Vertex REST + ADC).
func (s Settings) EffectiveUpstream() string {
	switch s.NormalizedProvider() {
	case ProviderOllie:
		b := strings.TrimRight(strings.TrimSpace(s.UpstreamBase), "/")
		if b == "" || strings.Contains(strings.ToLower(b), "cli-chat-proxy") || strings.Contains(strings.ToLower(b), "grok.com") {
			return OllieUpstream
		}
		if !strings.HasSuffix(b, "/v1") {
			return b + "/v1"
		}
		return b
	case ProviderKimiWork:
		b := strings.TrimRight(strings.TrimSpace(s.UpstreamBase), "/")
		if b == "" || strings.Contains(strings.ToLower(b), "cli-chat-proxy") ||
			strings.Contains(strings.ToLower(b), "olliechat") ||
			strings.Contains(strings.ToLower(b), "aiplatform") {
			return KimiWorkUpstream
		}
		if !strings.HasSuffix(b, "/v1") {
			return b + "/v1"
		}
		return b
	case ProviderGemini:
		// Informational only — actual calls go through internal/gemini.
		loc := s.EffectiveGeminiLocation()
		proj := s.EffectiveGeminiProject()
		return fmt.Sprintf("https://aiplatform.googleapis.com/v1beta1/projects/%s/locations/%s", proj, loc)
	case ProviderQwen:
		return s.EffectiveQwenUpstream()
	case ProviderDeepSeek:
		// Official DeepSeek API; custom UpstreamBase honored as override.
		b := strings.TrimRight(strings.TrimSpace(s.UpstreamBase), "/")
		if b == "" || strings.Contains(strings.ToLower(b), "cli-chat-proxy") ||
			strings.Contains(strings.ToLower(b), "olliechat") ||
			strings.Contains(strings.ToLower(b), "aiplatform") ||
			strings.Contains(strings.ToLower(b), "agent-gw") {
			return DeepSeekUpstream
		}
		if !strings.HasSuffix(b, "/v1") {
			return b + "/v1"
		}
		return b
	case ProviderAccio:
		return "https://phoenix-gw.alibaba.com/api/adk/llm"
	case ProviderOpenCodeZen:
		return OpenCodeZenUpstream
	case ProviderOpenCodeGo:
		return OpenCodeGoUpstream
	case ProviderCodex:
		return CodexUpstream
	default:
		b := strings.TrimRight(strings.TrimSpace(s.UpstreamBase), "/")
		if b == "" || strings.Contains(strings.ToLower(b), "olliechat") ||
			strings.Contains(strings.ToLower(b), "aiplatform") ||
			strings.Contains(strings.ToLower(b), "agent-gw") {
			return DefaultUpstream
		}
		return b
	}
}

// EffectiveQwenUpstream returns the QwenBridge base URL including /v1.
// Falls back to the default local bridge address when unset; never inherits
// another provider's upstream from UpstreamBase.
func (s Settings) EffectiveQwenUpstream() string {
	b := strings.TrimRight(strings.TrimSpace(s.QwenUpstream), "/")
	if b == "" {
		return QwenDefaultUpstream
	}
	if !strings.HasSuffix(b, "/v1") {
		return b + "/v1"
	}
	return b
}

func (s Settings) EffectiveGeminiProject() string {
	if p := strings.TrimSpace(s.GeminiProject); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT")); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("GCLOUD_PROJECT")); p != "" {
		return p
	}
	// Last-known project from the user's working ADC setup.
	return "project-84a077f4-4a06-4d1b-ab1"
}

func (s Settings) EffectiveGeminiLocation() string {
	if l := strings.TrimSpace(s.GeminiLocation); l != "" {
		return l
	}
	if l := strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_LOCATION")); l != "" {
		return l
	}
	return GeminiDefaultLocation
}

// ApplyProviderDefaults mutates settings when switching provider (model + upstream).
// Always resets DefaultModel to a catalog id valid for that provider.
func (s *Settings) ApplyProviderDefaults(provider string) {
	s.Provider = strings.ToLower(strings.TrimSpace(provider))
	if s.Provider == "" {
		s.Provider = ProviderXAI
	}
	switch s.NormalizedProvider() {
	case ProviderOllie:
		s.Provider = ProviderOllie
		s.UpstreamBase = OllieUpstream
		s.APIMode = "chat"
		s.DefaultModel = OllieDefaultModel
		switch strings.ToLower(strings.TrimSpace(s.ReasoningEffort)) {
		case "xhigh", "high", "":
			s.ReasoningEffort = "low"
		}
	case ProviderGemini:
		s.Provider = ProviderGemini
		s.APIMode = "chat"
		s.DefaultModel = GeminiDefaultModel
		if strings.TrimSpace(s.GeminiLocation) == "" {
			s.GeminiLocation = GeminiDefaultLocation
		}
		if strings.TrimSpace(s.GeminiProject) == "" {
			s.GeminiProject = s.EffectiveGeminiProject()
		}
		// Informational upstream for UI/health.
		s.UpstreamBase = s.EffectiveUpstream()
	case ProviderKimiWork:
		s.Provider = ProviderKimiWork
		s.UpstreamBase = KimiWorkUpstream
		// agent-gw has no /responses — chat/completions only.
		s.APIMode = "chat"
		s.DefaultModel = KimiWorkDefaultModel
	case ProviderQwen:
		s.Provider = ProviderQwen
		s.UpstreamBase = s.EffectiveQwenUpstream()
		// QwenBridge speaks OpenAI chat/completions (+ its own /responses, but
		// the proxy wires qwen through chat/completions like Ollie/Kimi).
		s.APIMode = "chat"
		s.DefaultModel = QwenDefaultModel
	case ProviderDeepSeek:
		s.Provider = ProviderDeepSeek
		s.UpstreamBase = DeepSeekUpstream
		// DeepSeek API is OpenAI chat/completions only (no /responses).
		s.APIMode = "chat"
		s.DefaultModel = DeepSeekDefaultModel
	case ProviderAccio:
		s.Provider = ProviderAccio
		s.UpstreamBase = "https://phoenix-gw.alibaba.com/api/adk/llm"
		s.APIMode = "chat"
		s.DefaultModel = AccioDefaultModel
	case ProviderOpenCodeZen:
		s.Provider = ProviderOpenCodeZen
		s.UpstreamBase = OpenCodeZenUpstream
		s.APIMode = "chat"
		s.DefaultModel = OpenCodeZenDefaultModel
	case ProviderOpenCodeGo:
		s.Provider = ProviderOpenCodeGo
		s.UpstreamBase = OpenCodeGoUpstream
		s.APIMode = "chat"
		s.DefaultModel = OpenCodeGoDefaultModel
	case ProviderCodex:
		s.Provider = ProviderCodex
		s.UpstreamBase = CodexUpstream
		s.APIMode = "responses"
		s.DefaultModel = CodexDefaultModel
	default:
		s.Provider = ProviderXAI
		s.UpstreamBase = DefaultUpstream
		s.DefaultModel = DefaultModel
		s.APIMode = "responses"
	}
}

func looksLikeOllieModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	// Kimi Work models are not Ollie even if they contain "kimi".
	if looksLikeKimiWorkModel(m) {
		return false
	}
	if m == OllieDefaultModel {
		return true
	}
	if strings.Contains(m, "euromodels") || strings.Contains(m, "accounts/") {
		return true
	}
	ollieHints := []string{
		"claude-", "claude_", "gpt-5", "deepseek-", "qwen-", "minimax-",
		"glm-", "mimo-", "agnes-", "nemotron-", "north-mini", "big-pickle",
		"fable", "sonnet-5", "opus-4", "flash-free",
	}
	for _, h := range ollieHints {
		if strings.Contains(m, h) {
			return true
		}
	}
	// legacy ollie catalog may expose kimi-k2.6 etc. without agent suffix
	if strings.Contains(m, "kimi-k2") || strings.Contains(m, "kimi-k3") {
		return true
	}
	return false
}

func StripKimiEffortSuffix(m string) string {
	for _, suffix := range []string{"-low", "-medium", "-high", "-xhigh", "-extra-high", "-extra_high", "-max"} {
		if strings.HasSuffix(m, suffix) {
			return strings.TrimSuffix(m, suffix)
		}
	}
	return m
}

// ExtractKimiWorkEffort returns the reasoning-effort suffix embedded in a Kimi model alias.
// Examples: "k3-agent-high" → ("k3-agent", "high"); "k3-agent" → ("k3-agent", "").
func ExtractKimiWorkEffort(model string) (base string, effort string) {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, suffix := range []string{"-xhigh", "-extra-high", "-extra_high", "-max", "-high", "-medium", "-low"} {
		if strings.HasSuffix(m, suffix) {
			effort = strings.TrimPrefix(suffix, "-")
			effort = strings.ReplaceAll(effort, "extra-high", "xhigh")
			effort = strings.ReplaceAll(effort, "extra_high", "xhigh")
			base = strings.TrimSuffix(m, suffix)
			return base, effort
		}
	}
	return m, ""
}

func looksLikeKimiWorkModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	switch m {
	case "kimi-for-coding", "kimi-code", "k3-agent", "k3-agent-ultra",
		"k3-max", "k3-swarm", "k2d6-agent", "k2p6", "k2p6-agent", "kimi-work":
		return true
	}
	if strings.Contains(m, "kimi-for-coding") {
		return true
	}
	if strings.HasSuffix(m, "-agent") && (strings.HasPrefix(m, "k3") || strings.HasPrefix(m, "k2") || strings.Contains(m, "kimi")) {
		return true
	}
	if strings.Contains(m, "agent-swarm") {
		return true
	}
	// Variant effort suffixes: k3-agent-low, k3-agent-medium, etc.
	if base := StripKimiEffortSuffix(m); base != m && looksLikeKimiWorkModel(base) {
		return true
	}
	return false
}

// looksLikeQwenModel reports whether the id belongs to the QwenBridge provider.
// Bridge ids are dynamic but Qwen-prefixed (qwen3.8, qwen3.7-plus, …).
func looksLikeQwenModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	return strings.HasPrefix(m, "qwen")
}

// looksLikeDeepSeekModel reports whether the id belongs to the DeepSeek API
// (deepseek-chat, deepseek-reasoner, deepseek-v4-*, …).
func looksLikeDeepSeekModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	return strings.HasPrefix(m, "deepseek")
}

// looksLikeOpenCodeZenModel reports the public/free OpenCode Zen namespace.
// The opencode/ prefix is preferred because names such as deepseek-v4-flash
// are also used by other providers. Short aliases are kept for compatibility
// with the standalone D:\proxy opencode adapter.
func looksLikeOpenCodeZenModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	for _, suffix := range []string{"-responses", "@responses", "/responses"} {
		m = strings.TrimSuffix(m, suffix)
	}
	if strings.HasPrefix(m, "opencode/") {
		return true
	}
	// Bare effort-suffixed aliases (x-preview-f-free-max) route like their base id.
	if base, eff := ExtractZenEffort(m); eff != "" {
		m = base
	}
	switch m {
	case "deepseek-v4-flash-free", "deepseek-v4-flash",
		"big-pickle", "x-preview-f-free", "ox-alpha-free",
		"muse-spark-1.2-contributor-free", "muse-spark-1.2-contributor",
		"mimo-v2.5-free", "mimo-v2.5", "hy3-free", "hy3",
		"nemotron-3-ultra-free", "nemotron-3-ultra",
		"nemotron-3.5-lightning-free", "nemotron-3.5-lightning",
		"north-mini-code-free", "north-mini-code",
		"ling-3.0-flash-free", "ling-3.0-flash",
		"laguna-s-2.1-free", "laguna-s-2.1":
		return true
	default:
		return false
	}
}

// OpenCode Go uses a distinct namespace so its authenticated requests never
// collide with the keyless OpenCode Zen Free models on the shared local route.
func looksLikeOpenCodeGoModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, suffix := range []string{"-responses", "@responses", "/responses"} {
		m = strings.TrimSuffix(m, suffix)
	}
	return strings.HasPrefix(m, "opencode-go/")
}

func looksLikeCodexModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, suffix := range []string{"-responses", "@responses", "/responses"} {
		m = strings.TrimSuffix(m, suffix)
	}
	return strings.HasPrefix(m, "codex/")
}

// IsBareCodexModel reports official Codex slugs that may arrive without the
// local codex/ namespace from first-party Codex clients.
func IsBareCodexModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	switch m {
	case "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.2":
		return true
	default:
		return false
	}
}

func resolveCodexModel(model string) string {
	m := strings.TrimSpace(model)
	if strings.HasPrefix(strings.ToLower(m), "codex/") {
		return m[len("codex/"):]
	}
	return m
}

func resolveOpenCodeGoModel(model string) string {
	m := strings.TrimSpace(model)
	if strings.HasPrefix(strings.ToLower(m), "opencode-go/") {
		m = m[len("opencode-go/"):]
	}
	// Zcode has persisted this model once with sentence punctuation appended.
	// The Go gateway only accepts the exact catalog identifier.
	return strings.TrimSuffix(m, ".")
}

// ExtractZenEffort splits an effort suffix (-low/-high/-max) from a Zen model
// id, mirroring the Kimi agent aliases. Example:
// "opencode/x-preview-f-free-max" → ("opencode/x-preview-f-free", "max").
func ExtractZenEffort(model string) (base string, effort string) {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, suffix := range []string{"-max", "-high", "-low"} {
		if strings.HasSuffix(m, suffix) {
			return strings.TrimSuffix(m, suffix), strings.TrimPrefix(suffix, "-")
		}
	}
	return m, ""
}

func resolveOpenCodeZenModel(model string) string {
	m := strings.TrimSpace(model)
	if strings.HasPrefix(strings.ToLower(m), "opencode/") {
		m = m[len("opencode/"):]
	}
	// Effort-suffixed aliases (…-max) never reach the wire: split them off and
	// resolve the bare catalog id. Callers apply the effort to the request body.
	if base, eff := ExtractZenEffort(m); eff != "" {
		m = base
	}
	switch strings.ToLower(m) {
	case "deepseek-v4-flash":
		return "deepseek-v4-flash-free"
	case "ox-alpha-free":
		return "x-preview-f-free"
	case "mimo-v2.5":
		return "mimo-v2.5-free"
	case "hy3":
		return "hy3-free"
	case "nemotron-3-ultra":
		return "nemotron-3-ultra-free"
	case "nemotron-3.5-lightning":
		return "nemotron-3.5-lightning-free"
	case "north-mini-code":
		return "north-mini-code-free"
	case "ling-3.0-flash":
		return "ling-3.0-flash-free"
	case "laguna-s-2.1":
		return "laguna-s-2.1-free"
	case "muse-spark-1.2-contributor":
		return "muse-spark-1.2-contributor-free"
	default:
		return m
	}
}

func looksLikeXAIModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(m, "grok-") || strings.Contains(m, "grok-")
}

func looksLikeGeminiModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	return strings.Contains(m, "gemini") || strings.HasPrefix(m, "publishers/google/models/")
}

// SanitizeModelForProvider ensures DefaultModel matches the active provider.
func (s *Settings) SanitizeModelForProvider() {
	switch s.NormalizedProvider() {
	case ProviderOllie:
		if s.DefaultModel == "" || looksLikeXAIModel(s.DefaultModel) || looksLikeGeminiModel(s.DefaultModel) || looksLikeKimiWorkModel(s.DefaultModel) {
			s.DefaultModel = OllieDefaultModel
		}
	case ProviderGemini:
		if s.DefaultModel == "" || looksLikeXAIModel(s.DefaultModel) || looksLikeOllieModel(s.DefaultModel) || looksLikeKimiWorkModel(s.DefaultModel) {
			s.DefaultModel = GeminiDefaultModel
		}
		if strings.TrimSpace(s.GeminiLocation) == "" {
			s.GeminiLocation = GeminiDefaultLocation
		}
		if strings.TrimSpace(s.GeminiProject) == "" {
			s.GeminiProject = s.EffectiveGeminiProject()
		}
	case ProviderKimiWork:
		if s.DefaultModel == "" || looksLikeXAIModel(s.DefaultModel) || looksLikeOllieModel(s.DefaultModel) || looksLikeGeminiModel(s.DefaultModel) {
			s.DefaultModel = KimiWorkDefaultModel
		}
		if s.UpstreamBase == "" || strings.Contains(strings.ToLower(s.UpstreamBase), "cli-chat-proxy") ||
			strings.Contains(strings.ToLower(s.UpstreamBase), "olliechat") {
			s.UpstreamBase = KimiWorkUpstream
		}
		// Always force chat/completions for Kimi Work.
		s.APIMode = "chat"
	case ProviderQwen:
		// Deliberately no looksLikeOllieModel check: "qwen-*" ids trip Ollie hints.
		if s.DefaultModel == "" || looksLikeXAIModel(s.DefaultModel) || looksLikeKimiWorkModel(s.DefaultModel) || looksLikeGeminiModel(s.DefaultModel) {
			s.DefaultModel = QwenDefaultModel
		}
		s.UpstreamBase = s.EffectiveQwenUpstream()
		s.APIMode = "chat"
	case ProviderDeepSeek:
		if s.DefaultModel == "" || !looksLikeDeepSeekModel(s.DefaultModel) {
			s.DefaultModel = DeepSeekDefaultModel
		}
		s.UpstreamBase = DeepSeekUpstream
		s.APIMode = "chat"
	case ProviderAccio:
		s.Provider = ProviderAccio
		if s.DefaultModel == "" || !looksLikeAccioModel(s.DefaultModel) {
			s.DefaultModel = AccioDefaultModel
		}
		s.UpstreamBase = "https://phoenix-gw.alibaba.com/api/adk/llm"
		s.APIMode = "chat"
	case ProviderOpenCodeZen:
		s.Provider = ProviderOpenCodeZen
		if s.DefaultModel == "" || !looksLikeOpenCodeZenModel(s.DefaultModel) {
			s.DefaultModel = OpenCodeZenDefaultModel
		}
		s.UpstreamBase = OpenCodeZenUpstream
		s.APIMode = "chat"
	case ProviderOpenCodeGo:
		s.Provider = ProviderOpenCodeGo
		if s.DefaultModel == "" || !looksLikeOpenCodeGoModel(s.DefaultModel) {
			s.DefaultModel = OpenCodeGoDefaultModel
		}
		s.UpstreamBase = OpenCodeGoUpstream
		s.APIMode = "chat"
	case ProviderCodex:
		s.Provider = ProviderCodex
		if s.DefaultModel == "" || !looksLikeCodexModel(s.DefaultModel) {
			s.DefaultModel = CodexDefaultModel
		}
		s.UpstreamBase = CodexUpstream
		s.APIMode = "responses"
	default:
		// grok-4.5 was removed from the xAI Build pool when 4.6 launched.
		// Migrate persisted defaults while still allowing explicit 4.5 requests.
		if strings.EqualFold(strings.TrimSpace(s.DefaultModel), "grok-4.5") {
			s.DefaultModel = DefaultModel
		}
		if s.DefaultModel == "" || looksLikeOllieModel(s.DefaultModel) || looksLikeGeminiModel(s.DefaultModel) || looksLikeKimiWorkModel(s.DefaultModel) {
			s.DefaultModel = DefaultModel
		}
		if strings.Contains(strings.ToLower(s.UpstreamBase), "olliechat") ||
			strings.Contains(strings.ToLower(s.UpstreamBase), "aiplatform") ||
			strings.Contains(strings.ToLower(s.UpstreamBase), "agent-gw") {
			s.UpstreamBase = DefaultUpstream
		}
	}
}

// PickAccountForProvider selects an account for the given provider using the specified strategy.
// This replaces the global active account approach with per-provider load balancing.
// If strategy is empty or invalid, it falls back to StrategyRoundRobin for session-auth providers.
// Returns nil if no usable account exists for the provider.
func (s *Store) PickAccountForProvider(provider string, strategy LoadBalancerStrategy) *Account {
	want := s.normalizeProviderFilter(provider)

	// API-key providers don't have account pools. Accio sessions are stored as
	// normal provider accounts and are selected by the same load balancer.
	if want == ProviderOllie || want == ProviderGemini || want == ProviderQwen || want == ProviderDeepSeek || want == ProviderOpenCodeZen || want == ProviderOpenCodeGo {
		return nil
	}

	// Default strategy
	if !strategy.IsValid() {
		strategy = StrategyRoundRobin
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Collect usable accounts for this provider.
	// Prefer non-expired access tokens: expired rows with a refresh_token are
	// still candidates (ensureCreds will refresh), but round-robin across a
	// pile of dead RTs was causing intermittent invalid_grant failures while a
	// healthy CLI-synced account sat unused.
	var fresh, stale []Account
	for _, a := range s.accounts {
		if a.NormalizedProvider() != want || !a.Usable() {
			continue
		}
		// For non-Kimi, also check expiry — but if refresh token exists, still usable.
		if want != ProviderKimiWork && a.Expired() && a.RefreshToken == "" {
			continue
		}
		if want != ProviderKimiWork && a.Expired() {
			stale = append(stale, a)
			continue
		}
		fresh = append(fresh, a)
	}
	usable := fresh
	if len(usable) == 0 {
		usable = stale
	}
	if len(usable) == 0 {
		return nil
	}

	switch strategy {
	case StrategyActive:
		// Prefer the global active account if it belongs to this provider and is
		// in the preferred (fresh-first) pool.
		if id := s.settings.ActiveAccountID; id != "" {
			for _, a := range usable {
				if a.ID == id {
					cp := a
					return &cp
				}
			}
			// Active is only in the stale pool while fresher accounts exist — still
			// honour it only when nothing fresh is available (usable already prefers fresh).
		}
		// Fall through to round-robin.
		fallthrough
	case StrategyRoundRobin:
		state := s.providerStates[want]
		if state == nil {
			state = &ProviderState{}
			s.providerStates[want] = state
		}
		// Sort for deterministic order.
		for i := 0; i < len(usable); i++ {
			for j := i + 1; j < len(usable); j++ {
				if usable[j].ID < usable[i].ID {
					usable[i], usable[j] = usable[j], usable[i]
				}
			}
		}
		idx := state.RRIndex % len(usable)
		state.RRIndex++
		cp := usable[idx]
		logging.Debug("store.account.picked", "provider", want, "account_id", cp.ID, "strategy", string(strategy), "fresh_pool", len(fresh), "stale_pool", len(stale))
		return &cp
	case StrategyLeastUsed:
		// Pick the account with the lowest in-flight request count.
		best := &usable[0]
		for i := 1; i < len(usable); i++ {
			if usable[i].requestCount < best.requestCount {
				best = &usable[i]
			}
		}
		cp := *best
		logging.Debug("store.account.picked", "provider", want, "account_id", cp.ID, "strategy", string(strategy))
		return &cp
	case StrategyRandom:
		// Seed with time for simplicity; good enough for load balancing.
		rng := time.Now().UnixNano()
		idx := int(rng % int64(len(usable)))
		cp := usable[idx]
		logging.Debug("store.account.picked", "provider", want, "account_id", cp.ID, "strategy", string(strategy))
		return &cp
	}

	return nil
}

// HasUsableAccountForProvider reports whether any account for the provider is
// usable, WITHOUT advancing the round-robin cursor or mutating balancer state.
// Use this in pollers/readiness checks; PickAccountForProvider advances RR.
func (s *Store) HasUsableAccountForProvider(provider string) bool {
	want := s.normalizeProviderFilter(provider)
	if want == ProviderOllie || want == ProviderGemini || want == ProviderQwen || want == ProviderDeepSeek || want == ProviderOpenCodeZen || want == ProviderOpenCodeGo {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.accounts {
		if a.NormalizedProvider() != want || !a.Usable() {
			continue
		}
		if want != ProviderKimiWork && a.Expired() && a.RefreshToken == "" {
			continue
		}
		return true
	}
	return false
}

// IncAccountRequestCount atomically increments the in-flight request counter for an account.
// Call DecAccountRequestCount when the request completes.
func (s *Store) IncAccountRequestCount(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.accounts[id]; ok {
		a.requestCount++
		s.accounts[id] = a
	}
}

// DecAccountRequestCount atomically decrements the in-flight request counter.
func (s *Store) DecAccountRequestCount(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.accounts[id]; ok {
		if a.requestCount > 0 {
			a.requestCount--
		}
		s.accounts[id] = a
	}
}

// SetLoadBalancerStrategy sets the default strategy for a provider (persisted in settings).
func (s *Store) SetLoadBalancerStrategy(provider string, strategy LoadBalancerStrategy) error {
	want := s.normalizeProviderFilter(provider)
	return s.UpdateSettings(func(st *Settings) {
		if st.LoadBalancerStrategies == nil {
			st.LoadBalancerStrategies = map[string]string{}
		}
		st.LoadBalancerStrategies[want] = string(strategy)
	})
}

// GetLoadBalancerStrategy returns the strategy for a provider.
func (s *Store) GetLoadBalancerStrategy(provider string) LoadBalancerStrategy {
	want := s.normalizeProviderFilter(provider)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.settings.LoadBalancerStrategies != nil {
		if v, ok := s.settings.LoadBalancerStrategies[want]; ok {
			return LoadBalancerStrategy(v)
		}
	}
	return StrategyRoundRobin
}

// WithAccountRefreshLock serializes rotating refresh tokens across GUI and
// headless proxy processes sharing this store. Stale locks from a crashed
// process expire after the longest expected token request.
func (s *Store) WithAccountRefreshLock(ctx context.Context, accountID string, fn func() error) error {
	if fn == nil {
		return nil
	}
	lockDir := filepath.Join(s.root, "locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(accountID))
	lockPath := filepath.Join(lockDir, fmt.Sprintf("refresh-%x.lock", sum[:12]))
	for {
		lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(lock, "%d\n", os.Getpid())
			defer func() {
				_ = lock.Close()
				_ = os.Remove(lockPath)
			}()
			// The other process may have rotated and persisted the refresh token
			// while this process waited for the lock.
			if s.db != nil {
				if err := s.ReloadAccountsFromDB(); err != nil {
					return err
				}
			}
			return fn()
		}
		if !os.IsExist(err) {
			return err
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > 2*time.Minute {
			_ = os.Remove(lockPath)
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func ProviderCatalog() []map[string]any {
	return []map[string]any{
		{
			"id": ProviderXAI, "name": "Grok (xAI)", "auth_mode": AuthModeSession,
			"description":   "OAuth device login · multi-conta",
			"default_model": DefaultModel, "default_api": "responses",
		},
		{
			"id": ProviderCodex, "name": "OpenAI Codex (ChatGPT)", "auth_mode": AuthModeSession,
			"description":   "OAuth oficial do Codex - usa a assinatura ChatGPT da conta",
			"default_model": CodexDefaultModel, "default_api": "responses",
		},
		{
			"id": ProviderKimiWork, "name": "Kimi Work", "auth_mode": AuthModeSession,
			"description":   "Google login → sk-kimi · até 3 contas · rotação + re-login HTTP",
			"default_model": KimiWorkDefaultModel, "default_api": "chat",
		},
		{
			"id": ProviderOllie, "name": "OllieChat", "auth_mode": AuthModeAPIKey,
			"description":   "API keyless · sem pool de contas",
			"default_model": OllieDefaultModel, "default_api": "chat",
		},
		{
			"id": ProviderGemini, "name": "Gemini (AI Studio)", "auth_mode": AuthModeSession,
			"description":   "Google AI Studio · Chrome login · pool de contas local",
			"default_model": GeminiDefaultModel, "default_api": "chat",
		},
		{
			"id": ProviderQwen, "name": "Qwen (QwenBridge)", "auth_mode": AuthModeAPIKey,
			"description":   "QwenBridge local · base URL + API key · sem pool de contas",
			"default_model": QwenDefaultModel, "default_api": "chat",
		},
		{
			"id": ProviderDeepSeek, "name": "DeepSeek", "auth_mode": AuthModeAPIKey,
			"description":   "DeepSeek API oficial · API key criptografada (DPAPI) · sem pool de contas",
			"default_model": DeepSeekDefaultModel, "default_api": "chat",
		},
		{
			"id": ProviderOpenCodeZen, "name": "OpenCode Zen Free", "auth_mode": AuthModeAPIKey,
			"description":   "Zen direto - bearer publico - sem terminal/opencode serve - modelos free",
			"default_model": OpenCodeZenDefaultModel, "default_api": "chat",
		},
		{
			"id": ProviderOpenCodeGo, "name": "OpenCode Go", "auth_mode": AuthModeAPIKey,
			"description":   "API key de opencode.ai/auth - gateway OpenCode direto - sem terminal",
			"default_model": OpenCodeGoDefaultModel, "default_api": "chat",
		},
		{
			"id": ProviderAccio, "name": "Accio", "auth_mode": AuthModeSession,
			"description":   "Accio/Phoenix · login OAuth · refresh automático",
			"default_model": AccioDefaultModel, "default_api": "chat",
		},
	}
}

type UsageTotals struct {
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Requests         int64   `json:"requests"`
	CostUSD          float64 `json:"cost_usd"`
	// Latency aggregates (ms)
	LatencySumMs   int64 `json:"latency_sum_ms"`
	TTFTSumMs      int64 `json:"ttft_sum_ms"`
	LatencySamples int64 `json:"latency_samples"`
}

// RequestSample is one completed turn for charts / history.
type RequestSample struct {
	ID               string  `json:"id"`
	At               string  `json:"at"` // RFC3339
	AccountID        string  `json:"account_id"`
	Model            string  `json:"model"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	LatencyMs        int64   `json:"latency_ms"`
	TTFTMs           int64   `json:"ttft_ms"`
	Estimated        bool    `json:"estimated"`
}

// Layout under AppData:
//
//	%LOCALAPPDATA%/GrokDesktop/
//	  grokdesktop.db   (SQLite — accounts, settings, usage, history)
//	  accounts/        (legacy JSON, imported once then kept as backup)
//	  settings.json    (legacy)
//	  usage.json       (legacy)
//	  history.json     (legacy)
//	  logs/
type Store struct {
	mu       sync.RWMutex
	root     string
	db       *sql.DB
	settings Settings
	usage    map[string]UsageTotals
	// accounts loaded from SQLite (JSON migrated on first open)
	accounts map[string]Account
	// recent request samples (persisted, capped)
	history []RequestSample
	// providerStates tracks per-provider load balancer state (round-robin, etc).
	providerStates map[string]*ProviderState
}

func DefaultDataDir() (string, error) {
	// Windows: %LOCALAPPDATA%\GrokDesktop
	// macOS:   ~/Library/Application Support/GrokDesktop
	// Linux:   ~/.local/share/GrokDesktop
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(base, AppName), nil
	}
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", AppName), nil
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, AppName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", AppName), nil
}

func Open(root string) (*Store, error) {
	if root == "" {
		dir, err := DefaultDataDir()
		if err != nil {
			return nil, err
		}
		root = dir
	}
	for _, sub := range []string{"", "accounts", "logs"} {
		p := root
		if sub != "" {
			p = filepath.Join(root, sub)
		}
		if err := os.MkdirAll(p, 0o700); err != nil {
			return nil, err
		}
	}
	s := &Store{
		root:           root,
		accounts:       map[string]Account{},
		usage:          map[string]UsageTotals{},
		settings:       defaultSettings(),
		providerStates: map[string]*ProviderState{},
	}
	if err := s.initDB(); err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	// Import legacy JSON once into SQLite (idempotent).
	if _, err := s.migrateJSONToSQLite(); err != nil {
		// non-fatal: continue with empty/partial
	}
	if err := s.loadFromSQLite(); err != nil {
		return nil, fmt.Errorf("sqlite load: %w", err)
	}
	// Migrate legacy plaintext Codex credentials under the same cross-process
	// lock used by refresh-token rotation.
	var codexAccountIDs []string
	for id, account := range s.accounts {
		if account.NormalizedProvider() == ProviderCodex {
			codexAccountIDs = append(codexAccountIDs, id)
		}
	}
	for _, id := range codexAccountIDs {
		if err := s.migrateCodexCredentialsAtRest(id); err != nil {
			return nil, fmt.Errorf("protect Codex credentials: %w", err)
		}
	}
	// DeepSeek key vault: file is source of truth, settings blob is a mirror.
	s.syncDeepSeekKeyVault()
	// OpenCode Go key vault: same treatment (stale processes re-saving
	// settings must not be able to wipe the user key).
	s.syncOpenCodeGoKeyVault()
	// One-time migrations from older app formats
	_ = s.migrateFromLegacy()
	// Bump stale client version baked into older installs.
	if staleGrokClientVersion(s.settings.ClientVersion) {
		s.settings.ClientVersion = DefaultClientVersion
		_ = s.saveSettingsLocked()
	}
	// Fix cross-provider leftover models (e.g. claude-* stuck after switching back to xAI).
	beforeModel := s.settings.DefaultModel
	beforeEffort := s.settings.ReasoningEffort
	s.settings.SanitizeModelForProvider()
	if s.settings.IsOllie() {
		switch strings.ToLower(strings.TrimSpace(s.settings.ReasoningEffort)) {
		case "xhigh", "high":
			s.settings.ReasoningEffort = "low"
		}
	}
	if s.settings.DefaultModel != beforeModel || s.settings.ReasoningEffort != beforeEffort {
		_ = s.saveSettingsLocked()
	}
	// ensure active account belongs to current provider when possible
	if s.settings.ActiveAccountID != "" {
		if a, ok := s.accounts[s.settings.ActiveAccountID]; !ok {
			s.settings.ActiveAccountID = ""
		} else if a.NormalizedProvider() != s.settings.NormalizedProvider() && s.settings.IsSessionAuth() {
			// keep id but PreferHealthyActive will pick a usable one for active provider
		}
	}
	if s.settings.ActiveAccountID == "" {
		want := s.settings.NormalizedProvider()
		for id, a := range s.accounts {
			if a.NormalizedProvider() == want {
				s.settings.ActiveAccountID = id
				break
			}
		}
		if s.settings.ActiveAccountID != "" {
			_ = s.saveSettingsLocked()
		}
	}
	// Pull fresher tokens from official Grok CLI if present.
	if n, err := s.SyncFromGrokCLI(); err == nil && n > 0 {
		_ = s.PreferHealthyActive()
	} else {
		_ = s.PreferHealthyActive()
	}
	return s, nil
}

// Close releases the SQLite handle.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		err := s.db.Close()
		s.db = nil
		return err
	}
	return nil
}

// staleGrokClientVersion is true for empty / pre-1.0.13 identities that
// cli-chat-proxy treats as chat-only (no function tools).
func staleGrokClientVersion(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || v == DefaultClientVersion {
		return v == ""
	}
	if strings.HasPrefix(v, "0.2.") {
		return true
	}
	switch v {
	case "1.0.4", "1.0.5":
		return true
	}
	return false
}

func defaultSettings() Settings {
	force := false
	return Settings{
		Provider:          ProviderXAI,
		DefaultModel:      DefaultModel,
		ReasoningEffort:   DefaultEffort,
		APIMode:           "responses",
		UpstreamBase:      DefaultUpstream,
		ClientVersion:     DefaultClientVersion,
		ProxyListen:       "127.0.0.1:8787",
		ProxyEnabled:      true,
		StoreResponses:    true,
		ForceDefaultModel: &force,
	}
}

func (s *Store) Root() string { return s.root }

// ---------- DeepSeek key vault ----------
//
// The encrypted DeepSeek key lives in its own file under secrets/ (the vault).
// The Settings blob field is only a mirror: if a legacy binary re-saves
// settings.json without the field, the vault survives and is re-injected on
// the next Open.

func (s *Store) secretsDir() string { return filepath.Join(s.root, "secrets") }

func (s *Store) deepSeekKeyPath() string { return filepath.Join(s.secretsDir(), "deepseek.key") }

func (s *Store) openCodeGoKeyPath() string { return filepath.Join(s.secretsDir(), "opencode-go.key") }

// WriteOpenCodeGoKeyFile atomically persists the encrypted blob (0600, dir 0700).
func (s *Store) WriteOpenCodeGoKeyFile(encryptedBlob string) error {
	if encryptedBlob == "" {
		return os.Remove(s.openCodeGoKeyPath())
	}
	if err := os.MkdirAll(s.secretsDir(), 0o700); err != nil {
		return err
	}
	tmp := s.openCodeGoKeyPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(encryptedBlob), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.openCodeGoKeyPath())
}

func (s *Store) readOpenCodeGoKeyFile() string {
	b, err := os.ReadFile(s.openCodeGoKeyPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// syncOpenCodeGoKeyVault runs on Open: the vault file is the source of truth.
// A legacy blob left in settings is migrated into the vault (one-way).
func (s *Store) syncOpenCodeGoKeyVault() {
	if b := s.readOpenCodeGoKeyFile(); b != "" {
		s.settings.OpenCodeGoAPIKey = b
		return
	}
	if secure.HasCiphertext(s.settings.OpenCodeGoAPIKey) {
		_ = s.WriteOpenCodeGoKeyFile(s.settings.OpenCodeGoAPIKey)
	}
}

// WriteDeepSeekKeyFile atomically persists the encrypted blob (0600, dir 0700).
func (s *Store) WriteDeepSeekKeyFile(encryptedBlob string) error {
	if encryptedBlob == "" {
		return os.Remove(s.deepSeekKeyPath())
	}
	if err := os.MkdirAll(s.secretsDir(), 0o700); err != nil {
		return err
	}
	tmp := s.deepSeekKeyPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(encryptedBlob), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.deepSeekKeyPath())
}

func (s *Store) readDeepSeekKeyFile() string {
	b, err := os.ReadFile(s.deepSeekKeyPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// syncDeepSeekKeyVault runs on Open: the vault file is the source of truth.
// A legacy blob left in settings is migrated into the vault (one-way).
func (s *Store) syncDeepSeekKeyVault() {
	if b := s.readDeepSeekKeyFile(); b != "" {
		s.settings.DeepSeekAPIKey = b
		return
	}
	if secure.HasCiphertext(s.settings.DeepSeekAPIKey) {
		_ = s.WriteDeepSeekKeyFile(s.settings.DeepSeekAPIKey)
	}
}

func (s *Store) settingsPath() string { return filepath.Join(s.root, "settings.json") }
func (s *Store) usagePath() string    { return filepath.Join(s.root, "usage.json") }
func (s *Store) historyPath() string  { return filepath.Join(s.root, "history.json") }
func (s *Store) accountsDir() string  { return filepath.Join(s.root, "accounts") }
func (s *Store) accountPath(id string) string {
	safe := id
	for _, c := range []string{`\`, `/`, `:`, `*`, `?`, `"`, `<`, `>`, `|`} {
		safe = strings.ReplaceAll(safe, c, "_")
	}
	return filepath.Join(s.accountsDir(), safe+".json")
}

func (s *Store) loadAll() error {
	// settings
	if b, err := os.ReadFile(s.settingsPath()); err == nil {
		b = stripUTF8BOM(b)
		var st Settings
		if err := json.Unmarshal(b, &st); err == nil {
			s.settings = mergeSettings(defaultSettings(), st)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	// usage
	if b, err := os.ReadFile(s.usagePath()); err == nil {
		var u map[string]UsageTotals
		if json.Unmarshal(b, &u) == nil && u != nil {
			s.usage = u
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	// history
	if b, err := os.ReadFile(s.historyPath()); err == nil {
		var h []RequestSample
		if json.Unmarshal(b, &h) == nil {
			s.history = h
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	// accounts
	entries, err := os.ReadDir(s.accountsDir())
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.accountsDir(), e.Name()))
		if err != nil {
			continue
		}
		var a Account
		if json.Unmarshal(b, &a) != nil || a.ID == "" {
			continue
		}
		// Legacy xAI accounts have no provider field.
		if strings.TrimSpace(a.Provider) == "" {
			a.Provider = ProviderXAI
		}
		a.Provider = a.NormalizedProvider()
		if decryptAccountCredentials(&a) != nil {
			continue
		}
		// Accept OAuth bearer or provider API keys (Kimi sk-kimi).
		if a.AccessToken == "" && a.APIKey == "" {
			continue
		}
		// Kimi keys sometimes stored only in AccessToken by mistake.
		if a.NormalizedProvider() == ProviderKimiWork && a.APIKey == "" && strings.HasPrefix(a.AccessToken, "sk-kimi-") {
			a.APIKey = a.AccessToken
		}
		s.accounts[a.ID] = a
	}

	// ensure active account valid
	if s.settings.ActiveAccountID != "" {
		if _, ok := s.accounts[s.settings.ActiveAccountID]; !ok {
			s.settings.ActiveAccountID = ""
		}
	}
	if s.settings.ActiveAccountID == "" {
		for id := range s.accounts {
			s.settings.ActiveAccountID = id
			break
		}
	}
	return nil
}

func mergeSettings(base, over Settings) Settings {
	if over.Provider != "" {
		base.Provider = over.Provider
	}
	if over.DefaultModel != "" {
		base.DefaultModel = over.DefaultModel
	}
	if over.ReasoningEffort != "" {
		base.ReasoningEffort = over.ReasoningEffort
	}
	if over.APIMode != "" {
		base.APIMode = over.APIMode
	}
	if over.UpstreamBase != "" {
		base.UpstreamBase = over.UpstreamBase
	}
	if over.ClientVersion != "" {
		base.ClientVersion = over.ClientVersion
	}
	if over.ProxyListen != "" {
		base.ProxyListen = over.ProxyListen
	}
	base.ProxyEnabled = over.ProxyEnabled
	if over.ProxyAPIKey != "" {
		base.ProxyAPIKey = over.ProxyAPIKey
	}
	base.StoreResponses = over.StoreResponses
	if over.ForceDefaultModel != nil {
		base.ForceDefaultModel = over.ForceDefaultModel
	}
	if over.GeminiProject != "" {
		base.GeminiProject = over.GeminiProject
	}
	if over.GeminiLocation != "" {
		base.GeminiLocation = over.GeminiLocation
	}
	if over.QwenAPIKey != "" {
		base.QwenAPIKey = over.QwenAPIKey
	}
	if over.DeepSeekAPIKey != "" {
		base.DeepSeekAPIKey = over.DeepSeekAPIKey
	}
	if over.QwenUpstream != "" {
		base.QwenUpstream = over.QwenUpstream
	}
	if over.OpenCodeGoAPIKey != "" {
		base.OpenCodeGoAPIKey = over.OpenCodeGoAPIKey
	}
	base.KimiStealthHeadless = over.KimiStealthHeadless
	base.GoogleEmail = over.GoogleEmail
	base.GooglePassword = over.GooglePassword
	if over.LoadBalancerStrategies != nil {
		base.LoadBalancerStrategies = over.LoadBalancerStrategies
	}
	if over.SystemPrompts != nil {
		base.SystemPrompts = over.SystemPrompts
	}
	if over.ActiveAccountID != "" {
		base.ActiveAccountID = over.ActiveAccountID
	}
	if over.ThemeAccent != "" {
		base.ThemeAccent = over.ThemeAccent
	}
	return base
}

func stripUTF8BOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		// Windows does not replace an existing destination atomically.
		_ = os.Remove(path)
		if replaceErr := os.Rename(tmp, path); replaceErr != nil {
			return replaceErr
		}
	}
	return nil
}

func (s *Store) saveSettingsLocked() error {
	if s.db != nil {
		if err := s.saveSettingsDB(s.settings); err != nil {
			return err
		}
	}
	// dual-write legacy JSON for safety during transition
	_ = writeJSON(s.settingsPath(), s.settings)
	return nil
}

func (s *Store) saveUsageLocked() error {
	if s.db != nil {
		if err := s.saveUsageDB(s.usage); err != nil {
			return err
		}
	}
	_ = writeJSON(s.usagePath(), s.usage)
	return nil
}

func (s *Store) saveHistoryLocked() error {
	// history rows are inserted one-by-one via insertHistoryDB in RecordRequest
	_ = writeJSON(s.historyPath(), s.history)
	return nil
}

func (s *Store) saveAccountLocked(a Account) error {
	a.DeviceID = cleanDeviceID(a.DeviceID)
	stored, err := accountForStorage(a)
	if err != nil {
		return err
	}
	if s.db != nil {
		if err := s.saveAccountDB(stored); err != nil {
			return err
		}
	}
	// dual-write JSON backup; a failed backup must not silently leave an older
	// plaintext credential file behind.
	return writeJSON(s.accountPath(a.ID), stored)
}

func (s *Store) migrateCodexCredentialsAtRest(accountID string) error {
	account, ok := s.GetAccount(accountID)
	if !ok || account == nil || account.NormalizedProvider() != ProviderCodex {
		return nil
	}
	needsMigration, err := s.codexCredentialsNeedMigration(*account)
	if err != nil || !needsMigration {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return s.WithAccountRefreshLock(ctx, accountID, func() error {
		account, ok := s.GetAccount(accountID)
		if !ok || account == nil || account.NormalizedProvider() != ProviderCodex {
			return nil
		}
		needsMigration, err := s.codexCredentialsNeedMigration(*account)
		if err != nil {
			return err
		}
		if !needsMigration {
			return nil
		}
		return s.UpsertAccount(*account)
	})
}

func (s *Store) codexCredentialsNeedMigration(account Account) (bool, error) {
	var dbAccess, dbRefresh string
	if s.db != nil {
		if err := s.db.QueryRow(`SELECT access_token, refresh_token FROM accounts WHERE id = ?`, account.ID).Scan(&dbAccess, &dbRefresh); err != nil {
			return false, err
		}
		if credentialNeedsEncryption(dbAccess) || credentialNeedsEncryption(dbRefresh) {
			return true, nil
		}
	}
	backup, err := os.ReadFile(s.accountPath(account.ID))
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var stored Account
	if json.Unmarshal(backup, &stored) != nil {
		return true, nil
	}
	return credentialNeedsEncryption(stored.AccessToken) || credentialNeedsEncryption(stored.RefreshToken), nil
}

func credentialNeedsEncryption(value string) bool {
	return strings.TrimSpace(value) != "" && !secure.HasCiphertext(value)
}

func accountForStorage(a Account) (Account, error) {
	if a.NormalizedProvider() != ProviderCodex {
		return a, nil
	}
	var err error
	if !secure.HasCiphertext(a.AccessToken) {
		a.AccessToken, err = secure.Encrypt(a.AccessToken)
		if err != nil {
			return Account{}, fmt.Errorf("encrypt Codex access token: %w", err)
		}
	}
	if !secure.HasCiphertext(a.RefreshToken) {
		a.RefreshToken, err = secure.Encrypt(a.RefreshToken)
		if err != nil {
			return Account{}, fmt.Errorf("encrypt Codex refresh token: %w", err)
		}
	}
	return a, nil
}

func decryptAccountCredentials(a *Account) error {
	if a == nil || a.NormalizedProvider() != ProviderCodex {
		return nil
	}
	accessToken, err := secure.Decrypt(a.AccessToken)
	if err != nil {
		return fmt.Errorf("decrypt Codex access token: %w", err)
	}
	refreshToken, err := secure.Decrypt(a.RefreshToken)
	if err != nil {
		return fmt.Errorf("decrypt Codex refresh token: %w", err)
	}
	a.AccessToken = accessToken
	a.RefreshToken = refreshToken
	return nil
}

func (s *Store) Path() string { return s.root }

// migrateFromLegacy imports:
//  1. %USERPROFILE%\.grok-openai-proxy\desktop\state.json (old desktop monolith)
//  2. %USERPROFILE%\.grok-openai-proxy\auth.json (python proxy oauth)
func (s *Store) migrateFromLegacy() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.accounts) > 0 {
		return nil // already has data
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	// 1) old desktop state
	oldState := filepath.Join(home, ".grok-openai-proxy", "desktop", "state.json")
	if b, err := os.ReadFile(oldState); err == nil {
		var d struct {
			Accounts []Account              `json:"accounts"`
			Settings Settings               `json:"settings"`
			Usage    map[string]UsageTotals `json:"usage"`
		}
		if json.Unmarshal(b, &d) == nil {
			for _, a := range d.Accounts {
				if a.ID == "" || a.AccessToken == "" {
					continue
				}
				s.accounts[a.ID] = a
				_ = s.saveAccountLocked(a)
			}
			if len(d.Accounts) > 0 {
				s.settings = mergeSettings(s.settings, d.Settings)
				if d.Usage != nil {
					s.usage = d.Usage
					_ = s.saveUsageLocked()
				}
				_ = s.saveSettingsLocked()
				return nil
			}
		}
	}

	// 2) python proxy auth.json (flat single account)
	authPath := filepath.Join(home, ".grok-openai-proxy", "auth.json")
	if b, err := os.ReadFile(authPath); err == nil {
		var flat map[string]any
		if json.Unmarshal(b, &flat) == nil {
			// our python format: access_token, refresh_token, ...
			if at, _ := flat["access_token"].(string); at != "" {
				a := Account{
					ID:           "migrated",
					Label:        "Imported",
					AccessToken:  at,
					RefreshToken: str(flat["refresh_token"]),
					ClientID:     or(str(flat["client_id"]), DefaultClientID),
					Issuer:       or(str(flat["issuer"]), DefaultIssuer),
					Scope:        str(flat["scope"]),
					Email:        str(flat["email"]),
					UserID:       str(flat["user_id"]),
					TeamID:       str(flat["team_id"]),
					CreatedAt:    time.Now().UTC(),
					UpdatedAt:    time.Now().UTC(),
				}
				if a.Email != "" {
					a.Label = a.Email
					if a.UserID != "" {
						a.ID = a.UserID
					}
				}
				if exp := str(flat["expires_at"]); exp != "" {
					if t, err := time.Parse(time.RFC3339Nano, exp); err == nil {
						a.ExpiresAt = t.UTC()
					} else if t, err := time.Parse(time.RFC3339, exp); err == nil {
						a.ExpiresAt = t.UTC()
					}
				}
				s.accounts[a.ID] = a
				s.settings.ActiveAccountID = a.ID
				_ = s.saveAccountLocked(a)
				_ = s.saveSettingsLocked()
			}
		}
	}
	return nil
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

func (s *Store) UpdateSettings(fn func(*Settings)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.settings)
	return s.saveSettingsLocked()
}

func (s *Store) ListAccounts() []Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a)
	}
	return out
}

// ListAccountsForProvider returns accounts belonging to provider (empty provider → active settings provider).
func (s *Store) ListAccountsForProvider(provider string) []Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	want := strings.ToLower(strings.TrimSpace(provider))
	if want == "" {
		want = s.settings.NormalizedProvider()
	}
	switch want {
	case "kimi", "kimi-work", "kimiwork":
		want = ProviderKimiWork
	case "grok", "x.ai":
		want = ProviderXAI
	case "codex", "openai-codex", "chatgpt":
		want = ProviderCodex
	}
	out := make([]Account, 0)
	for _, a := range s.accounts {
		if a.NormalizedProvider() == want {
			out = append(out, a)
		}
	}
	return out
}

func (s *Store) PublicAccounts() []map[string]any {
	return s.PublicAccountsForProvider("")
}

// PublicAccountsForProvider filters the account list for the UI rail / modal.
func (s *Store) PublicAccountsForProvider(provider string) []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	want := strings.ToLower(strings.TrimSpace(provider))
	if want == "" {
		want = s.settings.NormalizedProvider()
	}
	switch want {
	case "kimi", "kimi-work", "kimiwork":
		want = ProviderKimiWork
	case "grok", "x.ai":
		want = ProviderXAI
	case "codex", "openai-codex", "chatgpt":
		want = ProviderCodex
	}
	// API-key providers have no session account pool. Accio accounts are
	// synchronized from the independent native client into this same UI pool.
	if want == ProviderOllie || want == ProviderGemini || want == ProviderQwen || want == ProviderDeepSeek || want == ProviderOpenCodeZen || want == ProviderOpenCodeGo {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(s.accounts))
	for _, a := range s.accounts {
		if a.NormalizedProvider() != want {
			continue
		}
		u := s.usage[a.ID]
		keyHint := ""
		if k := a.APIKey; k != "" {
			if len(k) > 14 {
				keyHint = k[:10] + "…" + k[len(k)-4:]
			} else {
				keyHint = k
			}
		}
		hasWeb := a.NormalizedProvider() == ProviderKimiWork &&
			(strings.TrimSpace(a.RefreshToken) != "" ||
				(strings.TrimSpace(a.AccessToken) != "" && !strings.HasPrefix(strings.TrimSpace(a.AccessToken), "sk-kimi-")))
		out = append(out, map[string]any{
			"id":                  a.ID,
			"provider":            a.NormalizedProvider(),
			"label":               a.Label,
			"email":               a.Email,
			"user_id":             a.UserID,
			"team_id":             a.TeamID,
			"source":              a.Source,
			"api_key_hint":        keyHint,
			"has_web_session":     hasWeb,
			"has_refresh":         strings.TrimSpace(a.RefreshToken) != "",
			"has_google_refresh":  strings.TrimSpace(a.GoogleRefreshToken) != "",
			"google_email":        a.GoogleEmail,
			"has_google_password": a.GooglePassword != "",
			"expires_at":          a.ExpiresAt,
			"expired":             a.Expired(),
			"exhausted":           a.Exhausted(),
			"exhausted_at":        a.ExhaustedAt,
			"exhaust_reason":      a.ExhaustReason,
			"auth_denied":         a.AuthDenied(),
			"auth_denied_at":      a.AuthDeniedAt,
			"auth_denied_reason":  a.AuthDeniedReason,
			"active":              a.ID == s.settings.ActiveAccountID,
			"usage": map[string]any{
				"prompt_tokens":     u.PromptTokens,
				"completion_tokens": u.CompletionTokens,
				"reasoning_tokens":  u.ReasoningTokens,
				"total_tokens":      u.TotalTokens,
				"requests":          u.Requests,
				"cost_usd":          u.CostUSD,
			},
		})
	}
	// stable sort: active first, then label
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			ai, _ := out[i]["active"].(bool)
			aj, _ := out[j]["active"].(bool)
			if aj && !ai {
				out[i], out[j] = out[j], out[i]
				continue
			}
			if ai == aj {
				li, _ := out[i]["label"].(string)
				lj, _ := out[j]["label"].(string)
				if lj < li {
					out[i], out[j] = out[j], out[i]
				}
			}
		}
	}
	return out
}

func (s *Store) GetAccount(id string) (*Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[id]
	if !ok {
		return nil, false
	}
	cp := a
	return &cp, true
}

func (s *Store) ActiveAccount() (*Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	want := s.settings.NormalizedProvider()
	id := s.settings.ActiveAccountID
	if id != "" {
		if a, ok := s.accounts[id]; ok && a.NormalizedProvider() == want {
			cp := a
			return &cp, true
		}
	}
	for _, a := range s.accounts {
		if a.NormalizedProvider() != want {
			continue
		}
		cp := a
		return &cp, true
	}
	return nil, false
}

// PreferUsableAccount returns the active account if still usable; otherwise the first
// non-exhausted / non-auth-denied account for the active provider.
func (s *Store) PreferUsableAccount() (*Account, bool) {
	return s.PreferUsableAccountForProvider("")
}

// PreferUsableAccountForProvider is like PreferUsableAccount but for an explicit provider
// (empty = active settings provider). Used by HTTP multi-route (model → provider).
func (s *Store) PreferUsableAccountForProvider(provider string) (*Account, bool) {
	want := s.normalizeProviderFilter(provider)
	tryPick := func() (*Account, bool) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		try := func(id string) (*Account, bool) {
			if id == "" {
				return nil, false
			}
			a, ok := s.accounts[id]
			if !ok || !a.Usable() || a.NormalizedProvider() != want {
				return nil, false
			}
			cp := a
			return &cp, true
		}
		// Only prefer ActiveAccountID when it already belongs to the requested provider.
		// Global active often stays on an xAI row while provider=kimi_work / model routes to Kimi.
		if acc, ok := try(s.settings.ActiveAccountID); ok {
			return acc, true
		}
		var fallback *Account
		for _, a := range s.accounts {
			if a.NormalizedProvider() != want || !a.Usable() {
				continue
			}
			cp := a
			// Kimi Work: sk-kimi does not expire with web JWT — ignore ExpiresAt.
			if want == ProviderKimiWork || !cp.Expired() {
				return &cp, true
			}
			if fallback == nil {
				fallback = &cp
			}
		}
		if fallback != nil {
			return fallback, true
		}
		return nil, false
	}
	if acc, ok := tryPick(); ok {
		return acc, true
	}
	// Another process may have added accounts (shared SQLite); reload once.
	if s.db != nil {
		if err := s.ReloadAccountsFromDB(); err == nil {
			return tryPick()
		}
	}
	return nil, false
}

func (s *Store) normalizeProviderFilter(provider string) string {
	want := strings.ToLower(strings.TrimSpace(provider))
	if want == "" {
		s.mu.RLock()
		want = s.settings.NormalizedProvider()
		s.mu.RUnlock()
	}
	switch want {
	case "kimi", "kimi-work", "kimiwork", "moonshot-work":
		return ProviderKimiWork
	case "grok", "x.ai":
		return ProviderXAI
	case "codex", "openai-codex", "chatgpt":
		return ProviderCodex
	default:
		return want
	}
}

// ReloadAccountsFromDB re-reads accounts from SQLite into memory (multi-instance / external writes).
func (s *Store) ReloadAccountsFromDB() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return fmt.Errorf("db not open")
	}
	rows, err := s.db.Query(`SELECT id, provider, label, email, team_id, user_id,
		access_token, refresh_token, expires_at, api_key, device_id, source,
		exhausted_at, exhaust_reason, auth_denied_at, auth_denied_reason,
		client_id, issuer, scope, google_refresh_token, google_email, google_password, created_at, updated_at FROM accounts`)
	if err != nil {
		return err
	}
	defer rows.Close()
	next := make(map[string]Account, len(s.accounts))
	for rows.Next() {
		var a Account
		var exp, exh, auth, created, updated string
		if err := rows.Scan(
			&a.ID, &a.Provider, &a.Label, &a.Email, &a.TeamID, &a.UserID,
			&a.AccessToken, &a.RefreshToken, &exp, &a.APIKey, &a.DeviceID, &a.Source,
			&exh, &a.ExhaustReason, &auth, &a.AuthDeniedReason,
			&a.ClientID, &a.Issuer, &a.Scope, &a.GoogleRefreshToken, &a.GoogleEmail, &a.GooglePassword, &created, &updated,
		); err != nil {
			continue
		}
		a.DeviceID = cleanDeviceID(a.DeviceID)
		a.ExpiresAt = timeFromSQL(exp)
		a.ExhaustedAt = timeFromSQL(exh)
		a.AuthDeniedAt = timeFromSQL(auth)
		a.CreatedAt = timeFromSQL(created)
		a.UpdatedAt = timeFromSQL(updated)
		if strings.TrimSpace(a.Provider) == "" {
			a.Provider = ProviderXAI
		}
		a.Provider = a.NormalizedProvider()
		if decryptAccountCredentials(&a) != nil {
			continue
		}
		if a.AccessToken == "" && a.APIKey == "" {
			continue
		}
		if a.NormalizedProvider() == ProviderKimiWork && a.APIKey == "" && strings.HasPrefix(a.AccessToken, "sk-kimi-") {
			a.APIKey = a.AccessToken
		}
		// Preserve credentials from an in-flight update and the request counter.
		if old, ok := s.accounts[a.ID]; ok {
			if a.GoogleRefreshToken == "" {
				a.GoogleRefreshToken = old.GoogleRefreshToken
			}
			if a.GoogleEmail == "" {
				a.GoogleEmail = old.GoogleEmail
			}
			if a.GooglePassword == "" {
				a.GooglePassword = old.GooglePassword
			}
			a.requestCount = old.requestCount
		}
		next[a.ID] = a
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.accounts = next
	return nil
}

// NextUsableAccountID picks another usable account excluding exceptID (same provider as active settings).
func (s *Store) NextUsableAccountID(exceptID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	want := s.settings.NormalizedProvider()
	if exceptID != "" {
		if a, ok := s.accounts[exceptID]; ok {
			want = a.NormalizedProvider()
		}
	}
	var fallback string
	for _, a := range s.accounts {
		if a.ID == exceptID || !a.Usable() || a.NormalizedProvider() != want {
			continue
		}
		if !a.Expired() {
			return a.ID
		}
		if fallback == "" {
			fallback = a.ID
		}
	}
	return fallback
}

// PreferHealthyActive switches active to a usable account of the current provider when
// current active is missing, wrong provider, exhausted, or auth-denied.
// Kimi Work: sk-kimi does not follow web JWT ExpiresAt — expired JWT is fine if key exists.
func (s *Store) PreferHealthyActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := s.settings.NormalizedProvider()
	curID := s.settings.ActiveAccountID
	if curID != "" {
		if a, ok := s.accounts[curID]; ok && a.NormalizedProvider() == want && a.Usable() {
			// xAI: prefer non-expired when possible; still keep if only expired+refresh remains.
			if want == ProviderKimiWork || !a.Expired() || a.RefreshToken != "" {
				return false
			}
		}
	}
	var bestID string
	var bestScore int
	for _, a := range s.accounts {
		if a.NormalizedProvider() != want || !a.Usable() {
			continue
		}
		score := 1
		if want == ProviderKimiWork {
			if strings.TrimSpace(a.APIKey) != "" {
				score += 2
			}
			if strings.TrimSpace(a.RefreshToken) != "" {
				score++
			}
		} else if !a.Expired() {
			score += 2
		} else if a.RefreshToken != "" {
			score++
		}
		if bestID == "" || score > bestScore {
			bestID = a.ID
			bestScore = score
		}
	}
	if bestID == "" || bestID == curID {
		return false
	}
	s.settings.ActiveAccountID = bestID
	_ = s.saveSettingsLocked()
	return true
}

// MarkExhausted stamps quota exhaustion on an account.
func (s *Store) MarkExhausted(id, reason string) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return nil, fmt.Errorf("account not found: %s", id)
	}
	now := time.Now().UTC()
	a.ExhaustedAt = now
	a.ExhaustReason = reason
	a.UpdatedAt = now
	// Keep label informative without relying only on string matching.
	low := strings.ToLower(a.Label)
	if !strings.Contains(low, "esgotada") {
		if a.Email != "" {
			a.Label = "esgotada · " + a.Email
		} else if len(a.ID) >= 8 {
			a.Label = "esgotada · " + a.ID[:8]
		} else {
			a.Label = "esgotada"
		}
	}
	s.accounts[id] = a
	if err := s.saveAccountLocked(a); err != nil {
		return nil, err
	}
	logging.Warn("store.account.exhausted", "account_id", id, "provider", a.NormalizedProvider(), "reason", reason)
	cp := a
	return &cp, nil
}

// ClearExhausted removes quota exhaustion (e.g. after re-auth / new credits).
func (s *Store) ClearExhausted(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return fmt.Errorf("account not found: %s", id)
	}
	if a.ExhaustedAt.IsZero() && a.ExhaustReason == "" {
		return nil
	}
	a.ExhaustedAt = time.Time{}
	a.ExhaustReason = ""
	a.UpdatedAt = time.Now().UTC()
	// Strip esgotada prefix from label if we set it.
	if strings.HasPrefix(strings.ToLower(a.Label), "esgotada") {
		if a.Email != "" {
			a.Label = a.Email
		} else if len(a.ID) >= 8 {
			a.Label = "Conta " + a.ID[:8]
		}
	}
	s.accounts[id] = a
	return s.saveAccountLocked(a)
}

// MarkAuthDenied stamps chat/auth rejection so the account is skipped by PreferUsable.
func (s *Store) MarkAuthDenied(id, reason string) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return nil, fmt.Errorf("account not found: %s", id)
	}
	now := time.Now().UTC()
	a.AuthDeniedAt = now
	a.AuthDeniedReason = reason
	a.UpdatedAt = now
	low := strings.ToLower(a.Label)
	if !strings.Contains(low, "auth-denied") && !strings.Contains(low, "bloqueada") {
		if a.Email != "" {
			a.Label = "auth-denied · " + a.Email
		} else if len(a.ID) >= 8 {
			a.Label = "auth-denied · " + a.ID[:8]
		} else {
			a.Label = "auth-denied"
		}
	}
	s.accounts[id] = a
	logging.Warn("store.account.auth_denied", "account_id", id, "provider", a.NormalizedProvider(), "reason", reason)
	if err := s.saveAccountLocked(a); err != nil {
		return nil, err
	}
	cp := a
	return &cp, nil
}

// ClearAuthDenied clears a previous chat/auth rejection (e.g. after fresh tokens).
func (s *Store) ClearAuthDenied(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return fmt.Errorf("account not found: %s", id)
	}
	if a.AuthDeniedAt.IsZero() && a.AuthDeniedReason == "" {
		return nil
	}
	a.AuthDeniedAt = time.Time{}
	a.AuthDeniedReason = ""
	a.UpdatedAt = time.Now().UTC()
	if strings.HasPrefix(strings.ToLower(a.Label), "auth-denied") {
		if a.Email != "" {
			a.Label = a.Email
		} else if len(a.ID) >= 8 {
			a.Label = "Conta " + a.ID[:8]
		}
	}
	s.accounts[id] = a
	return s.saveAccountLocked(a)
}

// ClearAuthState clears both quota and auth-denied marks after a successful re-auth.
func (s *Store) ClearAuthState(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return fmt.Errorf("account not found: %s", id)
	}
	a.ExhaustedAt = time.Time{}
	a.ExhaustReason = ""
	a.AuthDeniedAt = time.Time{}
	a.AuthDeniedReason = ""
	a.UpdatedAt = time.Now().UTC()
	low := strings.ToLower(a.Label)
	if strings.HasPrefix(low, "esgotada") || strings.HasPrefix(low, "auth-denied") {
		if a.Email != "" {
			a.Label = a.Email
		} else if len(a.ID) >= 8 {
			a.Label = "Conta " + a.ID[:8]
		}
	}
	s.accounts[id] = a
	return s.saveAccountLocked(a)
}

// grokCLIAuthPath is ~/.grok/auth.json (official Grok CLI OIDC store).
func grokCLIAuthPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".grok", "auth.json"), nil
}

// SyncFromGrokCLI imports fresher OAuth tokens from the official Grok CLI auth.json.
// Returns how many accounts were updated or inserted.
//
// Important: proxy and official Grok CLI share the same OIDC client_id. xAI refresh
// tokens rotate/revoke — adopting a STALE CLI refresh_token after the proxy already
// rotated (or vice-versa) causes intermittent invalid_grant. We only adopt CLI
// tokens when the CLI access token is strictly fresher by expires_at.
func (s *Store) SyncFromGrokCLI() (int, error) {
	path, err := grokCLIAuthPath()
	if err != nil {
		return 0, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(b, &root); err != nil {
		return 0, err
	}
	updated := 0
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, raw := range root {
		var entry map[string]any
		if json.Unmarshal(raw, &entry) != nil {
			continue
		}
		access := str(entry["key"])
		if access == "" {
			access = str(entry["access_token"])
		}
		if access == "" {
			continue
		}
		refresh := str(entry["refresh_token"])
		email := str(entry["email"])
		userID := str(entry["user_id"])
		if userID == "" {
			userID = str(entry["principal_id"])
		}
		teamID := str(entry["team_id"])
		clientID := str(entry["oidc_client_id"])
		if clientID == "" {
			clientID = DefaultClientID
		}
		issuer := str(entry["oidc_issuer"])
		if issuer == "" {
			issuer = DefaultIssuer
		}
		var exp time.Time
		if es := str(entry["expires_at"]); es != "" {
			if t, e := time.Parse(time.RFC3339Nano, es); e == nil {
				exp = t.UTC()
			} else if t, e := time.Parse(time.RFC3339, es); e == nil {
				exp = t.UTC()
			}
		}
		id := userID
		if id == "" {
			if email == "" {
				continue
			}
			id = email
		}
		if old, ok := s.accounts[id]; ok {
			sameToken := old.AccessToken == access
			// CLI is fresher only when its access expiry is clearly ahead of ours.
			// 30s slack absorbs clock/rounding differences without thrashing.
			cliFresher := !exp.IsZero() && (old.ExpiresAt.IsZero() || exp.After(old.ExpiresAt.Add(30*time.Second)))
			proxyHasCreds := old.AccessToken != "" && old.RefreshToken != ""
			// Never clobber a working (or newer) proxy session with a stale CLI
			// snapshot. Previously AuthDenied/Exhausted forced an overwrite even
			// when CLI expires_at was older — that re-injected revoked RTs.
			if sameToken && !old.Expired() {
				// Access matches; still heal auth-denied/exhausted flags if CLI is healthy.
				if (old.AuthDenied() || old.Exhausted()) && !cliTokenExpired(exp) {
					a := old
					a.ExhaustedAt = time.Time{}
					a.ExhaustReason = ""
					a.AuthDeniedAt = time.Time{}
					a.AuthDeniedReason = ""
					a.UpdatedAt = time.Now().UTC()
					s.accounts[id] = a
					_ = s.saveAccountLocked(a)
					updated++
				}
				continue
			}
			if !cliFresher {
				// Proxy is equal/newer, or CLI has no expires_at. Keep proxy tokens.
				if proxyHasCreds || old.AccessToken != "" {
					continue
				}
			}
			a := old
			a.AccessToken = access
			if refresh != "" {
				a.RefreshToken = refresh
			}
			if !exp.IsZero() {
				a.ExpiresAt = exp
			}
			if email != "" {
				a.Email = email
			}
			if teamID != "" {
				a.TeamID = teamID
			}
			if userID != "" {
				a.UserID = userID
			}
			a.ClientID = clientID
			a.Issuer = issuer
			a.ExhaustedAt = time.Time{}
			a.ExhaustReason = ""
			a.AuthDeniedAt = time.Time{}
			a.AuthDeniedReason = ""
			a.UpdatedAt = time.Now().UTC()
			low := strings.ToLower(a.Label)
			if strings.HasPrefix(low, "esgotada") || strings.HasPrefix(low, "auth-denied") {
				if a.Email != "" {
					a.Label = a.Email
				}
			}
			if a.Label == "" || a.Label == "Grok account" {
				if a.Email != "" {
					a.Label = a.Email
				}
			}
			s.accounts[id] = a
			_ = s.saveAccountLocked(a)
			updated++
			logging.Info("store.cli_sync.adopted", "account_id", id, "cli_exp", exp)
			continue
		}
		now := time.Now().UTC()
		a := Account{
			ID:           id,
			Label:        email,
			Email:        email,
			TeamID:       teamID,
			UserID:       userID,
			AccessToken:  access,
			RefreshToken: refresh,
			ExpiresAt:    exp,
			ClientID:     clientID,
			Issuer:       issuer,
			CreatedAt:    now,
			UpdatedAt:    now,
			Provider:     ProviderXAI,
		}
		if a.Label == "" {
			a.Label = "Grok CLI"
		}
		s.accounts[id] = a
		_ = s.saveAccountLocked(a)
		if s.settings.ActiveAccountID == "" {
			s.settings.ActiveAccountID = id
			_ = s.saveSettingsLocked()
		}
		updated++
		logging.Info("store.cli_sync.inserted", "account_id", id)
	}
	return updated, nil
}

func cliTokenExpired(exp time.Time) bool {
	if exp.IsZero() {
		return false
	}
	return time.Now().UTC().After(exp)
}

// WriteAccountToGrokCLI mirrors a rotated xAI session into ~/.grok/auth.json so the
// official Grok CLI keeps the same refresh_token. Without this, the next CLI refresh
// (or a SyncFromGrokCLI of a stale file) races the proxy and yields invalid_grant.
// Best-effort: errors are returned but never fatal for request serving.
func (s *Store) WriteAccountToGrokCLI(acc Account) error {
	if acc.AccessToken == "" || acc.RefreshToken == "" {
		return nil
	}
	if acc.NormalizedProvider() != ProviderXAI {
		return nil
	}
	path, err := grokCLIAuthPath()
	if err != nil {
		return err
	}
	// Serialize writers (CLI also uses auth.json.lock; we use a sibling lock).
	lockPath := path + ".proxy-write.lock"
	lockF, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lockF.Close()
	// Best-effort exclusive section via O_EXCL stamp file is racy on Windows; we
	// still rewrite atomically via temp+rename below.

	var root map[string]any
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		if json.Unmarshal(b, &root) != nil {
			root = map[string]any{}
		}
	} else {
		root = map[string]any{}
	}

	clientID := acc.ClientID
	if clientID == "" {
		clientID = DefaultClientID
	}
	issuer := acc.Issuer
	if issuer == "" {
		issuer = DefaultIssuer
	}
	entryKey := issuer + "::" + clientID

	// Prefer merging into the existing entry for this issuer/client.
	entry, _ := root[entryKey].(map[string]any)
	if entry == nil {
		// Also match by user_id if the CLI used a different key shape.
		for k, v := range root {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			uid := str(m["user_id"])
			if uid == "" {
				uid = str(m["principal_id"])
			}
			if uid != "" && (uid == acc.ID || uid == acc.UserID) {
				entry = m
				entryKey = k
				break
			}
		}
	}
	if entry == nil {
		entry = map[string]any{}
	}

	// Do not overwrite CLI if its access token is strictly newer than ours.
	if es := str(entry["expires_at"]); es != "" {
		var cliExp time.Time
		if t, e := time.Parse(time.RFC3339Nano, es); e == nil {
			cliExp = t.UTC()
		} else if t, e := time.Parse(time.RFC3339, es); e == nil {
			cliExp = t.UTC()
		}
		if !cliExp.IsZero() && !acc.ExpiresAt.IsZero() && cliExp.After(acc.ExpiresAt.Add(30*time.Second)) {
			return nil
		}
	}

	entry["key"] = acc.AccessToken
	entry["refresh_token"] = acc.RefreshToken
	entry["auth_mode"] = "oidc"
	if !acc.ExpiresAt.IsZero() {
		entry["expires_at"] = acc.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	entry["oidc_issuer"] = issuer
	entry["oidc_client_id"] = clientID
	if acc.UserID != "" {
		entry["user_id"] = acc.UserID
		entry["principal_id"] = acc.UserID
	} else if acc.ID != "" {
		entry["user_id"] = acc.ID
		entry["principal_id"] = acc.ID
	}
	if acc.Email != "" {
		entry["email"] = acc.Email
	}
	if acc.TeamID != "" {
		entry["team_id"] = acc.TeamID
	}
	entry["principal_type"] = "User"
	root[entryKey] = entry

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		// Windows: replace existing
		_ = os.Remove(path)
		if err2 := os.Rename(tmp, path); err2 != nil {
			return err2
		}
	}
	logging.Info("store.cli_write.ok", "account_id", acc.ID, "path", path)
	return nil
}

func (s *Store) UpsertAccount(a Account) error {
	logging.Debug("store.account.upsert", "account_id", a.ID, "provider", a.NormalizedProvider())
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if a.ID == "" {
		return errors.New("account id required")
	}
	if strings.TrimSpace(a.Provider) == "" {
		a.Provider = ProviderXAI
	}
	a.Provider = a.NormalizedProvider()
	if a.CreatedAt.IsZero() {
		if old, ok := s.accounts[a.ID]; ok {
			a.CreatedAt = old.CreatedAt
		} else {
			a.CreatedAt = now
		}
	}
	a.UpdatedAt = now
	s.accounts[a.ID] = a
	// Normalize duplicates: the same Google/Kimi identity must not accumulate
	// multiple rows (re-login after remote logoff mints a NEW Kimi user id for
	// the SAME Google account, so kimi-<userID> rows multiply otherwise).
	// The quota-reset flow (logoff → re-login) is unaffected: it replaces the
	// predecessor row with the fresh one, which is exactly what dedup does.
	if a.NormalizedProvider() == ProviderKimiWork {
		s.dedupKimiAccountsLocked(a)
	}
	// Activate if empty or active belongs to another provider.
	activate := s.settings.ActiveAccountID == ""
	if !activate {
		if cur, ok := s.accounts[s.settings.ActiveAccountID]; !ok || cur.NormalizedProvider() != a.NormalizedProvider() {
			if s.settings.NormalizedProvider() == a.NormalizedProvider() {
				activate = true
			}
		}
	}
	if activate && s.settings.NormalizedProvider() == a.NormalizedProvider() {
		s.settings.ActiveAccountID = a.ID
		_ = s.saveSettingsLocked()
	}
	return s.saveAccountLocked(a)
}

// normalizeKimiIdentityEmail lowercases/trims an email and, for Gmail only,
// strips dots and +tags from the local part (Google treats them as identical).
func normalizeKimiIdentityEmail(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	local, domain, ok := strings.Cut(email, "@")
	if !ok {
		return email
	}
	if domain == "gmail.com" || domain == "googlemail.com" {
		if i := strings.Index(local, "+"); i >= 0 {
			local = local[:i]
		}
		local = strings.ReplaceAll(local, ".", "")
	}
	return local + "@" + domain
}

// sameKimiIdentity reports whether two Kimi Work accounts belong to the same
// Google/Kimi identity: matching Google refresh token (strongest) or matching
// normalized email on either the Email or GoogleEmail fields.
func sameKimiIdentity(a, b Account) bool {
	if a.GoogleRefreshToken != "" && a.GoogleRefreshToken == b.GoogleRefreshToken {
		return true
	}
	pairs := [][2]string{
		{a.Email, b.Email},
		{a.GoogleEmail, b.GoogleEmail},
		{a.Email, b.GoogleEmail},
		{a.GoogleEmail, b.Email},
	}
	for _, p := range pairs {
		x, y := normalizeKimiIdentityEmail(p[0]), normalizeKimiIdentityEmail(p[1])
		if x != "" && !strings.HasPrefix(x, "@") && x == y {
			return true
		}
	}
	return false
}

// dedupKimiAccountsLocked removes OTHER Kimi Work rows that share the freshly
// upserted account's identity. The new row always wins: it carries the live
// binding (Google maps the account to the newest Kimi user) and fresh quota.
// Caller must hold s.mu.
func (s *Store) dedupKimiAccountsLocked(keep Account) {
	for id, other := range s.accounts {
		if id == keep.ID || other.NormalizedProvider() != ProviderKimiWork {
			continue
		}
		if !sameKimiIdentity(keep, other) {
			continue
		}
		logging.Info("store.account.dedup", "removed", id, "kept", keep.ID, "email", other.Email)
		if s.db != nil {
			_ = s.deleteAccountDB(id)
		}
		_ = os.Remove(s.accountPath(id))
		delete(s.accounts, id)
		delete(s.usage, id)
		filtered := s.history[:0]
		for _, h := range s.history {
			if h.AccountID != id {
				filtered = append(filtered, h)
			}
		}
		s.history = filtered
		if s.settings.ActiveAccountID == id {
			s.settings.ActiveAccountID = keep.ID
			_ = s.saveSettingsLocked()
		}
	}
	_ = s.saveUsageLocked()
	_ = s.saveHistoryLocked()
}

func (s *Store) RemoveAccount(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, had := s.accounts[id]
	if s.db != nil {
		if err := s.deleteAccountDB(id); err != nil {
			return fmt.Errorf("delete account from database: %w", err)
		}
	}
	if err := os.Remove(s.accountPath(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete account backup: %w", err)
	}
	delete(s.accounts, id)
	logging.Info("store.account.remove", "account_id", id)
	if s.settings.ActiveAccountID == id {
		s.settings.ActiveAccountID = ""
		want := s.settings.NormalizedProvider()
		if had {
			want = old.NormalizedProvider()
		}
		for k, a := range s.accounts {
			if a.NormalizedProvider() == want {
				s.settings.ActiveAccountID = k
				break
			}
		}
		if err := s.saveSettingsLocked(); err != nil {
			return err
		}
	}
	delete(s.usage, id)
	_ = s.saveUsageLocked()
	filtered := s.history[:0]
	for _, h := range s.history {
		if h.AccountID != id {
			filtered = append(filtered, h)
		}
	}
	s.history = filtered
	_ = s.saveHistoryLocked()
	return nil
}

func (s *Store) SetActiveAccount(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[id]; !ok {
		return fmt.Errorf("account not found: %s", id)
	}
	s.settings.ActiveAccountID = id
	return s.saveSettingsLocked()
}

func (s *Store) RecordRequest(sample RequestSample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usage == nil {
		s.usage = map[string]UsageTotals{}
	}
	add := func(key string) {
		u := s.usage[key]
		u.PromptTokens += sample.PromptTokens
		u.CompletionTokens += sample.CompletionTokens
		u.ReasoningTokens += sample.ReasoningTokens
		u.CachedTokens += sample.CachedTokens
		u.TotalTokens += sample.TotalTokens
		if u.TotalTokens == 0 {
			u.TotalTokens = sample.PromptTokens + sample.CompletionTokens
		}
		u.Requests++
		u.CostUSD += sample.CostUSD
		if sample.LatencyMs > 0 {
			u.LatencySumMs += sample.LatencyMs
			u.TTFTSumMs += sample.TTFTMs
			u.LatencySamples++
		}
		s.usage[key] = u
	}
	if sample.AccountID != "" {
		add(sample.AccountID)
	}
	add("_global")

	// newest first, cap 200
	s.history = append([]RequestSample{sample}, s.history...)
	const maxHist = 200
	if len(s.history) > maxHist {
		s.history = s.history[:maxHist]
	}
	if s.db != nil {
		_ = s.insertHistoryDB(sample)
	}
	if err := s.saveUsageLocked(); err != nil {
		return err
	}
	return s.saveHistoryLocked()
}

// Deprecated wrapper
func (s *Store) AddUsage(accountID string, prompt, completion, reasoning int64) error {
	return s.RecordRequest(RequestSample{
		AccountID:        accountID,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		ReasoningTokens:  reasoning,
		TotalTokens:      prompt + completion,
		At:               time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Store) UsageSnapshot() map[string]UsageTotals {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]UsageTotals{}
	for k, v := range s.usage {
		out[k] = v
	}
	return out
}

func (s *Store) History(limit int) []RequestSample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > len(s.history) {
		limit = len(s.history)
	}
	out := make([]RequestSample, limit)
	copy(out, s.history[:limit])
	return out
}
