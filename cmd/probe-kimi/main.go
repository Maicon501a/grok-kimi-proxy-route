package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"grok-desktop/internal/proxyhttp"
	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
)

func main() {
	st, err := store.Open("")
	if err != nil {
		fatal("store: %v", err)
	}
	defer st.Close()

	// Show Kimi accounts in store
	kimi := st.ListAccountsForProvider(store.ProviderKimiWork)
	fmt.Printf("kimi_accounts=%d\n", len(kimi))
	for _, a := range kimi {
		fmt.Printf("  - id=%s label=%q email=%q usable=%v denied=%v key=%v\n",
			a.ID, a.Label, a.Email, a.Usable(), a.AuthDenied(), a.BearerToken() != "")
	}

	up := upstream.New()
	ensure := func(ctx context.Context) (string, *store.Account, store.Settings, error) {
		settings := st.Settings()
		if rp := proxyhttp.RouteProviderFrom(ctx); rp != "" {
			settings = settings.WithProvider(rp)
		}
		if settings.IsKimiWork() {
			acc, ok := st.PreferUsableAccountForProvider(store.ProviderKimiWork)
			if !ok || acc == nil {
				return "", nil, settings, fmt.Errorf("nenhuma conta Kimi Work")
			}
			return acc.BearerToken(), acc, settings, nil
		}
		acc, ok := st.PreferUsableAccountForProvider(store.ProviderXAI)
		if !ok || acc == nil {
			return "", nil, settings, fmt.Errorf("nenhuma conta xAI")
		}
		return acc.AccessToken, acc, settings, nil
	}

	srv := proxyhttp.New(st, up, ensure)
	listen := "127.0.0.1:8799"
	if err := srv.Start(listen); err != nil {
		fatal("listen: %v", err)
	}
	defer srv.Stop(context.Background())
	base := "http://" + srv.Addr()
	fmt.Println("proxy", base)

	// 1) models
	modelsBody, code := get(base + "/v1/models")
	fmt.Printf("GET /v1/models status=%d\n", code)
	var models struct {
		Data []struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
		} `json:"data"`
		Route string `json:"route"`
	}
	_ = json.Unmarshal(modelsBody, &models)
	fmt.Printf("route=%q count=%d\n", models.Route, len(models.Data))
	hasKimi, hasGrok := false, false
	for _, m := range models.Data {
		if m.ID == "kimi-for-coding" || m.Provider == store.ProviderKimiWork {
			hasKimi = true
		}
		if m.ID == "grok-4.6" {
			hasGrok = true
		}
		if m.Provider == store.ProviderKimiWork || m.ID == "grok-4.6" || m.ID == "kimi-for-coding" {
			fmt.Printf("  model %s provider=%s\n", m.ID, m.Provider)
		}
	}
	fmt.Printf("has_grok=%v has_kimi=%v\n", hasGrok, hasKimi)

	// 2) chat completions with kimi model
	payload := map[string]any{
		"model":  "kimi-for-coding",
		"stream": false,
		"messages": []map[string]string{
			{"role": "user", "content": "Responda só com a palavra OK"},
		},
	}
	b, _ := json.Marshal(payload)
	chatBody, chatCode, chatErr := post(base+"/v1/chat/completions", b)
	fmt.Printf("POST /v1/chat/completions model=kimi-for-coding status=%d err=%v\n", chatCode, chatErr)
	if len(chatBody) > 800 {
		fmt.Println(string(chatBody[:800]), "...")
	} else {
		fmt.Println(string(chatBody))
	}

	// 3) wrong endpoint for kimi should 400
	_, respCode, _ := post(base+"/v1/responses", b)
	fmt.Printf("POST /v1/responses with kimi model status=%d (expect 400)\n", respCode)

	if !hasKimi {
		os.Exit(2)
	}
	if chatCode < 200 || chatCode >= 300 {
		os.Exit(3)
	}
	fmt.Println("KIMI_OK")
}

func get(url string) ([]byte, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return []byte(err.Error()), 0
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode
}

func post(url string, body []byte) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer local")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode, nil
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(1)
}
