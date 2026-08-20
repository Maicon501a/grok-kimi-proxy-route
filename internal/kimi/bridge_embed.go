package kimi

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

// Embedded Google-login bridge (Node CJS script + batchexecute templates).
// Extracted under the app cache dir at runtime so a bare executable carries
// the whole login flow natively — no sibling source tree required.
//
//go:embed all:bridge
var bridgeFS embed.FS

// bridgeEmbedVersion is a short content fingerprint used as the extract
// directory name; changes whenever any embedded bridge file changes.
func bridgeEmbedVersion() string {
	h := sha256.New()
	_ = fs.WalkDir(bridgeFS, "bridge", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := bridgeFS.ReadFile(path)
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

// bridgeCacheRoot is where the embedded bridge is extracted.
func bridgeCacheRoot() (string, error) {
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "GrokDesktop"), nil
	}
	return os.TempDir(), nil
}

// extractEmbeddedBridge writes the embedded bridge into
// <cache>/GrokDesktop/google-bridge/<version>/ and returns that directory.
// Skips rewrite when the marker matches.
func extractEmbeddedBridge() (string, error) {
	dataRoot, err := bridgeCacheRoot()
	if err != nil {
		return "", err
	}
	ver := bridgeEmbedVersion()
	dest := filepath.Join(dataRoot, "google-bridge", ver)
	marker := filepath.Join(dest, ".embed-version")
	script := filepath.Join(dest, "login_bridge.cjs")
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
	err = fs.WalkDir(bridgeFS, "bridge", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("bridge", path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		out := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o700)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return err
		}
		src, err := bridgeFS.Open(path)
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
		return "", fmt.Errorf("bridge extract incomplete: missing %s", script)
	}
	// Best-effort prune older extract versions
	parent := filepath.Join(dataRoot, "google-bridge")
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if !e.IsDir() || e.Name() == ver {
			continue
		}
		_ = os.RemoveAll(filepath.Join(parent, e.Name()))
	}
	return dest, nil
}

// bridgeTemplate reads a batchexecute payload template from the embedded FS.
func bridgeTemplate(name string) (string, error) {
	b, err := bridgeFS.ReadFile("bridge/templates/" + name)
	if err != nil {
		return "", fmt.Errorf("embedded template %s: %w", name, err)
	}
	return string(b), nil
}
