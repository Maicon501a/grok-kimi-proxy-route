package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"grok-desktop/internal/gemini"
	"grok-desktop/internal/httperr"
	"grok-desktop/internal/logging"
	"grok-desktop/internal/store"
)

type Client struct {
	HTTP *http.Client
}

func New() *Client {
	return &Client{
		HTTP: &http.Client{
			Timeout: 0, // streaming
		},
	}
}

type ChatRequest struct {
	Model              string        `json:"model"`
	Messages           []ChatMessage `json:"messages"`
	Input              string        `json:"input,omitempty"`
	Stream             bool          `json:"stream"`
	ReasoningEffort    string        `json:"reasoning_effort"`
	PreviousResponseID string        `json:"previous_response_id"`
	LastResponseID     string        `json:"last_response_id"`
	APIMode            string        `json:"api_mode"` // chat | responses
	Temperature        float64       `json:"temperature"`
	MaxTokens          int           `json:"max_tokens"`
	// WebSearch is legacy (ignored). Native xAI web_search/x_search run server-side on Responses.
	WebSearch   bool   `json:"web_search"`
	SearchQuery string `json:"search_query,omitempty"`
}

type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

type StreamEvent struct {
	Type      string `json:"type"` // thinking | content | usage | done | error | meta | tool_* | search_*
	Text      string `json:"text,omitempty"`
	Error     string `json:"error,omitempty"`
	Usage     *Usage `json:"usage,omitempty"`
	Model     string `json:"model,omitempty"`
	ID        string `json:"id,omitempty"`
	Account   string `json:"account,omitempty"`
	Email     string `json:"email,omitempty"`
	LatencyMs int64  `json:"latency_ms,omitempty"`
	TTFTMs    int64  `json:"ttft_ms,omitempty"`
	Estimated bool   `json:"estimated,omitempty"`
	// Payload carries structured search/tool data for the UI.
	Payload map[string]any `json:"payload,omitempty"`
}

type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type ModelInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	APIMode     string `json:"api_mode,omitempty"`
	Root        string `json:"root,omitempty"`
}

func (c *Client) baseURL(s store.Settings) string {
	return s.EffectiveUpstream()
}

func (c *Client) authHeaders(token, version string, settings store.Settings) http.Header {
	h := make(http.Header)
	if token == "" && settings.IsOllie() {
		token = store.OllieAPIKey
	}
	if token == "" && settings.IsQwen() {
		token = strings.TrimSpace(settings.QwenAPIKey)
	}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream, application/json")
	if version == "" {
		version = store.DefaultClientVersion
	}
	switch {
	case settings.IsQwen():
		// QwenBridge: bearer only, no provider-specific headers.
	case settings.IsOllie():
		h.Set("User-Agent", "grok-desktop-ollie/"+version)
	case settings.IsKimiWork():
		h.Set("User-Agent", store.KimiWorkUserAgent)
		h.Set("X-Msh-Platform", "kimi-code-cli")
		h.Set("X-Msh-Version", "0.23.5")
	default:
		// Match official Grok CLI headers so cli-chat-proxy accepts the session.
		h.Set("x-grok-client-version", version)
		h.Set("x-grok-client-surface", "grok-cli")
		h.Set("User-Agent", "grok/"+version)
	}
	return h
}

func (c *Client) ListModels(ctx context.Context, token string, settings store.Settings) ([]ModelInfo, error) {
	if settings.IsGemini() {
		ids := gemini.ListModels(ctx, settings)
		out := make([]ModelInfo, 0, len(ids))
		for _, id := range ids {
			out = append(out, ModelInfo{
				ID: id, Name: id, Description: "Vertex AI · ADC", APIMode: "chat",
			})
		}
		return out, nil
	}
	if settings.IsKimiWork() {
		// UI may show "responses" preference, but upstream is chat/completions only.
		return []ModelInfo{
			{ID: "k3-agent", Name: "K3 Max (Work)", Description: "agent-gw · chat/completions · K3 rates", APIMode: "chat"},
			{ID: "k3-agent-low", Name: "K3 Max — Low Think", Description: "Desktop K3 · low reasoning effort", APIMode: "chat"},
			{ID: "k3-agent-medium", Name: "K3 Max — Medium Think", Description: "Desktop K3 · medium reasoning effort", APIMode: "chat"},
			{ID: "k3-agent-high", Name: "K3 Max — High Think", Description: "Desktop K3 · high reasoning effort", APIMode: "chat"},
			{ID: "k3-agent-xhigh", Name: "K3 Max — Extra High Think", Description: "Desktop K3 · xhigh reasoning effort", APIMode: "chat"},
			{ID: "k2d6-agent", Name: "K2.6 Agent (Work)", Description: "agent-gw · chat/completions · K2.6 rates", APIMode: "chat"},
			{ID: "k2d6-agent-low", Name: "K2.6 Agent — Low Think", Description: "Desktop K2.6 · low reasoning effort", APIMode: "chat"},
			{ID: "k2d6-agent-medium", Name: "K2.6 Agent — Medium Think", Description: "Desktop K2.6 · medium reasoning effort", APIMode: "chat"},
			{ID: "k2d6-agent-high", Name: "K2.6 Agent — High Think", Description: "Desktop K2.6 · high reasoning effort", APIMode: "chat"},
			{ID: "k2d6-agent-xhigh", Name: "K2.6 Agent — Extra High Think", Description: "Desktop K2.6 · xhigh reasoning effort", APIMode: "chat"},
		}, nil
	}
	if settings.IsOllie() {
		return c.listOllieModels(ctx, token, settings)
	}
	if settings.IsQwen() {
		return c.listQwenModels(ctx, token, settings)
	}
	url := c.baseURL(settings) + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.authHeaders(token, settings.ClientVersion, settings)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s", httperr.Format("models", resp.StatusCode, resp.Header.Get("Content-Type"), b))
	}
	var parsed struct {
		Data []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(parsed.Data)*2)
	for _, m := range parsed.Data {
		out = append(out, ModelInfo{
			ID: m.ID, Name: m.Name, Description: m.Description, APIMode: "chat",
		})
		out = append(out, ModelInfo{
			ID: m.ID + "-responses", Name: m.Name + " (Responses)",
			Description: m.Description + " — multi-turn token saving",
			APIMode:     "responses", Root: m.ID,
		})
	}
	return out, nil
}

// listOllieModels fetches /v1/models and also surfaces short public-config aliases.
func (c *Client) listOllieModels(ctx context.Context, token string, settings store.Settings) ([]ModelInfo, error) {
	url := c.baseURL(settings) + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.authHeaders(token, settings.ClientVersion, settings)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return ollieFallbackModels(), fmt.Errorf("models HTTP %d: %s", resp.StatusCode, string(b))
	}
	var parsed struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	_ = json.Unmarshal(b, &parsed)
	seen := map[string]bool{}
	out := make([]ModelInfo, 0, len(parsed.Data)+16)
	for _, m := range parsed.Data {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		name := m.Name
		if name == "" {
			name = shortModelName(m.ID)
		}
		out = append(out, ModelInfo{
			ID: m.ID, Name: name, Description: "OllieChat · free keyless", APIMode: "chat",
		})
		// Also expose short id when full path is long.
		if short := shortModelName(m.ID); short != m.ID && !seen[short] {
			seen[short] = true
			out = append(out, ModelInfo{
				ID: short, Name: short, Description: "OllieChat alias → " + m.ID, APIMode: "chat", Root: m.ID,
			})
		}
	}
	if len(out) == 0 {
		return ollieFallbackModels(), nil
	}
	return out, nil
}

func shortModelName(id string) string {
	// accounts/euromodels/models/claude-sonnet-5 → claude-sonnet-5
	if i := strings.LastIndex(id, "/"); i >= 0 && i+1 < len(id) {
		return id[i+1:]
	}
	return id
}

// listQwenModels fetches the dynamic model list from the local QwenBridge.
// Falls back to a minimal static list when the bridge errors or returns nothing.
func (c *Client) listQwenModels(ctx context.Context, token string, settings store.Settings) ([]ModelInfo, error) {
	url := c.baseURL(settings) + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.authHeaders(token, settings.ClientVersion, settings)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return qwenFallbackModels(), err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return qwenFallbackModels(), fmt.Errorf("models HTTP %d: %s", resp.StatusCode, string(b))
	}
	var parsed struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	_ = json.Unmarshal(b, &parsed)
	seen := map[string]bool{}
	out := make([]ModelInfo, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		name := m.Name
		if name == "" {
			name = m.ID
		}
		out = append(out, ModelInfo{
			ID: m.ID, Name: name, Description: "QwenBridge · local", APIMode: "chat",
		})
	}
	if len(out) == 0 {
		return qwenFallbackModels(), nil
	}
	return out, nil
}

func qwenFallbackModels() []ModelInfo {
	return []ModelInfo{
		{ID: store.QwenDefaultModel, Name: store.QwenDefaultModel, Description: "QwenBridge · local", APIMode: "chat"},
	}
}

func ollieFallbackModels() []ModelInfo {
	ids := []string{
		"claude-sonnet-5", "claude-opus-4-8", "claude-fable-5",
		"gpt-5.5", "gpt-5.6-luna",
		"deepseek-v4-pro", "deepseek-v4-flash-free",
		"qwen-3.7-plus", "kimi-k2.7-code", "minimax-m3",
		"glm-5.2", "glm-5.2-fast", "mimo-v2.5-free",
		"agnes-2.0-flash", "agnes-1.5-flash",
		"nemotron-3-ultra-free", "north-mini-code-free", "big-pickle",
	}
	out := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, ModelInfo{
			ID: id, Name: id, Description: "OllieChat · free keyless", APIMode: "chat",
		})
	}
	return out
}

func stripResponsesSuffix(model string) (string, bool) {
	m := strings.TrimSpace(model)
	low := strings.ToLower(m)
	switch {
	case strings.HasSuffix(low, "-responses"):
		return m[:len(m)-len("-responses")], true
	case strings.HasSuffix(low, "@responses"):
		return m[:len(m)-len("@responses")], true
	case strings.HasSuffix(low, "/responses"):
		return m[:len(m)-len("/responses")], true
	case strings.HasPrefix(low, "responses/"):
		return m[len("responses/"):], true
	default:
		return m, false
	}
}

func extractPrevID(req ChatRequest) string {
	for _, v := range []string{req.PreviousResponseID, req.LastResponseID} {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// StreamChat emits thinking/content/usage/done events.
func (c *Client) StreamChat(
	ctx context.Context,
	token string,
	settings store.Settings,
	accountLabel string,
	accountEmail string,
	req ChatRequest,
	emit func(StreamEvent),
) error {
	model := settings.ResolveModel(req.Model)
	// Use only what the caller put on the request. Desktop app chat fills
	// ReasoningEffort from UI settings in app.go; HTTP proxy never does.
	effort := req.ReasoningEffort
	emit(StreamEvent{
		Type:    "meta",
		Account: accountLabel,
		Email:   accountEmail,
		Model:   model,
	})

	// Inject current date/year so the model knows the present (e.g. 2026).
	if len(req.Messages) > 0 {
		req.Messages = ensureTemporalContext(append([]ChatMessage{}, req.Messages...))
	}

	prov := settings.NormalizedProvider()
	logging.Info("upstream.stream.start", "provider", prov, "model", model, "account_id", accountLabel)
	streamT0 := time.Now()

	// Gemini: Vertex generateContent via ADC (never hit xAI /responses).
	var streamErr error
	if settings.IsGemini() {
		streamErr = c.streamGemini(ctx, settings, model, req, emit)
	} else if settings.IsKimiWork() || settings.IsOllie() || settings.IsQwen() || strings.EqualFold(req.APIMode, "chat") {
		// OllieChat (and explicit chat mode): OpenAI chat/completions.
		// Kimi Work coding gateway only exposes /chat/completions (no /responses).
		// Ollie is chat-only. QwenBridge is wired chat-only.
		if settings.IsKimiWork() {
			model = resolveKimiUpstreamModel(model)
		}
		streamErr = c.streamChatCompletions(ctx, token, settings, model, effort, req, emit)
	} else {
		streamErr = c.streamResponses(ctx, token, settings, model, effort, req, emit)
	}
	if streamErr != nil {
		logging.Error("upstream.stream.error", "provider", prov, "model", model, "account_id", accountLabel, "err", streamErr.Error(), "duration_ms", time.Since(streamT0).Milliseconds())
		return streamErr
	}
	logging.Info("upstream.stream.done", "provider", prov, "model", model, "account_id", accountLabel, "duration_ms", time.Since(streamT0).Milliseconds())
	return nil
}

func resolveKimiUpstreamModel(model string) string {
	// Honor real agent-gw model ids (k3-agent, k2d6-agent, k2p6). Do not collapse
	// everything to the legacy "kimi-for-coding" brand string.
	return store.Settings{Provider: store.ProviderKimiWork}.ResolveModelForClient(model)
}

// applyKimiThinkingBody maps client effort into agent-gw `thinking` object.
// Desktop wire (observed): thinking: { type: "enabled"|"disabled", effort?: string, keep?: "all" }.
// effort is only set for known levels; empty effort → no thinking field (client omitted).
func applyKimiThinkingBody(body map[string]any, effort string) {
	if body == nil {
		return
	}
	// If client already sent a thinking object, leave it (and drop openAI-only effort).
	if th, ok := body["thinking"].(map[string]any); ok && th != nil {
		delete(body, "reasoning_effort")
		return
	}
	eff := strings.ToLower(strings.TrimSpace(effort))
	switch eff {
	case "":
		// Client sent nothing — do not invent global defaults.
		delete(body, "reasoning_effort")
		return
	case "off", "none", "disabled":
		body["thinking"] = map[string]any{"type": "disabled"}
		delete(body, "reasoning_effort")
	case "low", "medium", "high", "xhigh", "max", "minimal":
		// Map OpenAI-ish names to desktop levels. xhigh→max (desktop catalog).
		if eff == "xhigh" || eff == "extra_high" || eff == "extra-high" {
			eff = "max"
		}
		if eff == "minimal" {
			eff = "low"
		}
		body["thinking"] = map[string]any{
			"type":   "enabled",
			"effort": eff,
			"keep":   "all",
		}
		delete(body, "reasoning_effort")
	default:
		// Unknown string: enable thinking without effort (desktop does this when
		// supportEfforts is empty).
		body["thinking"] = map[string]any{"type": "enabled", "keep": "all"}
		delete(body, "reasoning_effort")
	}
}

// streamGemini talks to Vertex AI with ADC — used by desktop SendChat.
func (c *Client) streamGemini(
	ctx context.Context,
	settings store.Settings,
	model string,
	req ChatRequest,
	emit func(StreamEvent),
) error {
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		entry := map[string]any{"role": m.Role, "content": m.Content}
		if m.Name != "" {
			entry["name"] = m.Name
		}
		msgs = append(msgs, entry)
	}
	gc := gemini.New()
	if c.HTTP != nil {
		gc.HTTP = c.HTTP
	}
	// Client/request effort only — no global settings fallback.
	effort := req.ReasoningEffort
	t0 := time.Now()
	var ttftMs int64
	var contentLen, thinkLen int
	err := gc.StreamEvents(ctx, settings, model, msgs, func(kind, text string) {
		if text == "" {
			return
		}
		if ttftMs == 0 {
			ttftMs = time.Since(t0).Milliseconds()
		}
		switch kind {
		case "thinking":
			thinkLen += len(text)
			emit(StreamEvent{Type: "thinking", Text: text, Model: model})
		default:
			contentLen += len(text)
			emit(StreamEvent{Type: "content", Text: text, Model: model})
		}
	}, effort)
	if err != nil {
		return err
	}
	lat := time.Since(t0).Milliseconds()
	usage := &Usage{
		PromptTokens:     estimatePromptTokens(req.Messages),
		CompletionTokens: int64((contentLen + 3) / 4),
		ReasoningTokens:  int64((thinkLen + 3) / 4),
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens + usage.ReasoningTokens
	emit(StreamEvent{
		Type: "usage", Usage: usage, Model: model,
		LatencyMs: lat, TTFTMs: ttftMs, Estimated: true,
	})
	emit(StreamEvent{
		Type: "done", Model: model, Usage: usage,
		LatencyMs: lat, TTFTMs: ttftMs, Estimated: true,
	})
	return nil
}

func isQuotaPayload(payload map[string]any) (bool, string) {
	errObj, ok := payload["error"].(map[string]any)
	if !ok {
		if s, ok := payload["error"].(string); ok && s != "" {
			errObj = map[string]any{"message": s}
		} else {
			return false, ""
		}
	}
	msg := ""
	if m, ok := errObj["message"].(string); ok {
		msg = m
	} else if m, ok := payload["message"].(string); ok {
		msg = m
	}
	if msg == "" {
		return false, ""
	}
	low := strings.ToLower(msg)
	if strings.Contains(low, "usage limit") ||
		strings.Contains(low, "billing cycle") ||
		strings.Contains(low, "resource_exhausted") ||
		strings.Contains(low, "access_terminated") ||
		strings.Contains(low, "balance exhausted") ||
		strings.Contains(low, "quota exceeded") ||
		strings.Contains(low, "upgrade to get more") ||
		(strings.Contains(low, "rate limit exceeded") && (strings.Contains(low, "quota") || strings.Contains(low, "usage") || strings.Contains(low, "billing"))) {
		return true, msg
	}
	return false, ""
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := c.r.Read(p)
		done <- result{n, err}
	}()
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	case res := <-done:
		return res.n, res.err
	}
}

func newContextReader(ctx context.Context, r io.Reader) io.Reader {
	return &contextReader{ctx: ctx, r: r}
}

func (c *Client) streamChatCompletions(
	ctx context.Context,
	token string,
	settings store.Settings,
	model, effort string,
	req ChatRequest,
	emit func(StreamEvent),
) error {
	body := map[string]any{
		"model":    model,
		"messages": req.Messages,
		"stream":   true,
		// Critical: without this many providers omit usage on SSE streams
		"stream_options": map[string]any{"include_usage": true},
	}
	if settings.IsOllie() {
		// Free Ollie models: high/xhigh often burns the whole budget on thinking → empty content.
		eff := strings.ToLower(strings.TrimSpace(effort))
		switch eff {
		case "xhigh", "extra_high", "max":
			body["reasoning_effort"] = "high"
			if req.MaxTokens <= 0 {
				body["max_tokens"] = 4096
			}
		case "high", "medium", "low":
			body["reasoning_effort"] = eff
			if req.MaxTokens <= 0 {
				body["max_tokens"] = 2048
			}
		default:
			// omit effort — most reliable for free models
		}
	} else if effort != "" && !settings.IsKimiWork() {
		body["reasoning_effort"] = effort
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	} else if settings.IsOllie() {
		if _, ok := body["max_tokens"]; !ok {
			body["max_tokens"] = 4096
		}
	}
	if settings.IsKimiWork() {
		// Client-owned only: effort from request field and/or model alias (k3-agent-high).
		// Official Desktop agent-gw body uses thinking: {type, effort?, keep?} — not
		// OpenAI reasoning_effort alone (see KimiChatProvider.withThinking).
		_, modelEffort := store.ExtractKimiWorkEffort(model)
		if modelEffort != "" {
			effort = modelEffort
		}
		body["model"] = resolveKimiUpstreamModel(model)
		applyKimiThinkingBody(body, effort)
	}
	raw, _ := json.Marshal(body)
	url := c.baseURL(settings) + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	httpReq.Header = c.authHeaders(token, settings.ClientVersion, settings)

	t0 := time.Now()
	var ttftMs int64
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("%s", httperr.Format("chat", resp.StatusCode, resp.Header.Get("Content-Type"), b))
	}
	// Never stream HTML error pages as assistant content (Google robot 404 etc.).
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/html") {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s", httperr.Format("chat", resp.StatusCode, ct, b))
	}

	var usage *Usage
	var id, outModel string
	var contentLen, thinkLen int
	promptEst := estimatePromptTokens(req.Messages)
	sc := bufio.NewScanner(newContextReader(ctx, resp.Body))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			// Detect bare HTML bodies that slipped through without SSE framing.
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(strings.ToLower(trim), "<!doctype") || strings.HasPrefix(strings.ToLower(trim), "<html") {
				return fmt.Errorf("%s", httperr.Format("chat", resp.StatusCode, "text/html", []byte(trim)))
			}
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		// Detect mid-stream quota error (upstream may embed error inside SSE).
		if isQuota, qmsg := isQuotaPayload(chunk); isQuota {
			emit(StreamEvent{Type: "error", Error: qmsg, Model: outModel})
			emit(StreamEvent{Type: "done", Model: outModel})
			return fmt.Errorf("sse quota error: %s", qmsg)
		}
		if id == "" {
			if v, ok := chunk["id"].(string); ok {
				id = v
			}
		}
		if outModel == "" {
			if v, ok := chunk["model"].(string); ok {
				outModel = v
			}
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) > 0 {
			ch, _ := choices[0].(map[string]any)
			delta, _ := ch["delta"].(map[string]any)
			// Ollie uses reasoning / reasoning_content; OpenAI-style uses reasoning_content.
			for _, key := range []string{"reasoning_content", "reasoning"} {
				if rc, ok := delta[key].(string); ok && rc != "" {
					if ttftMs == 0 {
						ttftMs = time.Since(t0).Milliseconds()
					}
					thinkLen += len(rc)
					emit(StreamEvent{Type: "thinking", Text: rc, ID: id, Model: outModel})
				}
			}
			if ct, ok := delta["content"].(string); ok && ct != "" {
				if ttftMs == 0 {
					ttftMs = time.Since(t0).Milliseconds()
				}
				contentLen += len(ct)
				emit(StreamEvent{Type: "content", Text: ct, ID: id, Model: outModel})
			}
		}
		if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
			usage = parseChatUsage(u)
		}
	}
	lat := time.Since(t0).Milliseconds()
	estimated := false
	if usage == nil || (usage.PromptTokens == 0 && usage.CompletionTokens == 0) {
		// Fallback estimate ~4 chars/token
		usage = &Usage{
			PromptTokens:     promptEst,
			CompletionTokens: int64((contentLen + 3) / 4),
			ReasoningTokens:  int64((thinkLen + 3) / 4),
			TotalTokens:      0,
		}
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens + usage.ReasoningTokens
		estimated = true
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	emit(StreamEvent{
		Type: "usage", Usage: usage, ID: id, Model: outModel,
		LatencyMs: lat, TTFTMs: ttftMs, Estimated: estimated,
	})
	emit(StreamEvent{
		Type: "done", ID: id, Model: outModel, Usage: usage,
		LatencyMs: lat, TTFTMs: ttftMs, Estimated: estimated,
	})
	return sc.Err()
}

func (c *Client) streamResponses(
	ctx context.Context,
	token string,
	settings store.Settings,
	model, effort string,
	req ChatRequest,
	emit func(StreamEvent),
) error {
	prev := extractPrevID(req)
	var input any
	if strings.TrimSpace(req.Input) != "" {
		input = req.Input
	} else if len(req.Messages) > 0 {
		// only last user turn when chaining
		msgs := req.Messages
		if prev != "" {
			for i := len(msgs) - 1; i >= 0; i-- {
				if msgs[i].Role == "user" {
					msgs = []ChatMessage{msgs[i]}
					break
				}
			}
		}
		input = messagesToResponsesInput(msgs)
	} else {
		return fmt.Errorf("no input/messages")
	}

	body := map[string]any{
		"model":  model,
		"input":  input,
		"stream": true,
		"reasoning": map[string]any{
			"effort":  effort,
			"summary": "auto",
		},
		// Native xAI server-side search (replaces client-side DuckDuckGo).
		"tools": []map[string]any{
			{"type": "web_search"},
			{"type": "x_search"},
		},
		"tool_choice": "auto",
	}
	if settings.StoreResponses || prev != "" {
		body["store"] = true
	}
	if prev != "" {
		body["previous_response_id"] = prev
	}
	raw, _ := json.Marshal(body)
	url := c.baseURL(settings) + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	httpReq.Header = c.authHeaders(token, settings.ClientVersion, settings)

	t0 := time.Now()
	var ttftMs int64
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("%s", httperr.Format("responses", resp.StatusCode, resp.Header.Get("Content-Type"), b))
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/html") {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s", httperr.Format("responses", resp.StatusCode, ct, b))
	}

	var usage *Usage
	var id, outModel string
	var contentLen, thinkLen int
	// Track in-flight search items until output_item.done fills query/sources.
	pendingSearch := map[string]map[string]any{}
	promptEst := estimatePromptTokens(req.Messages)
	if strings.TrimSpace(req.Input) != "" {
		promptEst = int64((len(req.Input) + 3) / 4)
	}
	sc := bufio.NewScanner(newContextReader(ctx, resp.Body))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var eventName string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			continue
		}
		// Detect mid-stream quota error (upstream may embed error inside SSE).
		if isQuota, qmsg := isQuotaPayload(obj); isQuota {
			emit(StreamEvent{Type: "error", Error: qmsg, Model: outModel})
			emit(StreamEvent{Type: "done", Model: outModel})
			return fmt.Errorf("sse quota error: %s", qmsg)
		}
		typ, _ := obj["type"].(string)
		if typ == "" {
			typ = eventName
		}
		switch typ {
		case "response.created", "response.in_progress":
			if r, ok := obj["response"].(map[string]any); ok {
				if v, ok := r["id"].(string); ok {
					id = v
				}
				if v, ok := r["model"].(string); ok {
					outModel = v
				}
			}
		case "response.reasoning_summary_text.delta":
			if d, ok := obj["delta"].(string); ok && d != "" {
				if ttftMs == 0 {
					ttftMs = time.Since(t0).Milliseconds()
				}
				thinkLen += len(d)
				emit(StreamEvent{Type: "thinking", Text: d, ID: id, Model: outModel})
			}
		case "response.output_text.delta":
			if d, ok := obj["delta"].(string); ok && d != "" {
				if ttftMs == 0 {
					ttftMs = time.Since(t0).Milliseconds()
				}
				contentLen += len(d)
				emit(StreamEvent{Type: "content", Text: d, ID: id, Model: outModel})
			}
		case "response.output_item.added":
			if item, ok := obj["item"].(map[string]any); ok {
				handleSearchItemStart(item, id, outModel, pendingSearch, emit)
			}
		case "response.web_search_call.in_progress", "response.web_search_call.searching":
			itemID := strField(obj["item_id"])
			if itemID != "" {
				if _, ok := pendingSearch[itemID]; !ok {
					pendingSearch[itemID] = map[string]any{"kind": "web", "query": ""}
				}
				emit(StreamEvent{
					Type: "search_query", ID: itemID, Model: outModel, Text: strField(pendingSearch[itemID]["query"]),
					Payload: map[string]any{"provider": "xAI", "kind": "web", "status": "searching"},
				})
			}
		case "response.output_item.done":
			if item, ok := obj["item"].(map[string]any); ok {
				handleSearchItemDone(item, id, outModel, pendingSearch, t0, emit)
			}
		case "response.output_text.annotation.added":
			if ann, ok := obj["annotation"].(map[string]any); ok {
				emit(StreamEvent{
					Type:  "citation",
					ID:    id,
					Model: outModel,
					Text:  strField(ann["url"]),
					Payload: map[string]any{
						"url":   strField(ann["url"]),
						"title": strField(ann["title"]),
						"type":  strField(ann["type"]),
					},
				})
			}
		case "response.completed":
			if r, ok := obj["response"].(map[string]any); ok {
				if v, ok := r["id"].(string); ok {
					id = v
				}
				if v, ok := r["model"].(string); ok {
					outModel = v
				}
				if u, ok := r["usage"].(map[string]any); ok {
					usage = parseResponsesUsage(u)
				}
			}
		}
		eventName = ""
	}
	lat := time.Since(t0).Milliseconds()
	estimated := false
	if usage == nil || (usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0) {
		usage = &Usage{
			PromptTokens:     promptEst,
			CompletionTokens: int64((contentLen + 3) / 4),
			ReasoningTokens:  int64((thinkLen + 3) / 4),
		}
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens + usage.ReasoningTokens
		estimated = true
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	emit(StreamEvent{
		Type: "usage", Usage: usage, ID: id, Model: outModel,
		LatencyMs: lat, TTFTMs: ttftMs, Estimated: estimated,
	})
	emit(StreamEvent{
		Type: "done", ID: id, Model: outModel, Usage: usage,
		LatencyMs: lat, TTFTMs: ttftMs, Estimated: estimated,
	})
	return sc.Err()
}

func messagesToResponsesInput(msgs []ChatMessage) any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		role := m.Role
		if role == "system" {
			role = "user"
		}
		partType := "input_text"
		if m.Role == "assistant" {
			partType = "output_text"
		}
		out = append(out, map[string]any{
			"role": role,
			"content": []map[string]any{
				{"type": partType, "text": m.Content},
			},
		})
	}
	return out
}

func searchKindFromItem(item map[string]any) string {
	t := strings.ToLower(strField(item["type"]))
	name := strings.ToLower(strField(item["name"]))
	switch {
	case strings.Contains(t, "x_search") || strings.HasPrefix(name, "x_") || name == "x_search":
		return "x"
	case strings.Contains(t, "web_search") || name == "web_search":
		return "web"
	case t == "custom_tool_call" && (strings.Contains(name, "search") || strings.HasPrefix(name, "x_")):
		if strings.HasPrefix(name, "x_") {
			return "x"
		}
		return "web"
	default:
		return ""
	}
}

func extractSearchQuery(item map[string]any) string {
	if action, ok := item["action"].(map[string]any); ok {
		if q := strField(action["query"]); q != "" {
			return q
		}
	}
	if q := strField(item["query"]); q != "" {
		return q
	}
	// custom_tool_call input may be JSON string
	if in := strField(item["input"]); in != "" {
		var m map[string]any
		if json.Unmarshal([]byte(in), &m) == nil {
			if q, ok := m["query"].(string); ok && q != "" {
				return q
			}
		}
		return strings.TrimSpace(in)
	}
	return ""
}

func extractSearchResults(item map[string]any) []map[string]any {
	var sources []any
	if action, ok := item["action"].(map[string]any); ok {
		if s, ok := action["sources"].([]any); ok {
			sources = s
		}
	}
	if sources == nil {
		if s, ok := item["sources"].([]any); ok {
			sources = s
		}
	}
	out := make([]map[string]any, 0, len(sources))
	for _, raw := range sources {
		src, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		u := strField(src["url"])
		if u == "" {
			continue
		}
		title := strField(src["title"])
		domain := ""
		if parsed, err := parseHost(u); err == nil {
			domain = parsed
		}
		if title == "" {
			title = domain
		}
		out = append(out, map[string]any{
			"url":    u,
			"title":  title,
			"domain": domain,
			"type":   strField(src["type"]),
		})
	}
	return out
}

func parseHost(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	h := u.Hostname()
	if h == "" {
		return "", fmt.Errorf("no host")
	}
	return h, nil
}

func handleSearchItemStart(
	item map[string]any,
	respID, model string,
	pending map[string]map[string]any,
	emit func(StreamEvent),
) {
	kind := searchKindFromItem(item)
	if kind == "" {
		return
	}
	itemID := strField(item["id"])
	if itemID == "" {
		itemID = respID
	}
	q := extractSearchQuery(item)
	pending[itemID] = map[string]any{"kind": kind, "query": q}
	emit(StreamEvent{
		Type:  "tool_call",
		ID:    itemID,
		Model: model,
		Text:  kindLabel(kind),
		Payload: map[string]any{
			"provider": "xAI",
			"kind":     kind,
			"name":     kindLabel(kind),
			"status":   "running",
		},
	})
	emit(StreamEvent{
		Type:  "search_query",
		ID:    itemID,
		Model: model,
		Text:  q,
		Payload: map[string]any{
			"provider": "xAI",
			"kind":     kind,
			"query":    q,
			"status":   "searching",
		},
	})
}

func handleSearchItemDone(
	item map[string]any,
	respID, model string,
	pending map[string]map[string]any,
	t0 time.Time,
	emit func(StreamEvent),
) {
	kind := searchKindFromItem(item)
	if kind == "" {
		return
	}
	itemID := strField(item["id"])
	if itemID == "" {
		itemID = respID
	}
	q := extractSearchQuery(item)
	if q == "" {
		if p, ok := pending[itemID]; ok {
			q = strField(p["query"])
		}
	}
	results := extractSearchResults(item)
	ms := time.Since(t0).Milliseconds()
	emit(StreamEvent{
		Type:  "search_results",
		ID:    itemID,
		Model: model,
		Text:  q,
		Payload: map[string]any{
			"provider":    "xAI",
			"kind":        kind,
			"query":       q,
			"results":     results,
			"duration_ms": ms,
			"status":      "done",
		},
	})
	emit(StreamEvent{
		Type:  "tool_done",
		ID:    itemID,
		Model: model,
		Text:  kindLabel(kind),
		Payload: map[string]any{
			"provider": "xAI",
			"kind":     kind,
			"status":   "done",
		},
	})
	delete(pending, itemID)
}

func kindLabel(kind string) string {
	if kind == "x" {
		return "x_search"
	}
	return "web_search"
}

func parseChatUsage(u map[string]any) *Usage {
	out := &Usage{
		PromptTokens:     asInt64(u["prompt_tokens"]),
		CompletionTokens: asInt64(u["completion_tokens"]),
		TotalTokens:      asInt64(u["total_tokens"]),
	}
	if d, ok := u["completion_tokens_details"].(map[string]any); ok {
		out.ReasoningTokens = asInt64(d["reasoning_tokens"])
	}
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		out.CachedTokens = asInt64(d["cached_tokens"])
	}
	return out
}

func parseResponsesUsage(u map[string]any) *Usage {
	out := &Usage{
		PromptTokens:     asInt64(u["input_tokens"]),
		CompletionTokens: asInt64(u["output_tokens"]),
		TotalTokens:      asInt64(u["total_tokens"]),
	}
	if d, ok := u["output_tokens_details"].(map[string]any); ok {
		out.ReasoningTokens = asInt64(d["reasoning_tokens"])
	}
	if d, ok := u["input_tokens_details"].(map[string]any); ok {
		out.CachedTokens = asInt64(d["cached_tokens"])
	}
	return out
}

func estimatePromptTokens(msgs []ChatMessage) int64 {
	n := 0
	for _, m := range msgs {
		n += len(m.Role) + len(m.Content) + 8
	}
	return int64((n + 3) / 4)
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

// Ensure client used with deadline for non-stream
func init() {
	_ = time.Second
}
