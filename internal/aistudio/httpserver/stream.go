package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"grok-desktop/internal/aistudio/chat"
	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/converter"
	"grok-desktop/internal/aistudio/models"
	"grok-desktop/internal/aistudio/prompt"
	"grok-desktop/internal/aistudio/runtime"
	"grok-desktop/internal/aistudio/schema"
)

// handleStandard handles a non-streaming chat completion with retries.
func (s *Server) handleStandard(
	ctx context.Context,
	w http.ResponseWriter,
	requestID string,
	opts converter.RequestOptions,
	originalTools []models.Tool,
	chatCtx *resolvedContext,
	rt *runtime.Runtime,
	injector *prompt.Injector,
) {
	cfg := s.mgr.Config()
	maxAttempts := cfg.MaxRetries + 1
	toolOpts := converter.ToolParseOptions{Tools: originalTools, ToolChoice: opts.ToolChoice}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		defaultLogger.Printf("[REQ %s] tentativa %d/%d perfil=%s", requestID, attempt, maxAttempts, chatCtx.ProfileID)

		s.mgr.NoteRequest(chatCtx.ProfileID)
		result, err := rt.Chat.GenerateContent(ctx, chat.GenerateOptions{RequestID: requestID, RequestOptions: opts})
		if err != nil {
			defaultLogger.Printf("[REQ %s] tentativa %d falhou: %v", requestID, attempt, err)
			s.mgr.NoteFailure(chatCtx.ProfileID, err.Error(), cooldownForError(err, cfg))
			if shouldMigrate(err, nil) {
				if migrated, _ := s.tryMigrate(ctx, chatCtx, err.Error()); migrated && attempt < maxAttempts {
					rt, _ = s.mgr.GetRuntime(chatCtx.ProfileID)
					continue
				}
			}
			if attempt < maxAttempts {
				select {
				case <-time.After(time.Duration(cfg.RetryDelayMs) * time.Millisecond):
				case <-ctx.Done():
					writeError(w, http.StatusBadGateway, ctx.Err().Error(), "server_error")
					return
				}
				continue
			}
			writeError(w, http.StatusBadGateway, err.Error(), "server_error")
			return
		}

		if result.Status != http.StatusOK {
			s.mgr.NoteFailure(chatCtx.ProfileID, "upstream_status_"+itoa(result.Status), cooldownForStatus(result.Status, cfg))
			if shouldMigrateStatus(result.Status) {
				if migrated, _ := s.tryMigrate(ctx, chatCtx, "upstream_status_"+itoa(result.Status)); migrated && attempt < maxAttempts {
					rt, _ = s.mgr.GetRuntime(chatCtx.ProfileID)
					continue
				}
			}
			if shouldRetryStatus(result.Status) && attempt < maxAttempts {
				select {
				case <-time.After(time.Duration(cfg.RetryDelayMs) * time.Millisecond):
				case <-ctx.Done():
					writeError(w, http.StatusBadGateway, ctx.Err().Error(), "server_error")
					return
				}
				continue
			}
			writeError(w, statusOrDefault(result.Status), "AI Studio retornou status "+itoa(result.Status), "upstream_error")
			return
		} else {
			s.mgr.NoteSuccess(chatCtx.ProfileID)
		}

		parsed := converter.ParseGenerateContentResponse(result.Body, toolOpts)

		if shouldRetryToolResponse(&parsed, originalTools, opts) {
			if attempt <= cfg.MaxRetries {
				retryPrompt := injector.BuildRetryPrompt(collectUserText(opts.Messages), originalTools, opts.ToolChoice)
				if len(parsed.ValidationErrors) > 0 {
					retryPrompt += "\n\n[SCHEMA VALIDATION ERROR]\nYour last tool call had invalid arguments according to the JSON Schema.\nPlease correct the arguments and retry."
				}
				opts.Messages = append(opts.Messages, models.Message{Role: "user", Content: json.RawMessage(`"` + jsonEscapeString(retryPrompt) + `"`)})
				select {
				case <-time.After(time.Duration(cfg.RetryDelayMs) * time.Millisecond):
				case <-ctx.Done():
					writeError(w, http.StatusBadGateway, ctx.Err().Error(), "server_error")
					return
				}
				continue
			}
			writeError(w, http.StatusBadGateway, toolResponseFailureMessage(parsed, opts), "upstream_error")
			return
		}

		if empty := emptyParsedResponseReason(result, parsed); empty != "" {
			defaultLogger.Printf("[REQ %s] empty/incomplete upstream parse: %s bodyBytes=%d snippet=%s",
				requestID, empty, result.BodyBytes, logSnippetBody(result.Body))
			if attempt < maxAttempts {
				select {
				case <-time.After(time.Duration(cfg.RetryDelayMs) * time.Millisecond):
				case <-ctx.Done():
					writeError(w, http.StatusBadGateway, ctx.Err().Error(), "server_error")
					return
				}
				continue
			}
			writeError(w, http.StatusBadGateway, empty, "upstream_error")
			return
		}

		response := converter.ToOpenAIResponse(parsed, orModel(opts.Model, cfg.DefaultModel), cfg)
		writeJSON(w, http.StatusOK, response)
		return
	}
	writeError(w, http.StatusBadGateway, "Todas as tentativas falharam", "server_error")
}

// handleStream handles a streaming chat completion using the real-time live
// emitter. Text and reasoning are streamed chunk-by-chunk as soon as they
// arrive from AI Studio; tool calls are emitted atomically from the final
// authoritative parse so they are complete, validated and never leak partial
// protocol text. A keepalive goroutine writes SSE comment frames during
// upstream silence so long-thinking generations do not trip client idle
// timeouts.
func (s *Server) handleStream(
	ctx context.Context,
	w http.ResponseWriter,
	requestID string,
	opts converter.RequestOptions,
	originalTools []models.Tool,
	chatCtx *resolvedContext,
	rt *runtime.Runtime,
	injector *prompt.Injector,
) {
	cfg := s.mgr.Config()
	toolOpts := converter.ToolParseOptions{Tools: originalTools, ToolChoice: opts.ToolChoice}

	keepalive := time.Duration(cfg.Streaming.KeepaliveMs) * time.Millisecond
	emitter := newLiveEmitter(newResponseFlusher(w), requestID, orModel(opts.Model, cfg.DefaultModel), originalTools, toolOpts, keepalive)
	defer emitter.stop()

	maxAttempts := cfg.MaxRetries + 1
	var result *chat.GenerateContentResult
	var lastErr error
	var lastStatus int

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		s.mgr.NoteRequest(chatCtx.ProfileID)
		defaultLogger.Printf("[REQ %s] stream tentativa %d/%d perfil=%s live=true", requestID, attempt, maxAttempts, chatCtx.ProfileID)

		result, lastErr = rt.Chat.GenerateContent(ctx, chat.GenerateOptions{
			RequestID:      requestID,
			RequestOptions: opts,
			OnStreamChunk:  emitter.onChunk,
		})

		// If we already started streaming bytes, retry is not safe: the client
		// already received partial output. Close gracefully instead.
		if emitter.hasPayload() {
			break
		}

		if lastErr != nil {
			s.mgr.NoteFailure(chatCtx.ProfileID, lastErr.Error(), cooldownForError(lastErr, cfg))
			if shouldMigrate(lastErr, nil) {
				if migrated, _ := s.tryMigrate(ctx, chatCtx, lastErr.Error()); migrated && attempt < maxAttempts {
					rt, _ = s.mgr.GetRuntime(chatCtx.ProfileID)
					continue
				}
			}
			if attempt < maxAttempts {
				select {
				case <-time.After(time.Duration(cfg.RetryDelayMs) * time.Millisecond):
				case <-ctx.Done():
				}
				continue
			}
			break
		}

		if result.Status != http.StatusOK {
			lastStatus = statusOrDefault(result.Status)
			s.mgr.NoteFailure(chatCtx.ProfileID, "upstream_status_"+itoa(result.Status), cooldownForStatus(result.Status, cfg))
			if shouldMigrateStatus(result.Status) {
				if migrated, _ := s.tryMigrate(ctx, chatCtx, "upstream_status_"+itoa(result.Status)); migrated && attempt < maxAttempts {
					rt, _ = s.mgr.GetRuntime(chatCtx.ProfileID)
					continue
				}
			}
			if shouldRetryStatus(result.Status) && attempt < maxAttempts {
				select {
				case <-time.After(time.Duration(cfg.RetryDelayMs) * time.Millisecond):
				case <-ctx.Done():
				}
				continue
			}
			break
		}
		parsedAttempt := converter.ParseGenerateContentResponse(result.Body, toolOpts)
		if empty := emptyParsedResponseReason(result, parsedAttempt); empty != "" {
			defaultLogger.Printf("[REQ %s] stream empty/incomplete attempt %d/%d: %s bodyBytes=%d snippet=%s",
				requestID, attempt, maxAttempts, empty, result.BodyBytes, logSnippetBody(result.Body))
			if attempt < maxAttempts {
				select {
				case <-time.After(time.Duration(cfg.RetryDelayMs) * time.Millisecond):
				case <-ctx.Done():
					lastErr = ctx.Err()
					break
				}
				if lastErr == nil {
					continue
				}
			}
			lastErr = errors.New(empty)
			break
		}
		lastStatus = 0
		s.mgr.NoteSuccess(chatCtx.ProfileID)
		break
	}

	// If nothing was streamed and we have a hard error/status, emit a clean
	// error response (headers not yet sent).
	if !emitter.hasPayload() {
		if lastErr != nil {
			if emitter.started {
				emitter.emitError(lastErr.Error())
				logStreamAudit(requestID, "live", chatCtx.ProfileID, result, converter.ParsedResponse{}, "error", emitter.chunkCount(), emitter.done(), emitter.err(), ctx.Err(), lastErr, "upstream_error")
				return
			}
			writeError(w, http.StatusBadGateway, lastErr.Error(), "server_error")
			logStreamAudit(requestID, "live", chatCtx.ProfileID, result, converter.ParsedResponse{}, "error", emitter.chunkCount(), emitter.done(), emitter.err(), ctx.Err(), lastErr, "upstream_error")
			return
		}
		if lastStatus != 0 {
			if emitter.started {
				emitter.emitError("AI Studio retornou status " + itoa(lastStatus))
				logStreamAudit(requestID, "live", chatCtx.ProfileID, result, converter.ParsedResponse{}, "error", emitter.chunkCount(), emitter.done(), emitter.err(), ctx.Err(), lastErr, "upstream_status")
				return
			}
			writeError(w, lastStatus, "AI Studio retornou status "+itoa(lastStatus), "upstream_error")
			logStreamAudit(requestID, "live", chatCtx.ProfileID, result, converter.ParsedResponse{}, "error", emitter.chunkCount(), emitter.done(), emitter.err(), ctx.Err(), lastErr, "upstream_status")
			return
		}
	}

	// Streaming already started (or a 200 body was produced). Finalize with the
	// authoritative parse so tool calls are complete and validated.
	var parsed converter.ParsedResponse
	finish := "stop"
	if result != nil && result.Status == http.StatusOK {
		parsed = converter.ParseGenerateContentResponse(result.Body, toolOpts)
	}
	emitter.setUsage(parsed.Usage)

	if len(parsed.FunctionCalls) > 0 {
		emitter.finishWithToolCalls(parsed.FunctionCalls)
		finish = "tool_calls"
	} else {
		// Reconcile any trailing text from the final parse not yet streamed.
		finalText := strings.Join(parsed.TextParts, "")
		emitter.reconcileFinalText(finalText)
		emitter.finishStop()
	}

	outcome := "ok"
	if lastErr != nil && emitter.started {
		outcome = "incomplete_after_start"
		defaultLogger.Printf("[REQ %s] stream incomplete apos inicio: %v", requestID, lastErr)
	}
	logStreamAudit(requestID, "live", chatCtx.ProfileID, result, parsed, finish, emitter.chunkCount(), emitter.done(), emitter.err(), ctx.Err(), lastErr, outcome)
}

func logStreamAudit(
	requestID, streamMode, profileID string,
	result *chat.GenerateContentResult,
	parsed converter.ParsedResponse,
	finish string,
	sseChunks int,
	wroteDone bool,
	writeErr error,
	ctxErr error,
	lastErr error,
	outcome string,
) {
	bodyBytes := 0
	upstreamStatus := 0
	if result != nil {
		bodyBytes = result.BodyBytes
		if bodyBytes == 0 {
			bodyBytes = len(result.Body)
		}
		upstreamStatus = result.Status
	}
	textChars := 0
	for _, p := range parsed.TextParts {
		textChars += len(p)
	}
	reasoningChars := 0
	for _, p := range parsed.ReasoningParts {
		reasoningChars += len(p)
	}
	writeErrStr := ""
	if writeErr != nil {
		writeErrStr = writeErr.Error()
	}
	ctxErrStr := ""
	if ctxErr != nil {
		ctxErrStr = ctxErr.Error()
	}
	lastErrStr := ""
	if lastErr != nil {
		lastErrStr = lastErr.Error()
	}
	defaultLogger.Printf(
		"[REQ %s] stream_end mode=%s profile=%s outcome=%s upstreamStatus=%d bodyBytes=%d textChars=%d reasoningChars=%d functionCalls=%d finish=%s sseChunks=%d wroteDone=%t writeErr=%q ctxErr=%q lastErr=%q",
		requestID, streamMode, profileID, outcome, upstreamStatus, bodyBytes, textChars, reasoningChars,
		len(parsed.FunctionCalls), finish, sseChunks, wroteDone, writeErrStr, ctxErrStr, lastErrStr,
	)
}

func emptyParsedResponseReason(result *chat.GenerateContentResult, parsed converter.ParsedResponse) string {
	if result == nil || result.Status != http.StatusOK {
		return ""
	}
	if len(parsed.TextParts) > 0 || len(parsed.ReasoningParts) > 0 || len(parsed.FunctionCalls) > 0 || len(parsed.Images) > 0 {
		return ""
	}
	body := strings.TrimSpace(result.Body)
	if body == "" {
		return "AI Studio retornou body vazio (HTTP 200)"
	}
	// Body present but unusable: incomplete JSON or structure without extractable content.
	if parsed.Raw == "" || !looksLikeJSONArray(body) {
		return "AI Studio retornou body incompleto/invalido (HTTP 200, parse vazio)"
	}
	return "AI Studio retornou HTTP 200 sem texto, reasoning, tool calls ou imagens"
}

func looksLikeJSONArray(body string) bool {
	body = strings.TrimSpace(body)
	return strings.HasPrefix(body, "[") && strings.HasSuffix(body, "]")
}

func logSnippetBody(body string) string {
	const max = 240
	body = strings.TrimSpace(body)
	if len(body) <= max {
		return body
	}
	return body[:max] + "..."
}

func emitToolCalls(emit func(models.Delta, *string), calls []converter.FunctionCall, seen map[string]bool, requestID string) {
	openAIToolCalls := converter.BuildOpenAIToolCalls(calls, "call_"+requestID)
	for _, tc := range openAIToolCalls {
		if tc.ID != "" {
			seen[tc.ID] = true
		}
	}
	toolCalls := converter.BuildOpenAIStreamToolCallDeltas(openAIToolCalls)
	emit(models.Delta{ToolCalls: toolCalls}, nil)
}

// tryMigrate attempts to move the session to another profile on recoverable
// errors. Returns true on success and updates chatCtx.ProfileID.
func (s *Server) tryMigrate(ctx context.Context, chatCtx *resolvedContext, reason string) (bool, error) {
	if chatCtx != nil && chatCtx.ExplicitProfileLocked {
		return false, nil
	}
	cfg := s.mgr.Config()
	if !cfg.Migration.Enabled {
		return false, nil
	}
	res, err := s.mgr.MigrateSession(ctx, chatCtx.SessionID, chatCtx.ProfileID, reason, chatCtx.TriedProfileIDs)
	if err != nil {
		return false, err
	}
	chatCtx.ProfileID = res.Profile.ID
	chatCtx.TriedProfileIDs = appendUniqueString(chatCtx.TriedProfileIDs, res.Profile.ID)
	return true, nil
}

func shouldMigrate(err error, cfg *configSubset) bool {
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{"account_changed", "conta ativa do perfil mudou", "sapisid", "cdp connect failed", "timeout esperando bootstrap", "window.__codexcapturedgyb", "__codexcapturedgyb nao disponivel", "gyb indisponivel", "timeout gyb", "target closed", "session closed", "browser has disconnected"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func appendUniqueString(items []string, value string) []string {
	if value == "" {
		return items
	}
	for _, item := range items {
		if item == value {
			return items
		}
	}
	return append(items, value)
}

// configSubset is a minimal view of *config.Config used by the helpers in this
// file to avoid importing config transitively through many call sites.
type configSubset struct {
	MigrationCooldownRuntime int
}

func cooldownForError(err error, cfg *config.Config) int {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "sapisid") || strings.Contains(msg, "conta ativa do perfil mudou") {
		return cfg.Migration.CooldownMs.Auth
	}
	if shouldMigrate(err, nil) {
		return cfg.Migration.CooldownMs.Runtime
	}
	return 0
}

func cooldownForStatus(status int, cfg *config.Config) int {
	switch statusOrDefault(status) {
	case 429:
		return cfg.Migration.CooldownMs.Quota
	case 401, 403:
		return cfg.Migration.CooldownMs.Auth
	}
	return 0
}

func shouldMigrateStatus(status int) bool {
	switch statusOrDefault(status) {
	case 401, 403, 429:
		return true
	default:
		return false
	}
}

func shouldRetryStatus(status int) bool {
	status = statusOrDefault(status)
	return status == 401 || status == 403 || status == 429 || status >= 500
}

func statusOrDefault(status int) int {
	if status == 0 {
		return 502
	}
	return status
}

type toolStreamingPolicy struct {
	buffered          bool
	suppressText      bool
	suppressToolCalls bool
}

func resolveToolStreamingPolicy(tools []models.Tool, mode converter.StreamModeHint) toolStreamingPolicy {
	hasTools := len(tools) > 0
	if !hasTools {
		return toolStreamingPolicy{}
	}
	switch mode {
	case converter.StreamModeHint("buffered"):
		return toolStreamingPolicy{buffered: true, suppressText: true, suppressToolCalls: true}
	case converter.StreamModeHint("live"):
		return toolStreamingPolicy{}
	}
	return toolStreamingPolicy{suppressText: true, suppressToolCalls: true}
}

func shouldRetryToolResponse(parsed *converter.ParsedResponse, tools []models.Tool, opts converter.RequestOptions) bool {
	if len(tools) == 0 {
		return false
	}
	if len(parsed.FunctionCalls) > 0 {
		toolSpecs := toToolSpecs(tools)
		for _, call := range parsed.FunctionCalls {
			res := schema.ValidateToolCall(call.Name, call.Arguments, toolSpecs)
			if !res.Valid {
				parsed.ValidationErrors = toValidationErrors(res)
				return true
			}
		}
		return false
	}
	if requiresToolCall(opts.ToolChoice) {
		return true
	}
	visibleText := strings.Join(parsed.TextParts, "")
	return parsed.HasUnclosedToolCall ||
		len(parsed.RejectedToolCalls) > 0 ||
		converter.ExtractFunctionCallErrorText(parsed.Raw) != "" ||
		converter.LooksLikeToolProtocolPublic(visibleText)
}

func toolResponseFailureMessage(parsed converter.ParsedResponse, opts converter.RequestOptions) string {
	if len(parsed.ValidationErrors) > 0 {
		first := parsed.ValidationErrors[0]
		if first.Path != "" {
			return "AI Studio retornou tool call invalida: " + first.Path + " " + first.Message
		}
		return "AI Studio retornou tool call invalida: " + first.Message
	}
	if requiresToolCall(opts.ToolChoice) && len(parsed.FunctionCalls) == 0 {
		return "AI Studio nao retornou a tool call obrigatoria apos todas as tentativas"
	}
	if parsed.HasUnclosedToolCall {
		return "AI Studio retornou tool call incompleta apos todas as tentativas"
	}
	if len(parsed.RejectedToolCalls) > 0 {
		return "AI Studio retornou tool call rejeitada ou malformada apos todas as tentativas"
	}
	if malformed := converter.ExtractFunctionCallErrorText(parsed.Raw); malformed != "" {
		return malformed
	}
	return "AI Studio retornou tool call invalida apos todas as tentativas"
}

func toToolSpecs(tools []models.Tool) []schema.ToolSpec {
	specs := make([]schema.ToolSpec, 0, len(tools))
	for _, t := range tools {
		var spec schema.ToolSpec
		spec.Function.Name = t.Function.Name
		spec.Function.Parameters = t.Function.Parameters
		specs = append(specs, spec)
	}
	return specs
}

func requiresToolCall(toolChoice json.RawMessage) bool {
	if len(toolChoice) == 0 {
		return false
	}
	var s string
	if json.Unmarshal(toolChoice, &s) == nil {
		return s == "required"
	}
	return true
}

func toValidationErrors(res schema.Result) []converter.ValidationError {
	out := make([]converter.ValidationError, 0, len(res.Errors))
	for _, e := range res.Errors {
		out = append(out, converter.ValidationError{Path: e.Path, Message: e.Message})
	}
	return out
}

func collectUserText(messages []models.Message) string {
	var sb strings.Builder
	for _, msg := range messages {
		if msg.Role != "user" {
			continue
		}
		if s := plainContent(msg.Content); s != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(s)
		}
	}
	return sb.String()
}

func plainContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func jsonEscapeString(s string) string {
	b, _ := json.Marshal(s)
	// json.Marshal returns a quoted string; strip the quotes to embed in a template.
	return string(b[1 : len(b)-1])
}
