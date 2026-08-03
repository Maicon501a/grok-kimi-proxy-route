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
