package proxyhttp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	"grok-desktop/internal/pricing"
	"grok-desktop/internal/store"
)

// recordUsageFromJSONBody parses OpenAI chat/completions or responses JSON and persists usage/cost.
func (s *Server) recordUsageFromJSONBody(raw []byte, accountID, provider, model string, latencyMs int64) {
	if s == nil || s.store == nil || len(raw) == 0 {
		return
	}
	prompt, completion, reasoning, cached, total, ok := extractUsageFromOpenAIBody(raw)
	if !ok {
		return
	}
	if model == "" {
		model = extractModelFromBody(raw)
	}
	s.persistUsage(accountID, provider, model, prompt, completion, reasoning, cached, total, latencyMs, false)
}

// usageTeeReader copies SSE to the client while capturing the last usage payload.
type usageTeeReader struct {
	r        io.Reader
	sc       *bufio.Reader
	buf      bytes.Buffer
	lastJSON []byte
	done     bool
}

func newUsageTeeReader(r io.Reader) *usageTeeReader {
	return &usageTeeReader{r: r, sc: bufio.NewReaderSize(r, 64*1024)}
}

func (t *usageTeeReader) Read(p []byte) (int, error) {
	// Fill from underlying into buf if needed
	if t.buf.Len() == 0 && !t.done {
		line, err := t.sc.ReadBytes('\n')
		if len(line) > 0 {
			t.inspectSSELine(line)
			t.buf.Write(line)
		}
		if err != nil {
			if err == io.EOF {
				t.done = true
				if t.buf.Len() == 0 {
					return 0, io.EOF
				}
			} else if t.buf.Len() == 0 {
				return 0, err
			}
		}
	}
	return t.buf.Read(p)
}

func (t *usageTeeReader) inspectSSELine(line []byte) {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return
	}
	data := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if data == "" || data == "[DONE]" {
		return
	}
	if !strings.Contains(data, "usage") {
		return
	}
	// Keep last JSON chunk that mentions usage (chat SSE final chunk or responses completed).
	t.lastJSON = append([]byte(nil), []byte(data)...)
}

func (s *Server) recordUsageFromSSECapture(raw []byte, accountID, provider, model string, latencyMs int64) {
	if s == nil || s.store == nil || len(raw) == 0 {
		return
	}
	prompt, completion, reasoning, cached, total, ok := extractUsageFromOpenAIBody(raw)
	if !ok {
		// Some streams nest usage under response.usage
		prompt, completion, reasoning, cached, total, ok = extractUsageLoose(raw)
	}
	if !ok {
		return
	}
	if model == "" {
		model = extractModelFromBody(raw)
	}
	s.persistUsage(accountID, provider, model, prompt, completion, reasoning, cached, total, latencyMs, false)
}

func (s *Server) persistUsage(accountID, provider, model string, prompt, completion, reasoning, cached, total, latencyMs int64, estimated bool) {
	if total == 0 {
		total = prompt + completion + reasoning
	}
	if prompt == 0 && completion == 0 && reasoning == 0 {
		return
	}
	cost := pricing.CostUSD(model, provider, prompt, completion, reasoning, cached)
	sample := store.RequestSample{
		ID:               "req_" + uuid.NewString(),
		At:               time.Now().UTC().Format(time.RFC3339),
		AccountID:        accountID,
		Model:            model,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		ReasoningTokens:  reasoning,
		CachedTokens:     cached,
		TotalTokens:      total,
		CostUSD:          cost,
		LatencyMs:        latencyMs,
		Estimated:        estimated,
	}
	if err := s.store.RecordRequest(sample); err != nil {
		log.Printf("proxyhttp: record usage: %v", err)
	}
}

func extractModelFromBody(raw []byte) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	if s, ok := m["model"].(string); ok {
		return s
	}
	if resp, ok := m["response"].(map[string]any); ok {
		if s, ok := resp["model"].(string); ok {
			return s
		}
	}
	return ""
}

func extractUsageFromOpenAIBody(raw []byte) (prompt, completion, reasoning, cached, total int64, ok bool) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	// chat.completion: usage at top level
	if u, okU := m["usage"].(map[string]any); okU {
		return parseUsageMap(u)
	}
	// responses: usage at top or under response
	if resp, okR := m["response"].(map[string]any); okR {
		if u, okU := resp["usage"].(map[string]any); okU {
			return parseUsageMap(u)
		}
	}
	// SSE event types sometimes wrap differently
	if u, okU := m["response"].(map[string]any); okU {
		_ = u
	}
	return extractUsageLoose(raw)
}

func extractUsageLoose(raw []byte) (prompt, completion, reasoning, cached, total int64, ok bool) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	// walk one level for "usage"
	if u, okU := findUsageMap(m); okU {
		return parseUsageMap(u)
	}
	return
}

func findUsageMap(m map[string]any) (map[string]any, bool) {
	if u, ok := m["usage"].(map[string]any); ok {
		return u, true
	}
	for _, v := range m {
		if sub, ok := v.(map[string]any); ok {
			if u, ok := sub["usage"].(map[string]any); ok {
				return u, true
			}
		}
	}
	return nil, false
}

func parseUsageMap(u map[string]any) (prompt, completion, reasoning, cached, total int64, ok bool) {
	prompt = asInt64(u["prompt_tokens"])
	if prompt == 0 {
		prompt = asInt64(u["input_tokens"])
	}
	completion = asInt64(u["completion_tokens"])
	if completion == 0 {
		completion = asInt64(u["output_tokens"])
	}
	total = asInt64(u["total_tokens"])
	// details
	if d, okD := u["completion_tokens_details"].(map[string]any); okD {
		reasoning = asInt64(d["reasoning_tokens"])
	}
	if d, okD := u["output_tokens_details"].(map[string]any); okD {
		if reasoning == 0 {
			reasoning = asInt64(d["reasoning_tokens"])
		}
	}
	if d, okD := u["prompt_tokens_details"].(map[string]any); okD {
		cached = asInt64(d["cached_tokens"])
	}
	if d, okD := u["input_tokens_details"].(map[string]any); okD {
		if cached == 0 {
			cached = asInt64(d["cached_tokens"])
		}
	}
	if cached == 0 {
		cached = asInt64(u["cached_tokens"])
	}
	if reasoning == 0 {
		reasoning = asInt64(u["reasoning_tokens"])
	}
	ok = prompt > 0 || completion > 0 || reasoning > 0 || total > 0
	return
}
