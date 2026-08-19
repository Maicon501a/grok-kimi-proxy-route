package accio

import (
	"net/url"
	"os"
	"strings"
	"sync"
)

// localSignEnabled reports whether the proxy replaces the addon-generated
// pctb-x-sign with a Go-generated one. Controlled by ACCIO_SG_SIGN_LOCAL
// (default "1" = enabled; set "0" to keep the addon sign).
func localSignEnabled() bool {
	v := strings.TrimSpace(os.Getenv("ACCIO_SG_SIGN_LOCAL"))
	if v == "" {
		return true
	}
	return v != "0"
}

var (
	localSignMu   sync.Mutex
	localSignGen  *SignGenerator
	localSignInit bool
)

// applyLocalSign swaps the pctb-x-sign header for a Go-generated one.
//
// On the first call it extracts the 44-byte session body from the addon's
// real sign (the header value arrives URI-encoded from the daemon) and seeds
// the generator. Subsequent calls only pay the cost of the local generation;
// the daemon is still used for every other header (umt, mini-wua, ...).
//
// Any failure (bad sign, decode error) keeps the addon's sign as a fallback.
func applyLocalSign(headers map[string]string) map[string]string {
	if !localSignEnabled() {
		return headers
	}
	signEnc, ok := headers["pctb-x-sign"]
	if !ok {
		return headers
	}
	sign := signEnc
	if unescaped, err := url.QueryUnescape(signEnc); err == nil {
		sign = unescaped
	}
	if !strings.HasPrefix(sign, signMarker) {
		return headers
	}

	localSignMu.Lock()
	defer localSignMu.Unlock()

	if !localSignInit {
		body, err := ExtractSignBody(sign)
		if err != nil {
			return headers // keep addon sign
		}
		gen, err := NewSignGenerator(body)
		if err != nil {
			return headers
		}
		localSignGen = gen
		localSignInit = true
	}
	if localSignGen == nil {
		return headers
	}
	generated, err := localSignGen.Generate()
	if err != nil {
		return headers
	}
	headers["pctb-x-sign"] = url.QueryEscape(generated)
	return headers
}
