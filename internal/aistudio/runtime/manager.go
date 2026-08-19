// Package runtime ties together the per-profile CDP/Botguard/Chat clients and
// implements session routing, account load-balancing and migration.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"grok-desktop/internal/aistudio/account"
	"grok-desktop/internal/aistudio/botguard"
	"grok-desktop/internal/aistudio/cdp"
	"grok-desktop/internal/aistudio/chat"
	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/profile"
	"grok-desktop/internal/aistudio/session"
	"grok-desktop/internal/aistudio/tts"
)

// Manager owns all runtimes and stores.
type Manager struct {
	cfg      *config.Config
	profiles *profile.Registry
	sessions *session.Store
	accounts *account.Store
	replay   *chat.NativeReplayStore

	mu                sync.Mutex
	runtimes          map[string]*Runtime
	activeChatProfile string
}

// Runtime bundles the per-profile clients.
type Runtime struct {
	Profile  *profile.Profile
	CDP      *cdp.Client
	Botguard *botguard.Service
	Chat     *chat.Client
	TTS      *tts.Client
}

// New constructs a Manager.
func New(cfg *config.Config) *Manager {
	return &Manager{
		cfg:      cfg,
		profiles: profile.New(cfg),
		sessions: session.New(cfg.StateDir + "/sessions.json"),
		accounts: account.New(cfg.AccountFile),
		replay:   chat.NewNativeReplayStore(filepath.Join(cfg.StateDir, "gemini-native-tool-replay.json")),
		runtimes: map[string]*Runtime{},
	}
}

// Config returns the underlying config.
func (m *Manager) Config() *config.Config { return m.cfg }

// Sessions returns the session store.
func (m *Manager) Sessions() *session.Store { return m.sessions }

// Accounts returns the account store.
func (m *Manager) Accounts() *account.Store { return m.accounts }

// Profiles returns the profile registry.
func (m *Manager) Profiles() *profile.Registry { return m.profiles }

// ListProfiles returns all configured profiles.
func (m *Manager) ListProfiles() []profile.Profile { return m.profiles.List() }

// GetRuntimeIfExists returns a cached runtime without creating one.
func (m *Manager) GetRuntimeIfExists(profileID string) *Runtime {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtimes[profileID]
}

// GetRuntime returns (or lazily creates) the runtime for a profile id.
func (m *Manager) GetRuntime(profileID string) (*Runtime, error) {
	p, err := m.profiles.Resolve(profileID)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if rt, ok := m.runtimes[p.ID]; ok {
		return rt, nil
	}
	cdpClient := cdp.New(p, m.cfg)
	bg := botguard.New(cdpClient, m.cfg, p.ID)
	chatClient := chat.New(cdpClient, bg, m.cfg, p.ID, m.replay)
	ttsClient := tts.New(chatClient)
	rt := &Runtime{Profile: p, CDP: cdpClient, Botguard: bg, Chat: chatClient, TTS: ttsClient}
	m.runtimes[p.ID] = rt
	return rt, nil
}

// GetActiveChatProfile selects the active chat profile, respecting exclusions.
func (m *Manager) GetActiveChatProfile(excludedIDs []string) (*profile.Profile, error) {
	m.mu.Lock()
	active := m.activeChatProfile
	m.mu.Unlock()

	if active != "" {
		if p := m.profiles.Get(active); p != nil && !containsString(excludedIDs, active) && m.isProfileReusable(active) {
			return p, nil
		}
	}

	candidates := m.availableProfileIDs(excludedIDs)
	if len(candidates) == 0 {
		return nil, ErrNoAccount
	}
	pid, err := m.accounts.PickBestProfile(candidates, m.sessions.CountByProfile(), excludedIDs)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.activeChatProfile = pid
	m.mu.Unlock()
	return m.profiles.Get(pid), nil
}

// SwitchActiveChatProfile picks a new active profile, excluding the current one.
func (m *Manager) SwitchActiveChatProfile(fromID, reason string, excludedIDs []string) (*profile.Profile, error) {
	exclusions := append([]string{}, excludedIDs...)
	if fromID != "" {
		exclusions = append(exclusions, fromID)
	}
	candidates := m.availableProfileIDs(exclusions)
	if len(candidates) == 0 {
		return nil, ErrNoAccount
	}
	pid, err := m.accounts.PickBestProfile(candidates, m.sessions.CountByProfile(), exclusions)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.activeChatProfile = pid
	m.mu.Unlock()
	log.Printf("[ROUTER] Perfil ativo trocado %s -> %s (%s)", orDefault(fromID, "none"), pid, orDefault(reason, "unknown"))
	return m.profiles.Get(pid), nil
}

// SetActiveChatProfile updates the profile used by subsequent unpinned chat
// requests. It is called together with registry default changes so the admin
// API takes effect immediately rather than only after a restart.
func (m *Manager) SetActiveChatProfile(profileID string) error {
	if m.profiles.Get(profileID) == nil {
		return fmt.Errorf("runtime: perfil nao encontrado: %s", profileID)
	}
	m.mu.Lock()
	m.activeChatProfile = profileID
	m.mu.Unlock()
	return nil
}

// NoteRequest/NoteSuccess/NoteFailure proxy to the account store.
func (m *Manager) NoteRequest(profileID string) { m.accounts.NoteRequest(profileID) }
func (m *Manager) NoteSuccess(profileID string) { m.accounts.NoteSuccess(profileID) }
func (m *Manager) NoteFailure(profileID, message string, cooldownMs int) {
	m.accounts.NoteFailure(profileID, message, cooldownMs)
}

// ChooseProfileForNewSession picks the least-used available profile, preferring
// warm runtimes, excluding the provided ids.
func (m *Manager) ChooseProfileForNewSession(excludedIDs []string) (*profile.Profile, error) {
	profiles := m.listAvailableProfiles(excludedIDs)
	warm := make([]string, 0, len(profiles))
	for _, p := range profiles {
		if m.isProfileWarm(p.ID) {
			warm = append(warm, p.ID)
		}
	}
	candidates := warm
	if len(candidates) == 0 {
		candidates = make([]string, 0, len(profiles))
		for _, p := range profiles {
			candidates = append(candidates, p.ID)
		}
	}
	if len(candidates) == 0 {
		return nil, ErrNoAccount
	}
	pid, err := m.accounts.PickBestProfile(candidates, m.sessions.CountByProfile(), excludedIDs)
	if err != nil {
		return nil, err
	}
	return m.profiles.Get(pid), nil
}

// MigrateResult holds the outcome of a session migration.
type MigrateResult struct {
	Profile *profile.Profile
	Runtime *Runtime
}

// MigrateSession moves a session to a fresh profile and records the migration.
func (m *Manager) MigrateSession(ctx context.Context, sessionID, fromProfileID, reason string, excludedIDs []string) (*MigrateResult, error) {
	next, err := m.SwitchActiveChatProfile(fromProfileID, reason, excludedIDs)
	if err != nil {
		return nil, err
	}
	current := m.sessions.Get(sessionID)
	migrationCount := 0
	if current != nil {
		migrationCount = current.MigrationCount + 1
	}
	m.sessions.ClearPromptBinding(sessionID, next.ID, fromProfileID, orDefault(reason, "unknown"), migrationCount)
	m.accounts.NoteMigration(fromProfileID, next.ID)
	rt, err := m.GetRuntime(next.ID)
	if err != nil {
		return nil, err
	}
	return &MigrateResult{Profile: next, Runtime: rt}, nil
}

func (m *Manager) isProfileAvailable(profileID string) bool {
	p := m.profiles.Get(profileID)
	if p == nil {
		return false
	}
	if p.IsValid != nil && !*p.IsValid {
		return false
	}
	summary := m.accounts.Summarize([]string{profileID}, nil)
	if len(summary) == 0 {
		return true
	}
	return summary[0].Available
}

func (m *Manager) isProfileReusable(profileID string) bool {
	p := m.profiles.Get(profileID)
	if p == nil {
		return false
	}
	if p.IsValid != nil && !*p.IsValid {
		return false
	}
	summary := m.accounts.Summarize([]string{profileID}, nil)
	if len(summary) == 0 {
		return true
	}
	return summary[0].Available && summary[0].LastError == ""
}

func (m *Manager) isProfileWarm(profileID string) bool {
	m.mu.Lock()
	rt, ok := m.runtimes[profileID]
	m.mu.Unlock()
	if !ok || rt == nil || rt.Chat == nil {
		return false
	}
	return rt.Chat.IsReadyForRequests()
}

func (m *Manager) profileIDs() []string {
	profiles := m.ListProfiles()
	out := make([]string, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, p.ID)
	}
	return out
}

func (m *Manager) availableProfileIDs(excludedIDs []string) []string {
	profiles := m.listAvailableProfiles(excludedIDs)
	out := make([]string, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, p.ID)
	}
	return out
}

func (m *Manager) listAvailableProfiles(excludedIDs []string) []profile.Profile {
	excluded := map[string]bool{}
	for _, id := range excludedIDs {
		if id != "" {
			excluded[id] = true
		}
	}
	profiles := m.ListProfiles()
	out := make([]profile.Profile, 0, len(profiles))
	for _, p := range profiles {
		if excluded[p.ID] {
			continue
		}
		if !m.isProfileAvailable(p.ID) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Close persists all stores and terminates every browser process owned by this
// runtime. Disconnecting CDP alone leaves the managed Chrome alive and causes
// one full browser tree to accumulate on every app restart.
func (m *Manager) Close() {
	m.sessions.Close()
	m.accounts.Close()
	if m.replay != nil {
		m.replay.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	m.ShutdownAllManagedBrowsers(ctx)
	cancel()

	m.mu.Lock()
	for _, rt := range m.runtimes {
		rt.CDP.Disconnect()
	}
	m.runtimes = make(map[string]*Runtime)
	m.mu.Unlock()
}

// DisconnectProfile disconnects and forgets the cached runtime for a profile.
func (m *Manager) DisconnectProfile(profileID string) {
	m.mu.Lock()
	rt := m.runtimes[profileID]
	delete(m.runtimes, profileID)
	m.mu.Unlock()
	if rt != nil {
		rt.CDP.Disconnect()
	}
}

// ShutdownManagedBrowserProfile terminates the managed browser for a profile.
func (m *Manager) ShutdownManagedBrowserProfile(ctx context.Context, profileID string) error {
	rt, err := m.GetRuntime(profileID)
	if err != nil {
		return err
	}
	if rt == nil || rt.CDP == nil {
		return nil
	}
	if err := rt.CDP.ShutdownBrowserProcess(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.runtimes, profileID)
	m.mu.Unlock()
	return nil
}

// ShutdownAllManagedBrowsers terminates only browser runtimes already opened by
// this manager. Iterating ListProfiles()+GetRuntime used to instantiate missing
// runtimes while shutting down, which could launch extra Chrome processes.
func (m *Manager) ShutdownAllManagedBrowsers(ctx context.Context) {
	m.mu.Lock()
	snapshot := make(map[string]*Runtime, len(m.runtimes))
	for id, rt := range m.runtimes {
		snapshot[id] = rt
	}
	m.runtimes = make(map[string]*Runtime)
	m.mu.Unlock()

	for id, rt := range snapshot {
		if rt == nil || rt.CDP == nil {
			continue
		}
		if err := rt.CDP.ShutdownBrowserProcess(ctx); err != nil {
			log.Printf("[RUNTIME] shutdown browser %s falhou: %v", id, err)
		}
		rt.CDP.Disconnect()
	}
}

// ErrNoAccount is returned when no eligible account remains.
var ErrNoAccount = errors.New("runtime: nenhuma conta disponivel")

func containsString(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// WarmUpAll warms every runtime's HTTP caches (best-effort).
func (m *Manager) WarmUpAll(ctx context.Context) {
	m.mu.Lock()
	snapshot := make([]*Runtime, 0, len(m.runtimes))
	for _, rt := range m.runtimes {
		snapshot = append(snapshot, rt)
	}
	m.mu.Unlock()

	for _, rt := range snapshot {
		if rt.Chat == nil {
			continue
		}
		if _, err := rt.Chat.WarmUpHttpCaches(ctx); err != nil {
			log.Printf("[INIT] warmup %s falhou: %v", rt.Profile.ID, err)
		}
	}
}

// EagerBootBotguards connects and bootstraps Botguard for the configured
// profiles. Mode "all" includes every profile; "default" includes only the
// default profile; "none" disables boot.
func (m *Manager) EagerBootBotguards(ctx context.Context, mode string) {
	mode = normalizeMode(mode)
	if mode == "none" {
		return
	}
	var ids []string
	if mode == "all" {
		ids = m.profileIDs()
	} else {
		if p, err := m.profiles.Resolve(""); err == nil {
			ids = []string{p.ID}
		}
	}
	for _, id := range ids {
		rt, err := m.GetRuntime(id)
		if err != nil {
			continue
		}
		if err := rt.Botguard.EnsureReady(ctx); err != nil {
			log.Printf("[BOOT] Botguard %s falhou: %v", id, err)
			m.NoteFailure(id, fmt.Sprintf("botguard_boot_%s", err.Error()), m.cfg.Migration.CooldownMs.Runtime)
			continue
		}
		m.accounts.NoteBootSuccess(id)
		log.Printf("[BOOT] Botguard %s pronto", id)
	}
}

func normalizeMode(mode string) string {
	switch mode {
	case "", "default", "all", "none":
		return mode
	}
	return "default"
}
