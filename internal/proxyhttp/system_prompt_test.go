package proxyhttp

import "testing"

func TestInjectManagedSystemPromptIntoMessages(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": "hello"}}
	got := injectManagedSystemPromptIntoMessages(msgs, "Proxy rules")
	if len(got) != 2 {
		t.Fatalf("message count = %d, want 2", len(got))
	}
	first := got[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "Proxy rules" {
		t.Fatalf("unexpected injected message: %#v", first)
	}
}

func TestInjectManagedSystemPromptIntoResponsesPreservesInstructions(t *testing.T) {
	body := map[string]any{"instructions": "Client rules"}
	injectManagedSystemPromptIntoResponses(body, "Proxy rules")
	if got, want := body["instructions"], "Proxy rules\n\nClient rules"; got != want {
		t.Fatalf("instructions = %#v, want %q", got, want)
	}
}

func TestInjectManagedSystemPromptIntoAnthropicPreservesBlocks(t *testing.T) {
	req := map[string]any{"system": []any{map[string]any{"type": "text", "text": "Client rules"}}}
	injectManagedSystemPromptIntoAnthropic(req, "Proxy rules")
	blocks := req["system"].([]any)
	if len(blocks) != 2 || blocks[0].(map[string]any)["text"] != "Proxy rules" {
		t.Fatalf("unexpected system blocks: %#v", blocks)
	}
}
