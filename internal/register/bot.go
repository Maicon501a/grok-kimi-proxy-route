package register

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Progress represents a step event from the signup bot.
type Progress struct {
	Step    string `json:"step"`
	Message string `json:"message"`
}

// Result is the final outcome from the bot.
type Result struct {
	Status      string            `json:"status"` // "success" | "error"
	Reason      string            `json:"reason,omitempty"`
	Step        string            `json:"step,omitempty"`
	Screenshot  string            `json:"screenshot,omitempty"`
	Creds       map[string]string `json:"creds,omitempty"` // email, name, password, provider
	AccessToken string            `json:"access_token,omitempty"`
}

// Runner manages the embedded Python signup subprocess.
type Runner struct {
	PythonPath    string // default "python3" / "python" on Windows
	BotDir        string // extracted embedded bot directory
	DataRoot      string // app data root used for the managed virtualenv
	EmailProvider string // luckmail | mailnest | gmail | mailtm
	MaxAttempts   int
}

// New creates a Runner with sensible defaults.
func New(pythonPath, botDir string) *Runner {
	if pythonPath == "" {
		if runtime.GOOS == "windows" {
			pythonPath = "python"
		} else {
			pythonPath = "python3"
		}
	}
	return &Runner{PythonPath: pythonPath, BotDir: botDir}
}

// CreateAccount runs the signup bot with the given verification URL.
// It streams progress events via the progress callback and returns
// the final Result. The context timeout should match the device grant TTL.
func (r *Runner) CreateAccount(
	ctx context.Context,
	verificationURL string,
	userCode string,
	progress func(Progress),
) (*Result, error) {
	script := filepath.Join(r.BotDir, "grok_signup.py")
	if st, err := os.Stat(script); err != nil || st.IsDir() {
		return &Result{
			Status: "error",
			Reason: fmt.Sprintf(
				"embedded signup script missing after extraction: %s",
				script,
			),
			Step: "start",
		}, nil
	}
	if _, err := exec.LookPath(r.PythonPath); err != nil {
		// absolute path may not be on PATH
		if st, err2 := os.Stat(r.PythonPath); err2 != nil || st.IsDir() {
			return &Result{
				Status: "error",
				Reason: fmt.Sprintf("python not found: %q (install Python 3 + pip, or set python_path in settings)", r.PythonPath),
				Step:   "start",
			}, nil
		}
	}

	// Auto venv + dependency install under AppData. Credentials are never used
	// as a reason to enable dependency setup or plaintext persistence.
	if r.DataRoot != "" {
		if progress != nil {
			progress(Progress{Step: "deps", Message: "checking python packages"})
		}
		py, err := EnsureBotDeps(ctx, r.DataRoot, r.PythonPath, r.BotDir, func(msg string) {
			if progress != nil {
				progress(Progress{Step: "deps", Message: msg})
			}
		})
		if err != nil {
			return &Result{
				Status: "error",
				Reason: fmt.Sprintf("bot deps: %v", err),
				Step:   "deps",
			}, nil
		}
		if py != "" {
			r.PythonPath = py
		}
	}

	args := []string{
		script,
		"--verification-url", verificationURL,
		"--headless", "false",
	}
	if userCode != "" {
		args = append(args, "--user-code", userCode)
	}
	if provider := strings.TrimSpace(r.EmailProvider); provider != "" {
		args = append(args, "--email-provider", provider)
	}
	if r.MaxAttempts > 0 {
		args = append(args, "--max-attempts", fmt.Sprint(r.MaxAttempts))
	}

	// Do not use CommandContext — we need to kill the full process tree (Chrome)
	// ourselves on cancel, and hide the console on Windows.
	var cmd *exec.Cmd
	base := strings.ToLower(filepath.Base(r.PythonPath))
	if runtime.GOOS == "windows" && (base == "py" || base == "py.exe") {
		cmd = exec.Command(r.PythonPath, append([]string{"-3"}, args...)...)
	} else {
		cmd = exec.Command(r.PythonPath, args...)
	}
	cmd.Dir = r.BotDir
	// PYTHONUTF8 reduces cp1252 issues on Windows consoles
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
	// Visible Chrome: do not CREATE_NO_WINDOW / HideWindow on Windows.
	allowGUIChildren(cmd)
	prepareProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("register stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("register stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return &Result{
			Status: "error",
			Reason: fmt.Sprintf("failed to start bot: %v (python=%s script=%s)", err, r.PythonPath, script),
			Step:   "start",
		}, nil
	}
	if err := setupProcessJob(cmd); err != nil {
		// taskkill/process-group fallback still works; log the weaker lifecycle
		// instead of failing a signup that has already started.
		log.Printf("register: process job setup failed: %v", err)
	}

	var (
		stderrMu  sync.Mutex
		stderrBuf strings.Builder
		resultCh  = make(chan *Result, 1)
		creds     map[string]string
		waitErr   error
	)
	waitDone := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		releaseProcessJob(cmd)
		close(waitDone)
	}()

	// Kill tree if context cancelled while bot is still running.
	go func() {
		select {
		case <-ctx.Done():
			killProcessTree(cmd)
		case <-waitDone:
		}
	}()

	go func() {
		defer close(resultCh)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 256*1024)
		for sc.Scan() {
			line := sc.Text()
			if len(line) < 2 {
				continue
			}
			if line == "__STEP__ done" {
				continue
			}
			if len(line) > 9 && line[:9] == "__STEP__ " {
				step := line[9:]
				p := Progress{Step: step}
				if progress != nil {
					progress(p)
				}
				continue
			}
			if len(line) > 9 && line[:9] == "__CREDS__" && line[9] == ' ' {
				payload := line[10:]
				var c map[string]string
				if err := json.Unmarshal([]byte(payload), &c); err != nil {
					log.Printf("register: bad creds json: %v", err)
					continue
				}
				creds = c
				continue
			}
			if len(line) > 10 && line[:10] == "__RESULT__" && line[10] == ' ' {
				payload := line[11:]
				var res Result
				if err := json.Unmarshal([]byte(payload), &res); err != nil {
					log.Printf("register: bad result json: %v", err)
					continue
				}
				res.Creds = creds
				log.Printf("register: got __RESULT__ status=%s", res.Status)
				resultCh <- &res
				return
			}
			log.Printf("register stdout: %s", line)
		}
		if err := sc.Err(); err != nil {
			log.Printf("register: stdout scan: %v", err)
		} else {
			log.Printf("register: stdout EOF (no __RESULT__)")
		}
	}()

	go func() {
		b, _ := io.ReadAll(stderr)
		if len(b) > 0 {
			stderrMu.Lock()
			stderrBuf.Write(b)
			stderrMu.Unlock()
			msg := string(b)
			if len(msg) > 2000 {
				msg = msg[:2000] + "…"
			}
			log.Printf("register stderr: %s", msg)
		}
	}()

	var result *Result
	select {
	case res := <-resultCh:
		result = res
	case <-ctx.Done():
		killProcessTree(cmd)
		result = &Result{Status: "error", Reason: "timeout/cancelled" + killHint()}
	case <-waitDone:
		// stdout and process completion race on fast scripts. Give the scanner a
		// chance to publish the already-written __RESULT__ before declaring failure.
		select {
		case res := <-resultCh:
			result = res
		case <-time.After(500 * time.Millisecond):
		}
	}

	<-waitDone

	if result == nil {
		stderrMu.Lock()
		errText := strings.TrimSpace(stderrBuf.String())
		stderrMu.Unlock()
		reason := "no result from bot"
		if waitErr != nil {
			reason = fmt.Sprintf("no result from bot (exit: %v)", waitErr)
		}
		if errText != "" {
			snip := errText
			if len(snip) > 400 {
				snip = snip[:400] + "…"
			}
			snip = strings.ReplaceAll(snip, "\r\n", " ")
			snip = strings.ReplaceAll(snip, "\n", " ")
			reason = reason + ": " + snip
		} else {
			reason = reason + fmt.Sprintf(" (python=%s bot_dir=%s — check Python/patchright/Chrome install)", r.PythonPath, r.BotDir)
		}
		result = &Result{Status: "error", Reason: reason, Step: "start"}
	}
	return result, nil
}
