// probe-google-http: full Google OAuth → Kimi login over pure HTTP (no browser).
// Reverse-engineered from traced v3 signin batchexecute flow (see research/google-auth-flow).
package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha1"
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
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	batchBase      = "https://accounts.google.com/v3/signin/_/AccountsSignInUi/data/batchexecute"
	oauthUIBatch   = "https://accounts.google.com/_/OAuthUi/data/batchexecute"
	// UA major MUST match the uTLS ClientHello (HelloChrome_133) — a mismatch
	// between sec-ch-ua/UA and the TLS fingerprint triggers rrk=46 rejections.
	chromeMajor = "133"
	chromeUA    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
	// Windows API key from public reverse of chrome.dll (dsekz/chrome-x-browser-validation-header)
	xbvWinAPIKey = "AIzaSyA2KlwBX3mkFo30om9LUFYQhpqLoa_BNhE"
)

// mutable UA state — session mode overrides these to match the browser that
// loaded the page (page loads and Go RPCs must tell one coherent story).
var (
	curUA    = chromeUA
	curMajor = chromeMajor
	// exact client hints exported from the browser session (uaData). When set,
	// secChHeaders mirrors them verbatim instead of the synthetic Chrome 133 set.
	curHints *uaHints
)

type uaHints struct {
	Brands          []struct {
		Brand   string `json:"brand"`
		Version string `json:"version"`
	} `json:"brands"`
	Mobile          bool   `json:"mobile"`
	Platform        string `json:"platform"`
	Architecture    string `json:"architecture"`
	Bitness         string `json:"bitness"`
	Model           string `json:"model"`
	PlatformVersion string `json:"platformVersion"`
	FullVersionList []struct {
		Brand   string `json:"brand"`
		Version string `json:"version"`
	} `json:"fullVersionList"`
	Wow64       bool     `json:"wow64"`
	FormFactors []string `json:"formFactors"`
}

func brandList(bs []struct {
	Brand   string `json:"brand"`
	Version string `json:"version"`
}) string {
	parts := make([]string, len(bs))
	for i, b := range bs {
		parts[i] = `"` + b.Brand + `";v="` + b.Version + `"`
	}
	return strings.Join(parts, ", ")
}

func secChHeaders() map[string]string {
	if curHints != nil {
		h := map[string]string{
			"sec-ch-ua":                  brandList(curHints.Brands),
			"sec-ch-ua-full-version-list": brandList(curHints.FullVersionList),
			"sec-ch-ua-arch":             `"` + curHints.Architecture + `"`,
			"sec-ch-ua-bitness":          `"` + curHints.Bitness + `"`,
			"sec-ch-ua-model":            `""`,
			"sec-ch-ua-mobile":           "?0",
			"sec-ch-ua-platform":         `"` + curHints.Platform + `"`,
			"sec-ch-ua-platform-version": `"` + curHints.PlatformVersion + `"`,
			"sec-ch-ua-wow64":            "?0",
		}
		if curHints.Mobile {
			h["sec-ch-ua-mobile"] = "?1"
		}
		if curHints.Wow64 {
			h["sec-ch-ua-wow64"] = "?1"
		}
		for _, b := range curHints.FullVersionList {
			if b.Brand == "Chromium" || b.Brand == "Google Chrome" {
				h["sec-ch-ua-full-version"] = `"` + b.Version + `"`
			}
		}
		if len(curHints.FormFactors) > 0 {
			h["sec-ch-ua-form-factors"] = `"` + strings.Join(curHints.FormFactors, `", "`) + `"`
		}
		return h
	}
	v := curMajor
	fv := curMajor + ".0.0.0"
	return map[string]string{
		"sec-ch-ua":                  `"Google Chrome";v="` + v + `", "Chromium";v="` + v + `", "Not_A Brand";v="8"`,
		"sec-ch-ua-arch":             `"x86"`,
		"sec-ch-ua-bitness":          `"64"`,
		"sec-ch-ua-full-version-list": `"Google Chrome";v="` + fv + `", "Chromium";v="` + fv + `", "Not_A Brand";v="8.0.0.0"`,
		"sec-ch-ua-mobile":           "?0",
		"sec-ch-ua-model":            `""`,
		"sec-ch-ua-platform":         `"Windows"`,
		"sec-ch-ua-platform-version": `"19.0.0"`,
		"sec-ch-ua-wow64":            "?0",
	}
}

// xBrowserValidation = Base64(SHA-1(API_KEY + User-Agent)) — header Chrome sends.
func xBrowserValidation() string {
	sum := sha1.Sum([]byte(xbvWinAPIKey + curUA))
	return base64.StdEncoding.EncodeToString(sum[:])
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

func randLetters(n int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, n)
	r := make([]byte, n)
	rand.Read(r)
	for i := range b {
		b[i] = alpha[int(r[i])%len(alpha)]
	}
	return string(b)
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

func mustRead(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		fatal("read %s: %v", path, err)
	}
	return string(b)
}

func fatal(f string, a ...any) {
	fmt.Printf("\n[FATAL] "+f+"\n", a...)
	os.Exit(1)
}

func log(f string, a ...any) { fmt.Printf("[+] "+f+"\n", a...) }

var verbose bool

func dbg(f string, a ...any) {
	if verbose {
		fmt.Printf("    .. "+f+"\n", a...)
	}
}

type gclient struct {
	hc    tls_client.HttpClient
	jar   tls_client.CookieJar
	reqid int
}

func newGClient() *gclient {
	return newGClientProfile(profiles.Chrome_133)
}

func newGClientProfile(p profiles.ClientProfile) *gclient {
	jar := tls_client.NewCookieJar()
	hc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithTimeoutSeconds(60),
		tls_client.WithClientProfile(p),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithCookieJar(jar),
	)
	if err != nil {
		fatal("tls-client: %v", err)
	}
	// browser uses a random 5-digit _reqid, +100000 per batchexecute step
	return &gclient{hc: hc, jar: jar, reqid: 10000 + randInt(90000)}
}

// do issues a request with browser-ish headers. Does not follow redirects.
func (g *gclient) do(method, rawurl string, body io.Reader, hdr map[string]string) (*http.Response, []byte) {
	req, err := http.NewRequest(method, rawurl, body)
	if err != nil {
		fatal("new request: %v", err)
	}
	req.Header.Set("User-Agent", curUA)
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en-US;q=0.8")
	for k, v := range secChHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Browser-Validation", xBrowserValidation())
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		fatal("%s %s: %v", method, rawurl, err)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	if err != nil {
		fatal("read body: %v", err)
	}
	return resp, b
}

// followRedirects manually walks 3xx chains, stopping before 127.0.0.1 loopback.
// Returns the final response (and the loopback Location if that was the last hop).
func (g *gclient) followRedirects(rawurl string) (*http.Response, []byte, string) {
	cur := rawurl
	for i := 0; i < 12; i++ {
		resp, b := g.do("GET", cur, nil, nil)
		loc := resp.Header.Get("Location")
		dbg("GET %s → %d  loc=%s", truncURL(cur), resp.StatusCode, truncURL(loc))
		if resp.StatusCode >= 300 && resp.StatusCode < 400 && loc != "" {
			if strings.HasPrefix(loc, "http://127.0.0.1") || strings.HasPrefix(loc, "http://localhost") {
				return resp, b, loc
			}
			u, err := url.Parse(cur)
			if err != nil {
				fatal("parse: %v", err)
			}
			rel, err := url.Parse(loc)
			if err != nil {
				fatal("parse loc: %v", err)
			}
			cur = u.ResolveReference(rel).String()
			continue
		}
		return resp, b, ""
	}
	fatal("too many redirects from %s", cur)
	return nil, nil, ""
}

func truncURL(u string) string {
	if len(u) > 110 {
		return u[:110] + "…"
	}
	return u
}

// wizVal extracts "KEY":"value" from a boq page's WIZ_global_data.
func wizVal(html, key string) string {
	re := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `":"((?:[^"\\]|\\.)*)"`)
	m := re.FindStringSubmatch(html)
	if m == nil {
		return ""
	}
	s, err := strconvUnquote(m[1])
	if err != nil {
		return m[1]
	}
	return s
}

func strconvUnquote(s string) (string, error) {
	var out string
	if err := json.Unmarshal([]byte(`"`+s+`"`), &out); err != nil {
		return "", err
	}
	return out, nil
}

// batchExecute posts one RPC to AccountsSignInUi batchexecute and returns the
// decoded wrb.fr payload string for that rpcid.
func (g *gclient) batchExecute(rpcid, sourcePath, fSid, bl, dsh, tl, innerJSON, at string) (string, error) {
	q := url.Values{
		"rpcids":      {rpcid},
		"source-path": {sourcePath},
		"f.sid":       {fSid},
		"bl":          {bl},
		"hl":          {"pt-BR"},
		"_reqid":      {fmt.Sprintf("%d", g.reqid)},
		"rt":          {"c"},
	}
	g.reqid += 100000
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
	resp, b := g.do("POST", batchBase+"?"+q.Encode(), strings.NewReader(form.Encode()), map[string]string{
		"Content-Type":               "application/x-www-form-urlencoded;charset=UTF-8",
		"X-Same-Domain":              "1",
		"Referer":                    "https://accounts.google.com/",
		"Origin":                     "https://accounts.google.com",
		"sec-fetch-dest":             "empty",
		"sec-fetch-mode":             "cors",
		"sec-fetch-site":             "same-origin",
		"x-goog-ext-278367001-jspb":  `["GeneralOAuthFlow"]`,
		"x-goog-ext-391502476-jspb":  fmt.Sprintf(`["%s","lso"]`, dsh),
	})
	dbg("batch %s → HTTP %d (%d bytes)", rpcid, resp.StatusCode, len(b))
	if verbose && len(b) < 1200 {
		dbg("raw: %s", string(b))
	}
	return extractWrbFr(string(b), rpcid)
}

// extractWrbFr parses a batchexecute response (length-prefixed JSON chunks after
// the )]}' guard) and returns the decoded payload string of the given rpcid.
func extractWrbFr(body, rpcid string) (string, error) {
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
			sub, ok := item.([]any)
			if !ok || len(sub) < 3 {
				continue
			}
			if sub[0] == "wrb.fr" && sub[1] == rpcid {
				payload, _ := sub[2].(string)
				return payload, nil
			}
		}
	}
	return "", fmt.Errorf("wrb.fr for %s not found in %d chunks (body head: %.200s)", rpcid, len(chunks), body)
}

// walkStrings recursively collects all strings in a decoded JSON value.
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

// findPair walks for [key, value] pairs.
func findPair(v any, key string) string {
	if arr, ok := v.([]any); ok {
		if len(arr) == 2 {
			if k, ok := arr[0].(string); ok && k == key {
				if s, ok := arr[1].(string); ok {
					return s
				}
			}
		}
		for _, e := range arr {
			if r := findPair(e, key); r != "" {
				return r
			}
		}
	}
	return ""
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// mintToken runs the Node botguard minter and returns the "!" token.
func mintToken(pageURL string, args ...string) (string, error) {
	full := append([]string{"tmp_probe/mint.mjs"}, args...)
	cmd := exec.Command("node", full...)
	cmd.Env = append(os.Environ(), "GLIF_HTTP_DEBUG=0")
	if pageURL != "" {
		cmd.Env = append(cmd.Env, "GLIF_PAGE_URL="+pageURL)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("minter: %v | %s", err, truncStr(stderr.String(), 400))
	}
	tok := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(tok, "!") {
		return "", fmt.Errorf("minter returned no token: %s", truncStr(stderr.String(), 300))
	}
	return tok, nil
}

func localPart(email string) string {
	if i := strings.Index(email, "@"); i > 0 {
		return email[:i]
	}
	return email
}

func strconvQuote(s string) string {
	q, _ := json.Marshal(s)
	return string(q)
}

// batchExecuteMulti posts a multi-RPC batchexecute with a raw f.req JSON string.
func (g *gclient) batchExecuteMulti(rpcidsQuery, sourcePath, fSid, bl, dsh, tl, freqJSON, at string) (string, error) {
	q := url.Values{
		"rpcids":      {rpcidsQuery},
		"source-path": {sourcePath},
		"f.sid":       {fSid},
		"bl":          {bl},
		"hl":          {"pt-BR"},
		"_reqid":      {fmt.Sprintf("%d", g.reqid)},
		"rt":          {"c"},
	}
	g.reqid += 100000
	if dsh != "" {
		q.Set("dsh", dsh)
	}
	if tl != "" {
		q.Set("TL", tl)
	}
	form := url.Values{"f.req": {freqJSON}, "at": {at}}
	resp, b := g.do("POST", batchBase+"?"+q.Encode(), strings.NewReader(form.Encode()), map[string]string{
		"Content-Type":              "application/x-www-form-urlencoded;charset=UTF-8",
		"X-Same-Domain":             "1",
		"Referer":                   "https://accounts.google.com/",
		"x-goog-ext-278367001-jspb": `["GeneralOAuthFlow"]`,
		"x-goog-ext-391502476-jspb": fmt.Sprintf(`["%s","lso"]`, dsh),
	})
	dbg("batch %s → HTTP %d (%d bytes)", rpcidsQuery, resp.StatusCode, len(b))
	return string(b), nil
}

func pkceGen() (string, string) {
	b := make([]byte, 32)
	rand.Read(b)
	v := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(v))
	return v, base64.RawURLEncoding.EncodeToString(sum[:])
}

func main() {
	email := flag.String("email", "", "google account email")
	password := flag.String("password", "", "google account password")
	bgMode := flag.String("bg", "empty", "botguard token mode: empty|omit|junk|mint|session")
	hashMode := flag.String("hash", "null", "tail hash mode: null|omit")
	sessionFile := flag.String("session", "", "session.json from tmp_probe/export_session.mjs (browser-minted token + cookies)")
	flag.BoolVar(&verbose, "v", false, "verbose")
	flag.Parse()
	if *email == "" || *password == "" {
		fatal("need -email and -password")
	}

	clientID, clientSecret := googleOAuthCreds()
	verifier, challenge := pkceGen()
	redirectURI := "http://127.0.0.1:61120/callback"

	var g *gclient
	var identURL, at, fSid, bl, dsh, rart, cont, html string
	sessionToken := ""

	if *sessionFile != "" {
		// ── Session mode: browser loaded the identifier page and minted the
		// token; Go continues the credential RPCs over pure HTTP. ──
		data, err := os.ReadFile(*sessionFile)
		if err != nil {
			fatal("read session: %v", err)
		}
		var sess struct {
			IdentURL string   `json:"identURL"`
			UA       string   `json:"ua"`
			UAData   *uaHints `json:"uaData"`
			PKCE     struct {
				Verifier  string `json:"verifier"`
				Challenge string `json:"challenge"`
			} `json:"pkce"`
			Wiz struct {
				At   string `json:"at"`
				FSid string `json:"fSid"`
				Bl   string `json:"bl"`
				Dsh  string `json:"dsh"`
				Rart string `json:"rart"`
				Cont string `json:"cont"`
			} `json:"wiz"`
			Cookies []struct {
				Name     string  `json:"name"`
				Value    string  `json:"value"`
				Domain   string  `json:"domain"`
				Path     string  `json:"path"`
				Expires  float64 `json:"expires"`
				HTTPOnly bool    `json:"httpOnly"`
				Secure   bool    `json:"secure"`
			} `json:"cookies"`
			IDToken string `json:"idToken"`
		}
		if err := json.Unmarshal(data, &sess); err != nil {
			fatal("parse session: %v", err)
		}
		// UA coherence: Go RPCs must claim the same UA the browser used.
		curUA = sess.UA
		if m := regexp.MustCompile(`Chrome/(\d+)\.`).FindStringSubmatch(sess.UA); m != nil {
			curMajor = m[1]
		}
		if sess.UAData != nil {
			curHints = sess.UAData
		}
		g = newGClientProfile(profiles.Chrome_146)
		// seed the browser's cookies into the Go jar
		for _, c := range sess.Cookies {
			for _, dom := range []string{c.Domain, strings.TrimPrefix(c.Domain, ".")} {
				u := &url.URL{Scheme: "https", Host: dom, Path: "/"}
				ck := &http.Cookie{
					Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
					Secure: c.Secure, HttpOnly: c.HTTPOnly,
				}
				if c.Expires > 0 {
					ck.Expires = time.Unix(int64(c.Expires), 0)
				}
				g.jar.SetCookies(u, []*http.Cookie{ck})
			}
		}
		identURL = sess.IdentURL
		at, fSid, bl = sess.Wiz.At, sess.Wiz.FSid, sess.Wiz.Bl
		dsh, rart, cont = sess.Wiz.Dsh, sess.Wiz.Rart, sess.Wiz.Cont
		verifier = sess.PKCE.Verifier
		challenge = sess.PKCE.Challenge
		sessionToken = sess.IDToken
		log("session mode: f.sid=%s bl=%s ua=Chrome/%s cookies=%d token=%d chars",
			fSid, bl, curMajor, len(sess.Cookies), len(sessionToken))
	} else {
		g = newGClient()

		// ── Step 1: OAuth auth URL → 302 → identifier URL ──────────────────
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
		resp, _ := g.do("GET", authURL, nil, nil)
		if resp.StatusCode != 302 {
			fatal("oauth auth: expected 302, got %d", resp.StatusCode)
		}
		identURL = resp.Header.Get("Location")
		if !strings.Contains(identURL, "/v3/signin/identifier") {
			fatal("unexpected redirect: %s", truncURL(identURL))
		}
		log("step1 ok: identifier URL obtida")

		// ── Step 2: GET identifier page, extract WIZ data ──────────────────
		resp, b := g.do("GET", identURL, nil, nil)
		if resp.StatusCode != 200 {
			fatal("identifier page: HTTP %d", resp.StatusCode)
		}
		html = string(b)
		at = wizVal(html, "SNlM0e")
		fSid = wizVal(html, "FdrFJe")
		bl = wizVal(html, "cfb2h")
		if at == "" || fSid == "" || bl == "" {
			fatal("WIZ data missing: at=%q f.sid=%q bl=%q", at, fSid, bl)
		}
		iu, _ := url.Parse(identURL)
		iq := iu.Query()
		dsh = iq.Get("dsh")
		rart = iq.Get("rart")
		cont = iq.Get("continue")
		log("step2 ok: at=%s… f.sid=%s bl=%s", at[:12], fSid, bl)
		dbg("dsh=%s rart=%s…", dsh, rart[:16])
		dbg("continue=%s", truncURL(cont))

		// ambient requests the browser fires in parallel right after the identifier
		// page loads, before MI613e (from trace4): CheckConnection, bscframe, 204.
		go func() {
			g.do("GET", fmt.Sprintf("https://accounts.youtube.com/accounts/CheckConnection?pmpo=https%%3A%%2F%%2Faccounts.google.com&v=%d&timestamp=%d",
				randInt(2000000000)-1000000000, time.Now().UnixMilli()), nil, map[string]string{
				"Referer": identURL,
			})
		}()
		go func() {
			g.do("GET", "https://accounts.google.com/_/bscframe", nil, map[string]string{
				"Referer": identURL,
			})
		}()
		go func() {
			g.do("GET", "https://accounts.google.com/generate_204?"+randLetters(6), nil, map[string]string{
				"Referer": identURL,
			})
		}()
		time.Sleep(300 * time.Millisecond)

		// browserinfo beacon (browser sends it before MI613e)
		biQ := url.Values{
			"f.sid": {fSid}, "bl": {bl}, "hl": {"pt-BR"},
			"_reqid": {"10100"}, "rt": {"j"},
		}
		if dsh != "" {
			biQ.Set("dsh", dsh)
		}
		biForm := url.Values{
			"f.req": {`[9,1,1,[null,800,1280],[null,800,1280],[1,1,null,1],[0,2,2]]`},
			"at":    {at},
		}
		_, biB := g.do("POST", "https://accounts.google.com/v3/signin/_/AccountsSignInUi/browserinfo?"+biQ.Encode(),
			strings.NewReader(biForm.Encode()), map[string]string{
				"Content-Type":  "application/x-www-form-urlencoded;charset=UTF-8",
				"X-Same-Domain": "1",
				"Referer":       "https://accounts.google.com/",
			})
		dbg("browserinfo → %d bytes", len(biB))
	}

	// ── Step 3: MI613e identifier lookup ───────────────────────────────
	// trailing field = fresh ephemeral ECDH P-256 public key (SEC1 uncompressed,
	// base64) — RE'd 2026-08-19: window.crypto.subtle.generateKey(ECDH P-256),
	// private key stored in sessionStorage for later challenge decryption.
	ecdhKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		fatal("ecdh: %v", err)
	}
	tailKey := base64.StdEncoding.EncodeToString(ecdhKey.PublicKey().Bytes())

	tpl := mustRead("tmp_probe/templates/MI613e.tpl.json")
	bgID := `""`
	switch *bgMode {
	case "junk":
		bgID = `"!AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`
	case "empty":
		bgID = `""`
	case "omit":
		defer func() {}() // handled after substitution
	}
	_ = bgID
	repl := strings.NewReplacer(
		"@@CONTINUE@@", cont,
		"@@RART@@", rart,
		"@@DSH@@", dsh,
		"@@CHALLENGE@@", challenge,
		"@@TL@@", "",
		"@@EMAIL@@", *email,
		"@@YT@@", "youtube:417",
		"@@TAILHASH@@", tailKey,
	)
	miInner := repl.Replace(tpl)
	// botguard token handling
	idTokRe := regexp.MustCompile(`\["identity-signin-identifier","[^"]*"\]`)
	switch {
	case sessionToken != "":
		// browser-minted token from export_session.mjs
		miInner = idTokRe.ReplaceAllString(miInner, `["identity-signin-identifier",`+strconvQuote(sessionToken)+`]`)
		log("step2.5 ok: usando token botguard do browser (%d chars)", len(sessionToken))
	}
	switch *bgMode {
	case "mint":
		os.WriteFile("tmp_probe/live_identifier.html", []byte(html), 0o600)
		binding := fmt.Sprintf(`{"Ko":{"replayKey":"%s"}}`, localPart(*email))
		tok, merr := mintToken("", "--html", "tmp_probe/live_identifier.html", "--binding", binding)
		if merr != nil {
			fatal("mint identifier token: %v", merr)
		}
		log("step2.5 ok: identifier botguard token mintado (%d bytes decod)", len(tok))
		miInner = idTokRe.ReplaceAllString(miInner, `["identity-signin-identifier",`+strconvQuote(tok)+`]`)
	case "empty":
		miInner = idTokRe.ReplaceAllString(miInner, `["identity-signin-identifier",""]`)
	case "omit":
		miInner = idTokRe.ReplaceAllString(miInner, ``)
		miInner = strings.ReplaceAll(miInner, `[,`, `[`)
		miInner = strings.ReplaceAll(miInner, `,]`, `]`)
		miInner = strings.ReplaceAll(miInner, `,,`, `,`)
	case "junk":
		miInner = idTokRe.ReplaceAllString(miInner, `["identity-signin-identifier","!AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"]`)
	}
	// validate JSON
	var tmp any
	if err := json.Unmarshal([]byte(miInner), &tmp); err != nil {
		fatal("MI613e template invalid after substitution: %v", err)
	}
	_ = hashMode // tail hash is now a real ECDH key; flag kept for CLI compat

	payload, err := g.batchExecute("MI613e", "/v3/signin/identifier", fSid, bl, dsh, "", miInner, at)
	if err != nil {
		fatal("MI613e: %v", err)
	}
	var miResp any
	if err := json.Unmarshal([]byte(payload), &miResp); err != nil {
		fatal("MI613e payload not json: %v\n%s", err, truncStr(payload, 300))
	}
	var strs []string
	walkStrings(miResp, &strs)
	tl := findPair(miResp, "TL")
	pwdPath := ""
	for _, s := range strs {
		if strings.Contains(s, "/v3/signin/challenge/pwd") {
			pwdPath = s
			break
		}
	}
	if tl == "" || pwdPath == "" {
		fmt.Printf("[!] MI613e response did not yield pwd challenge. payload head:\n%.600s\n", payload)
		fatal("identifier rejected (bgMode=%s hashMode=%s?)", *bgMode, *hashMode)
	}
	log("step3 ok: TL=%s… pwdPath=%s", tl[:12], pwdPath)

	// ── Step 3.5: WZfWSd multi-RPC → password challenge bfkj (vmCode+program) ──
	pwdBgToken := ""
	if *bgMode == "mint" {
		wzTpl := mustRead("tmp_probe/templates/WZfWSd_multi.tpl.json")
		wzFreq := strings.NewReplacer("@@CLIENTID@@", clientID).Replace(wzTpl)
		wzBody, err := g.batchExecuteMulti(
			"WZfWSd,etGTrd,Aho3hb,i3kFoc,zKAP2e,RzSO2e",
			"/v3/signin/challenge/pwd", fSid, bl, dsh, tl, wzFreq, at)
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
		progRe := regexp.MustCompile(`"([A-Za-z0-9+/=]{10000,})"`)
		pm := progRe.FindStringSubmatch(zkPayload)
		if vmCode == "" || pm == nil {
			fatal("zKAP2e: vmCode/program not found (vmCode=%d)", len(vmCode))
		}
		os.WriteFile("tmp_probe/live_pwd_vmcode.js", []byte(vmCode), 0o600)
		os.WriteFile("tmp_probe/live_pwd_program.txt", []byte(pm[1]), 0o600)
		binding := fmt.Sprintf(`{"Ko":{"replayKey":"%s"}}`, localPart(*email))
		tok, merr := mintToken("https://accounts.google.com/v3/signin/challenge/pwd",
			"--vmcode", "tmp_probe/live_pwd_vmcode.js", "--program", "tmp_probe/live_pwd_program.txt", "--binding", binding)
		if merr != nil {
			fatal("mint password token: %v", merr)
		}
		pwdBgToken = tok
		log("step3.5 ok: password botguard token mintado")
	}

	// ── Step 4: B4hajb password ────────────────────────────────────────
	tpl = mustRead("tmp_probe/templates/B4hajb.tpl.json")
	repl = strings.NewReplacer(
		"@@CONTINUE@@", cont,
		"@@RART@@", rart,
		"@@DSH@@", dsh,
		"@@CHALLENGE@@", challenge,
		"@@TL@@", tl,
		"@@EMAIL@@", *email,
		"@@PASSWORD@@", *password,
		"@@YT@@", "youtube:417",
	)
	b4Inner := repl.Replace(tpl)
	pwdTokRe := regexp.MustCompile(`\["identity-signin-password","[^"]*"\]`)
	switch *bgMode {
	case "mint":
		b4Inner = pwdTokRe.ReplaceAllString(b4Inner, `["identity-signin-password",`+strconvQuote(pwdBgToken)+`]`)
	case "empty":
		b4Inner = pwdTokRe.ReplaceAllString(b4Inner, `["identity-signin-password",""]`)
	case "omit":
		b4Inner = pwdTokRe.ReplaceAllString(b4Inner, ``)
		b4Inner = strings.ReplaceAll(b4Inner, `[,`, `[`)
		b4Inner = strings.ReplaceAll(b4Inner, `,]`, `]`)
		b4Inner = strings.ReplaceAll(b4Inner, `,,`, `,`)
	case "junk":
		b4Inner = pwdTokRe.ReplaceAllString(b4Inner, `["identity-signin-password","!AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"]`)
	}
	if err := json.Unmarshal([]byte(b4Inner), &tmp); err != nil {
		fatal("B4hajb template invalid after substitution: %v", err)
	}
	payload, err = g.batchExecute("B4hajb", "/v3/signin/challenge/pwd", fSid, bl, dsh, tl, b4Inner, at)
	if err != nil {
		fatal("B4hajb: %v", err)
	}
	var b4Resp any
	if err := json.Unmarshal([]byte(payload), &b4Resp); err != nil {
		fatal("B4hajb payload not json: %v\n%.400s", err, payload)
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
		fmt.Printf("[!] B4hajb response has no CheckCookie URL. payload head:\n%.800s\n", payload)
		fatal("password rejected? (bgMode=%s)", *bgMode)
	}
	if strings.HasPrefix(checkCookieURL, "/") {
		checkCookieURL = "https://accounts.google.com" + checkCookieURL
	}
	log("step4 ok: CheckCookie=%s", truncURL(checkCookieURL))

	// ── Step 5: CheckCookie → consent page ─────────────────────────────
	resp, b, loop := g.followRedirects(checkCookieURL)
	if loop != "" {
		fatal("unexpected: loopback before consent: %s", truncURL(loop))
	}
	if resp.StatusCode != 200 || !strings.Contains(resp.Request.URL.Host, "accounts.google") {
		fatal("consent chain ended oddly: HTTP %d %s", resp.StatusCode, truncURL(resp.Request.URL.String()))
	}
	consentHTML := string(b)
	at2 := wizVal(consentHTML, "SNlM0e")
	fSid2 := wizVal(consentHTML, "FdrFJe")
	bl2 := wizVal(consentHTML, "cfb2h")
	dsh2 := wizVal(consentHTML, "Qzxixc")
	tokRe := regexp.MustCompile(`!(ChR[A-Za-z0-9_+/=\-\x{2219}]+)`)
	tokM := tokRe.FindStringSubmatch(consentHTML)
	if at2 == "" || len(tokM) < 2 {
		fatal("consent page tokens missing: at2=%q tok=%v", at2, tokM)
	}
	consentTok := "!" + tokM[1]
	log("step5 ok: consent page, token=%s…", consentTok[:20])

	// ── Step 6: xyhAld consent approval ────────────────────────────────
	tpl = mustRead("tmp_probe/templates/xyhAld.tpl.json")
	repl = strings.NewReplacer(
		"@@CONSENTTOK@@", consentTok,
		"@@DSH@@", dsh,
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
		"_reqid":       {fmt.Sprintf("%d", g.reqid)},
		"rt":           {"c"},
	}
	innerQuoted, _ := json.Marshal(xyInner)
	freq := fmt.Sprintf(`[[["xyhAld",%s,null,"generic"]]]`, string(innerQuoted))
	form := url.Values{"f.req": {freq}, "at": {at2}}
	req, _ := http.NewRequest("POST", oauthUIBatch+"?"+q.Encode(), strings.NewReader(form.Encode()))
	req.Header.Set("User-Agent", curUA)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	req.Header.Set("X-Same-Domain", "1")
	req.Header.Set("Referer", "https://accounts.google.com/")
	if dsh2 != "" {
		req.Header.Set("x-goog-ext-391502476-jspb", fmt.Sprintf(`["%s"]`, dsh2))
	}
	resp2, err := g.hc.Do(req)
	if err != nil {
		fatal("xyhAld: %v", err)
	}
	b2, _ := io.ReadAll(io.LimitReader(resp2.Body, 4<<20))
	resp2.Body.Close()
	payload, err = extractWrbFr(string(b2), "xyhAld")
	if err != nil {
		fmt.Printf("[!] xyhAld response raw head:\n%.600s\n", string(b2))
		fatal("xyhAld: %v", err)
	}
	var xyResp any
	if err := json.Unmarshal([]byte(payload), &xyResp); err != nil {
		fatal("xyhAld payload not json: %v\n%.400s", err, payload)
	}
	strs = nil
	walkStrings(xyResp, &strs)
	followURL := ""
	for _, s := range strs {
		if strings.Contains(s, "/signin/oauth/consent") && strings.Contains(s, "as=") {
			followURL = s
			break
		}
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

	// ── Step 7: follow to loopback code ────────────────────────────────
	_, _, loop = g.followRedirects(followURL)
	if loop == "" {
		fatal("no loopback redirect at the end")
	}
	lu, _ := url.Parse(loop)
	code := lu.Query().Get("code")
	if code == "" {
		fatal("loopback URL without code: %s", truncURL(loop))
	}
	log("step7 ok: authorization code capturado!")

	// ── Step 8: code → tokens ──────────────────────────────────────────
	form = url.Values{
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	resp3, b3 := g.do("POST", googleTokenURL, strings.NewReader(form.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	})
	if resp3.StatusCode >= 400 {
		fatal("token exchange HTTP %d: %.300s", resp3.StatusCode, string(b3))
	}
	var tok map[string]any
	json.Unmarshal(b3, &tok)
	idToken, _ := tok["id_token"].(string)
	refreshTok, _ := tok["refresh_token"].(string)
	if idToken == "" {
		fatal("no id_token in response: %.300s", string(b3))
	}
	log("step8 ok: id_token=%s… refresh=%v", idToken[:24], refreshTok != "")

	out := map[string]string{
		"id_token":      idToken,
		"refresh_token": refreshTok,
	}
	ob, _ := json.MarshalIndent(out, "", "  ")
	os.WriteFile("tmp_probe/google_tokens.json", ob, 0o600)
	log("tokens salvos em tmp_probe/google_tokens.json")
	fmt.Println("\n=== LOGIN HTTP DIRECT COMPLETO ===")
}
