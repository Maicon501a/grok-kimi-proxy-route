// Package accmgr keeps the Accio account pool topped up so user requests are
// never killed by an exhausted quota. It periodically aggregates the credits
// of every account in the pool; when the total drops below a configurable
// threshold it creates a fresh account (temp inbox + real Chrome via CDP +
// the proxy's own PKCE login) and lets the existing OnLogin bridge sync it
// into the proxy store.
package accmgr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"grok-desktop/internal/accio"
	"grok-desktop/internal/logging"
)

// Config holds the account-manager tuning knobs.
type Config struct {
	// MinCredits is the aggregate remaining balance that triggers a top-up.
	MinCredits int
	// MaxAccounts caps the pool size; no accounts are created beyond it.
	MaxAccounts int
	// CreateCooldown is the minimum delay between two account creations.
	CreateCooldown time.Duration
	// CheckEvery is how often the pool balance is re-aggregated.
	CheckEvery time.Duration
	// WARP rotates the Cloudflare WARP IP before each account creation.
	WARP bool
	// Headless runs the signup Chrome without a window (default true).
	Headless bool
}

// DefaultConfig returns sane defaults: top up below 300 credits, at most 10
// accounts, one creation per 10 minutes, balance re-check every minute.
// WARP rotation is OFF by default: the risk system keys on the browser
// profile, not the IP (see waf-accio-re.md §11.1), and WARP's datacenter IPs
// are actively counterproductive (codes issued under them get poisoned even
// for the Node exchange path) — enable with ACCIO_USE_WARP=1.
//
// The 10-minute create cooldown is deliberate and load-bearing: Accio's risk
// control heats up on signup VELOCITY per IP/profile. A burst of creations in
// a few minutes gets the resulting accounts limited (entitlement NOT_LOGIN)
// or outright blocked (423). Spaced creations come out clean with the full
// credit grant (~520: 500 referral + 20 daily).
func DefaultConfig() Config {
	return Config{
		MinCredits:     300,
		MaxAccounts:    10,
		CreateCooldown: 10 * time.Minute,
		CheckEvery:     time.Minute,
		WARP:           strings.TrimSpace(os.Getenv("ACCIO_USE_WARP")) == "1",
		Headless:       true,
	}
}

// Manager owns the top-up loop and exposes live status for the UI.
type Manager struct {
	acc *accio.Client
	cfg Config

	mu          sync.Mutex
	creating    bool
	lastCreate  time.Time
	lastCheck   time.Time
	balances    map[string]int
	total       int
	created     int
	failures    int
	consecutiveFailures int
	lastErr     string
	checking    bool
	stopCh      chan struct{}
	doneCh      chan struct{}
	started     bool
	enabled     bool
}

// New wires a Manager on top of the shared Accio client (pool on disk).
func New(acc *accio.Client, cfg Config) *Manager {
	return &Manager{
		acc:      acc,
		cfg:      cfg,
		balances: make(map[string]int),
		enabled:  true,
	}
}

// Start launches the background loop. Safe to call once.
func (m *Manager) Start() {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.stopCh = make(chan struct{})
	m.doneCh = make(chan struct{})
	m.mu.Unlock()

	logging.Info("accmgr.start", "min_credits", m.cfg.MinCredits, "max_accounts", m.cfg.MaxAccounts, "check_every", m.cfg.CheckEvery.String())
	go m.loop()
}

// Stop terminates the loop (blocking until it exits).
func (m *Manager) Stop() {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return
	}
	close(m.stopCh)
	m.mu.Unlock()
	<-m.doneCh
}

// SetEnabled toggles the manager without killing the loop.
func (m *Manager) SetEnabled(v bool) {
	m.mu.Lock()
	m.enabled = v
	m.mu.Unlock()
}

// SetConfig atomically replaces the tuning knobs.
func (m *Manager) SetConfig(cfg Config) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
	logging.Info("accmgr.config", "min_credits", cfg.MinCredits, "max_accounts", cfg.MaxAccounts)
}

func (m *Manager) loop() {
	defer close(m.doneCh)

	m.check(context.Background())
	for {
		// Read the interval every cycle so SetConfig takes effect without a
		// restart (the old ticker froze the initial CheckEvery forever).
		m.mu.Lock()
		every := m.cfg.CheckEvery
		m.mu.Unlock()
		if every <= 0 {
			every = time.Minute
		}
		timer := time.NewTimer(every)
		select {
		case <-m.stopCh:
			timer.Stop()
			return
		case <-timer.C:
			m.check(context.Background())
		}
	}
}

// check aggregates balances and triggers a creation when below threshold.
func (m *Manager) check(ctx context.Context) {
	m.mu.Lock()
	enabled := m.enabled
	m.mu.Unlock()
	if !enabled {
		return
	}

	total, infos, err := m.aggregateDetailed(ctx)
	// Prune accounts that never got their credits approved (remaining=0 and
	// total=0): they are dead weight and, counted toward the pool cap, could
	// stall top-ups forever. Exhausted-but-approved accounts (total>0) stay —
	// daily pools replenish.
	approved := 0
	for id, bi := range infos {
		if !pruneable(bi, time.Now()) {
			approved++
			continue
		}
		if derr := m.acc.RemoveAccount(id); derr == nil {
			logging.Warn("accmgr.account_removed_unapproved", "id", id)
			delete(infos, id)
		}
	}
	balances := make(map[string]int, len(infos))
	for id, bi := range infos {
		balances[id] = bi.remaining
	}
	m.mu.Lock()
	m.balances = balances
	m.total = total
	m.lastCheck = time.Now()
	if err != nil {
		m.lastErr = "check: " + err.Error()
	}
	m.mu.Unlock()
	// A broken account must not stall top-ups: the check still proceeds to
	// the threshold decision using whatever balances were readable.
	if err != nil {
		logging.Warn("accmgr.check_err", "err", err.Error(), "total", total)
	}

	m.mu.Lock()
	need := total < m.cfg.MinCredits && approved < m.cfg.MaxAccounts
	cooldown := backoffCooldown(m.cfg.CreateCooldown, m.consecutiveFailures)
	cooldownOK := time.Since(m.lastCreate) >= cooldown
	m.mu.Unlock()
	if !need || !cooldownOK {
		return
	}
	// Run the creation async so a multi-minute signup does not block the
	// balance loop; the creating flag inside createAccount prevents doubles.
	// lastCreate is stamped up front so the cooldown also bounds failures.
	m.mu.Lock()
	m.lastCreate = time.Now()
	m.mu.Unlock()
	go func() {
		cctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
		defer cancel()
		if err := m.createAccount(cctx); err != nil {
			m.mu.Lock()
			m.failures++
			m.consecutiveFailures++
			m.lastErr = err.Error()
			m.mu.Unlock()
			logging.Warn("accmgr.create_failed", "err", err.Error(), "consecutive", m.consecutiveFailures)
			return
		}
		m.mu.Lock()
		m.created++
		m.consecutiveFailures = 0
		m.mu.Unlock()
		logging.Info("accmgr.account_created")
	}()
}

// balanceInfo holds one account's entitlement snapshot.
type balanceInfo struct {
	remaining int
	total     int
	savedAt   int64 // unix millis, from TokenRecord.SavedAt
}

// pruneGracePeriod protects fresh accounts whose entitlement is still being
// provisioned server-side from the zero-balance prune.
const pruneGracePeriod = 10 * time.Minute

// pruneable reports whether an account never received credits (remaining=0
// and total=0) and is past the provisioning grace period. Such accounts are
// dead weight; counted toward the pool cap they could stall top-ups forever.
func pruneable(bi balanceInfo, now time.Time) bool {
	if bi.total > 0 || bi.remaining > 0 {
		return false
	}
	if bi.savedAt <= 0 {
		return true // legacy record without a timestamp: judged immediately
	}
	return now.Sub(time.UnixMilli(bi.savedAt)) >= pruneGracePeriod
}

// backoffCooldown grows the create cooldown exponentially after consecutive
// failures (base → 2x → 4x → … capped at 1h) so a hostile WAF period does
// not burn temp-mail accounts in a tight loop. One success resets it.
func backoffCooldown(base time.Duration, consecutiveFailures int) time.Duration {
	cooldown := base
	for i := 0; i < consecutiveFailures && i < 6; i++ {
		cooldown *= 2
	}
	if cooldown > time.Hour {
		return time.Hour
	}
	return cooldown
}

// Aggregate sums the remaining credits of every account in the pool.
func (m *Manager) Aggregate(ctx context.Context) (int, map[string]int, error) {
	total, infos, err := m.aggregateDetailed(ctx)
	balances := make(map[string]int, len(infos))
	for id, bi := range infos {
		balances[id] = bi.remaining
	}
	return total, balances, err
}

// aggregateDetailed reads every account's balance in parallel (bounded by a
// semaphore so a big pool cannot stampede the entitlement endpoint). A
// failing account is auto-healed (refresh + persist the rotated pair) once
// before being counted, so one expired access token cannot stall top-ups.
func (m *Manager) aggregateDetailed(ctx context.Context) (int, map[string]balanceInfo, error) {
	accounts := m.acc.Accounts()
	infos := make(map[string]balanceInfo, len(accounts))
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, acct := range accounts {
		wg.Add(1)
		go func(a accio.TokenRecord) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rem, tot, err := m.balanceFor(ctx, a)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				logging.Warn("accmgr.account_balance_err", "id", a.ID, "err", err.Error())
				return
			}
			mu.Lock()
			infos[a.ID] = balanceInfo{remaining: rem, total: tot, savedAt: a.SavedAt}
			mu.Unlock()
		}(acct)
	}
	wg.Wait()
	total := 0
	for _, bi := range infos {
		total += bi.remaining
	}
	return total, infos, firstErr
}

// balanceFor reads one account's balance (remaining + total ever granted),
// refreshing (and persisting the rotated pair) when the access token is
// rejected.
func (m *Manager) balanceFor(ctx context.Context, acc accio.TokenRecord) (int, int, error) {
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	credits, err := m.acc.CreditsFor(cctx, acc)
	cancel()
	if err == nil {
		return int(firstValueInt(credits, "remaining")), int(firstValueInt(credits, "total")), nil
	}
	if !strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "403") && !strings.Contains(err.Error(), "NOT_LOGIN") {
		return 0, 0, err
	}
	// Auto-heal: refresh the account's own pair (rotation-aware) and retry.
	// If the refresh token itself is dead, the account is garbage — remove it
	// so it cannot occupy a slot of the pool cap. Exception: fresh accounts
	// get "auth not pass" (502) from the refresh endpoint until server-side
	// provisioning completes (the official app sees the same); that is not a
	// dead account, keep it.
	rctx, cancel2 := context.WithTimeout(ctx, 30*time.Second)
	access, refresh, rerr := m.acc.RefreshWith(rctx, acc.AccessToken, acc.RefreshToken, acc.Cookie)
	cancel2()
	if rerr != nil {
		if strings.Contains(rerr.Error(), "auth not pass") {
			return 0, 0, fmt.Errorf("balance refresh deferred (account still provisioning): %w", rerr)
		}
		if derr := m.acc.RemoveAccount(acc.ID); derr == nil {
			logging.Warn("accmgr.account_removed_dead", "id", acc.ID)
		}
		return 0, 0, fmt.Errorf("balance refresh: %w", rerr)
	}
	acc.AccessToken = access
	if refresh != "" {
		acc.RefreshToken = refresh
	}
	if serr := m.acc.SaveAccount(acc); serr != nil {
		return 0, 0, fmt.Errorf("balance persist rotated token: %w", serr)
	}
	cctx2, cancel3 := context.WithTimeout(ctx, 25*time.Second)
	credits, err = m.acc.CreditsFor(cctx2, acc)
	cancel3()
	if err != nil {
		return 0, 0, err
	}
	return int(firstValueInt(credits, "remaining")), int(firstValueInt(credits, "total")), nil
}

// TopUpNow forces an immediate creation attempt (used by tests / UI button).
func (m *Manager) TopUpNow(ctx context.Context) error {
	m.mu.Lock()
	if m.creating {
		m.mu.Unlock()
		return errors.New("creation already in progress")
	}
	m.mu.Unlock()
	return m.createAccount(ctx)
}

// createAccount runs one full signup pipeline and blocks until the account is
// in the pool (or fails). Guarded by the creating flag.
func (m *Manager) createAccount(ctx context.Context) error {
	m.mu.Lock()
	if m.creating {
		m.mu.Unlock()
		return errors.New("creation already in progress")
	}
	m.creating = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.creating = false
		m.mu.Unlock()
	}()

	rec, err := signupFlow(ctx, m.acc, signupOptions{
		warp:     m.cfg.WARP,
		headless: m.cfg.Headless,
	})
	if err != nil {
		return fmt.Errorf("signup: %w", err)
	}
	logging.Info("accmgr.signup_ok", "id", rec.ID, "email", rec.Email)
	return nil
}

// Status returns a snapshot for the UI (safe for JSON binding).
func (m *Manager) Status() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	bal := make(map[string]int, len(m.balances))
	for k, v := range m.balances {
		bal[k] = v
	}
	return map[string]any{
		"enabled":       m.enabled,
		"min_credits":   m.cfg.MinCredits,
		"max_accounts":  m.cfg.MaxAccounts,
		"total":         m.total,
		"balances":      bal,
		"accounts":      len(bal),
		"creating":      m.creating,
		"created":       m.created,
		"failures":      m.failures,
		"consecutive_failures": m.consecutiveFailures,
		"last_err":      m.lastErr,
		"last_check_at": m.lastCheck.UnixMilli(),
		"last_create_at": m.lastCreate.UnixMilli(),
	}
}
