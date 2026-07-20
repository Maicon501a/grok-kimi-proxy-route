package main

import (
	"embed"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"grok-desktop/internal/logging"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// Kill any previous GrokDesktop.exe before we start (prevents stale proxy/store).
	killOtherGrokDesktopProcesses()

	// Log panics so a real crash leaves a trail (does not swallow them).
	defer func() {
		if r := recover(); r != nil {
			logCrash(fmt.Sprintf("panic: %v\n%s", r, debug.Stack()))
			panic(r)
		}
	}()
	setupFileLog()

	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "Grok Desktop",
		Width:     1280,
		Height:    860,
		MinWidth:  960,
		MinHeight: 640,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 5, G: 5, B: 5, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind: []interface{}{
			app,
		},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			Theme:                windows.Dark,
		},
	})
	if err != nil {
		log.Printf("wails.Run: %v", err)
		logCrash("wails.Run: " + err.Error())
		println("Error:", err.Error())
	}
}

// killOtherGrokDesktopProcesses terminates every GrokDesktop.exe process
// except the current one. Uses PID enumeration — never taskkill /IM (would
// suicide the new process before it finishes starting).
func killOtherGrokDesktopProcesses() {
	myPid := os.Getpid()

	out, _ := exec.Command("wmic", "process", "where", "name='GrokDesktop.exe'", "get", "ProcessId", "/format:csv").Output()
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Node") {
			continue
		}
		for _, f := range strings.Split(line, ",") {
			f = strings.TrimSpace(f)
			if pid, err := strconv.Atoi(f); err == nil && pid > 0 && pid != myPid {
				_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
			}
		}
	}

	// Grace period so Windows releases ports/handles before we bind.
	time.Sleep(400 * time.Millisecond)
}

func setupFileLog() {
	dir, err := defaultLogDir()
	if err != nil {
		return
	}
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "app.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	mw := io.MultiWriter(os.Stdout, f)
	log.SetOutput(mw)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	logging.SetOutput(mw)
	logging.SetLevel(logging.ParseLevel(os.Getenv("GROK_LOG_LEVEL")))
	log.Printf("——— Grok Desktop start %s ———", time.Now().Format(time.RFC3339))
}

func logCrash(msg string) {
	dir, err := defaultLogDir()
	if err != nil {
		return
	}
	path := filepath.Join(dir, "crash.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(fmt.Sprintf("[%s] %s\n", time.Now().Format(time.RFC3339), msg))
}

func defaultLogDir() (string, error) {
	appData := os.Getenv("LOCALAPPDATA")
	if appData == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		appData = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(appData, "GrokDesktop", "logs"), nil
}
