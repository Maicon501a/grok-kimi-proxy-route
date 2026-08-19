package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"grok-desktop/internal/aistudio/converter"
	"grok-desktop/internal/aistudio/models"
)

func TestEnrichNativeToolReplayRestoresOpaqueFields(t *testing.T) {
	store := newNativeReplayStore("", nativeReplayRetention, time.Now)
	if err := store.Remember([]converter.FunctionCall{{
		ID:                             "call_42",
		Name:                           "get_current_temperature",
		Arguments:                      map[string]any{"location": "London"},
		AistudioNativeToken:            "opaque-token",
		AistudioNativeArgumentsPayload: json.RawMessage(`[[["location",[null,null,"London"]]]]`),
	}}, "profile-a"); err != nil {
		t.Fatalf("remember replay: %v", err)
	}
	client := &Client{nativeReplay: store}
	messages := []models.Message{{
		Role: "assistant",
		ToolCalls: []models.ToolCall{{
			ID:   "call_42",
			Type: "function",
			Function: models.FunctionCall{
				Name:      "get_current_temperature",
				Arguments: `{"location":"London"}`,
			},
		}},
	}}

	got := client.enrichNativeToolReplay(messages)
	call := got[0].ToolCalls[0]
	if call.AistudioNativeToken != "opaque-token" {
		t.Fatalf("opaque token was not restored: %#v", call)
	}
	if string(call.AistudioNativeArgumentsPayload) != `[[["location",[null,null,"London"]]]]` {
		t.Fatalf("native arguments payload was not restored: %s", call.AistudioNativeArgumentsPayload)
	}
	if messages[0].ToolCalls[0].AistudioNativeToken != "" {
		t.Fatal("input messages were mutated")
	}
}

func TestEnrichNativeToolReplaySurvivesProfileRotation(t *testing.T) {
	store := newNativeReplayStore("", nativeReplayRetention, time.Now)
	if err := store.Remember([]converter.FunctionCall{{
		ID:                             "call_rotated",
		Name:                           "read",
		Arguments:                      map[string]any{"filePath": "index.html"},
		AistudioNativeToken:            "opaque-thought-signature",
		AistudioNativeArgumentsPayload: json.RawMessage(`[[["filePath",[null,null,"index.html"]]]]`),
	}}, "profile-a"); err != nil {
		t.Fatalf("remember replay: %v", err)
	}
	destination := &Client{profileID: "profile-b", nativeReplay: store}
	messages := []models.Message{{
		Role: "assistant",
		ToolCalls: []models.ToolCall{{
			ID:   "call_rotated",
			Type: "function",
			Function: models.FunctionCall{
				Name:      "read",
				Arguments: `{"filePath":"index.html"}`,
			},
		}},
	}}

	got := destination.enrichNativeToolReplay(messages)
	call := got[0].ToolCalls[0]
	if call.AistudioNativeToken != "opaque-thought-signature" {
		t.Fatalf("rotated profile did not restore thought signature: %#v", call)
	}
	if len(call.AistudioNativeArgumentsPayload) == 0 {
		t.Fatal("rotated profile did not restore native arguments payload")
	}
}

func TestNativeReplayStoreSurvivesProcessRestart(t *testing.T) {
	now := time.Date(2026, 8, 14, 16, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "native-tool-replay.json")
	first := newNativeReplayStore(path, nativeReplayRetention, func() time.Time { return now })
	if err := first.Remember([]converter.FunctionCall{{
		ID:                             "call_restart",
		Name:                           "grep",
		Arguments:                      map[string]any{"pattern": "TODO"},
		AistudioNativeToken:            "restart-safe-signature",
		AistudioNativeArgumentsPayload: json.RawMessage(`[[["pattern",[null,null,"TODO"]]]]`),
	}}, "profile-a"); err != nil {
		t.Fatalf("remember replay: %v", err)
	}

	// Reopen immediately without Close: Remember persists synchronously, so an
	// abrupt process restart does not depend on a graceful flush.
	second := newNativeReplayStore(path, nativeReplayRetention, func() time.Time { return now.Add(time.Minute) })
	t.Cleanup(second.Close)
	got := second.Enrich(toolCallMessages("call_restart", "grep", `{"pattern":"TODO"}`))
	call := got[0].ToolCalls[0]
	if call.AistudioNativeToken != "restart-safe-signature" {
		t.Fatalf("persisted thought signature was not restored: %#v", call)
	}
	if len(call.AistudioNativeArgumentsPayload) == 0 {
		t.Fatal("persisted native arguments payload was not restored")
	}
}

func TestNativeReplayStoreDeletesEntriesOlderThanTenDays(t *testing.T) {
	current := time.Date(2026, 8, 14, 16, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "native-tool-replay.json")
	first := newNativeReplayStore(path, nativeReplayRetention, func() time.Time { return current.Add(-11 * 24 * time.Hour) })
	if err := first.Remember([]converter.FunctionCall{{
		ID:                  "call_expired",
		Name:                "read",
		Arguments:           map[string]any{},
		AistudioNativeToken: "expired-signature",
	}}, "profile-a"); err != nil {
		t.Fatalf("remember replay: %v", err)
	}
	first.Close()

	second := newNativeReplayStore(path, nativeReplayRetention, func() time.Time { return current })
	got := second.Enrich(toolCallMessages("call_expired", "read", `{}`))
	if got[0].ToolCalls[0].AistudioNativeToken != "" {
		t.Fatal("entry older than ten days was restored")
	}
	second.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cleaned replay file: %v", err)
	}
	var data nativeReplayFile
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("cleaned replay file is invalid JSON: %v", err)
	}
	if len(data.Entries) != 0 {
		t.Fatalf("expired entries remained on disk: %#v", data.Entries)
	}
}

func TestNativeReplayStoreRecoversFromMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native-tool-replay.json")
	if err := os.WriteFile(path, []byte(`{"entries":`), 0o600); err != nil {
		t.Fatalf("write malformed fixture: %v", err)
	}
	store := NewNativeReplayStore(path)
	if err := store.Remember([]converter.FunctionCall{{
		ID:                  "call_after_corruption",
		Name:                "read",
		Arguments:           map[string]any{},
		AistudioNativeToken: "valid-signature",
	}}, "profile-a"); err != nil {
		t.Fatalf("remember replay after corruption: %v", err)
	}
	store.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recovered replay file: %v", err)
	}
	var data nativeReplayFile
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("recovered replay file is invalid JSON: %v", err)
	}
	found := false
	for _, entry := range data.Entries {
		if entry.CallID == "call_after_corruption" && entry.Token == "valid-signature" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("new replay entry was not persisted after corruption: %#v", data.Entries)
	}
}

func TestNativeReplayStoreRejectsSameIDWithDifferentArguments(t *testing.T) {
	store := newNativeReplayStore("", nativeReplayRetention, time.Now)
	if err := store.Remember([]converter.FunctionCall{{
		ID:                  "call_collision",
		Name:                "read",
		Arguments:           map[string]any{"filePath": "first.txt"},
		AistudioNativeToken: "signature-for-first",
	}}, "profile-a"); err != nil {
		t.Fatalf("remember replay: %v", err)
	}

	got := store.Enrich(toolCallMessages("call_collision", "read", `{"filePath":"second.txt"}`))
	if got[0].ToolCalls[0].AistudioNativeToken != "" {
		t.Fatal("replay metadata crossed arguments with the same call ID and name")
	}
}

func toolCallMessages(id, name, arguments string) []models.Message {
	return []models.Message{{
		Role: "assistant",
		ToolCalls: []models.ToolCall{{
			ID:   id,
			Type: "function",
			Function: models.FunctionCall{
				Name:      name,
				Arguments: arguments,
			},
		}},
	}}
}
