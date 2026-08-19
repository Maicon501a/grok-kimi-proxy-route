// Package auth builds Google AI Studio runtime authentication material from
// captured request headers and browser cookies.
package auth

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const Origin = "https://aistudio.google.com"

// allowedHeaders is the allow-list of captured headers carried into runtime
// requests. Order is preserved to match the original behavior.
var allowedHeaders = []string{
	"X-User-Agent",
	"Content-Type",
	"X-AIStudio-Visit-Id",
	"X-Goog-Ext-519733851-bin",
	"X-Goog-Api-Key",
	"X-Goog-AuthUser",
}

// Cookie is a simplified browser cookie.
type Cookie struct {
	Name  string
	Value string
}

// RuntimeAuth bundles the computed runtime authentication material.
type RuntimeAuth struct {
	Headers            map[string]string
	AuthUser           string
	SAPISID            string
	AccountFingerprint string
	CookieString       string
}

// BuildRuntimeAuth computes authorization headers, the account fingerprint and
// the cookie string from the captured headers and browser cookies.
// Returns an error if no SAPISID/__Secure-3PAPISID cookie is present.
func BuildRuntimeAuth(capturedHeaders map[string]string, cookies []Cookie) (*RuntimeAuth, error) {
	sapisid := ""
	for _, c := range cookies {
		if c.Name == "SAPISID" || c.Name == "__Secure-3PAPISID" {
			sapisid = c.Value
			break
		}
	}
	if sapisid == "" {
		return nil, fmt.Errorf("auth: cookie SAPISID/3PAPISID nao encontrado na sessao atual")
	}

	headers := make(map[string]string, len(allowedHeaders))
	for _, key := range allowedHeaders {
		if v := pickHeader(capturedHeaders, key); v != "" {
			headers[key] = v
		}
	}

	authUser := headers["X-Goog-AuthUser"]
	if authUser == "" {
		authUser = "0"
	}
	headers["Authorization"] = BuildAuthorization(sapisid)

	return &RuntimeAuth{
		Headers:            headers,
		AuthUser:           authUser,
		SAPISID:            sapisid,
		AccountFingerprint: BuildAccountFingerprint(sapisid, authUser),
		CookieString:       BuildCookieString(cookies),
	}, nil
}

// BuildAuthorization returns the composite SAPISIDHASH authorization header
// value Google expects for AI Studio RPCs.
func BuildAuthorization(sapisid string) string {
	ts := time.Now().Unix()
	digest := sha1Hash(fmt.Sprintf("%d %s %s", ts, sapisid, Origin))
	return fmt.Sprintf("SAPISIDHASH %d_%s SAPISID1PHASH %d_%s SAPISID3PHASH %d_%s",
		ts, digest, ts, digest, ts, digest)
}

// BuildAccountFingerprint returns a stable hash that identifies the active
// account for migration/dedup purposes.
func BuildAccountFingerprint(sapisid, authUser string) string {
	return sha1Hash(authUser + ":" + sapisid)
}

// BuildCookieString assembles a "k=v; k=v" cookie header from a cookie list.
func BuildCookieString(cookies []Cookie) string {
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		if c.Name == "" {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// SHA256Hex returns the hex-encoded SHA-256 digest of the input.
func SHA256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func sha1Hash(text string) string {
	sum := sha1.Sum([]byte(text))
	return hex.EncodeToString(sum[:])
}

// canonicalLookup builds a case-insensitive header map keyed by the canonical
// (Title-Case) header name. This replaces the original's ad-hoc per-key
// lowercasing which was both inconsistent and O(n) per lookup.
func pickHeader(headers map[string]string, expectedKey string) string {
	if headers == nil {
		return ""
	}
	if value := headers[expectedKey]; value != "" {
		return value
	}
	for key, value := range headers {
		if strings.EqualFold(key, expectedKey) && value != "" {
			return value
		}
	}
	return ""
}

// ParseAuthUser extracts a numeric auth user from a string, defaulting to 0.
func ParseAuthUser(value string) int {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}
