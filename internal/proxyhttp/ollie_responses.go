package proxyhttp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// pipeOllieChatSSEToResponses translates chat SSE into a minimal Responses SSE stream.
func pipeOllieChatSSEToResponses(w http.ResponseWriter, body io.Reader, model string) error {
	return pipeOllieChatSSEToResponsesContext(context.Background(), w, body, model)
}

func pipeOllieChatSSEToResponsesContext(ctx context.Context, w http.ResponseWriter, body io.Reader, model string) error {
	flusher, _ := w.(http.Flusher)
	setSSEHeaders(w)
	w.WriteHeader(http.StatusOK)

	id := "resp_" + uuid.NewString()
	created := time.Now().Unix()
	writeEvent := func(ev string, payload any) {
		b, _ := json.Marshal(payload)
		if ev != "" {
			fmt.Fprintf(w, "event: %s\n", ev)
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	writeEvent("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": id, "object": "response", "created_at": created, "model": model, "status": "in_progress",
		},
	})

	sc := bufio.NewScanner(newContextReader(ctx, body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var content strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		ch, _ := choices[0].(map[string]any)
		delta, _ := ch["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if t, ok := delta["content"].(string); ok && t != "" {
			content.WriteString(t)
			writeEvent("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "delta": t,
			})
		}
	}

	writeEvent("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": id, "object": "response", "created_at": created, "model": model, "status": "completed",
			"output": []any{
				map[string]any{
					"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": content.String()}},
				},
			},
		},
	})
	fmt.Fprintf(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	return sc.Err()
}
