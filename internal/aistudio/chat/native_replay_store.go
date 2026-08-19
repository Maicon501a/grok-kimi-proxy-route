package chat

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"grok-desktop/internal/aistudio/converter"
	"grok-desktop/internal/aistudio/models"
	"grok-desktop/internal/aistudio/storage"
)

const (
	nativeReplayFileVersion = 1
	nativeReplayRetention   = 10 * 24 * time.Hour
	nativeReplayMaxEntries  = 10_000
	nativeReplayMaxFileSize = 64 << 20
	nativeReplayFutureSkew  = 5 * time.Minute
)

// NativeReplayStore persists the opaque metadata AI Studio requires when a
// native function call is replayed with its tool result. OpenAI-compatible
// streaming clients keep the call ID/name/arguments but discard Gemini's
// thought signature and exact native argument payload, so the provider keeps
// those two fields in this removable sidecar JSON file.
type NativeReplayStore struct {
	mu        sync.Mutex
	path      string
	entries   map[string]nativeReplayEntry
	now       func() time.Time
	retention time.Duration
}

type nativeReplayFile struct {
	Version int                          `json:"version"`
	Entries map[string]nativeReplayEntry `json:"entries"`
}

type nativeReplayEntry struct {
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	ArgumentsHash    string          `json:"arguments_hash"`
	Token            string          `json:"thought_signature"`
	ArgumentsPayload json.RawMessage `json:"native_arguments_payload,omitempty"`
	SourceProfileID  string          `json:"source_profile_id,omitempty"`
	StoredAt         time.Time       `json:"stored_at"`
}

// NewNativeReplayStore loads a replay sidecar. Missing or malformed files are
// treated as empty; deleting the JSON file fully resets this feature.
func NewNativeReplayStore(path string) *NativeReplayStore {
	return newNativeReplayStore(path, nativeReplayRetention, time.Now)
}

func newNativeReplayStore(path string, retention time.Duration, now func() time.Time) *NativeReplayStore {
	if now == nil {
		now = time.Now
	}
	s := &NativeReplayStore{
		path:      path,
		entries:   make(map[string]nativeReplayEntry),
		now:       now,
		retention: retention,
	}
	if path != "" {
		needsRewrite := s.load()
		if needsRewrite {
			_ = s.saveLocked()
		}
	}
	s.mu.Lock()
	changed := s.pruneLocked(now(), s.retention)
	if s.pruneOverflowLocked() {
		changed = true
	}
	if changed {
		_ = s.saveLocked()
	}
	s.mu.Unlock()
	return s
}

func (s *NativeReplayStore) load() bool {
	raw, err := storage.ReadFile(s.path)
	if err != nil {
		return true
	}
	if len(raw) == 0 {
		return false
	}
	if len(raw) > nativeReplayMaxFileSize {
		return true
	}
	var data nativeReplayFile
	if err := json.Unmarshal(raw, &data); err != nil || data.Version != nativeReplayFileVersion || data.Entries == nil {
		return true
	}
	s.entries = data.Entries
	return false
}

// Remember stores signatures and exact argument payloads from a successful AI
// Studio response. One process-wide store is shared by every account profile,
// allowing a signed tool loop to survive quota-driven profile rotation.
func (s *NativeReplayStore) Remember(calls []converter.FunctionCall, sourceProfileID string) error {
	if s == nil || len(calls) == 0 {
		return nil
	}
	now := s.now().UTC()
	s.mu.Lock()
	changed := s.pruneLocked(now, s.retention)
	for _, call := range calls {
		if call.ID == "" || call.AistudioNativeToken == "" {
			continue
		}
		argumentsHash := hashArguments(call.Arguments)
		key := replayEntryKey(call.ID, call.Name, argumentsHash)
		s.entries[key] = nativeReplayEntry{
			CallID:           call.ID,
			Name:             call.Name,
			ArgumentsHash:    argumentsHash,
			Token:            call.AistudioNativeToken,
			ArgumentsPayload: append(json.RawMessage(nil), call.AistudioNativeArgumentsPayload...),
			SourceProfileID:  sourceProfileID,
			StoredAt:         now,
		}
		changed = true
	}
	if s.pruneOverflowLocked() {
		changed = true
	}
	if changed {
		err := s.saveLocked()
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	return nil
}

// Enrich restores opaque replay fields without mutating the caller's messages.
func (s *NativeReplayStore) Enrich(messages []models.Message) []models.Message {
	if s == nil || len(messages) == 0 {
		return messages
	}
	s.mu.Lock()
	if s.pruneLocked(s.now(), s.retention) {
		_ = s.saveLocked()
	}
	if len(s.entries) == 0 {
		s.mu.Unlock()
		return messages
	}

	out := append([]models.Message(nil), messages...)
	changed := false
	for i := range out {
		if out[i].Role != "assistant" || len(out[i].ToolCalls) == 0 {
			continue
		}
		calls := append([]models.ToolCall(nil), out[i].ToolCalls...)
		messageChanged := false
		for j := range calls {
			argumentsHash := hashRawArguments(calls[j].Function.Arguments)
			key := replayEntryKey(calls[j].ID, calls[j].Function.Name, argumentsHash)
			entry, ok := s.entries[key]
			if !ok || entry.CallID != calls[j].ID || entry.Name != calls[j].Function.Name || entry.ArgumentsHash != argumentsHash {
				continue
			}
			if calls[j].AistudioNativeToken == "" {
				calls[j].AistudioNativeToken = entry.Token
				messageChanged = true
			}
			if len(calls[j].AistudioNativeArgumentsPayload) == 0 && len(entry.ArgumentsPayload) > 0 {
				calls[j].AistudioNativeArgumentsPayload = append(json.RawMessage(nil), entry.ArgumentsPayload...)
				messageChanged = true
			}
		}
		if messageChanged {
			out[i].ToolCalls = calls
			changed = true
		}
	}
	s.mu.Unlock()
	if !changed {
		return messages
	}
	return out
}

func (s *NativeReplayStore) pruneLocked(now time.Time, retention time.Duration) bool {
	changed := false
	for id, entry := range s.entries {
		if entry.StoredAt.IsZero() || entry.StoredAt.After(now.Add(nativeReplayFutureSkew)) || now.Sub(entry.StoredAt) > retention {
			delete(s.entries, id)
			changed = true
		}
	}
	return changed
}

func (s *NativeReplayStore) pruneOverflowLocked() bool {
	if len(s.entries) <= nativeReplayMaxEntries {
		return false
	}
	for len(s.entries) > nativeReplayMaxEntries {
		oldestKey := ""
		var oldest time.Time
		for key, entry := range s.entries {
			if oldestKey == "" || entry.StoredAt.Before(oldest) {
				oldestKey = key
				oldest = entry.StoredAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(s.entries, oldestKey)
	}
	return true
}

func (s *NativeReplayStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	data := nativeReplayFile{Version: nativeReplayFileVersion, Entries: s.entries}
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return storage.WriteAtomic(s.path, encoded)
}

func replayEntryKey(callID, name, argumentsHash string) string {
	sum := sha256.Sum256([]byte(callID + "\x00" + name + "\x00" + argumentsHash))
	return fmt.Sprintf("%x", sum[:])
}

func hashRawArguments(raw string) string {
	var value any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		value = strings.TrimSpace(raw)
	}
	return hashArguments(value)
}

func hashArguments(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		encoded = []byte(fmt.Sprintf("%v", value))
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:])
}

// Close exists for lifecycle symmetry. Writes are synchronous, so there is no
// buffered replay state left to flush.
func (s *NativeReplayStore) Close() {}
