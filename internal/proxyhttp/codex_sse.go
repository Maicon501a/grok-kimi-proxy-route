package proxyhttp

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const codexReasoningSignaturePrefix = "codex-reasoning:"

// completedResponseFromSSE aggregates the always-streaming Codex backend into
// a normal Responses object for downstream clients that requested stream=false.
func completedResponseFromSSE(r io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(io.LimitReader(r, 64<<20))
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
			Error    json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		switch event.Type {
		case "response.completed", "response.done":
			if len(event.Response) > 0 && string(event.Response) != "null" {
				return event.Response, nil
			}
		case "response.failed", "error":
			if len(event.Error) > 0 {
				return nil, fmt.Errorf("codex responses stream failed: %s", strings.TrimSpace(string(event.Error)))
			}
			return nil, fmt.Errorf("codex responses stream failed")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("codex responses stream ended without response.completed")
}

func writeOpenAIError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": "api_error"},
	})
}

func ensureResponsesInclude(body map[string]any, required string) {
	if body == nil || required == "" {
		return
	}
	var values []any
	switch raw := body["include"].(type) {
	case []any:
		values = append(values, raw...)
	case []string:
		for _, value := range raw {
			values = append(values, value)
		}
	case string:
		if raw != "" {
			values = append(values, raw)
		}
	}
	for _, value := range values {
		if text, ok := value.(string); ok && text == required {
			body["include"] = values
			return
		}
	}
	body["include"] = append(values, required)
}

func responsesReasoningItems(response map[string]any) []any {
	output, _ := response["output"].([]any)
	items := make([]any, 0)
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok || asString(item["type"]) != "reasoning" || asString(item["encrypted_content"]) == "" {
			continue
		}
		items = append(items, item)
	}
	return items
}

func codexReasoningSignature(item map[string]any) string {
	if asString(item["type"]) != "reasoning" || asString(item["encrypted_content"]) == "" {
		return ""
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return ""
	}
	return codexReasoningSignaturePrefix + base64.RawURLEncoding.EncodeToString(raw)
}

func reasoningItemFromSignature(signature string) (map[string]any, bool) {
	if !strings.HasPrefix(signature, codexReasoningSignaturePrefix) {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(signature, codexReasoningSignaturePrefix))
	if err != nil {
		return nil, false
	}
	var item map[string]any
	if json.Unmarshal(raw, &item) != nil || asString(item["type"]) != "reasoning" || asString(item["encrypted_content"]) == "" {
		return nil, false
	}
	return item, true
}

func reasoningSummaryText(item map[string]any) string {
	var parts []string
	if summary, ok := item["summary"].([]any); ok {
		for _, raw := range summary {
			if part, ok := raw.(map[string]any); ok {
				if text := asString(part["text"]); text != "" {
					parts = append(parts, text)
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}
