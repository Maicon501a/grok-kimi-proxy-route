package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/storage"
)

const managedProfileMarker = "managedByProxy"

var managedBrowserArgs = []string{
	"--disable-blink-features=AutomationControlled",
	"--disable-features=ChromeWhatsNewUI",
	"--disable-infobars",
	"--exclude-switches=enable-automation",
	"--no-sandbox",
	"--disable-setuid-sandbox",
	"--disable-dev-shm-usage",
	"--disable-gpu",
	"--disable-background-networking",
	"--disable-sync",
	"--no-first-run",
	"--no-default-browser-check",
	"--disable-ipc-flooding-protection",
	"--disable-renderer-backgrounding",
	"--disable-background-timer-throttling",
	"--disable-backgrounding-occluded-windows",
	"--metrics-recording-only",
	"--mute-audio",
	"--password-store=basic",
}

func (c *Client) ensureBrowser(parent context.Context) (string, bool, error) {
	if ws := c.wsEndpoint(); ws != "" {
		return ws, false, nil
	}

	c.clearSavedConnection()
	if c.userDataDir() == "" {
		return "", false, ErrNoWSEndpoint
	}
	ws, pid, err := c.launchManagedBrowser(parent)
	if err != nil {
		return "", false, err
	}
	c.managedPID = pid
	return ws, true, nil
}

func (c *Client) launchManagedBrowser(parent context.Context) (string, int, error) {
	userDataDir := c.userDataDir()
	if strings.TrimSpace(userDataDir) == "" {
		return "", 0, errors.New("cdp: perfil sem userDataDir para autoabrir navegador")
	}
	if err := os.MkdirAll(userDataDir, 0o755); err != nil {
		return "", 0, err
	}

	browserPath, err := findManagedBrowserExecutable()
	if err != nil {
		return "", 0, err
	}
	debugPort := chooseManagedDebugPort(c.ProfileID())
	args := []string{
		fmt.Sprintf("--user-data-dir=%s", userDataDir),
		fmt.Sprintf("--remote-debugging-port=%d", debugPort),
		"--remote-debugging-address=127.0.0.1",
	}
	args = append(args, managedBrowserArgs...)
	if c.cfg.AIStudio.BrowserMode != config.BrowserVisibleLegacy {
		args = append(args, "--headless=new")
	}
	if runtime.GOOS == "windows" {
		args = append(args, "--disable-features=CalculateNativeWinOcclusion")
	}

	cmd := exec.Command(browserPath, args...)
	if runtime.GOOS == "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			HideWindow: c.cfg.AIStudio.BrowserMode != config.BrowserVisibleLegacy,
		}
	}

	if err := cmd.Start(); err != nil {
		return "", 0, err
	}

	wsEndpoint, err := waitForBrowserWSEndpoint(debugPort, c.connectTimeout())
	if err != nil {
		_ = killManagedProcess(cmd.Process.Pid)
		return "", 0, err
	}
	c.persistConnectionInfo(wsEndpoint)
	return wsEndpoint, cmd.Process.Pid, nil
}

func (c *Client) connectionFile() string {
	if c.profile != nil && strings.TrimSpace(c.profile.ConnectionFile) != "" {
		return c.profile.ConnectionFile
	}
	if c.profile == nil || c.profile.AllowGlobalEndpoint {
		return c.cfg.AIStudio.ConnectionFile
	}
	return ""
}

func (c *Client) userDataDir() string {
	if c.profile != nil && strings.TrimSpace(c.profile.UserDataDir) != "" {
		return c.profile.UserDataDir
	}
	for _, fallback := range c.cfg.Profiles.FallbackProfiles {
		if fallback.ID == c.ProfileID() && strings.TrimSpace(fallback.UserDataDir) != "" {
			return fallback.UserDataDir
		}
	}
	return ""
}

func (c *Client) persistConnectionInfo(wsEndpoint string) {
	connectionFile := c.connectionFile()
	if connectionFile == "" || wsEndpoint == "" {
		return
	}
	debugPort := extractDebugPort(wsEndpoint)
	payload := map[string]any{
		"wsEndpoint":         wsEndpoint,
		"debugPort":          debugPort,
		"host":               "127.0.0.1",
		"httpUrl":            fmt.Sprintf("http://127.0.0.1:%d", debugPort),
		"profileId":          c.ProfileID(),
		managedProfileMarker: true,
		"updatedAt":          time.Now().UTC().Format(time.RFC3339),
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	_ = storage.WriteAtomic(connectionFile, encoded)
}

func (c *Client) clearSavedConnection() {
	connectionFile := c.connectionFile()
	if strings.TrimSpace(connectionFile) == "" {
		return
	}
	_ = os.Remove(connectionFile)
}

func findManagedBrowserExecutable() (string, error) {
	candidates := []string{
		strings.TrimSpace(os.Getenv("AISTUDIO_BROWSER_PATH")),
		filepath.Join(os.Getenv("ProgramFiles"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Microsoft", "Edge", "Application", "msedge.exe"),
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) != "" {
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
	}
	for _, binary := range []string{"google-chrome", "chromium-browser", "chromium", "msedge", "microsoft-edge"} {
		if path, err := exec.LookPath(binary); err == nil {
			return path, nil
		}
	}
	return "", errors.New("cdp: nao encontrei chrome/msedge instalado para navegador gerenciado")
}

func waitForBrowserWSEndpoint(debugPort int, timeout time.Duration) (string, error) {
	startedAt := time.Now()
	var lastErr error = errors.New("timeout")
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	candidates := []string{
		fmt.Sprintf("http://127.0.0.1:%d/json/version", debugPort),
		fmt.Sprintf("http://localhost:%d/json/version", debugPort),
		fmt.Sprintf("http://[::1]:%d/json/version", debugPort),
	}

	for time.Since(startedAt) < timeout {
		for _, endpoint := range candidates {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, nil)
			if err != nil {
				lastErr = err
				continue
			}
			resp, err := client.Do(req)
			if err != nil {
				lastErr = err
				continue
			}
			var payload struct {
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}
			err = json.NewDecoder(resp.Body).Decode(&payload)
			_ = resp.Body.Close()
			if err == nil && strings.TrimSpace(payload.WebSocketDebuggerURL) != "" {
				return strings.TrimSpace(payload.WebSocketDebuggerURL), nil
			}
			lastErr = fmt.Errorf("%s -> http %d", endpoint, resp.StatusCode)
		}
		time.Sleep(250 * time.Millisecond)
	}

	return "", fmt.Errorf("cdp: timeout aguardando ws endpoint do browser (%v)", lastErr)
}

func chooseManagedDebugPort(profileID string) int {
	base := 64000
	spread := 1000
	hash := 7
	for _, ch := range profileID {
		hash = ((hash * 31) + int(ch)) & 0x7fffffff
	}
	return base + (hash % spread)
}

func extractDebugPort(wsEndpoint string) int {
	parsed, err := url.Parse(strings.TrimSpace(wsEndpoint))
	if err != nil {
		return 0
	}
	if parsed.Port() == "" {
		return 0
	}
	n, err := strconv.Atoi(parsed.Port())
	if err != nil {
		return 0
	}
	return n
}

func killManagedProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	if runtime.GOOS == "windows" {
		cmd := exec.Command("taskkill", "/PID", fmt.Sprintf("%d", pid), "/T", "/F")
		return cmd.Run()
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

func killManagedProfileProcesses(parent context.Context, userDataDir string) error {
	if runtime.GOOS != "windows" || strings.TrimSpace(userDataDir) == "" {
		return nil
	}
	escaped := strings.ReplaceAll(userDataDir, "'", "''")
	script := fmt.Sprintf(
		"$needle = '%s'; Get-CimInstance Win32_Process | Where-Object { ($_.Name -eq 'chrome.exe' -or $_.Name -eq 'msedge.exe') -and $_.CommandLine -like \"*$needle*\" } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }",
		escaped,
	)
	cmd := exec.CommandContext(parent, "powershell.exe", "-NoProfile", "-Command", script)
	if runtime.GOOS == "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	}
	return cmd.Run()
}
