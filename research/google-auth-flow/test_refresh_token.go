package main

// Test: Google OAuth Refresh Token Flow
// Run: go run research/google-auth-flow/test_refresh_token.go
//
// This tests if a stored Google refresh_token can be used to get a fresh
// id_token without any browser interaction. This is the KEY to VM deployment.

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
	googleTokenURL = "https://oauth2.googleapis.com/token"
	clientID       = "626581754197-v82pavblj7tgk6ap9ouqbi9lv821l6qo.apps.googleusercontent.com"
)

func main() {
	refreshToken := os.Getenv("GOOGLE_REFRESH_TOKEN")
	clientSecret := os.Getenv("GOOGLE_CLIENT_SECRET")
	if refreshToken == "" {
		fmt.Println("Set GOOGLE_REFRESH_TOKEN env var with a valid Google refresh token")
		fmt.Println("To get one: run the full browser login once and extract refresh_token")
		fmt.Println("from the /token response.")
		os.Exit(1)
	}
	if clientSecret == "" {
		fmt.Println("Set GOOGLE_CLIENT_SECRET env var with the OAuth client secret")
		os.Exit(1)
	}

	form := url.Values{
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"grant_type":    {"refresh_token"},
	}

	req, err := http.NewRequest(http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		fmt.Printf("FAIL: create request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

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

	idToken, _ := data["id_token"].(string)
	accessToken, _ := data["access_token"].(string)
	expiresIn, _ := data["expires_in"].(float64)

	if idToken == "" {
		fmt.Printf("FAIL: response missing id_token\nBody: %s\n", string(b))
		os.Exit(1)
	}

	fmt.Printf("SUCCESS! Got fresh tokens without browser:\n")
	fmt.Printf("  access_token: %s...\n", truncate(accessToken, 40))
	fmt.Printf("  id_token:     %s...\n", truncate(idToken, 40))
	fmt.Printf("  expires_in:   %.0f seconds\n", expiresIn)
	fmt.Printf("\nThis id_token can now be POSTed to Kimi /api/auth/login/google\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
