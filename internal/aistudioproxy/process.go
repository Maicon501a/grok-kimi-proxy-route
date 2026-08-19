package aistudioproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"grok-desktop/internal/aistudio/admin"
	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/httpserver"
	aistudioruntime "grok-desktop/internal/aistudio/runtime"
)

type Manager struct {
	mu       sync.RWMutex
	dataRoot string
	baseURL  string
	token    string
	server   *http.Server
	runtime  *aistudioruntime.Manager
	cancel   context.CancelFunc
	done     chan error
	client   *http.Client
	starting bool
}

func New(dataRoot string) *Manager {
	return &Manager{
		dataRoot: filepath.Join(dataRoot, "aistudio-proxy"),
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (m *Manager) DataRoot() string { return m.dataRoot }

func (m *Manager) BaseURL() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.baseURL
}

func (m *Manager) Start(ctx context.Context, legacyProjectDir string, eagerBoot ...bool) error {
	m.mu.Lock()
	if m.server != nil {
		m.mu.Unlock()
		return nil
	}
	if m.starting {
		m.mu.Unlock()
		return fmt.Errorf("AI Studio runtime is already starting")
	}
	m.starting = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.starting = false
		m.mu.Unlock()
	}()

	if err := os.MkdirAll(m.dataRoot, 0o700); err != nil {
		return err
	}
	if strings.TrimSpace(legacyProjectDir) != "" {
		if migrated, err := MigrateLegacy(legacyProjectDir, m.dataRoot); err != nil {
			log.Printf("AI Studio profile migration skipped: %v", err)
		} else if migrated {
			log.Printf("AI Studio profiles migrated from %s", legacyProjectDir)
		}
	}
	cfg, err := config.LoadEmbedded(m.dataRoot)
	if err != nil {
		return err
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}

	runtimeManager := aistudioruntime.New(cfg)
	proxyHandler := httpserver.New(runtimeManager).Handler()
	adminHandler := admin.NewHandlerWithToken(runtimeManager, admin.NewMetricsStore(), token)
	rootHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/") {
			adminHandler.ServeHTTP(w, r)
			return
		}
		proxyHandler.ServeHTTP(w, r)
	})
	server := &http.Server{Handler: rootHandler, ReadHeaderTimeout: 30 * time.Second}
	serviceCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	baseURL := "http://" + listener.Addr().String()
	adminHandler.SetShutdown(func() { go shutdownHTTPServer(server) })
	m.mu.Lock()
	m.baseURL = baseURL
	m.token = token
	m.server = server
	m.runtime = runtimeManager
	m.cancel = cancel
	m.done = done
	m.mu.Unlock()
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		cancel()
		runtimeManager.Close()
		m.mu.Lock()
		if m.server == server {
			m.server = nil
			m.runtime = nil
			m.cancel = nil
			m.baseURL = ""
			m.token = ""
			m.done = nil
		}
		m.mu.Unlock()
		done <- err
		close(done)
	}()
	// Starting Chrome/Botguard for every app launch costs hundreds of MB even
	// while another provider is selected. Boot eagerly only when the caller
	// explicitly requests it; Gemini initializes lazily on first use otherwise.
	if len(eagerBoot) > 0 && eagerBoot[0] {
		go func() {
			runtimeManager.EagerBootBotguards(serviceCtx, cfg.EagerBoot)
			runtimeManager.WarmUpAll(serviceCtx)
		}()
	}

	readyCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	if err := m.waitReady(readyCtx, done); err != nil {
		_ = m.Stop(context.Background())
		return err
	}
	log.Printf("AI Studio runtime listening on %s", baseURL)
	return nil
}

func (m *Manager) waitReady(ctx context.Context, done <-chan error) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := m.Health(ctx); err == nil {
			return nil
		}
		select {
		case err := <-done:
			if err == nil {
				err = fmt.Errorf("runtime exited before health check")
			}
			return err
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("AI Studio runtime did not become ready: %w", ctx.Err())
		}
	}
}

func (m *Manager) Health(ctx context.Context) error {
	base := m.BaseURL()
	if base == "" {
		return fmt.Errorf("AI Studio runtime is not started")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health returned %s", resp.Status)
	}
	return nil
}

func (m *Manager) Stop(ctx context.Context) error {
	m.mu.RLock()
	server, done := m.server, m.done
	m.mu.RUnlock()
	if server == nil {
		return nil
	}
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := m.adminJSON(stopCtx, http.MethodPost, "/admin/api/shutdown", map[string]any{}); err != nil {
		go shutdownHTTPServer(server)
	}
	select {
	case <-done:
		return nil
	case <-stopCtx.Done():
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return nil
	}
}

// ShutdownManagedBrowsers releases RAM/CPU without stopping the local proxy
// server. Gemini recreates the selected profile lazily on the next request.
func (m *Manager) ShutdownManagedBrowsers(ctx context.Context) {
	m.mu.RLock()
	runtimeManager := m.runtime
	m.mu.RUnlock()
	if runtimeManager != nil {
		runtimeManager.ShutdownAllManagedBrowsers(ctx)
	}
}

func shutdownHTTPServer(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

func randomToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
