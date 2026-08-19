package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"grok-desktop/internal/store"
)

type streamRoundTripper func(*http.Request) (*http.Response, error)

func (f streamRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestStreamChatCompletionsPreservesTTFTAndUsage(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	c := &Client{HTTP: &http.Client{Transport: streamRoundTripper(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatalf("request body: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				`data: {"id":"chat-1","model":"deepseek-v4-flash-free","choices":[{"delta":{"content":"Hel"}}]}` + "\n\n" +
					`data: {"id":"chat-1","model":"deepseek-v4-flash-free","choices":[{"delta":{"content":"lo"}}]}` + "\n\n" +
					`data: {"id":"chat-1","model":"deepseek-v4-flash-free","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n" +
					"data: [DONE]\n\n",
			)),
		}, nil
	})}}

	var content string
	var sawUsage, sawDone bool
	err := c.StreamChat(t.Context(), "", store.Settings{Provider: store.ProviderOpenCodeZen}, "Zen", "", ChatRequest{
		Model:    "opencode/deepseek-v4-flash-free",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	}, func(ev StreamEvent) {
		switch ev.Type {
		case "content":
			content += ev.Text
		case "usage":
			sawUsage = ev.Usage != nil && ev.Usage.TotalTokens == 5
		case "done":
			sawDone = true
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if content != "Hello" || !sawUsage || !sawDone {
		t.Fatalf("content=%q usage=%v done=%v", content, sawUsage, sawDone)
	}
	if gotAuth != "Bearer public" {
		t.Fatalf("authorization=%q", gotAuth)
	}
	if gotBody["model"] != "deepseek-v4-flash-free" {
		t.Fatalf("wire model=%v, want prefix-free Zen id", gotBody["model"])
	}
	if gotBody["stream"] != true {
		t.Fatalf("wire stream=%v", gotBody["stream"])
	}
}

func TestOpenCodeGoUsesConfiguredBearerAndStripsNamespace(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	var gotURL string
	c := &Client{HTTP: &http.Client{Transport: streamRoundTripper(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotURL = r.URL.String()
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatalf("request body: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})}}

	err := c.StreamChat(t.Context(), "go-key", store.Settings{Provider: store.ProviderOpenCodeGo}, "OpenCode Go", "", ChatRequest{
		Model: "opencode-go/big-pickle", Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	}, func(StreamEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer go-key" {
		t.Fatalf("authorization=%q", gotAuth)
	}
	if gotBody["model"] != "big-pickle" {
		t.Fatalf("wire model=%v", gotBody["model"])
	}
	if gotURL != "https://opencode.ai/zen/go/v1/chat/completions" {
		t.Fatalf("url=%q", gotURL)
	}
}

func TestCodexStreamPreservesEncryptedReasoningHistory(t *testing.T) {
	var gotBody map[string]any
	var gotAccept string
	c := &Client{HTTP: &http.Client{Transport: streamRoundTripper(func(r *http.Request) (*http.Response, error) {
		gotAccept = r.Header.Get("Accept")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatalf("request body: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				`data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs-new","encrypted_content":"cipher-new","summary":[]}}` + "\n\n" +
					`data: {"type":"response.completed","response":{"id":"resp-1","model":"gpt-5.6-sol","output":[{"type":"reasoning","id":"rs-new","encrypted_content":"cipher-new","summary":[]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}` + "\n\n",
			)),
		}, nil
	})}}
	oldReasoning := map[string]any{"type": "reasoning", "id": "rs-old", "encrypted_content": "cipher-old", "summary": []any{}}
	var reasoningEvents int
	err := c.StreamChat(t.Context(), "token", store.Settings{Provider: store.ProviderCodex}, "Codex", "", ChatRequest{
		Model: "codex/gpt-5.6-sol", ReasoningEffort: "high",
		Messages: []ChatMessage{
			{Role: "assistant", Content: "prior", ReasoningItems: []map[string]any{oldReasoning}},
			{Role: "user", Content: "next"},
		},
	}, func(event StreamEvent) {
		if event.Type == "reasoning_item" && event.Payload["encrypted_content"] == "cipher-new" {
			reasoningEvents++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAccept != "text/event-stream" || reasoningEvents != 1 {
		t.Fatalf("accept=%q reasoning_events=%d", gotAccept, reasoningEvents)
	}
	input, _ := gotBody["input"].([]any)
	foundOld := false
	for _, raw := range input {
		if item, ok := raw.(map[string]any); ok && item["encrypted_content"] == "cipher-old" {
			foundOld = true
		}
	}
	if !foundOld || gotBody["store"] != false || gotBody["parallel_tool_calls"] != false {
		t.Fatalf("Codex stateless body incomplete: %#v", gotBody)
	}
}

func TestSplitSystemInstructions(t *testing.T) {
	instructions, messages := splitSystemInstructions([]ChatMessage{
		{Role: "system", Content: "Proxy rules"},
		{Role: "system", Content: "Temporal context"},
		{Role: "user", Content: "Hello"},
	})
	if got, want := instructions, "Proxy rules\n\nTemporal context"; got != want {
		t.Fatalf("instructions = %q, want %q", got, want)
	}
	if len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("non-system messages = %#v", messages)
	}
}
