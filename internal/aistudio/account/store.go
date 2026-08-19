// Package account tracks per-profile health/usage metrics and implements the
// load-balancing selection logic (least-used + availability + jitter).
package account

import (
	"encoding/json"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"

	"grok-desktop/internal/aistudio/models"
	"grok-desktop/internal/aistudio/storage"
)

// Store is the thread-safe account state store.
type Store struct {
	mu     sync.RWMutex
	data   storeData
	writer *storage.AsyncWriter
}

type storeData struct {
	Accounts map[string]*models.AccountState `json:"accounts"`
}

// New opens or creates the account store at path.
func New(path string) *Store {
	s := &Store{
		data:   storeData{Accounts: map[string]*models.AccountState{}},
		writer: storage.NewAsyncWriter(path, 500*time.Millisecond),
	}
	s.load(path)
	return s
}

func (s *Store) load(path string) {
	raw, err := storage.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return
	}
	var parsed storeData
	if err := json.Unmarshal(raw, &parsed); err == nil && parsed.Accounts != nil {
		s.data.Accounts = parsed.Accounts
	}
}

// Close flushes pending writes.
func (s *Store) Close() { s.writer.Close() }

// Get returns a copy of the state for a profile.
func (s *Store) Get(profileID string) models.AccountState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st, ok := s.data.Accounts[profileID]; ok {
		return *st
	}
	return defaultState(profileID)
}

// update applies a mutator under the lock and persists.
func (s *Store) update(profileID string, fn func(*models.AccountState, time.Time)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	st := s.data.Accounts[profileID]
	if st == nil {
		d := defaultState(profileID)
		st = &d
		s.data.Accounts[profileID] = st
	}
	fn(st, now)
	encoded, _ := json.MarshalIndent(s.data, "", "  ")
	s.writer.Schedule(encoded)
}

// NoteRequest increments request counters.
func (s *Store) NoteRequest(profileID string) {
	s.update(profileID, func(st *models.AccountState, now time.Time) {
		st.TotalRequests++
		st.RecentRequests++
		st.LastUsedAt = now.Format(time.RFC3339)
	})
}

// NoteSuccess clears failure counters.
func (s *Store) NoteSuccess(profileID string) {
	s.update(profileID, func(st *models.AccountState, _ time.Time) {
		st.ConsecutiveFailures = 0
		st.LastError = ""
		st.CooldownUntil = ""
	})
}

// NoteBootSuccess clears only stale bootstrap failures. It must not erase
// quota/auth/chat cooldowns that are unrelated to the eager Botguard boot.
func (s *Store) NoteBootSuccess(profileID string) {
	s.update(profileID, func(st *models.AccountState, _ time.Time) {
		if !strings.HasPrefix(st.LastError, "botguard_boot_") {
			return
		}
		st.ConsecutiveFailures = 0
		st.LastError = ""
		st.CooldownUntil = ""
	})
}

// NoteFailure records a failure with an optional cooldown window.
func (s *Store) NoteFailure(profileID, message string, cooldownMs int) {
	s.update(profileID, func(st *models.AccountState, now time.Time) {
		st.ConsecutiveFailures++
		st.Failures++
		if message == "" {
			message = "unknown_error"
		}
		st.LastError = message
		st.LastFailureAt = now.Format(time.RFC3339)
		if cooldownMs > 0 {
			st.CooldownUntil = now.Add(time.Duration(cooldownMs) * time.Millisecond).Format(time.RFC3339)
		}
	})
}

// NoteMigration records a migration between two profiles.
func (s *Store) NoteMigration(fromID, toID string) {
	s.update(fromID, func(st *models.AccountState, _ time.Time) {
		st.MigrationsOut++
	})
	s.update(toID, func(st *models.AccountState, _ time.Time) {
		st.MigrationsIn++
	})
}

// Summarize returns the public view of the given profiles.
func (s *Store) Summarize(profileIDs []string, sessionCounts map[string]int) []models.AccountSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	out := make([]models.AccountSummary, 0, len(profileIDs))
	for _, pid := range profileIDs {
		st := s.data.Accounts[pid]
		if st == nil {
			st = &models.AccountState{}
			d := defaultState(pid)
			st = &d
		}
		cooldown := parseTime(st.CooldownUntil)
		available := cooldown.IsZero() || !cooldown.After(now)
		out = append(out, models.AccountSummary{
			ProfileID:           pid,
			ActiveSessions:      sessionCounts[pid],
			TotalRequests:       st.TotalRequests,
			RecentRequests:      st.RecentRequests,
			ConsecutiveFailures: st.ConsecutiveFailures,
			CooldownUntil:       st.CooldownUntil,
			Available:           available,
			LastUsedAt:          st.LastUsedAt,
			LastError:           st.LastError,
		})
	}
	return out
}

// PickLeastUsedProfile returns the best profile id from candidates (excluding
// excludedIDs) using a weighted score with jitter.
func (s *Store) PickLeastUsedProfile(candidates []string, sessionCounts map[string]int, excludedIDs []string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	summaries := s.candidateSummariesLocked(candidates, sessionCounts, excludedIDs)
	if len(summaries) == 0 {
		return "", ErrNoAccountAvailable
	}

	available := filter(summaries, func(a models.AccountSummary) bool { return a.Available })
	pool := summaries
	if len(available) > 0 {
		pool = available
	}

	best := ""
	bestScore := math.MaxFloat64
	for _, entry := range pool {
		score := scoreAccount(entry)
		if score < bestScore {
			bestScore = score
			best = entry.ProfileID
		}
	}
	return best, nil
}

// PickBestProfile returns the most reliable profile id among the candidates,
// preferring accounts with clean recent success over random spreading.
func (s *Store) PickBestProfile(candidates []string, sessionCounts map[string]int, excludedIDs []string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	summaries := s.candidateSummariesLocked(candidates, sessionCounts, excludedIDs)
	if len(summaries) == 0 {
		return "", ErrNoAccountAvailable
	}

	available := filter(summaries, func(a models.AccountSummary) bool { return a.Available })
	pool := summaries
	if len(available) > 0 {
		pool = available
	}

	best := ""
	bestScore := math.MaxFloat64
	for _, entry := range pool {
		score := scoreReliableAccount(entry)
		if score < bestScore || (score == bestScore && (best == "" || entry.ProfileID < best)) {
			bestScore = score
			best = entry.ProfileID
		}
	}
	return best, nil
}

// PickRandomProfile returns a random available (or any if none available)
// profile id.
func (s *Store) PickRandomProfile(candidates []string, excludedIDs []string, onlyAvailable bool) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	excluded := toSet(excludedIDs)
	pool := make([]string, 0, len(candidates))
	for _, pid := range candidates {
		if excluded[pid] {
			continue
		}
		pool = append(pool, pid)
	}
	if len(pool) == 0 {
		return "", ErrNoAccountAvailable
	}

	if onlyAvailable {
		available := make([]string, 0, len(pool))
		for _, pid := range pool {
			st := s.data.Accounts[pid]
			if st == nil {
				available = append(available, pid)
				continue
			}
			cooldown := parseTime(st.CooldownUntil)
			if cooldown.IsZero() || !cooldown.After(time.Now()) {
				available = append(available, pid)
			}
		}
		if len(available) > 0 {
			pool = available
		}
	}

	return pool[rand.Intn(len(pool))], nil
}

func (s *Store) candidateSummariesLocked(candidates []string, sessionCounts map[string]int, excludedIDs []string) []models.AccountSummary {
	excluded := toSet(excludedIDs)
	now := time.Now()
	summaries := make([]models.AccountSummary, 0, len(candidates))
	for _, pid := range candidates {
		if excluded[pid] {
			continue
		}
		st := s.data.Accounts[pid]
		if st == nil {
			d := defaultState(pid)
			st = &d
		}
		cooldown := parseTime(st.CooldownUntil)
		available := cooldown.IsZero() || !cooldown.After(now)
		summaries = append(summaries, models.AccountSummary{
			ProfileID:           pid,
			ActiveSessions:      sessionCounts[pid],
			TotalRequests:       st.TotalRequests,
			RecentRequests:      st.RecentRequests,
			ConsecutiveFailures: st.ConsecutiveFailures,
			CooldownUntil:       st.CooldownUntil,
			Available:           available,
			LastUsedAt:          st.LastUsedAt,
			LastError:           st.LastError,
		})
	}
	return summaries
}

func scoreAccount(entry models.AccountSummary) float64 {
	activeSessionsCost := float64(entry.ActiveSessions) * 50
	recentRequestsCost := float64(entry.RecentRequests) * 10
	totalRequestsCost := float64(entry.TotalRequests) * 1

	lastUsed := parseTime(entry.LastUsedAt)
	timeSinceLastUseSec := 3600.0
	if !lastUsed.IsZero() {
		timeSinceLastUseSec = time.Since(lastUsed).Seconds()
	}
	recencyBonus := math.Min(timeSinceLastUseSec, 900) * -0.1

	base := activeSessionsCost + recentRequestsCost + totalRequestsCost + recencyBonus
	jitter := (rand.Float64() - 0.5) * 15
	return base + jitter
}

func scoreReliableAccount(entry models.AccountSummary) float64 {
	score := float64(entry.ActiveSessions) * 25
	score += float64(entry.RecentRequests) * 0.05
	score += float64(entry.TotalRequests) * 0.01
	score += float64(entry.ConsecutiveFailures) * 1000
	if strings.TrimSpace(entry.LastError) != "" {
		score += 300
	}

	lastUsed := parseTime(entry.LastUsedAt)
	if lastUsed.IsZero() {
		score += 180
	} else {
		stalenessMinutes := time.Since(lastUsed).Minutes()
		if stalenessMinutes < 0 {
			stalenessMinutes = 0
		}
		score += math.Min(stalenessMinutes, 120) * 2
	}

	return score
}

func defaultState(profileID string) models.AccountState {
	return models.AccountState{ProfileID: profileID}
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return t
}

func toSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		if item != "" {
			out[item] = true
		}
	}
	return out
}

func filter(list []models.AccountSummary, predicate func(models.AccountSummary) bool) []models.AccountSummary {
	out := make([]models.AccountSummary, 0, len(list))
	for _, item := range list {
		if predicate(item) {
			out = append(out, item)
		}
	}
	return out
}
