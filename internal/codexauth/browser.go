package codexauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultCallbackPort  = 1455
	fallbackCallbackPort = 1457
)

type BrowserLogin struct {
	AuthURL     string
	RedirectURI string

	client       *Client
	listener     net.Listener
	state        string
	codeVerifier string
}

type browserLoginResult struct {
	tokens *TokenResponse
	err    error
}

// StartBrowser begins the official Codex authorization-code flow. The callback
// ports match the redirect URI allow-list used by the first-party Codex CLI.
func (c *Client) StartBrowser() (*BrowserLogin, error) {
	return c.startBrowser([]int{defaultCallbackPort, fallbackCallbackPort})
}

func (c *Client) startBrowser(ports []int) (*BrowserLogin, error) {
	var listener net.Listener
	var lastErr error
	for attempt := 0; attempt < 10 && listener == nil; attempt++ {
		for _, port := range ports {
			listener, lastErr = net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
			if lastErr == nil {
				break
			}
		}
		if listener == nil && attempt < 9 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if listener == nil {
		return nil, fmt.Errorf("codex callback ports unavailable: %w", lastErr)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", port)
	verifier, err := randomURLSafe(64)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	state, err := randomURLSafe(32)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	challengeSum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeSum[:])
	return &BrowserLogin{
		AuthURL:      c.buildAuthorizeURL(redirectURI, challenge, state),
		RedirectURI:  redirectURI,
		client:       c,
		listener:     listener,
		state:        state,
		codeVerifier: verifier,
	}, nil
}

func (c *Client) buildAuthorizeURL(redirectURI, challenge, state string) string {
	query := url.Values{}
	query.Set("response_type", "code")
	query.Set("client_id", c.clientID())
	query.Set("redirect_uri", redirectURI)
	query.Set("scope", DefaultScope)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("id_token_add_organizations", "true")
	query.Set("codex_cli_simplified_flow", "true")
	query.Set("state", state)
	query.Set("originator", "codex_cli_rs")
	return c.endpoint("/oauth/authorize") + "?" + query.Encode()
}

func (login *BrowserLogin) Wait(ctx context.Context) (*TokenResponse, error) {
	if login == nil || login.listener == nil || login.client == nil {
		return nil, fmt.Errorf("codex browser login is not initialized")
	}
	result := make(chan browserLoginResult, 1)
	serveErr := make(chan error, 1)
	var callbackOnce sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		callbackState := r.URL.Query().Get("state")
		if callbackState != login.state && callbackState != login.state+".onboarding_entrypoint=life_sciences" {
			http.Error(w, "OAuth state mismatch", http.StatusBadRequest)
			return
		}
		callbackOnce.Do(func() {
			if code := strings.TrimSpace(r.URL.Query().Get("error")); code != "" {
				detail := strings.TrimSpace(r.URL.Query().Get("error_description"))
				if detail == "" {
					detail = code
				}
				writeBrowserResult(w, http.StatusUnauthorized, "Login Codex recusado", detail)
				result <- browserLoginResult{err: fmt.Errorf("codex OAuth: %s", detail)}
				return
			}
			code := strings.TrimSpace(r.URL.Query().Get("code"))
			if code == "" {
				writeBrowserResult(w, http.StatusBadRequest, "Login Codex incompleto", "O callback não retornou o código de autorização.")
				result <- browserLoginResult{err: fmt.Errorf("codex OAuth callback missing authorization code")}
				return
			}
			tokens, err := login.client.exchangeWithRedirect(r.Context(), code, login.codeVerifier, login.RedirectURI)
			if err != nil {
				writeBrowserResult(w, http.StatusBadGateway, "Falha ao concluir login Codex", err.Error())
				result <- browserLoginResult{err: err}
				return
			}
			writeBrowserResult(w, http.StatusOK, "Login concluído", "Você já pode fechar esta janela e voltar ao aplicativo.")
			result <- browserLoginResult{tokens: tokens}
		})
	})
	mux.HandleFunc("/cancel", func(w http.ResponseWriter, _ *http.Request) {
		writeBrowserResult(w, http.StatusOK, "Login cancelado", "Volte ao aplicativo para tentar novamente.")
		callbackOnce.Do(func() {
			result <- browserLoginResult{err: context.Canceled}
		})
	})

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(login.listener); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()

	var out browserLoginResult
	select {
	case out = <-result:
	case err := <-serveErr:
		out.err = err
	case <-ctx.Done():
		out.err = ctx.Err()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	return out.tokens, out.err
}

func randomURLSafe(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate OAuth random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func writeBrowserResult(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="pt-BR"><head><meta charset="utf-8"><title>%s</title><style>body{font-family:system-ui;background:#111;color:#eee;display:grid;place-items:center;min-height:100vh;margin:0}.card{max-width:540px;padding:32px;border:1px solid #333;border-radius:18px;background:#1b1b1b}h1{font-size:24px}p{color:#bbb;line-height:1.5}</style></head><body><main class="card"><h1>%s</h1><p>%s</p></main></body></html>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(detail))
}
