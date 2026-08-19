// Package session persists proxy sessions (profile binding, prompt metadata,
// tool mode) to disk via a debounced writer.
package session

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"grok-desktop/internal/aistudio/models"
	"grok-desktop/internal/aistudio/storage"
)

// Store is the thread-safe session store.
type Store struct {
	mu     sync.RWMutex
	data   storeData
	writer *storage.AsyncWriter
}

type storeData struct {
	Sessions map[string]*models.Session `json:"sessions"`
}

// New opens or creates the store at the given path.
func New(path string) *Store {
	s := &Store{
		data:   storeData{Sessions: map[string]*models.Session{}},
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
	if err := json.Unmarshal(raw, &parsed); err == nil && parsed.Sessions != nil {
		s.data.Sessions = parsed.Sessions
	}
}

func (s *Store) save() {
	s.saveLocked()
}

func (s *Store) saveLocked() {
	encoded, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return
	}
	s.writer.Schedule(encoded)
}

// Close flushes pending writes.
func (s *Store) Close() { s.writer.Close() }

// Get returns a copy of a session, or nil if absent.
func (s *Store) Get(id string) *models.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sess, ok := s.data.Sessions[id]; ok {
		cp := *sess
		return &cp
	}
	return nil
}

// List returns copies of all sessions.
func (s *Store) List() []models.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]models.Session, 0, len(s.data.Sessions))
	for _, sess := range s.data.Sessions {
		out = append(out, *sess)
	}
	return out
}

// CountByProfile counts active (non-stateless) sessions per profile.
func (s *Store) CountByProfile() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	counts := map[string]int{}
	for _, sess := range s.data.Sessions {
		if sess.ConversationMode == "stateless" {
			continue
		}
		pid := sess.ProfileID
		if pid == "" {
			pid = "default"
		}
		counts[pid]++
	}
	return counts
}

// FindLatestByClientSessionID returns the most recently updated session matching
// the client session id, excluding excludeID if non-empty.
func (s *Store) FindLatestByClientSessionID(clientSessionID, excludeID string) *models.Session {
	clientSessionID = strings.TrimSpace(clientSessionID)
	if clientSessionID == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	matches := s.matchesByClientSessionLocked(clientSessionID, excludeID)
	if len(matches) == 0 {
		return nil
	}
	cp := *matches[0]
	return &cp
}

// FindLogicalSeedByClientSessionID aggregates seed fields across all sessions
// sharing a client session id. The original Node implementation merged these
// field-by-field; we preserve that behaviour.
func (s *Store) FindLogicalSeedByClientSessionID(clientSessionID, excludeID string) *models.Session {
	clientSessionID = strings.TrimSpace(clientSessionID)
	if clientSessionID == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	matches := s.matchesByClientSessionLocked(clientSessionID, excludeID)
	if len(matches) == 0 {
		return nil
	}
	seed := &models.Session{}
	fields := []string{
		"conversation_mode", "profile_id", "last_model", "system_instruction",
		"thinking_level", "tool_calling_mode", "aistudio_prompt_id",
		"aistudio_prompt_url", "account_fingerprint", "client_session_id",
		"proxycode_mode", "proxycode_chat_generation",
	}
	merged := map[string]string{}
	for _, field := range fields {
		for _, m := range matches {
			val := sessionFieldString(m, field)
			if val != "" {
				merged[field] = val
				break
			}
		}
	}
	seed.ConversationMode = merged["conversation_mode"]
	seed.ProfileID = merged["profile_id"]
	seed.LastModel = merged["last_model"]
	seed.SystemInstruction = merged["system_instruction"]
	seed.ThinkingLevel = merged["thinking_level"]
	seed.ToolCallingMode = merged["tool_calling_mode"]
	seed.AistudioPromptID = merged["aistudio_prompt_id"]
	seed.AistudioPromptURL = merged["aistudio_prompt_url"]
	seed.AccountFingerprint = merged["account_fingerprint"]
	seed.ClientSessionID = merged["client_session_id"]
	seed.ProxyCodeMode = merged["proxycode_mode"]
	if merged["proxycode_chat_generation"] != "" {
		if n, err := atoiSafe(merged["proxycode_chat_generation"]); err == nil {
			seed.ProxyCodeChatGeneration = n
		}
	}
	return seed
}

func (s *Store) matchesByClientSessionLocked(clientSessionID, excludeID string) []*models.Session {
	matches := []*models.Session{}
	for _, sess := range s.data.Sessions {
		if sess.ClientSessionID != clientSessionID {
			continue
		}
		if excludeID != "" && sess.SessionID == excludeID {
			continue
		}
		matches = append(matches, sess)
	}
	sort.Slice(matches, func(i, j int) bool {
		return timestampOf(matches[i]) > timestampOf(matches[j])
	})
	return matches
}

// EnsureSession returns the session for id, creating or updating it with the
// provided metadata. Missing metadata fields are preserved from the existing
// session.
func (s *Store) EnsureSession(id string, meta SessionMeta) *models.Session {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	existing := s.data.Sessions[id]
	if existing == nil {
		sess := &models.Session{
			SessionID:               id,
			ProfileID:               meta.ProfileID,
			CreatedAt:               now,
			UpdatedAt:               now,
			ConversationMode:        meta.ConversationMode,
			AistudioPromptID:        meta.AistudioPromptID,
			AistudioPromptURL:       meta.AistudioPromptURL,
			AccountFingerprint:      meta.AccountFingerprint,
			LastModel:               meta.LastModel,
			SystemInstruction:       meta.SystemInstruction,
			ThinkingLevel:           meta.ThinkingLevel,
			ToolCallingMode:         meta.ToolCallingMode,
			ClientSessionID:         meta.ClientSessionID,
			ProxyCodeMode:           meta.ProxyCodeMode,
			ProxyCodeChatGeneration: meta.ProxyCodeChatGeneration,
		}
		s.data.Sessions[id] = sess
		s.saveLocked()
		cp := *sess
		return &cp
	}

	if meta.ProfileID != "" {
		existing.ProfileID = meta.ProfileID
	}
	if meta.ConversationMode != "" {
		existing.ConversationMode = meta.ConversationMode
	}
	if meta.LastModel != "" {
		existing.LastModel = meta.LastModel
	}
	existing.AistudioPromptID = firstNonEmpty(meta.AistudioPromptID, existing.AistudioPromptID)
	existing.AistudioPromptURL = firstNonEmpty(meta.AistudioPromptURL, existing.AistudioPromptURL)
	existing.AccountFingerprint = firstNonEmpty(meta.AccountFingerprint, existing.AccountFingerprint)
	if meta.SystemInstruction != "" {
		existing.SystemInstruction = meta.SystemInstruction
	}
	if meta.ThinkingLevel != "" {
		existing.ThinkingLevel = meta.ThinkingLevel
	}
	if meta.ToolCallingMode != "" {
		existing.ToolCallingMode = meta.ToolCallingMode
	}
	if meta.ClientSessionID != "" {
		existing.ClientSessionID = meta.ClientSessionID
	}
	if meta.ProxyCodeMode != "" {
		existing.ProxyCodeMode = meta.ProxyCodeMode
	}
	if meta.ProxyCodeChatGeneration != 0 {
		existing.ProxyCodeChatGeneration = meta.ProxyCodeChatGeneration
	}
	existing.UpdatedAt = now

	s.saveLocked()
	cp := *existing
	return &cp
}

// UpdateMeta applies a partial metadata update to a session.
func (s *Store) UpdateMeta(id string, meta SessionMeta) *models.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.data.Sessions[id]
	if sess == nil {
		return nil
	}
	applyMeta(sess, meta)
	sess.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.saveLocked()
	cp := *sess
	return &cp
}

// ClearProfileBindings detaches all sessions bound to a profile (used during
// migration/account removal).
func (s *Store) ClearProfileBindings(profileID, reason string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := 0
	for _, sess := range s.data.Sessions {
		if sess.ProfileID != profileID {
			continue
		}
		sess.ProfileID = ""
		sess.AistudioPromptID = ""
		sess.AistudioPromptURL = ""
		sess.AccountFingerprint = ""
		sess.MigratedFromProfileID = profileID
		sess.MigrationReason = reason
		sess.MigrationCount++
		sess.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		changed++
	}
	if changed > 0 {
		s.saveLocked()
	}
	return changed
}

// ClearPromptBinding resets the AI Studio prompt/account binding of a session,
// used when it migrates to a different profile.
func (s *Store) ClearPromptBinding(id, profileID, fromProfileID, reason string, migrationCount int) *models.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.data.Sessions[id]
	if sess == nil {
		return nil
	}
	sess.ProfileID = profileID
	sess.AistudioPromptID = ""
	sess.AistudioPromptURL = ""
	sess.AccountFingerprint = ""
	sess.MigratedFromProfileID = fromProfileID
	sess.MigrationReason = reason
	sess.MigrationCount = migrationCount
	sess.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.saveLocked()
	cp := *sess
	return &cp
}

func applyMeta(sess *models.Session, meta SessionMeta) {
	if meta.ProfileID != "" {
		sess.ProfileID = meta.ProfileID
	}
	if meta.ConversationMode != "" {
		sess.ConversationMode = meta.ConversationMode
	}
	if meta.LastModel != "" {
		sess.LastModel = meta.LastModel
	}
	sess.AistudioPromptID = firstNonEmpty(meta.AistudioPromptID, sess.AistudioPromptID)
	sess.AistudioPromptURL = firstNonEmpty(meta.AistudioPromptURL, sess.AistudioPromptURL)
	sess.AccountFingerprint = firstNonEmpty(meta.AccountFingerprint, sess.AccountFingerprint)
	if meta.SystemInstruction != "" {
		sess.SystemInstruction = meta.SystemInstruction
	}
	if meta.ThinkingLevel != "" {
		sess.ThinkingLevel = meta.ThinkingLevel
	}
	if meta.ToolCallingMode != "" {
		sess.ToolCallingMode = meta.ToolCallingMode
	}
	if meta.ClientSessionID != "" {
		sess.ClientSessionID = meta.ClientSessionID
	}
	if meta.ProxyCodeMode != "" {
		sess.ProxyCodeMode = meta.ProxyCodeMode
	}
	if meta.ProxyCodeChatGeneration != 0 {
		sess.ProxyCodeChatGeneration = meta.ProxyCodeChatGeneration
	}
	if meta.MigratedFromProfileID != "" {
		sess.MigratedFromProfileID = meta.MigratedFromProfileID
	}
	if meta.MigrationReason != "" {
		sess.MigrationReason = meta.MigrationReason
	}
	if meta.MigrationCount != 0 {
		sess.MigrationCount = meta.MigrationCount
	}
}

// SessionMeta carries optional session metadata updates. Zero values are
// ignored so callers can perform partial updates.
type SessionMeta struct {
	ProfileID               string
	ConversationMode        string
	AistudioPromptID        string
	AistudioPromptURL       string
	AccountFingerprint      string
	LastModel               string
	SystemInstruction       string
	ThinkingLevel           string
	ToolCallingMode         string
	ClientSessionID         string
	ProxyCodeMode           string
	ProxyCodeChatGeneration int
	MigratedFromProfileID   string
	MigrationReason         string
	MigrationCount          int
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func timestampOf(sess *models.Session) int64 {
	for _, v := range []string{sess.UpdatedAt, sess.CreatedAt} {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

func sessionFieldString(sess *models.Session, field string) string {
	switch field {
	case "conversation_mode":
		return sess.ConversationMode
	case "profile_id":
		return sess.ProfileID
	case "last_model":
		return sess.LastModel
	case "system_instruction":
		return sess.SystemInstruction
	case "thinking_level":
		return sess.ThinkingLevel
	case "tool_calling_mode":
		return sess.ToolCallingMode
	case "aistudio_prompt_id":
		return sess.AistudioPromptID
	case "aistudio_prompt_url":
		return sess.AistudioPromptURL
	case "account_fingerprint":
		return sess.AccountFingerprint
	case "client_session_id":
		return sess.ClientSessionID
	case "proxycode_mode":
		return sess.ProxyCodeMode
	case "proxycode_chat_generation":
		if sess.ProxyCodeChatGeneration != 0 {
			return itoa(sess.ProxyCodeChatGeneration)
		}
	}
	return ""
}

func atoiSafe(s string) (int, error) {
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errInvalidInt
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

var errInvalidInt = &parseError{}

type parseError struct{}

func (e *parseError) Error() string { return "invalid integer" }
