# Google → Kimi Auth Flow - Research Summary

**Date:** 2026-07-17
**Objective:** Determine if Google OAuth login → Kimi can be automated via HTTP direct (without Playwright/browser)
**Result:** **PARTIAL** — Sign-in step requires browser, but token refresh is fully HTTP-direct

---

## What We Did

1. **Decoded embedded credentials** from `internal/kimi/google_login.go` (XOR 0x5A)
2. **Captured live traffic** via Reqable proxy during Chrome browser login
3. **Analyzed 83+ requests** to `accounts.google.com` and internal Google APIs
4. **Tested HTTP-direct** approaches with Go scripts (all fail at sign-in)
5. **Documented all endpoints** with request/response examples

---

## Key Findings

### The Google Sign-in Wall

Google's OAuth flow cannot be bypassed with HTTP requests alone because:

- **Dynamic tokens**: `dsh`, `part`, `rapt`, `at` are generated per-session and embedded in JS
- **JavaScript requirement**: The sign-in page (`/v3/signin/identifier`) is ~1MB of HTML+JS that renders tokens dynamically
- **Browser fingerprinting**: `POST /v3/signin/_/AccountsSignInUi/browserinfo` sends viewport metrics, feature flags, and a validation hash
- **Cookie dependency**: Requires `SID`, `HSID`, `SSID`, `APISID`, `SAPISID`, `NID`, `OTZ`, `__Host-GAPS` with complex lifecycles
- **Anti-bot**: CAPTCHA triggers on suspicious patterns

### The Working HTTP-Direct Path (for VMs)

Once you have a **Google refresh_token** (obtained once via browser), everything else is HTTP-direct:

```
Google refresh_token ──► POST oauth2.googleapis.com/token ──► id_token
                                                          │
                                                          ▼
                                              POST www.kimi.com/api/auth/login/google
                                                          │
                                                          ▼
                                              Kimi access_token + refresh_token
                                                          │
                                                          ▼
                                              GET www.kimi.com/api/auth/token/refresh
```

**Files:**
- `test_refresh_token.go` — Tests Google refresh token → id_token (no browser)
- `test_kimi_login.go` — Tests id_token → Kimi session (no browser)
- `test_direct_oauth.go` — Proves HTTP-direct sign-in fails
- `test_direct_oauth_valid_pkce.go` — Proves even valid PKCE hits the JS wall

---

## Credentials (from source code)

```
client_id:     626581754197-v82pavblj7tgk6ap9ouqbi9lv821l6qo.apps.googleusercontent.com
client_secret: <redacted; provide GOOGLE_CLIENT_SECRET locally>
```

These are the Kimi Desktop OAuth app credentials, embedded via XOR obfuscation.

---

## Endpoint Reference

| # | Endpoint | Method | Browser? | Purpose |
|---|----------|--------|----------|---------|
| 1 | `accounts.google.com/o/oauth2/v2/auth` | GET | **YES** | Start OAuth, redirects to sign-in |
| 2 | `accounts.google.com/v3/signin/identifier` | GET | **YES** | Sign-in page (JS-rendered) |
| 3 | `accounts.google.com/v3/signin/_/AccountsSignInUi/browserinfo` | POST | **YES** | Fingerprinting + session init |
| 4 | `accounts.google.com/signin/oauth/v3/consent` | GET | **YES** | OAuth consent (after login) |
| 5 | `oauth2.googleapis.com/token` | POST | **NO** | Exchange code → tokens |
| 6 | `oauth2.googleapis.com/token` | POST | **NO** | Refresh token → new id_token |
| 7 | `www.kimi.com/api/auth/login/google` | POST | **NO** | id_token → Kimi session |
| 8 | `www.kimi.com/api/auth/token/refresh` | GET | **NO** | Extend Kimi session |

---

## Captured Traffic Artifacts

From Reqable captures (see `ENDPOINTS.md` for full details):

### Google Internal APIs (Chrome-only, not reusable)
- `oauthaccountmanager.googleapis.com/v1/issuetoken` — Chrome sync tokens, bound to browser profile
- `play.google.com/log` — Analytics/telemetry
- `update.googleapis.com` — Chrome update checks

### Loopback Callbacks (from browser to app)
- `GET 127.0.0.1:61120/callback?code=4/0AXEQxI...&scope=...&authuser=0&prompt=none`
- These are the authorization codes captured by the Go HTTP server

---

## VM Deployment Strategy

```
Phase 1 (One-time, with browser/Playwright):
  ├─ Run full login flow
  ├─ Capture Google refresh_token
  └─ Capture Kimi refresh_token

Phase 2 (Runtime on VM, HTTP direct):
  ├─ Google refresh_token → POST oauth2.googleapis.com/token → id_token
  ├─ id_token → POST www.kimi.com/api/auth/login/google → Kimi tokens
  └─ Kimi refresh_token → GET /api/auth/token/refresh → extend session

Phase 3 (When refresh fails):
  └─ Re-run Phase 1 (browser re-auth required)
```

**Storage:** Encrypt and persist both refresh tokens in a secure store (e.g., system keychain or encrypted file).

---

## Test Results

### test_direct_oauth.go
```
FAIL: Redirected to error page (invalid code_challenge format)
→ Even trivial validation errors are caught immediately
```

### test_direct_oauth_valid_pkce.go
```
OK: Reached sign-in page (/v3/signin/identifier)
Body: 1,048,576 bytes of HTML+JS
→ Contains dsh=S1210028423:1784300360456329
→ Contains continue URL with part=AJi8hANDZtId...
→ Contains rart=ANgoxcdyrt9VmnXmvcpaw...
→ No form tokens extractable without JS execution
CONCLUSION: HTTP-direct sign-in is NOT possible
```

---

## Files

| File | Purpose |
|------|---------|
| `README.md` | This file |
| `ENDPOINTS.md` | Full endpoint analysis with headers and bodies |
| `decode_creds.go` | Extracts OAuth credentials from source XOR |
| `test_refresh_token.go` | Tests Google refresh token flow (works without browser) |
| `test_kimi_login.go` | Tests Kimi login with Google id_token (works without browser) |
| `test_direct_oauth.go` | Proves HTTP-direct sign-in fails (invalid PKCE) |
| `test_direct_oauth_valid_pkce.go` | Proves HTTP-direct sign-in fails even with valid PKCE |
