// Command signup_probe runs the exact end-to-end acceptance path used by the
// desktop app: account registration, OAuth Device Flow, and a Grok 4.6 request.
// It also saves the minted OAuth pair to <data-root>/token-latest.json so the
// same account can be re-probed without recreating it.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"grok-desktop/internal/oauth"
	"grok-desktop/internal/register"
	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
)

func main() {
	provider := flag.String("provider", register.EmailProvider(), "luckmail | mailnest | gmail | mailtm")
	python := flag.String("python", "", "host Python executable")
	dataRoot := flag.String("data-root", "", "managed venv/extraction directory")
	attempts := flag.Int("attempts", 2, "registration attempts")
	persist := flag.Bool("persist", true, "persist the successful account in the desktop xAI pool")
	flag.Parse()

	if err := register.ValidateEnvironment(*provider); err != nil {
		fatal(err)
	}
	if *dataRoot == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			fatal(err)
		}
		*dataRoot = filepath.Join(cache, "GrokDesktop", "signup-probe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	botDir, err := register.ExtractEmbeddedBot(*dataRoot)
	if err != nil {
		fatal(err)
	}
	oauthClient := oauth.New()
	device, err := oauthClient.StartDevice(ctx)
	if err != nil {
		fatal(err)
	}
	verificationURL := device.VerificationURIComplete
	if verificationURL == "" {
		verificationURL = device.VerificationURI
	}
	type pollResult struct {
		token *oauth.TokenResponse
		err   error
	}
	pollCh := make(chan pollResult, 1)
	go func() {
		token, pollErr := oauthClient.PollDevice(ctx, device.DeviceCode, device.Interval)
		pollCh <- pollResult{token: token, err: pollErr}
	}()

	runner := register.New(*python, botDir)
	runner.DataRoot = *dataRoot
	runner.EmailProvider = *provider
	runner.MaxAttempts = *attempts
	result, err := runner.CreateAccount(ctx, verificationURL, device.UserCode, func(progress register.Progress) {
		fmt.Fprintln(os.Stderr, "signup:", progress.Step, progress.Message)
	})
	if err != nil {
		fatal(err)
	}
	if result == nil || result.Status != "success" {
		fatal(fmt.Errorf("signup %s: %s", value(result, func(r *register.Result) string { return r.Step }), value(result, func(r *register.Result) string { return r.Reason })))
	}
	polled := <-pollCh
	if polled.err != nil {
		fatal(polled.err)
	}
	email, userID := oauthClient.UserInfo(ctx, polled.token.AccessToken, oauthClient.Issuer)
	claims := oauth.ParseAccessClaims(polled.token.AccessToken)
	report := map[string]any{
		"account_created": true,
		"oauth_valid":     true,
		"model":           "grok-4.6",
		"model_valid":     false,
		"email":           first(email, result.Creds["email"]),
		"password":        result.Creds["password"],
		"provider":        result.Creds["provider"],
		"oauth_tier":      claims.Tier,
		"oauth_bot_flag":  claims.BotFlag,
		"oauth_scope":     claims.Scope,
	}
	if *persist {
		if err := persistProbeAccount(polled.token, first(email, result.Creds["email"]), userID); err != nil {
			report["persisted"] = false
			report["persist_error"] = err.Error()
		} else {
			report["persisted"] = true
		}
	}

	// Persist the OAuth pair so this account can be re-probed later.
	tokenFile := map[string]any{
		"email":         first(email, result.Creds["email"]),
		"password":      result.Creds["password"],
		"access_token":  polled.token.AccessToken,
		"refresh_token": polled.token.RefreshToken,
		"expires_in":    polled.token.ExpiresIn,
		"scope":         polled.token.Scope,
		"created_at":    time.Now().UTC().Format(time.RFC3339),
	}
	if raw, err := json.MarshalIndent(tokenFile, "", "  "); err == nil {
		tokenPath := filepath.Join(*dataRoot, "token-latest.json")
		if writeErr := os.WriteFile(tokenPath, raw, 0o600); writeErr == nil {
			fmt.Fprintln(os.Stderr, "token saved:", tokenPath)
		}
	}

	// Billing check — the official CLI provisions/reads credits here on launch.
	report["billing"] = billingStatus(ctx, polled.token.AccessToken)

	settings := store.Settings{
		Provider: store.ProviderXAI, UpstreamBase: store.DefaultUpstream,
		DefaultModel: "grok-4.6", APIMode: "responses", ClientVersion: store.DefaultClientVersion,
	}
	type probeAttempt struct {
		mode    string
		version string
		label   string
	}
	tries := []probeAttempt{
		{mode: "responses", version: store.DefaultClientVersion, label: "responses@" + store.DefaultClientVersion},
		{mode: "responses", version: "1.0.4", label: "responses@1.0.4"},
		{mode: "chat", version: "1.0.4", label: "chat@1.0.4"},
	}
	var lastErr error
	var answer string
	var winningMode string
	for round := 1; round <= 5; round++ {
		for _, try := range tries {
			settings.ClientVersion = try.version
			out, streamErr := streamOnce(ctx, polled.token.AccessToken, settings, userID, email, try.mode)
			fmt.Fprintf(os.Stderr, "probe round=%d %s err=%v\n", round, try.label, streamErr)
			if streamErr == nil && strings.TrimSpace(out) != "" {
				answer = out
				winningMode = try.label
				lastErr = nil
				break
			}
			lastErr = streamErr
		}
		if winningMode != "" {
			break
		}
		if round < 5 {
			fmt.Fprintf(os.Stderr, "probe: retrying in 30s (round %d/5)…\n", round)
			select {
			case <-ctx.Done():
			case <-time.After(30 * time.Second):
			}
		}
	}
	if winningMode == "" {
		report["model_error"] = fmt.Sprint(lastErr)
		printJSON(report)
		fatal(fmt.Errorf("Grok 4.6 probe: %w", lastErr))
	}
	report["model_valid"] = true
	report["winning_mode"] = winningMode
	report["response"] = strings.TrimSpace(answer)
	printJSON(report)
}

// persistProbeAccount keeps live acceptance runs from creating valid accounts
// that never enter the desktop pool. It intentionally does not change the
// active account; the running app or next restart may choose it normally.
func persistProbeAccount(tok *oauth.TokenResponse, email, userID string) error {
	if tok == nil || strings.TrimSpace(tok.AccessToken) == "" {
		return fmt.Errorf("empty OAuth token")
	}
	st, err := store.Open("")
	if err != nil {
		return err
	}
	defer st.Close()
	acc := oauth.AccountFromToken(tok, store.DefaultClientID, store.DefaultIssuer)
	if strings.TrimSpace(userID) != "" {
		acc.ID = userID
		acc.UserID = userID
	}
	if strings.TrimSpace(email) != "" {
		acc.Email = strings.TrimSpace(email)
		acc.Label = strings.TrimSpace(email)
	}
	acc.Provider = store.ProviderXAI
	acc.Source = "signup_probe"
	return st.UpsertAccount(acc)
}

// streamOnce performs one Grok request through the same upstream path used in
// production. mode selects the wire: "responses" (/v1/responses) or "chat"
// (/v1/chat/completions, the official CLI path).
func streamOnce(ctx context.Context, token string, settings store.Settings, userID, email, mode string) (string, error) {
	var content strings.Builder
	req := upstream.ChatRequest{
		Model: "grok-4.6", Stream: true, ReasoningEffort: "low", APIMode: mode, MaxTokens: 32,
	}
	if mode == "chat" {
		req.Messages = []upstream.ChatMessage{{Role: "user", Content: "Reply with exactly: GROK_4_6_OK"}}
	} else {
		req.Input = "Reply with exactly: GROK_4_6_OK"
	}
	err := upstream.New().StreamChat(ctx, token, settings, userID, email, req, func(event upstream.StreamEvent) {
		if event.Type == "content" {
			content.WriteString(event.Text)
		}
	})
	return content.String(), err
}

// billingStatus mirrors the official CLI's credits probe.
func billingStatus(ctx context.Context, token string) map[string]any {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://cli-chat-proxy.grok.com/v1/billing?format=credits", nil)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-grok-client-version", store.DefaultClientVersion)
	req.Header.Set("x-grok-client-identifier", store.DefaultClientIdentifier)
	req.Header.Set("x-grok-client-surface", store.DefaultClientSurface)
	req.Header.Set("User-Agent", "grok/"+store.DefaultClientVersion)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return map[string]any{"status": resp.StatusCode, "body": string(body)}
}

func value(result *register.Result, pick func(*register.Result) string) string {
	if result == nil {
		return ""
	}
	return pick(result)
}

func first(values ...string) string {
	for _, item := range values {
		if strings.TrimSpace(item) != "" {
			return item
		}
	}
	return ""
}

func printJSON(value any) {
	raw, _ := json.MarshalIndent(value, "", "  ")
	fmt.Println(string(raw))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
