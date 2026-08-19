package accio

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed security-guard/sg_daemon.js
var sgDaemonScript []byte

// sgDaemon manages a persistent Node.js sidecar process that generates
// Security Guard headers. Communication is via JSON lines on stdin/stdout.
type sgDaemon struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	ready   bool
	reqID   int64
	pending map[string]chan sgResponse
	done    chan struct{}
}

type sgRequest struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

type sgResponse struct {
	ID      string            `json:"id"`
	Ready   bool              `json:"ready,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Error   string            `json:"error,omitempty"`
}

var globalSGDaemon *sgDaemon

// sgDaemonMu serializes daemon start/restart so a failed or dead daemon is
// retried on the next call instead of failing forever (the old sync.Once
// consumed itself on a first-boot failure — node missing, bundle missing —
// and every later request returned "security guard daemon unavailable" until
// the process restarted).
var sgDaemonMu sync.Mutex

// securityGuardBundleDir returns the path to the bundled security-guard
// directory (contains sg_daemon.js + prebuild/ + resources/).
func securityGuardBundleDir() string {
	// 1. Persistent per-user bundle: extracted/copied to the data dir on
	//    first use so the app does not depend on the working directory
	//    (double-clicked exe runs from build/bin) or on Program Files write
	//    access. Reused until an explicit env override.
	if dir := persistentBundleDir(); dir != "" {
		return dir
	}
	// 2. Explicit env override
	if v := strings.TrimSpace(os.Getenv("ACCIO_SECURITY_GUARD_DIR")); v != "" {
		return v
	}
	// 3. Relative to the executable
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "security-guard")
		if _, err := os.Stat(filepath.Join(candidate, "sg_daemon.js")); err == nil {
			return candidate
		}
	}
	// 4. Relative to working directory
	if _, err := os.Stat(filepath.Join("security-guard", "sg_daemon.js")); err == nil {
		abs, _ := filepath.Abs("security-guard")
		return abs
	}
	// 5. Fallback: installed Accio app (legacy)
	local := os.Getenv("LOCALAPPDATA")
	candidate := filepath.Join(local, "Programs", "Accio", "resources", "app.asar.unpacked", "node_modules", "@phoenix-common", "security-guard")
	if _, err := os.Stat(filepath.Join(candidate, "prebuild", "win32-x64", "security_guard.node")); err == nil {
		return candidate
	}
	return ""
}

// persistentBundleDir materialises the security-guard bundle into a stable
// per-user directory (%LOCALAPPDATA%\GrokDesktop\security-guard) copying from
// any source that already has sg_daemon.js + the native addon. Returns "" when
// no source is available.
func persistentBundleDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	base := filepath.Join(home, "AppData", "Local", "GrokDesktop")
	target := filepath.Join(base, "security-guard")
	if complete := bundleComplete(target); complete {
		return target
	}
	src := bundleSourceDir()
	if src == "" {
		return ""
	}
	if err := copyBundle(src, target); err != nil {
		log.Printf("accio: security-guard bundle copy failed: %v", err)
		return ""
	}
	if complete := bundleComplete(target); complete {
		return target
	}
	return ""
}

// bundleSourceDir finds a complete bundle to copy from: cwd, exe dir, the
// embedded project copy (internal/accio/security-guard) or the installed
// Accio app.
func bundleSourceDir() string {
	candidates := []string{
		filepath.Join("internal", "accio", "security-guard"),
		"security-guard",
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "security-guard"))
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		candidates = append(candidates,
			filepath.Join(local, "Programs", "Accio", "resources", "app.asar.unpacked", "node_modules", "@phoenix-common", "security-guard"))
	}
	for _, candidate := range candidates {
		if bundleComplete(candidate) {
			return candidate
		}
	}
	return ""
}

// bundleComplete reports whether dir has sg_daemon.js, the native addon and
// the resources DLLs required to run the Security Guard.
func bundleComplete(dir string) bool {
	if dir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "sg_daemon.js")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "prebuild", "win32-x64", "security_guard.node")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "resources", "x64", "AliSafeProxy.dll")); err != nil {
		return false
	}
	return true
}

// copyBundle copies sg_daemon.js, prebuild/ and resources/ from src to dst.
func copyBundle(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, rel := range []string{"sg_daemon.js", "prebuild", "resources"} {
		s := filepath.Join(src, rel)
		if _, err := os.Stat(s); err != nil {
			continue
		}
		d := filepath.Join(dst, rel)
		if err := copyTree(s, d); err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyTree(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func securityNodePath() string {
	if v := strings.TrimSpace(os.Getenv("ACCIO_NODE_PATH")); v != "" {
		return v
	}
	if v, err := exec.LookPath("node"); err == nil {
		return v
	}
	// Fallback: Accio's bundled Node
	root := filepath.Join(os.Getenv("APPDATA"), "Accio", "pre-install")
	var found string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && strings.EqualFold(info.Name(), "node.exe") {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// getSGDaemon returns the global singleton daemon, starting it if needed.
// If the daemon process died (crash, node exit), a fresh one is started on
// the next call instead of failing forever. ctx bounds the startup wait so a
// hung node sidecar cannot stall a request for longer than the caller's
// deadline (the old 15s fixed wait made a broken UMID init cost 45s across
// the chat retry loop).
func getSGDaemon(ctx context.Context) (*sgDaemon, error) {
	sgDaemonMu.Lock()
	defer sgDaemonMu.Unlock()
	if globalSGDaemon != nil && !sgDaemonDead(globalSGDaemon) {
		return globalSGDaemon, nil
	}
	if globalSGDaemon != nil {
		log.Printf("accio: sg daemon died; restarting")
	}
	d := &sgDaemon{
		pending: make(map[string]chan sgResponse),
		done:    make(chan struct{}),
	}
	if err := d.start(ctx); err != nil {
		// Do not cache the failure: the next call tries to start again.
		return nil, err
	}
	globalSGDaemon = d
	return globalSGDaemon, nil
}

func sgDaemonDead(d *sgDaemon) bool {
	if d == nil {
		return true
	}
	select {
	case <-d.done:
		return true
	default:
		return false
	}
}

func (d *sgDaemon) start(ctx context.Context) error {
	nodePath := securityNodePath()
	if nodePath == "" {
		return errors.New("node.js not found; set ACCIO_NODE_PATH or install Node")
	}

	bundleDir := securityGuardBundleDir()
	if bundleDir == "" {
		return errors.New("security-guard bundle not found; set ACCIO_SECURITY_GUARD_DIR")
	}

	daemonScript := filepath.Join(bundleDir, "sg_daemon.js")

	// If the daemon script doesn't exist at the bundle path, write the embedded one
	if _, err := os.Stat(daemonScript); err != nil {
		if writeErr := os.WriteFile(daemonScript, sgDaemonScript, 0o700); writeErr != nil {
			return fmt.Errorf("write sg_daemon.js: %w", writeErr)
		}
	}

	cmd := exec.Command(nodePath, daemonScript)
	cmd.Dir = bundleDir
	cmd.Env = append(os.Environ(), "ACCIO_SG_APPKEY="+DefaultSecurityAppKey)
	cmd.Stderr = os.Stderr // daemon logs go to stderr
	// Never pop a console window for the sidecar (the official app's daemon
	// also runs headless). Also detach so a parent crash doesn't leave the
	// terminal behind; the daemon exits when stdin closes (Shutdown).
	hideSecuritySidecarWindow(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sg daemon: %w", err)
	}

	d.cmd = cmd
	d.stdin = stdin
	d.scanner = bufio.NewScanner(stdout)
	d.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	// Reap the child process and signal done when it exits.
	go func() {
		_ = cmd.Wait()
		select {
		case <-d.done:
		default:
			close(d.done)
		}
	}()

	// Read loop
	go d.readLoop()

	// Wait for ready signal, bounded by both the caller's deadline and a
	// hard 15s cap (the daemon normally reports ready in <1s).
	readyCh := make(chan sgResponse, 1)
	d.mu.Lock()
	d.pending["__ready__"] = readyCh
	d.mu.Unlock()

	readyTimer := time.NewTimer(15 * time.Second)
	defer readyTimer.Stop()
	select {
	case resp := <-readyCh:
		if resp.Error != "" {
			return fmt.Errorf("sg daemon init failed: %s", resp.Error)
		}
		d.ready = true
		return nil
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return fmt.Errorf("sg daemon start interrupted: %w", ctx.Err())
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		return errors.New("sg daemon ready timeout (15s)")
	case <-d.done:
		return errors.New("sg daemon exited before ready")
	}
}

func (d *sgDaemon) readLoop() {
	defer close(d.done)
	for d.scanner.Scan() {
		line := d.scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp sgResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}

		d.mu.Lock()
		// Handle ready signal
		if resp.Ready {
			if ch, ok := d.pending["__ready__"]; ok {
				ch <- resp
				delete(d.pending, "__ready__")
			}
			d.mu.Unlock()
			continue
		}
		// Route response to waiting caller
		if ch, ok := d.pending[resp.ID]; ok {
			ch <- resp
			delete(d.pending, resp.ID)
		}
		d.mu.Unlock()
	}
	// Scanner ended → process died. Notify all pending.
	d.mu.Lock()
	for id, ch := range d.pending {
		ch <- sgResponse{ID: id, Error: "sg daemon process exited"}
		delete(d.pending, id)
	}
	d.mu.Unlock()
}

// headers sends a URL to the daemon and waits for the security headers.
func (d *sgDaemon) headers(ctx context.Context, requestURL string) (map[string]string, error) {
	if !d.ready {
		return nil, errors.New("sg daemon not ready")
	}

	d.mu.Lock()
	d.reqID++
	id := fmt.Sprintf("sg-%d-%d", time.Now().UnixMilli(), d.reqID)
	ch := make(chan sgResponse, 1)
	d.pending[id] = ch
	d.mu.Unlock()

	req := sgRequest{ID: id, URL: requestURL}
	data, _ := json.Marshal(req)
	data = append(data, '\n')

	if _, err := d.stdin.Write(data); err != nil {
		d.mu.Lock()
		delete(d.pending, id)
		d.mu.Unlock()
		return nil, fmt.Errorf("write to sg daemon: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != "" {
			return nil, errors.New("Security Guard: " + resp.Error)
		}
		if len(resp.Headers) == 0 {
			return nil, errors.New("Security Guard returned no headers")
		}
		return resp.Headers, nil
	case <-ctx.Done():
		d.mu.Lock()
		delete(d.pending, id)
		d.mu.Unlock()
		return nil, fmt.Errorf("Security Guard timeout: %w", ctx.Err())
	case <-d.done:
		return nil, errors.New("Security Guard daemon crashed")
	}
}

// Shutdown gracefully stops the daemon process. Call this on proxy shutdown.
func (d *sgDaemon) Shutdown() {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Close stdin → daemon detects EOF → exits cleanly
	if d.stdin != nil {
		_ = d.stdin.Close()
	}

	// Give it 2s to exit gracefully, then force kill
	if d.cmd != nil && d.cmd.Process != nil {
		select {
		case <-d.done:
			// Already exited
		case <-time.After(2 * time.Second):
			_ = d.cmd.Process.Kill()
		}
	}
}

// ShutdownSGDaemon is the package-level shutdown hook.
func ShutdownSGDaemon() {
	sgDaemonMu.Lock()
	defer sgDaemonMu.Unlock()
	if globalSGDaemon != nil {
		globalSGDaemon.Shutdown()
		globalSGDaemon = nil
	}
}

// securityHeaders is the main entry point called by chatWithToken.
// It replaces the old per-request process spawn with the persistent daemon.
func (c *Client) securityHeaders(ctx context.Context, requestURL string) (map[string]string, error) {
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	daemon, err := getSGDaemon(callCtx)
	if err != nil {
		return nil, err
	}
	headers, err := daemon.headers(callCtx, requestURL)
	if err != nil {
		return nil, err
	}
	return applyLocalSign(headers), nil
}

// SecurityHeadersForURL exposes the WAF security headers (x-sign, x-mini-wua,
// sgext, …) for an arbitrary gateway URL. The PKCE token exchange endpoint
// started rejecting unsigned requests (90001 invalid_code), so the login
// path needs the same signing the chat path already had.
func (c *Client) SecurityHeadersForURL(ctx context.Context, requestURL string) (map[string]string, error) {
	return c.securityHeaders(ctx, requestURL)
}
