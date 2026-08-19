package proxyhttp

import (
	"strings"
	"testing"
)

func TestCompletedResponseFromSSE(t *testing.T) {
	raw, err := completedResponseFromSSE(strings.NewReader(
		"event: response.output_text.delta\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
			"event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[]}}\n\n" +
			"data: [DONE]\n\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != `{"id":"resp-1","output":[]}` {
		t.Fatalf("response=%s", got)
	}
}

func TestEnsureResponsesIncludePreservesValues(t *testing.T) {
	body := map[string]any{"include": []any{"file_search_call.results"}}
	ensureResponsesInclude(body, "reasoning.encrypted_content")
	values := body["include"].([]any)
	if len(values) != 2 || values[0] != "file_search_call.results" || values[1] != "reasoning.encrypted_content" {
		t.Fatalf("include=%#v", values)
	}
}
