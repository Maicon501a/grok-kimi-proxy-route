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
// accounts, one creation per 3 minutes, balance re-check every 2 minutes.
// WARP rotation is on by default (each creation gets a fresh IP — the Accio
// anti-fraud marks IPs after a few signups); disable with ACCIO_USE_WARP=0.
func DefaultConfig() Config {
	return Config{
		MinCredits:     300,
		MaxAccounts:    10,
		CreateCooldown: 3 * time.Minute,
		CheckEvery:     2 * time.Minute,
		WARP:           true,
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
	ticker := time.NewTicker(m.cfg.CheckEvery)
	defer ticker.Stop()

	m.check(context.Background())
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
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

	total, balances, err := m.Aggregate(ctx)
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
	need := total < m.cfg.MinCredits && len(balances) < m.cfg.MaxAccounts
	// Backoff: after consecutive failures the cooldown grows exponentially
	// (3m → 6m → 12m → … capped at 1h) so a hostile WAF period does not
	// burn temp-mail accounts in a tight loop. One success resets it.
	cooldown := m.cfg.CreateCooldown
	for i := 0; i < m.consecutiveFailures && i < 5; i++ {
		cooldown *= 2
	}
	if cooldown > time.Hour {
		cooldown = time.Hour
	}
	cooldownOK := time.Since(m.lastCreate) >= cooldown
	m.mu.Unlock()
	if !need || !cooldownOK {
		return
	}
	if err := m.createAccount(ctx); err != nil {
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
	m.lastCreate = time.Now()
	m.mu.Unlock()
	logging.Info("accmgr.account_created")
}

// Aggregate sums the remaining credits of every account in the pool. A
// failing account is auto-healed (refresh + persist the rotated pair) once
// before being counted, so one expired access token cannot stall top-ups.
func (m *Manager) Aggregate(ctx context.Context) (int, map[string]int, error) {
	accounts := m.acc.Accounts()
	balances := make(map[string]int, len(accounts))
	total := 0
	var firstErr error
	for _, acc := range accounts {
		rem, err := m.balanceFor(ctx, acc)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			logging.Warn("accmgr.account_balance_err", "id", acc.ID, "err", err.Error())
			continue
		}
		balances[acc.ID] = rem
		total += rem
	}
	return total, balances, firstErr
}

// balanceFor reads one account's balance, refreshing (and persisting the
// rotated pair) when the access token is rejected.
func (m *Manager) balanceFor(ctx context.Context, acc accio.TokenRecord) (int, error) {
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	credits, err := m.acc.CreditsFor(cctx, acc)
	cancel()
	if err == nil {
		return int(firstValueInt(credits, "remaining")), nil
	}
	if !strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "403") && !strings.Contains(err.Error(), "NOT_LOGIN") {
		return 0, err
	}
	// Auto-heal: refresh the account's own pair (rotation-aware) and retry.
	// If the refresh token itself is dead, the account is garbage — remove it
	// so it cannot occupy a slot of the pool cap.
	rctx, cancel2 := context.WithTimeout(ctx, 30*time.Second)
	access, refresh, rerr := m.acc.RefreshWith(rctx, acc.RefreshToken)
	cancel2()
	if rerr != nil {
		if derr := m.acc.RemoveAccount(acc.ID); derr == nil {
			logging.Warn("accmgr.account_removed_dead", "id", acc.ID)
		}
		return 0, fmt.Errorf("balance refresh: %w", rerr)
	}
	acc.AccessToken = access
	if refresh != "" {
		acc.RefreshToken = refresh
	}
	if serr := m.acc.SaveAccount(acc); serr != nil {
		return 0, fmt.Errorf("balance persist rotated token: %w", serr)
	}
	cctx2, cancel3 := context.WithTimeout(ctx, 25*time.Second)
	credits, err = m.acc.CreditsFor(cctx2, acc)
	cancel3()
	if err != nil {
		return 0, err
	}
	return int(firstValueInt(credits, "remaining")), nil
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
