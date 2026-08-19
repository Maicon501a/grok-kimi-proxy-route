package register

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Embedded signup bot (Python). Extracted under the app data dir at runtime so a
// bare executable works without a sibling source tree.
//
//go:embed all:bot
var botFS embed.FS

// BotEmbedVersion is a short content fingerprint used as the extract directory name.
// Rebuilt whenever any embedded bot file changes.
func BotEmbedVersion() string {
	h := sha256.New()
	_ = fs.WalkDir(botFS, "bot", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := botFS.ReadFile(path)
		if err != nil {
			return err
		}
		_, _ = h.Write([]byte(path))
		_, _ = h.Write(b)
		return nil
	})
	sum := hex.EncodeToString(h.Sum(nil))
	if len(sum) > 12 {
		sum = sum[:12]
	}
	return sum
}

// ExtractEmbeddedBot writes the embedded bot into dataRoot/signup-bot/<version>/
// and returns that directory (absolute). Skips rewrite when the marker matches.
func ExtractEmbeddedBot(dataRoot string) (string, error) {
	if dataRoot == "" {
		return "", fmt.Errorf("data root empty")
	}
	ver := BotEmbedVersion()
	dest := filepath.Join(dataRoot, "signup-bot", ver)
	marker := filepath.Join(dest, ".embed-version")
	script := filepath.Join(dest, "grok_signup.py")
	if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) == ver {
		if st, err := os.Stat(script); err == nil && !st.IsDir() {
			return dest, nil
		}
	}
	if err := os.RemoveAll(dest); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return "", err
	}
	err := fs.WalkDir(botFS, "bot", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("bot", path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// skip junk if present
		base := filepath.Base(rel)
		if base == "__pycache__" || strings.HasSuffix(base, ".pyc") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		out := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o700)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return err
		}
		src, err := botFS.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		f, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(f, src)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		_ = os.RemoveAll(dest)
		return "", err
	}
	if err := os.WriteFile(marker, []byte(ver+"\n"), 0o600); err != nil {
		return "", err
	}
	if st, err := os.Stat(script); err != nil || st.IsDir() {
		return "", fmt.Errorf("extract incomplete: missing %s", script)
	}
	// Best-effort prune older extract versions
	parent := filepath.Join(dataRoot, "signup-bot")
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if !e.IsDir() || e.Name() == ver {
			continue
		}
		_ = os.RemoveAll(filepath.Join(parent, e.Name()))
	}
	return dest, nil
}

// HasEmbeddedBot reports whether the binary includes the signup bot tree.
func HasEmbeddedBot() bool {
	_, err := botFS.ReadFile("bot/grok_signup.py")
	return err == nil
}
