# Google → Kimi Auth Flow - Endpoint Analysis

> Research date: 2026-07-17
> Captured via: Reqable proxy + Chrome DevTools
> Goal: Determine if HTTP-direct automation is possible without Playwright

---

## Discovered Credentials (XOR-decoded)

```
client_id:     626581754197-v82pavblj7tgk6ap9ouqbi9lv821l6qo.apps.googleusercontent.com
client_secret: <redacted; provide GOOGLE_CLIENT_SECRET locally>
```

---

## Complete Endpoint Map

### 1. OAuth2 Authorization Init
```
GET https://accounts.google.com/o/oauth2/v2/auth
?client_id=626581754197-v82pavblj7tgk6ap9ouqbi9lv821l6qo.apps.googleusercontent.com
&redirect_uri=http://127.0.0.1:61120/callback
&response_type=code
&scope=email+profile+openid
&code_challenge=<pkce-sha256>
&code_challenge_method=S256
&access_type=offline
&prompt=select_account
```

**Response:** 302 → `https://accounts.google.com/v3/signin/identifier?...`

**Key params injected by Google:**
- `dsh=S2014017131:1784299613821774` — session hash (anti-replay)
- `continue=https://accounts.google.com/signin/oauth/consent?...` — consent continuation URL with `part` token
- `rart=ANgoxcc...` — anti-bot/replay token
- `flowName=GeneralOAuthFlow`
- `service=lso`

---

### 2. Browser Info Fingerprinting
```
POST https://accounts.google.com/v3/signin/_/AccountsSignInUi/browserinfo
?f.sid=-8464657048124984282
&bl=boq_identityfrontendauthuiserver_20260707.00_p0
&hl=pt-BR
&dsh=S2014017131:1784299613821774
&_reqid=142417
&rt=j
```

**Headers:**
```
Content-Type: application/x-www-form-urlencoded;charset=UTF-8
X-Same-Domain: 1
X-Chrome-Id-Consistency-Request: version=1,client_id=77185425430.apps.googleusercontent.com,device_id=...,signin_mode=all_accounts
X-Browser-Channel: stable
X-Browser-Year: 2026
X-Browser-Validation: MiKPDMaakj8qHMX/kQ+T5/KjErg=
X-Browser-Copyright: Copyright 2026 Google LLC. All Rights Reserved.
X-Client-Data: CJXqygE=
Cookie: __Host-GAPS, OTZ, NID, HSID, SSID, APISID, SAPISID, SID, ...
```

**Body:** `f.req=[9,1,1,[null,1050,1680],[null,887,809],[1,1,null,1],[1,2,1]]&at=APv38trAHJANaUkFjl7TLwZ3fx-s:1784299614075`

**Response:** `)]}'\n\n[[["f.mt"],["di",34],["af.httprm",34,"9216515754328806275",64],["e",4,null,null,91]]]`

> **Note:** This endpoint sends browser fingerprinting data. The `f.req` array contains viewport dimensions and feature flags. The `at` token is extracted from the initial HTML page.

---

### 3. Sign-in Identifier Page (HTML)
```
GET https://accounts.google.com/v3/signin/identifier
?dsh=...
&continue=...
&flowName=GeneralOAuthFlow
```

Returns HTML with embedded JavaScript that renders the login form. Contains hidden fields:
- `<input name="identifiertoken" value="...">`
- `<input name="identifier" value="...">`
- `<input name="identifiertoken_audio" value="...">`
- `data-initial-sign-in-data="..."` (JSON with challenge tokens)

**Critical:** The sign-in page requires JavaScript execution. Without JS, the form tokens cannot be generated.

---

### 4. OAuth Consent (after successful sign-in)
```
GET https://accounts.google.com/signin/oauth/v3/consent
?authuser=0
&part=AJi8hAMXWLTXBN-ix60Dv882lrcwaSYXiHn_mah8dnmpUgDjaVqovVoaqBJboX53SEUSYX3Nf27RZzl11QApa-ACVD7xg9oFmhwwODJVYGLI-w06hLaqdttzIrpvi1f6LWCuZl86VcZT7t8UMFQC1gfUjFIhnJi5m0-yojuLEfdYOLMMMiOZnv50_iL16-5ke17s5HwhVx09fMd-CdHxUzu-1SjReqgDxR0JVbABuuXXx-AMTaDwoK_QD-SCaPqH9ePUhlF34m-WKiw_HyUJqYnCFal9MOdYaXvIaIV5QZf9VHsUadrYXzWTUeHgv28Y-4th0BzO1eCba3-TR-hVzuRZbt41asJL0AdEkv7oT3vjz1IljCk3eRJNXnJ_W9JBlobcZyGdh7zk7u6Ga4_L7x8Pp8u2Sv3GzyuPAx2UHvBZpyDGXnZPAbk8NGrSiIifP3K8GiwlKlHlA1znQ-FgaTheC6NVioL1DQ
&flowName=GeneralOAuthFlow
&as=S1981284399:1784299652159728
&client_id=626581754197-v82pavblj7tgk6ap9ouqbi9lv821l6qo.apps.googleusercontent.com
&requestPath=/signin/oauth/v3/consent
&pli=1
&rapt=AEjHL4OiAyiyCH6PLl7IDwni9QDTUbwb8EWhKSg-HQh8ITyriI9UrCaxN7pNSHuJ1QG0iPzyrI9FY0tn-Xr4MF_wA1bVvQvh0Qq9Vf4Q5EVRBSHmRX7SOvA
```

**Response:** 302 → `http://127.0.0.1:61120/callback?iss=https://accounts.google.com&code=4/0AXEQxI...&scope=email+profile+...&authuser=0&prompt=none`

> **Key observation:** The `part` and `rapt` tokens are one-time use and tied to the session. They are generated during the sign-in flow and cannot be reused or pre-computed.

---

### 5. Authorization Code Exchange
```
POST https://oauth2.googleapis.com/token
Content-Type: application/x-www-form-urlencoded

grant_type=authorization_code
&code=4/0AXEQxI...
&client_id=626581754197-v82pavblj7tgk6ap9ouqbi9lv821l6qo.apps.googleusercontent.com
&client_secret=<GOOGLE_CLIENT_SECRET>
&redirect_uri=http://127.0.0.1:61120/callback
&code_verifier=<pkce_verifier>
```

**Response:**
```json
{
  "access_token": "ya29...",
  "id_token": "eyJ...",
  "refresh_token": "1//...",
  "expires_in": 3599,
  "scope": "email profile openid https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/userinfo.email",
  "token_type": "Bearer"
}
```

---

### 6. Google Refresh Token (no browser needed!)
```
POST https://oauth2.googleapis.com/token
Content-Type: application/x-www-form-urlencoded

grant_type=refresh_token
&refresh_token=1//...
&client_id=626581754197-v82pavblj7tgk6ap9ouqbi9lv821l6qo.apps.googleusercontent.com
&client_secret=<GOOGLE_CLIENT_SECRET>
```

**Response:**
```json
{
  "access_token": "ya29...",
  "id_token": "eyJ...",
  "expires_in": 3599
}
```

> **Note:** `refresh_token` is only returned on the FIRST authorization_code exchange. If already used, it won't be in the response.

---

### 7. Kimi Login (Google ID Token → Kimi Session)
```
POST https://www.kimi.com/api/auth/login/google
Content-Type: application/json
Origin: https://www.kimi.com
Referer: https://www.kimi.com/
User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36
x-msh-platform: windows
x-msh-version: 3.1.0

{"code": "<google id_token>"}
```

**Response:**
```json
{
  "access_token": "eyJ... (Kimi JWT)",
  "refresh_token": "eyJ...",
  "user": {
    "id": "...",
    "email": "...",
    "name": "..."
  }
}
```

---

## HTTP-Direct Feasibility Assessment

### What CAN be done via HTTP direct
| Step | Endpoint | HTTP Direct? | Notes |
|------|----------|-------------|-------|
| Code exchange | `oauth2.googleapis.com/token` | **YES** | Standard OAuth2, no browser needed |
| Refresh token | `oauth2.googleapis.com/token` | **YES** | Best option for VM/automation |
| Kimi login | `www.kimi.com/api/auth/login/google` | **YES** | Simple POST with JSON |
| Kimi refresh | `www.kimi.com/api/auth/token/refresh` | **YES** | Already implemented |

### What CANNOT be done via HTTP direct
| Step | Endpoint | HTTP Direct? | Notes |
|------|----------|-------------|-------|
| Google sign-in | `accounts.google.com/v3/signin` | **NO** | Requires JS execution, CSRF tokens, CAPTCHA |
| Consent approval | `accounts.google.com/signin/oauth/v3/consent` | **NO** | Requires authenticated session cookies |
| Browser info | `AccountsSignInUi/browserinfo` | **NO** | Fingerprinting + session binding |

### Blocking factors for full HTTP-direct
1. **Dynamic tokens**: `dsh`, `part`, `rapt`, `at` are session-bound and rotated
2. **JavaScript challenges**: Google serves obfuscated JS that must execute to proceed
3. **Cookie requirements**: `SID`, `HSID`, `SSID`, `APISID`, `SAPISID`, `NID`, `OTZ`, `__Host-GAPS` are mandatory and have complex lifecycles
4. **CAPTCHA**: Triggers after suspicious patterns (missing headers, wrong fingerprint, rapid requests)
5. **HSTS + CSP**: Strict security policies that a naive HTTP client can't satisfy
6. **Device fingerprinting**: `X-Browser-Validation`, `X-Client-Data`, viewport metrics in `f.req`

---

## Chrome Internal APIs (captured from browser)

These are Google Chrome's internal APIs, not usable for external automation:

```
POST https://oauthaccountmanager.googleapis.com/v1/issuetoken
Authorization: BoundOAuth ...
X-OAuth-Client-Id: 77185425430.apps.googleusercontent.com
```

Used by Chrome for sync, cryptauth, and kid.family.readonly scopes. Requires Chrome's internal `BoundOAuth` credential which is bound to the browser profile and OS keychain. **Not reproducible externally.**

---

## Recommended VM Strategy

```
┌─────────────────┐     ┌──────────────────┐     ┌─────────────────┐
│  Initial Setup  │────▶│  Store Tokens    │────▶│  Runtime (VM)   │
│  (Playwright    │     │  (refresh_token  │     │  (HTTP direct)  │
│   once)         │     │  + kimi_refresh) │     │                 │
└─────────────────┘     └──────────────────┘     └─────────────────┘
```

1. **One-time setup** (with Playwright/browser): Get Google `refresh_token` + Kimi `refresh_token`
2. **Persist tokens** in encrypted storage
3. **Runtime on VM**:
   - Use Google `refresh_token` → get fresh `id_token`
   - Use `id_token` → POST to Kimi `/api/auth/login/google` → get Kimi tokens
   - Use Kimi `refresh_token` → `GET /api/auth/token/refresh` → extend session

**Limitations:**
- Google refresh tokens for "Testing" apps expire after ~7 days of inactivity
- Kimi refresh tokens may also expire (need to test)
- When Google refresh expires, need browser re-auth (or manual re-login)

---

## Cookie Inventory (from captured traffic)

Required for Google sign-in session (all `.google.com` domain):
```
SID        — session ID
HSID       — secure session
SSID       — secure session
APISID     — API session
SAPISID    — secure API session
NID        — preferences/anti-bot
OTZ        — OAuth token state
__Host-GAPS — Google Account Protection
__Secure-1PSID / __Secure-3PSID — secure session variants
SIDCC / __Secure-1PSIDCC / __Secure-3PSIDCC — consent/cookie consent
ACCOUNT_CHOOSER — account selection state
LSID       — login session (scoped to services)
```

---

## Test Files

- `test_refresh_token.go` — Tests Google refresh token flow
- `test_kimi_login.go` — Tests Kimi login with Google id_token
- `test_direct_oauth.go` — Tests full HTTP-direct attempts (expected to fail at sign-in)
