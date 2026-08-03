package main

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"grok-desktop/internal/accio"
	"grok-desktop/internal/accmgr"
	"grok-desktop/internal/kimi"
	"grok-desktop/internal/logging"
	"grok-desktop/internal/mcpconfig"
	"grok-desktop/internal/oauth"
	"grok-desktop/internal/pricing"
	"grok-desktop/internal/proxyhttp"
	"grok-desktop/internal/secure"
	"grok-desktop/internal/signup"
	"grok-desktop/internal/skills"
	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
)

type App struct {
	ctx context.Context

	store    *store.Store
	oauth    *oauth.Client
	upstream *upstream.Client
	proxy    *proxyhttp.Server
	accio    *accio.Client
	accMgr   *accmgr.Manager
	skills   *skills.Store
	mcp      *mcpconfig.Store

	mu            sync.Mutex
	deviceCancel  context.CancelFunc
	deviceState   *deviceLoginState
	reqCancel     context.CancelFunc
	activeReq     *ActiveRequest
	signupRunning bool
	signupCancel  context.CancelFunc
	autoCreate    bool
	// kimiReloginCount limits concurrent Kimi auto re-logins (max 2).
	kimiReloginCount int
	// kimiReloginInFlight dedupes re-logins per account (queued or running).
	kimiReloginInFlight map[string]bool
	// kimiReloginQueue holds accountIDs waiting for a free re-login slot (FIFO).
	kimiReloginQueue []kimiReloginRequest
	// kimiReloginWaiters holds join-wait callbacks fired when an account's
	// re-login completes (or any account becomes usable).
	kimiReloginWaiters map[string][]func(bool)
	// kimiCapMu serializes the Kimi account cap check + insert (TOCTOU).
	kimiCapMu sync.Mutex

	// Per-account refresh serialization (refresh-token rotation is not concurrent-safe).
	refreshGates sync.Map // accountID → *sync.Mutex

	// Throttle CLI auth sync so Codex multi-request storms don't hammer disk every turn.
	lastCLISync   time.Time
	lastCLISyncMu sync.Mutex
}

type deviceLoginState struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
	StartedAt       string `json:"started_at"`
}

type ActiveRequest struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	Label     string `json:"label"`
	Model     string `json:"model"`
	StartedAt string `json:"started_at"`
	Phase     string `json:"phase"` // thinking | content | idle
}

// maxKimiWorkAccounts is the maximum number of Kimi Work accounts a user may register.
// Proxy works with 1, 2, or 3 — there is no minimum. Load balancing and re-login
// keep whatever is registered operational.
const maxKimiWorkAccounts = 3

// maxConcurrentKimiRelogins caps parallel Kimi re-login flows (HTTP is cheap but
// the Playwright fallback spawns a browser each).
const maxConcurrentKimiRelogins = 2

// kimiReloginRequest is a queued re-login waiting for a free slot.
type kimiReloginRequest struct {
	accountID string
	reason    string
}

func NewApp() *App {
	return &App{}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	// Stream every structured log line to the UI in real time (Logs modal).
	logging.SetSink(func(level logging.Level, msg string, fields map[string]any) {
		a.safeEmit("log", map[string]any{
			"ts":     time.Now().UnixMilli(),
			"level":  level.String(),
			"msg":    msg,
			"fields": fields,
		})
	})
	st, err := store.Open("")
	if err != nil {
		runtime.LogErrorf(ctx, "store open: %v", err)
		return
	}
	a.store = st
	a.oauth = oauth.New()
	// Align OAuth client surface version with the installed Grok CLI default.
	if v := st.Settings().ClientVersion; v != "" {
		a.oauth.CLIVersion = v
	}
	a.upstream = upstream.New()
	if ac, err := accio.New(filepath.Join(st.Root(), "accio")); err == nil {
		a.accio = ac
		for _, t := range ac.Accounts() {
			a.syncAccioAccount(t)
		}
		// Wire login callback → Wails event so the UI can poll/refresh.
		ac.OnLogin(func(t accio.TokenRecord) {
			a.syncAccioAccount(t)
			a.safeEmit("accio:login", map[string]any{
				"id":    t.ID,
				"email": t.Email,
				"label": t.Label,
			})
		})
		// Account manager: keeps the pool topped up so requests never die on an
		// exhausted quota. Started below (after the store is ready).
		a.accMgr = accmgr.New(ac, accmgr.Config{
			MinCredits:     envInt("ACCIO_MIN_CREDITS", 300),
			MaxAccounts:    envInt("ACCIO_MAX_ACCOUNTS", 10),
			CreateCooldown: envDur("ACCIO_CREATE_COOLDOWN", 3*time.Minute),
			CheckEvery:     envDur("ACCIO_CHECK_EVERY", 2*time.Minute),
			WARP:           os.Getenv("ACCIO_USE_WARP") != "0",
			Headless:       os.Getenv("ACCIO_SIGNUP_VISIBLE") != "1",
		})
		// Import the existing standalone proxy credential once, without logging it.
		standalone := filepath.Join(filepath.Dir(st.Root()), "accio-proxy", "token.json")
		if _, err := os.Stat(filepath.Join(st.Root(), "accio", "token.json")); os.IsNotExist(err) {
			if err := ac.ImportTokenFile(standalone); err == nil {
				// ImportTokenFile writes the private Accio credential but does not
				// fire the login callback. Sync it explicitly or the proxy pool
				// will not see the imported account until the next app restart.
				if t, readErr := ac.CurrentAccount(); readErr == nil {
					a.syncAccioAccount(t)
				}
			}
		}
	}
	a.proxy = proxyhttp.New(st, a.upstream, a.ensureCreds)
	a.proxy.SetAccio(a.accio)
	a.proxy.SetForceRefresh(a.forceRefreshAccount)
	// Start the Accio account top-up loop (no-op unless accounts exist and
	// the aggregate balance drops below the configured floor).
	if a.accMgr != nil {
		a.accMgr.Start()
	}
	a.proxy.SetQuotaHandler(func(accountID, reason string) bool {
		// Blocks when needed until re-login refills a usable Kimi account; never requires min-3.
		a.markAccountExhausted(accountID, reason)
		if a.kimiUsableAnyReady() {
			return true
		}
		after, ok := a.store.ActiveAccount()
		return ok && after != nil && after.Usable()
	})
	a.proxy.SetAuthFailHandler(func(accountID, reason string) bool {
		a.markAccountAuthDenied(accountID, reason)
		after, ok := a.store.ActiveAccount()
		return ok && after != nil && after.ID != accountID && after.Usable()
	})
	if sk, err := skills.Open(filepath.Join(st.Root(), "skills")); err == nil {
		a.skills = sk
	} else {
		runtime.LogErrorf(ctx, "skills: %v", err)
	}
	if mc, err := mcpconfig.Open(filepath.Join(st.Root(), "mcp_servers.json")); err == nil {
		a.mcp = mc
	} else {
		runtime.LogErrorf(ctx, "mcpconfig: %v", err)
	}

	settings := st.Settings()
	if settings.ProxyEnabled {
		listen := settings.ProxyListen
		if listen == "" {
			listen = "127.0.0.1:8787"
		}
		if err := a.proxy.Start(listen); err != nil {
			// Port busy (e.g. old Python proxy) — try fallback
			fallback := "127.0.0.1:8788"
			runtime.LogErrorf(ctx, "proxy start on %s failed: %v; trying %s", listen, err, fallback)
			if err2 := a.proxy.Start(fallback); err2 != nil {
				runtime.LogErrorf(ctx, "proxy fallback failed: %v", err2)
			} else {
				_ = a.store.UpdateSettings(func(s *store.Settings) { s.ProxyListen = fallback })
				runtime.LogInfof(ctx, "OpenAI proxy listening on http://%s", a.proxy.Addr())
			}
		} else {
			runtime.LogInfof(ctx, "OpenAI proxy listening on http://%s", a.proxy.Addr())
		}
	}
}

func (a *App) shutdown(ctx context.Context) {
	accio.ShutdownSGDaemon()
	if a.accMgr != nil {
		a.accMgr.Stop()
	}
	if a.proxy != nil {
		_ = a.proxy.Stop(context.Background())
	}
	a.mu.Lock()
	if a.deviceCancel != nil {
		a.deviceCancel()
	}
	if a.reqCancel != nil {
		a.reqCancel()
	}
	a.mu.Unlock()
}

// ---------- Public API for frontend ----------

func (a *App) GetBootstrap() map[string]any {
	if a.store == nil {
		return map[string]any{"error": "store not ready"}
	}
	s := a.store.Settings()
	active := map[string]any{}
	if s.IsQwen() {
		active = map[string]any{
			"id": "qwen", "email": s.EffectiveQwenUpstream(), "label": "QwenBridge", "provider": store.ProviderQwen, "auth_mode": store.AuthModeAPIKey,
		}
	} else if s.IsDeepSeek() {
		active = map[string]any{
			"id": "deepseek", "email": store.DeepSeekUpstream, "label": "DeepSeek", "provider": store.ProviderDeepSeek, "auth_mode": store.AuthModeAPIKey,
		}
	} else if s.IsOllie() {
		active = map[string]any{
			"id": "ollie", "email": "keyless@olliechat", "label": "OllieChat", "provider": store.ProviderOllie, "auth_mode": store.AuthModeAPIKey,
		}
	} else if s.IsGemini() {
		active = map[string]any{
			"id": "gemini-adc", "email": s.EffectiveGeminiProject(), "label": "Gemini ADC", "provider": store.ProviderGemini, "auth_mode": store.AuthModeAPIKey,
		}
	} else if s.IsOpenCodeZen() {
		active = map[string]any{
			"id": "opencode-zen-free", "email": "keyless@opencode.ai", "label": "OpenCode Zen Free", "provider": store.ProviderOpenCodeZen, "auth_mode": store.AuthModeAPIKey,
		}
	} else if acc, ok := a.store.ActiveAccount(); ok && acc != nil {
		active = map[string]any{
			"id": acc.ID, "email": acc.Email, "label": acc.Label, "expires_at": acc.ExpiresAt,
			"provider": acc.NormalizedProvider(), "auth_mode": s.ProviderAuthMode(),
		}
	}
	proxyAddr := ""
	if a.proxy != nil {
		proxyAddr = a.proxy.Addr()
	}
	dataDir := ""
	if a.store != nil {
		dataDir = a.store.Root()
	}
	skillsList := []skills.Skill{}
	if a.skills != nil {
		if list, err := a.skills.List(); err == nil {
			skillsList = list
		}
	}
	mcpList := []map[string]any{}
	if a.mcp != nil {
		mcpList = a.mcp.List(true)
	}
	masked := s
	masked.QwenAPIKey = maskQwenAPIKey(s.QwenAPIKey)
	masked.DeepSeekAPIKey = maskDeepSeekAPIKey(s.DeepSeekAPIKey)
	return map[string]any{
		"settings":       masked,
		"accounts":       a.store.PublicAccountsForProvider(s.NormalizedProvider()),
		"active":         active,
		"usage":          a.store.UsageSnapshot(),
		"proxy_addr":     proxyAddr,
		"proxy_base":     fmt.Sprintf("http://%s/v1", proxyAddr),
		"data_dir":       dataDir,
		"active_request": a.GetActiveRequest(),
		"skills":         skillsList,
		"mcp_servers":    mcpList,
		"providers":      store.ProviderCatalog(),
		"auth_mode":      s.ProviderAuthMode(),
		"provider":       s.NormalizedProvider(),
		"endpoints": []string{
			"/v1/models", "/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1/search",
		},
	}
}

// qwenKeyMask is the sentinel exposed to the frontend instead of the real
// QwenBridge API key. UpdateSettings ignores patches carrying this value.
const qwenKeyMask = "********"

func maskQwenAPIKey(key string) string {
	if strings.TrimSpace(key) == "" {
		return ""
	}
	return qwenKeyMask
}

// maskDeepSeekAPIKey hides the stored DeepSeek key (an encrypted blob) behind
// the same sentinel the frontend treats as "unchanged".
func maskDeepSeekAPIKey(key string) string {
	if strings.TrimSpace(key) == "" {
		return ""
	}
	return qwenKeyMask
}

func (a *App) GetSettings() store.Settings {
	if a.store == nil {
		return store.Settings{}
	}
	s := a.store.Settings()
	s.QwenAPIKey = maskQwenAPIKey(s.QwenAPIKey)
	s.DeepSeekAPIKey = maskDeepSeekAPIKey(s.DeepSeekAPIKey)
	return s
}

func (a *App) UpdateSettings(patch map[string]any) (store.Settings, error) {
	// DeepSeek API key: encrypt BEFORE persisting so the on-disk blob is always
	// ciphertext (DPAPI on Windows). The masked sentinel means "unchanged" and
	// an explicit empty string clears the stored key. The encrypted blob is
	// written to the secrets vault file (survives legacy settings re-saves);
	// the Settings field keeps a mirror copy.
	var deepSeekKeyEnc string
	var hasDeepSeekKeyPatch bool
	if v, ok := patch["deepseek_api_key"].(string); ok {
		hasDeepSeekKeyPatch = true
		switch {
		case v == qwenKeyMask:
			// unchanged
		case strings.TrimSpace(v) == "":
			deepSeekKeyEnc = ""
			if err := a.store.WriteDeepSeekKeyFile(""); err != nil {
				return store.Settings{}, fmt.Errorf("deepseek: falha ao limpar API key: %w", err)
			}
		default:
			enc, err := secure.Encrypt(strings.TrimSpace(v))
			if err != nil {
				return store.Settings{}, fmt.Errorf("deepseek: falha ao criptografar API key: %w", err)
			}
			if err := a.store.WriteDeepSeekKeyFile(enc); err != nil {
				return store.Settings{}, fmt.Errorf("deepseek: falha ao salvar API key: %w", err)
			}
			deepSeekKeyEnc = enc
		}
	}
	err := a.store.UpdateSettings(func(s *store.Settings) {
		if v, ok := patch["provider"].(string); ok && v != "" {
			// Full switch: resets model + upstream for that provider (no leftover ids).
			s.ApplyProviderDefaults(v)
		}
		if hasDeepSeekKeyPatch {
			s.DeepSeekAPIKey = deepSeekKeyEnc
		}
		if v, ok := patch["default_model"].(string); ok && v != "" {
			s.DefaultModel = v
		}
		if v, ok := patch["gemini_project"].(string); ok && v != "" {
			s.GeminiProject = strings.TrimSpace(v)
		}
		if v, ok := patch["gemini_location"].(string); ok && v != "" {
			s.GeminiLocation = strings.TrimSpace(v)
		}
		if v, ok := patch["qwen_upstream"].(string); ok && v != "" {
			s.QwenUpstream = strings.TrimSpace(v)
		}
		if v, ok := patch["qwen_api_key"].(string); ok {
			// The masked sentinel means "unchanged" — never overwrite the real key.
			if v != qwenKeyMask {
				s.QwenAPIKey = strings.TrimSpace(v)
			}
		}
		if v, ok := patch["reasoning_effort"].(string); ok && v != "" {
			s.ReasoningEffort = v
		}
		if v, ok := patch["api_mode"].(string); ok && v != "" {
			s.APIMode = v
		}
		if v, ok := patch["upstream_base"].(string); ok && v != "" {
			s.UpstreamBase = v
		}
		if v, ok := patch["client_version"].(string); ok && v != "" {
			s.ClientVersion = v
		}
		if v, ok := patch["proxy_listen"].(string); ok && v != "" {
			s.ProxyListen = v
		}
		if v, ok := patch["proxy_enabled"].(bool); ok {
			s.ProxyEnabled = v
		}
		if v, ok := patch["proxy_api_key"].(string); ok {
			s.ProxyAPIKey = v
		}
		if v, ok := patch["store_responses"].(bool); ok {
			s.StoreResponses = v
		}
		if v, ok := patch["force_default_model"].(bool); ok {
			s.ForceDefaultModel = &v
		}
		// Keep provider/upstream/model coherent after any patch.
		if s.IsOllie() && (s.UpstreamBase == "" || strings.Contains(s.UpstreamBase, "cli-chat-proxy")) {
			s.UpstreamBase = store.OllieUpstream
		}
		if s.IsXAI() && strings.Contains(strings.ToLower(s.UpstreamBase), "olliechat") {
			s.UpstreamBase = store.DefaultUpstream
		}
		s.SanitizeModelForProvider()
	})
	if err != nil {
		return store.Settings{}, err
	}
	// Only restart the HTTP listener when enable/listen address actually change.
	// Restarting on every model/provider tweak dropped in-flight Codex streams
	// and could leave the UI looking "dead".
	s := a.store.Settings()
	a.reconcileProxy(s)
	s.QwenAPIKey = maskQwenAPIKey(s.QwenAPIKey)
	s.DeepSeekAPIKey = maskDeepSeekAPIKey(s.DeepSeekAPIKey)
	return s, nil
}

func (a *App) reconcileProxy(s store.Settings) {
	if a.proxy == nil {
		return
	}
	wantListen := s.ProxyListen
	if wantListen == "" {
		wantListen = "127.0.0.1:8787"
	}
	cur := a.proxy.Addr()
	if !s.ProxyEnabled {
		if cur != "" {
			_ = a.proxy.Stop(context.Background())
		}
		return
	}
	// Already listening on the desired host:port — keep connections.
	if cur != "" && (cur == wantListen || "http://"+cur == wantListen) {
		return
	}
	// Addr() may be host:port without scheme; also accept resolved form.
	if cur != "" {
		// If only model/provider changed, Start() is a no-op while srv != nil.
		// Only bounce when the listen target differs.
		if normalizeListen(cur) == normalizeListen(wantListen) {
			return
		}
		_ = a.proxy.Stop(context.Background())
	}
	if err := a.proxy.Start(wantListen); err != nil {
		fallback := "127.0.0.1:8788"
		if err2 := a.proxy.Start(fallback); err2 == nil {
			_ = a.store.UpdateSettings(func(st *store.Settings) { st.ProxyListen = fallback })
		}
	}
}

func normalizeListen(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "https://")
	return s
}

func (a *App) ListAccounts() []map[string]any {
	if a.store == nil {
		return nil
	}
	return a.store.PublicAccountsForProvider(a.store.Settings().NormalizedProvider())
}

// ListAccountsForProvider returns accounts for a specific provider (UI modal).
func (a *App) ListAccountsForProvider(provider string) []map[string]any {
	if a.store == nil {
		return nil
	}
	return a.store.PublicAccountsForProvider(provider)
}

// ListProviders returns the multi-route provider catalog (auth vs api_key).
func (a *App) ListProviders() []map[string]any {
	return store.ProviderCatalog()
}

func (a *App) SetActiveAccount(id string) error {
	if a.store == nil {
		return fmt.Errorf("store not ready")
	}
	if err := a.store.SetActiveAccount(id); err != nil {
		return err
	}
	if a.accio != nil {
		if acc, ok := a.store.GetAccount(id); ok && acc != nil && acc.NormalizedProvider() == store.ProviderAccio {
			if err := a.accio.SetActiveAccount(id); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) RemoveAccount(id string) error {
	if a.store == nil {
		return fmt.Errorf("store not ready")
	}
	acc, ok := a.store.GetAccount(id)
	if !ok || acc == nil {
		return fmt.Errorf("account not found")
	}
	if acc.NormalizedProvider() == store.ProviderAccio && a.accio != nil {
		if err := a.accio.RemoveAccount(id); err != nil {
			return fmt.Errorf("remove Accio account: %w", err)
		}
	}
	return a.store.RemoveAccount(id)
}

// LogoffKimiAccount permanently deletes the Kimi user on kimi.com (Settings → Delete Account)
// using the stored web JWT, then removes the local pool entry. Requires access/refresh session
// (Google login or paste JWT) — sk-kimi-only accounts cannot remote-delete.
func (a *App) LogoffKimiAccount(id string) (map[string]any, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	acc, ok := a.store.GetAccount(id)
	if !ok || acc == nil {
		return nil, fmt.Errorf("account not found")
	}
	if acc.NormalizedProvider() != store.ProviderKimiWork {
		return nil, fmt.Errorf("só contas Kimi Work podem ser deletadas no site")
	}
	if !kimi.HasWebSession(acc.AccessToken, acc.RefreshToken) {
		return nil, fmt.Errorf("esta conta não tem sessão web (só sk-kimi). Faça login Google ou cole o JWT para poder deletar no kimi.com")
	}
	// Refresh web JWT if needed, then DELETE /api/user/logoff.
	if _, err := kimi.LogoffWithSession(acc.AccessToken, acc.RefreshToken); err != nil {
		return nil, fmt.Errorf("falha ao deletar no kimi.com: %w", err)
	}
	email := acc.Email
	label := acc.Label
	_ = a.store.RemoveAccount(id)
	a.safeEmit("account:logoff", map[string]any{
		"id": id, "email": email, "reason": "manual_ui", "remote": true,
	})
	return map[string]any{
		"id": id, "email": email, "label": label, "remote": true, "removed": true,
	}, nil
}

func (a *App) RenameAccount(id, label string) error {
	acc, ok := a.store.GetAccount(id)
	if !ok {
		return fmt.Errorf("account not found")
	}
	acc.Label = label
	return a.store.UpsertAccount(*acc)
}

// StartKimiBrowserLogin mirrors Kimi Desktop Google login:
// open USER's default browser → Google OAuth loopback → POST /api/auth/login/google → mint sk-kimi.
// Does NOT use Playwright and does NOT read Kimi Desktop APPDATA.
func (a *App) StartKimiBrowserLogin() (map[string]any, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	a.safeEmit("kimi:login", map[string]any{
		"phase":   "browser",
		"message": "Abrindo o navegador do sistema (Google)…",
	})
	gl, err := kimi.LoginWithGoogleBrowser(5 * time.Minute)
	if err != nil {
		return nil, err
	}
	access := gl.AccessToken
	refresh := gl.RefreshToken
	if p, _ := kimi.DecodeJWT(access); p != nil && p.Exp > 0 && time.Until(time.Unix(p.Exp, 0)) < 2*time.Minute && refresh != "" {
		if renewed, rerr := kimi.RefreshAccessToken(refresh); rerr == nil && renewed != nil {
			access = renewed.AccessToken
			if renewed.RefreshToken != "" {
				refresh = renewed.RefreshToken
			}
		}
	}
	payload, _ := kimi.DecodeJWT(access)
	return a.addKimiSession(access, refresh, gl.GoogleRefreshToken, payload, "google_browser", gl.Email, gl.Name)
}

// StartKimiStealthLoginNewAccount forces a fresh browser context (isolated profile)
// so the user can log into a *different* Google account without reusing the saved
// persistent profile. Internal proxy still keeps all credentials for refresh/re-login.
func (a *App) StartKimiStealthLoginNewAccount(autoClose bool) (map[string]any, error) {
	return a.startKimiStealthLoginNewAccount(autoClose, false)
}

// startKimiStealthLoginNewAccount is the internal variant; background=true keeps
// global settings (provider/active account) untouched for replenish re-logins.
func (a *App) startKimiStealthLoginNewAccount(autoClose bool, background bool) (map[string]any, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	root, err := a.kimiProjectRoot()
	if err != nil {
		return nil, err
	}
	// Isolated temp profile — forces Google to ask for credentials again.
	profileDir := filepath.Join(root, "browser-data", fmt.Sprintf("google-profile-new-%d", time.Now().UnixNano()))
	_ = os.MkdirAll(profileDir, 0o700)

	a.safeEmit("kimi:login", map[string]any{
		"phase":   "stealth_new",
		"message": "Iniciando login com perfil isolado (nova conta Google)…",
	})
	gl, err := kimi.LoginWithGoogleStealth(root, profileDir, 5*time.Minute, autoClose, a.store.Settings().KimiStealthHeadless, a.store.Settings().GoogleEmail, a.store.Settings().GooglePassword)
	if err != nil {
		return nil, err
	}
	access := gl.AccessToken
	refresh := gl.RefreshToken
	if p, _ := kimi.DecodeJWT(access); p != nil && p.Exp > 0 && time.Until(time.Unix(p.Exp, 0)) < 2*time.Minute && refresh != "" {
		if renewed, rerr := kimi.RefreshAccessToken(refresh); rerr == nil && renewed != nil {
			access = renewed.AccessToken
			if renewed.RefreshToken != "" {
				refresh = renewed.RefreshToken
			}
		}
	}
	payload, _ := kimi.DecodeJWT(access)
	rec, err := a.addKimiSession(access, refresh, gl.GoogleRefreshToken, payload, "google_stealth_new", gl.Email, gl.Name, background)
	if err != nil {
		return nil, err
	}
	if rec != nil {
		rec["mode"] = "playwright_new"
	}
	return rec, nil
}

// StartKimiStealthLogin prefers full-HTTP Google refresh when a stored Google
// refresh_token exists; falls back to a clean isolated Playwright profile.
func (a *App) StartKimiStealthLogin(autoClose bool) (map[string]any, error) {
	return a.StartKimiStealthLoginForAccount("", autoClose)
}

// StartKimiStealthLoginForAccount is used by auto re-login: HTTP Google refresh for
// the preferred account, else clean Playwright profile (no shared stealth profile).
func (a *App) StartKimiStealthLoginForAccount(accountID string, autoClose bool) (map[string]any, error) {
	return a.startKimiStealthLoginForAccount(accountID, autoClose, false)
}

// startKimiStealthLoginForAccount is the internal variant; background=true keeps
// global settings (provider/active account) untouched for replenish re-logins.
func (a *App) startKimiStealthLoginForAccount(accountID string, autoClose bool, background bool) (map[string]any, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	// HTTP-first: reuse stored Google refresh token for this account (or any Kimi).
	if googleRT, emailHint := a.pickGoogleRefreshTokenFor(accountID); googleRT != "" {
		a.safeEmit("kimi:login", map[string]any{
			"phase":   "http_refresh",
			"message": "Login Kimi via HTTP (Google refresh)…",
		})
		gl, err := kimi.LoginWithGoogleRefresh(googleRT)
		if err == nil && gl != nil {
			access := gl.AccessToken
			refresh := gl.RefreshToken
			if p, _ := kimi.DecodeJWT(access); p != nil && p.Exp > 0 && time.Until(time.Unix(p.Exp, 0)) < 2*time.Minute && refresh != "" {
				if renewed, rerr := kimi.RefreshAccessToken(refresh); rerr == nil && renewed != nil {
					access = renewed.AccessToken
					if renewed.RefreshToken != "" {
						refresh = renewed.RefreshToken
					}
				}
			}
			payload, _ := kimi.DecodeJWT(access)
			email := gl.Email
			if email == "" {
				email = emailHint
			}
			rec, serr := a.addKimiSession(access, refresh, gl.GoogleRefreshToken, payload, "google_http_refresh", email, gl.Name, background)
			if serr == nil {
				if rec != nil {
					rec["mode"] = "http_refresh"
				}
				return rec, nil
			}
			err = serr
		}
		log.Printf("kimi http google-refresh failed, falling back to clean profile: %v", err)
		a.safeEmit("kimi:login", map[string]any{
			"phase":   "http_refresh_failed",
			"message": "HTTP falhou — abrindo perfil limpo…",
			"error":   err.Error(),
		})
	}

	a.safeEmit("kimi:login", map[string]any{
		"phase":   "clean_profile",
		"message": "Abrindo login com perfil limpo (Playwright)…",
	})
	return a.startKimiStealthLoginNewAccount(autoClose, background)
}

// pickGoogleRefreshTokenFor returns a stored Google OAuth refresh_token for HTTP re-login.
// Prefers the given accountID, then active Kimi account, then any Kimi account that has one.
func (a *App) pickGoogleRefreshTokenFor(accountID string) (token, email string) {
	if a.store == nil {
		return "", ""
	}
	if accountID != "" {
		if acc, ok := a.store.GetAccount(accountID); ok && acc != nil && acc.NormalizedProvider() == store.ProviderKimiWork {
			if t := strings.TrimSpace(acc.GoogleRefreshToken); t != "" {
				return t, acc.Email
			}
		}
	}
	if acc, ok := a.store.ActiveAccount(); ok && acc != nil && acc.NormalizedProvider() == store.ProviderKimiWork {
		if t := strings.TrimSpace(acc.GoogleRefreshToken); t != "" {
			return t, acc.Email
		}
	}
	for _, row := range a.store.ListAccountsForProvider(store.ProviderKimiWork) {
		if t := strings.TrimSpace(row.GoogleRefreshToken); t != "" {
			return t, row.Email
		}
	}
	return "", ""
}

// pickGoogleRefreshToken returns a stored Google OAuth refresh token (prefer active, then any).
func (a *App) pickGoogleRefreshToken() (token, email string) {
	return a.pickGoogleRefreshTokenFor("")
}

// AddKimiFromJWT mints/reuses sk-kimi from an explicit web access_token JWT (optional fallback).
func (a *App) AddKimiFromJWT(accessToken string) (map[string]any, error) {
	accessToken = strings.TrimSpace(accessToken)
	accessToken = strings.TrimPrefix(accessToken, "Bearer ")
	payload, err := kimi.DecodeJWT(accessToken)
	if err != nil {
		return nil, fmt.Errorf("jwt inválido: %w", err)
	}
	return a.addKimiSession(accessToken, "", "", payload, "paste_jwt", "", "")
}

// AddKimiAPIKey registers a pasted sk-kimi key (optional fallback).
func (a *App) AddKimiAPIKey(apiKey, label string) (map[string]any, error) {
	apiKey = strings.TrimSpace(apiKey)
	if !strings.HasPrefix(apiKey, "sk-kimi-") {
		return nil, fmt.Errorf("api key deve começar com sk-kimi-")
	}
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	id := "kimi-" + apiKey[len(apiKey)-8:]
	a.kimiCapMu.Lock()
	defer a.kimiCapMu.Unlock()
	if _, exists := a.store.GetAccount(id); !exists {
		if n := a.countKimiAccountsTowardCap(); n >= maxKimiWorkAccounts {
			return nil, fmt.Errorf("limite de %d contas Kimi Work — remova uma em Contas antes de adicionar outra (ativas: %d)", maxKimiWorkAccounts, n)
		}
	}
	if label == "" {
		label = "Kimi Work"
	}
	acc := store.Account{
		ID:        id,
		Provider:  store.ProviderKimiWork,
		Label:     label,
		Email:     label,
		APIKey:    apiKey,
		Source:    "paste_key",
		ClientID:  "kimi-work",
		Issuer:    "https://agent-gw.kimi.com",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	_ = a.store.UpdateSettings(func(s *store.Settings) {
		if s.NormalizedProvider() != store.ProviderKimiWork {
			s.ApplyProviderDefaults(store.ProviderKimiWork)
		}
	})
	if err := a.store.UpsertAccount(acc); err != nil {
		return nil, err
	}
	_ = a.store.SetActiveAccount(acc.ID)
	a.safeEmit("account:added", map[string]any{"id": acc.ID, "provider": store.ProviderKimiWork})
	return map[string]any{
		"id": acc.ID, "label": acc.Label, "provider": store.ProviderKimiWork, "source": acc.Source,
	}, nil
}

// kimiProjectRoot finds repo root that contains scripts/kimi-browser-login.mjs
func (a *App) kimiProjectRoot() (string, error) {
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			dir,
			filepath.Join(dir, ".."),
			filepath.Join(dir, "..", ".."),
			filepath.Dir(filepath.Dir(dir)), // build/bin -> repo
		)
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	candidates = append(candidates, `D:\grok proxy open ai compatible`)
	for _, c := range candidates {
		script := filepath.Join(c, "scripts", "kimi-browser-login.mjs")
		if st, err := os.Stat(script); err == nil && !st.IsDir() {
			return c, nil
		}
		// build/bin layout
		script = filepath.Join(c, "..", "..", "scripts", "kimi-browser-login.mjs")
		if st, err := os.Stat(script); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(filepath.Join(c, "..", ".."))
			return abs, nil
		}
	}
	return "", fmt.Errorf("scripts/kimi-browser-login.mjs não encontrado — rode a partir do repo GrokDesktop")
}

// addKimiSession mints a fresh sk-kimi WORK key for a web session and persists it.
// background=true is used by auto replenish re-logins: global settings
// (Provider / ActiveAccountID) are left untouched so a background refill never
// hijacks the user's selected account.
func (a *App) addKimiSession(accessToken, refreshToken, googleRefreshToken string, payload *kimi.JWTPayload, source, email, displayName string, background ...bool) (map[string]any, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	bg := len(background) > 0 && background[0]
	minted, err := kimi.MintWorkAPIKey(accessToken, "grok-desktop-kimi")
	if err != nil {
		return nil, err
	}
	// Validate the minted sk-kimi key actually works on the upstream before saving.
	if err := kimi.TestAPIKey(minted.APIKey); err != nil {
		return nil, fmt.Errorf("sk-kimi mintado rejeitado pelo upstream: %w", err)
	}
	// Validate Kimi refresh token if present (so future re-login works).
	if refreshToken != "" {
		if _, err := kimi.RefreshAccessToken(refreshToken); err != nil {
			return nil, fmt.Errorf("refresh token Kimi inválido: %w", err)
		}
	}
	// Validate Google refresh token if present (so HTTP re-login works).
	if googleRefreshToken != "" {
		if _, err := kimi.RefreshGoogleIDToken(googleRefreshToken); err != nil {
			return nil, fmt.Errorf("Google refresh token inválido ou revogado: %w", err)
		}
	}
	userID := minted.UserID
	if userID == "" && payload != nil {
		userID = payload.Sub
	}
	email = strings.TrimSpace(email)
	displayName = strings.TrimSpace(displayName)
	id := "kimi-" + userID
	if userID == "" {
		id = "kimi-" + minted.APIKey[len(minted.APIKey)-8:]
	}
	// Cap: max N active Kimi accounts. Existing id (re-login same user) always allowed.
	// Exhausted rows do not count so auto re-login can refill after remote logoff.
	// kimiCapMu makes the count-check + insert atomic (two parallel re-logins must
	// not both pass the check and exceed the cap).
	a.kimiCapMu.Lock()
	defer a.kimiCapMu.Unlock()
	if _, exists := a.store.GetAccount(id); !exists {
		if n := a.countKimiAccountsTowardCap(); n >= maxKimiWorkAccounts {
			return nil, fmt.Errorf("limite de %d contas Kimi Work — remova uma em Contas antes de adicionar outra (ativas: %d)", maxKimiWorkAccounts, n)
		}
	}
	// Prefer Gmail so multi-account UI is readable.
	label := "Kimi Work"
	if email != "" {
		label = email
	} else if displayName != "" {
		label = displayName
	} else if userID != "" {
		label = "Kimi · " + userID
		if len(userID) > 12 {
			label = "Kimi · " + userID[:8] + "…"
		}
	}
	// Preserve custom label / created on re-login same user
	if prev, ok := a.store.GetAccount(id); ok && prev != nil {
		if prev.Label != "" && prev.Label != prev.Email && prev.Label != userID &&
			!strings.HasPrefix(strings.ToLower(prev.Label), "esgotada") &&
			!strings.HasPrefix(strings.ToLower(prev.Label), "auth-denied") {
			label = prev.Label
		}
		if email == "" && prev.Email != "" && strings.Contains(prev.Email, "@") {
			email = prev.Email
		}
		// Preserve existing Google refresh token if new login didn't provide one.
		// (Google only sends refresh_token on the first authorization_code exchange.)
		if googleRefreshToken == "" && prev.GoogleRefreshToken != "" {
			googleRefreshToken = prev.GoogleRefreshToken
		}
	}
	if email == "" {
		email = label
	}
	now := time.Now().UTC()
	created := now
	if prev, ok := a.store.GetAccount(id); ok && prev != nil && !prev.CreatedAt.IsZero() {
		created = prev.CreatedAt
	}
	acc := store.Account{
		ID:                 id,
		Provider:           store.ProviderKimiWork,
		Label:              label,
		Email:              email,
		UserID:             userID,
		APIKey:             minted.APIKey,
		DeviceID:           minted.DeviceID,
		Source:             source,
		ClientID:           "kimi-work",
		Issuer:             "https://www.kimi.com",
		AccessToken:        accessToken,
		RefreshToken:       refreshToken,
		GoogleRefreshToken: googleRefreshToken,
		ExpiresAt:          minted.ExpiresAt,
		CreatedAt:          created,
		UpdatedAt:          now,
		// Fresh login always clears prior poison marks.
		ExhaustedAt:      time.Time{},
		ExhaustReason:    "",
		AuthDeniedAt:     time.Time{},
		AuthDeniedReason: "",
	}
	if !bg {
		_ = a.store.UpdateSettings(func(s *store.Settings) {
			if s.NormalizedProvider() != store.ProviderKimiWork {
				s.ApplyProviderDefaults(store.ProviderKimiWork)
			}
			s.ActiveAccountID = acc.ID
			s.Provider = store.ProviderKimiWork
		})
	}
	if err := a.store.UpsertAccount(acc); err != nil {
		return nil, err
	}
	if !bg {
		_ = a.store.SetActiveAccount(acc.ID)
	}
	_ = a.store.ClearAuthState(acc.ID)
	a.safeEmit("account:added", map[string]any{"id": acc.ID, "provider": store.ProviderKimiWork, "user_id": userID, "source": source})
	hint := minted.APIKey
	if len(hint) > 14 {
		hint = hint[:10] + "…" + hint[len(hint)-4:]
	}
	return map[string]any{
		"id": acc.ID, "label": acc.Label, "email": email, "user_id": userID, "provider": store.ProviderKimiWork,
		"api_key_hint": hint, "source": source,
		"has_refresh":           refreshToken != "",
		"refresh_tested":        true,
		"has_google_refresh":    googleRefreshToken != "",
		"google_refresh_tested": googleRefreshToken != "",
		"upstream_tested":       true,
		"expires_at":            minted.ExpiresAt,
		"expires_in_seconds":    int(time.Until(minted.ExpiresAt).Seconds()),
	}, nil
}

// StartDeviceLogin begins OAuth device flow. Frontend shows URL + code.
func (a *App) StartDeviceLogin() (*deviceLoginState, error) {
	a.mu.Lock()
	if a.deviceCancel != nil {
		a.deviceCancel()
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Minute)
	a.deviceCancel = cancel
	a.mu.Unlock()

	start, err := a.oauth.StartDevice(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	url := start.VerificationURIComplete
	if url == "" {
		url = start.VerificationURI
	}
	st := &deviceLoginState{
		DeviceCode:      start.DeviceCode,
		UserCode:        start.UserCode,
		VerificationURL: url,
		Interval:        start.Interval,
		ExpiresIn:       start.ExpiresIn,
		StartedAt:       time.Now().UTC().Format(time.RFC3339),
	}
	a.mu.Lock()
	a.deviceState = st
	a.mu.Unlock()

	// background poll
	go func() {
		tok, err := a.oauth.PollDevice(ctx, start.DeviceCode, start.Interval)
		if err != nil {
			if ctx.Err() == nil {
				runtime.EventsEmit(a.ctx, "auth:error", err.Error())
			}
			return
		}
		acc := oauth.AccountFromToken(tok, a.oauth.ClientID, a.oauth.Issuer)
		email, uid := a.oauth.UserInfo(context.Background(), tok.AccessToken, a.oauth.Issuer)
		if email != "" {
			acc.Email = email
		}
		if uid != "" {
			acc.UserID = uid
			acc.ID = uid
		}
		// Re-login same xAI user → refresh tokens; keep custom label if any.
		if prev, ok := a.store.GetAccount(acc.ID); ok && prev != nil {
			lowLabel := strings.ToLower(prev.Label)
			if prev.Label != "" && prev.Label != prev.Email && prev.Label != "Grok account" &&
				!strings.Contains(lowLabel, "esgotada") && !strings.Contains(lowLabel, "auth-denied") {
				acc.Label = prev.Label
			}
			acc.CreatedAt = prev.CreatedAt
			// Fresh OAuth after re-auth clears quota / auth-denied marks.
			acc.ExhaustedAt = time.Time{}
			acc.ExhaustReason = ""
			acc.AuthDeniedAt = time.Time{}
			acc.AuthDeniedReason = ""
		}
		if acc.Label == "" || acc.Label == "Grok account" {
			if acc.Email != "" {
				acc.Label = acc.Email
			} else if len(acc.ID) >= 8 {
				acc.Label = "Conta " + acc.ID[:8]
			} else {
				acc.Label = "Conta"
			}
		}
		acc.Provider = store.ProviderXAI
		acc.Source = "oauth"
		if err := a.store.UpsertAccount(acc); err != nil {
			runtime.EventsEmit(a.ctx, "auth:error", err.Error())
			return
		}
		// New account (or re-auth) becomes the active one for the next request.
		_ = a.store.SetActiveAccount(acc.ID)
		runtime.EventsEmit(a.ctx, "auth:success", map[string]any{
			"id":       acc.ID,
			"email":    acc.Email,
			"label":    acc.Label,
			"provider": store.ProviderXAI,
			"accounts": a.store.PublicAccountsForProvider(store.ProviderXAI),
			"count":    len(a.store.ListAccountsForProvider(store.ProviderXAI)),
		})
		a.mu.Lock()
		a.deviceState = nil
		a.mu.Unlock()
	}()

	return st, nil
}

func (a *App) CancelDeviceLogin() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deviceCancel != nil {
		a.deviceCancel()
		a.deviceCancel = nil
	}
	a.deviceState = nil
}

func (a *App) GetDeviceLoginState() *deviceLoginState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.deviceState
}

func (a *App) OpenExternal(url string) {
	runtime.BrowserOpenURL(a.ctx, url)
}

// StartAccioLogin starts the native Accio browser callback flow. Tokens stay in
// the private AppData store and are never returned to the webview.
func (a *App) StartAccioLogin() (string, error) {
	if a.accio == nil {
		return "", fmt.Errorf("accio provider is not initialized")
	}
	// Let the native client choose an available loopback port so a stale
	// callback listener cannot make login appear to start without receiving it.
	loginURL, err := a.accio.StartLogin(a.ctx, 0)
	if err != nil {
		return "", err
	}
	runtime.BrowserOpenURL(a.ctx, loginURL)
	return loginURL, nil
}

func (a *App) syncAccioAccount(t accio.TokenRecord) {
	if a.store == nil || strings.TrimSpace(t.AccessToken) == "" {
		return
	}
	id := t.ID
	if id == "" {
		id = "accio-" + fmt.Sprintf("%x", md5.Sum([]byte(t.AccessToken)))[:16]
	}
	if err := a.store.UpsertAccount(store.Account{
		ID: id, Provider: store.ProviderAccio, Label: firstNonEmpty(t.Label, "Accio"), Email: t.Email,
		AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, Source: "accio-login",
		ClientID: "accio-work", Issuer: "https://www.accio.com", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		logging.Warn("accio.account.sync_failed", "account_id", id, "err", err.Error())
		return
	}
	// Login completion should make the newly captured account active only when
	// Accio is the selected route; this avoids unexpectedly switching providers.
	if a.store.Settings().IsAccio() {
		_ = a.store.SetActiveAccount(id)
		if a.accio != nil {
			_ = a.accio.SetActiveAccount(id)
		}
	}
}

func (a *App) AccioStatus() map[string]any {
	if a.accio == nil {
		return map[string]any{"authenticated": false, "error": "not initialized"}
	}
	return a.accio.Status()
}

// AccioCredits queries the entitlement/quota endpoint and returns total,
// remaining, used, and plan type for the avatar progress indicator.
func (a *App) AccioCredits() (map[string]any, error) {
	if a.accio == nil {
		return nil, fmt.Errorf("accio provider is not initialized")
	}
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	return a.accio.Credits(ctx)
}

// AccioManagerStatus exposes the account top-up manager state for the UI.
func (a *App) AccioManagerStatus() map[string]any {
	if a.accMgr == nil {
		return map[string]any{"enabled": false, "error": "not initialized"}
	}
	return a.accMgr.Status()
}

// SetAccioManagerConfig reconfigures the top-up manager at runtime.
// enabled=false pauses creation; minCredits/maxAccounts are validated to sane
// bounds before being applied.
func (a *App) SetAccioManagerConfig(enabled bool, minCredits, maxAccounts int) {
	if a.accMgr == nil {
		return
	}
	if minCredits < 0 {
		minCredits = 0
	}
	if maxAccounts < 1 {
		maxAccounts = 1
	}
	if maxAccounts > 50 {
		maxAccounts = 50
	}
	a.accMgr.SetEnabled(enabled)
	a.accMgr.SetConfig(accmgr.Config{
		MinCredits:     minCredits,
		MaxAccounts:    maxAccounts,
		CreateCooldown: envDur("ACCIO_CREATE_COOLDOWN", 3*time.Minute),
		CheckEvery:     envDur("ACCIO_CHECK_EVERY", 2*time.Minute),
		WARP:           os.Getenv("ACCIO_USE_WARP") != "0",
		Headless:       os.Getenv("ACCIO_SIGNUP_VISIBLE") != "1",
	})
	logging.Info("app.accmgr.config", "enabled", enabled, "min_credits", minCredits, "max_accounts", maxAccounts)
}

// AccioTopUpNow forces one account creation immediately (UI button / testing).
func (a *App) AccioTopUpNow() (map[string]any, error) {
	if a.accMgr == nil {
		return nil, fmt.Errorf("accio manager is not initialized")
	}
	ctx, cancel := context.WithTimeout(a.ctx, 7*time.Minute)
	defer cancel()
	if err := a.accMgr.TopUpNow(ctx); err != nil {
		return nil, err
	}
	return a.accMgr.Status(), nil
}

func (a *App) ListModels() ([]upstream.ModelInfo, error) {
	settings := a.store.Settings()
	if settings.IsAccio() {
		if a.accio == nil {
			return nil, fmt.Errorf("accio provider is not initialized")
		}
		models, err := a.accio.Models(a.ctx)
		if err != nil {
			return nil, err
		}
		out := make([]upstream.ModelInfo, 0, len(models))
		for _, m := range models {
			freeUse, locked := m.FreeUse, m.Locked
			out = append(out, upstream.ModelInfo{
				ID: m.ID, Name: m.Name, Description: m.Description, APIMode: "chat",
				ContextWindow: m.Context, ReasoningEfforts: m.ReasoningEfforts,
				DefaultReasoningEffort: m.DefaultReasoningEffort, FreeUse: &freeUse, Locked: &locked,
			})
		}
		return out, nil
	}
	if settings.IsKimiWork() {
		out := make([]upstream.ModelInfo, 0)
		for _, m := range kimi.StaticModels() {
			out = append(out, upstream.ModelInfo{
				ID: m["id"], Name: m["name"], Description: "Kimi Work · coding", APIMode: m["api_mode"],
			})
		}
		return out, nil
	}
	// Track the Inc'd account so the in-flight counter does not leak.
	ctx, inflight := store.WithInflightTracker(a.ctx)
	defer inflight.DecAll(a.store)
	token, acc, settings, err := a.ensureCreds(ctx)
	if err != nil {
		return nil, err
	}
	if acc != nil {
		store.TrackInflight(ctx, acc.ID)
	}
	return a.upstream.ListModels(ctx, token, settings)
}

func (a *App) GetUsage() map[string]store.UsageTotals {
	if a.store == nil {
		return map[string]store.UsageTotals{}
	}
	return a.store.UsageSnapshot()
}

// GetStats returns usage totals, latency history, pricing and OpenAI-compatible config.
func (a *App) GetStats() map[string]any {
	if a.store == nil {
		return map[string]any{"error": "store not ready"}
	}
	usage := a.store.UsageSnapshot()
	hist := a.store.History(60)
	g := usage["_global"]
	avgLat, avgTTFT := int64(0), int64(0)
	if g.LatencySamples > 0 {
		avgLat = g.LatencySumMs / g.LatencySamples
		avgTTFT = g.TTFTSumMs / g.LatencySamples
	}

	// sparkline series (oldest → newest for chart L→R)
	latSeries := make([]int64, 0, len(hist))
	ttftSeries := make([]int64, 0, len(hist))
	costSeries := make([]float64, 0, len(hist))
	for i := len(hist) - 1; i >= 0; i-- {
		h := hist[i]
		latSeries = append(latSeries, h.LatencyMs)
		ttftSeries = append(ttftSeries, h.TTFTMs)
		costSeries = append(costSeries, h.CostUSD)
	}

	proxyAddr := ""
	if a.proxy != nil {
		proxyAddr = a.proxy.Addr()
	}
	if proxyAddr == "" {
		proxyAddr = a.store.Settings().ProxyListen
	}
	baseURL := fmt.Sprintf("http://%s/v1", proxyAddr)
	modelsURL := baseURL + "/models"
	settings := a.store.Settings()
	apiKey := settings.ProxyAPIKey
	if apiKey == "" {
		apiKey = "local" // placeholder for clients that require a key
	}
	providerName := "Grok Proxy Plus"

	// OpenCode: same baseURL lists Grok + Kimi + OpenCode Zen Free; client picks model id.
	openCodeJSON := fmt.Sprintf(`{
  "provider": {
    "grok-proxy-plus": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "%s",
      "options": {
        "baseURL": "%s",
        "apiKey": "%s"
      },
      "models": {
        "grok-4.5": { "name": "Grok 4.5 (xAI · /v1/responses)" },
        "k3-agent": { "name": "K3 Max (Work)" },
        "k3-agent-low": { "name": "K3 Max Low Think" },
        "k3-agent-medium": { "name": "K3 Max Medium Think" },
        "k3-agent-high": { "name": "K3 Max High Think" },
        "k3-agent-xhigh": { "name": "K3 Max Extra High Think" },
        "k2d6-agent": { "name": "K2.6 Agent (Work)" },
        "k2d6-agent-low": { "name": "K2.6 Agent Low Think" },
        "k2d6-agent-medium": { "name": "K2.6 Agent Medium Think" },
        "k2d6-agent-high": { "name": "K2.6 Agent High Think" },
		"k2d6-agent-xhigh": { "name": "K2.6 Agent Extra High Think" },
		"opencode/deepseek-v4-flash-free": { "name": "DeepSeek V4 Flash Free (OpenCode Zen)" },
		"opencode/big-pickle": { "name": "Big Pickle (OpenCode Zen)" },
		"opencode/mimo-v2.5-free": { "name": "MiMo V2.5 Free (OpenCode Zen)" },
		"opencode/nemotron-3-ultra-free": { "name": "Nemotron 3 Ultra Free (OpenCode Zen)" },
		"opencode/north-mini-code-free": { "name": "North Mini Code Free (OpenCode Zen)" },
		"opencode/ling-3.0-flash-free": { "name": "Ling 3.0 Flash Free (OpenCode Zen)" },
		"opencode/laguna-s-2.1-free": { "name": "Laguna S 2.1 Free (OpenCode Zen)" }
      }
    }
  }
}`, providerName, baseURL, apiKey)

	kiloGuide := fmt.Sprintf(`Kilo Code · OpenAI Compatible

1. Provider type: OpenAI Compatible
2. Base URL:  %s
3. API Key:   %s  (qualquer string se o app não exige key)
4. Models:    o Kilo chama GET %s
              e lista o que o proxy devolve (Grok + Kimi juntos).
5. Escolha o model no dropdown do Kilo:
   • grok-4.5          → Grok (POST /v1/responses)
   • k3-agent          → K3 Max (padrão)
   • k3-agent-low      → K3 Max Low Think
   • k3-agent-medium   → K3 Max Medium Think
   • k3-agent-high     → K3 Max High Think
   • k3-agent-xhigh    → K3 Max Extra High Think
   • k2d6-agent        → K2.6 Agent (padrão)
   • k2d6-agent-low    → K2.6 Agent Low Think
   • k2d6-agent-medium → K2.6 Agent Medium Think
   • k2d6-agent-high   → K2.6 Agent High Think
   • k2d6-agent-xhigh  → K2.6 Agent Extra High Think

Importante:
• Não precisa trocar "provedor ativo" no app desktop.
• O model da request decide Grok vs Kimi.
• App precisa estar aberto (proxy local).`, baseURL, apiKey, modelsURL)

	openaiEnv := fmt.Sprintf(`# Base do proxy (OpenAI-compatible)
OPENAI_BASE_URL=%s
OPENAI_API_KEY=%s

# Escolha UM model por request:
# Grok:  OPENAI_MODEL=grok-4.5          → use /v1/responses
# Kimi:  OPENAI_MODEL=kimi-for-coding   → use /v1/chat/completions`, baseURL, apiKey)

	curlExample := fmt.Sprintf(`# Listar models (Grok + Kimi)
curl %s

# Grok (Responses)
curl %s/responses \
  -H "Authorization: Bearer %s" \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"grok-4.5\",\"stream\":true,\"input\":\"Olá\"}"

# Kimi Work (chat/completions)
curl %s/chat/completions \
  -H "Authorization: Bearer %s" \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"kimi-for-coding\",\"stream\":false,\"messages\":[{\"role\":\"user\",\"content\":\"Olá\"}]}"`,
		modelsURL, baseURL, apiKey, baseURL, apiKey)

	zooGuide := fmt.Sprintf(`Zoo Code · VS Code Extension

1. Open VS Code Settings (Ctrl+,) → search "Zoo Code provider"
2. Add a custom OpenAI-compatible provider:
   • Name:     Grok Proxy Plus
   • Base URL: %s
   • API Key:  %s
3. Models (add all or pick the ones you want):
   • grok-4.5
   • kimi-for-coding
   • k3-agent
   • k3-agent-low
   • k3-agent-medium
   • k3-agent-high
   • k3-agent-xhigh

Tip: The proxy auto-routes by model id.
   grok-4.5 → xAI /responses
   kimi-*   → Kimi Work /chat/completions`, baseURL, apiKey)

	return map[string]any{
		"usage":          usage,
		"global":         g,
		"history":        hist,
		"latency_series": latSeries,
		"ttft_series":    ttftSeries,
		"cost_series":    costSeries,
		"avg_latency_ms": avgLat,
		"avg_ttft_ms":    avgTTFT,
		"pricing":        pricing.AllRates(),
		"zoo_guide":      zooGuide,
		"active_rate":    "auto",
		"proxy": map[string]any{
			"base_url":       baseURL,
			"models_url":     modelsURL,
			"api_key":        apiKey,
			"route":          "model",
			"openai_env":     openaiEnv,
			"opencode":       openCodeJSON,
			"kilo":           kiloGuide,
			"curl":           curlExample,
			"models_note":    "GET /v1/models lista Grok + Kimi + OpenCode Zen Free. O client escolhe o model; o proxy roteia.",
			"models_example": []string{"grok-4.5", "kimi-for-coding", "k3-agent", "opencode/deepseek-v4-flash-free", "opencode/mimo-v2.5-free"},
		},
		"data_dir": a.store.Root(),
	}
}

func (a *App) GetActiveRequest() *ActiveRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeReq == nil {
		return nil
	}
	cp := *a.activeReq
	return &cp
}

func (a *App) CancelChat() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reqCancel != nil {
		a.reqCancel()
		a.reqCancel = nil
	}
	a.activeReq = nil
	runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "done"})
}

// SendChat streams a completion. Events: chat:event
// Provider is chosen from the request model (same as HTTP proxy), not the UI global.
func (a *App) SendChat(req upstream.ChatRequest) error {
	route := a.store.Settings().WithProviderForModel(req.Model)
	if message := store.ProviderAvailabilityMessage(route.NormalizedProvider()); message != "" && store.ProviderAvailabilityBlocksRequests(route.NormalizedProvider()) {
		return errors.New(message)
	}
	ctxRoute := proxyhttp.WithRouteProvider(a.ctx, route.NormalizedProvider())
	// Track accounts Inc'd by ensure (initial pick + rotation retries) so
	// in-flight counters are decremented exactly once per Inc when the stream ends.
	ctxRoute, inflight := store.WithInflightTracker(ctxRoute)
	token, acc, settings, err := a.ensureCreds(ctxRoute)
	if err != nil {
		inflight.DecAll(a.store)
		return err
	}
	if acc != nil {
		store.TrackInflight(ctxRoute, acc.ID)
	}
	settings = settings.WithProvider(route.NormalizedProvider())
	if req.Model == "" {
		req.Model = settings.DefaultModel
	}
	// UI-only: desktop chat uses global settings when the in-app composer omits effort.
	// The HTTP proxy never does this — OpenCode/API clients own their request body.
	if req.ReasoningEffort == "" {
		req.ReasoningEffort = settings.ReasoningEffort
	}
	if req.APIMode == "" {
		req.APIMode = settings.APIMode
	}
	// Kimi Work agent-gw has no /responses — always chat/completions.
	if settings.IsAccio() || settings.IsDeepSeek() || settings.IsOpenCodeZen() {
		req.APIMode = "chat"
	}
	if settings.IsKimiWork() {
		req.APIMode = "chat"
		req.Model = kimi.ResolveKimiModel(req.Model)
		if req.Model == "" || req.Model == "default" {
			req.Model = store.KimiWorkDefaultModel
		}
	} else if settings.IsXAI() {
		req.APIMode = "responses"
	}

	a.mu.Lock()
	if a.reqCancel != nil {
		a.reqCancel()
	}
	// Derive from ctxRoute so retries keep the pinned route provider and the
	// in-flight tracker (never the UI global provider mid-request).
	ctx, cancel := context.WithCancel(ctxRoute)
	a.reqCancel = cancel
	reqID := uuid.NewString()
	label := acc.Label
	if label == "" {
		label = acc.Email
	}
	a.activeReq = &ActiveRequest{
		ID:        reqID,
		AccountID: acc.ID,
		Email:     acc.Email,
		Label:     label,
		Model:     req.Model,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Phase:     "thinking",
	}
	a.mu.Unlock()
	runtime.EventsEmit(a.ctx, "request:active", a.GetActiveRequest())
	logging.Info("chat.ui.start", "provider", route.NormalizedProvider(), "model", req.Model, "account_id", acc.ID)
	chatT0 := time.Now()

	// adoptCreds tracks a rotated/force-refreshed account's Inc and releases the
	// previous account's in-flight counter (the old stream already ended).
	adoptCreds := func(prevID string, acc2 *store.Account) {
		if acc2 == nil {
			return
		}
		store.TrackInflight(ctx, acc2.ID)
		if prevID != "" && acc2.ID != prevID {
			if n := inflight.Remove(prevID); n > 0 {
				for i := 0; i < n; i++ {
					a.store.DecAccountRequestCount(prevID)
				}
			}
		}
	}

	go func() {
		defer inflight.DecAll(a.store)
		defer func() {
			a.mu.Lock()
			a.reqCancel = nil
			a.activeReq = nil
			a.mu.Unlock()
			runtime.EventsEmit(a.ctx, "request:active", nil)
		}()
		// Wails event delivery is much more expensive than appending a token.
		// Keep first-token latency immediate, then coalesce text for one frame.
		// Control events still arrive in order and flush pending text first.
		chatEvents := newChatEventBatcher(func(ev upstream.StreamEvent) {
			a.safeEmit("chat:event", ev)
		})
		defer chatEvents.Close()

		emit := func(ev upstream.StreamEvent) {
			switch ev.Type {
			case "thinking":
				a.setPhase("thinking")
			case "content":
				a.setPhase("content")
			case "tool_call", "search_query", "search_results":
				a.setPhase("searching")
			}
			if ev.Account == "" {
				ev.Account = label
			}
			if ev.Email == "" {
				ev.Email = acc.Email
			}
			// Fan-out search UI events (native xAI web_search / x_search)
			switch ev.Type {
			case "search_query":
				payload := map[string]any{
					"query": ev.Text, "provider": "xAI", "tool_call_id": ev.ID,
				}
				if ev.Payload != nil {
					for k, v := range ev.Payload {
						payload[k] = v
					}
				}
				runtime.EventsEmit(a.ctx, "search:start", payload)
			case "search_results":
				payload := map[string]any{"query": ev.Text, "provider": "xAI", "tool_call_id": ev.ID}
				if ev.Payload != nil {
					for k, v := range ev.Payload {
						payload[k] = v
					}
				}
				runtime.EventsEmit(a.ctx, "search:results", payload)
				runtime.EventsEmit(a.ctx, "search:done", map[string]any{"query": ev.Text, "tool_call_id": ev.ID})
			case "tool_call":
				runtime.EventsEmit(a.ctx, "tool:call", map[string]any{
					"id": ev.ID, "name": ev.Text, "payload": ev.Payload,
				})
			case "tool_done":
				runtime.EventsEmit(a.ctx, "tool:done", map[string]any{
					"id": ev.ID, "name": ev.Text, "payload": ev.Payload,
				})
			case "tool_error":
				runtime.EventsEmit(a.ctx, "search:error", map[string]any{
					"error": ev.Error, "tool_call_id": ev.ID,
				})
			}
			chatEvents.Push(ev)
			if ev.Type == "usage" && ev.Usage != nil {
				model := ev.Model
				if model == "" {
					model = req.Model
				}
				cost := pricing.CostUSD(model, route.NormalizedProvider(), ev.Usage.PromptTokens, ev.Usage.CompletionTokens, ev.Usage.ReasoningTokens, ev.Usage.CachedTokens)
				total := ev.Usage.TotalTokens
				if total == 0 {
					total = ev.Usage.PromptTokens + ev.Usage.CompletionTokens
				}
				sample := store.RequestSample{
					ID:               ev.ID,
					At:               time.Now().UTC().Format(time.RFC3339),
					AccountID:        acc.ID,
					Model:            model,
					PromptTokens:     ev.Usage.PromptTokens,
					CompletionTokens: ev.Usage.CompletionTokens,
					ReasoningTokens:  ev.Usage.ReasoningTokens,
					CachedTokens:     ev.Usage.CachedTokens,
					TotalTokens:      total,
					CostUSD:          cost,
					LatencyMs:        ev.LatencyMs,
					TTFTMs:           ev.TTFTMs,
					Estimated:        ev.Estimated,
				}
				_ = a.store.RecordRequest(sample)
				runtime.EventsEmit(a.ctx, "usage:update", a.store.UsageSnapshot())
				runtime.EventsEmit(a.ctx, "stats:sample", sample)
			}
		}

		if req.ReasoningEffort == "" {
			req.ReasoningEffort = settings.ReasoningEffort
		}
		// xAI: Responses + native search.
		// Kimi Work / Ollie / Gemini / Qwen / DeepSeek: OpenAI chat/completions only.
		if settings.IsOllie() || settings.IsGemini() || settings.IsKimiWork() || settings.IsQwen() || settings.IsDeepSeek() || settings.IsOpenCodeZen() || settings.IsAccio() {
			req.APIMode = "chat"
		} else {
			req.APIMode = "responses"
		}
		// Kimi wire model id
		if settings.IsKimiWork() {
			req.Model = kimi.ResolveKimiModel(req.Model)
			if req.Model == "" {
				req.Model = store.KimiWorkDefaultModel
			}
		}

		// Inject skills + MCP catalog into conversation context.
		req.Messages = a.injectAgentContext(req.Messages)

		// Accio uses its native Phoenix wire format; do not send it through the
		// generic OpenAI upstream client or let another provider's retry logic run.
		if settings.IsAccio() {
			if a.accio == nil {
				runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "error", Error: "Accio provider is not initialized"})
				runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "done"})
				return
			}
			raw, marshalErr := json.Marshal(req)
			if marshalErr != nil {
				runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "error", Error: marshalErr.Error()})
				runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "done"})
				return
			}
			var native map[string]any
			if marshalErr = json.Unmarshal(raw, &native); marshalErr != nil {
				runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "error", Error: marshalErr.Error()})
				runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "done"})
				return
			}
			native["model"] = req.Model
			native["stream"] = true
			native["request_id"] = reqID
			logging.Info("chat.accio.payload", "model", req.Model, "msgs", len(req.Messages), "effort", req.ReasoningEffort, "api_mode", req.APIMode, "token_len", len(token))
			firstThinking := false
			firstContent := false
			firstDone := false
			accioEmit := func(text, reasoning string, call map[string]any, done bool) {
				if reasoning != "" && !firstThinking {
					firstThinking = true
					logging.Info("chat.accio.first_thinking", "preview", truncText(reasoning, 40))
				}
				if text != "" && !firstContent {
					firstContent = true
					logging.Info("chat.accio.first_content", "preview", truncText(text, 40))
				}
				if done && !firstDone {
					firstDone = true
					logging.Info("chat.accio.event_done", "text_chars", len(text), "has_content", firstContent, "has_thinking", firstThinking)
				}
				if reasoning != "" {
					emit(upstream.StreamEvent{Type: "thinking", Text: reasoning, Model: req.Model})
				}
				if text != "" {
					emit(upstream.StreamEvent{Type: "content", Text: text, Model: req.Model})
				}
				if call != nil {
					emit(upstream.StreamEvent{Type: "tool_call", ID: fmt.Sprint(call["id"]), Text: fmt.Sprint(call["function"]), Model: req.Model, Payload: call})
				}
				if done {
					emit(upstream.StreamEvent{Type: "done", Model: req.Model})
				}
			}
			var err error
			for attempt := 0; attempt < 3; attempt++ {
				err = a.accio.StreamWithToken(ctx, native, token, accioEmit)
				if err == nil || ctx.Err() != nil {
					break
				}
				rawErr := accio.GatewayRawError(err)
				logging.Error("accio.gateway.error", "account_id", acc.ID, "model", req.Model, "attempt", attempt+1, "status", accio.GatewayStatus(err), "raw", rawErr)
				runtime.EventsEmit(a.ctx, "accio:error", map[string]any{"account_id": acc.ID, "model": req.Model, "attempt": attempt + 1, "status": accio.GatewayStatus(err), "raw": rawErr, "error": err.Error()})
				if isQuotaExhaustedErr(err) {
					a.markAccountExhausted(acc.ID, rawErr)
				} else if isAuthDeniedErr(err) {
					a.markAccountAuthDenied(acc.ID, rawErr)
				} else {
					break
				}
				next := a.pickNonExhausted(acc.ID)
				if next == "" {
					break
				}
				prev := acc.ID
				tok2, acc2, _, err2 := a.ensureCredsFor(ctx, next)
				if err2 != nil || acc2 == nil {
					break
				}
				token, acc = tok2, acc2
				label = firstNonEmpty(acc.Label, acc.Email)
				runtime.EventsEmit(a.ctx, "account:rotated", map[string]any{"id": acc.ID, "from": prev, "reason": "accio"})
				logging.Warn("accio.account.rotated", "from_account", prev, "to_account", acc.ID, "reason", "gateway_error")
			}
			if err != nil && ctx.Err() == nil {
				rawErr := accio.GatewayRawError(err)
				logging.Error("chat.ui.error", "provider", route.NormalizedProvider(), "model", req.Model, "account_id", acc.ID, "raw", rawErr, "err", err.Error(), "duration_ms", time.Since(chatT0).Milliseconds())
				emit(upstream.StreamEvent{Type: "error", Error: rawErr, Model: req.Model})
				emit(upstream.StreamEvent{Type: "done", Model: req.Model})
			}
			return
		}

		err := a.upstream.StreamChat(ctx, token, settings, label, acc.Email, req, emit)
		if err != nil && ctx.Err() == nil {
			// Gemini uses ADC (no multi-account rotate).
			if settings.IsGemini() || settings.IsOpenCodeZen() || settings.IsOllie() || settings.IsQwen() || settings.IsDeepSeek() {
				logging.Error("chat.ui.error", "provider", route.NormalizedProvider(), "model", req.Model, "account_id", acc.ID, "err", err, "duration_ms", time.Since(chatT0).Milliseconds())
				runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "error", Error: err.Error()})
				runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "done"})
				return
			}
			// Auth failure: force-refresh once, then rotate.
			if isAuthDeniedErr(err) && acc != nil {
				if tokR, accR, settingsR, errR := a.forceRefreshAccount(ctx, acc.ID); errR == nil && tokR != "" && tokR != token {
					adoptCreds(acc.ID, accR)
					err = a.upstream.StreamChat(ctx, tokR, settingsR, label, accR.Email, req, emit)
					if err == nil {
						logging.Info("chat.ui.done", "provider", route.NormalizedProvider(), "model", req.Model, "account_id", acc.ID, "duration_ms", time.Since(chatT0).Milliseconds())
						return
					}
					token, acc, settings = tokR, accR, settingsR
				}
				if err != nil && isAuthDeniedErr(err) {
					a.markAccountAuthDenied(acc.ID, err.Error())
					if nextID := a.pickNonExhausted(acc.ID); nextID != "" {
						if tok2, acc2, settings2, err2 := a.ensureCredsFor(ctx, nextID); err2 == nil {
							adoptCreds(acc.ID, acc2)
							label2 := acc2.Label
							if label2 == "" {
								label2 = acc2.Email
							}
							runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{
								Type: "content",
								Text: "\n\n_Auth negada — trocando para " + label2 + "…_\n\n",
							})
							err = a.upstream.StreamChat(ctx, tok2, settings2, label2, acc2.Email, req, emit)
							if err == nil {
								logging.Info("chat.ui.done", "provider", route.NormalizedProvider(), "model", req.Model, "account_id", acc.ID, "duration_ms", time.Since(chatT0).Milliseconds())
								return
							}
						}
					}
				}
			}
			if isQuotaExhaustedErr(err) {
				// Blocks on Kimi when re-login recreates the only usable account.
				a.markAccountExhausted(acc.ID, err.Error())
				nextID := a.pickNonExhausted(acc.ID)
				if nextID == "" {
					if after, ok := a.store.ActiveAccount(); ok && after != nil && after.Usable() {
						nextID = after.ID
					}
				}
				if nextID != "" {
					if tok2, acc2, settings2, err2 := a.ensureCredsFor(ctx, nextID); err2 == nil {
						adoptCreds(acc.ID, acc2)
						label2 := acc2.Label
						if label2 == "" {
							label2 = acc2.Email
						}
						runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{
							Type: "content",
							Text: "\n\n_Conta esgotada — trocando para " + label2 + "…_\n\n",
						})
						err = a.upstream.StreamChat(ctx, tok2, settings2, label2, acc2.Email, req, emit)
						if err == nil {
							return
						}
						if isQuotaExhaustedErr(err) {
							a.markAccountExhausted(acc2.ID, err.Error())
						}
					}
				}
			}
			logging.Error("chat.ui.error", "provider", route.NormalizedProvider(), "model", req.Model, "account_id", acc.ID, "err", err, "duration_ms", time.Since(chatT0).Milliseconds())
			runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "error", Error: err.Error()})
			runtime.EventsEmit(a.ctx, "chat:event", upstream.StreamEvent{Type: "done"})
			return
		}
		logging.Info("chat.ui.done", "provider", route.NormalizedProvider(), "model", req.Model, "account_id", acc.ID, "duration_ms", time.Since(chatT0).Milliseconds())
	}()
	return nil
}

func (a *App) setPhase(phase string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeReq == nil || a.activeReq.Phase == phase {
		return
	}
	a.activeReq.Phase = phase
	cp := *a.activeReq
	runtime.EventsEmit(a.ctx, "request:active", &cp)
}

// safeEmit never lets a Wails/webview emit failure take down the process mid-proxy request.
func (a *App) safeEmit(event string, data any) {
	if a == nil || a.ctx == nil {
		return
	}
	defer func() { _ = recover() }()
	runtime.EventsEmit(a.ctx, event, data)
}

func (a *App) ensureCreds(ctx context.Context) (string, *store.Account, store.Settings, error) {
	return a.ensureCredsInner(ctx, "", false)
}

// forceRefreshAccount re-syncs CLI tokens and forces OAuth refresh for the given account.
func (a *App) forceRefreshAccount(ctx context.Context, accountID string) (string, *store.Account, store.Settings, error) {
	return a.ensureCredsInner(ctx, accountID, true)
}

func (a *App) ensureCredsInner(ctx context.Context, preferID string, forceRefresh bool) (string, *store.Account, store.Settings, error) {
	if a.store == nil {
		return "", nil, store.Settings{}, fmt.Errorf("store not ready")
	}
	settings := a.store.Settings()
	// Request-scoped provider from HTTP multi-route (model → provider). Does not mutate store.
	if rp := proxyhttp.RouteProviderFrom(ctx); rp != "" {
		settings = settings.WithProvider(rp)
	}
	// Accio uses independent credential files but selects from the common pool,
	// allowing unlimited logged-in accounts and automatic failover.
	if settings.IsAccio() {
		if a.accio == nil {
			return "", nil, settings, fmt.Errorf("accio provider is not initialized")
		}
		strategy := a.store.GetLoadBalancerStrategy(store.ProviderAccio)
		var acc *store.Account
		if preferID != "" {
			if candidate, ok := a.store.GetAccount(preferID); ok && candidate != nil && candidate.NormalizedProvider() == store.ProviderAccio && candidate.Usable() {
				acc = candidate
			}
		}
		if acc == nil {
			acc = a.store.PickAccountForProvider(store.ProviderAccio, strategy)
		}
		if acc == nil {
			return "", nil, settings, fmt.Errorf("nenhuma conta Accio utilizável — faça login em Adicionar conta")
		}
		return acc.BearerToken(), acc, settings, nil
	}
	// QwenBridge: single local bridge account — base URL + API key from settings.
	// No fallback to another provider when the key is missing.
	if settings.IsQwen() {
		key := strings.TrimSpace(settings.QwenAPIKey)
		if key == "" {
			return "", nil, settings, fmt.Errorf("qwen: API key do QwenBridge não configurada — defina em Global (Qwen API key)")
		}
		acc := &store.Account{
			ID:          "qwen",
			Provider:    store.ProviderQwen,
			Label:       "QwenBridge",
			Email:       settings.EffectiveQwenUpstream(),
			AccessToken: key,
			APIKey:      key,
			ClientID:    "qwen",
			Issuer:      settings.EffectiveQwenUpstream(),
		}
		return key, acc, settings, nil
	}
	// DeepSeek: single official API account — encrypted key from settings.
	if settings.IsDeepSeek() {
		key := settings.DeepSeekAPIKeyPlain()
		if key == "" {
			return "", nil, settings, fmt.Errorf("deepseek: API key não configurada — selecione o provedor DeepSeek e cole a chave")
		}
		acc := &store.Account{
			ID:          "deepseek",
			Provider:    store.ProviderDeepSeek,
			Label:       "DeepSeek",
			Email:       store.DeepSeekUpstream,
			AccessToken: key,
			APIKey:      key,
			ClientID:    "deepseek",
			Issuer:      store.DeepSeekUpstream,
		}
		return key, acc, settings, nil
	}
	// OpenCode Zen Free is native/keyless: call Zen directly with its public
	// bearer token. No opencode CLI, `opencode serve`, terminal, or account is
	// required for either the desktop UI or the HTTP route.
	if settings.IsOpenCodeZen() {
		acc := &store.Account{
			ID:          "opencode-zen-free",
			Provider:    store.ProviderOpenCodeZen,
			Label:       "OpenCode Zen Free",
			Email:       "keyless@opencode.ai",
			AccessToken: store.OpenCodeZenAPIKey,
			ClientID:    "opencode-zen",
			Issuer:      store.OpenCodeZenUpstream,
		}
		return store.OpenCodeZenAPIKey, acc, settings, nil
	}
	// OllieChat is keyless — no xAI OAuth account required.
	if settings.IsOllie() {
		acc := &store.Account{
			ID:          "ollie",
			Provider:    store.ProviderOllie,
			Label:       "OllieChat",
			Email:       "keyless@olliechat",
			AccessToken: store.OllieAPIKey,
			ClientID:    "ollie",
			Issuer:      "https://olliechat.vercel.app",
		}
		return store.OllieAPIKey, acc, settings, nil
	}
	// Gemini uses Application Default Credentials (no xAI account).
	if settings.IsGemini() {
		acc := &store.Account{
			ID:          "gemini-adc",
			Provider:    store.ProviderGemini,
			Label:       "Gemini (ADC)",
			Email:       settings.EffectiveGeminiProject(),
			AccessToken: store.GeminiCredMarker,
			ClientID:    "google-adc",
			Issuer:      "https://accounts.google.com",
		}
		return store.GeminiCredMarker, acc, settings, nil
	}
	// Kimi Work: multi-account pool (1–3 accounts). Works with any count; no minimum.
	if settings.IsKimiWork() {
		strategy := a.store.GetLoadBalancerStrategy(store.ProviderKimiWork)
		var acc *store.Account
		if preferID != "" {
			if a2, ok2 := a.store.GetAccount(preferID); ok2 && a2 != nil && a2.NormalizedProvider() == store.ProviderKimiWork && a2.Usable() {
				acc = a2
			}
		}
		if acc == nil {
			acc = a.store.PickAccountForProvider(store.ProviderKimiWork, strategy)
		}
		if acc == nil {
			// No usable account — block request while auto re-login recovers any stored account.
			kimiAccounts := a.store.ListAccountsForProvider(store.ProviderKimiWork)
			if len(kimiAccounts) == 0 {
				return "", nil, settings, fmt.Errorf("nenhuma conta Kimi Work — adicione em Contas (máx. %d)", maxKimiWorkAccounts)
			}
			a.mu.Lock()
			reloginCount := a.kimiReloginCount
			a.mu.Unlock()
			if reloginCount > 0 {
				a.waitAutoKimiRelogin("", "ensureCreds: no usable account, waiting for in-flight re-login")
				acc = a.store.PickAccountForProvider(store.ProviderKimiWork, strategy)
			}
			if acc == nil {
				for _, ka := range kimiAccounts {
					log.Printf("ensureCreds: recovering Kimi account %s (exhausted=%v)", ka.ID, ka.Exhausted())
					a.waitAutoKimiRelogin(ka.ID, "ensureCreds: recovering "+ka.ID)
					acc = a.store.PickAccountForProvider(store.ProviderKimiWork, strategy)
					if acc != nil {
						break
					}
				}
			}
			if acc == nil {
				return "", nil, settings, fmt.Errorf("nenhuma conta Kimi Work disponível — re-login automático falhou; confira refresh tokens em Contas")
			}
		}
		// Partial pool death heal: a usable account was picked, but exhausted/
		// auth-denied rows with a Google refresh token must also be replenished —
		// otherwise the pool shrinks until only one account remains. Deduped and
		// queued inside startKimiRelogin, so this is cheap on every request.
		for _, row := range a.store.ListAccountsForProvider(store.ProviderKimiWork) {
			if !row.Exhausted() && !row.AuthDenied() {
				continue
			}
			if strings.TrimSpace(row.GoogleRefreshToken) == "" {
				continue
			}
			a.maybeAutoKimiRelogin(row.ID, "ensureCreds: partial pool heal")
		}
		a.store.IncAccountRequestCount(acc.ID)
		if forceRefresh {
			if err := a.remintKimiWorkKey(acc); err != nil {
				runtime.LogErrorf(a.ctx, "kimi remint: %v", err)
			} else if fresh, ok2 := a.store.GetAccount(acc.ID); ok2 && fresh != nil {
				acc = fresh
			}
		}
		tok := acc.BearerToken()
		if tok == "" {
			a.store.DecAccountRequestCount(acc.ID)
			return "", nil, settings, fmt.Errorf("conta Kimi sem sk-kimi")
		}
		return tok, acc, settings, nil
	}

	// Sync CLI tokens at most once per 30s — Codex fires many parallel /v1 calls.
	// forceRefresh always re-reads ~/.grok/auth.json: the official CLI rotates
	// refresh tokens and holds auth.json.lock; without a fresh pull we often hit
	// invalid_grant on a revoked RT while the CLI still has a valid one.
	a.lastCLISyncMu.Lock()
	doSync := forceRefresh || time.Since(a.lastCLISync) > 30*time.Second
	if doSync {
		a.lastCLISync = time.Now()
	}
	a.lastCLISyncMu.Unlock()
	if doSync {
		if n, err := a.store.SyncFromGrokCLI(); err == nil && n > 0 {
			_ = a.store.PreferHealthyActive()
			a.safeEmit("account:cli_synced", map[string]any{"updated": n})
		}
	}
	// Keep route override (xAI) after CLI sync reloads store settings.
	if rp := proxyhttp.RouteProviderFrom(ctx); rp != "" {
		settings = a.store.Settings().WithProvider(rp)
	} else {
		settings = a.store.Settings()
	}

	// xAI: multi-account pool with load balancing.
	strategy := a.store.GetLoadBalancerStrategy(store.ProviderXAI)
	var acc *store.Account
	if preferID != "" {
		if a2, ok2 := a.store.GetAccount(preferID); ok2 && a2 != nil && a2.NormalizedProvider() == store.ProviderXAI && a2.Usable() {
			acc = a2
		}
	}
	if acc == nil {
		acc = a.store.PickAccountForProvider(store.ProviderXAI, strategy)
	}
	if acc == nil {
		if raw, has := a.store.ActiveAccount(); has && raw != nil && raw.NormalizedProvider() == store.ProviderXAI && (raw.Exhausted() || raw.AuthDenied()) {
			a.maybeAutoCreate(firstNonEmpty(raw.ExhaustReason, raw.AuthDeniedReason))
			return "", nil, settings, fmt.Errorf("todas as contas esgotadas ou bloqueadas — adicione conta ou relogue")
		}
		return "", nil, settings, fmt.Errorf("nenhuma conta — adicione uma conta em Contas")
	}

	// Track in-flight requests for least-used strategy.
	a.store.IncAccountRequestCount(acc.ID)

	// After forced CLI sync, re-load — Sync may have replaced AT/RT for this id.
	if forceRefresh {
		if latest, ok := a.store.GetAccount(acc.ID); ok && latest != nil {
			*acc = *latest
		}
	}

	// Skip JWT bot-flagged tokens (chat endpoint returns 403 permission-denied).
	if oauth.BotFlagged(acc.AccessToken) {
		a.store.DecAccountRequestCount(acc.ID)
		_, _ = a.store.MarkAuthDenied(acc.ID, "bot_flag_source present in access token JWT")
		return "", nil, settings, fmt.Errorf("conta bloqueada (bot flag) — relogue ou use outra conta")
	}

	// forceRefresh still triggers OAuth (auth-denied recovery), but only after
	// SyncFromGrokCLI above so we hold the same RT the official CLI just wrote.
	needRefresh := forceRefresh || acc.ExpiresSoon(5*time.Minute) || acc.Expired() || acc.AccessToken == ""
	if needRefresh && acc.RefreshToken != "" {
		refreshedOK := false
		if err := a.refreshAccountLocked(ctx, acc); err != nil {
			// invalid_grant: RT revoked (CLI race, re-login elsewhere, or dead pool row).
			// Re-sync ~/.grok/auth.json once, then mark denied and rotate — never fail the
			// whole multi-account pool on one bad RT.
			if oauth.IsInvalidGrant(err) {
				logging.Warn("xai.refresh.invalid_grant", "account_id", acc.ID, "err", err.Error())
				if n, serr := a.store.SyncFromGrokCLI(); serr == nil && n > 0 {
					if latest, ok := a.store.GetAccount(acc.ID); ok && latest != nil {
						*acc = *latest
						if retryErr := a.refreshAccountLocked(ctx, acc); retryErr == nil {
							refreshedOK = true
						}
					}
				}
				if !refreshedOK {
					_, _ = a.store.MarkAuthDenied(acc.ID, "invalid_grant: "+err.Error())
					a.store.DecAccountRequestCount(acc.ID)
					if next := a.store.NextUsableAccountID(acc.ID); next != "" && next != acc.ID {
						logging.Info("xai.refresh.rotate_after_invalid_grant", "from", acc.ID, "to", next)
						return a.ensureCredsInner(ctx, next, false)
					}
					return "", nil, settings, fmt.Errorf("token expirado — faça login de novo: %v", err)
				}
			} else if forceRefresh || acc.Expired() {
				a.store.DecAccountRequestCount(acc.ID)
				if next := a.store.NextUsableAccountID(acc.ID); next != "" && next != acc.ID {
					logging.Info("xai.refresh.rotate_after_fail", "from", acc.ID, "to", next, "err", err.Error())
					return a.ensureCredsInner(ctx, next, false)
				}
				return "", nil, settings, fmt.Errorf("token expirado — faça login de novo: %v", err)
			}
			// Soft failure near expiry: keep current token only if not fully expired.
		} else {
			refreshedOK = true
		}
		if refreshedOK {
			// Re-check bot flag after refresh.
			if oauth.BotFlagged(acc.AccessToken) {
				a.store.DecAccountRequestCount(acc.ID)
				_, _ = a.store.MarkAuthDenied(acc.ID, "bot_flag_source present after refresh")
				return "", nil, settings, fmt.Errorf("conta bloqueada (bot flag) — relogue ou use outra conta")
			}
		}
	}
	if acc.AccessToken == "" {
		a.store.DecAccountRequestCount(acc.ID)
		return "", nil, settings, fmt.Errorf("conta sem access_token")
	}
	if acc.Expired() && !forceRefresh {
		a.store.DecAccountRequestCount(acc.ID)
		if next := a.store.NextUsableAccountID(acc.ID); next != "" && next != acc.ID {
			return a.ensureCredsInner(ctx, next, false)
		}
		return "", nil, settings, fmt.Errorf("token expirado — faça login de novo")
	}
	return acc.AccessToken, acc, settings, nil
}

func (a *App) accountRefreshMu(id string) *sync.Mutex {
	v, _ := a.refreshGates.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// refreshAccountLocked performs OAuth refresh with per-account mutex and persists tokens.
// On success it also mirrors tokens into ~/.grok/auth.json so the official CLI does not
// keep a revoked refresh_token after we rotate.
func (a *App) refreshAccountLocked(ctx context.Context, acc *store.Account) error {
	if acc == nil || acc.RefreshToken == "" {
		return fmt.Errorf("no refresh token")
	}
	mu := a.accountRefreshMu(acc.ID)
	mu.Lock()
	defer mu.Unlock()
	// Re-read after lock — another goroutine may have refreshed already.
	if latest, ok := a.store.GetAccount(acc.ID); ok && latest != nil {
		if !latest.ExpiresSoon(5*time.Minute) && latest.AccessToken != "" && latest.AccessToken != acc.AccessToken {
			*acc = *latest
			return nil
		}
		acc.RefreshToken = latest.RefreshToken
		acc.AccessToken = latest.AccessToken
		acc.ExpiresAt = latest.ExpiresAt
	}
	logging.Info("xai.refresh.start", "account_id", acc.ID)
	tok, err := a.oauth.Refresh(ctx, acc.RefreshToken, acc.ClientID, acc.Issuer)
	if err != nil {
		logging.Error("xai.refresh.failed", "account_id", acc.ID, "err", err.Error())
		return err
	}
	logging.Info("xai.refresh.ok", "account_id", acc.ID)
	acc.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		acc.RefreshToken = tok.RefreshToken
	}
	claims := oauth.ParseAccessClaims(tok.AccessToken)
	if !claims.Exp.IsZero() {
		acc.ExpiresAt = claims.Exp
	} else {
		acc.ExpiresAt = time.Now().UTC().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	acc.UpdatedAt = time.Now().UTC()
	// Successful refresh clears prior auth denial (quota left alone).
	acc.AuthDeniedAt = time.Time{}
	acc.AuthDeniedReason = ""
	if err := a.store.UpsertAccount(*acc); err != nil {
		return err
	}
	_ = a.store.ClearAuthDenied(acc.ID)
	// Keep official Grok CLI in lockstep (same OIDC client_id / rotating RT).
	if err := a.store.WriteAccountToGrokCLI(*acc); err != nil {
		logging.Warn("xai.refresh.cli_write_failed", "account_id", acc.ID, "err", err.Error())
	}
	return nil
}

func (a *App) ensureCredsFor(ctx context.Context, id string) (string, *store.Account, store.Settings, error) {
	return a.ensureCredsInner(ctx, id, false)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n := 0
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
}

func envDur(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func truncText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// StartAutoSignup creates a web account via chrome isolate + darkemail, then starts device OAuth.
func (a *App) StartAutoSignup() error {
	a.mu.Lock()
	if a.signupRunning {
		a.mu.Unlock()
		return fmt.Errorf("signup já em andamento")
	}
	a.signupRunning = true
	ctx, cancel := context.WithTimeout(a.ctx, 8*time.Minute)
	a.signupCancel = cancel
	a.mu.Unlock()

	go func() {
		defer func() {
			a.mu.Lock()
			a.signupRunning = false
			a.signupCancel = nil
			a.mu.Unlock()
		}()
		emit := func(msg string) {
			runtime.EventsEmit(a.ctx, "signup:progress", map[string]any{"message": msg})
		}
		emit("iniciando criação automática…")
		res, err := signup.RunFullCapture(ctx, "", "", "", "Alex", "Rivera", false, emit)
		if err != nil {
			runtime.EventsEmit(a.ctx, "signup:error", err.Error())
			return
		}
		email, password, userID := "", "", ""
		if res != nil {
			email = res.Email
			password = res.Password
			userID = signup.ExtractUserIDFromCookies(res.Cookies)
			if userID == "" {
				userID = signup.ExtractUserIDFromCookies(res.Credentials.Cookie)
			}
		}
		emit("conta web criada: " + email)
		href := ""
		if res != nil {
			href = res.FinalUI.Href
		}
		runtime.EventsEmit(a.ctx, "signup:web_ok", map[string]any{
			"email": email, "password": password, "user_id": userID, "href": href,
		})
		emit("iniciando device OAuth para tokens da API…")
		st, err := a.StartDeviceLogin()
		if err != nil {
			runtime.EventsEmit(a.ctx, "signup:error", "web ok, device login falhou: "+err.Error())
			return
		}
		runtime.EventsEmit(a.ctx, "signup:device", map[string]any{
			"email": email, "password": password,
			"user_code": st.UserCode, "verification_url": st.VerificationURL,
		})
		runtime.EventsEmit(a.ctx, "signup:done", map[string]any{
			"email": email, "password": password, "phase": "waiting_device_oauth",
		})
	}()
	return nil
}

func (a *App) CancelAutoSignup() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.signupCancel != nil {
		a.signupCancel()
		a.signupCancel = nil
	}
	a.signupRunning = false
}

func (a *App) IsSignupRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.signupRunning
}

func (a *App) SetAutoCreateOnExhausted(enabled bool) {
	a.mu.Lock()
	a.autoCreate = enabled
	a.mu.Unlock()
}

func (a *App) GetAutoCreateOnExhausted() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.autoCreate
}

func (a *App) GetKimiStealthHeadless() bool {
	if a.store == nil {
		return false
	}
	return a.store.Settings().KimiStealthHeadless
}

func (a *App) SetKimiStealthHeadless(enabled bool) {
	if a.store == nil {
		return
	}
	_ = a.store.UpdateSettings(func(s *store.Settings) {
		s.KimiStealthHeadless = enabled
	})
}

func (a *App) GetGoogleCredentials() (email, password string) {
	if a.store == nil {
		return "", ""
	}
	s := a.store.Settings()
	return s.GoogleEmail, s.GooglePassword
}

func (a *App) SetGoogleCredentials(email, password string) {
	if a.store == nil {
		return
	}
	_ = a.store.UpdateSettings(func(s *store.Settings) {
		s.GoogleEmail = strings.TrimSpace(email)
		s.GooglePassword = password
	})
}

// GetAccountGoogleCredentials returns per-account Google email/password for stealth re-login.
func (a *App) GetAccountGoogleCredentials(accountID string) (email, password string) {
	if a.store == nil || accountID == "" {
		return "", ""
	}
	acc, ok := a.store.GetAccount(accountID)
	if !ok || acc == nil {
		return "", ""
	}
	return acc.GoogleEmail, acc.GooglePassword
}

// SetAccountGoogleCredentials stores per-account Google email/password for stealth re-login.
func (a *App) SetAccountGoogleCredentials(accountID, email, password string) {
	if a.store == nil || accountID == "" {
		return
	}
	acc, ok := a.store.GetAccount(accountID)
	if !ok || acc == nil {
		return
	}
	acc.GoogleEmail = strings.TrimSpace(email)
	acc.GooglePassword = password
	acc.UpdatedAt = time.Now().UTC()
	_ = a.store.UpsertAccount(*acc)
}

func (a *App) markAccountExhausted(id, reason string) {
	acc, ok := a.store.GetAccount(id)
	if !ok || acc == nil {
		return
	}
	isKimi := acc.NormalizedProvider() == store.ProviderKimiWork
	// Kimi Work: logoff remote user on quota, keep local row exhausted, rotate or wait re-login.
	// Never fails the client solely because fewer than 3 accounts are registered.
	if isKimi {
		if a.tryKimiLogoffOnQuota(acc, reason) {
			return
		}
	}
	acc, err := a.store.MarkExhausted(id, reason)
	if err != nil || acc == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "account:exhausted", map[string]any{
		"id": id, "email": acc.Email, "reason": reason, "exhausted_at": acc.ExhaustedAt,
	})
	next := a.pickNonExhausted(id)
	if next != "" {
		_ = a.store.SetActiveAccount(next)
		runtime.EventsEmit(a.ctx, "account:rotated", map[string]any{"id": next, "reason": "quota"})
		if isKimi {
			// Replenish this account in background while request continues on another.
			a.maybeAutoKimiRelogin(id, reason+" (rotated; replenish)")
		}
		return
	}
	if isKimi {
		// No other usable account — block until re-login (HTTP or clean profile) succeeds.
		a.waitAutoKimiRelogin(id, reason)
		return
	}
	a.maybeAutoCreate(reason)
}

// remintKimiWorkKey refreshes the web JWT if needed and mints a fresh sk-kimi WORK key.
// If Kimi refresh fails but a Google refresh_token exists, recovers session via HTTP.
// Gated per account: Kimi/Google refresh tokens rotate server-side, so parallel
// remints of the same account would invalidate each other and poison the pool.
func (a *App) remintKimiWorkKey(acc *store.Account) error {
	if a.store == nil || acc == nil {
		return fmt.Errorf("no account")
	}
	mu := a.accountRefreshMu(acc.ID)
	mu.Lock()
	defer mu.Unlock()
	// Re-read after lock — another goroutine may have reminted already; also pick
	// up the freshest web session (refresh tokens rotate).
	if latest, ok := a.store.GetAccount(acc.ID); ok && latest != nil {
		if latest.APIKey != "" && latest.APIKey != acc.APIKey && latest.Usable() {
			*acc = *latest
			return nil
		}
		acc.APIKey = latest.APIKey
		acc.AccessToken = latest.AccessToken
		acc.RefreshToken = latest.RefreshToken
		acc.GoogleRefreshToken = latest.GoogleRefreshToken
		acc.ExpiresAt = latest.ExpiresAt
	}
	if !kimi.HasWebSession(acc.AccessToken, acc.RefreshToken) {
		// Recover via Google HTTP refresh when available.
		if gr := strings.TrimSpace(acc.GoogleRefreshToken); gr != "" {
			gl, err := kimi.LoginWithGoogleRefresh(gr)
			if err != nil {
				return fmt.Errorf("no web session and google http refresh failed: %w", err)
			}
			_, err = a.addKimiSession(gl.AccessToken, gl.RefreshToken, gl.GoogleRefreshToken, nil, "google_http_refresh", gl.Email, gl.Name, true)
			return err
		}
		return fmt.Errorf("no web session to remint sk-kimi")
	}
	access, refresh, err := kimi.EnsureAccessToken(acc.AccessToken, acc.RefreshToken)
	if err != nil {
		// Last resort: Google HTTP re-login.
		if gr := strings.TrimSpace(acc.GoogleRefreshToken); gr != "" {
			gl, gerr := kimi.LoginWithGoogleRefresh(gr)
			if gerr == nil && gl != nil {
				_, err2 := a.addKimiSession(gl.AccessToken, gl.RefreshToken, gl.GoogleRefreshToken, nil, "google_http_refresh", gl.Email, gl.Name, true)
				return err2
			}
		}
		return err
	}
	minted, err := kimi.MintWorkAPIKey(access, "grok-desktop-kimi")
	if err != nil {
		return err
	}
	acc.APIKey = minted.APIKey
	acc.AccessToken = access
	if refresh != "" {
		acc.RefreshToken = refresh
	}
	if minted.DeviceID != "" {
		acc.DeviceID = minted.DeviceID
	}
	if !minted.ExpiresAt.IsZero() {
		acc.ExpiresAt = minted.ExpiresAt
	}
	acc.AuthDeniedAt = time.Time{}
	acc.AuthDeniedReason = ""
	acc.ExhaustedAt = time.Time{}
	acc.ExhaustReason = ""
	acc.UpdatedAt = time.Now().UTC()
	if strings.HasPrefix(strings.ToLower(acc.Label), "auth-denied") || strings.HasPrefix(strings.ToLower(acc.Label), "esgotada") {
		if acc.Email != "" {
			acc.Label = acc.Email
		}
	}
	if err := a.store.UpsertAccount(*acc); err != nil {
		return err
	}
	_ = a.store.ClearAuthState(acc.ID)
	return nil
}

// tryKimiLogoffOnQuota permanently deletes the Kimi user when quota is exhausted.
// Local row stays exhausted so credentials (Google refresh) remain for auto re-login.
// Cleanup of obsolete rows happens after a successful re-login.
func (a *App) tryKimiLogoffOnQuota(acc *store.Account, reason string) bool {
	if acc == nil {
		return false
	}
	if !kimi.HasWebSession(acc.AccessToken, acc.RefreshToken) {
		log.Printf("kimi quota: account %s has no web session — mark exhausted only (paste sk-kimi?)", acc.ID)
		_, _ = a.store.MarkExhausted(acc.ID, reason)
		return false
	}
	_, err := kimi.LogoffWithSession(acc.AccessToken, acc.RefreshToken)
	if err != nil {
		log.Printf("kimi logoff failed for %s: %v — falling back to exhausted mark", acc.ID, err)
		runtime.EventsEmit(a.ctx, "account:logoff_failed", map[string]any{
			"id": acc.ID, "email": acc.Email, "reason": reason, "error": err.Error(),
		})
		_, _ = a.store.MarkExhausted(acc.ID, reason)
		return false
	}
	email, id := acc.Email, acc.ID
	if _, merr := a.store.MarkExhausted(id, reason+" (logoff OK, awaiting re-login)"); merr != nil {
		log.Printf("kimi logoff OK but mark exhausted failed: %v", merr)
	}
	log.Printf("kimi logoff OK — account deleted on kimi.com, kept locally as exhausted: %s (%s)", id, email)
	runtime.EventsEmit(a.ctx, "account:logoff", map[string]any{
		"id": id, "email": email, "reason": reason, "remote": true,
	})
	runtime.EventsEmit(a.ctx, "account:exhausted", map[string]any{
		"id": id, "email": email, "reason": reason, "logoff": true,
	})
	next := a.pickNonExhausted(id)
	if next != "" {
		_ = a.store.SetActiveAccount(next)
		runtime.EventsEmit(a.ctx, "account:rotated", map[string]any{"id": next, "reason": "quota_logoff"})
		a.maybeAutoKimiRelogin(id, reason+" (rotated; replenish)")
		return true
	}
	// No other usable account: wait for re-login (HTTP Google refresh, else clean Playwright profile).
	a.waitAutoKimiRelogin(id, reason)
	return true
}

// maybeAutoKimiRelogin starts Kimi re-login in background (HTTP refresh first, clean profile fallback).
func (a *App) maybeAutoKimiRelogin(accountID string, reason string) {
	a.startKimiRelogin(accountID, reason, nil)
}

// waitAutoKimiRelogin runs (or joins) Kimi re-login and blocks until a usable Kimi account exists.
func (a *App) waitAutoKimiRelogin(accountID string, reason string) bool {
	done := make(chan bool, 1)
	a.startKimiRelogin(accountID, reason, func(ok bool) {
		select {
		case done <- ok:
		default:
		}
	})
	timeout := 6 * time.Minute
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case ok := <-done:
		return ok
	case <-timer.C:
		log.Printf("kimi re-login wait timed out after %s (reason=%s)", timeout, reason)
		return false
	case <-a.ctx.Done():
		return false
	}
}

// startKimiRelogin runs Kimi re-login once: Google HTTP refresh when available,
// else Playwright with a clean isolated profile. Up to maxConcurrentKimiRelogins
// concurrent; excess requests are ENQUEUED per account (never silently dropped)
// and drained when a slot frees. Re-logins are deduped per account: a second
// request for an account already queued/running returns immediately (joiners
// with onDone are attached as waiters).
func (a *App) startKimiRelogin(accountID string, reason string, onDone func(bool)) {
	a.mu.Lock()
	if a.kimiReloginInFlight == nil {
		a.kimiReloginInFlight = map[string]bool{}
	}
	if a.kimiReloginWaiters == nil {
		a.kimiReloginWaiters = map[string][]func(bool){}
	}
	// Dedupe: same account already queued or running.
	if a.kimiReloginInFlight[accountID] {
		if onDone != nil {
			a.kimiReloginWaiters[accountID] = append(a.kimiReloginWaiters[accountID], onDone)
		}
		a.mu.Unlock()
		logging.Info("kimi.relogin.deduped", "account_id", accountID, "reason", reason)
		if onDone != nil {
			go a.pollKimiReloginJoin(accountID)
		}
		return
	}
	a.kimiReloginInFlight[accountID] = true
	if onDone != nil {
		a.kimiReloginWaiters[accountID] = append(a.kimiReloginWaiters[accountID], onDone)
	}
	if a.kimiReloginCount >= maxConcurrentKimiRelogins {
		// Capacity full — enqueue instead of dropping; drained by runKimiRelogin.
		a.kimiReloginQueue = append(a.kimiReloginQueue, kimiReloginRequest{accountID: accountID, reason: reason})
		a.mu.Unlock()
		logging.Info("kimi.relogin.enqueued", "account_id", accountID, "reason", reason, "queue_len", len(a.kimiReloginQueue))
		if onDone != nil {
			go a.pollKimiReloginJoin(accountID)
		}
		return
	}
	a.kimiReloginCount++
	a.mu.Unlock()
	go a.runKimiRelogin(accountID, reason)
}

// pollKimiReloginJoin watches a queued/deduped re-login for accountID: fires the
// registered waiters early when any account becomes usable; otherwise the waiter
// is fired when runKimiRelogin finishes this account's own re-login (started when
// a slot frees and the FIFO queue drains). Exits when waiters are gone.
func (a *App) pollKimiReloginJoin(accountID string) {
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		if a.ctx != nil {
			select {
			case <-a.ctx.Done():
				a.finishKimiReloginWaiters(accountID, false)
				return
			default:
			}
		}
		a.mu.Lock()
		waiting := len(a.kimiReloginWaiters[accountID]) > 0
		a.mu.Unlock()
		if !waiting {
			return // waiters fired by runKimiRelogin completion
		}
		if a.kimiUsableAnyReady() {
			a.finishKimiReloginWaiters(accountID, true)
			return
		}
		time.Sleep(400 * time.Millisecond)
	}
	a.finishKimiReloginWaiters(accountID, false)
}

// finishKimiReloginWaiters fires and clears all join-wait callbacks for accountID.
func (a *App) finishKimiReloginWaiters(accountID string, ok bool) {
	a.mu.Lock()
	ws := a.kimiReloginWaiters[accountID]
	delete(a.kimiReloginWaiters, accountID)
	a.mu.Unlock()
	for _, w := range ws {
		if w != nil {
			w(ok)
		}
	}
}

// runKimiRelogin performs one account's re-login (HTTP first, clean Playwright
// profile fallback), retrying up to 3 attempts with backoff before giving up.
// The dedupe entry is held until success or final failure. On completion the
// slot frees and the FIFO queue drains — the next queued account's re-login
// starts here instead of being dropped.
func (a *App) runKimiRelogin(accountID string, reason string) {
	ok := false
	defer func() {
		a.mu.Lock()
		a.kimiReloginCount--
		if a.kimiReloginCount < 0 {
			a.kimiReloginCount = 0
		}
		delete(a.kimiReloginInFlight, accountID)
		// Drain the queue while slots are free — never drop a queued replenish.
		for a.kimiReloginCount < maxConcurrentKimiRelogins && len(a.kimiReloginQueue) > 0 {
			next := a.kimiReloginQueue[0]
			a.kimiReloginQueue = a.kimiReloginQueue[1:]
			a.kimiReloginCount++
			logging.Info("kimi.relogin.dequeued", "account_id", next.accountID, "reason", next.reason)
			go a.runKimiRelogin(next.accountID, next.reason)
		}
		a.mu.Unlock()
		a.finishKimiReloginWaiters(accountID, ok)
	}()

	hasHTTP := false
	if t, _ := a.pickGoogleRefreshTokenFor(accountID); t != "" {
		hasHTTP = true
	}
	msg := "Cota esgotada — re-login Kimi (HTTP se possível, senão perfil limpo)…"
	if hasHTTP {
		msg = "Cota esgotada — re-login Kimi via HTTP (Google refresh)…"
	}
	logging.Info("kimi.relogin.start", "account_id", accountID, "reason", reason, "http_first", hasHTTP)
	runtime.EventsEmit(a.ctx, "kimi:relogin", map[string]any{
		"phase":   "start",
		"reason":  reason,
		"message": msg,
		"http":    hasHTTP,
	})

	// Retry up to 3 attempts with backoff (5s / 15s / 30s between tries) —
	// transient Google/Kimi HTTP blips must not kill the replenish loop.
	backoffs := []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}
	var rec map[string]any
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			wait := backoffs[attempt-1]
			log.Printf("kimi re-login retry %d/3 for %s in %s: %v", attempt+1, accountID, wait, err)
			if a.ctx != nil {
				select {
				case <-a.ctx.Done():
					return
				case <-time.After(wait):
				}
			} else {
				time.Sleep(wait)
			}
		}
		// HTTP first; falls back to clean Playwright profile. background=true:
		// never flip global provider/active account from a replenish loop.
		rec, err = a.startKimiStealthLoginForAccount(accountID, true, true)
		if err == nil && rec != nil {
			break
		}
	}
	if err != nil || rec == nil {
		if err == nil {
			err = fmt.Errorf("re-login returned no account")
		}
		logging.Error("kimi.relogin.failed", "account_id", accountID, "reason", reason, "err", err.Error())
		runtime.EventsEmit(a.ctx, "kimi:relogin", map[string]any{
			"phase":   "error",
			"reason":  reason,
			"error":   err.Error(),
			"message": "Falha no re-login automático Kimi: " + err.Error(),
		})
		return
	}
	ok = a.kimiUsableAnyReady()
	mode, _ := rec["mode"].(string)
	if mode == "" {
		mode = "unknown"
	}
	logging.Info("kimi.relogin.ok", "account_id", accountID, "mode", mode, "usable", ok)
	doneMsg := "Conta Kimi renovada"
	if mode == "http_refresh" {
		doneMsg = "Conta Kimi renovada via HTTP"
	} else if mode == "playwright" || mode == "playwright_new" {
		doneMsg = "Conta Kimi renovada via perfil limpo"
	}
	// Remove obsolete exhausted row if re-login created a different account id.
	if accountID != "" {
		if newID, _ := rec["id"].(string); newID != "" && newID != accountID {
			if old, ok2 := a.store.GetAccount(accountID); ok2 && old != nil && old.Exhausted() {
				_ = a.store.RemoveAccount(accountID)
				log.Printf("kimi re-login cleanup: removed old exhausted account %s", accountID)
			}
		}
	}
	runtime.EventsEmit(a.ctx, "kimi:relogin", map[string]any{
		"phase":   "ok",
		"reason":  reason,
		"account": rec,
		"mode":    mode,
		"message": doneMsg,
	})
	runtime.EventsEmit(a.ctx, "accounts:changed", map[string]any{"source": "kimi_relogin", "mode": mode})
	if id, _ := rec["id"].(string); id != "" {
		runtime.EventsEmit(a.ctx, "account:rotated", map[string]any{"id": id, "reason": "kimi_relogin", "mode": mode})
	}
}

// kimiUsableAnyReady is true if any Kimi Work account in the pool is usable.
// Side-effect-free: pollers call this every 400ms, so it must not advance the
// shared round-robin cursor (PickAccountForProvider would).
func (a *App) kimiUsableAnyReady() bool {
	if a.store == nil {
		return false
	}
	return a.store.HasUsableAccountForProvider(store.ProviderKimiWork)
}

// countKimiAccountsTowardCap counts accounts that occupy a registration slot
// (not exhausted). Exhausted rows await re-login and do not block refill.
func (a *App) countKimiAccountsTowardCap() int {
	if a.store == nil {
		return 0
	}
	n := 0
	for _, row := range a.store.ListAccountsForProvider(store.ProviderKimiWork) {
		if row.Exhausted() {
			continue
		}
		n++
	}
	return n
}

func (a *App) markAccountAuthDenied(id, reason string) {
	// Never poison a Kimi sk-kimi account with an xAI-shaped auth error (wrong upstream).
	low := strings.ToLower(reason)
	if acc, ok := a.store.GetAccount(id); ok && acc != nil && acc.NormalizedProvider() == store.ProviderKimiWork {
		if strings.Contains(low, "x_xai_token_auth") ||
			(strings.Contains(low, "no auth context") && strings.Contains(low, "auth_kind=bearer")) {
			return
		}
		// Invalid sk-kimi: try remint from web JWT before permanently denying.
		if strings.Contains(low, "invalid_authentication") ||
			strings.Contains(low, "api key appears to be invalid") ||
			strings.Contains(low, "may have expired") {
			if err := a.remintKimiWorkKey(acc); err == nil {
				runtime.LogInfof(a.ctx, "kimi: reminted sk-kimi after auth failure for %s", id)
				return
			}
		}
	}
	acc, err := a.store.MarkAuthDenied(id, reason)
	if err != nil || acc == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "account:auth_denied", map[string]any{
		"id": id, "email": acc.Email, "reason": reason, "auth_denied_at": acc.AuthDeniedAt,
	})
	next := a.pickNonExhausted(id)
	if next != "" {
		_ = a.store.SetActiveAccount(next)
		runtime.EventsEmit(a.ctx, "account:rotated", map[string]any{"id": next, "reason": "auth_denied"})
		return
	}
	// Do not auto-create on auth denial by default — user should re-login the real account.
}

func (a *App) maybeAutoCreate(reason string) {
	a.mu.Lock()
	auto := a.autoCreate
	running := a.signupRunning
	a.mu.Unlock()
	if auto && !running {
		runtime.EventsEmit(a.ctx, "signup:auto_triggered", map[string]any{"reason": reason})
		_ = a.StartAutoSignup()
	}
}

func (a *App) pickNonExhausted(except string) string {
	if a.store == nil {
		return ""
	}
	return a.store.NextUsableAccountID(except)
}

func isQuotaExhaustedErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "usage balance exhausted") || strings.Contains(s, "balance exhausted") {
		return true
	}
	if strings.Contains(s, "http 402") || (strings.Contains(s, "http 403") && (strings.Contains(s, "quota") || strings.Contains(s, "credit") || strings.Contains(s, "diamond") || strings.Contains(s, "balance"))) {
		return true
	}
	// Kimi Work plan / billing cycle cap
	kimiQuota := []string{
		"usage limit", "billing cycle", "resource_exhausted", "access_terminated",
		"upgrade to get more", "quota exceeded", "quota limit", "credit exhausted",
		"insufficient quota", "no remaining quota", "daily limit", "monthly limit",
	}
	for _, p := range kimiQuota {
		if strings.Contains(s, p) {
			return true
		}
	}
	if strings.Contains(s, "http 429") && (strings.Contains(s, "exhausted") || strings.Contains(s, "quota") || strings.Contains(s, "balance") || strings.Contains(s, "rate limit")) {
		return true
	}
	// Some gateways only return the message without status in the error string.
	if strings.Contains(s, "quota") && (strings.Contains(s, "exceed") || strings.Contains(s, "exhaust")) {
		return true
	}
	return false
}

func isAuthDeniedErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	// Prefer quota classification for Kimi billing 403s.
	if isQuotaExhaustedErr(err) {
		return false
	}
	if strings.Contains(s, "permission-denied") || strings.Contains(s, "access to the chat endpoint is denied") {
		return true
	}
	if strings.Contains(s, "http 403") || strings.Contains(s, "status 403") {
		return true
	}
	if strings.Contains(s, "http 401") || strings.Contains(s, "invalid or expired credentials") {
		return true
	}
	if strings.Contains(s, "no auth context") {
		return true
	}
	return false
}

// SyncGrokCLI is exposed to the UI / selftest to pull tokens from ~/.grok/auth.json.
func (a *App) SyncGrokCLI() map[string]any {
	if a.store == nil {
		return map[string]any{"ok": false, "error": "store not ready"}
	}
	n, err := a.store.SyncFromGrokCLI()
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	switched := a.store.PreferHealthyActive()
	acc, _ := a.store.ActiveAccount()
	email := ""
	id := ""
	if acc != nil {
		email = acc.Email
		id = acc.ID
	}
	return map[string]any{
		"ok": true, "updated": n, "switched_active": switched,
		"active_id": id, "active_email": email,
		"accounts": a.store.PublicAccounts(),
	}
}

func (a *App) injectAgentContext(msgs []upstream.ChatMessage) []upstream.ChatMessage {
	var blocks []string
	if a.skills != nil {
		if cat := a.skills.CatalogPrompt(); cat != "" {
			blocks = append(blocks, cat)
		}
	}
	if a.mcp != nil {
		if cat := a.mcp.CatalogPrompt(); cat != "" {
			blocks = append(blocks, cat)
		}
	}
	blocks = append(blocks, `Skills authoring: when the user asks you to create or update a skill, call the app capability by describing it clearly as:
CREATE_SKILL:
name: <id-or-title>
description: <one line>
---
<body markdown>
The desktop app can persist skills under AppData/skills. MCP servers with API keys (e.g. Context7) are configured in Settings → MCP with environment secrets.`)
	if len(blocks) == 0 {
		return msgs
	}
	extra := ""
	for i, b := range blocks {
		if i > 0 {
			extra += "\n\n"
		}
		extra += b
	}
	if len(msgs) > 0 && msgs[0].Role == "system" {
		if !containsStr(msgs[0].Content, "Available local skills") && !containsStr(msgs[0].Content, "CREATE_SKILL:") {
			msgs[0].Content = msgs[0].Content + "\n\n" + extra
		}
		return msgs
	}
	return append([]upstream.ChatMessage{{Role: "system", Content: extra}}, msgs...)
}

func containsStr(s, sub string) bool {
	return strings.Contains(s, sub)
}

// ---------- Skills ----------

func (a *App) ListSkills() ([]skills.Skill, error) {
	if a.skills == nil {
		return nil, fmt.Errorf("skills store not ready")
	}
	return a.skills.List()
}

func (a *App) GetSkill(id string) (*skills.Skill, error) {
	if a.skills == nil {
		return nil, fmt.Errorf("skills store not ready")
	}
	return a.skills.Get(id)
}

func (a *App) CreateSkill(name, description, body string) (*skills.Skill, error) {
	if a.skills == nil {
		return nil, fmt.Errorf("skills store not ready")
	}
	sk, err := a.skills.Create(name, description, body)
	if err != nil {
		return nil, err
	}
	return sk, nil
}

func (a *App) UpdateSkill(id, name, description, body string) (*skills.Skill, error) {
	if a.skills == nil {
		return nil, fmt.Errorf("skills store not ready")
	}
	return a.skills.Update(id, name, description, body)
}

func (a *App) DeleteSkill(id string) error {
	if a.skills == nil {
		return fmt.Errorf("skills store not ready")
	}
	return a.skills.Delete(id)
}

// ---------- MCP servers (with API key / SK support) ----------

func (a *App) ListMCPServers() []map[string]any {
	if a.mcp == nil {
		return nil
	}
	return a.mcp.List(true)
}

func (a *App) UpsertMCPServer(cfg map[string]any) (map[string]any, error) {
	if a.mcp == nil {
		return nil, fmt.Errorf("mcp store not ready")
	}
	sv := mcpconfig.Server{
		ID:      strMap(cfg, "id"),
		Name:    strMap(cfg, "name"),
		Type:    strMap(cfg, "type"),
		URL:     strMap(cfg, "url"),
		Enabled: true,
	}
	if v, ok := cfg["enabled"].(bool); ok {
		sv.Enabled = v
	}
	if v, ok := cfg["timeout_ms"].(float64); ok {
		sv.TimeoutMs = int(v)
	}
	if cmd, ok := cfg["command"].([]any); ok {
		for _, c := range cmd {
			if s, ok := c.(string); ok {
				sv.Command = append(sv.Command, s)
			}
		}
	}
	if env, ok := cfg["environment"].(map[string]any); ok {
		sv.Environment = map[string]string{}
		for k, v := range env {
			if s, ok := v.(string); ok {
				sv.Environment[k] = s
			}
		}
	}
	if hdr, ok := cfg["headers"].(map[string]any); ok {
		sv.Headers = map[string]string{}
		for k, v := range hdr {
			if s, ok := v.(string); ok {
				sv.Headers[k] = s
			}
		}
	}
	if sv.ID == "" {
		return nil, fmt.Errorf("id required")
	}
	if err := a.mcp.Upsert(sv); err != nil {
		return nil, err
	}
	got, _ := a.mcp.Get(sv.ID)
	return got.Public(true), nil
}

func (a *App) DeleteMCPServer(id string) error {
	if a.mcp == nil {
		return fmt.Errorf("mcp store not ready")
	}
	return a.mcp.Delete(id)
}

func strMap(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}
