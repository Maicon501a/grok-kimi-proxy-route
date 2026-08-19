package accmgr

// Residential-proxy support for the signup browser.
//
// Motivation: Accio's risk control scores signup VELOCITY per IP/profile.
// After a handful of creations from one IP the issued accounts come out
// limited (entitlement NOT_LOGIN) or blocked (423) — see waf-accio-re.md §13.
// The only way to create accounts at high cadence is to give each creation a
// fresh residential IP. Rotating residential gateways (IPRoyal, Proxy-Cheap,
// BrightData, ...) give one endpoint that returns a new exit IP per
// connection, which is exactly what a per-attempt browser session needs.
//
// Configuration: ACCIO_SIGNUP_PROXY=http://user:pass@gw.example:12321
//   - http:// and socks5:// schemes are supported (Chrome flag passthrough).
//   - user:pass auth is answered via the Fetch auth-challenge handler
//     (Chrome ignores credentials embedded in --proxy-server).
//   - Only the signup BROWSER uses the proxy: it is the component that hits
//     every risk-scored endpoint (signup form, code submission, oauth/code
//     issuance). The token exchange stays on the home IP via the Node
//     sidecar — the same split the official desktop app exhibits.

import (
	"net/url"
	"os"
	"strings"
)

// signupProxy parses ACCIO_SIGNUP_PROXY. Returns nil when unset or invalid.
func signupProxy() *url.URL {
	raw := strings.TrimSpace(os.Getenv("ACCIO_SIGNUP_PROXY"))
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil
	}
	switch u.Scheme {
	case "http", "https", "socks5":
	default:
		return nil
	}
	return u
}

// proxyServerFlag renders the value for Chrome's --proxy-server flag: the
// scheme and host only — credentials are answered through the auth challenge.
func proxyServerFlag(u *url.URL) string {
	return u.Scheme + "://" + u.Host
}

// proxyCredentials extracts the user:pass pair, if present.
func proxyCredentials(u *url.URL) (user, pass string, ok bool) {
	if u.User == nil {
		return "", "", false
	}
	p, _ := u.User.Password()
	return u.User.Username(), p, true
}
