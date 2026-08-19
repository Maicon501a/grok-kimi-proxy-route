// Package chat implements the HTTP-first direct chat client that signs and
// sends MakerSuiteService/GenerateContent requests using the Botguard capture.
//
// This is the high-throughput path (no browser round-trip per request) and the
// primary mode of the proxy. The original Node implementation interleaved a
// slow browser-driven fallback; here the browser path lives only in the
// botguard bootstrap.
package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	stdlog "log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"grok-desktop/internal/aistudio/auth"
	"grok-desktop/internal/aistudio/botguard"
	"grok-desktop/internal/aistudio/cdp"
	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/converter"
	"grok-desktop/internal/aistudio/jsonx"
	"grok-desktop/internal/aistudio/models"
)

// Client is the per-profile direct chat client.
type Client struct {
	cdp       *cdp.Client
	cfg       *config.Config
	botguard  *botguard.Service
	profileID string
	http      *http.Client

	mu             sync.Mutex
	cachedAuth     *auth.RuntimeAuth
	lastAuthFailAt time.Time

	uploadMu        sync.Mutex
	uploadContext   *uploadContext
	uploadContextAt time.Time

	nativeReplay *NativeReplayStore
}

// New creates a direct chat client bound to a profile.
func New(c *cdp.Client, b *botguard.Service, cfg *config.Config, profileID string, nativeReplay *NativeReplayStore) *Client {
	headerTimeout := time.Duration(cfg.GenerateHeaderTimeout) * time.Millisecond
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
	}
	return &Client{
		cdp:          c,
		cfg:          cfg,
		botguard:     b,
		profileID:    profileID,
		nativeReplay: nativeReplay,
		http: &http.Client{
			Transport: transport,
			// No total Timeout: long generations are governed by a per-request
			// hard ceiling (context) plus an idle-read watchdog so a stalled
			// upstream is detected without killing healthy slow streams.
		},
	}
}

// StateDir returns the runtime-owned state directory for companion clients
// that need to place optional diagnostics beside the rest of the profile data.
func (c *Client) StateDir() string {
	if c == nil || c.cfg == nil {
		return ""
	}
	return c.cfg.StateDir
}

// GenerateContentResult bundles the upstream response plus capture metadata.
type GenerateContentResult struct {
	Status      int
	Body        string
	BodyBytes   int
	RequestBody string
	CaptureMeta *CaptureMeta
	ReadErr     error // non-nil when the body was truncated / read incompletely
}

// CaptureMeta mirrors the upstream capture fields surfaced to the caller.
type CaptureMeta struct {
	CapturedAt         string
	RequestURL         string
	PromptID           string
	PromptURL          string
	AccountFingerprint string
	AuthUser           string
	ProfileID          string
}

// GenerateOptions are the request-level options for a chat generation.
type GenerateOptions struct {
	converter.RequestOptions
	RequestID string
	// OnRawChunk is invoked for every raw TCP frame read from the upstream
	// body. It is the low-level streaming hook used by legacy/buffered paths.
	OnRawChunk func(text string) error
	// OnStreamChunk is invoked for each complete AI Studio GenerateContent
	// chunk decoded from the streaming JSON array. When set, the HTTP fast
	// path uses a streaming json.Decoder so chunks are delivered incrementally
	// instead of buffering the entire body. The full body is still captured
	// for the final authoritative parse.
	OnStreamChunk func(chunk []any) error
}

// GenerateContent sends a GenerateContent request. It prefers the HTTP fast
// path when a capture + cached auth + field4 are available.
func (c *Client) GenerateContent(ctx context.Context, opts GenerateOptions) (*GenerateContentResult, error) {
	// Warm caches best-effort.
	_ = c.warmUp(ctx)

	capture := c.botguard.GetCapture()
	authInfo := c.getCachedAuth()
	if capture != nil && authInfo != nil {
		result, err := c.tryHTTP(ctx, opts, capture, authInfo)
		if err == nil {
			switch result.Status {
			case http.StatusOK:
				c.rememberNativeToolCalls(result.Body, opts)
				return result, nil
			case http.StatusUnauthorized, http.StatusForbidden:
				// Fall through to the browser path on auth/field4 failures to
				// refresh runtime state on the same profile first.
			case http.StatusTooManyRequests:
				// Authentication refresh cannot repair a per-user quota. Return
				// immediately so the routing layer can rotate profiles once.
				return result, nil
			default:
				return result, nil
			}
		}
	}

	result, err := c.generateViaBrowser(ctx, opts)
	if err == nil && result != nil && result.Status == http.StatusOK {
		c.rememberNativeToolCalls(result.Body, opts)
	}
	return result, err
}

// rememberNativeToolCalls keeps AI Studio's opaque replay metadata server-side.
// OpenAI clients commonly discard unknown extension fields when they send an
// assistant tool call back with its result, but AI Studio requires these fields
// to replay the model function-call part.
func (c *Client) rememberNativeToolCalls(body string, opts GenerateOptions) {
	if !shouldUseNativeToolCalling(opts.ToolCallingMode) || body == "" {
		return
	}
	parsed := converter.ParseGenerateContentResponse(body, converter.ToolParseOptions{
		Tools:      opts.Tools,
		ToolChoice: opts.ToolChoice,
	})
	if len(parsed.FunctionCalls) == 0 {
		return
	}

	if c.nativeReplay == nil {
		return
	}
	if err := c.nativeReplay.Remember(parsed.FunctionCalls, c.profileID); err != nil {
		stdlog.Printf("[CHAT] native replay persist failed profile=%s: %v", c.profileID, err)
	}
}

func (c *Client) enrichNativeToolReplay(messages []models.Message) []models.Message {
	if c.nativeReplay == nil {
		return messages
	}
	return c.nativeReplay.Enrich(messages)
}

// tryHTTP executes the fast path using cached material.
func (c *Client) tryHTTP(ctx context.Context, opts GenerateOptions, capture *botguard.Capture, authInfo *auth.RuntimeAuth) (*GenerateContentResult, error) {
	capture, body, headers, err := c.buildSignedRequest(ctx, opts, capture, authInfo)
	if err != nil {
		return nil, err
	}

	// Derive a request-scoped context that:
	//   - inherits caller cancellation (client disconnect),
	//   - applies a hard ceiling (GenerateTimeout) as a safety net,
	//   - is cancelled by an idle-read watchdog when the upstream stalls for
	//     longer than GenerateIdleReadMs between frames.
	// This replaces the old total http.Client.Timeout, which killed healthy
	// long generations (e.g. gemini-3.1-pro with high thinking) mid-stream.
	idleCtx, idleCancel, watchdog := c.startReadWatchdog(ctx)
	defer idleCancel()

	req, err := http.NewRequestWithContext(idleCtx, http.MethodPost, capture.RequestURL, bytes.NewReader(body))
	if err != nil {
		idleCancel()
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		idleCancel()
		if werr := watchdog.Err(); werr != nil {
			return nil, fmt.Errorf("chat: upstream estagnou (idle read): %w", werr)
		}
		return nil, err
	}
	defer resp.Body.Close()

	status := resp.StatusCode
	contentLength := resp.ContentLength

	// Stream-aware body read. Read errors after bytes are received are
	// preserved so callers can refuse incomplete bodies instead of treating
	// a truncated payload as a clean completion.
	var full strings.Builder
	var readErr error
	var streamedChunks int
	switch {
	case status != http.StatusOK:
		// Error payloads are not GenerateContent chunk arrays (403 commonly
		// arrives as a short RPC error tuple). Read them raw so the caller keeps
		// the real HTTP status and can rotate/retry the account.
		_, readErr = io.ReadAll(io.TeeReader(resp.Body, &fullWriter{sb: &full, ctx: idleCtx}))
	case opts.OnStreamChunk != nil:
		// Preferred streaming path: decode the AI Studio JSON array chunk by
		// chunk using a streaming json.Decoder. A TeeReader captures the full
		// body so the final authoritative parse still has everything.
		streamedChunks, readErr = streamGenerateContentArray(resp.Body, &full, opts.OnStreamChunk, idleCtx)
	case opts.OnRawChunk != nil:
		buf := make([]byte, 32*1024)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				pingWatchdog(idleCtx)
				chunk := string(buf[:n])
				full.WriteString(chunk)
				if cerr := opts.OnRawChunk(chunk); cerr != nil {
					return nil, cerr
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				readErr = rerr
				break
			}
		}
	default:
		// TeeReader + fullWriter: espelha o body em full e pinga o watchdog a
		// cada frame, igual ao caminho de streaming.
		_, rerr := io.ReadAll(io.TeeReader(resp.Body, &fullWriter{sb: &full, ctx: idleCtx}))
		if rerr != nil {
			readErr = rerr
		}
	}
	if werr := watchdog.Err(); werr != nil && readErr == nil {
		readErr = werr
	}

	bodyText := full.String()
	result := &GenerateContentResult{
		Status:      status,
		Body:        bodyText,
		BodyBytes:   len(bodyText),
		RequestBody: string(body),
		ReadErr:     readErr,
		CaptureMeta: &CaptureMeta{
			CapturedAt:         capture.CapturedAt,
			RequestURL:         capture.RequestURL,
			PromptID:           capture.PromptID,
			PromptURL:          capture.PromptURL,
			AccountFingerprint: capture.AccountFingerprint,
			AuthUser:           capture.AuthUser,
			ProfileID:          c.profileID,
		},
	}
	if readErr != nil {
		// A watchdog idle-read timeout surfaces as context.Canceled on the
		// request. Promote it to a precise error so retries/migration can react.
		if errors.Is(readErr, context.Canceled) {
			if werr := watchdog.Err(); werr != nil {
				readErr = werr
			}
		}
		stdlog.Printf("[CHAT] HTTP body read incomplete profile=%s status=%d bytes=%d chunks=%d contentLength=%d err=%v snippet=%s",
			c.profileID, status, result.BodyBytes, streamedChunks, contentLength, readErr, logSnippet(result.Body))
		// If we already streamed chunks to the client, the partial body is
		// usable: return it with ReadErr populated so the streaming handler can
		// close the SSE gracefully instead of discarding delivered content.
		if streamedChunks > 0 && status == http.StatusOK {
			result.ReadErr = readErr
			return result, nil
		}
		// Incomplete body with nothing delivered is not a usable success.
		return nil, fmt.Errorf("chat: leitura incompleta do body upstream (%d bytes, content-length=%d): %w", result.BodyBytes, contentLength, readErr)
	}
	if contentLength > 0 && int64(result.BodyBytes) != contentLength {
		stdlog.Printf("[CHAT] HTTP body length mismatch profile=%s status=%d bytes=%d contentLength=%d",
			c.profileID, status, result.BodyBytes, contentLength)
	}
	if status != http.StatusOK {
		c.writeDebugUpstreamBody(body)
		stdlog.Printf("[CHAT] HTTP fast-path profile=%s status=%d body=%s", c.profileID, status, logSnippet(result.Body))
	} else if c.cfg != nil && c.cfg.Debug.MessageFlow {
		stdlog.Printf("[CHAT] HTTP fast-path ok profile=%s status=%d bodyBytes=%d contentLength=%d",
			c.profileID, status, result.BodyBytes, contentLength)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		c.InvalidateAuthCache()
		c.botguard.InvalidateField4()
		c.invalidateUploadContext()
	}
	return result, nil
}

// GenerateCustom assina (Botguard) e envia um payload MakerSuite arbitrario
// (ex.: TTS) por HTTP puro, reutilizando o mesmo transporte, watchdog de
// leitura e caches do chat. Nenhum browser participa do data path.
func (c *Client) GenerateCustom(ctx context.Context, payload []any, hashSource string) (*GenerateContentResult, error) {
	_ = c.warmUp(ctx)

	capture := c.botguard.GetCapture()
	authInfo := c.getCachedAuth()
	if capture == nil || authInfo == nil {
		// Sessao fria: bootstrap de sessao (botguard/cookies) e segue em HTTP.
		if err := c.botguard.EnsureReady(ctx); err != nil {
			return nil, err
		}
		capture = c.botguard.GetCapture()
		if capture == nil {
			return nil, errors.New("chat: capture indisponivel apos bootstrap")
		}
		var err error
		authInfo, err = c.buildAuth(ctx, capture)
		if err != nil {
			return nil, err
		}
		c.setAuth(authInfo)
	}

	if len(payload) > 4 && !field4Disabled() {
		if strings.TrimSpace(hashSource) == "" {
			hashSource = extractHashSource(payload[1])
		}
		field4, err := c.botguard.GenerateField4(ctx, hashSource)
		if err != nil {
			return nil, err
		}
		payload[4] = field4
	}

	body, err := marshalJSONNoEscape(payload)
	if err != nil {
		return nil, err
	}
	headers := c.buildNodeHeaders(capture, authInfo)

	idleCtx, idleCancel, watchdog := c.startReadWatchdog(ctx)
	defer idleCancel()

	req, err := http.NewRequestWithContext(idleCtx, http.MethodPost, capture.RequestURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if werr := watchdog.Err(); werr != nil {
			return nil, fmt.Errorf("chat: upstream estagnou (idle read): %w", werr)
		}
		return nil, err
	}
	defer resp.Body.Close()

	var full strings.Builder
	_, readErr := io.ReadAll(io.TeeReader(resp.Body, &fullWriter{sb: &full, ctx: idleCtx}))
	if readErr == nil {
		if werr := watchdog.Err(); werr != nil {
			readErr = werr
		}
	}
	if readErr != nil {
		if errors.Is(readErr, context.Canceled) {
			if werr := watchdog.Err(); werr != nil {
				readErr = werr
			}
		}
		return nil, fmt.Errorf("chat: leitura incompleta do body upstream (%d bytes): %w", full.Len(), readErr)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		c.InvalidateAuthCache()
		c.botguard.InvalidateField4()
		c.invalidateUploadContext()
	}

	return &GenerateContentResult{
		Status:      resp.StatusCode,
		Body:        full.String(),
		BodyBytes:   full.Len(),
		RequestBody: string(body),
		CaptureMeta: &CaptureMeta{
			CapturedAt:         capture.CapturedAt,
			RequestURL:         capture.RequestURL,
			PromptID:           capture.PromptID,
			PromptURL:          capture.PromptURL,
			AccountFingerprint: capture.AccountFingerprint,
			AuthUser:           capture.AuthUser,
			ProfileID:          c.profileID,
		},
	}, nil
}

// generateViaBrowser is the slow path used when the fast HTTP path has no
// valid capture/auth or hit an auth/quota status. It refreshes runtime state
// via the browser bootstrap (botguard + cookies — login-session territory)
// and then retries the generation over pure HTTP. The generation request
// itself NEVER goes through the browser: this proxy is full HTTP direct on
// the data path.
func (c *Client) generateViaBrowser(ctx context.Context, opts GenerateOptions) (*GenerateContentResult, error) {
	if err := c.botguard.EnsureReady(ctx); err != nil {
		return nil, err
	}

	capture := c.botguard.GetCapture()
	if capture == nil {
		return nil, errors.New("chat: capture indisponivel apos bootstrap")
	}

	authInfo, err := c.buildAuth(ctx, capture)
	if err != nil {
		return nil, err
	}
	if accountFingerprintChanged(capture, authInfo) {
		c.InvalidateAuthCache()
		c.botguard.InvalidateField4()
		c.botguard.InvalidateCapture()
		c.invalidateUploadContext()

		if err := c.botguard.EnsureReady(ctx); err != nil {
			return nil, err
		}
		capture = c.botguard.GetCapture()
		if capture == nil {
			return nil, errors.New("chat: capture indisponivel apos refresh de conta")
		}
		authInfo, err = c.buildAuth(ctx, capture)
		if err != nil {
			return nil, err
		}
		if accountFingerprintChanged(capture, authInfo) {
			return nil, errors.New("chat: conta ativa do perfil mudou")
		}
	}
	c.setAuth(authInfo)

	// Retry over pure HTTP with the refreshed capture/auth.
	result, err := c.tryHTTP(ctx, opts, capture, authInfo)
	if err != nil {
		return nil, err
	}
	if result.Status != http.StatusOK {
		stdlog.Printf("[CHAT] HTTP pos-refresh profile=%s authUser=%s visit=%t ext=%t api=%t auth=%t capture_fp=%s runtime_fp=%s",
			c.profileID,
			authInfo.AuthUser,
			authInfo.Headers["X-AIStudio-Visit-Id"] != "",
			authInfo.Headers["X-Goog-Ext-519733851-bin"] != "",
			authInfo.Headers["X-Goog-Api-Key"] != "",
			authInfo.Headers["Authorization"] != "",
			capture.AccountFingerprint,
			authInfo.AccountFingerprint,
		)
	}
	return result, nil
}

// warmUp populates the cached auth if a capture exists.
func (c *Client) warmUp(ctx context.Context) error {
	capture := c.botguard.GetCapture()
	if capture == nil {
		return nil
	}
	if c.getCachedAuth() != nil {
		return nil
	}
	authInfo, err := c.buildAuth(ctx, capture)
	if err != nil {
		return err
	}
	c.setAuth(authInfo)
	return nil
}

func (c *Client) buildAuth(ctx context.Context, capture *botguard.Capture) (*auth.RuntimeAuth, error) {
	var cookies []auth.Cookie
	if err := c.cdp.RunExclusive(ctx, "chat-auth-cookies", func(pageCtx context.Context) error {
		raw, err := c.cdp.GetCookies(pageCtx)
		if err != nil {
			return err
		}
		cookies = make([]auth.Cookie, 0, len(raw))
		for _, ck := range raw {
			cookies = append(cookies, auth.Cookie{Name: ck.Name, Value: ck.Value})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return auth.BuildRuntimeAuth(capture.RequestHeaders, cookies)
}

func (c *Client) buildPayloadFromCache(ctx context.Context, opts GenerateOptions, capture *botguard.Capture, authInfo *auth.RuntimeAuth) ([]any, error) {
	var template []any
	if err := json.Unmarshal(capture.TemplatePayload, &template); err != nil {
		return nil, fmt.Errorf("chat: template invalido: %w", err)
	}

	messages := opts.Messages
	if shouldUseNativeToolCalling(opts.ToolCallingMode) {
		messages = c.enrichNativeToolReplay(messages)
	}
	native := converter.NormalizeOpenAIMessages(messages, shouldUseNativeToolCalling(opts.ToolCallingMode))
	prepared, err := c.prepareMessages(ctx, native.Messages, capture, authInfo)
	if err != nil {
		return nil, err
	}
	converted := converter.MessagesToMakerSuite(prepared, "")

	systemInstruction := combineInstructions(native.SystemInstruction, opts.SystemInstruction)
	if c.cfg.Debug.MessageFlow {
		stdlog.Printf(
			"[REQ %s] msgflow upstream profile=%s input=%d normalized=%d prepared=%d sent=%d native=%t systemInstruction=%t roles_input=%s roles_normalized=%s roles_prepared=%s sig_input=%s sig_normalized=%s sig_prepared=%s",
			requestIDOrFallback(opts.RequestID),
			c.profileID,
			len(opts.Messages),
			len(native.Messages),
			len(prepared),
			len(converted),
			shouldUseNativeToolCalling(opts.ToolCallingMode),
			strings.TrimSpace(systemInstruction) != "",
			messageRoleSummary(opts.Messages),
			messageRoleSummary(native.Messages),
			messageRoleSummary(prepared),
			messageFlowSummary(opts.Messages),
			messageFlowSummary(native.Messages),
			messageFlowSummary(prepared),
		)
	}

	// upstream passou a exigir o ID completo do recurso ("models/<id>");
	// nomes bare ("gemini-3.6-flash") recebem 400 invalid argument
	model := orString(opts.Model, c.cfg.DefaultModel)
	if model != "" && !strings.HasPrefix(model, "models/") {
		model = "models/" + model
	}
	template[0] = model
	template[1] = converted
	template[2] = buildSafetyArray(opts.SafetySettings)
	if len(template) > 5 {
		template[5] = buildSystemInstructionSlot(systemInstruction)
	}
	if len(template) > 6 {
		template[6] = buildFunctionCallingSlot(opts.Tools, opts.ToolCallingMode)
	}
	if modelDisallowsTools(template[0]) && len(template) > 6 {
		template[6] = nil
	}

	configSlot := ensureConfigSlot(template, 3, 17)
	configSlot[3] = orInt(opts.MaxTokens, c.cfg.MaxTokensDefault)
	configSlot[4] = orFloat(opts.TopP, c.cfg.TopPDefault)
	configSlot[5] = orFloat(opts.Temperature, c.cfg.TemperatureDefault)
	configSlot[6] = c.cfg.TopKDefault

	if modelName, ok := template[0].(string); ok && strings.Contains(strings.ToLower(modelName), "gemma") {
		configSlot[13] = nil
		configSlot[16] = nil
	}
	if modelDisallowsThinking(template[0]) {
		configSlot[13] = nil
		configSlot[16] = nil
	}
	applyThinkingLevel(configSlot, template[0], opts.ThinkingLevel)
	applyImageAspectRatio(template, 3, template[0], opts.ImageAspectRatio)

	return template, nil
}

func (c *Client) buildNodeHeaders(capture *botguard.Capture, authInfo *auth.RuntimeAuth) map[string]string {
	headers := map[string]string{}
	freshKeys := map[string]bool{}
	for k := range authInfo.Headers {
		freshKeys[strings.ToLower(k)] = true
	}
	for k, v := range capture.RequestHeaders {
		if strings.HasPrefix(k, ":") {
			continue
		}
		lower := strings.ToLower(k)
		if lower == "content-length" || lower == "authorization" || lower == "cookie" || freshKeys[lower] {
			continue
		}
		headers[k] = v
	}
	for k, v := range authInfo.Headers {
		headers[k] = v
	}
	headers["Cookie"] = authInfo.CookieString
	if _, ok := headers["Origin"]; !ok {
		headers["Origin"] = "https://aistudio.google.com"
	}
	if _, ok := headers["Referer"]; !ok {
		headers["Referer"] = "https://aistudio.google.com/"
	}
	if _, ok := headers["Accept"]; !ok {
		headers["Accept"] = "*/*"
	}
	return headers
}

func (c *Client) getCachedAuth() *auth.RuntimeAuth {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cachedAuth
}

func (c *Client) setAuth(a *auth.RuntimeAuth) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cachedAuth = a
}

// InvalidateAuthCache clears the cached auth (e.g. on 401).
func (c *Client) InvalidateAuthCache() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cachedAuth = nil
	c.lastAuthFailAt = time.Now()
}

// IsReadyForRequests reports whether the fast path is fully primed.
func (c *Client) IsReadyForRequests() bool {
	if c.botguard == nil || !c.botguard.IsReady() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cachedAuth != nil
}
func (c *Client) WarmUpHttpCaches(ctx context.Context) (WarmedResult, error) {
	if c.botguard == nil || !c.botguard.IsReady() {
		return WarmedResult{Warmed: false, Reason: "botguard_not_ready"}, nil
	}
	capture := c.botguard.GetCapture()
	if capture == nil {
		return WarmedResult{Warmed: false, Reason: "no_capture"}, nil
	}
	if c.getCachedAuth() == nil {
		authInfo, err := c.buildAuth(ctx, capture)
		if err != nil {
			return WarmedResult{Warmed: false, Reason: err.Error()}, nil
		}
		c.setAuth(authInfo)
	}
	return WarmedResult{Warmed: true}, nil
}

// WarmedResult is the outcome of WarmUpHttpCaches.
type WarmedResult struct {
	Warmed bool
	Reason string
}

// --- helpers ---

func shouldUseNativeToolCalling(mode string) bool {
	return strings.ToLower(strings.TrimSpace(mode)) == "native_first"
}

func (c *Client) buildSignedRequest(ctx context.Context, opts GenerateOptions, capture *botguard.Capture, authInfo *auth.RuntimeAuth) (*botguard.Capture, []byte, map[string]string, error) {
	if capture == nil {
		return nil, nil, nil, errors.New("chat: capture indisponivel")
	}
	if authInfo == nil {
		return nil, nil, nil, errors.New("chat: auth indisponivel")
	}

	for attempt := 0; attempt < 2; attempt++ {
		payload, err := c.buildPayloadFromCache(ctx, opts, capture, authInfo)
		if err != nil {
			return nil, nil, nil, err
		}
		field4 := ""
		if !field4Disabled() {
			hashSource := extractHashSource(payload[1])
			field4, err = c.botguard.GenerateField4(ctx, hashSource)
			if err != nil {
				return nil, nil, nil, err
			}
		}

		freshCapture := c.botguard.GetCapture()
		if attempt == 0 && captureChanged(capture, freshCapture) {
			capture = freshCapture
			if capture == nil {
				return nil, nil, nil, errors.New("chat: capture indisponivel apos refresh do botguard")
			}
			authInfo, err = c.buildAuth(ctx, capture)
			if err != nil {
				return nil, nil, nil, err
			}
			c.setAuth(authInfo)
			continue
		}

		if field4Disabled() {
			payload[4] = nil // omite de vez (teste: upstream exige assinatura?)
		} else {
			payload[4] = field4
		}
		body, err := marshalJSONNoEscape(payload)
		if err != nil {
			return nil, nil, nil, err
		}
		headers := c.buildNodeHeaders(capture, authInfo)
		return capture, body, headers, nil
	}

	return nil, nil, nil, errors.New("chat: nao foi possivel assinar requisicao apos refresh do botguard")
}

type browserFetchResponse struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
	Error  string `json:"error"`
}

// fetchGenerateContentViaBrowser foi removido: a geracao e full HTTP direct.
// O browser so participa do bootstrap de sessao (botguard/cookies/login).

func extractHashSource(messagesField any) string {
	messages, ok := messagesField.([]any)
	if !ok {
		return ""
	}

	// AI Studio hashes every content part in request order. Text contributes
	// its text, inline data contributes only the encoded bytes, file parts
	// contribute the file ID, and every other part (including native function
	// calls/results) contributes an empty string. The empty entries matter:
	// strings.Join preserves their separator spaces in the signed source.
	fragments := make([]string, 0)
	for _, messageNode := range messages {
		message, ok := messageNode.([]any)
		if !ok || len(message) == 0 {
			continue
		}
		parts, ok := message[0].([]any)
		if !ok {
			continue
		}
		for _, partNode := range parts {
			part, ok := partNode.([]any)
			if !ok {
				continue
			}
			fragments = append(fragments, hashFragmentForPart(part))
		}
	}
	return strings.Join(fragments, " ")
}

func hashFragmentForPart(part []any) string {
	if len(part) >= 2 && part[0] == nil {
		if text, ok := part[1].(string); ok {
			return text
		}
	}
	if len(part) >= 3 && part[0] == nil && part[1] == nil {
		if inline, ok := part[2].([]any); ok && len(inline) >= 2 {
			if data, ok := inline[1].(string); ok {
				return data
			}
		}
	}
	if len(part) >= 6 && part[0] == nil && part[1] == nil {
		if file, ok := part[5].([]any); ok && len(file) > 0 {
			if id, ok := file[0].(string); ok {
				return id
			}
		}
	}
	return ""
}

func captureChanged(before, after *botguard.Capture) bool {
	if before == nil || after == nil {
		return before != after
	}
	return before.CapturedAt != after.CapturedAt ||
		before.RequestURL != after.RequestURL ||
		before.AuthUser != after.AuthUser ||
		before.AccountFingerprint != after.AccountFingerprint
}

func accountFingerprintChanged(capture *botguard.Capture, authInfo *auth.RuntimeAuth) bool {
	if capture == nil || authInfo == nil {
		return false
	}
	if strings.TrimSpace(capture.AccountFingerprint) == "" || strings.TrimSpace(authInfo.AccountFingerprint) == "" {
		return false
	}
	return capture.AccountFingerprint != authInfo.AccountFingerprint
}

func buildSafetyArray(safety json.RawMessage) any {
	if len(safety) == 0 {
		return safetyTuples(5)
	}
	var s string
	if json.Unmarshal(safety, &s) == nil {
		switch strings.ToLower(s) {
		case "off":
			return safetyTuples(5)
		case "low":
			return safetyTuples(3)
		}
		return safetyTuples(5)
	}
	var arr []any
	if json.Unmarshal(safety, &arr) == nil {
		return arr
	}
	return safetyTuples(5)
}

func safetyTuples(threshold int) []any {
	return []any{
		[]any{nil, nil, 7, threshold},
		[]any{nil, nil, 8, threshold},
		[]any{nil, nil, 9, threshold},
		[]any{nil, nil, 10, threshold},
	}
}

func buildSystemInstructionSlot(text string) any {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	return []any{[]any{[]any{nil, text}}, "user"}
}

func buildFunctionCallingSlot(tools []models.Tool, toolCallingMode string) any {
	return converter.BuildFunctionCallingSlot(tools, toolCallingMode)
}

func ensureConfigSlot(template []any, idx int, minLen int) []any {
	if idx >= len(template) {
		return nil
	}
	slot, ok := template[idx].([]any)
	if !ok {
		slot = make([]any, minLen)
		template[idx] = slot
	}
	for len(slot) < minLen {
		slot = append(slot, nil)
	}
	template[idx] = slot
	return slot
}

func applyThinkingLevel(configSlot []any, model any, level string) {
	if len(configSlot) == 0 {
		return
	}
	if modelDisallowsThinking(model) {
		configSlot[16] = nil
		return
	}
	slot := mapThinkingLevelSlot(model, level)
	if slot == nil {
		return
	}
	configSlot[16] = slot
}

func modelDisallowsThinking(model any) bool {
	modelName, _ := model.(string)
	modelName = strings.ToLower(modelName)
	return strings.Contains(modelName, "gemini-2.5-flash-image") ||
		strings.Contains(modelName, "gempix")
}

func modelDisallowsTools(model any) bool {
	return modelDisallowsThinking(model)
}

func applyImageAspectRatio(template []any, configIdx int, model any, ratio string) {
	if !modelDisallowsThinking(model) {
		return
	}
	configSlot := ensureConfigSlot(template, configIdx, 27)
	if len(configSlot) <= 26 {
		return
	}
	ratio = strings.TrimSpace(ratio)
	if ratio == "" {
		configSlot[26] = nil
		return
	}
	configSlot[26] = []any{ratio}
}

func mapThinkingLevelSlot(model any, level string) []any {
	l := strings.ToLower(strings.TrimSpace(level))
	if l == "" {
		return nil
	}
	numericMap := map[string]int{"low": 1, "medium": 2, "high": 3, "minimal": 4}
	n, ok := numericMap[l]
	if !ok {
		return nil
	}
	modelName := ""
	if s, ok := model.(string); ok {
		modelName = strings.ToLower(s)
	}
	if strings.Contains(modelName, "gemma-4-31b-it") && (l != "minimal" && l != "high") {
		return nil
	}
	if strings.Contains(modelName, "gemini-3.1-pro-preview") && !(l == "low" || l == "medium" || l == "high") {
		return nil
	}
	if strings.Contains(modelName, "gemini-3-flash-preview") && !(l == "minimal" || l == "low" || l == "medium" || l == "high") {
		return nil
	}
	return []any{1, nil, nil, n}
}

func combineInstructions(values ...string) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			parts = append(parts, v)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// field4Disabled reports whether the per-request Botguard signature (field 4)
// should be skipped. Test switch to verify whether the AI Studio upstream
// actually enforces the botguard token on GenerateContent: se aceitar sem,
// o proxy roda 100% HTTP apos o login, sem Chrome em runtime.
func field4Disabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AISTUDIO_DISABLE_FIELD4")))
	return v == "1" || v == "true" || v == "yes"
}

func orString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func orInt(value *int, fallback int) any {
	if value == nil {
		return fallback
	}
	return *value
}

func orFloat(value *float64, fallback float64) any {
	if value == nil {
		return fallback
	}
	return *value
}

func logSnippet(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "<empty>"
	}
	text = strings.ReplaceAll(text, "\r", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	if len(text) > 400 {
		return text[:400] + "..."
	}
	return text
}

func requestIDOrFallback(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "no-request-id"
	}
	return value
}

func messageRoleSummary(messages []models.Message) string {
	if len(messages) == 0 {
		return "<empty>"
	}
	parts := make([]string, 0, len(messages))
	limit := len(messages)
	if limit > 24 {
		limit = 24
	}
	for i := 0; i < limit; i++ {
		role := strings.TrimSpace(messages[i].Role)
		if role == "" {
			role = "?"
		}
		parts = append(parts, role)
	}
	if len(messages) > limit {
		parts = append(parts, fmt.Sprintf("...(+%d)", len(messages)-limit))
	}
	return strings.Join(parts, ">")
}

func messageFlowSummary(messages []models.Message) string {
	if len(messages) == 0 {
		return "<empty>"
	}
	parts := make([]string, 0, len(messages))
	limit := len(messages)
	if limit > 16 {
		limit = 16
	}
	for i := 0; i < limit; i++ {
		parts = append(parts, messageSignature(i, messages[i]))
	}
	if len(messages) > limit {
		parts = append(parts, fmt.Sprintf("...(+%d)", len(messages)-limit))
	}
	return strings.Join(parts, " | ")
}

func messageSignature(index int, msg models.Message) string {
	role := strings.TrimSpace(msg.Role)
	if role == "" {
		role = "?"
	}
	if s := plainContent(msg.Content); s != "" {
		return fmt.Sprintf("%d:%s:text:%d:%s", index, role, len(s), shortHash(s))
	}
	if len(msg.ToolCalls) > 0 {
		return fmt.Sprintf("%d:%s:toolcalls:%d:%s", index, role, len(msg.ToolCalls), shortHash(string(msg.Content)))
	}
	if len(msg.Content) > 0 {
		return fmt.Sprintf("%d:%s:json:%d:%s", index, role, len(msg.Content), shortHash(string(msg.Content)))
	}
	if msg.ToolCallID != "" {
		return fmt.Sprintf("%d:%s:tool-result:%s", index, role, shortHash(msg.ToolCallID))
	}
	return fmt.Sprintf("%d:%s:empty", index, role)
}

func shortHash(value string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(value))
	return fmt.Sprintf("%08x", h.Sum32())
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

func (c *Client) writeDebugUpstreamBody(body []byte) {
	if len(body) == 0 || c.cfg == nil {
		return
	}
	dir := filepath.Join(c.cfg.StateDir, "debug")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "go-last-upstream-body.json"), body, 0o600)
}

func marshalJSONNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

// keep jsonx referenced for DecodeLenient helpers used elsewhere.
var _ = jsonx.Decode
