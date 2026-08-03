package proxyhttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"grok-desktop/internal/logging"
	"grok-desktop/internal/store"
)

// handleMessages implements Anthropic Messages API: POST /v1/messages
// Routes by client model id (same as the chat endpoints):
//   - Ollie → native Anthropic passthrough (OllieChat speaks /messages)
//   - Kimi Work → OpenAI chat/completions translation (agent-gw is chat-only)
//   - xAI → Responses API translation (cli-chat-proxy only speaks /responses)
//
// Quota/auth failures rotate accounts exactly like proxyUpstream, with the
// route provider pinned on every ensure/forceRefresh call.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"method not allowed"}}`, http.StatusMethodNotAllowed)
		return
	}
	if !s.gateAnthropic(r) {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid api key")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxProxyBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid json")
		return
	}

	// Route by client model only — never UI global provider.
	reqModel := asString(req["model"])
	settings := s.store.Settings().WithProviderForModel(reqModel)
	routeProv := settings.NormalizedProvider()
	ctx := WithRouteProvider(r.Context(), routeProv)
	ctx = logging.WithRequestID(ctx, uuid.NewString()[:8])
	msgLog := logging.FromContext(ctx).With("provider", routeProv, "path", "/v1/messages")
	msgLog.Info("proxy.request.start", "model", reqModel)
	// Track accounts Inc'd by ensure so in-flight counters Dec on completion.
	ctx, inflight := store.WithInflightTracker(ctx)
	defer inflight.DecAll(s.store)
	token, acc, settings2, err := s.ensure(ctx)
	if err != nil {
		msgLog.Error("proxy.request.error", "model", reqModel, "status", 401, "reason", err.Error())
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", err.Error())
		return
	}
	if acc != nil {
		store.TrackInflight(ctx, acc.ID)
	}
	settings = settings2.WithProvider(routeProv)
	token, acc = tokenAccountForSettings(s, ctx, token, acc, settings)
	model := settings.ResolveModelForClient(reqModel)
	stream, _ := req["stream"].(bool)
	maxTokens := asInt(req["max_tokens"], 4096)
	// Client body only — never inherit global settings.ReasoningEffort.
	effort := asString(req["reasoning_effort"])
	// Anthropic thinking / effort aliases from the client request only.
	if th, ok := req["thinking"].(map[string]any); ok {
		if t := asString(th["type"]); t == "enabled" {
			if effort == "" {
				effort = "high"
			}
		}
	}

	// Ollie natively supports Anthropic /v1/messages — passthrough is more faithful.
	if settings.IsOllie() {
		s.proxyOllieAnthropic(w, r, token, settings, body, stream)
		return
	}

	messages := anthropicToOpenAIMessages(req)
	messages = injectTemporalIntoMessages(messages)

	oaBody := map[string]any{
		"model":      model,
		"messages":   messages,
		"stream":     stream,
		"max_tokens": maxTokens,
	}
	if effort != "" {
		oaBody["reasoning_effort"] = effort
	}
	if stream {
		oaBody["stream_options"] = map[string]any{"include_usage": true}
	}
	if tools := sanitizeChatTools(req["tools"]); len(tools) > 0 {
		oaBody["tools"] = tools
		oaBody["tool_choice"] = mapAnthropicToolChoice(req["tool_choice"])
	}
	if t, ok := req["temperature"].(float64); ok {
		oaBody["temperature"] = t
	}
	if t, ok := req["top_p"].(float64); ok {
		oaBody["top_p"] = t
	}

	// Kimi Work agent-gw: chat/completions wire with hard context limits.
	if settings.IsKimiWork() {
		oaBody = clampKimiContextLimits(oaBody, model)
		sanitizeKimiWorkChatBody(oaBody)
	}

	// xAI gateway only speaks /responses — translate the wire body with the
	// same machinery the /v1/chat/completions route uses.
	upstreamPath := "/chat/completions"
	upBody := oaBody
	if settings.IsXAI() {
		upstreamPath = "/responses"
		rb := chatCompletionsBodyToResponses(oaBody, settings)
		if _, ok := rb["tools"]; !ok {
			rb["tools"] = nativeSearchTools()
			rb["tool_choice"] = "auto"
		}
		upBody = rb
	}
	raw, _ := json.Marshal(upBody)
	url := strings.TrimRight(settings.EffectiveUpstream(), "/") + upstreamPath

	startedAt := time.Now()
	accountID := ""
	if acc != nil {
		accountID = acc.ID
	}
	authRetried := false
	// rotateCreds adopts credentials from a retry ensure/forceRefresh. Settings
	// stay pinned to the route; only token/acc swap. Returns false when the
	// rotated account belongs to another provider — fail instead of sending a
	// cross-provider body upstream with a mismatched token.
	rotateCreds := func(tok2 string, acc2 *store.Account) bool {
		if acc2 != nil && acc2.NormalizedProvider() != routeProv {
			return false
		}
		if acc2 != nil && acc2.ID != accountID {
			// Dec the previous account now that the next one is Inc'd.
			if n := inflight.Remove(accountID); n > 0 {
				for i := 0; i < n; i++ {
					s.store.DecAccountRequestCount(accountID)
				}
			}
		}
		if acc2 != nil {
			store.TrackInflight(ctx, acc2.ID)
		}
		token, acc = tok2, acc2
		if acc2 != nil {
			accountID = acc2.ID
		}
		return true
	}

	var resp *http.Response
	for attempt := 0; attempt < 3; attempt++ {
		upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			writeAnthropicError(w, 500, "api_error", err.Error())
			return
		}
		setUpstreamAuthHeaders(upReq, token, settings)
		if stream {
			upReq.Header.Set("Accept", "text/event-stream")
		} else {
			upReq.Header.Set("Accept", "application/json")
		}

		upSentAt := time.Now()
		resp, err = upstreamHTTPClient.Do(upReq)
		if err != nil {
			msgLog.Error("proxy.request.error", "model", model, "account_id", accountID, "status", 502, "reason", err.Error())
			writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
			return
		}
		msgLog.Info("proxy.upstream.response", "attempt", attempt+1, "status", resp.StatusCode, "latency_ms", time.Since(upSentAt).Milliseconds(), "account_id", accountID)
		if resp.StatusCode < 400 {
			break
		}

		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		code := resp.StatusCode
		_ = resp.Body.Close()
		resp = nil
		reason := fmt.Sprintf("HTTP %d: %s", code, string(errBody))

		// Kimi global capacity — fail fast, no account rotate / re-mint spam.
		if settings.IsKimiWork() && isKimiCapacityBusy(code, errBody) {
			msgLog.Warn("proxy.kimi.capacity_busy", "detail", truncateForLog(string(errBody), 200))
			msg := "Too many people are chatting with Kimi right now. Please try again soon."
			if m, ok := extractUpstreamMessage(errBody); ok && m != "" {
				msg = m
			}
			w.Header().Set("Retry-After", "30")
			writeAnthropicError(w, http.StatusServiceUnavailable, "overloaded_error", msg)
			return
		}

		// Quota first (Kimi billing 403 must not be treated as auth-denied).
		// NOTE: every ensure/forceRefresh below runs with the route-pinned ctx,
		// never the raw r.Context() — rotation must stay on the same provider.
		if isQuotaStatus(code, errBody) {
			if fn := s.quotaHandler(); fn != nil && accountID != "" {
				if rotated := fn(accountID, reason); rotated {
					tok2, acc2, _, err2 := s.ensure(ctx)
					// Retry on new account OR same id refilled (re-login / remint).
					if err2 == nil && tok2 != "" && (acc2 == nil || acc2.Usable()) {
						prev := accountID
						if !rotateCreds(tok2, acc2) {
							writeAnthropicCrossProviderError(w, routeProv, acc2)
							return
						}
						msgLog.Warn("proxy.rotate", "from_account", prev, "to_account", accountID, "reason", "quota")
						continue
					}
				}
			} else if accountID != "" {
				_, _ = s.store.MarkExhausted(accountID, reason)
				if next := s.store.NextUsableAccountID(accountID); next != "" {
					_ = s.store.SetActiveAccount(next)
					tok2, acc2, _, err2 := s.ensure(ctx)
					if err2 == nil {
						if !rotateCreds(tok2, acc2) {
							writeAnthropicCrossProviderError(w, routeProv, acc2)
							return
						}
						continue
					}
				}
			}
		}

		// Auth denial: force-refresh same account once, then rotate.
		if isAuthStatus(code, errBody) && accountID != "" && !isQuotaStatus(code, errBody) {
		if isCrossProviderAuthMismatch(acc, settings, errBody) {
			msgLog.Warn("proxy.auth.cross_provider_mismatch", "account_id", accountID, "provider", routeProv)
		} else {
				if !authRetried {
					authRetried = true
					if fr := s.forceRefreshFn(); fr != nil {
						tok2, acc2, _, err2 := fr(ctx, accountID)
						if err2 == nil && tok2 != "" && tok2 != token {
							if !rotateCreds(tok2, acc2) {
								writeAnthropicCrossProviderError(w, routeProv, acc2)
								return
							}
							msgLog.Warn("proxy.rotate", "from_account", accountID, "to_account", accountID, "reason", "auth_force_refresh")
							continue
						}
					}
					// Also re-run ensure in case the refresh path was unavailable.
					tok2, acc2, _, err2 := s.ensure(ctx)
					if err2 == nil && tok2 != "" && tok2 != token {
						if !rotateCreds(tok2, acc2) {
							writeAnthropicCrossProviderError(w, routeProv, acc2)
							return
						}
						msgLog.Warn("proxy.rotate", "from_account", accountID, "to_account", accountID, "reason", "auth_token_changed")
						continue
					}
				}

				// Mark auth-denied and rotate to another account.
				if fn := s.authFailHandler(); fn != nil {
					if rotated := fn(accountID, reason); rotated {
						tok2, acc2, _, err2 := s.ensure(ctx)
						if err2 == nil && (acc2 == nil || acc2.ID != accountID) {
							prev := accountID
							if !rotateCreds(tok2, acc2) {
								writeAnthropicCrossProviderError(w, routeProv, acc2)
								return
							}
							msgLog.Warn("proxy.rotate", "from_account", prev, "to_account", accountID, "reason", "auth")
							continue
						}
					}
				} else {
					_, _ = s.store.MarkAuthDenied(accountID, reason)
					if next := s.store.NextUsableAccountID(accountID); next != "" {
						_ = s.store.SetActiveAccount(next)
						tok2, acc2, _, err2 := s.ensure(ctx)
						if err2 == nil {
							if !rotateCreds(tok2, acc2) {
								writeAnthropicCrossProviderError(w, routeProv, acc2)
								return
							}
							continue
						}
					}
				}
			}
		}

		// Final error to client.
		msgLog.Error("proxy.request.error", "model", model, "account_id", accountID, "status", code, "reason", truncateForLog(string(errBody), 200))
		writeAnthropicError(w, code, "api_error", string(errBody))
		return
	}
	if resp == nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream retries exhausted")
		return
	}
	defer resp.Body.Close()
	latencyMs := time.Since(startedAt).Milliseconds()

	// xAI: upstream spoke Responses — translate back to Anthropic shapes.
	if settings.IsXAI() {
		if !stream {
			rawResp, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
			s.recordUsageFromJSONBody(rawResp, accountID, routeProv, model, latencyMs)
			out, err := responsesJSONToAnthropicMessage(rawResp, model)
			if err != nil {
				writeAnthropicError(w, 500, "api_error", err.Error())
				return
			}
			msgLog.Info("proxy.request.done", "model", model, "account_id", accountID, "duration_ms", latencyMs, "status", 200)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
			return
		}
		tee := newUsageTeeReader(resp.Body)
		if err := streamResponsesToAnthropic(r.Context(), w, tee, model); err != nil {
			s.handleStreamQuota(accountID, err)
		}
		if len(tee.lastJSON) > 0 {
			s.recordUsageFromSSECapture(tee.lastJSON, accountID, routeProv, model, latencyMs)
		}
		return
	}

	// Kimi Work: upstream spoke OpenAI chat/completions.
	if !stream {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		s.recordUsageFromJSONBody(b, accountID, routeProv, model, latencyMs)
		out, err := openAIChatToAnthropicMessage(b, model)
		if err != nil {
			writeAnthropicError(w, 500, "api_error", err.Error())
			return
		}
		msgLog.Info("proxy.request.done", "model", model, "account_id", accountID, "duration_ms", latencyMs, "status", 200)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
		return
	}

	tee := newUsageTeeReader(resp.Body)
	if err := streamOpenAIToAnthropic(r.Context(), w, tee, model); err != nil {
		s.handleStreamQuota(accountID, err)
	}
	if len(tee.lastJSON) > 0 {
		s.recordUsageFromSSECapture(tee.lastJSON, accountID, routeProv, model, latencyMs)
	}
}

func (s *Server) gateAnthropic(r *http.Request) bool {
	key := s.store.Settings().ProxyAPIKey
	if key == "" {
		return true
	}
	// Anthropic: x-api-key or Authorization: Bearer
	if r.Header.Get("x-api-key") == key {
		return true
	}
	return s.gate(r)
}

// proxyOllieAnthropic forwards Anthropic Messages JSON as-is to OllieChat.
func (s *Server) proxyOllieAnthropic(w http.ResponseWriter, r *http.Request, token string, settings store.Settings, body []byte, stream bool) {
	// Ensure required max_tokens for Anthropic shape.
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		if _, ok := m["max_tokens"]; !ok {
			m["max_tokens"] = 4096
		}
		reqModel, _ := m["model"].(string)
		// Anthropic Ollie passthrough: honor client model (no Codex force here).
		m["model"] = settings.ResolveModelForClient(reqModel)
		if stream {
			m["stream"] = true
		}
		body, _ = json.Marshal(m)
	}
	url := strings.TrimRight(settings.EffectiveUpstream(), "/") + "/messages"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		writeAnthropicError(w, 500, "api_error", err.Error())
		return
	}
	setUpstreamAuthHeaders(upReq, token, settings)
	upReq.Header.Set("anthropic-version", "2023-06-01")
	if v := r.Header.Get("anthropic-version"); v != "" {
		upReq.Header.Set("anthropic-version", v)
	}
	if stream {
		upReq.Header.Set("Accept", "text/event-stream")
	} else {
		upReq.Header.Set("Accept", "application/json")
	}
	resp, err := upstreamHTTPClient.Do(upReq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer resp.Body.Close()
	// Pass through status + body (JSON or SSE).
	for k, vv := range resp.Header {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func writeAnthropicError(w http.ResponseWriter, code int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    typ,
			"message": msg,
		},
	})
}

// writeAnthropicCrossProviderError fails the request when account rotation
// landed on a different provider than the model route pinned (Anthropic-shaped
// sibling of writeCrossProviderError).
func writeAnthropicCrossProviderError(w http.ResponseWriter, routeProv string, acc *store.Account) {
	got := "nil"
	if acc != nil {
		got = acc.NormalizedProvider()
	}
	writeAnthropicError(w, http.StatusBadGateway, "api_error",
		"account rotation returned provider "+got+" for route "+routeProv+" — refusing cross-provider retry")
}

func anthropicToOpenAIMessages(req map[string]any) []any {
	out := make([]any, 0, 8)
	// system can be string or array of text blocks
	if sys := req["system"]; sys != nil {
		text := anthropicContentToText(sys)
		if text != "" {
			out = append(out, map[string]any{"role": "system", "content": text})
		}
	}
	msgs, _ := req["messages"].([]any)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := asString(m["role"])
		if role == "" {
			continue
		}
		// tool_result messages → OpenAI tool role
		if role == "user" {
			// content may include tool_result blocks
			if blocks, ok := m["content"].([]any); ok {
				var textParts []string
				for _, b := range blocks {
					bm, ok := b.(map[string]any)
					if !ok {
						continue
					}
					switch asString(bm["type"]) {
					case "tool_result":
						out = append(out, map[string]any{
							"role":         "tool",
							"tool_call_id": asString(bm["tool_use_id"]),
							"content":      anthropicContentToText(bm["content"]),
						})
					case "text":
						textParts = append(textParts, asString(bm["text"]))
					default:
						if t := asString(bm["text"]); t != "" {
							textParts = append(textParts, t)
						}
					}
				}
				if len(textParts) > 0 {
					out = append(out, map[string]any{"role": "user", "content": strings.Join(textParts, "\n")})
				}
				continue
			}
		}
		if role == "assistant" {
			if blocks, ok := m["content"].([]any); ok {
				var textParts []string
				var toolCalls []any
				for _, b := range blocks {
					bm, ok := b.(map[string]any)
					if !ok {
						continue
					}
					switch asString(bm["type"]) {
					case "text":
						textParts = append(textParts, asString(bm["text"]))
					case "tool_use":
						args := bm["input"]
						argStr, _ := json.Marshal(args)
						toolCalls = append(toolCalls, sanitizeToolCall(map[string]any{
							"id":   asString(bm["id"]),
							"type": "function",
							"function": map[string]any{
								"name":      asString(bm["name"]),
								"arguments": string(argStr),
							},
						}))
					}
				}
				msg := map[string]any{"role": "assistant", "content": strings.Join(textParts, "")}
				if len(toolCalls) > 0 {
					msg["tool_calls"] = toolCalls
				}
				out = append(out, msg)
				continue
			}
		}
		out = append(out, map[string]any{
			"role":    role,
			"content": anthropicContentToText(m["content"]),
		})
	}
	return out
}

func anthropicContentToText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, b := range c {
			switch t := b.(type) {
			case string:
				parts = append(parts, t)
			case map[string]any:
				if asString(t["type"]) == "text" || t["type"] == nil {
					if s := asString(t["text"]); s != "" {
						parts = append(parts, s)
					}
				} else if s := asString(t["text"]); s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func mapAnthropicToolChoice(v any) any {
	switch t := v.(type) {
	case string:
		switch t {
		case "auto", "any", "none":
			if t == "any" {
				return "required"
			}
			return t
		default:
			return "auto"
		}
	case map[string]any:
		// {"type":"tool","name":"foo"} → OpenAI tool choice
		if asString(t["type"]) == "tool" {
			return map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": asString(t["name"]),
				},
			}
		}
	}
	return "auto"
}

func openAIChatToAnthropicMessage(raw []byte, model string) ([]byte, error) {
	var oa map[string]any
	if err := json.Unmarshal(raw, &oa); err != nil {
		return nil, err
	}
	id := "msg_" + uuid.NewString()
	content := make([]any, 0, 2)
	stopReason := "end_turn"
	choices, _ := oa["choices"].([]any)
	if len(choices) > 0 {
		ch, _ := choices[0].(map[string]any)
		msg, _ := ch["message"].(map[string]any)
		if msg != nil {
			if t := asString(msg["content"]); t != "" {
				content = append(content, map[string]any{"type": "text", "text": t})
			}
			if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
				stopReason = "tool_use"
				for _, tc := range tcs {
					tcm, _ := tc.(map[string]any)
					sanitized := sanitizeToolCall(tcm)
					if sanitized == nil {
						continue
					}
					fn, _ := sanitized["function"].(map[string]any)
					var input any = map[string]any{}
					_ = json.Unmarshal([]byte(asString(fn["arguments"])), &input)
					content = append(content, map[string]any{
						"type":  "tool_use",
						"id":    asString(sanitized["id"]),
						"name":  asString(fn["name"]),
						"input": input,
					})
				}
			}
		}
		if fr := asString(ch["finish_reason"]); fr == "tool_calls" {
			stopReason = "tool_use"
		} else if fr == "length" {
			stopReason = "max_tokens"
		}
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if u, ok := oa["usage"].(map[string]any); ok {
		usage["input_tokens"] = asInt(u["prompt_tokens"], 0)
		usage["output_tokens"] = asInt(u["completion_tokens"], 0)
	}
	out := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         usage,
	}
	return json.Marshal(out)
}

// responsesJSONToAnthropicMessage maps a non-stream xAI /responses JSON body to
// an Anthropic Message. Reuses the Responses→chat and chat→Anthropic converters.
func responsesJSONToAnthropicMessage(raw []byte, model string) ([]byte, error) {
	chat, err := responsesJSONToChatCompletion(raw, model)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(chat)
	if err != nil {
		return nil, err
	}
	return openAIChatToAnthropicMessage(b, model)
}

// streamResponsesToAnthropic translates an xAI Responses SSE stream into
// Anthropic Messages SSE events (message_start/content_block_*/message_delta/
// message_stop). Mirrors pipeResponsesSSEToChat event coverage: output_text
// deltas become text blocks, client function_call items become tool_use blocks,
// server-side web/x search items are skipped, and mid-stream quota payloads end
// the stream gracefully with an "sse quota error" for account rotation.
func streamResponsesToAnthropic(ctx context.Context, w http.ResponseWriter, body io.Reader, model string) error {
	setSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
	fw := newFlushWriter(w)
	msgID := "msg_" + uuid.NewString()
	_ = writeSSEJSON(fw, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}, json.Marshal)

	openBlock := -1 // index of the currently open content block (-1 = none)
	blockKind := "" // "text" | "tool_use"
	nextIndex := 0
	toolBlockIndex := map[string]int{} // itemID/callID → content block index
	argSent := map[string]bool{}
	stopReason := "end_turn"
	outputTok := 0

	closeOpen := func() {
		if openBlock >= 0 {
			_ = writeSSEJSON(fw, "content_block_stop", map[string]any{
				"type": "content_block_stop", "index": openBlock,
			}, json.Marshal)
			openBlock = -1
			blockKind = ""
		}
	}
	emitText := func(delta string) {
		if blockKind != "text" {
			closeOpen()
			idx := nextIndex
			nextIndex++
			_ = writeSSEJSON(fw, "content_block_start", map[string]any{
				"type": "content_block_start", "index": idx,
				"content_block": map[string]any{"type": "text", "text": ""},
			}, json.Marshal)
			openBlock = idx
			blockKind = "text"
		}
		_ = writeSSEJSON(fw, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": openBlock,
			"delta": map[string]any{"type": "text_delta", "text": delta},
		}, json.Marshal)
	}
	resolveToolIdx := func(keys ...string) (int, bool) {
		for _, k := range keys {
			if k == "" {
				continue
			}
			if i, ok := toolBlockIndex[k]; ok {
				return i, true
			}
		}
		return -1, false
	}
	markToolKeys := func(idx int, keys ...string) {
		for _, k := range keys {
			if k != "" {
				toolBlockIndex[k] = idx
			}
		}
	}
	emitArgsDelta := func(idx int, itemID, callID, delta string) {
		if delta == "" {
			return
		}
		_ = writeSSEJSON(fw, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": delta},
		}, json.Marshal)
		argSent[firstNonEmpty(itemID, callID)] = true
	}
	// startTool opens (or reuses) a tool_use block for a function_call item.
	// allowFullArgs is true on output_item.done / response.completed harvests
	// where the item carries the full arguments string.
	startTool := func(item map[string]any, allowFullArgs bool) {
		tc := responsesItemToChatToolCall(item)
		if tc == nil {
			return
		}
		callID := asString(tc["id"])
		itemID := asString(item["id"])
		fn, _ := tc["function"].(map[string]any)
		name := asString(fn["name"])
		args := asString(fn["arguments"])
		if !allowFullArgs {
			args = ""
		}
		if idx, ok := resolveToolIdx(itemID, callID); ok {
			// Block already open — deliver the full args once if we have them.
			if allowFullArgs && args != "" && args != "{}" && !argSent[firstNonEmpty(itemID, callID)] {
				emitArgsDelta(idx, itemID, callID, args)
			}
			return
		}
		closeOpen()
		if callID == "" {
			callID = "toolu_" + uuid.NewString()
		}
		idx := nextIndex
		nextIndex++
		markToolKeys(idx, itemID, callID)
		_ = writeSSEJSON(fw, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": idx,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    callID,
				"name":  name,
				"input": map[string]any{},
			},
		}, json.Marshal)
		openBlock = idx
		blockKind = "tool_use"
		stopReason = "tool_use"
		if args != "" && args != "{}" {
			emitArgsDelta(idx, itemID, callID, args)
		}
	}
	emitToolArgs := func(itemID, callID, delta string) {
		if delta == "" {
			return
		}
		idx, ok := resolveToolIdx(itemID, callID)
		if !ok {
			// Args arrived without an item event — open a block so no data is lost.
			closeOpen()
			if callID == "" {
				callID = "toolu_" + uuid.NewString()
			}
			idx = nextIndex
			nextIndex++
			markToolKeys(idx, itemID, callID)
			_ = writeSSEJSON(fw, "content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    callID,
					"name":  "",
					"input": map[string]any{},
				},
			}, json.Marshal)
			openBlock = idx
			blockKind = "tool_use"
			stopReason = "tool_use"
		}
		emitArgsDelta(idx, itemID, callID, delta)
	}
	finish := func() error {
		closeOpen()
		_ = writeSSEJSON(fw, "message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": outputTok},
		}, json.Marshal)
		_ = writeSSEJSON(fw, "message_stop", map[string]any{"type": "message_stop"}, json.Marshal)
		_, _ = io.WriteString(fw, "\n")
		return nil
	}

	sc := bufio.NewScanner(newContextReader(ctx, body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		// Mid-stream quota error → graceful Anthropic error event, then mark the
		// account via the "sse quota error" contract (handleStreamQuota).
		if isQuota, qmsg := isQuotaErrorPayload(ev); isQuota {
			closeOpen()
			_ = writeSSEJSON(fw, "error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": qmsg},
			}, json.Marshal)
			_ = writeSSEJSON(fw, "message_stop", map[string]any{"type": "message_stop"}, json.Marshal)
			return fmt.Errorf("sse quota error: %s", qmsg)
		}
		typ, _ := ev["type"].(string)
		switch typ {
		case "response.output_text.delta":
			if d, ok := ev["delta"].(string); ok && d != "" {
				emitText(d)
			}
		case "response.output_item.added":
			if item, ok := ev["item"].(map[string]any); ok {
				startTool(item, false)
			}
		case "response.output_item.done":
			if item, ok := ev["item"].(map[string]any); ok {
				startTool(item, true)
			}
		case "response.function_call_arguments.delta":
			d, _ := ev["delta"].(string)
			if d == "" {
				d, _ = ev["arguments"].(string)
			}
			emitToolArgs(asString(ev["item_id"]), asString(ev["call_id"]), d)
		case "response.function_call_arguments.done":
			itemID := asString(ev["item_id"])
			callID := asString(ev["call_id"])
			args, _ := ev["arguments"].(string)
			if args == "" || argSent[firstNonEmpty(itemID, callID)] {
				continue
			}
			emitToolArgs(itemID, callID, args)
		case "response.completed", "response.done":
			if respObj, ok := ev["response"].(map[string]any); ok {
				// Harvest function calls in case item events were missed.
				for _, rawTC := range extractResponsesFunctionCalls(respObj) {
					if tcm, ok := rawTC.(map[string]any); ok {
						fn, _ := tcm["function"].(map[string]any)
						startTool(map[string]any{
							"type":      "function_call",
							"call_id":   asString(tcm["id"]),
							"id":        asString(tcm["id"]),
							"name":      asString(fn["name"]),
							"arguments": asString(fn["arguments"]),
						}, true)
					}
				}
				if u, ok := respObj["usage"].(map[string]any); ok {
					outputTok = asInt(u["output_tokens"], outputTok)
				}
			}
			return finish()
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("sse scan: %w", err)
	}
	return finish()
}

func streamOpenAIToAnthropic(ctx context.Context, w http.ResponseWriter, body io.Reader, model string) error {
	setSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
	fw := newFlushWriter(w)
	msgID := "msg_" + uuid.NewString()
	// message_start
	_ = writeSSEJSON(fw, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}, json.Marshal)

	textStarted := false
	textIndex := 0
	toolIndex := 0
	toolStarted := map[string]int{} // id → content block index
	inputTok, outputTok := 0, 0
	stopReason := "end_turn"

	sc := bufio.NewScanner(newContextReader(ctx, body))
	sc.Buffer(make([]byte, 0, 64*1024), 2<<20)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		// Mid-stream quota error → graceful Anthropic error event, then mark the
		// account via the "sse quota error" contract (handleStreamQuota).
		if isQuota, qmsg := isQuotaErrorPayload(chunk); isQuota {
			if textStarted {
				_ = writeSSEJSON(fw, "content_block_stop", map[string]any{
					"type": "content_block_stop", "index": textIndex,
				}, json.Marshal)
			}
			_ = writeSSEJSON(fw, "error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": qmsg},
			}, json.Marshal)
			_ = writeSSEJSON(fw, "message_stop", map[string]any{"type": "message_stop"}, json.Marshal)
			return fmt.Errorf("sse quota error: %s", qmsg)
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			inputTok = asInt(u["prompt_tokens"], inputTok)
			outputTok = asInt(u["completion_tokens"], outputTok)
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		ch, _ := choices[0].(map[string]any)
		if fr := asString(ch["finish_reason"]); fr != "" {
			if fr == "tool_calls" {
				stopReason = "tool_use"
			} else if fr == "length" {
				stopReason = "max_tokens"
			} else {
				stopReason = "end_turn"
			}
		}
		delta, _ := ch["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		// reasoning → thinking-ish text (emit as text for compatibility)
		if rc := asString(delta["reasoning_content"]); rc != "" {
			if !textStarted {
				_ = writeSSEJSON(fw, "content_block_start", map[string]any{
					"type": "content_block_start", "index": textIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				}, json.Marshal)
				textStarted = true
			}
			_ = writeSSEJSON(fw, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": textIndex,
				"delta": map[string]any{"type": "text_delta", "text": rc},
			}, json.Marshal)
		}
		if t := asString(delta["content"]); t != "" {
			if !textStarted {
				_ = writeSSEJSON(fw, "content_block_start", map[string]any{
					"type": "content_block_start", "index": textIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				}, json.Marshal)
				textStarted = true
			}
			_ = writeSSEJSON(fw, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": textIndex,
				"delta": map[string]any{"type": "text_delta", "text": t},
			}, json.Marshal)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				tcm, _ := tc.(map[string]any)
				id := asString(tcm["id"])
				fn, _ := tcm["function"].(map[string]any)
				name := asString(fn["name"])
				args := asString(fn["arguments"])
				idx, ok := toolStarted[id]
				if !ok || id == "" {
					// new tool block
					if textStarted {
						_ = writeSSEJSON(fw, "content_block_stop", map[string]any{
							"type": "content_block_stop", "index": textIndex,
						}, json.Marshal)
						textStarted = false
						toolIndex = textIndex + 1
					}
					if id == "" {
						id = "toolu_" + uuid.NewString()
					}
					idx = toolIndex
					toolStarted[id] = idx
					toolIndex++
					_ = writeSSEJSON(fw, "content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": idx,
						"content_block": map[string]any{
							"type":  "tool_use",
							"id":    id,
							"name":  name,
							"input": map[string]any{},
						},
					}, json.Marshal)
					stopReason = "tool_use"
				}
				if name != "" {
					// name only on start; ignore subsequent
				}
				if args != "" {
					_ = writeSSEJSON(fw, "content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": idx,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
					}, json.Marshal)
				}
			}
		}
	}

	// close open blocks
	if textStarted {
		_ = writeSSEJSON(fw, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": textIndex,
		}, json.Marshal)
	}
	for _, idx := range toolStarted {
		_ = writeSSEJSON(fw, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": idx,
		}, json.Marshal)
	}

	_ = writeSSEJSON(fw, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outputTok},
	}, json.Marshal)
	_ = writeSSEJSON(fw, "message_stop", map[string]any{"type": "message_stop"}, json.Marshal)
	// final blank
	_, _ = io.WriteString(fw, "\n")
	_ = inputTok
	_ = time.Now()
	return sc.Err()
}

func asInt(v any, def int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		i, _ := t.Int64()
		return int(i)
	default:
		return def
	}
}
