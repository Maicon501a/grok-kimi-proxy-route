// Package secure encrypts sensitive values (API keys) at rest.
//
// On Windows it uses DPAPI (CryptProtectData / CryptUnprotectData) scoped to
// the current user, so the ciphertext can only be decrypted by the same user
// on the same machine — the OS manages the master key, and no key material is
// ever stored next to the blob. On other platforms it falls back to AES-256-GCM
// with a key derived from the machine + user identity.
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	b64 "encoding/base64"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
)

// ErrCannotDecrypt is returned when the stored blob cannot be decrypted
// (different user/machine, tampered data, or a non-Windows key mismatch).
var ErrCannotDecrypt = errors.New("secure: cannot decrypt value")

// Encrypt protects a plaintext value and returns a base64 ciphertext blob.
// The result is portable across app restarts for the same OS user.
func Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	blob, err := encryptAtRest([]byte(plaintext))
	if err != nil {
		return "", err
	}
	prefix := "aes:"
	if runtime.GOOS == "windows" {
		prefix = "dpapi:"
	}
	return prefix + b64.StdEncoding.EncodeToString(blob), nil
}

// Decrypt reverses Encrypt. Empty input returns an empty string (no error).
func Decrypt(ciphertext string) (string, error) {
	ct := strings.TrimSpace(ciphertext)
	if ct == "" {
		return "", nil
	}
	switch {
	case strings.HasPrefix(ct, "dpapi:"):
		raw, err := b64.StdEncoding.DecodeString(strings.TrimPrefix(ct, "dpapi:"))
		if err != nil {
			return "", ErrCannotDecrypt
		}
		out, err := decryptDPAPI(raw)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrCannotDecrypt, err)
		}
		return string(out), nil
	case strings.HasPrefix(ct, "aes:"):
		raw, err := b64.StdEncoding.DecodeString(strings.TrimPrefix(ct, "aes:"))
		if err != nil {
			return "", ErrCannotDecrypt
		}
		out, err := aesGCMOpen(raw)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrCannotDecrypt, err)
		}
		return string(out), nil
	default:
		// Legacy plaintext (should not exist for new fields) — surface as-is.
		return ct, nil
	}
}

// HasCiphertext reports whether a stored value looks like an encrypted blob.
func HasCiphertext(value string) bool {
	v := strings.TrimSpace(value)
	return strings.HasPrefix(v, "dpapi:") || strings.HasPrefix(v, "aes:")
}

// ---------- Non-Windows fallback (AES-256-GCM) ----------

// aesKey derives a per-user machine key from OS identity. This is weaker than
// DPAPI (key lives on the same disk) but keeps non-Windows builds functional.
func aesKey() []byte {
	parts := []string{runtime.GOOS}
	if h, err := os.Hostname(); err == nil {
		parts = append(parts, h)
	}
	if home, err := os.UserHomeDir(); err == nil {
		parts = append(parts, home)
	}
	sum := sha256.Sum256([]byte("grok-desktop/secure/v1|" + strings.Join(parts, "|")))
	return sum[:]
}

func aesGCMSeal(plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(aesKey())
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func aesGCMOpen(blob []byte) ([]byte, error) {
	block, err := aes.NewCipher(aesKey())
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize() {
		return nil, ErrCannotDecrypt
	}
	return gcm.Open(nil, blob[:gcm.NonceSize()], blob[gcm.NonceSize():], nil)
}
