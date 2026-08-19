// Package cdp wraps chromedp to provide a dedicated, lockable AI Studio browser
// tab per profile, with helpers for cookie retrieval, prompt population, and
// the Botguard hook injection used to capture the gyb callback.
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	browserproto "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	cruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/profile"
)

const proxyPageURL = "about:blank"

// Client owns a chromedp browser context for a single profile.
type Client struct {
	profile *profile.Profile
	cfg     *config.Config

	mu              sync.Mutex
	ctx             context.Context
	cancel          context.CancelFunc
	connected       bool
	managedPID      int
	launchedByProxy bool

	exclusiveMu sync.Mutex
}

// New creates a Client for the given profile.
func New(p *profile.Profile, cfg *config.Config) *Client {
	return &Client{profile: p, cfg: cfg}
}

// ProfileID returns the bound profile id.
func (c *Client) ProfileID() string {
	if c.profile == nil {
		return "default"
	}
	return c.profile.ID
}

// Connect attaches to the running Chrome instance (via wsEndpoint) and ensures
// a dedicated browser tab is available. Higher-level flows navigate it as
// needed, so connect keeps this step lightweight and stable.
func (c *Client) Connect(parent context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected && c.ctx != nil {
		probeCtx, cancel := context.WithTimeout(c.ctx, 3*time.Second)
		defer cancel()

		var title string
		if err := chromedp.Run(probeCtx, chromedp.Title(&title)); err == nil {
			return nil
		}
		c.teardownLocked()
	}

	ws, launched, err := c.ensureBrowser(parent)
	if err != nil {
		return err
	}

	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), ws)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	c.ctx = browserCtx
	c.cancel = func() {
		browserCancel()
		allocCancel()
	}

	if err := c.runConnect(parent); err != nil {
		c.teardownLocked()
		return fmt.Errorf("cdp: connect failed: %w", err)
	}
	if err := c.applyManagedPageSpoofLocked(); err != nil {
		c.teardownLocked()
		return fmt.Errorf("cdp: managed spoof failed: %w", err)
	}
	c.connected = true
	c.launchedByProxy = launched
	return nil
}

// Disconnect releases the chromedp context and closes managed browsers launched
// by this client.
func (c *Client) Disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connected && c.ctx != nil && c.launchedByProxy {
		_ = browserproto.Close().Do(c.ctx)
	}
	c.teardownLocked()
	c.launchedByProxy = false
	c.managedPID = 0
}

func (c *Client) teardownLocked() {
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.ctx = nil
	c.connected = false
}

func (c *Client) runConnect(parent context.Context) error {
	errCh := make(chan error, 1)
	// Snapshot the context before starting the goroutine. A timeout can make
	// Connect tear the client down while chromedp.Run is still being scheduled;
	// reading c.ctx inside the goroutine would then race with teardownLocked and
	// occasionally pass a nil context to chromedp, causing a process-wide panic.
	ctx := c.ctx
	go func(runCtx context.Context) {
		errCh <- chromedp.Run(runCtx, chromedp.Navigate(proxyPageURL))
	}(ctx)

	timeout := time.NewTimer(c.connectTimeout())
	defer timeout.Stop()

	select {
	case err := <-errCh:
		return err
	case <-parent.Done():
		return parent.Err()
	case <-timeout.C:
		return context.DeadlineExceeded
	}
}

func (c *Client) wsEndpoint() string {
	candidates := make([]string, 0, 4)
	allowGlobalEndpoint := c.profile == nil || c.profile.AllowGlobalEndpoint

	if c.profile != nil {
		if ws := strings.TrimSpace(c.profile.WSEndpoint); ws != "" {
			candidates = append(candidates, ws)
		}
		if meta, err := readConnectionMeta(c.profile.ConnectionFile); err == nil {
			if candidate := meta.endpointCandidate(); candidate != "" {
				candidates = append(candidates, candidate)
			}
		}
	}

	if allowGlobalEndpoint {
		if meta, err := readConnectionMeta(c.cfg.AIStudio.ConnectionFile); err == nil {
			if candidate := meta.endpointCandidate(); candidate != "" {
				candidates = append(candidates, candidate)
			}
		}
		if ws := strings.TrimSpace(c.cfg.AIStudio.WSEndpoint); ws != "" {
			candidates = append(candidates, ws)
		}
	}

	for _, candidate := range candidates {
		if ws := resolveLiveWSEndpoint(candidate); ws != "" {
			return ws
		}
	}
	return ""
}

// ShutdownBrowserProcess closes a managed browser for the current profile and
// clears the persisted connection metadata.
func (c *Client) ShutdownBrowserProcess(parent context.Context) error {
	c.mu.Lock()
	if c.connected && c.ctx != nil {
		if err := browserproto.Close().Do(c.ctx); err != nil {
			_ = err
		}
	}
	pid := c.managedPID
	c.teardownLocked()
	c.launchedByProxy = false
	c.managedPID = 0
	c.mu.Unlock()

	if pid > 0 {
		_ = killManagedProcess(pid)
	}
	if userDataDir := c.userDataDir(); userDataDir != "" {
		_ = killManagedProfileProcesses(parent, userDataDir)
	}
	c.clearSavedConnection()
	return nil
}

// RunExclusive acquires the per-profile lock and runs fn with the chromedp
// context, ensuring the page is connected first.
func (c *Client) RunExclusive(parent context.Context, label string, fn func(ctx context.Context) error) error {
	c.exclusiveMu.Lock()
	defer c.exclusiveMu.Unlock()

	if err := c.Connect(parent); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(c.ctx)
	stop := context.AfterFunc(parent, cancel)
	defer func() {
		stop()
		cancel()
	}()
	return fn(runCtx)
}

// Context returns the chromedp context (must be connected).
func (c *Client) Context() (context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.connected || c.ctx == nil {
		return nil, errors.New("cdp: cliente nao conectado")
	}
	return c.ctx, nil
}

// GetCookies returns cookies for the AI Studio domains.
func (c *Client) GetCookies(ctx context.Context) ([]*network.Cookie, error) {
	urls := []string{
		"https://aistudio.google.com",
		"https://alkalimakersuite-pa.clients6.google.com",
	}
	var cookies []*network.Cookie
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		cookies, err = network.GetCookies().WithURLs(urls).Do(ctx)
		return err
	}))
	return cookies, err
}

// InstallBotguardHooks injects the script that captures gyb before navigation.
func (c *Client) InstallBotguardHooks(ctx context.Context) error {
	script := botguardHookScript()
	return chromedp.Run(
		ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(script).Do(ctx)
			return err
		}),
		chromedp.Evaluate(script, nil),
	)
}

// ApplyUserAgentOverride forces a specific User-Agent string on the browser
// tab via CDP emulation.setUserAgentOverride. This is called explicitly from
// the Botguard bootstrap so that the captured CountTokens headers carry a
// believable UA (the default Chromium headless UA "HeadlessChrome/..." is
// rejected by Google with 403). Unlike applyManagedPageSpoofLocked() this
// method is unconditional: the caller already decided spoofing is needed.
func (c *Client) ApplyUserAgentOverride(ctx context.Context, userAgent string) error {
	if strings.TrimSpace(userAgent) == "" {
		return nil
	}
	return chromedp.Run(ctx, emulation.SetUserAgentOverride(userAgent))
}

// Evaluate runs an arbitrary JS expression and decodes the JSON result into out.
func (c *Client) Evaluate(ctx context.Context, expr string, out any) error {
	var raw json.RawMessage
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &raw, awaitPromise)); err != nil {
		return err
	}
	if out != nil && len(raw) > 0 && string(raw) != "null" {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// EvaluateBool runs an expression expected to return a boolean.
func (c *Client) EvaluateBool(ctx context.Context, expr string) (bool, error) {
	var result bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &result)); err != nil {
		return false, err
	}
	return result, nil
}

// FillPrompt populates the AI Studio prompt textarea with text.
func (c *Client) FillPrompt(ctx context.Context, text string) error {
	expr := fmt.Sprintf(fillPromptExpr, jsonString(text))
	return chromedp.Run(ctx, chromedp.Evaluate(expr, nil))
}

// DismissInterferingUI clicks skip/close/cancel/dismiss/not-now buttons up to
// three times.
func (c *Client) DismissInterferingUI(ctx context.Context) error {
	for attempt := 0; attempt < 3; attempt++ {
		clicked, err := c.EvaluateBool(ctx, dismissUIExpr)
		if err != nil {
			return err
		}
		if !clicked {
			return nil
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func botguardHookScript() string {
	// O probe recorder (debug) wrapa getters de Navigator/Screen/etc. e o
	// integrity check do Botguard detecta — nunca em producao.
	if strings.ToLower(strings.TrimSpace(os.Getenv("AISTUDIO_DEBUG_BG_PROBE"))) == "1" {
		return botguardHookScriptSource + "\n" + botguardProbeRecorderSource
	}
	return botguardHookScriptSource
}

func awaitPromise(p *cruntime.EvaluateParams) *cruntime.EvaluateParams {
	return p.WithAwaitPromise(true)
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

func (c *Client) connectTimeout() time.Duration {
	timeout := time.Duration(c.cfg.BrowserConnectTimeout) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	pageTimeout := time.Duration(c.cfg.PageLoadTimeout) * time.Millisecond
	if pageTimeout > timeout {
		timeout = pageTimeout
	}
	return timeout
}

func (c *Client) applyManagedPageSpoofLocked() error {
	if !c.shouldApplyManagedSpoof() || c.ctx == nil {
		return nil
	}
	return chromedp.Run(c.ctx,
		emulation.SetUserAgentOverride(c.cfg.AIStudio.VisibleUserAgent),
		emulation.SetDeviceMetricsOverride(
			int64(c.cfg.AIStudio.VisibleViewportWidth),
			int64(c.cfg.AIStudio.VisibleViewportHeight),
			c.cfg.AIStudio.VisibleDeviceScale,
			false,
		),
		emulation.SetTouchEmulationEnabled(false),
	)
}

func (c *Client) shouldApplyManagedSpoof() bool {
	if c.cfg == nil || c.cfg.AIStudio.BrowserMode != config.BrowserHeadlessSpoof {
		return false
	}
	if c.launchedByProxy {
		return true
	}
	connectionFile := c.connectionFile()
	if strings.TrimSpace(connectionFile) == "" {
		return false
	}
	raw, err := os.ReadFile(connectionFile)
	if err != nil {
		return false
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return false
	}
	flag, _ := meta[managedProfileMarker].(bool)
	return flag
}

type connectionMeta struct {
	WSEndpoint string `json:"wsEndpoint"`
	HTTPURL    string `json:"httpUrl"`
	DebugPort  int    `json:"debugPort"`
	Host       string `json:"host"`
}

func (m connectionMeta) endpointCandidate() string {
	if ws := strings.TrimSpace(m.WSEndpoint); ws != "" {
		return ws
	}
	if httpURL := strings.TrimSpace(m.HTTPURL); httpURL != "" {
		return httpURL
	}
	if host := strings.TrimSpace(m.Host); host != "" && m.DebugPort > 0 {
		return fmt.Sprintf("http://%s:%d", host, m.DebugPort)
	}
	if m.DebugPort > 0 {
		return fmt.Sprintf("http://127.0.0.1:%d", m.DebugPort)
	}
	return ""
}

func readConnectionMeta(path string) (*connectionMeta, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("empty connection file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta connectionMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func resolveLiveWSEndpoint(candidate string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return ""
	}
	versionURL, err := browserVersionURL(candidate)
	if err != nil {
		return candidate
	}

	reqCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, versionURL, nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	var payload struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return ""
	}
	if ws := strings.TrimSpace(payload.WebSocketDebuggerURL); ws != "" {
		return ws
	}
	return ""
}

func browserVersionURL(candidate string) (string, error) {
	parsed, err := url.Parse(candidate)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported endpoint scheme: %s", parsed.Scheme)
	}
	parsed.Path = "/json/version"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// ErrNoWSEndpoint is returned when no Chrome debugging endpoint is configured.
var ErrNoWSEndpoint = errors.New("cdp: nenhum wsEndpoint configurado para o perfil")
