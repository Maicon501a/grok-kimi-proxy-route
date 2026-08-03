package accmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Inbox is a disposable email inbox that can receive the Accio verification
// code. Multiple providers are rotated so a flagged domain cannot block the
// whole pipeline.
type Inbox interface {
	// Address returns the disposable email address.
	Address() string
	// WaitCode polls until a 6-digit ACCIO code arrives (or ctx expires).
	WaitCode(ctx context.Context) (string, error)
}

// NewInbox creates an inbox from a rotating provider pool: mail.tm first,
// then tempmail.lol, then 1secmail as last resort. Providers and domains are
// rotated so a flagged disposable domain cannot stall the pipeline.
func NewInbox(ctx context.Context) (Inbox, error) {
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

// ---- mail.tm ----

type MailTM struct {
	addr     string
	password string
	token    string
	http     *http.Client
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

func (tm *MailTM) Address() string { return tm.addr }

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

func (tm *MailTM) WaitCode(ctx context.Context) (string, error) {
	return waitCodeLoop(ctx, func() (string, error) {
		var msgs []struct {
			Subject string `json:"subject"`
		}
		err := getJSON(ctx, tm.http, mailTmBase+"/messages", map[string]string{
			"Authorization": "Bearer " + tm.token,
		}, &msgs)
		if err != nil {
			return "", err
		}
		for _, m := range msgs {
			if code := codeFromSubject(m.Subject); code != "" {
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
}

func newTempmailLol(ctx context.Context) (*TempmailLol, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	var out struct {
		Address string `json:"address"`
		Token   string `json:"token"`
	}
	if err := postJSON(ctx, client, "https://api.tempmail.lol/generate", map[string]any{}, &out); err != nil {
		return nil, err
	}
	if out.Address == "" || out.Token == "" {
		return nil, fmt.Errorf("tempmail.lol: incomplete response")
	}
	return &TempmailLol{addr: out.Address, token: out.Token, http: client}, nil
}

func (tl *TempmailLol) Address() string { return tl.addr }

func (tl *TempmailLol) WaitCode(ctx context.Context) (string, error) {
	return waitCodeLoop(ctx, func() (string, error) {
		var msgs []struct {
			Subject string `json:"subject"`
		}
		err := getJSON(ctx, tl.http, "https://api.tempmail.lol/auth", map[string]string{
			"Authorization": "Bearer " + tl.token,
		}, &msgs)
		if err != nil {
			return "", err
		}
		for _, m := range msgs {
			if code := codeFromSubject(m.Subject); code != "" {
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

	user := fmt.Sprintf("acc%d%d", time.Now().UnixMilli()%1000000, randSuffix())
	return &OneSecMail{
		addr:   user + "@" + domain,
		login:  user,
		domain: domain,
		http:   client,
	}, nil
}

func (os *OneSecMail) Address() string { return os.addr }

func (os *OneSecMail) WaitCode(ctx context.Context) (string, error) {
	return waitCodeLoop(ctx, func() (string, error) {
		var msgs []struct {
			Subject string `json:"subject"`
		}
		u := fmt.Sprintf("%s/?action=getMessages&login=%s&domain=%s", oneSecBase, os.login, os.domain)
		err := getJSON(ctx, os.http, u, nil, &msgs)
		if err != nil {
			return "", err
		}
		for _, m := range msgs {
			if code := codeFromSubject(m.Subject); code != "" {
				return code, nil
			}
		}
		return "", nil
	})
}

// ---- shared helpers ----

var codeRe = regexp.MustCompile(`\b(\d{6})\b`)

func codeFromSubject(subject string) string {
	if m := codeRe.FindStringSubmatch(subject); m != nil {
		return m[1]
	}
	return ""
}

// waitCodeLoop polls the provider every ~3-5s until a code arrives.
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
		case <-time.After(3*time.Second + time.Duration(rand.Intn(2000))*time.Millisecond):
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
