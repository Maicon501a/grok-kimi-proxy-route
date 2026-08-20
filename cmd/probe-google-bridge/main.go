// probe-google-bridge: full Google OAuth login using a real headful Chrome as
// the TRANSPORT (login_bridge.mjs over stdio JSON-lines) while Go owns all
// flow logic: payload templates, parsing, PKCE, token exchange.
//
// Rationale (RE findings 2026-08-20): Google's botguard token is only accepted
// when minted inside the exact live page-load instance, and the RPC channel's
// TLS/HTTP2 fingerprint must match a real browser. So the browser loads pages
// and carries the RPCs via in-page fetch(); Go does everything else.
package main

import (
	"bufio"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"

	"grok-desktop/internal/kimi"
	"github.com/bogdanfinn/tls-client/profiles"
)

const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	batchBase      = "https://accounts.google.com/v3/signin/_/AccountsSignInUi/data/batchexecute"
	oauthUIBatch   = "https://accounts.google.com/_/OAuthUi/data/batchexecute"
)

var verbose bool

func log(f string, a ...any) { fmt.Printf("[+] "+f+"\n", a...) }
func dbg(f string, a ...any) {
	if verbose {
		fmt.Printf("    .. "+f+"\n", a...)
	}
}
func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "[!] "+f+"\n", a...)
	os.Exit(1)
}

// ── bridge client ──────────────────────────────────────────────────────────

type bridge struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	seq    int64
}

type bridgeResp struct {
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
	Body        string          `json:"body"`
	RespHeaders map[string]string `json:"respHeaders"`
	// navCapture
	Hops json.RawMessage `json:"hops"`
	// submitEmail / getURL
	URL      string `json:"url"`
	HasPwdBg bool   `json:"hasPwdBg"`
	// getPwdProgram
	Found   bool   `json:"found"`
	VMCode  string `json:"vmCode"`
	Program string `json:"program"`
}

func newBridge() *bridge {
	c := exec.Command("node", "tmp_probe/login_bridge.mjs")
	c.Stderr = os.Stderr
	stdin, err := c.StdinPipe()
	if err != nil {
		fatal("bridge stdin: %v", err)
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		fatal("bridge stdout: %v", err)
	}
	if err := c.Start(); err != nil {
		fatal("bridge start: %v", err)
	}
	return &bridge{cmd: c, stdin: stdin, reader: bufio.NewReader(stdout)}
}

func (b *bridge) call(payload map[string]any) *bridgeResp {
	id := atomic.AddInt64(&b.seq, 1)
	payload["id"] = id
	data, _ := json.Marshal(payload)
	if _, err := b.stdin.Write(append(data, '\n')); err != nil {
		fatal("bridge write: %v", err)
	}
	for {
		line, err := b.reader.ReadBytes('\n')
		if err != nil {
			fatal("bridge read: %v", err)
		}
		var r bridgeResp
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		if r.ID != id {
			continue
		}
		if !r.OK {
			fatal("bridge %v failed: %s", payload["cmd"], r.Error)
		}
		return &r
	}
}

func (b *bridge) load(rawurl string) *bridgeResp {
	return b.call(map[string]any{"cmd": "load", "url": rawurl})
}

func (b *bridge) mint(binding string) string {
	r := b.call(map[string]any{"cmd": "mint", "binding": json.RawMessage(binding)})
	return r.Token
}

func (b *bridge) mintWith(vmCode, program, binding string) string {
	r := b.call(map[string]any{"cmd": "mintWith", "vmCode": vmCode, "program": program, "binding": json.RawMessage(binding)})
	return r.Token
}

func (b *bridge) pushState(rawurl string) {
	b.call(map[string]any{"cmd": "pushState", "url": rawurl})
}

// setSessionKey writes sessionStorage.sessionPrivateKey = privRaw||pubRaw
// (Latin-1) exactly like the page's own JS does before MI613e — the botguard
// VM's snapshot reads it, so the key must exist BEFORE minting the token.
func (b *bridge) setSessionKey(priv, pub []byte) {
	hexStr := make([]byte, 0, (len(priv)+len(pub))*2)
	const hexdig = "0123456789abcdef"
	for _, by := range append(append([]byte{}, priv...), pub...) {
		hexStr = append(hexStr, hexdig[by>>4], hexdig[by&15])
	}
	b.call(map[string]any{"cmd": "setSessionKey", "hex": string(hexStr)})
}

// fillEmail types the email into the identifier field so the bg snapshot sees
// the same DOM state as a real submission.
func (b *bridge) fillEmail(email string) {
	b.call(map[string]any{"cmd": "fillEmail", "email": email})
}

// submitEmail lets the PAGE do its own MI613e: fill email + Enter, waits for
// the real password DOM. Returns the transitioned URL (has TL).
func (b *bridge) submitEmail(email string) *bridgeResp {
	return b.call(map[string]any{"cmd": "submitEmail", "email": email})
}

// fillPassword types the password into the real pwd field (bg snapshot reads it).
func (b *bridge) fillPassword(password string) {
	b.call(map[string]any{"cmd": "fillPassword", "password": password})
}

// getPwdProgram returns the pwd-page bg vmCode+program captured from the
// page's own WZfWSd response.
func (b *bridge) getPwdProgram() *bridgeResp {
	return b.call(map[string]any{"cmd": "getPwdProgram"})
}

// fetch sends an RPC through the browser page's own fetch() — byte-identical
// TLS/H2/headers to the real flow.
func (b *bridge) fetch(rawurl, body string, headers map[string]string) *bridgeResp {
	return b.call(map[string]any{
		"cmd": "fetch", "method": "POST", "url": rawurl, "body": body, "headers": headers,
	})
}

// fetchGet does an in-page GET with redirect:'manual' (no navigation).
func (b *bridge) fetchGet(rawurl string) *bridgeResp {
	return b.call(map[string]any{"cmd": "fetch", "method": "GET", "url": rawurl})
}

// navHop is one recorded main-frame response during a navigation.
type navHop struct {
	URL      string `json:"url"`
	Status   int    `json:"status"`
	Location string `json:"location"`
}

// followNav navigates to startURL in the browser while recording every
// main-frame response hop; returns the loopback code when a hop's Location
// (or URL) points at 127.0.0.1. The final TCP failure to 127.0.0.1 is
// expected and swallowed by the bridge.
func (b *bridge) followNav(startURL string) (code string) {
	r := b.call(map[string]any{"cmd": "navCapture", "url": startURL})
	var hops []navHop
	if err := json.Unmarshal(r.Hops, &hops); err != nil {
		return ""
	}
	for _, h := range hops {
		dbg("followNav hop: HTTP %d %s → %s", h.Status, truncURL(h.URL), truncURL(h.Location))
		for _, cand := range []string{h.Location, h.URL} {
			if strings.HasPrefix(cand, "http://127.0.0.1") {
				lu, _ := url.Parse(cand)
				if c := lu.Query().Get("code"); c != "" {
					return c
				}
			}
		}
	}
	return ""
}

// batchExecute posts one RPC to AccountsSignInUi batchexecute via the browser.
func (b *bridge) batchExecute(rpcid, sourcePath, fSid, bl, dsh, tl, innerJSON, at string, reqid int) (string, error) {
	q := url.Values{
		"rpcids":      {rpcid},
		"source-path": {sourcePath},
		"f.sid":       {fSid},
		"bl":          {bl},
		"hl":          {"pt-BR"},
		"_reqid":      {fmt.Sprintf("%d", reqid)},
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
	if verbose {
		os.WriteFile("tmp_probe/sent_"+rpcid+".json", []byte(innerJSON), 0o600)
	}
	form := url.Values{"f.req": {freq}, "at": {at}}
	r := b.fetch(batchBase+"?"+q.Encode(), form.Encode(), map[string]string{
		"Content-Type":              "application/x-www-form-urlencoded;charset=UTF-8",
		"X-Same-Domain":             "1",
		"x-goog-ext-278367001-jspb": `["GeneralOAuthFlow"]`,
		"x-goog-ext-391502476-jspb": fmt.Sprintf(`["%s","lso"]`, dsh),
	})
	dbg("batch %s → HTTP %d (%d bytes)", rpcid, r.Status, len(r.Body))
	if len(r.Body) < 400 {
		fmt.Printf("    .. raw %s: %s\n", rpcid, r.Body)
	}
	return extractWrbFr(r.Body, rpcid)
}

// batchExecuteMulti posts a raw multi-RPC f.req via the browser.
func (b *bridge) batchExecuteMulti(rpcids, sourcePath, fSid, bl, dsh, tl, rawFreq, at string, reqid int) (string, error) {
	q := url.Values{
		"rpcids":      {rpcids},
		"source-path": {sourcePath},
		"f.sid":       {fSid},
		"bl":          {bl},
		"hl":          {"pt-BR"},
		"_reqid":      {fmt.Sprintf("%d", reqid)},
		"rt":          {"c"},
	}
	if dsh != "" {
		q.Set("dsh", dsh)
	}
	if tl != "" {
		q.Set("TL", tl)
	}
	form := url.Values{"f.req": {rawFreq}, "at": {at}}
	r := b.fetch(batchBase+"?"+q.Encode(), form.Encode(), map[string]string{
		"Content-Type":              "application/x-www-form-urlencoded;charset=UTF-8",
		"X-Same-Domain":             "1",
		"x-goog-ext-278367001-jspb": `["GeneralOAuthFlow"]`,
		"x-goog-ext-391502476-jspb": fmt.Sprintf(`["%s","lso"]`, dsh),
	})
	dbg("batch multi %s → HTTP %d (%d bytes)", rpcids, r.Status, len(r.Body))
	return r.Body, nil
}

// ── helpers (ported from probe-google-http) ────────────────────────────────

func extractWrbFr(body, rpcid string) (string, error) {
	body = strings.TrimPrefix(body, ")]}'" )
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
	return "", fmt.Errorf("wrb.fr %s not found (body %d bytes, head %.200s)", rpcid, len(body), body)
}

func walkStrings(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, e := range t {
			walkStrings(e, out)
		}
	case map[string]any:
		for _, e := range t {
			walkStrings(e, out)
		}
	}
}

// findPair locates ["KEY","value"] pairs in a decoded payload.
func findPair(v any, key string) string {
	if a, ok := v.([]any); ok {
		if len(a) == 2 {
			if k, ok := a[0].(string); ok && k == key {
				if s, ok := a[1].(string); ok {
					return s
				}
			}
		}
		for _, e := range a {
			if r := findPair(e, key); r != "" {
				return r
			}
		}
	}
	return ""
}

func wizVal(html, key string) string {
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

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func localPart(email string) string {
	if i := strings.Index(email, "@"); i > 0 {
		return email[:i]
	}
	return email
}

func pkceGen() (string, string) {
	b := make([]byte, 32)
	rand.Read(b)
	v := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(v))
	return v, base64.RawURLEncoding.EncodeToString(sum[:])
}

func mustRead(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		fatal("read %s: %v", p, err)
	}
	return string(b)
}

func truncURL(u string) string {
	if len(u) > 110 {
		return u[:110] + "…"
	}
	return u
}

func truncStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

const oauthXorKey = byte(0x5A)

func xorDecode(enc []byte) string {
	out := make([]byte, len(enc))
	for i, b := range enc {
		out[i] = b ^ oauthXorKey
	}
	return string(out)
}

func googleOAuthCreds() (string, string) {
	clientID := xorDecode([]byte{
		0x6c, 0x68, 0x6c, 0x6f, 0x62, 0x6b, 0x6d, 0x6f, 0x6e, 0x6b, 0x63, 0x6d, 0x77,
		0x2c, 0x62, 0x68, 0x2a, 0x3b, 0x2c, 0x38, 0x36, 0x30, 0x6d, 0x2e, 0x3d, 0x31,
		0x6c, 0x3b, 0x2a, 0x63, 0x35, 0x2f, 0x2b, 0x38, 0x33, 0x63, 0x36, 0x2c, 0x62,
		0x68, 0x6b, 0x36, 0x6c, 0x2b, 0x35, 0x74, 0x3b, 0x2a, 0x2a, 0x29, 0x74, 0x3d,
		0x35, 0x35, 0x3d, 0x36, 0x3f, 0x2f, 0x29, 0x3f, 0x28, 0x39, 0x35, 0x34, 0x2e,
		0x3f, 0x34, 0x2e, 0x74, 0x39, 0x35, 0x37,
	})
	clientSecret := xorDecode([]byte{
		0x1d, 0x15, 0x19, 0x09, 0x0a, 0x02, 0x77, 0x11, 0x18, 0x3c, 0x00, 0x6c, 0x0f,
		0x03, 0x6a, 0x19, 0x6e, 0x3f, 0x18, 0x0c, 0x34, 0x05, 0x6c, 0x00, 0x69, 0x1f,
		0x6f, 0x6a, 0x28, 0x6a, 0x0a, 0x6e, 0x38, 0x6d, 0x36,
	})
	return clientID, clientSecret
}

// ── main flow ──────────────────────────────────────────────────────────────

func main() {
	email := flag.String("email", "", "google account email")
	password := flag.String("password", "", "google account password")
	mode := flag.String("mode", "page", "page = real UI transition to pwd page (default); fetch = Go-driven MI613e/WZfWSd")
	flag.BoolVar(&verbose, "v", false, "verbose")
	flag.Parse()
	if *email == "" || *password == "" {
		fatal("need -email and -password")
	}

	clientID, clientSecret := googleOAuthCreds()
	verifier, challenge := pkceGen()
	redirectURI := "http://127.0.0.1:61120/callback"

	br := newBridge()
	defer br.cmd.Process.Kill()
	br.call(map[string]any{"cmd": "start"})
	log("bridge up (real Chrome)")

	// ── Step 1+2: load auth URL in browser → identifier page ────────────
	authURL := googleAuthURL + "?" + url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {"email profile openid"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"include_granted_scopes": {"true"},
	}.Encode()
	r := br.load(authURL)
	if !strings.Contains(r.FinalURL, "/v3/signin/identifier") {
		fatal("unexpected page after auth URL: %s", truncURL(r.FinalURL))
	}
	html := r.HTML
	at := wizVal(html, "SNlM0e")
	fSid := wizVal(html, "FdrFJe")
	bl := wizVal(html, "cfb2h")
	iu, _ := url.Parse(r.FinalURL)
	dsh := iu.Query().Get("dsh")
	rart := iu.Query().Get("rart")
	cont := iu.Query().Get("continue")
	if at == "" || fSid == "" || bl == "" {
		fatal("WIZ data missing: at=%q f.sid=%q bl=%q", at, fSid, bl)
	}
	log("step1-2 ok: identifier page (f.sid=%s bl=%s)", fSid, bl)

	var payload string
	var tmp any
	var strs []string
	var tl, pwdToken string
	var err error
	urlOverrides := map[string]string{} // fresh values from the live pwd page URL
	cidInt := 0

	if *mode == "page" {
		// ── Step 2.5+3: real page transition (fill email + Enter) ────────
		// The page sends its own MI613e + WZfWSd and renders the TRUE pwd
		// DOM. The pwd bg snapshot reads that DOM (like fillEmail at MI613e),
		// so the B4hajb token must be minted against the real password page,
		// not a pushState'd identifier DOM.
		se := br.submitEmail(*email)
		log("step3 ok: pwd page (%s)", truncURL(se.URL))
		pu, _ := url.Parse(se.URL)
		tl = pu.Query().Get("TL")
		if v := pu.Query().Get("dsh"); v != "" {
			dsh = v
		}
		if tl == "" {
			fatal("no TL in pwd page URL: %s", truncURL(se.URL))
		}
		// B4hajb's [6][0] is the sorted query-pair list of THIS page URL and
		// [1] is its cid — capture the raw pairs now (decoded once).
		for _, kv := range strings.Split(pu.RawQuery, "&") {
			k, v, _ := strings.Cut(kv, "=")
			dk, _ := url.QueryUnescape(k)
			dv, _ := url.QueryUnescape(v)
			urlOverrides[dk] = dv
		}
		if c, cerr := strconv.Atoi(urlOverrides["cid"]); cerr == nil {
			cidInt = c
		}
		dbg("pwd URL overrides: cid=%d checkConnection=%s keys=%d", cidInt, urlOverrides["checkConnection"], len(urlOverrides))
		pg := br.getPwdProgram()
		if !pg.Found {
			fatal("pwd bg program not captured from the page's WZfWSd")
		}
		br.fillPassword(*password)
		pwdToken = br.mintWith(pg.VMCode, pg.Program, fmt.Sprintf(`{"Ko":{"replayKey":"%s"}}`, localPart(*email)))
		log("step3.5 ok: password token (%d chars)", len(pwdToken))
	} else {
		// ── Step 2.5: ECDH keypair → sessionStorage → mint identifier token ──
		// The page's JS stores sessionPrivateKey=priv||pub in sessionStorage BEFORE
		// MI613e and the botguard VM's snapshot reads it — so the key must be in
		// place before we mint, and the request's trailing field must be THIS pubkey.
		ecdhKey, err := ecdh.P256().GenerateKey(rand.Reader)
		if err != nil {
			fatal("ecdh: %v", err)
		}
		pubBytes := ecdhKey.PublicKey().Bytes()
		privBytes := ecdhKey.Bytes()
		tailKey := base64.StdEncoding.EncodeToString(pubBytes)
		br.setSessionKey(privBytes, pubBytes)
		br.fillEmail(*email)
		idToken := br.mint(fmt.Sprintf(`{"Ko":{"replayKey":"%s"}}`, localPart(*email)))
		log("step2.5 ok: identifier token (%d chars)", len(idToken))

		// ── Step 3: MI613e ──────────────────────────────────────────────────
		tpl := mustRead("tmp_probe/templates/MI613e.tpl.json")
		repl := strings.NewReplacer(
			"@@CONTINUE@@", cont,
			"@@RART@@", rart,
			"@@DSH@@", dsh,
			"@@CHALLENGE@@", challenge,
			"@@TL@@", "",
			"@@EMAIL@@", *email,
			"@@YT@@", "youtube:320",
			"@@TAILHASH@@", tailKey,
		)
		miInner := repl.Replace(tpl)
		idTokRe := regexp.MustCompile(`\["identity-signin-identifier","[^"]*"\]`)
		miInner = idTokRe.ReplaceAllString(miInner, `["identity-signin-identifier",`+strconvQuote(idToken)+`]`)
		if err := json.Unmarshal([]byte(miInner), &tmp); err != nil {
			fatal("MI613e template invalid: %v", err)
		}
		payload, err = br.batchExecute("MI613e", "/v3/signin/identifier", fSid, bl, dsh, "", miInner, at, 10000+randInt(90000))
		if err != nil {
			fatal("MI613e: %v", err)
		}
		var miResp any
		if err := json.Unmarshal([]byte(payload), &miResp); err != nil {
			fatal("MI613e payload not json: %.300s", payload)
		}
		walkStrings(miResp, &strs)
		tl = findPair(miResp, "TL")
		pwdPath := ""
		for _, s := range strs {
			if strings.Contains(s, "/v3/signin/challenge/pwd") {
				pwdPath = s
				break
			}
		}
		if tl == "" || pwdPath == "" {
			fmt.Printf("[!] MI613e rejected. payload head:\n%.600s\n", payload)
			fatal("identifier rejected")
		}
		log("step3 ok: TL=%s… pwdPath=%s", truncStr(tl, 12), truncURL(pwdPath))

		// ── Step 3.5: WZfWSd multi-RPC → password bfkj program ──────────────
		wzTpl := mustRead("tmp_probe/templates/WZfWSd_multi.tpl.json")
		wzFreq := strings.NewReplacer("@@CLIENTID@@", clientID).Replace(wzTpl)
		wzBody, err := br.batchExecuteMulti(
			"WZfWSd,etGTrd,Aho3hb,i3kFoc,zKAP2e,RzSO2e",
			"/v3/signin/challenge/pwd", fSid, bl, dsh, tl, wzFreq, at, 20000+randInt(90000))
		if err != nil {
			fatal("WZfWSd: %v", err)
		}
		zkPayload, err := extractWrbFr(wzBody, "zKAP2e")
		if err != nil {
			fatal("WZfWSd zKAP2e: %v", err)
		}
		var zk any
		if err := json.Unmarshal([]byte(zkPayload), &zk); err != nil {
			fatal("zKAP2e payload not json: %v", err)
		}
		vmCode := ""
		if a, ok := zk.([]any); ok && len(a) > 4 {
			if b, ok := a[4].([]any); ok && len(b) > 1 {
				if c, ok := b[1].([]any); ok && len(c) > 5 {
					vmCode, _ = c[5].(string)
				}
			}
		}
		// Go's regexp caps repetition at 1000 — scan quoted strings manually for
		// the long base64 program.
		prog := ""
		for _, part := range strings.Split(zkPayload, `"`) {
			if len(part) < 10000 {
				continue
			}
			ok := true
			for _, c := range part {
				if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=') {
					ok = false
					break
				}
			}
			if ok && len(part) > len(prog) {
				prog = part
			}
		}
		pm := []string{"", prog}
		if vmCode == "" || pm[1] == "" {
			fatal("zKAP2e: vmCode/program not found (vmCode=%d prog=%d)", len(vmCode), len(pm[1]))
		}
		// the real flow transitions SPA-style: URL becomes the pwd challenge page
		pwdURL := pwdPath
		if strings.HasPrefix(pwdURL, "/") {
			pwdURL = "https://accounts.google.com" + pwdURL
		}
		br.pushState(pwdURL)
		pwdToken = br.mintWith(vmCode, pm[1], fmt.Sprintf(`{"Ko":{"replayKey":"%s"}}`, localPart(*email)))
		log("step3.5 ok: password token (%d chars)", len(pwdToken))

		// browserinfo beacon on the pwd page (trace4 #27: after WZfWSd, before B4hajb)
		biQ := url.Values{
			"f.sid": {fSid}, "bl": {bl}, "hl": {"pt-BR"},
			"TL": {tl}, "_reqid": {fmt.Sprintf("%d", 25000 + randInt(90000))}, "rt": {"j"},
		}
		if dsh != "" {
			biQ.Set("dsh", dsh)
		}
		biBody := url.Values{
			"f.req": {`[9,1,1,[null,800,1280],[null,800,1280],[1,1,null,1],[0,2,2]]`},
			"at":    {at},
		}.Encode()
		br.fetch("https://accounts.google.com/v3/signin/_/AccountsSignInUi/browserinfo?"+biQ.Encode(), biBody, map[string]string{
			"Content-Type":  "application/x-www-form-urlencoded;charset=UTF-8",
			"X-Same-Domain": "1",
		})
		dbg("browserinfo enviado")
	}

	// ── Step 4: B4hajb password ─────────────────────────────────────────
	tpl := mustRead("tmp_probe/templates/B4hajb.tpl.json")
	repl := strings.NewReplacer(
		"@@CONTINUE@@", cont,
		"@@RART@@", rart,
		"@@DSH@@", dsh,
		"@@CHALLENGE@@", challenge,
		"@@TL@@", tl,
		"@@EMAIL@@", *email,
		"@@PASSWORD@@", *password,
		"@@YT@@", "youtube:320",
	)
	b4Inner := repl.Replace(tpl)
	pwdTokRe := regexp.MustCompile(`\["identity-signin-password","[^"]*"\]`)
	b4Inner = pwdTokRe.ReplaceAllString(b4Inner, `["identity-signin-password",`+strconvQuote(pwdToken)+`]`)
	if err := json.Unmarshal([]byte(b4Inner), &tmp); err != nil {
		fatal("B4hajb template invalid: %v", err)
	}
	// JSON surgery: the query-pair list [6][0] and cid [1] must reflect the
	// LIVE pwd page URL (checkConnection=youtube:NNN, cid=N change per run).
	if len(urlOverrides) > 0 {
		var b4Arr []any
		if err := json.Unmarshal([]byte(b4Inner), &b4Arr); err != nil {
			fatal("B4hajb parse for surgery: %v", err)
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
	}
	payload, err = br.batchExecute("B4hajb", "/v3/signin/challenge/pwd", fSid, bl, dsh, tl, b4Inner, at, 30000+randInt(90000))
	if err != nil {
		fatal("B4hajb: %v", err)
	}
	var b4Resp any
	if err := json.Unmarshal([]byte(payload), &b4Resp); err != nil {
		r2 := br.fetch(batchBase+"?debug=1", "", map[string]string{"X-Same-Domain": "1"})
		_ = r2
		fatal("B4hajb payload not json: %.400s", payload)
	}
	strs = nil
	walkStrings(b4Resp, &strs)
	checkCookieURL := ""
	for _, s := range strs {
		if strings.Contains(s, "CheckCookie") {
			checkCookieURL = s
			break
		}
	}
	if checkCookieURL == "" {
		fmt.Printf("[!] B4hajb rejected. payload head:\n%.800s\n", payload)
		fatal("password rejected")
	}
	if strings.HasPrefix(checkCookieURL, "/") {
		checkCookieURL = "https://accounts.google.com" + checkCookieURL
	}
	log("step4 ok: CheckCookie")

	// ── Step 5: CheckCookie chain (browser navigates) ───────────────────
	r = br.load(checkCookieURL)
	if strings.Contains(r.FinalURL, "CheckCookie") {
		// JS/meta interstitial — wait for the URL to move on
		r2 := br.call(map[string]any{"cmd": "waitLeave", "fragment": "CheckCookie", "ms": 20000})
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
	if code != "" {
		// already-consented account: consent was auto-approved and the whole
		// chain ran straight to the loopback redirect.
		log("step5-7 ok: consent auto-aprovado → authorization code!")
	}
	if code == "" {
		// Primary: let the page click its own approve button (robust against
		// consent-page variants that don't embed the !ChR token in the HTML).
		cc := br.call(map[string]any{"cmd": "clickConsent"})
		if cc.Code != "" {
			code = cc.Code
			log("step6-7 ok: consent aprovado pela página → authorization code!")
		}
	}
	if code == "" {
		consentHTML := r.HTML
		at2 := wizVal(consentHTML, "SNlM0e")
		fSid2 := wizVal(consentHTML, "FdrFJe")
		bl2 := wizVal(consentHTML, "cfb2h")
		dsh2 := wizVal(consentHTML, "Qzxixc")
		tokRe := regexp.MustCompile(`!(ChR[A-Za-z0-9_+/=\-\x{2219}]+)`)
		tokM := tokRe.FindStringSubmatch(consentHTML)
		if at2 == "" || len(tokM) < 2 {
			os.WriteFile("tmp_probe/checkcookie_page.html", []byte(consentHTML), 0o600)
			fmt.Printf("[!] consent chain ended at %s (html %d bytes → tmp_probe/checkcookie_page.html)\n", truncURL(r.FinalURL), len(consentHTML))
			fatal("consent page tokens missing: at2=%q", at2)
		}
		consentTok := "!" + tokM[1]
		log("step5 ok: consent page (%s)", truncURL(r.FinalURL))

		// the consent page URL carries as= / rapt= needed downstream
		cu, _ := url.Parse(r.FinalURL)
		asVal := cu.Query().Get("as")
		raptVal := cu.Query().Get("rapt")

		// ── Step 6: xyhAld consent approval (via browser fetch) ─────────────
		tpl = mustRead("tmp_probe/templates/xyhAld.tpl.json")
		repl = strings.NewReplacer(
			"@@CONSENTTOK@@", consentTok,
			"@@DSH@@", asVal,
			"@@APPDOMAIN@@", "http://127.0.0.1:61120",
			"@@CLIENTID@@", clientID,
		)
		xyInner := repl.Replace(tpl)
		if err := json.Unmarshal([]byte(xyInner), &tmp); err != nil {
			fatal("xyhAld template invalid: %v", err)
		}
		q := url.Values{
			"rpcids":       {"xyhAld"},
			"source-path":  {"/signin/oauth/id"},
			"f.sid":        {fSid2},
			"bl":           {bl2},
			"hl":           {"pt-BR"},
			"authuser":     {"0"},
			"soc-app":      {"1"},
			"soc-platform": {"1"},
			"soc-device":   {"1"},
			"_reqid":       {fmt.Sprintf("%d", 40000+randInt(90000))},
			"rt":           {"c"},
		}
		innerQuoted, _ := json.Marshal(xyInner)
		freq := fmt.Sprintf(`[[["xyhAld",%s,null,"generic"]]]`, string(innerQuoted))
		form := url.Values{"f.req": {freq}, "at": {at2}}
		xyHeaders := map[string]string{
			"Content-Type":  "application/x-www-form-urlencoded;charset=UTF-8",
			"X-Same-Domain": "1",
		}
		if dsh2 != "" {
			xyHeaders["x-goog-ext-391502476-jspb"] = fmt.Sprintf(`["%s"]`, dsh2)
		}
		xyR := br.fetch(oauthUIBatch+"?"+q.Encode(), form.Encode(), xyHeaders)
		payload, err = extractWrbFr(xyR.Body, "xyhAld")
		if err != nil {
			fmt.Printf("[!] xyhAld raw head:\n%.600s\n", xyR.Body)
			fatal("xyhAld: %v", err)
		}
		var xyResp any
		if err := json.Unmarshal([]byte(payload), &xyResp); err != nil {
			fatal("xyhAld payload not json: %.400s", payload)
		}
		strs = nil
		walkStrings(xyResp, &strs)
		// the response carries a fresh "part" token (AJi8hA…); the real page then
		// navigates to /signin/oauth/consent?as=…&part=<new>&rapt=…&xsrf=<at2>
		newPart := ""
		for _, s := range strs {
			if strings.HasPrefix(s, "AJi8hA") {
				newPart = s
				break
			}
		}
		followURL := ""
		if newPart != "" {
			followURL = "https://accounts.google.com/signin/oauth/consent?" + url.Values{
				"as":        {asVal},
				"authuser":  {"0"},
				"client_id": {clientID},
				"flowName":  {"GeneralOAuthFlow"},
				"part":      {newPart},
				"rapt":      {raptVal},
				"xsrf":      {at2},
			}.Encode()
		}
		if followURL == "" {
			for _, s := range strs {
				if strings.Contains(s, "/signin/oauth") {
					followURL = s
					break
				}
			}
		}
		if followURL == "" {
			fmt.Printf("[!] xyhAld payload head:\n%.800s\n", payload)
			fatal("no follow URL after consent approval")
		}
		if strings.HasPrefix(followURL, "/") {
			followURL = "https://accounts.google.com" + followURL
		}
		log("step6 ok: follow=%s", truncURL(followURL))

		// ── Step 7: follow to loopback code (recorded nav hops) ───────────
		code = br.followNav(followURL)
		if code == "" {
			fatal("no loopback code after follow chain")
		}
		log("step7 ok: authorization code!")
	}

	// ── Step 8: code → tokens (pure Go; no anti-bot on this endpoint) ───
	jar := tls_client.NewCookieJar()
	hc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithTimeoutSeconds(60),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithCookieJar(jar),
	)
	if err != nil {
		fatal("tls-client: %v", err)
	}
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
	resp3, err := hc.Do(req)
	if err != nil {
		fatal("token exchange: %v", err)
	}
	b3, _ := io.ReadAll(io.LimitReader(resp3.Body, 1<<20))
	resp3.Body.Close()
	if resp3.StatusCode >= 400 {
		fatal("token exchange HTTP %d: %.300s", resp3.StatusCode, string(b3))
	}
	var tok map[string]any
	json.Unmarshal(b3, &tok)
	googleIDToken, _ := tok["id_token"].(string)
	refreshTok, _ := tok["refresh_token"].(string)
	if googleIDToken == "" {
		fatal("no id_token: %.300s", string(b3))
	}
	log("step8 ok: id_token=%s… refresh=%v", truncStr(googleIDToken, 24), refreshTok != "")

	out := map[string]string{
		"id_token":      googleIDToken,
		"refresh_token": refreshTok,
	}
	ob, _ := json.MarshalIndent(out, "", "  ")
	os.WriteFile("tmp_probe/google_tokens.json", ob, 0o600)
	log("tokens salvos em tmp_probe/google_tokens.json")

	// ── Step 9: Google → Kimi (pure HTTP) ───────────────────────────────
	// LoginWithGoogleRefresh refreshes the Google id_token and trades it for
	// Kimi tokens via account.gateway.v1.AuthService/LoginWithThirdParty.
	ksess, err := kimi.LoginWithGoogleRefresh(refreshTok)
	if err != nil {
		fatal("kimi login: %v", err)
	}
	log("step9 ok: Kimi user=%s email=%s exp=%d", ksess.UserID, ksess.Email, ksess.Exp)
	kb, _ := json.MarshalIndent(ksess, "", "  ")
	os.WriteFile("tmp_probe/kimi_session.json", kb, 0o600)
	log("sessão Kimi salva em tmp_probe/kimi_session.json")
	fmt.Println("\n=== LOGIN COMPLETO E2E: Google (bridge) → Kimi (HTTP) ===")
}

func randInt(n int) int {
	var b [8]byte
	rand.Read(b[:])
	v := int(b[0]) | int(b[1])<<8 | int(b[2])<<16 | int(b[3])<<24
	if v < 0 {
		v = -v
	}
	return v % n
}
