package register

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	depsMu   sync.Mutex
	depsDone = map[string]bool{} // key: venv path + req hash
)

// VenvDir returns the app-managed virtualenv path under dataRoot.
func VenvDir(dataRoot string) string {
	return filepath.Join(dataRoot, "python-venv")
}

func venvPython(venv string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Scripts", "python.exe")
	}
	return filepath.Join(venv, "bin", "python3")
}

func requirementsHash(reqPath string) string {
	b, err := os.ReadFile(reqPath)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// EnsureBotDeps creates dataRoot/python-venv if needed and pip-installs bot requirements.
// Returns the python executable to use (venv preferred). Idempotent when marker matches.
func EnsureBotDeps(ctx context.Context, dataRoot, hostPython, botDir string, progress func(string)) (pythonPath string, err error) {
	if dataRoot == "" || botDir == "" {
		return hostPython, fmt.Errorf("dataRoot/botDir required")
	}
	req := filepath.Join(botDir, "requirements.txt")
	if st, e := os.Stat(req); e != nil || st.IsDir() {
		return hostPython, nil // nothing to install
	}
	hash := requirementsHash(req)
	venv := VenvDir(dataRoot)
	py := venvPython(venv)
	marker := filepath.Join(venv, ".deps-hash")

	depsMu.Lock()
	defer depsMu.Unlock()
	key := venv + "|" + hash
	if depsDone[key] {
		if st, e := os.Stat(py); e == nil && !st.IsDir() {
			return py, nil
		}
	}
	if b, e := os.ReadFile(marker); e == nil && strings.TrimSpace(string(b)) == hash {
		if st, e := os.Stat(py); e == nil && !st.IsDir() {
			// Quick import check for the two runtime-critical packages.
			if checkImport(ctx, py, "curl_cffi") && checkImport(ctx, py, "patchright") && checkImport(ctx, py, "DrissionPage") {
				depsDone[key] = true
				return py, nil
			}
		}
	}

	emit := func(s string) {
		log.Printf("register deps: %s", s)
		if progress != nil {
			progress(s)
		}
	}

	if hostPython == "" {
		hostPython = "python3"
		if runtime.GOOS == "windows" {
			hostPython = "python"
		}
	}

	// Create venv if missing or python broken
	if st, e := os.Stat(py); e != nil || st.IsDir() {
		emit("creating python venv…")
		_ = os.RemoveAll(venv)
		if err := os.MkdirAll(filepath.Dir(venv), 0o700); err != nil {
			return hostPython, err
		}
		if err := runPython(ctx, hostPython, nil, "-m", "venv", venv); err != nil {
			return hostPython, fmt.Errorf("python -m venv: %w (install Python 3 from python.org with pip)", err)
		}
		py = venvPython(venv)
	}

	emit("pip install signup dependencies…")
	// Upgrade pip quietly then install requirements
	_ = runPython(ctx, py, nil, "-m", "pip", "install", "--upgrade", "pip", "setuptools", "wheel")
	if err := runPython(ctx, py, nil, "-m", "pip", "install", "-r", req); err != nil {
		return hostPython, fmt.Errorf("pip install: %w", err)
	}
	if !checkImport(ctx, py, "curl_cffi") || !checkImport(ctx, py, "patchright") || !checkImport(ctx, py, "DrissionPage") {
		return hostPython, fmt.Errorf("signup dependencies still missing after pip install (python=%s)", py)
	}
	if err := os.WriteFile(marker, []byte(hash+"\n"), 0o600); err != nil {
		return py, err
	}
	depsDone[key] = true
	emit("bot deps ready")
	return py, nil
}

func checkImport(ctx context.Context, py, mod string) bool {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := runPython(cctx, py, nil, "-c", "import "+mod)
	return err == nil
}

func runPython(ctx context.Context, py string, env []string, args ...string) error {
	var cmd *exec.Cmd
	base := strings.ToLower(filepath.Base(py))
	if runtime.GOOS == "windows" && (base == "py" || base == "py.exe") {
		cmd = exec.CommandContext(ctx, py, append([]string{"-3"}, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, py, args...)
	}
	if env != nil {
		cmd.Env = env
	} else {
		cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8", "PIP_DISABLE_PIP_VERSION_CHECK=1")
	}
	hideConsoleWindow(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String() + "\n" + stdout.String())
		if len(msg) > 600 {
			msg = msg[:600] + "…"
		}
		msg = strings.ReplaceAll(msg, "\r\n", " ")
		msg = strings.ReplaceAll(msg, "\n", " ")
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}
