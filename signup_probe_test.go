package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
)

type signupProbeTransport func(*http.Request) (*http.Response, error)

func (f signupProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestProbeXAIModelUsesGrok46Responses(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	client := upstream.New()
	client.HTTP = &http.Client{Transport: signupProbeTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/responses" {
			t.Fatalf("path=%s", req.URL.Path)
		}
		if req.Header.Get("Authorization") != "Bearer access-test" {
			t.Fatalf("authorization=%q", req.Header.Get("Authorization"))
		}
		raw, _ := io.ReadAll(req.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "grok-4.6" || body["input"] != "Reply with exactly: GROK_4_6_OK" {
			t.Fatalf("body=%s", raw)
		}
		sse := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-46\",\"model\":\"grok-4.6\"}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"GROK_4_6_OK\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-46\",\"model\":\"grok-4.6\",\"usage\":{\"input_tokens\":5,\"output_tokens\":4,\"total_tokens\":9}}}\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	})}
	app := &App{store: st, upstream: client}
	answer, err := app.probeXAIModel(t.Context(), &store.Account{
		ID: "new-xai", Label: "New xAI", Email: "new@example.com", AccessToken: "access-test",
	}, "grok-4.6")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "GROK_4_6_OK" {
		t.Fatalf("answer=%q", answer)
	}
}
