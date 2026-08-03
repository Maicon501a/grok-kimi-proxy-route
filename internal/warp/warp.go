package warp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config controls isolated WARP proxy mode. WARP is enabled by default and
// can be disabled with WARP_AUTO_FAILOVER=false.
type Config struct {
	Enabled        bool
	SocksHost      string
	SocksPort      int
	Cooldown       time.Duration
	Binary         string
	CommandTimeout time.Duration
	StartupWait    time.Duration
}

func ConfigFromEnv() Config {
	enabled := true
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("WARP_AUTO_FAILOVER"))); v == "false" || v == "0" || v == "off" {
		enabled = false
	}
	port := 40000
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("WARP_SOCKS_PORT"))); err == nil && v > 0 && v <= 65535 {
		port = v
	}
	commandTimeout := 30 * time.Second
	if v, err := time.ParseDuration(strings.TrimSpace(os.Getenv("WARP_COMMAND_TIMEOUT"))); err == nil && v > 0 {
		commandTimeout = v
	}
	startupWait := 1500 * time.Millisecond
	if v, err := time.ParseDuration(strings.TrimSpace(os.Getenv("WARP_STARTUP_WAIT"))); err == nil && v >= 0 {
		startupWait = v
	}
	binary := strings.TrimSpace(os.Getenv("WARP_BIN_PATH"))
	if binary == "" {
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			candidate := filepath.Join(pf, "Cloudflare", "Cloudflare WARP", "warp-cli.exe")
			if _, err := os.Stat(candidate); err == nil {
				binary = candidate
			}
		}
	}
	if binary == "" {
		binary = "warp-cli"
	}
	cooldown := time.Minute
	if v, err := time.ParseDuration(strings.TrimSpace(os.Getenv("WARP_COOLDOWN"))); err == nil && v >= 0 {
		cooldown = v
	} else if v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("WARP_COOLDOWN_MS")), 10, 64); err == nil && v >= 0 {
		cooldown = time.Duration(v) * time.Millisecond
	}
	return Config{
		Enabled:        enabled,
		SocksHost:      envOr("WARP_SOCKS_HOST", "127.0.0.1"),
		SocksPort:      port,
		Cooldown:       cooldown,
		Binary:         binary,
		CommandTimeout: commandTimeout,
		StartupWait:    startupWait,
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

type Manager struct {
	mu          sync.Mutex
	cfg         Config
	active      bool
	activating  bool
	wait        chan struct{}
	lastAttempt time.Time
	lastError   string
	activatedAt time.Time
}

var defaultManager = New(ConfigFromEnv())

func New(cfg Config) *Manager { return &Manager{cfg: cfg} }

func Default() *Manager { return defaultManager }

func (m *Manager) Enabled() bool {
	return m != nil && m.cfg.Enabled
}

func (m *Manager) IsActive() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

// MarkInactive invalidates the cached state after a WARP-routed connection
// fails. The next qualifying Zen error can then re-run warp-cli instead of
// believing a stale Connected state forever.
func (m *Manager) MarkInactive(reason string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.active = false
	if strings.TrimSpace(reason) != "" {
		m.lastError = strings.TrimSpace(reason)
	}
	m.mu.Unlock()
}

// Activate enables WARP proxy mode once and coalesces concurrent triggers.
// A failed activation is cooldown-limited so a burst of 500s cannot spawn a
// storm of warp-cli processes.
func (m *Manager) Activate(ctx context.Context) error {
	if m == nil || !m.cfg.Enabled {
		return errors.New("WARP failover disabled")
	}
	m.mu.Lock()
	if m.active {
		m.mu.Unlock()
		return nil
	}
	if m.activating {
		wait := m.wait
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
		}
		m.mu.Lock()
		active, errText := m.active, m.lastError
		m.mu.Unlock()
		if active {
			return nil
		}
		if errText == "" {
			errText = "WARP activation failed"
		}
		return errors.New(errText)
	}
	if !m.lastAttempt.IsZero() && time.Since(m.lastAttempt) < m.cfg.Cooldown {
		errText := m.lastError
		m.mu.Unlock()
		if errText == "" {
			errText = "WARP activation is cooling down"
		}
		return errors.New(errText)
	}
	m.activating = true
	m.wait = make(chan struct{})
	m.lastAttempt = time.Now()
	wait := m.wait
	m.mu.Unlock()

	err := m.ensureReady(ctx)
	m.mu.Lock()
	m.activating = false
	if err == nil {
		m.active = true
		m.lastError = ""
		m.activatedAt = time.Now()
	} else {
		m.active = false
		m.lastError = err.Error()
	}
	close(wait)
	m.mu.Unlock()
	return err
}

func (m *Manager) ensureReady(ctx context.Context) error {
	if _, err := m.runCLI(ctx, "proxy", "port", strconv.Itoa(m.cfg.SocksPort)); err != nil {
		return err
	}
	if _, err := m.runCLI(ctx, "mode", "proxy"); err != nil {
		return err
	}
	if _, err := m.runCLI(ctx, "connect"); err != nil {
		return err
	}
	if m.cfg.StartupWait > 0 {
		timer := time.NewTimer(m.cfg.StartupWait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	status, err := m.runCLI(ctx, "status")
	if err != nil {
		return err
	}
	statusLower := strings.ToLower(status)
	if strings.Contains(statusLower, "disconnected") || !strings.Contains(statusLower, "connected") {
		return fmt.Errorf("WARP did not reach Connected state: %s", strings.TrimSpace(status))
	}
	return nil
}

func (m *Manager) runCLI(parent context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, m.cfg.CommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.cfg.Binary, args...)
	hideCommandWindow(cmd)
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("warp-cli %s: %w", strings.Join(args, " "), ctx.Err())
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			msg = strings.TrimSpace(string(exitErr.Stderr))
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("warp-cli %s failed: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// HTTPClient clones the caller's client settings and routes all its TCP
// connections through this manager's local SOCKS5 endpoint. It is intended
// only for a Zen retry; the normal client remains on the default route.
func (m *Manager) HTTPClient(base *http.Client) *http.Client {
	if m == nil {
		return base
	}
	if base == nil {
		base = &http.Client{}
	}
	var transport *http.Transport
	if baseTransport, ok := base.Transport.(*http.Transport); ok && baseTransport != nil {
		transport = baseTransport.Clone()
	} else if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaultTransport.Clone()
	} else {
		transport = (&http.Transport{}).Clone()
	}
	transport.Proxy = nil
	transport.DialTLS = nil
	transport.DialTLSContext = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialSOCKS5(ctx, socksProxyAddress(m.cfg.SocksHost, m.cfg.SocksPort), address)
	}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: base.CheckRedirect,
		Jar:           base.Jar,
		Timeout:       base.Timeout,
	}
}

func (m *Manager) Status() map[string]any {
	if m == nil {
		return map[string]any{"enabled": false}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	status := map[string]any{
		"enabled":         m.cfg.Enabled,
		"active":          m.active,
		"activating":      m.activating,
		"socks":           socksProxyAddress(m.cfg.SocksHost, m.cfg.SocksPort),
		"binary":          m.cfg.Binary,
		"cooldown_ms":     m.cfg.Cooldown.Milliseconds(),
		"last_error":      m.lastError,
		"activated_at":    nil,
		"last_attempt_at": nil,
	}
	if !m.activatedAt.IsZero() {
		status["activated_at"] = m.activatedAt.UTC().Format(time.RFC3339)
	}
	if !m.lastAttempt.IsZero() {
		status["last_attempt_at"] = m.lastAttempt.UTC().Format(time.RFC3339)
	}
	return status
}

// ShouldFailover recognizes Zen failures that plausibly depend on the current
// egress. A 500 is retried only for the gateway's internal-error envelope;
// validation/auth/model errors are not blindly sent through WARP.
func ShouldFailover(status int, body []byte) bool {
	if status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout {
		return true
	}
	if status != http.StatusInternalServerError {
		return false
	}
	low := strings.ToLower(string(body))
	return strings.Contains(low, "internal server error") ||
		strings.Contains(low, "internal_server_error") ||
		strings.Contains(low, "upstream error") ||
		strings.Contains(low, "upstream_error")
}

func IsNetworkFailure(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "connection reset") ||
		strings.Contains(text, "connection refused") ||
		strings.Contains(text, "no such host") ||
		strings.Contains(text, "i/o timeout")
}
