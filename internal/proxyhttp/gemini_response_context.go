package proxyhttp

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"grok-desktop/internal/aistudio/storage"
)

const (
	geminiResponseContextVersion   = 1
	geminiResponseContextRetention = 10 * 24 * time.Hour
	geminiResponseContextMax       = 10_000
)

type geminiResponseContextStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]geminiResponseContextEntry
	now     func() time.Time
}

type geminiResponseContextFile struct {
	Version int                                   `json:"version"`
	Entries map[string]geminiResponseContextEntry `json:"entries"`
}

type geminiResponseContextEntry struct {
	ResponseID string                       `json:"response_id"`
	Calls      []geminiResponseFunctionCall `json:"calls"`
	StoredAt   time.Time                    `json:"stored_at"`
}

type geminiResponseFunctionCall struct {
	CallID    string `json:"call_id"`
	ItemID    string `json:"item_id,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func newGeminiResponseContextStore(root string) *geminiResponseContextStore {
	path := ""
	if strings.TrimSpace(root) != "" {
		path = filepath.Join(root, "aistudio-proxy", "state", "gemini-responses-context.json")
	}
	s := &geminiResponseContextStore{
		path:    path,
		entries: map[string]geminiResponseContextEntry{},
		now:     time.Now,
	}
	s.load()
	return s
}

func (s *geminiResponseContextStore) load() {
	if s == nil || s.path == "" {
		return
	}
	raw, err := storage.ReadFile(s.path)
	if err != nil || len(raw) == 0 {
		return
	}
	var data geminiResponseContextFile
	if json.Unmarshal(raw, &data) != nil || data.Version != geminiResponseContextVersion || data.Entries == nil {
		return
	}
	s.entries = data.Entries
	s.pruneLocked(s.now().UTC())
}

func (s *geminiResponseContextStore) rememberResponse(response map[string]any) {
	if s == nil || response == nil {
		return
	}
	responseID := strings.TrimSpace(asString(response["id"]))
	if responseID == "" {
		return
	}
	output, _ := response["output"].([]any)
	calls := make([]geminiResponseFunctionCall, 0)
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok || strings.ToLower(strings.TrimSpace(asString(item["type"]))) != "function_call" {
			continue
		}
		name := strings.TrimSpace(asString(item["name"]))
		callID := firstNonEmpty(asString(item["call_id"]), asString(item["id"]))
		if name == "" || callID == "" {
			continue
		}
		args := asString(item["arguments"])
		if args == "" {
			args = "{}"
		}
		calls = append(calls, geminiResponseFunctionCall{
			CallID: callID, ItemID: asString(item["id"]), Name: name, Arguments: args,
		})
	}
	if len(calls) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.pruneLocked(now)
	s.entries[responseID] = geminiResponseContextEntry{ResponseID: responseID, Calls: calls, StoredAt: now}
	s.pruneOverflowLocked()
	s.saveLocked()
}

// enrichContinuation reconstructs assistant function_call items omitted by the
// canonical Responses continuation form:
// previous_response_id + input:[function_call_output]. The AI Studio runtime is
// stateless, so it needs both halves of the tool exchange in the copied request.
func (s *geminiResponseContextStore) enrichContinuation(body map[string]any) bool {
	if s == nil || body == nil {
		return false
	}
	previousID := strings.TrimSpace(asString(body["previous_response_id"]))
	if previousID == "" {
		return false
	}
	input, ok := body["input"].([]any)
	if !ok || len(input) == 0 {
		return false
	}

	s.mu.Lock()
	s.pruneLocked(s.now().UTC())
	entry, found := s.entries[previousID]
	s.mu.Unlock()
	if !found || len(entry.Calls) == 0 {
		return false
	}

	existingCalls := map[string]bool{}
	outputCalls := map[string]bool{}
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		typ := strings.ToLower(strings.TrimSpace(asString(item["type"])))
		callID := firstNonEmpty(asString(item["call_id"]), asString(item["id"]))
		switch typ {
		case "function_call", "custom_tool_call":
			existingCalls[callID] = true
		case "function_call_output", "custom_tool_call_output":
			outputCalls[callID] = true
		}
	}

	prefix := make([]any, 0, len(entry.Calls))
	for _, call := range entry.Calls {
		if !outputCalls[call.CallID] || existingCalls[call.CallID] {
			continue
		}
		itemID := call.ItemID
		if itemID == "" {
			itemID = call.CallID
		}
		prefix = append(prefix, map[string]any{
			"type":      "function_call",
			"id":        itemID,
			"call_id":   call.CallID,
			"name":      call.Name,
			"arguments": call.Arguments,
			"status":    "completed",
		})
	}
	if len(prefix) == 0 {
		return false
	}
	body["input"] = append(prefix, input...)
	return true
}

func (s *geminiResponseContextStore) pruneLocked(now time.Time) {
	for key, entry := range s.entries {
		if entry.StoredAt.IsZero() || entry.StoredAt.After(now.Add(5*time.Minute)) || now.Sub(entry.StoredAt) > geminiResponseContextRetention {
			delete(s.entries, key)
		}
	}
}

func (s *geminiResponseContextStore) pruneOverflowLocked() {
	for len(s.entries) > geminiResponseContextMax {
		oldestKey := ""
		var oldest time.Time
		for key, entry := range s.entries {
			if oldestKey == "" || entry.StoredAt.Before(oldest) {
				oldestKey, oldest = key, entry.StoredAt
			}
		}
		if oldestKey == "" {
			return
		}
		delete(s.entries, oldestKey)
	}
}

func (s *geminiResponseContextStore) saveLocked() {
	if s.path == "" {
		return
	}
	raw, err := json.MarshalIndent(geminiResponseContextFile{Version: geminiResponseContextVersion, Entries: s.entries}, "", "  ")
	if err == nil {
		_ = storage.WriteAtomic(s.path, raw)
	}
}
