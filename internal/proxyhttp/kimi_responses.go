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

// pipeKimiChatSSEToResponses translates Kimi (Moonshot) chat.completion SSE
// into OpenAI Responses SSE so Codex can consume K3 xHigh natively.
//
// Supports:
//   - reasoning_content  → response.reasoning_text.delta
//   - content            → response.output_text.delta
//   - tool_calls         → response.output_item.added / function_call_arguments.delta / output_item.done
//   - usage              → attached on response.completed
func pipeKimiChatSSEToResponses(w http.ResponseWriter, body io.Reader, model string) error {
	return pipeKimiChatSSEToResponsesContext(context.Background(), w, body, model)
}

// pipeKimiChatSSEToResponsesContext is the request-scoped variant used by the
// HTTP proxy. Cancelling the client request must stop the scanner immediately,
// otherwise a quiet upstream can keep a proxy goroutine alive indefinitely.
func pipeKimiChatSSEToResponsesContext(ctx context.Context, w http.ResponseWriter, body io.Reader, model string) error {
	flusher, _ := w.(http.Flusher)
	setSSEHeaders(w)
	w.WriteHeader(http.StatusOK)

	respID := "resp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	writeFailure := func(message, code string) {
		writeEvent("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id":     respID,
				"object": "response",
				"status": "failed",
				"error": map[string]any{
					"message": message,
					"type":    "upstream_error",
					"code":    code,
				},
			},
		})
		fmt.Fprintf(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}

	writeEvent("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":         respID,
			"object":     "response",
			"created_at": created,
			"model":      model,
			"status":     "in_progress",
		},
	})

	// Pending tool calls tracked by index.
	type toolCall struct {
		index     int
		id        string
		name      string
		arguments strings.Builder
		started   bool
	}
	pendingTools := map[int]*toolCall{}

	var lastUsage any
	var sawContent bool
	var sawToolCall bool
	var sawReasoning bool

	sc := bufio.NewScanner(newContextReader(ctx, body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

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

		// Detect mid-stream quota error (upstream sends 200 then an error payload).
		if isQuota, qmsg := isQuotaPayload(chunk); isQuota {
			writeFailure(qmsg, "quota_exhausted")
			return fmt.Errorf("sse quota error: %s", qmsg)
		}
		// Do not turn an upstream error into a successful but empty Responses
		// completion. Clients such as Zed otherwise report only
		// "empty_model_response" and lose Kimi's actual error text.
		if msg := sseErrorPayloadMessage(chunk); msg != "" {
			writeFailure(msg, "upstream_error")
			return fmt.Errorf("sse upstream error: %s", msg)
		}

		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			// Usage-only chunk (some providers send it as a separate line with empty choices).
			if u, ok := chunk["usage"]; ok {
				lastUsage = u
			}
			continue
		}

		ch, _ := choices[0].(map[string]any)
		delta, _ := ch["delta"].(map[string]any)
		if delta == nil {
			continue
		}

		// 1. Reasoning (K3 xHigh emits this heavily)
		if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
			sawReasoning = true
			writeEvent("response.reasoning_text.delta", map[string]any{
				"type":  "response.reasoning_text.delta",
				"delta": rc,
			})
		}

		// 2. Content
		if txt, ok := delta["content"].(string); ok && txt != "" {
			writeEvent("response.output_text.delta", map[string]any{
				"type":  "response.output_text.delta",
				"delta": txt,
			})
			sawContent = true
		}

		// 3. Tool calls
		if rawTCs, ok := delta["tool_calls"].([]any); ok && len(rawTCs) > 0 {
			for _, raw := range rawTCs {
				tc, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				idxF, _ := tc["index"].(float64)
				idx := int(idxF)

				t, exists := pendingTools[idx]
				if !exists {
					t = &toolCall{index: idx}
					pendingTools[idx] = t
				}

				// ID / type / name arrive on the first delta for this index.
				if id, ok := tc["id"].(string); ok && id != "" {
					t.id = id
				}
				if fn, ok := tc["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok && name != "" {
						t.name = name
					}
					if args, ok := fn["arguments"].(string); ok && args != "" {
						t.arguments.WriteString(args)
					}
				}

				// Emit output_item.added once we have enough metadata.
				if !t.started && t.id != "" && t.name != "" {
					args := t.arguments.String()
					if args == "" {
						args = "{}"
					}
					writeEvent("response.output_item.added", map[string]any{
						"type": "response.output_item.added",
						"item": sanitizeToolCall(map[string]any{
							"type":      "function_call",
							"call_id":   t.id,
							"id":        t.id,
							"name":      t.name,
							"arguments": args,
							"status":    "in_progress",
						}),
					})
					t.started = true
					sawToolCall = true
				}

				// Emit argument deltas (only if we already started and have new arg text).
				if t.started {
					if fn, ok := tc["function"].(map[string]any); ok {
						if args, ok := fn["arguments"].(string); ok && args != "" {
							writeEvent("response.function_call_arguments.delta", map[string]any{
								"type":    "response.function_call_arguments.delta",
								"item_id": t.id,
								"call_id": t.id,
								"delta":   args,
							})
						}
					}
				}
			}
		}

		// Finish reason + usage on the same final chunk.
		if finish, _ := ch["finish_reason"].(string); finish != "" {
			if u, ok := chunk["usage"]; ok {
				lastUsage = u
			}
		}
	}

	// Emit output_item.done for every started tool call.
	for _, t := range pendingTools {
		if !t.started {
			continue
		}
		args := t.arguments.String()
		if args == "" {
			args = "{}"
		}
		writeEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done",
			"item": sanitizeToolCall(map[string]any{
				"type":      "function_call",
				"call_id":   t.id,
				"id":        t.id,
				"name":      t.name,
				"arguments": args,
				"status":    "completed",
			}),
		})
	}

	if !sawContent && !sawToolCall {
		msg := "Kimi upstream ended without assistant content, tool calls, or an error payload."
		if sawReasoning {
			msg = "Kimi upstream sent reasoning but no final assistant content."
		}
		writeFailure(msg, "empty_upstream_response")
		return fmt.Errorf("sse empty response: %s", msg)
	}

	// Build output array for completed event.
	var output []any
	if sawContent {
		// We don't have the full accumulated text without buffering everything;
		// Codex mainly cares about the completed event shape + usage.
		output = append(output, map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": ""},
			},
		})
	}
	if sawToolCall {
		for _, t := range pendingTools {
			if !t.started {
				continue
			}
			args := t.arguments.String()
			if args == "" {
				args = "{}"
			}
			output = append(output, sanitizeToolCall(map[string]any{
				"type":      "function_call",
				"call_id":   t.id,
				"name":      t.name,
				"arguments": args,
				"status":    "completed",
			}))
		}
	}

	completedPayload := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":         respID,
			"object":     "response",
			"created_at": created,
			"model":      model,
			"status":     "completed",
			"output":     output,
		},
	}
	if lastUsage != nil {
		completedPayload["response"].(map[string]any)["usage"] = normalizeUsageToResponses(lastUsage)
	}
	writeEvent("response.completed", completedPayload)

	fmt.Fprintf(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}

	return sc.Err()
}

// sseErrorPayloadMessage extracts an upstream SSE error message even when it
// is not a quota issue. The caller forwards it as response.failed verbatim.
func sseErrorPayloadMessage(payload map[string]any) string {
	raw, exists := payload["error"]
	if !exists {
		return ""
	}
	if msg, ok := raw.(string); ok {
		return strings.TrimSpace(msg)
	}
	errObj, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	if msg, ok := errObj["message"].(string); ok && strings.TrimSpace(msg) != "" {
		return strings.TrimSpace(msg)
	}
	if msg, ok := payload["message"].(string); ok {
		return strings.TrimSpace(msg)
	}
	return ""
}

// normalizeUsageToResponses adapts chat.usage into Responses-style usage numbers.
func normalizeUsageToResponses(u any) map[string]any {
	m, ok := u.(map[string]any)
	if !ok {
		if raw, ok := u.(json.RawMessage); ok {
			_ = json.Unmarshal(raw, &m)
		}
	}
	if m == nil {
		return map[string]any{}
	}
	out := map[string]any{}

	if v, ok := m["prompt_tokens"]; ok {
		out["input_tokens"] = v
	} else if v, ok := m["input_tokens"]; ok {
		out["input_tokens"] = v
	}
	if v, ok := m["completion_tokens"]; ok {
		out["output_tokens"] = v
	} else if v, ok := m["output_tokens"]; ok {
		out["output_tokens"] = v
	}
	if v, ok := m["reasoning_tokens"]; ok {
		out["output_tokens"] = asInt64(out["output_tokens"]) + asInt64(v)
	}
	if v, ok := m["total_tokens"]; ok {
		out["total_tokens"] = v
	} else {
		out["total_tokens"] = asInt64(out["input_tokens"]) + asInt64(out["output_tokens"])
	}
	return out
}
