package accmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Inbox is a disposable email inbox that can receive the Accio verification
// code. Multiple providers are rotated so a flagged domain cannot block the
// whole pipeline.
type Inbox interface {
	// Address returns the disposable email address.
	Address() string
	// Reset marks every message currently in the mailbox as seen, so the
	// next WaitCode only accepts codes from messages that arrive afterwards.
	// Without this, a retry pass would read the previous pass's stale code.
	Reset(ctx context.Context)
	// WaitCode polls until a 6-digit ACCIO code arrives (or ctx expires).
	WaitCode(ctx context.Context) (string, error)
	// Provider names the inbox backend ("mailtm", "tempmaillol", "1secmail").
	Provider() string
	// Secret returns the credential needed to reopen this exact inbox later
	// (mail.tm password, tempmail.lol token; empty for 1secmail).
	Secret() string
}

// NewInbox creates an inbox from a rotating provider pool: mail.tm first,
// then tempmail.lol, then 1secmail as last resort. Providers and domains are
// rotated so a flagged disposable domain cannot stall the pipeline.
func NewInbox(ctx context.Context) (Inbox, error) {
	// ACCIO_TEMPMAIL forces a single provider (mailtm | tempmaillol |
	// 1secmail) — used to A/B disposable domains when a domain gets flagged.
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ACCIO_TEMPMAIL"))) {
	case "mailtm":
		return newMailTM(ctx)
	case "tempmaillol":
		return newTempmailLol(ctx)
	case "1secmail":
		return newOneSecMail(ctx)
	}
	providers := []func(context.Context) (Inbox, error){
		func(ctx context.Context) (Inbox, error) { return newMailTM(ctx) },
		func(ctx context.Context) (Inbox, error) { return newTempmailLol(ctx) },
		func(ctx context.Context) (Inbox, error) { return newMailTM(ctx) },
		func(ctx context.Context) (Inbox, error) { return newOneSecMail(ctx) },
	}
	var lastErr error
	for _, p := range providers {
		in, err := p(ctx)
		if err == nil {
			return in, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("no inbox provider available: %w", lastErr)
}

// ReopenInbox reconnects to a previously created inbox using the persisted
// provider + secret (see TokenRecord.InboxProvider/InboxSecret). Used to log
// a pending account in again — the Accio site sends a fresh code to the same
// disposable address.
func ReopenInbox(ctx context.Context, provider, address, secret string) (Inbox, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	switch provider {
	case "mailtm":
		tm := &MailTM{addr: address, password: secret, http: client}
		if err := tm.authenticate(ctx); err != nil {
			return nil, fmt.Errorf("reopen mailtm: %w", err)
		}
		return tm, nil
	case "tempmaillol":
		return &TempmailLol{addr: address, token: secret, http: client}, nil
	default:
		return nil, fmt.Errorf("cannot reopen provider %q", provider)
	}
}

// ---- mail.tm ----

type MailTM struct {
	addr     string
	password string
	token    string
	http     *http.Client
	seen     seenSet
}

const mailTmBase = "https://api.mail.tm"

func newMailTM(ctx context.Context) (*MailTM, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	var domains []struct {
		Domain string `json:"domain"`
	}
	if err := getJSON(ctx, client, mailTmBase+"/domains", nil, &domains); err != nil {
		return nil, err
	}
	// Collect all well-formed domains and pick one at random — the Accio
	// anti-fraud flags specific disposable domains, so a single hardcoded
	// domain gets blocked after a few signups.
	domainRe := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	valid := make([]string, 0, len(domains))
	for _, d := range domains {
		if d.Domain != "" && domainRe.MatchString(d.Domain) {
			valid = append(valid, d.Domain)
		}
	}
	if len(valid) == 0 {
		return nil, fmt.Errorf("mail.tm: no valid domains available")
	}
	domain := valid[rand.Intn(len(valid))]

	addr := fmt.Sprintf("accs%d%s@%s", time.Now().UnixMilli()%1000000, randSuffix(), domain)
	pass := "Acci0!" + randSuffix() + "x"

	var created map[string]any
	if err := postJSON(ctx, client, mailTmBase+"/accounts", map[string]any{
		"address": addr, "password": pass,
	}, &created); err != nil {
		return nil, fmt.Errorf("mail.tm create %s: %w", addr, err)
	}

	tm := &MailTM{addr: addr, password: pass, http: client}
	if err := tm.authenticate(ctx); err != nil {
		return nil, err
	}
	return tm, nil
}

func (tm *MailTM) Address() string  { return tm.addr }
func (tm *MailTM) Provider() string { return "mailtm" }
func (tm *MailTM) Secret() string   { return tm.password }

func (tm *MailTM) authenticate(ctx context.Context) error {
	var tok struct {
		Token string `json:"token"`
	}
	if err := postJSON(ctx, tm.http, mailTmBase+"/token", map[string]any{
		"address": tm.addr, "password": tm.password,
	}, &tok); err != nil {
		return err
	}
	tm.token = tok.Token
	return nil
}

func (tm *MailTM) listMessages(ctx context.Context) ([]struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Intro   string `json:"intro"`
}, error,
) {
	var msgs []struct {
		ID      string `json:"id"`
		Subject string `json:"subject"`
		Intro   string `json:"intro"`
	}
	err := getJSON(ctx, tm.http, mailTmBase+"/messages", map[string]string{
		"Authorization": "Bearer " + tm.token,
	}, &msgs)
	return msgs, err
}

func (tm *MailTM) Reset(ctx context.Context) {
	msgs, err := tm.listMessages(ctx)
	if err != nil {
		return
	}
	keys := make([]string, 0, len(msgs))
	for _, m := range msgs {
		keys = append(keys, m.ID)
	}
	tm.seen.replace(keys)
}

func (tm *MailTM) WaitCode(ctx context.Context) (string, error) {
	return waitCodeLoop(ctx, func() (string, error) {
		msgs, err := tm.listMessages(ctx)
		if err != nil {
			return "", err
		}
		for _, m := range msgs {
			if tm.seen.has(m.ID) {
				continue
			}
			code := codeFromText(m.Subject + "\n" + m.Intro)
			if code == "" && m.ID != "" {
				// Code may only appear in the body: fetch the full message.
				var full struct {
					Subject string   `json:"subject"`
					Text    string   `json:"text"`
					HTML    []string `json:"html"`
				}
				if err := getJSON(ctx, tm.http, mailTmBase+"/messages/"+m.ID, map[string]string{
					"Authorization": "Bearer " + tm.token,
				}, &full); err == nil {
					code = codeFromText(full.Subject + "\n" + full.Text + "\n" + strings.Join(full.HTML, "\n"))
				}
			}
			if code != "" {
				tm.seen.mark(m.ID)
				return code, nil
			}
		}
		return "", nil
	})
}

// ---- tempmail.lol ----

type TempmailLol struct {
	addr  string
	token string
	http  *http.Client
	seen  seenSet
}

func newTempmailLol(ctx context.Context) (*TempmailLol, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	var out struct {
		Address string `json:"address"`
		Token   string `json:"token"`
	}
	// /generate accepts GET only (POST now returns an HTML info page).
	if err := getJSON(ctx, client, "https://api.tempmail.lol/generate", nil, &out); err != nil {
		return nil, err
	}
	if out.Address == "" || out.Token == "" {
		return nil, fmt.Errorf("tempmail.lol: incomplete response")
	}
	return &TempmailLol{addr: out.Address, token: out.Token, http: client}, nil
}

func (tl *TempmailLol) Address() string  { return tl.addr }
func (tl *TempmailLol) Provider() string { return "tempmaillol" }
func (tl *TempmailLol) Secret() string   { return tl.token }

func (tl *TempmailLol) listMessages(ctx context.Context) ([]struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
	HTML    string `json:"html"`
}, error,
) {
	// v2 inbox endpoint (the old /auth bearer endpoint was retired and now
	// serves an HTML info page or rate-limit errors).
	var out struct {
		Emails []struct {
			Subject string `json:"subject"`
			Body    string `json:"body"`
			HTML    string `json:"html"`
		} `json:"emails"`
		Expired bool `json:"expired"`
	}
	err := getJSON(ctx, tl.http, "https://api.tempmail.lol/v2/inbox?token="+tl.token, nil, &out)
	return out.Emails, err
}

// tempmail.lol messages carry no id; the subject+body pair is unique enough
// for a mailbox that only receives verification codes.
func tempmailLolKey(subject, body string) string { return subject + "\x00" + body }

func (tl *TempmailLol) Reset(ctx context.Context) {
	msgs, err := tl.listMessages(ctx)
	if err != nil {
		return
	}
	keys := make([]string, 0, len(msgs))
	for _, m := range msgs {
		keys = append(keys, tempmailLolKey(m.Subject, m.Body))
	}
	tl.seen.replace(keys)
}

func (tl *TempmailLol) WaitCode(ctx context.Context) (string, error) {
	// tempmail.lol rate-limits inbox checks below ~3s — poll every ~4-5s.
	return waitCodeLoopSlow(ctx, func() (string, error) {
		msgs, err := tl.listMessages(ctx)
		if err != nil {
			return "", err
		}
		for _, m := range msgs {
			key := tempmailLolKey(m.Subject, m.Body)
			if tl.seen.has(key) {
				continue
			}
			if code := codeFromText(m.Subject + "\n" + m.Body + "\n" + m.HTML); code != "" {
				tl.seen.mark(key)
				return code, nil
			}
		}
		return "", nil
	})
}

// ---- 1secmail ----

type OneSecMail struct {
	addr   string
	login  string
	domain string
	http   *http.Client
	seen   seenSet
}

const oneSecBase = "https://www.1secmail.com/api/v1"

func newOneSecMail(ctx context.Context) (*OneSecMail, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	// Get a list of domains and pick one randomly (the API offers many;
	// rotating domains avoids the Accio disposable-domain blocklist).
	var domains []string
	if err := getJSON(ctx, client, oneSecBase+"/?action=getDomainList", nil, &domains); err != nil {
		return nil, err
	}
	if len(domains) == 0 {
		return nil, fmt.Errorf("1secmail: no domains available")
	}
	domain := domains[rand.Intn(len(domains))]

	user := fmt.Sprintf("acc%d%s", time.Now().UnixMilli()%1000000, randSuffix())
	return &OneSecMail{
		addr:   user + "@" + domain,
		login:  user,
		domain: domain,
		http:   client,
	}, nil
}

func (os *OneSecMail) Address() string   { return os.addr }
func (os1 *OneSecMail) Provider() string { return "1secmail" }
func (os1 *OneSecMail) Secret() string   { return "" }

func (os *OneSecMail) listMessages(ctx context.Context) ([]struct {
	ID      int    `json:"id"`
	Subject string `json:"subject"`
}, error,
) {
	var msgs []struct {
		ID      int    `json:"id"`
		Subject string `json:"subject"`
	}
	u := fmt.Sprintf("%s/?action=getMessages&login=%s&domain=%s", oneSecBase, os.login, os.domain)
	err := getJSON(ctx, os.http, u, nil, &msgs)
	return msgs, err
}

func (os *OneSecMail) Reset(ctx context.Context) {
	msgs, err := os.listMessages(ctx)
	if err != nil {
		return
	}
	keys := make([]string, 0, len(msgs))
	for _, m := range msgs {
		keys = append(keys, strconv.Itoa(m.ID))
	}
	os.seen.replace(keys)
}

func (os *OneSecMail) WaitCode(ctx context.Context) (string, error) {
	return waitCodeLoop(ctx, func() (string, error) {
		msgs, err := os.listMessages(ctx)
		if err != nil {
			return "", err
		}
		for _, m := range msgs {
			key := strconv.Itoa(m.ID)
			if os.seen.has(key) {
				continue
			}
			code := codeFromText(m.Subject)
			if code == "" && m.ID != 0 {
				// Code may only appear in the body: fetch the full message.
				var full struct {
					Subject  string `json:"subject"`
					Body     string `json:"body"`
					TextBody string `json:"textBody"`
					HTMLBody string `json:"htmlBody"`
				}
				mu := fmt.Sprintf("%s/?action=readMessage&login=%s&domain=%s&id=%d", oneSecBase, os.login, os.domain, m.ID)
				if err := getJSON(ctx, os.http, mu, nil, &full); err == nil {
					code = codeFromText(full.Subject + "\n" + full.Body + "\n" + full.TextBody + "\n" + full.HTMLBody)
				}
			}
			if code != "" {
				os.seen.mark(key)
				return code, nil
			}
		}
		return "", nil
	})
}

// ---- shared helpers ----

// seenSet tracks message keys already present in a mailbox so WaitCode can
// ignore verification codes left over from earlier passes/attempts.
type seenSet struct {
	mu   sync.Mutex
	keys map[string]bool
}

func (s *seenSet) replace(keys []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = make(map[string]bool, len(keys))
	for _, k := range keys {
		s.keys[k] = true
	}
}

func (s *seenSet) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys[key]
}

func (s *seenSet) mark(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		s.keys = map[string]bool{}
	}
	s.keys[key] = true
}

// codeReSix prefers the canonical 6-digit code; codeReAny accepts 4-8 digits
// as a fallback (the provider occasionally changes the code format).
var codeReSix = regexp.MustCompile(`\b(\d{6})\b`)
var codeReAny = regexp.MustCompile(`\b(\d{4,8})\b`)

// codeFromText extracts the verification code from any email text (subject,
// intro, plain or HTML body). A 6-digit match wins; shorter/longer codes are
// accepted only when no 6-digit candidate exists.
func codeFromText(text string) string {
	if m := codeReSix.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	if m := codeReAny.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

func codeFromSubject(subject string) string {
	return codeFromText(subject)
}

// waitCodeLoop polls the provider every ~1.5-2.5s until a code arrives.
func waitCodeLoop(ctx context.Context, poll func() (string, error)) (string, error) {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		if code, err := poll(); err == nil && code != "" {
			return code, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(1500*time.Millisecond + time.Duration(rand.Intn(1000))*time.Millisecond):
		}
	}
	return "", fmt.Errorf("verification code did not arrive within 3m")
}

// waitCodeLoopSlow is waitCodeLoop with a ~4-5s interval for providers that
// rate-limit faster polling (tempmail.lol: "only check once every 3-5 seconds").
func waitCodeLoopSlow(ctx context.Context, poll func() (string, error)) (string, error) {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		if code, err := poll(); err == nil && code != "" {
			return code, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(4*time.Second + time.Duration(rand.Intn(1000))*time.Millisecond):
		}
	}
	return "", fmt.Errorf("verification code did not arrive within 3m")
}

func randSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%100000)
}

func getJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, truncateStr(string(raw), 200))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", url, err)
	}
	return nil
}

func postJSON(ctx context.Context, client *http.Client, url string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: HTTP %d: %s", url, resp.StatusCode, truncateStr(string(raw), 200))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode %s: %w", url, err)
		}
	}
	return nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func firstValueInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case string:
		var i int
		_, _ = fmt.Sscanf(v, "%d", &i)
		return i
	}
	return 0
}
