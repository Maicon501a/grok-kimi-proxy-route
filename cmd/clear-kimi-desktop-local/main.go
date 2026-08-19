package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	clear := flag.Bool("clear", false, "backup and remove Kimi Desktop Local Storage auth database")
	flag.Parse()
	if !*clear {
		fail("dry run: pass -clear to remove the local Kimi Desktop auth database")
	}
	if kimiRunning() {
		fail("Kimi.exe is running; close Kimi Desktop before clearing local auth")
	}
	appData := os.Getenv("APPDATA")
	if appData == "" {
		home, _ := os.UserHomeDir()
		appData = filepath.Join(home, "AppData", "Roaming")
	}
	levelDB := filepath.Join(appData, "kimi-desktop", "Local Storage", "leveldb")
	entries, err := os.ReadDir(levelDB)
	if err != nil {
		fail("read local storage: %v", err)
	}
	backup := filepath.Join(os.TempDir(), "opencode", "kimi-desktop-leveldb-backup-"+time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(backup, 0o700); err != nil {
		fail("create backup: %v", err)
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		src := filepath.Join(levelDB, entry.Name())
		dst := filepath.Join(backup, entry.Name())
		if err := copyFile(src, dst); err != nil {
			fail("backup %s: %v", entry.Name(), err)
		}
		if err := os.Remove(src); err != nil {
			fail("remove %s after backup: %v", entry.Name(), err)
		}
		removed++
	}
	fmt.Printf("cleared_kimi_desktop_local_auth files=%d backup=%s\n", removed, backup)
}

func kimiRunning() bool {
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq Kimi.exe", "/NH").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "kimi.exe")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	return errors.Join(copyErr, closeErr)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
