package register

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testPython(t *testing.T) string {
	t.Helper()
	names := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		names = []string{"python", "py"}
	}
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	t.Skip("Python not installed")
	return ""
}

func TestRunnerParsesProtocolAndArguments(t *testing.T) {
	python := testPython(t)
	dir := t.TempDir()
	script := `import json, sys
args = sys.argv[1:]
provider = args[args.index("--email-provider") + 1]
assert args[args.index("--verification-url") + 1] == "https://auth.example/device"
assert args[args.index("--user-code") + 1] == "ABC-123"
assert args[args.index("--max-attempts") + 1] == "3"
print("__STEP__ discovery scanning", flush=True)
print("__CREDS__ " + json.dumps({"email":"new@example.com","password":"secret","provider":provider}), flush=True)
print("__RESULT__ " + json.dumps({"status":"success"}), flush=True)
`
	if err := os.WriteFile(filepath.Join(dir, "grok_signup.py"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := New(python, dir)
	runner.EmailProvider = "mailtm"
	runner.MaxAttempts = 3
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var steps []string
	result, err := runner.CreateAccount(ctx, "https://auth.example/device", "ABC-123", func(p Progress) {
		steps = append(steps, p.Step)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Status != "success" {
		t.Fatalf("result=%#v", result)
	}
	if result.Creds["email"] != "new@example.com" || result.Creds["provider"] != "mailtm" {
		t.Fatalf("creds=%#v", result.Creds)
	}
	if len(steps) != 1 || !strings.Contains(steps[0], "discovery") {
		t.Fatalf("steps=%#v", steps)
	}
}

func TestEmbeddedPythonCompiles(t *testing.T) {
	python := testPython(t)
	dir := t.TempDir()
	botDir, err := ExtractEmbeddedBot(dir)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(botDir, "grok_signup.py")
	cmd := exec.Command(python, "-c", `import sys; p=sys.argv[1]; compile(open(p, encoding="utf-8").read(), p, "exec")`, script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("python compile: %v: %s", err, out)
	}
}
