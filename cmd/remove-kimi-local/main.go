package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"grok-desktop/internal/kimi"
	"grok-desktop/internal/store"
)

func main() {
	idFlag := flag.String("id", "kimi-da1h1j223rd5m0f0ahn0", "exact Kimi account id")
	emailFlag := flag.String("email", "vibedark128@gmail.com", "expected account email")
	flag.Parse()

	root, err := store.DefaultDataDir()
	if err != nil {
		fail("data directory: %v", err)
	}
	backup := filepath.Join(os.TempDir(), "opencode", "grokdesktop-account-backup-"+time.Now().UTC().Format("20060102-150405"))
	if err := copyTree(root, backup); err != nil {
		fail("backup store: %v", err)
	}

	st, err := store.Open(root)
	if err != nil {
		fail("open store: %v", err)
	}
	defer st.Close()
	acc, ok := st.GetAccount(strings.TrimSpace(*idFlag))
	if !ok || acc == nil || acc.NormalizedProvider() != store.ProviderKimiWork {
		fail("Kimi account not found: %s", *idFlag)
	}
	if !strings.EqualFold(strings.TrimSpace(acc.Email), strings.TrimSpace(*emailFlag)) {
		fail("email mismatch for %s: got %q", acc.ID, acc.Email)
	}

	remoteStatus := "not_attempted"
	if kimi.HasWebSession(acc.AccessToken, acc.RefreshToken) {
		if _, logoutErr := kimi.LogoutWithSession(acc.AccessToken, acc.RefreshToken); logoutErr != nil {
			remoteStatus = "warning: " + logoutErr.Error()
		} else {
			remoteStatus = "logged_out"
		}
	}
	if err := st.RemoveAccount(acc.ID); err != nil {
		fail("remove account: %v", err)
	}
	if _, stillThere := st.GetAccount(acc.ID); stillThere {
		fail("account still present after removal: %s", acc.ID)
	}
	fmt.Printf("removed_kimi_account id=%s email=%s remote=%s backup=%s\n", acc.ID, acc.Email, remoteStatus, backup)
}

func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
