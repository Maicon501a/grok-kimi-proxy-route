// google_bridge_login.go — Google OAuth login via a real headful Chrome used
// purely as a TRANSPORT ("bridge"), while Go owns all flow logic.
//
// RE findings (2026-08-20): Google's botguard token is only accepted when
// minted inside the exact live page-load instance, and headless Chromium is
// detected (rrk=46). So the browser (real Chrome, off-screen, no UI typing
// except field fills) loads pages and carries RPCs via in-page fetch();
// Go builds/parses every payload, does PKCE, the token exchange and the
// Kimi LoginWithThirdParty call.
//
// Flow (all steps proven in cmd/probe-google-bridge):
//  1. load OAuth auth URL → identifier page (WIZ data)
//  2. page submits the email itself (real MI613e + WZfWSd + real pwd DOM)
//  3. Go mints the password botguard token in the live pwd page
//  4. Go sends B4hajb (password) via in-page fetch — cid/checkConnection
//     must come from the LIVE pwd page URL or Google hard-rejects with [3]
//  5. CheckCookie chain → consent page (or straight to loopback code)
//  6. page clicks its own approve button (xyhAld) → loopback code
//  7. code + PKCE → Google tokens (pure HTTP)
//  8. Google id_token → Kimi tokens (pure HTTP)
//
// VM note: requires real Chrome (channel:'chrome') with a display — use Xvfb
// on headless Linux VMs. After the first login, LoginWithGoogleRefresh works
// with NO browser at all.
package kimi

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"strings"
	"sync/atomic"
	"time"
)

// ── bridge client (scripts/login_bridge.mjs over stdio JSON-lines) ─────────

type googleBridge struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	seq    int64
}

type googleBridgeResp struct {
	ID    int64  `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	// load
	FinalURL string `json:"finalURL"`
	Status   int    `json:"status"`
	HTML     string `json:"html"`
	Code     string `json:"code"`
	// mint
	Token string `json:"token"`
	// fetch
	Body string `json:"body"`
	// submitEmail / getURL
	URL      string `json:"url"`
	HasPwdBg bool   `json:"hasPwdBg"`
	// getPwdProgram
	Found   bool   `json:"found"`
	VMCode  string `json:"vmCode"`
	Program string `json:"program"`
	// navCapture / clickConsent
	Hops json.RawMessage `json:"hops"`
}

func startGoogleBridge(projectRoot string) (*googleBridge, error) {
	// The bridge script is embedded in the binary and extracted to the cache
	// dir — the flow works from a bare executable with no source tree.
	bridgeDir, err := extractEmbeddedBridge()
	if err != nil {
		return nil, err
	}
	script := filepath.Join(bridgeDir, "login_bridge.cjs")
	c := exec.Command("node", script)
	c.Dir = bridgeDir
	c.Stderr = nil // bridge logs are diagnostics only
	// playwright resolution (CJS honors NODE_PATH): repo node_modules (dev),
	// a node_modules next to the extracted script, and global npm root (VMs).
	var nodePath []string
	if projectRoot != "" {
		if st, err := os.Stat(filepath.Join(projectRoot, "node_modules")); err == nil && st.IsDir() {
			nodePath = append(nodePath, filepath.Join(projectRoot, "node_modules"))
		}
	}
	nodePath = append(nodePath, filepath.Join(bridgeDir, "node_modules"))
	if g := globalNpmRoot(); g != "" {
		nodePath = append(nodePath, g)
	}
	env := append([]string{}, os.Environ()...)
	env = append(env, "NODE_PATH="+strings.Join(nodePath, string(os.PathListSeparator)))
	c.Env = env
	stdin, err := c.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("node bridge start: %w", err)
	}
	return &googleBridge{cmd: c, stdin: stdin, reader: bufio.NewReader(stdout)}, nil
}

// globalNpmRoot returns `npm root -g` (cached) so a globally installed
// playwright (npm i -g playwright) resolves on bare VMs.
var globalNpmRoot = func() func() string {
	var once sync.Once
	var dir string
	return func() string {
		once.Do(func() {
			out, err := exec.Command("npm", "root", "-g").Output()
			if err == nil {
				dir = strings.TrimSpace(string(out))
			}
		})
		return dir
	}
}()

func (b *googleBridge) call(payload map[string]any) (*googleBridgeResp, error) {
	id := atomic.AddInt64(&b.seq, 1)
	payload["id"] = id
	data, _ := json.Marshal(payload)
	if _, err := b.stdin.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("bridge write: %w", err)
	}
	for {
		line, err := b.reader.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("bridge read: %w", err)
		}
		var r googleBridgeResp
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		if r.ID != id {
			continue
		}
		if !r.OK {
			return nil, fmt.Errorf("bridge %v: %s", payload["cmd"], r.Error)
		}
		return &r, nil
	}
}

func (b *googleBridge) close() {
	if b.cmd != nil && b.cmd.Process != nil {
		b.cmd.Process.Kill()
	}
}

// must is a tiny helper that turns (resp, err) into resp-or-fail.
func (b *googleBridge) must(payload map[string]any) *googleBridgeResp {
	r, err := b.call(payload)
	if err != nil {
		poisonSharedBridge(b)
		fatalBridge(err)
	}
	return r
}

// bridgeAbort carries a fatal bridge error out of must().
type bridgeAbort struct{ err error }

func fatalBridge(err error) { panic(bridgeAbort{err}) }

// ── shared bridge: ONE Chrome process serves every login, sequentially ─────
//
// gbShared serializes all bridge logins (in-process anti-multi-Chrome guard)
// and reuses a single node+Chrome process. Each login gets a FRESH isolated
// browser context (newSession) so Google accounts never share cookies/state.
var gbShared struct {
	mu sync.Mutex
	br *googleBridge
}

// acquireSharedBridge locks the shared bridge for exclusive use and opens a
// fresh isolated session on it. Call release() when the login is done.
func acquireSharedBridge(projectRoot string) (br *googleBridge, release func(), err error) {
	gbShared.mu.Lock()
	release = func() { gbShared.mu.Unlock() }
	if gbShared.br == nil {
		br, err = startGoogleBridge(projectRoot)
		if err != nil {
			release()
			return nil, nil, err
		}
		gbShared.br = br
	}
	br = gbShared.br
	if _, err = br.call(map[string]any{"cmd": "newSession"}); err != nil {
		// process died or is broken — restart once
		br.close()
		gbShared.br = nil
		br, err = startGoogleBridge(projectRoot)
		if err != nil {
			release()
			return nil, nil, err
		}
		gbShared.br = br
		if _, err = br.call(map[string]any{"cmd": "newSession"}); err != nil {
			poisonSharedBridge(br)
			release()
			return nil, nil, err
		}
	}
	return br, release, nil
}

// poisonSharedBridge kills the shared bridge (after a mid-flow failure or a
// timeout) so the next login starts from a clean process.
func poisonSharedBridge(br *googleBridge) {
	gbShared.mu.Lock()
	defer gbShared.mu.Unlock()
	if gbShared.br == br {
		br.close()
		gbShared.br = nil
	}
}

// CloseGoogleBridge shuts down the shared browser (app/proxy shutdown).
func CloseGoogleBridge() {
	gbShared.mu.Lock()
	defer gbShared.mu.Unlock()
	if gbShared.br != nil {
		gbShared.br.close()
		gbShared.br = nil
	}
}

// ── small helpers ──────────────────────────────────────────────────────────

func gbWizVal(html, key string) string {
	re := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `":"((?:[^"\\]|\\.)*)"`)
	m := re.FindStringSubmatch(html)
	if m == nil {
		return ""
	}
	var out string
	if err := json.Unmarshal([]byte(`"`+m[1]+`"`), &out); err != nil {
		return m[1]
	}
	return out
}

func gbWalkStrings(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, e := range t {
			gbWalkStrings(e, out)
		}
	case map[string]any:
		for _, e := range t {
			gbWalkStrings(e, out)
		}
	}
}

func gbExtractWrbFr(body, rpcid string) (string, error) {
	body = strings.TrimPrefix(body, ")]}'")
	lines := strings.Split(body, "\n")
	var chunks []string
	for i := 0; i < len(lines); i++ {
		n := 0
		if _, err := fmt.Sscanf(strings.TrimSpace(lines[i]), "%d", &n); err != nil || n <= 0 {
			continue
		}
		if i+1 < len(lines) {
			chunks = append(chunks, lines[i+1])
			i++
		}
	}
	for _, c := range chunks {
		var arr []any
		if err := json.Unmarshal([]byte(c), &arr); err != nil {
			continue
		}
		for _, item := range arr {
			a, ok := item.([]any)
			if !ok || len(a) < 3 {
				continue
			}
			if a[0] == "wrb.fr" && a[1] == rpcid {
				s, _ := a[2].(string)
				return s, nil
			}
		}
	}
	return "", fmt.Errorf("wrb.fr %s not found (%d bytes)", rpcid, len(body))
}

func gbLocalPart(email string) string {
	if i := strings.Index(email, "@"); i > 0 {
		return email[:i]
	}
	return email
}

func gbPKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	rand.Read(b)
	v := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(v))
	return v, base64.RawURLEncoding.EncodeToString(sum[:])
}

func gbRandInt(n int) int {
	var b [4]byte
	rand.Read(b[:])
	v := int(b[0]) | int(b[1])<<8 | int(b[2])<<16 | int(b[3])<<24
	if v < 0 {
		v = -v
	}
	return v % n
}

// ── main entry ─────────────────────────────────────────────────────────────

// LoginWithGoogleBridge performs a full Google OAuth login using a real
// headful Chrome as transport (scripts/login_bridge.mjs) and trades the
// resulting tokens for a Kimi session. projectRoot must contain
// scripts/login_bridge.mjs, scripts/templates/*.tpl.json and a node_modules
// with playwright + real Chrome installed.
//
// All calls share ONE Chrome process (gbShared): logins run sequentially and
// each gets a fresh isolated browser context. The timeout watchdog kills the
// shared bridge on expiry, which fails the in-flight call and aborts cleanly.
func LoginWithGoogleBridge(projectRoot, googleEmail, googlePassword string, timeout time.Duration) (sess *GoogleLoginSession, err error) {
	defer func() {
		if r := recover(); r != nil {
			if ab, ok := r.(bridgeAbort); ok {
				err = ab.err
				sess = nil
				return
			}
			panic(r)
		}
	}()
	br, release, err := acquireSharedBridge(projectRoot)
	if err != nil {
		return nil, err
	}
	defer release()
	timer := time.AfterFunc(timeout, func() { poisonSharedBridge(br) })
	defer timer.Stop()
	sess, err = loginWithGoogleBridgeInner(br, projectRoot, googleEmail, googlePassword)
	return
}

func loginWithGoogleBridgeInner(br *googleBridge, projectRoot, googleEmail, googlePassword string) (*GoogleLoginSession, error) {
	clientID, clientSecret, err := googleOAuthCreds()
	if err != nil {
		return nil, err
	}
	verifier, challenge := gbPKCE()
	redirectURI := "http://127.0.0.1:61120/callback"

	// ── Step 1: load auth URL → identifier page ─────────────────────────
	authURL := googleAuthURL + "?" + url.Values{
		"client_id":              {clientID},
		"redirect_uri":           {redirectURI},
		"response_type":          {"code"},
		"scope":                  {"email profile openid"},
		"code_challenge":         {challenge},
		"code_challenge_method":  {"S256"},
		"access_type":            {"offline"},
		"prompt":                 {"consent"},
		"include_granted_scopes": {"true"},
	}.Encode()
	r := br.must(map[string]any{"cmd": "load", "url": authURL})
	if !strings.Contains(r.FinalURL, "/v3/signin/identifier") {
		return nil, fmt.Errorf("unexpected page after auth URL: %s", gbTrunc(r.FinalURL, 110))
	}
	at := gbWizVal(r.HTML, "SNlM0e")
	fSid := gbWizVal(r.HTML, "FdrFJe")
	bl := gbWizVal(r.HTML, "cfb2h")
	iu, _ := url.Parse(r.FinalURL)
	dsh := iu.Query().Get("dsh")
	if at == "" || fSid == "" || bl == "" {
		return nil, fmt.Errorf("WIZ data missing on identifier page")
	}

	// ── Step 2: page submits the email itself → real pwd page ───────────
	se := br.must(map[string]any{"cmd": "submitEmail", "email": googleEmail})
	pu, _ := url.Parse(se.URL)
	tl := pu.Query().Get("TL")
	if v := pu.Query().Get("dsh"); v != "" {
		dsh = v
	}
	if tl == "" {
		return nil, fmt.Errorf("no TL in pwd page URL (identifier rejected?): %s", gbTrunc(se.URL, 110))
	}
	// B4hajb's query-pair list and cid must reflect the LIVE pwd page URL.
	urlOverrides := map[string]string{}
	for _, kv := range strings.Split(pu.RawQuery, "&") {
		k, v, _ := strings.Cut(kv, "=")
		dk, _ := url.QueryUnescape(k)
		dv, _ := url.QueryUnescape(v)
		urlOverrides[dk] = dv
	}
	cidInt, _ := strconv.Atoi(urlOverrides["cid"])

	pg := br.must(map[string]any{"cmd": "getPwdProgram"})
	if !pg.Found {
		return nil, fmt.Errorf("pwd botguard program not captured from the page's WZfWSd")
	}
	br.must(map[string]any{"cmd": "fillPassword", "password": googlePassword})
	pwdTok := br.must(map[string]any{
		"cmd": "mintWith", "vmCode": pg.VMCode, "program": pg.Program,
		"binding": json.RawMessage(fmt.Sprintf(`{"Ko":{"replayKey":"%s"}}`, gbLocalPart(googleEmail))),
	})

	// ── Step 3: B4hajb password (Go-driven, in-page fetch) ──────────────
	tplStr, err := bridgeTemplate("B4hajb.tpl.json")
	if err != nil {
		return nil, err
	}
	cont := iu.Query().Get("continue")
	rart := iu.Query().Get("rart")
	b4Inner := strings.NewReplacer(
		"@@CONTINUE@@", cont,
		"@@RART@@", rart,
		"@@DSH@@", dsh,
		"@@CHALLENGE@@", challenge,
		"@@TL@@", tl,
		"@@EMAIL@@", googleEmail,
		"@@PASSWORD@@", googlePassword,
		"@@YT@@", "youtube:320",
	).Replace(tplStr)
	pwdTokRe := regexp.MustCompile(`\["identity-signin-password","[^"]*"\]`)
	tokJSON, _ := json.Marshal(pwdTok.Token)
	b4Inner = pwdTokRe.ReplaceAllString(b4Inner, `["identity-signin-password",`+string(tokJSON)+`]`)
	// JSON surgery: [6][0] query pairs + [1] cid from the live pwd URL.
	var b4Arr []any
	if err := json.Unmarshal([]byte(b4Inner), &b4Arr); err != nil {
		return nil, fmt.Errorf("B4hajb template invalid: %w", err)
	}
	if cidInt > 0 && len(b4Arr) > 1 {
		b4Arr[1] = float64(cidInt)
	}
	if sec, ok := b4Arr[6].([]any); ok && len(sec) > 0 {
		if pairs, ok := sec[0].([]any); ok {
			for _, p := range pairs {
				pa, ok := p.([]any)
				if !ok || len(pa) < 2 {
					continue
				}
				k, _ := pa[0].(string)
				if v, ok2 := urlOverrides[k]; ok2 {
					pa[1] = v
				}
			}
		}
	}
	fixed, _ := json.Marshal(b4Arr)
	b4Inner = string(fixed)

	payload, err := gbBatchExecute(br, "B4hajb", "/v3/signin/challenge/pwd", fSid, bl, dsh, tl, b4Inner, at)
	if err != nil {
		return nil, err
	}
	var b4Resp any
	if err := json.Unmarshal([]byte(payload), &b4Resp); err != nil {
		return nil, fmt.Errorf("B4hajb rejected: %.200s", payload)
	}
	var strs []string
	gbWalkStrings(b4Resp, &strs)
	checkCookieURL := ""
	for _, s := range strs {
		if strings.Contains(s, "CheckCookie") {
			checkCookieURL = s
			break
		}
	}
	if checkCookieURL == "" {
		return nil, fmt.Errorf("password rejected (no CheckCookie): %.200s", payload)
	}
	if strings.HasPrefix(checkCookieURL, "/") {
		checkCookieURL = "https://accounts.google.com" + checkCookieURL
	}

	// ── Step 4: CheckCookie chain → consent / loopback code ─────────────
	r = br.must(map[string]any{"cmd": "load", "url": checkCookieURL})
	if strings.Contains(r.FinalURL, "CheckCookie") {
		r2 := br.must(map[string]any{"cmd": "waitLeave", "fragment": "CheckCookie", "ms": 20000})
		r.FinalURL = r2.URL
		r.HTML = r2.HTML
		r.Code = r2.Code
	}
	code := r.Code
	if code == "" && strings.Contains(r.FinalURL, "127.0.0.1") {
		if u2, perr := url.Parse(r.FinalURL); perr == nil {
			code = u2.Query().Get("code")
		}
	}

	// ── Step 5: consent approval → loopback code ────────────────────────
	if code == "" {
		cc := br.must(map[string]any{"cmd": "clickConsent"})
		code = cc.Code
	}
	if code == "" {
		return nil, fmt.Errorf("no authorization code after consent")
	}

	// ── Step 6: code → Google tokens (pure HTTP) ────────────────────────
	form := url.Values{
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	req, _ := http.NewRequest("POST", googleTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 45 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("token exchange HTTP %d: %.300s", resp.StatusCode, body)
	}
	var tok map[string]any
	json.Unmarshal(body, &tok)
	googleIDToken, _ := tok["id_token"].(string)
	googleRefresh, _ := tok["refresh_token"].(string)
	if googleIDToken == "" {
		return nil, fmt.Errorf("no id_token: %.300s", body)
	}

	// ── Step 7: Google id_token → Kimi tokens (pure HTTP) ───────────────
	ksess, err := exchangeGoogleIDTokenForKimi(googleIDToken)
	if err != nil {
		return nil, err
	}
	ksess.Source = "google_bridge"
	return &GoogleLoginSession{Session: *ksess, IDToken: googleIDToken, GoogleRefreshToken: googleRefresh}, nil
}

// gbBatchExecute posts one RPC to AccountsSignInUi batchexecute via the bridge.
func gbBatchExecute(br *googleBridge, rpcid, sourcePath, fSid, bl, dsh, tl, innerJSON, at string) (string, error) {
	q := url.Values{
		"rpcids":      {rpcid},
		"source-path": {sourcePath},
		"f.sid":       {fSid},
		"bl":          {bl},
		"hl":          {"pt-BR"},
		"_reqid":      {fmt.Sprintf("%d", 10000+gbRandInt(90000))},
		"rt":          {"c"},
	}
	if dsh != "" {
		q.Set("dsh", dsh)
	}
	if tl != "" {
		q.Set("TL", tl)
	}
	innerQuoted, _ := json.Marshal(innerJSON)
	freq := fmt.Sprintf(`[[["%s",%s,null,"generic"]]]`, rpcid, string(innerQuoted))
	form := url.Values{"f.req": {freq}, "at": {at}}
	r, err := br.call(map[string]any{
		"cmd": "fetch", "method": "POST",
		"url":  "https://accounts.google.com/v3/signin/_/AccountsSignInUi/data/batchexecute?" + q.Encode(),
		"body": form.Encode(),
		"headers": map[string]string{
			"Content-Type":              "application/x-www-form-urlencoded;charset=UTF-8",
			"X-Same-Domain":             "1",
			"x-goog-ext-278367001-jspb": `["GeneralOAuthFlow"]`,
			"x-goog-ext-391502476-jspb": fmt.Sprintf(`["%s","lso"]`, dsh),
		},
	})
	if err != nil {
		return "", err
	}
	return gbExtractWrbFr(r.Body, rpcid)
}

func gbTrunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
