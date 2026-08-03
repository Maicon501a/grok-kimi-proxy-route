package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	clientID    = "626581754197-v82pavblj7tgk6ap9ouqbi9lv821l6qo.apps.googleusercontent.com"
	googleToken = "https://oauth2.googleapis.com/token"
	kimiLogin   = "https://www.kimi.com/api/auth/login/google"
)

func main() {
	rt := strings.TrimSpace(os.Getenv("GOOGLE_REFRESH_TOKEN"))
	if strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_SECRET")) == "" {
		fmt.Println("GOOGLE_CLIENT_SECRET required")
		os.Exit(1)
	}
	if rt == "" {
		fmt.Println("GOOGLE_REFRESH_TOKEN required")
		os.Exit(1)
	}
	n := 8
	ok := 0
	var lastID, lastAccess, lastRefresh string
	for i := 1; i <= n; i++ {
		fmt.Printf("\n=== RUN %d/%d ===\n", i, n)
		idToken, err := googleRefresh(rt)
		if err != nil {
			fmt.Printf("FAIL google: %v\n", err)
			continue
		}
		if idToken == lastID {
			fmt.Println("WARN: same id_token as previous run")
		} else {
			fmt.Printf("google id_token ok len=%d changed=%v\n", len(idToken), lastID != "")
		}
		lastID = idToken

		access, refresh, email, err := kimiLoginWithID(idToken)
		if err != nil {
			fmt.Printf("FAIL kimi: %v\n", err)
			continue
		}
		fmt.Printf("kimi ok email=%q access_len=%d refresh_len=%d\n", email, len(access), len(refresh))
		if lastAccess != "" && access == lastAccess {
			fmt.Println("NOTE: same kimi access_token as previous run")
		}
		if lastRefresh != "" && refresh == lastRefresh {
			fmt.Println("NOTE: same kimi refresh_token as previous run")
		}
		lastAccess, lastRefresh = access, refresh
		ok++
		time.Sleep(1500 * time.Millisecond)
	}
	fmt.Printf("\n=== SUMMARY: %d/%d success ===\n", ok, n)
	if ok < n {
		os.Exit(1)
	}
}

func googleRefresh(rt string) (string, error) {
	form := url.Values{
		"refresh_token": {rt},
		"client_id":     {clientID},
		"client_secret": {strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_SECRET"))},
		"grant_type":    {"refresh_token"},
	}
	req, err := http.NewRequest(http.MethodPost, googleToken, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 45 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(b), 240))
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		return "", err
	}
	id, _ := data["id_token"].(string)
	if id == "" {
		return "", fmt.Errorf("missing id_token: %s", truncate(string(b), 240))
	}
	return id, nil
}

func kimiLoginWithID(idToken string) (access, refresh, email string, err error) {
	body, _ := json.Marshal(map[string]any{"code": idToken})
	req, err := http.NewRequest(http.MethodPost, kimiLogin, strings.NewReader(string(body)))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://www.kimi.com")
	req.Header.Set("Referer", "https://www.kimi.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36")
	req.Header.Set("x-msh-platform", "windows")
	req.Header.Set("x-msh-version", "3.1.0")
	resp, err := (&http.Client{Timeout: 45 * time.Second}).Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(b), 300))
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		return "", "", "", err
	}
	access, _ = data["access_token"].(string)
	if access == "" {
		access, _ = data["accessToken"].(string)
	}
	refresh, _ = data["refresh_token"].(string)
	if refresh == "" {
		refresh, _ = data["refreshToken"].(string)
	}
	if u, ok := data["user"].(map[string]any); ok {
		email, _ = u["email"].(string)
	}
	if access == "" {
		return "", "", "", fmt.Errorf("missing access_token: %s", truncate(string(b), 300))
	}
	return access, refresh, email, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
