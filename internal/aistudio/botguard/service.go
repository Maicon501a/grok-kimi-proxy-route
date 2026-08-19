// Package botguard bootstraps and caches the AI Studio Botguard capture
// (request URL/headers/template + gyb callback) used to sign GenerateContent
// requests.
package botguard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cdprotocdp "github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"grok-desktop/internal/aistudio/auth"
	"grok-desktop/internal/aistudio/cdp"
	"grok-desktop/internal/aistudio/config"
)

const (
	chatURLEndpoint         = "https://aistudio.google.com/prompts/new_chat"
	bootstrapPrompt         = "."
	bootstrapTimeout        = 90 * time.Second
	countTokensEndpoint     = "MakerSuiteService/CountTokens"
	generateContentEndpoint = "MakerSuiteService/GenerateContent"
)

// Capture bundles the captured runtime material needed to build GenerateContent
// requests.
type Capture struct {
	CapturedAt         string            `json:"capturedAt"`
	RequestURL         string            `json:"requestUrl"`
	RequestHeaders     map[string]string `json:"requestHeaders"`
	TemplatePayload    json.RawMessage   `json:"templatePayload"`
	PromptURL          string            `json:"promptUrl"`
	PromptID           string            `json:"promptId"`
	AccountFingerprint string            `json:"accountFingerprint"`
	AuthUser           string            `json:"authUser"`
}

// Service owns the Botguard capture lifecycle for a profile.
type Service struct {
	cdp       *cdp.Client
	cfg       *config.Config
	profileID string

	mu               sync.Mutex
	capture          *Capture
	ready            bool
	bootstrapInProg  bool
	bootstrapErr     error
	cachedField4     string
	cachedField4Hash string
}

// New creates a Service bound to a CDP client.
func New(c *cdp.Client, cfg *config.Config, profileID string) *Service {
	return &Service{cdp: c, cfg: cfg, profileID: profileID}
}

// GetCapture returns the current capture, if any.
func (s *Service) GetCapture() *Capture {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.capture == nil {
		return nil
	}
	cp := *s.capture
	return &cp
}

// IsReady reports whether a capture is available.
func (s *Service) IsReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready && s.capture != nil
}

// EnsureReady bootstraps the capture if missing, optionally waiting for a
// concurrent bootstrap to finish.
func (s *Service) EnsureReady(ctx context.Context) error {
	s.mu.Lock()
	if s.ready && s.capture != nil {
		s.mu.Unlock()
		return nil
	}
	if s.bootstrapInProg {
		s.mu.Unlock()
		deadline := time.Now().Add(bootstrapTimeout)
		for time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
			s.mu.Lock()
			if s.ready && s.capture != nil {
				s.mu.Unlock()
				return nil
			}
			if !s.bootstrapInProg {
				err := s.bootstrapErr
				s.mu.Unlock()
				if err != nil {
					return err
				}
				return errors.New("botguard: bootstrap concorrente terminou sem captura")
			}
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		return errors.New("botguard: bootstrap concorrente nao completou")
	}
	s.bootstrapInProg = true
	s.bootstrapErr = nil
	s.mu.Unlock()

	err := s.bootstrap(ctx)
	s.mu.Lock()
	s.bootstrapErr = err
	s.bootstrapInProg = false
	s.mu.Unlock()
	return err
}

// GenerateField4 returns the Botguard signature for a content hash, caching it.
//
// Modo de teste AISTUDIO_STATIC_FIELD4=1: assina uma unica vez e reusa o
// mesmo token para todo conteudo, para verificar empiricamente se o upstream
// valida o token contra o conteudo da mensagem. Se aceitar, o proxy pode
// operar sem Chrome em runtime apos um unico bootstrap.
//
// Modo AISTUDIO_FIELD4_CMD: em vez do browser, executa um comando externo
// (ex.: o runner Node do Botguard) passando o hash como ultimo argumento e
// lendo o token do stdout. E o caminho do bootstrap full HTTP direct: a VM
// Botguard roda em Node puro, sem Chrome nenhum na geracao da assinatura.
func (s *Service) GenerateField4(ctx context.Context, hashSource string) (string, error) {
	if cmd := field4ExternalCmd(); cmd != "" {
		return s.generateField4External(ctx, cmd, hashSource)
	}
	if err := s.EnsureReady(ctx); err != nil {
		return "", err
	}
	hash := auth.SHA256Hex(hashSource)
	if staticField4Enabled() {
		hash = "static"
	}
	s.mu.Lock()
	if s.cachedField4 != "" && s.cachedField4Hash == hash {
		v := s.cachedField4
		s.mu.Unlock()
		return v, nil
	}
	s.mu.Unlock()

	value, err := s.callGyb(ctx, hash)
	if err != nil {
		if !shouldRebootstrapAfterGybError(err) {
			return "", err
		}
		s.invalidateCapture()
		if bootErr := s.EnsureReady(ctx); bootErr != nil {
			return "", bootErr
		}
		value, err = s.callGyb(ctx, hash)
		if err != nil {
			return "", err
		}
	}
	s.mu.Lock()
	s.cachedField4 = value
	s.cachedField4Hash = hash
	s.mu.Unlock()
	return value, nil
}

// InvalidateField4 clears the cached field4 token.
func (s *Service) InvalidateField4() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cachedField4 = ""
	s.cachedField4Hash = ""
}

// HasCachedField4 reports whether a cached field4 token exists.
func (s *Service) HasCachedField4() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cachedField4 != ""
}

func (s *Service) invalidateCapture() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidateCaptureLocked()
}

// InvalidateCapture clears the cached capture so the next request forces a
// fresh bootstrap on the same profile.
func (s *Service) InvalidateCapture() {
	s.invalidateCapture()
}

func (s *Service) invalidateCaptureLocked() {
	s.capture = nil
	s.ready = false
	s.cachedField4 = ""
	s.cachedField4Hash = ""
}

func (s *Service) bootstrap(ctx context.Context) error {
	return s.cdp.RunExclusive(ctx, "botguard-bootstrap", func(pageCtx context.Context) error {
		if err := s.cdp.InstallBotguardHooks(pageCtx); err != nil {
			return err
		}

		// Force the Windows Chrome User-Agent BEFORE navigating to AI
		// Studio. Without this, the Chromium headless default
		// "HeadlessChrome/..." leaks into the captured
		// EventRequestWillBeSent headers, and Google rejects the
		// subsequent HTTP fast-path calls with 403 ("The caller does
		// not have permission") because it detects headless browsers.
		// The CDP-level SetUserAgentOverride in cdp/client.go's
		// applyManagedPageSpoofLocked() is supposed to do this, but
		// it depends on shouldApplyManagedSpoof() which can silently
		// skip the override. Apply it here unconditionally so the
		// bootstrap capture always carries a believable UA.
		if err := s.cdp.ApplyUserAgentOverride(pageCtx, s.cfg.AIStudio.VisibleUserAgent); err != nil {
			return fmt.Errorf("botguard: user-agent override falhou: %w", err)
		}

		// Debug: troca o loader do Botguard pelo instrumentado (catches da VM
		// reportam via window.__bgErr) para comparar browser real vs Node.
		if strings.ToLower(strings.TrimSpace(os.Getenv("AISTUDIO_DEBUG_BG_SWAP"))) == "1" {
			if instBody, rerr := os.ReadFile(filepath.Join(s.cfg.StateDir, "bg-loader-inst.js")); rerr == nil {
				execCtx := pageCtx
				if c := chromedp.FromContext(pageCtx); c != nil && c.Target != nil {
					execCtx = cdprotocdp.WithExecutor(pageCtx, c.Target)
				}
				_ = fetch.Enable().WithPatterns([]*fetch.RequestPattern{
					{URLPattern: "*/js/bg/*", RequestStage: fetch.RequestStageRequest},
				}).Do(execCtx)
				chromedp.ListenTarget(pageCtx, func(ev interface{}) {
					paused, ok := ev.(*fetch.EventRequestPaused)
					if !ok {
						return
					}
					go func() {
						_ = fetch.FulfillRequest(paused.RequestID, 200).
							WithResponseHeaders([]*fetch.HeaderEntry{{Name: "Content-Type", Value: "application/javascript"}}).
							WithBody(base64.StdEncoding.EncodeToString(instBody)).
							Do(execCtx)
						log.Printf("[BGSWAP] loader instrumentado servido para %s", paused.Request.URL)
					}()
				})
			}
		}

		// Set up network interception.
		debugNet := strings.ToLower(strings.TrimSpace(os.Getenv("AISTUDIO_DEBUG_BG_NET"))) == "1"
		waaReqs := map[network.RequestID]string{}
		allReqs := map[network.RequestID]string{}
		chromedp.ListenTarget(pageCtx, func(ev interface{}) {
			switch ev := ev.(type) {
			case *network.EventRequestWillBeSent:
				if debugNet && ev.Request != nil {
					log.Printf("[BGNET] %s %s", ev.Request.Method, ev.Request.URL)
					allReqs[ev.RequestID] = ev.Request.URL
				}
				if debugNet && ev.Request != nil && strings.Contains(ev.Request.URL, "waa-pa") {
					waaReqs[ev.RequestID] = ev.Request.URL
					hdrJSON, _ := json.Marshal(ev.Request.Headers)
					log.Printf("[BGNET-WAA-HDR] %s", hdrJSON)
					for _, entry := range ev.Request.PostDataEntries {
						if entry == nil || entry.Bytes == "" {
							continue
						}
						log.Printf("[BGNET-WAA-REQ] entry(b64)=%s", entry.Bytes)
					}
				}
			case *network.EventResponseReceived:
				if debugNet && ev.Response != nil && (strings.Contains(ev.Response.URL, "MakerSuiteService/CountTokens") || strings.Contains(ev.Response.URL, "MakerSuiteService/GenerateContent")) {
					log.Printf("[BGNET] CT/GC resposta status=%d url=%s", ev.Response.Status, ev.Response.URL)
				}
			case *network.EventLoadingFinished:
				url, ok := waaReqs[ev.RequestID]
				if ok {
					delete(waaReqs, ev.RequestID)
				}
				allURL, allOK := allReqs[ev.RequestID]
				if allOK {
					delete(allReqs, ev.RequestID)
				}
				if !ok && !(allOK && !strings.Contains(allURL, "gstatic.com") && !strings.Contains(allURL, "fonts") && !strings.Contains(allURL, "googletagmanager") && !strings.Contains(allURL, ".png") && !strings.Contains(allURL, ".svg")) {
					return
				}
				saveAs := ""
				if ok {
					saveAs = s.debugPath(fmt.Sprintf("debug-waa-program-%d.bin", time.Now().UnixNano()))
				} else if allOK && strings.Contains(allURL, "MakerSuiteService/ListModels") {
					saveAs = s.debugPath("go-last-listmodels.json")
				}
				go func() {
					execCtx := pageCtx
					if c := chromedp.FromContext(pageCtx); c != nil && c.Target != nil {
						execCtx = cdprotocdp.WithExecutor(pageCtx, c.Target)
					}
					body, err := network.GetResponseBody(ev.RequestID).Do(execCtx)
					if err != nil {
						log.Printf("[BGNET-WAA-RESP] erro lendo body: %v", err)
						return
					}
					if saveAs != "" {
						log.Printf("[BGNET-WAA-RESP] %s bodyLen=%d", url, len(body))
						_ = os.WriteFile(saveAs, body, 0o600)
						return
					}
					// varredura: procura o programa botguard (prefixo XrH) nas
					// respostas RPC do AI Studio
					if idx := strings.Index(string(body), "XrH"); idx >= 0 {
						log.Printf("[BGNET-SCAN] %s contem XrH bodyLen=%d", allURL, len(body))
						s.writeDebugFile(fmt.Sprintf("debug-xrh-response-%d.bin", time.Now().UnixNano()), body, 0o600)
					}
				}()
			}
		})

		captureCh := make(chan rawCapture, 1)
		pending := &ctPendingStore{}
		listenForCountTokens(pageCtx, captureCh, pending)
		go s.pollCtJsCapture(pageCtx, captureCh, pending)

		if err := chromedp.Run(pageCtx,
			chromedp.Navigate(chatURLEndpoint),
			chromedp.Sleep(1500*time.Millisecond),
		); err != nil {
			return err
		}

		// Fail-fast: if the browser was redirected to /welcome or any
		// non-chat URL, the profile is not authenticated. Without a
		// logged-in session the AI Studio page never fires the
		// CountTokens request and we would sit here for
		// bootstrapTimeout (90s) just to fail anyway. Detect this
		// early and return a specific error so the router can
		// invalidate the account instead of putting it on a long
		// runtime cooldown.
		if unauth, reason := s.detectUnauthenticatedSession(pageCtx); unauth {
			return fmt.Errorf("botguard: sessao nao autenticada (%s) - reimporte os cookies", reason)
		}

		_ = s.cdp.DismissInterferingUI(pageCtx)
		_ = s.cdp.FillPrompt(pageCtx, bootstrapPrompt)

		select {
		case raw := <-captureCh:
			return s.finalizeCapture(pageCtx, raw)
		case <-time.After(bootstrapTimeout):
			return errors.New("botguard: timeout esperando bootstrap do CountTokens")
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

// detectUnauthenticatedSession checks whether the browser ended up on a
// non-chat page after navigating to chatURLEndpoint. Google redirects
// unauthenticated users to /welcome (or a login page), in which case the
// CountTokens network event will never fire. Returns (true, reason) when
// the session looks unauthenticated.
func (s *Service) detectUnauthenticatedSession(ctx context.Context) (bool, string) {
	var currentURL string
	if err := chromedp.Run(ctx, chromedp.Location(&currentURL)); err != nil {
		return false, ""
	}
	currentURL = strings.TrimSpace(currentURL)

	// AI Studio keeps logged-in users under /prompts/...; everything
	// else is a redirect chain that ended on a non-chat page.
	switch {
	case strings.Contains(currentURL, "/prompts/"):
		// Looks authenticated. Still, double-check that a SAPISID
		// cookie exists for the Google domains — if cookies were
		// written to SQLite but cannot be decrypted (the Linux
		// keyring bug we had before), the browser behaves as if
		// logged-out even though the URL briefly shows /prompts/.
		cookies, err := s.cdp.GetCookies(ctx)
		if err != nil {
			return false, ""
		}
		hasAuth := false
		for _, c := range cookies {
			if c == nil {
				continue
			}
			if c.Name == "SAPISID" || c.Name == "__Secure-3PAPISID" {
				hasAuth = true
				break
			}
		}
		if !hasAuth {
			return true, "cookies SAPISID ausentes no browser"
		}
		return false, ""
	case strings.Contains(currentURL, "/welcome"),
		strings.Contains(currentURL, "accounts.google.com"),
		strings.Contains(currentURL, "/signin"),
		strings.Contains(currentURL, "/ServiceLogin"),
		currentURL == "", strings.HasSuffix(currentURL, "aistudio.google.com/"):
		return true, "url=" + currentURL
	default:
		// Unknown URL; let the bootstrap continue and rely on the
		// CountTokens timeout if it really is unauthenticated.
		return false, ""
	}
}

// ctPendingStore guarda URL+headers do request CountTokens/GenerateContent
// cujo body nao veio pelo CDP (upload streamed: hasPostData=false e
// getRequestPostData retorna -32000). O body e lido pelo hook JS de fetch
// (window.__codexCtCapture) e combinado aqui pelo poller.
type ctPendingStore struct {
	mu      sync.Mutex
	url     string
	headers map[string]string
}

func (p *ctPendingStore) set(url string, headers map[string]string) {
	p.mu.Lock()
	p.url = url
	p.headers = headers
	p.mu.Unlock()
}

func (p *ctPendingStore) get() (string, map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.url, p.headers
}

// pollCtJsCapture le window.__codexCtCapture (gravado pelo hook de fetch
// injetado em scripts.go) e emite o rawCapture quando o evento de rede
// correspondente ja forneceu URL+headers.
func (s *Service) pollCtJsCapture(ctx context.Context, out chan<- rawCapture, pending *ctPendingStore) {
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	diagCount := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		url, headers := pending.get()
		if strings.ToLower(strings.TrimSpace(os.Getenv("AISTUDIO_DEBUG_BG_NET"))) == "1" && diagCount < 40 {
			diagCount++
			var diag string
			if err := chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify({fetchHook: typeof window.fetch==='function' && !!window.fetch.__codexCt, xhrHook: typeof XMLHttpRequest!=='undefined' && !!XMLHttpRequest.prototype.__codexCt, capture: JSON.stringify(window.__codexCtCapture||null).slice(0,300), seen: JSON.stringify(window.__codexCtLastSeen||null).slice(0,220), saves: window.__codexCtSaves||0, seenN: window.__codexCtSeen||0, href: location.href.slice(0,60)})`, &diag)); err == nil {
				log.Printf("[BGNET] CT-hook diag pend=%v: %s", url != "", diag)
			} else {
				log.Printf("[BGNET] CT-hook diag falhou: %v", err)
			}
		}
		if url == "" {
			continue
		}
		var body string
		if err := chromedp.Run(ctx, chromedp.Evaluate(`(window.__codexCtCapture && window.__codexCtCapture.body) || ''`, &body)); err != nil || body == "" {
			continue
		}
		select {
		case out <- rawCapture{URL: url, Headers: headers, Template: []byte(body)}:
			if strings.ToLower(strings.TrimSpace(os.Getenv("AISTUDIO_DEBUG_BG_NET"))) == "1" {
				log.Printf("[BGNET] capture via JS hook url=%s bodyLen=%d", url, len(body))
			}
		default:
		}
		return
	}
}

func listenForCountTokens(ctx context.Context, out chan<- rawCapture, pending *ctPendingStore) {
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		req, ok := ev.(*network.EventRequestWillBeSent)
		if !ok {
			return
		}
		url := req.Request.URL
		var targetURL string
		switch {
		case contains(url, countTokensEndpoint):
			targetURL = replaceFirst(url, countTokensEndpoint, generateContentEndpoint)
		case contains(url, generateContentEndpoint):
			targetURL = url
		default:
			return
		}
		if strings.ToLower(strings.TrimSpace(os.Getenv("AISTUDIO_DEBUG_BG_NET"))) == "1" {
			initJSON, _ := json.Marshal(req.Initiator)
			log.Printf("[BGNET] CT/GC evento method=%s hasPostData=%v entries=%d initiator=%s", req.Request.Method, req.Request.HasPostData, len(req.Request.PostDataEntries), string(initJSON)[:min(200, len(initJSON))])
		}
		var template []byte
		for _, entry := range req.Request.PostDataEntries {
			if entry == nil {
				continue
			}
			if entry.Bytes == "" {
				continue
			}
			decoded, err := base64Decode(entry.Bytes)
			if err == nil {
				template = decoded
				break
			}
			template = []byte(entry.Bytes)
			break
		}
		if template == nil {
			// Upload streamed: body nao vem no CDP. Registra URL+headers e
			// deixa o poller do hook JS completar com o body.
			pending.set(targetURL, toHeaderMap(req.Request.Headers))
			if strings.ToLower(strings.TrimSpace(os.Getenv("AISTUDIO_DEBUG_BG_NET"))) == "1" {
				initJSON, _ := json.Marshal(req.Initiator)
				log.Printf("[BGNET] CountTokens/GenerateContent sem postData no CDP (streamed) url=%s initiator=%s", url, string(initJSON)[:min(400, len(initJSON))])
			}
			return
		}
		select {
		case out <- rawCapture{URL: targetURL, Headers: toHeaderMap(req.Request.Headers), Template: template}:
		default:
		}
	})
}

func (s *Service) finalizeCapture(ctx context.Context, raw rawCapture) error {
	if strings.ToLower(strings.TrimSpace(os.Getenv("AISTUDIO_DEBUG_BG_NET"))) == "1" {
		s.writeDebugFile("go-last-capture-template.json", raw.Template, 0o600)
		var callLog string
		if err := chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify(window.__codexBgCallLog || [])`, &callLog)); err == nil {
			log.Printf("[BGNET-CALLS] %s", callLog)
		}
		var telemetry string
		if err := chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify(window.__codexBgTelemetry || [])`, &telemetry)); err == nil {
			s.writeDebugFile("debug-bg-telemetry-browser.json", []byte(telemetry), 0o600)
			log.Printf("[BGNET-TELEMETRY] %d eventos", len(telemetry))
		}
		var errLog string
		if err := chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify(window.__bgErrLog || [])`, &errLog)); err == nil {
			s.writeDebugFile("debug-bg-errlog-browser.json", []byte(errLog), 0o600)
			log.Printf("[BGNET-ERRLOG] %d chars", len(errLog))
		}
		var probeLog string
		if err := chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify(window.__probeLog || [])`, &probeLog)); err == nil {
			s.writeDebugFile("debug-bg-probelog-browser.json", []byte(probeLog), 0o600)
			log.Printf("[BGNET-PROBELOG] %d chars", len(probeLog))
		}
	}
	// Evaluate returns the inner JSON object directly (chromedp decodes objects).
	var runtime struct {
		HasGyb   bool   `json:"hasGyb"`
		URL      string `json:"url"`
		TimeZone string `json:"timeZone"`
	}
	expr := `JSON.stringify({ hasGyb: typeof window.__codexCapturedGyb === 'function', url: location.href, timeZone: (Intl.DateTimeFormat().resolvedOptions().timeZone || 'America/Sao_Paulo') })`
	// Evaluate decodes the returned JSON object into runtime; if a string is
	// returned (double-encoded), decode it again.
	var rawJSON json.RawMessage
	if err := s.cdp.Evaluate(ctx, expr, &rawJSON); err != nil {
		return err
	}
	if err := decodeMaybeString(rawJSON, &runtime); err != nil {
		return err
	}
	if !runtime.HasGyb {
		return errors.New("botguard: __codexCapturedGyb nao disponivel apos bootstrap")
	}

	cookies, err := s.cdp.GetCookies(ctx)
	if err != nil {
		return err
	}
	authCookies := make([]auth.Cookie, 0, len(cookies))
	for _, c := range cookies {
		authCookies = append(authCookies, auth.Cookie{Name: c.Name, Value: c.Value})
	}
	authInfo, err := auth.BuildRuntimeAuth(raw.Headers, authCookies)
	authUser := "0"
	if err == nil {
		authUser = authInfo.AuthUser
	}

	var model string
	if len(raw.Template) > 0 {
		var arr []any
		if json.Unmarshal(raw.Template, &arr) == nil && len(arr) > 0 {
			if s, ok := arr[0].(string); ok {
				model = s
			}
		}
	}
	if model == "" {
		model = s.cfg.DefaultModel
	}

	template := buildBootstrapTemplatePayload(model, runtime.TimeZone, s.cfg)

	s.mu.Lock()
	s.capture = &Capture{
		CapturedAt:         time.Now().UTC().Format(time.RFC3339),
		RequestURL:         raw.URL,
		RequestHeaders:     raw.Headers,
		TemplatePayload:    template,
		PromptURL:          runtime.URL,
		PromptID:           extractPromptIDFromURL(runtime.URL),
		AccountFingerprint: authFingerprint(authInfo),
		AuthUser:           authUser,
	}
	s.ready = true
	s.cachedField4 = ""
	s.cachedField4Hash = ""
	s.mu.Unlock()
	return nil
}

func staticField4Enabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AISTUDIO_STATIC_FIELD4")))
	return v == "1" || v == "true" || v == "yes"
}

// field4ExternalCmd retorna o comando externo de assinatura (modo full HTTP
// direct com a VM Botguard rodando em Node, sem Chrome). Formato: comando
// com argumentos; o hash do conteudo e passado como ultimo argumento e o
// token e lido da primeira linha do stdout.
func field4ExternalCmd() string {
	return strings.TrimSpace(os.Getenv("AISTUDIO_FIELD4_CMD"))
}

// generateField4External assina o hash via comando externo (ex.: runner Node
// da VM Botguard). Mantem o mesmo cache por hash do caminho via browser.
func (s *Service) generateField4External(ctx context.Context, cmdLine, hashSource string) (string, error) {
	hash := auth.SHA256Hex(hashSource)
	s.mu.Lock()
	if s.cachedField4 != "" && s.cachedField4Hash == hash {
		v := s.cachedField4
		s.mu.Unlock()
		return v, nil
	}
	s.mu.Unlock()

	parts := strings.Fields(cmdLine)
	if len(parts) == 0 {
		return "", errors.New("botguard: AISTUDIO_FIELD4_CMD vazio")
	}
	args := append(parts[1:], hash)
	execCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(execCtx, parts[0], args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("botguard: signer externo falhou: %v: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("botguard: signer externo falhou: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("botguard: signer externo retornou token vazio")
	}
	if os.Getenv("AISTUDIO_DEBUG_FIELD4") == "1" {
		preview := token
		if len(preview) > 24 {
			preview = preview[:24]
		}
		log.Printf("[FIELD4-EXT] hash=%s token_len=%d preview=%q", hash[:8], len(token), preview)
	}
	s.mu.Lock()
	s.cachedField4 = token
	s.cachedField4Hash = hash
	s.mu.Unlock()
	return token, nil
}

func authFingerprint(a *auth.RuntimeAuth) string {
	if a == nil {
		return ""
	}
	return a.AccountFingerprint
}

// decodeMaybeString unmarshals raw into target. If raw is itself a JSON string,
// it is first decoded and then re-decoded into target (handles double-encoded
// results returned by JSON.stringify in the page).
func decodeMaybeString(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		return json.Unmarshal([]byte(s), target)
	}
	return json.Unmarshal(raw, target)
}

func (s *Service) callGyb(ctx context.Context, contentHash string) (string, error) {
	var raw json.RawMessage
	expr := fmt.Sprintf(`(async () => { return await new Promise((resolve, reject) => {
  if (typeof window.__codexCapturedGyb !== 'function') { reject(new Error('gyb indisponivel')); return; }
  let timer = setTimeout(() => { timer = null; reject(new Error('timeout gyb')); }, 10000);
  try {
    window.__codexCapturedGyb((value) => { if (timer) { clearTimeout(timer); timer = null; } resolve(value); }, [{ content: %q }, undefined, undefined, undefined]);
  } catch (e) { if (timer) { clearTimeout(timer); timer = null; } reject(e); }
}); })()`, contentHash)

	if err := s.cdp.RunExclusive(ctx, "botguard-gyb", func(pageCtx context.Context) error {
		return s.cdp.Evaluate(pageCtx, expr, &raw)
	}); err != nil {
		return "", fmt.Errorf("botguard: gyb falhou: %w", err)
	}
	value, err := decodeGybValue(raw)
	if err != nil {
		return "", fmt.Errorf("botguard: retorno gyb inesperado: %w", err)
	}
	if strings.ToLower(strings.TrimSpace(os.Getenv("AISTUDIO_DEBUG_BG_TOKEN"))) == "1" {
		s.writeDebugFile("debug-browser-token.txt", []byte(value), 0o600)
		log.Printf("[BGTOKEN] hash=%s tokenLen=%d", contentHash, len(value))
	}
	return value, nil
}

func (s *Service) debugPath(name string) string {
	if s == nil || s.cfg == nil {
		return ""
	}
	dir := filepath.Join(s.cfg.StateDir, "debug")
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, filepath.Base(name))
}

func (s *Service) writeDebugFile(name string, data []byte, mode os.FileMode) {
	if path := s.debugPath(name); path != "" {
		_ = os.WriteFile(path, data, mode)
	}
}

func shouldRebootstrapAfterGybError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"gyb indisponivel",
		"timeout gyb",
		"window.__codexcapturedgyb",
		"__codexcapturedgyb nao disponivel",
		"target closed",
		"session closed",
		"browser has disconnected",
		"websocket url timeout",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func decodeGybValue(raw json.RawMessage) (string, error) {
	var direct string
	if err := json.Unmarshal(raw, &direct); err == nil && strings.TrimSpace(direct) != "" {
		return direct, nil
	}

	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return "", err
	}
	if value := extractBestString(node); strings.TrimSpace(value) != "" {
		return value, nil
	}
	return "", fmt.Errorf("%s", string(raw))
}

func extractBestString(node any) string {
	switch v := node.(type) {
	case string:
		return v
	case []any:
		best := ""
		for _, item := range v {
			if candidate := extractBestString(item); len(candidate) > len(best) {
				best = candidate
			}
		}
		return best
	case map[string]any:
		for _, key := range []string{"value", "token", "field4", "result", "data"} {
			if candidate := extractBestString(v[key]); strings.TrimSpace(candidate) != "" {
				return candidate
			}
		}
		best := ""
		for _, item := range v {
			if candidate := extractBestString(item); len(candidate) > len(best) {
				best = candidate
			}
		}
		return best
	default:
		return ""
	}
}

type rawCapture struct {
	URL      string
	Headers  map[string]string
	Template []byte
}

func toHeaderMap(headers network.Headers) map[string]string {
	out := map[string]string{}
	for k, v := range headers {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// base64Decode is a thin wrapper for the standard library, kept here so the
// botguard package doesn't grow a blanket encoding/base64 import for a single
// call site.
func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

func contains(s, substr string) bool { return len(substr) > 0 && indexOf(s, substr) >= 0 }

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func replaceFirst(s, old, new string) string {
	idx := indexOf(s, old)
	if idx < 0 {
		return s
	}
	return s[:idx] + new + s[idx+len(old):]
}

func extractPromptIDFromURL(rawURL string) string {
	pathStart := indexOf(rawURL, "/prompts/")
	if pathStart < 0 {
		return ""
	}
	rest := rawURL[pathStart+len("/prompts/"):]
	end := len(rest)
	for i, ch := range rest {
		if ch == '/' || ch == '?' || ch == '#' {
			end = i
			break
		}
	}
	id := rest[:end]
	if id == "new_chat" {
		return ""
	}
	return id
}

// buildBootstrapTemplatePayload returns the AI Studio nested-array payload used
// as a template for GenerateContent requests.
func buildBootstrapTemplatePayload(model, timeZone string, cfg *config.Config) json.RawMessage {
	if timeZone == "" {
		timeZone = "America/Sao_Paulo"
	}
	payload := []any{
		model,
		[][]any{{[]any{[]any{nil, bootstrapPrompt}}, "user"}},
		[][]any{{nil, nil, 7, 5}, {nil, nil, 8, 5}, {nil, nil, 9, 5}, {nil, nil, 10, 5}},
		[]any{nil, nil, nil, cfg.MaxTokensDefault, cfg.TopPDefault, cfg.TemperatureDefault, cfg.TopKDefault,
			nil, nil, nil, nil, nil, nil, nil, nil, nil, []any{1, nil, nil, 3}},
		nil, nil,
		[]any{[]any{nil, nil, nil, []any{nil, []any{}}}},
		nil, nil, nil, 1, nil, nil,
		[][]any{{nil, nil, timeZone}},
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}
