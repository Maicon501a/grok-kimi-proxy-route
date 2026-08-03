package main

// Test: Kimi Login with Google ID Token
// Run: go run research/google-auth-flow/test_kimi_login.go
//
// This tests POSTing a Google id_token to Kimi's /api/auth/login/google endpoint.
// Requires a valid Google id_token (obtained via browser login or refresh_token).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const kimiURL = "https://www.kimi.com"

func main() {
	idToken := os.Getenv("GOOGLE_ID_TOKEN")
	if idToken == "" {
		fmt.Println("Set GOOGLE_ID_TOKEN env var with a valid Google id_token")
		fmt.Println("Get one by:")
		fmt.Println("  1. Running test_refresh_token.go with a valid refresh_token")
		fmt.Println("  2. Or extracting from browser login callback")
		os.Exit(1)
	}

	body, _ := json.Marshal(map[string]any{"code": idToken})
	req, err := http.NewRequest(http.MethodPost, kimiURL+"/api/auth/login/google", bytes.NewReader(body))
	if err != nil {
		fmt.Printf("FAIL: create request: %v\n", err)
		os.Exit(1)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", kimiURL)
	req.Header.Set("Referer", kimiURL+"/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36")
	req.Header.Set("x-msh-platform", "windows")
	req.Header.Set("x-msh-version", "3.1.0")

	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("FAIL: do request: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode >= 400 {
		fmt.Printf("FAIL: HTTP %d\nBody: %s\n", resp.StatusCode, string(b))
		os.Exit(1)
	}

	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		fmt.Printf("FAIL: json parse: %v\nBody: %s\n", err, string(b))
		os.Exit(1)
	}

	access, _ := data["access_token"].(string)
	if access == "" {
		access, _ = data["accessToken"].(string)
	}
	refresh, _ := data["refresh_token"].(string)
	if refresh == "" {
		refresh, _ = data["refreshToken"].(string)
	}

	if access == "" {
		fmt.Printf("FAIL: response missing access_token\nBody: %s\n", string(b))
		os.Exit(1)
	}

	fmt.Printf("SUCCESS! Got Kimi session:\n")
	fmt.Printf("  access_token:  %s...\n", truncate(access, 40))
	fmt.Printf("  refresh_token: %s...\n", truncate(refresh, 40))

	if u, ok := data["user"].(map[string]any); ok {
		if email, ok := u["email"].(string); ok {
			fmt.Printf("  email:         %s\n", email)
		}
		if id, ok := u["id"].(string); ok {
			fmt.Printf("  user_id:       %s\n", id)
		}
	}
	fmt.Printf("\nStore refresh_token for future use. Kimi refresh endpoint:\n")
	fmt.Printf("  GET %s/api/auth/token/refresh\n", kimiURL)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
