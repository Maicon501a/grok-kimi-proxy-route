package converter

import (
	"reflect"
	"testing"

	"grok-desktop/internal/aistudio/models"
)

func TestEncodeNativeStructuredValueBooleanUsesBoolSlot(t *testing.T) {
	want := []any{nil, nil, nil, false}
	if got := encodeNativeStructuredValue(false); !reflect.DeepEqual(got, want) {
		t.Fatalf("boolean encoded in wrong protobuf Value slot: got %#v want %#v", got, want)
	}
}

func TestEncodeNativeToolResultPartSupportsBooleanFields(t *testing.T) {
	encoded := encodeNativeToolResultPart(models.ContentPart{
		Type:       "native_tool_result",
		Name:       "read",
		ToolCallID: "call_bool",
		Result:     []byte(`{"truncated":false}`),
	})

	payload, ok := encoded.([]any)
	if !ok || len(payload) != 12 {
		t.Fatalf("unexpected native tool result payload: %#v", encoded)
	}
	tuple, ok := payload[11].([]any)
	if !ok || len(tuple) != 3 {
		t.Fatalf("unexpected native tool result tuple: %#v", payload[11])
	}
	wrappedEntries, ok := tuple[1].([]any)
	if !ok || len(wrappedEntries) != 1 {
		t.Fatalf("unexpected native tool result entries: %#v", tuple[1])
	}
	entries, ok := wrappedEntries[0].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("unexpected native tool result field list: %#v", wrappedEntries[0])
	}
	field, ok := entries[0].([]any)
	if !ok || len(field) != 2 {
		t.Fatalf("unexpected native tool result field: %#v", entries[0])
	}
	want := []any{nil, nil, nil, false}
	if !reflect.DeepEqual(field[1], want) {
		t.Fatalf("boolean field encoded incorrectly: got %#v want %#v", field[1], want)
	}
}
