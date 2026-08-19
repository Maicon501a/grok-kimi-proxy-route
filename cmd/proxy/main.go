// Headless OpenAI-compatible proxy (same AppData store as Grok Desktop).
// Use this when you want the proxy to stay up without the GUI window.
//
//	go run ./cmd/proxy
//	go build -o grok-proxy.exe ./cmd/proxy
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"grok-desktop/internal/accio"
	"grok-desktop/internal/codexauth"
	"grok-desktop/internal/kimi"
	"grok-desktop/internal/logging"
	"grok-desktop/internal/oauth"
	"grok-desktop/internal/proxyhttp"
	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
)

// Per-account refresh serialization (refresh-token rotation is not concurrent-safe:
// parallel refreshes of the same account invalidate all but one and poison the pool).
var refreshGates sync.Map // accountID → *sync.Mutex

func accountRefreshMu(id string) *sync.Mutex {
	v, _ := refreshGates.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// replenishInFlight dedupes background Kimi replenish goroutines per account.
var replenishInFlight sync.Map // accountID → struct{}

// Throttle CLI auth sync so parallel request storms don't hammer disk every turn.
var (
	lastCLISync   time.Time
	lastCLISyncMu sync.Mutex
	headlessAccio *accio.Client
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	logging.SetOutput(os.Stdout)
	logging.SetLevel(logging.ParseLevel(os.Getenv("GROK_LOG_LEVEL")))
	st, err := store.Open("")
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	oa := oauth.New()
	ca := codexauth.New()
	if v := st.Settings().ClientVersion; v != "" {
		oa.CLIVersion = v
	}
	up := upstream.New()
	// The GUI stores Accio's account pool in the dedicated accio subdirectory.
	// Keep the headless proxy on the same source of truth instead of looking for
	// a legacy root-level token.json and silently falling back to Nexus.
	ac, err := accio.New(filepath.Join(st.Root(), "accio"))
	if err != nil {
		log.Printf("accio: disabled: %v", err)
	} else {
		headlessAccio = ac
	}

	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		return ensureCreds(ctx, st, oa, ca, "", false)
	}
	forceRefresh := func(ctx context.Context, id string) (string, *store.Account, store.Settings, error) {
		return ensureCreds(ctx, st, oa, ca, id, true)
	}

	// autoReloginKimi re-registers an exhausted Kimi account via HTTP-only Google
	// refresh (logoff→re-register gives a fresh quota window) so the proxy does
	// not return quota errors to the client while recovery is in progress.
	// Gated per account: Google/Kimi refresh tokens rotate server-side, so parallel
	// re-logins of the same account would invalidate each other and poison the pool.
	// The caller MUST mark the account exhausted/denied before calling: the
	// early-return only fires when the row was dead on arrival and is usable now
	// (healed by another goroutine while we waited for the gate).
	autoReloginKimi := func(accountID, reason string) bool {
		mu := accountRefreshMu(accountID)
		mu.Lock()
		defer mu.Unlock()
		acc, ok := st.GetAccount(accountID)
		if !ok || acc == nil {
			return false
		}
		wasDead := acc.Exhausted() || acc.AuthDenied()
		if acc.Usable() && acc.BearerToken() != "" {
			// Healed by another goroutine while we waited. Only counts as
			// recovery when it was dead on arrival — never a no-op true.
			return wasDead
		}
		return reloginKimiViaGoogleHTTP(st, acc)
	}

	srv := proxyhttp.New(st, up, ensure)
	srv.SetAccio(ac)
	srv.SetForceRefresh(forceRefresh)
	srv.SetQuotaHandler(func(accountID, reason string) bool {
		acc, ok := st.GetAccount(accountID)
		if !ok || acc == nil || acc.NormalizedProvider() != store.ProviderKimiWork {
			_, _ = st.MarkExhausted(accountID, reason)
			if next := st.NextUsableAccountID(accountID); next != "" {
				_ = st.SetActiveAccount(next)
				return true
			}
			return false
		}
		// 1. Mark FIRST — concurrent requests must rotate away immediately, and
		//    autoReloginKimi's early-return semantics depend on the mark.
		_, _ = st.MarkExhausted(accountID, reason)
		// 2. Logoff (remote user delete) BEFORE re-login: quota only resets for a
		//    freshly re-registered user. Local row stays exhausted so the Google
		//    refresh token remains for the re-login below.
		if kimi.HasWebSession(acc.AccessToken, acc.RefreshToken) {
			if _, err := kimi.LogoffWithSession(acc.AccessToken, acc.RefreshToken); err != nil {
				logging.Warn("kimi.logoff.failed", "account_id", accountID, "err", err.Error())
			} else {
				logging.Info("kimi.logoff.ok", "account_id", accountID, "email", acc.Email)
			}
		} else {
			logging.Warn("kimi.quota.no_web_session", "account_id", accountID)
		}
		// 3. Rotate to another usable account; replenish this one in background.
		//    Deduped per account: a quota storm must not spawn unbounded goroutines
		//    all refreshing the same rotating refresh token.
		if next := st.NextUsableAccountID(accountID); next != "" {
			_ = st.SetActiveAccount(next)
			if _, loaded := replenishInFlight.LoadOrStore(accountID, struct{}{}); !loaded {
				go func(id string) {
					defer replenishInFlight.Delete(id)
					if autoReloginKimi(id, reason+" (rotated; replenish)") {
						logging.Info("kimi.replenish.ok", "account_id", id)
					}
				}(accountID)
			}
			return true
		}
		// 4. No other usable account — recover ANY Kimi account via HTTP (blocks).
		for _, a := range st.ListAccounts() {
			if a.NormalizedProvider() != store.ProviderKimiWork {
				continue
			}
			if a.Usable() && a.BearerToken() != "" {
				return true
			}
			if autoReloginKimi(a.ID, reason) {
				return true
			}
		}
		// Final peek: a background replenish may have swapped rows (new user id)
		// while we waited on per-account gates above.
		return st.HasUsableAccountForProvider(store.ProviderKimiWork)
	})
	srv.SetAuthFailHandler(func(accountID, reason string) bool {
		_, _ = st.MarkAuthDenied(accountID, reason)
		if next := st.NextUsableAccountID(accountID); next != "" {
			_ = st.SetActiveAccount(next)
			return true
		}
		return false
	})

	settings := st.Settings()
	listen := settings.ProxyListen
	if listen == "" {
		listen = "127.0.0.1:8787"
	}
	if err := srv.Start(listen); err != nil {
		listen = "127.0.0.1:8788"
		if err2 := srv.Start(listen); err2 != nil {
			log.Fatalf("listen: %v / fallback: %v", err, err2)
		}
	}
	logging.Info("proxy.startup",
		"addr", srv.Addr(), "provider", settings.NormalizedProvider(),
		"model", settings.ResolveModel("default"), "store", st.Root(),
		"providers", "xai,openai_codex,kimi_work,ollie,gemini,qwen,deepseek,opencode_zen,opencode_go,accio")
	log.Printf("Ctrl+C to stop")

	janitorCtx, stopJanitor := context.WithCancel(context.Background())
	defer stopJanitor()
	startTokenJanitor(janitorCtx, st, oa)

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	stopJanitor()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Stop(ctx)
	accio.ShutdownSGDaemon()
	fmt.Println("stopped")
}

// startTokenJanitor keeps xAI access tokens perpetually fresh while the proxy
// is running, mirroring the official Grok CLI (refresh ~5 min before expiry).
// Without it, access tokens (6h TTL) lapse whenever no traffic arrives — e.g.
// overnight — and the whole pool looks dead even though every refresh token is
// still valid. Refresh tokens themselves die after prolonged idleness (~30d),
// so keeping the chain warm also protects accounts from permanent invalid_grant.
func startTokenJanitor(ctx context.Context, st *store.Store, oa *oauth.Client) {
	// Per-account backoff after transient refresh failures (network, 5xx):
	// a minute-tick hammering a broken upstream looks abusive.
	backoff := map[string]time.Time{}
	tick := func() {
		// Adopt tokens the official CLI refreshed while we were idle before
		// spending our own (rotating) refresh tokens.
		_, _ = st.SyncFromGrokCLI()
		for _, acc := range st.ListAccountsForProvider(store.ProviderXAI) {
			if ctx.Err() != nil {
				return
			}
			if !acc.Usable() || strings.TrimSpace(acc.RefreshToken) == "" {
				continue
			}
			if !acc.Expired() && !acc.ExpiresSoon(5*time.Minute) {
				continue
			}
			if retry, ok := backoff[acc.ID]; ok && time.Now().Before(retry) {
				continue
			}
			cp := acc
			if err := refreshXAIAccount(ctx, st, oa, &cp, false); err != nil {
				if oauth.IsInvalidGrant(err) {
					// Dead RT: stop retrying every tick.
					delete(backoff, acc.ID)
					_, _ = st.MarkAuthDenied(acc.ID, "invalid_grant: "+err.Error())
					logging.Warn("xai.janitor.invalid_grant", "account_id", acc.ID, "err", err.Error())
				} else {
					backoff[acc.ID] = time.Now().Add(10 * time.Minute)
					logging.Warn("xai.janitor.refresh_failed", "account_id", acc.ID, "err", err.Error())
				}
			} else {
				delete(backoff, acc.ID)
				logging.Info("xai.janitor.refreshed", "account_id", acc.ID)
			}
			// Stagger so a freshly-created pool does not burst auth.x.ai.
			select {
			case <-ctx.Done():
				return
			case <-time.After(1500 * time.Millisecond):
			}
		}
	}
	go func() {
		// First pass soon after launch: heal any pool that lapsed while the
		// proxy was down (access tokens only live 6h).
		select {
		case <-ctx.Done():
			return
		case <-time.After(8 * time.Second):
		}
		tick()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				tick()
			}
		}
	}()
}

// reloginKimiViaGoogleHTTP re-registers a Kimi user over HTTP (Google refresh →
// Kimi login → mint sk-kimi). After logoff the re-login may create a NEW Kimi
// user id — the fresh session is written under the NEW row id and the old row is
// removed (mirrors the app's addKimiSession + cleanup pattern); the old row's
// identity is never overwritten with another user's credentials. A plain remint
// into the same exhausted user is NOT a recovery (quota is per-user).
// Caller must hold accountRefreshMu(acc.ID).
func reloginKimiViaGoogleHTTP(st *store.Store, acc *store.Account) bool {
	if acc == nil {
		return false
	}
	gr := strings.TrimSpace(acc.GoogleRefreshToken)
	if gr == "" {
		log.Printf("proxy: %s has no Google refresh token — cannot re-login over HTTP", acc.ID)
		return false
	}
	sess, err := kimi.LoginWithGoogleRefresh(gr)
	if err != nil || sess == nil {
		log.Printf("proxy: HTTP Google refresh failed for %s: %v", acc.ID, err)
		return false
	}
	minted, err := kimi.MintWorkAPIKey(sess.AccessToken, "grok-desktop-proxy")
	if err != nil || minted == nil || minted.APIKey == "" {
		log.Printf("proxy: mint after HTTP re-login failed for %s: %v", acc.ID, err)
		return false
	}
	newID := "kimi-" + minted.UserID
	if minted.UserID == "" {
		newID = "kimi-" + minted.APIKey[len(minted.APIKey)-8:]
	}
	now := time.Now().UTC()
	email := sess.Email
	if email == "" {
		email = acc.Email
	}
	label := acc.Label
	if low := strings.ToLower(label); label == "" || strings.HasPrefix(low, "esgotada") || strings.HasPrefix(low, "auth-denied") {
		label = email
	}
	googleRT := sess.GoogleRefreshToken
	if googleRT == "" {
		googleRT = acc.GoogleRefreshToken
	}
	next := store.Account{
		ID:                 newID,
		Provider:           store.ProviderKimiWork,
		Label:              label,
		Email:              email,
		UserID:             minted.UserID,
		APIKey:             minted.APIKey,
		DeviceID:           minted.DeviceID,
		Source:             "google_http_refresh",
		ClientID:           "kimi-work",
		Issuer:             "https://www.kimi.com",
		AccessToken:        sess.AccessToken,
		RefreshToken:       sess.RefreshToken,
		GoogleRefreshToken: googleRT,
		GoogleEmail:        acc.GoogleEmail,
		GooglePassword:     acc.GooglePassword,
		ExpiresAt:          minted.ExpiresAt,
		CreatedAt:          acc.CreatedAt,
		UpdatedAt:          now,
	}
	if next.CreatedAt.IsZero() {
		next.CreatedAt = now
	}
	if err := st.UpsertAccount(next); err != nil {
		logging.Error("kimi.relogin.persist_failed", "account_id", newID, "err", err.Error())
		return false
	}
	if newID != acc.ID {
		_ = st.RemoveAccount(acc.ID)
		logging.Info("kimi.relogin.new_user", "new_id", newID, "old_id", acc.ID, "email", email)
	} else {
		logging.Info("kimi.relogin.ok", "account_id", acc.ID, "email", email)
	}
	return true
}

func ensureAccioCreds(ctx context.Context, settings store.Settings) (string, *store.Account, store.Settings, error) {
	if headlessAccio == nil {
		return "", nil, settings, fmt.Errorf("accio: cliente não inicializado")
	}
	tok, err := headlessAccio.CurrentToken()
	if err != nil {
		return "", nil, settings, fmt.Errorf("accio: faça login no app Wails ou importe token.json")
	}
	return tok, &store.Account{ID: "accio", Provider: store.ProviderAccio, Label: "Accio", Email: "Accio/Phoenix", AccessToken: tok}, settings, nil
}

func ensureCreds(
	ctx context.Context,
	st *store.Store,
	oa *oauth.Client,
	ca *codexauth.Client,
	preferID string,
	forceRefresh bool,
) (string, *store.Account, store.Settings, error) {
	settings := st.Settings()
	if rp := proxyhttp.RouteProviderFrom(ctx); rp != "" {
		settings = settings.WithProvider(rp)
	}
	if settings.IsAccio() {
		return ensureAccioCreds(ctx, settings)
	}
	if settings.IsQwen() {
		key := strings.TrimSpace(settings.QwenAPIKey)
		if key == "" {
			return "", nil, settings, fmt.Errorf("qwen: API key do QwenBridge não configurada — defina qwen_api_key nas settings")
		}
		acc := &store.Account{
			ID: "qwen", Provider: store.ProviderQwen, Label: "QwenBridge",
			Email:       settings.EffectiveQwenUpstream(),
			AccessToken: key,
			APIKey:      key,
		}
		return key, acc, settings, nil
	}
	if settings.IsDeepSeek() {
		key := settings.DeepSeekAPIKeyPlain()
		if key == "" {
			return "", nil, settings, fmt.Errorf("deepseek: API key não configurada — configure no app desktop")
		}
		acc := &store.Account{
			ID: "deepseek", Provider: store.ProviderDeepSeek, Label: "DeepSeek",
			Email:       store.DeepSeekUpstream,
			AccessToken: key,
			APIKey:      key,
		}
		return key, acc, settings, nil
	}
	if settings.IsOpenCodeZen() {
		acc := &store.Account{
			ID: "opencode-zen-free", Provider: store.ProviderOpenCodeZen,
			Label: "OpenCode Zen Free", Email: "keyless@opencode.ai",
			AccessToken: store.OpenCodeZenAPIKey,
		}
		return store.OpenCodeZenAPIKey, acc, settings, nil
	}
	if settings.IsOpenCodeGo() {
		key := settings.OpenCodeGoAPIKeyPlain()
		if key == "" {
			return "", nil, settings, fmt.Errorf("opencode go: API key não configurada — configure no app desktop")
		}
		acc := &store.Account{
			ID: "opencode-go", Provider: store.ProviderOpenCodeGo, Label: "OpenCode Go",
			Email: store.OpenCodeGoUpstream, AccessToken: key, APIKey: key,
		}
		return key, acc, settings, nil
	}
	if settings.IsOllie() {
		acc := &store.Account{
			ID: "ollie", Label: "OllieChat", Email: "keyless@olliechat",
			AccessToken: store.OllieAPIKey,
		}
		return store.OllieAPIKey, acc, settings, nil
	}
	if settings.IsGemini() {
		acc := &store.Account{
			ID:          "gemini-adc",
			Label:       "Gemini (ADC)",
			Email:       settings.EffectiveGeminiProject(),
			AccessToken: store.GeminiCredMarker,
		}
		return store.GeminiCredMarker, acc, settings, nil
	}
	if settings.IsCodex() {
		var acc *store.Account
		if preferID != "" {
			if candidate, ok := st.GetAccount(preferID); ok && candidate != nil && candidate.NormalizedProvider() == store.ProviderCodex && candidate.Usable() {
				acc = candidate
			}
		}
		if acc == nil {
			acc = st.PickAccountForProvider(store.ProviderCodex, st.GetLoadBalancerStrategy(store.ProviderCodex))
		}
		if acc == nil {
			return "", nil, settings, fmt.Errorf("nenhuma conta Codex - faça login no app desktop")
		}
		st.IncAccountRequestCount(acc.ID)
		if forceRefresh || acc.ExpiresSoon(5*time.Minute) || acc.Expired() || acc.AccessToken == "" {
			if err := refreshCodexAccount(ctx, st, ca, acc, forceRefresh); err != nil {
				if codexauth.IsInvalidGrant(err) {
					_, _ = st.MarkAuthDenied(acc.ID, "codex refresh: "+err.Error())
					st.DecAccountRequestCount(acc.ID)
					if next := st.NextUsableAccountID(acc.ID); next != "" {
						return ensureCreds(ctx, st, oa, ca, next, false)
					}
					return "", nil, settings, fmt.Errorf("sessão Codex expirada: %w", err)
				}
				if forceRefresh || acc.Expired() {
					st.DecAccountRequestCount(acc.ID)
					return "", nil, settings, fmt.Errorf("refresh Codex temporariamente indisponível: %w", err)
				}
			}
		}
		if acc.AccessToken == "" || acc.Expired() {
			st.DecAccountRequestCount(acc.ID)
			return "", nil, settings, fmt.Errorf("sessão Codex expirada - faça login novamente")
		}
		settings.CodexAccountID = acc.TeamID
		settings.CodexFedRAMP = strings.Contains(acc.Source, "fedramp")
		return acc.AccessToken, acc, settings, nil
	}
	if settings.IsKimiWork() {
		// Load-balance across the Kimi pool (request-scoped routing; never flip
		// the global active account or rewrite settings on a read path).
		var acc *store.Account
		if preferID != "" {
			if a, ok2 := st.GetAccount(preferID); ok2 && a != nil && a.NormalizedProvider() == store.ProviderKimiWork && a.Usable() {
				acc = a
			}
		}
		if acc == nil {
			acc = st.PickAccountForProvider(store.ProviderKimiWork, st.GetLoadBalancerStrategy(store.ProviderKimiWork))
		}
		if acc == nil {
			return "", nil, settings, fmt.Errorf("nenhuma conta Kimi Work")
		}
		st.IncAccountRequestCount(acc.ID)
		tok := acc.BearerToken()
		if tok == "" {
			st.DecAccountRequestCount(acc.ID)
			return "", nil, settings, fmt.Errorf("conta Kimi sem sk-kimi")
		}
		return tok, acc, settings, nil
	}
	// Sync CLI tokens at most once per 30s — parallel request storms must not
	// read ~/.grok/auth.json + write SQLite on every request.
	// forceRefresh always re-reads: official CLI rotates RTs under auth.json.lock.
	lastCLISyncMu.Lock()
	doSync := forceRefresh || time.Since(lastCLISync) > 30*time.Second
	if doSync {
		lastCLISync = time.Now()
	}
	lastCLISyncMu.Unlock()
	if doSync {
		_, _ = st.SyncFromGrokCLI()
	}
	if rp := proxyhttp.RouteProviderFrom(ctx); rp != "" {
		settings = st.Settings().WithProvider(rp)
	} else {
		settings = st.Settings()
	}

	// xAI: load-balance across the pool instead of piling onto ActiveAccountID.
	var acc *store.Account
	if preferID != "" {
		if a, ok2 := st.GetAccount(preferID); ok2 && a != nil && a.NormalizedProvider() == store.ProviderXAI && a.Usable() {
			acc = a
		}
	}
	if acc == nil {
		acc = st.PickAccountForProvider(store.ProviderXAI, st.GetLoadBalancerStrategy(store.ProviderXAI))
	}
	if acc == nil {
		return "", nil, settings, fmt.Errorf("nenhuma conta — faça login no Grok Desktop")
	}
	st.IncAccountRequestCount(acc.ID)
	if oauth.BotFlagged(acc.AccessToken) {
		st.DecAccountRequestCount(acc.ID)
		_, _ = st.MarkAuthDenied(acc.ID, "bot_flag_source")
		if next := st.NextUsableAccountID(acc.ID); next != "" {
			return ensureCreds(ctx, st, oa, ca, next, false)
		}
		return "", nil, settings, fmt.Errorf("conta bloqueada (bot flag)")
	}
	// After a forced CLI sync, re-load the preferred account — Sync may have
	// replaced AT/RT (or healed flags) for this id.
	if forceRefresh {
		if latest, ok := st.GetAccount(acc.ID); ok && latest != nil {
			*acc = *latest
		}
	}
	// forceRefresh still triggers OAuth (auth-denied recovery), but only after
	// SyncFromGrokCLI above so we hold the same RT the official CLI just wrote.
	need := forceRefresh || acc.ExpiresSoon(5*time.Minute) || acc.Expired() || acc.AccessToken == ""
	if need && acc.RefreshToken != "" {
		if err := refreshXAIAccount(ctx, st, oa, acc, forceRefresh); err != nil {
			if oauth.IsInvalidGrant(err) {
				logging.Warn("xai.refresh.invalid_grant", "account_id", acc.ID, "err", err.Error())
				// Official CLI may have rotated the RT; pull ~/.grok/auth.json and retry once.
				recovered := false
				if n, serr := st.SyncFromGrokCLI(); serr == nil && n > 0 {
					if latest, ok := st.GetAccount(acc.ID); ok && latest != nil {
						*acc = *latest
						if retryErr := refreshXAIAccount(ctx, st, oa, acc, forceRefresh); retryErr == nil {
							recovered = true
						}
					}
				}
				if !recovered {
					_, _ = st.MarkAuthDenied(acc.ID, "invalid_grant: "+err.Error())
					st.DecAccountRequestCount(acc.ID)
					if next := st.NextUsableAccountID(acc.ID); next != "" && next != acc.ID {
						logging.Info("xai.refresh.rotate_after_invalid_grant", "from", acc.ID, "to", next)
						return ensureCreds(ctx, st, oa, ca, next, false)
					}
					return "", nil, settings, fmt.Errorf("token expirado: %v", err)
				}
			} else if forceRefresh || acc.Expired() {
				st.DecAccountRequestCount(acc.ID)
				if next := st.NextUsableAccountID(acc.ID); next != "" && next != acc.ID {
					logging.Info("xai.refresh.rotate_after_fail", "from", acc.ID, "to", next, "err", err.Error())
					return ensureCreds(ctx, st, oa, ca, next, false)
				}
				return "", nil, settings, fmt.Errorf("token expirado: %v", err)
			}
			// Soft failure near expiry: keep current token only if not fully expired.
		}
	}
	if acc.AccessToken == "" {
		st.DecAccountRequestCount(acc.ID)
		return "", nil, settings, fmt.Errorf("conta sem access_token")
	}
	return acc.AccessToken, acc, settings, nil
}

// refreshXAIAccount performs the OAuth refresh under a per-account mutex and
// persists rotated tokens. After acquiring the lock it re-reads the account and
// skips the refresh when another goroutine already rotated the tokens.
// On success it also mirrors tokens into ~/.grok/auth.json (same OIDC client as
// the official Grok CLI) so concurrent CLI+proxy use does not revoke each other.
func refreshXAIAccount(ctx context.Context, st *store.Store, oa *oauth.Client, acc *store.Account, force bool) error {
	if acc == nil || acc.RefreshToken == "" {
		return fmt.Errorf("no refresh token")
	}
	mu := accountRefreshMu(acc.ID)
	mu.Lock()
	defer mu.Unlock()
	// Re-read after lock — another goroutine may have refreshed already.
	if latest, ok := st.GetAccount(acc.ID); ok && latest != nil {
		if !force && !latest.ExpiresSoon(5*time.Minute) && latest.AccessToken != "" && latest.AccessToken != acc.AccessToken {
			*acc = *latest
			return nil
		}
		acc.RefreshToken = latest.RefreshToken
		acc.AccessToken = latest.AccessToken
		acc.ExpiresAt = latest.ExpiresAt
	}
	logging.Info("xai.refresh.start", "account_id", acc.ID)
	tok, err := oa.Refresh(ctx, acc.RefreshToken, acc.ClientID, acc.Issuer)
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
	if err := st.UpsertAccount(*acc); err != nil {
		return err
	}
	_ = st.ClearAuthDenied(acc.ID)
	if err := st.WriteAccountToGrokCLI(*acc); err != nil {
		logging.Warn("xai.refresh.cli_write_failed", "account_id", acc.ID, "err", err.Error())
	}
	return nil
}

func refreshCodexAccount(ctx context.Context, st *store.Store, ca *codexauth.Client, acc *store.Account, force bool) error {
	if acc == nil || acc.RefreshToken == "" || ca == nil {
		return fmt.Errorf("no Codex refresh token")
	}
	mu := accountRefreshMu(acc.ID)
	mu.Lock()
	defer mu.Unlock()
	return st.WithAccountRefreshLock(ctx, acc.ID, func() error {
		if latest, ok := st.GetAccount(acc.ID); ok && latest != nil {
			if !latest.ExpiresSoon(5*time.Minute) && latest.AccessToken != "" && latest.AccessToken != acc.AccessToken {
				*acc = *latest
				return nil
			}
			*acc = *latest
		}
		tok, err := ca.Refresh(ctx, acc.RefreshToken)
		if err != nil {
			return err
		}
		acc.AccessToken = tok.AccessToken
		if tok.RefreshToken != "" {
			acc.RefreshToken = tok.RefreshToken
		}
		claims := codexauth.ParseClaims(tok.AccessToken)
		if !claims.ExpiresAt.IsZero() {
			acc.ExpiresAt = claims.ExpiresAt
		}
		if tok.IDToken != "" {
			identity := codexauth.ParseClaims(tok.IDToken)
			if identity.Email != "" {
				acc.Email = identity.Email
			}
			if identity.UserID != "" {
				acc.UserID = identity.UserID
			}
			if identity.AccountID != "" {
				acc.TeamID = identity.AccountID
			}
			if identity.FedRAMP {
				acc.Source = "codex_oauth_fedramp"
			}
		}
		acc.UpdatedAt = time.Now().UTC()
		acc.AuthDeniedAt = time.Time{}
		acc.AuthDeniedReason = ""
		if err := st.UpsertAccount(*acc); err != nil {
			return err
		}
		return st.ClearAuthDenied(acc.ID)
	})
}
