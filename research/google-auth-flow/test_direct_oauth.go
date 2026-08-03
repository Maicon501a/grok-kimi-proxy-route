package main

// Test: Full HTTP-direct OAuth attempt (EXPECTED TO FAIL at sign-in)
// Run: go run research/google-auth-flow/test_direct_oauth.go
//
// This demonstrates why the Google sign-in step cannot be automated with HTTP alone.
// It attempts to follow redirects and extract tokens from HTML, which will fail
// because Google requires JavaScript execution and session cookies.

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	clientID       = "626581754197-v82pavblj7tgk6ap9ouqbi9lv821l6qo.apps.googleusercontent.com"
)

func main() {
	// Step 1: Build OAuth URL (same as browser)
	authURL := googleAuthURL + "?" + url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {"http://127.0.0.1:61120/callback"},
		"response_type":         {"code"},
		"scope":                 {"email profile openid"},
		"code_challenge":        {"test_challenge_not_real"},
		"code_challenge_method": {"S256"},
		"access_type":           {"offline"},
		"prompt":                {"select_account"},
	}.Encode()

	fmt.Println("=== STEP 1: Request OAuth authorization URL ===")
	fmt.Printf("GET %s\n\n", authURL)

	client := &http.Client{
		Timeout: 45 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			fmt.Printf("  ↳ Redirect: %s %s\n", req.Method, req.URL.String())
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}

	req, _ := http.NewRequest(http.MethodGet, authURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("FAIL: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	body := string(b)

	fmt.Printf("\nFinal URL: %s\n", resp.Request.URL.String())
	fmt.Printf("Status: %d\n", resp.StatusCode)
	fmt.Printf("Content-Type: %s\n", resp.Header.Get("Content-Type"))
	fmt.Printf("Body length: %d bytes\n\n", len(body))

	// Step 2: Try to find form tokens in the HTML (will fail without JS)
	fmt.Println("=== STEP 2: Extract tokens from HTML (expected to fail) ===")

	// Look for identifiertoken
	re := regexp.MustCompile(`name="identifiertoken"[^>]*value="([^"]+)"`)
	m := re.FindStringSubmatch(body)
	if m != nil {
		fmt.Printf("  Found identifiertoken: %s...\n", truncate(m[1], 30))
	} else {
		fmt.Println("  identifiertoken: NOT FOUND (Google hides this behind JS)")
	}

	// Look for data-initial-sign-in-data
	re2 := regexp.MustCompile(`data-initial-sign-in-data="([^"]+)"`)
	m2 := re2.FindStringSubmatch(body)
	if m2 != nil {
		fmt.Printf("  Found data-initial-sign-in-data: %s...\n", truncate(m2[1], 50))
	} else {
		fmt.Println("  data-initial-sign-in-data: NOT FOUND (requires JS to render)")
	}

	// Look for dsh token
	re3 := regexp.MustCompile(`dsh=([A-Za-z0-9_%-]+)`)
	m3 := re3.FindStringSubmatch(resp.Request.URL.String())
	if m3 != nil {
		fmt.Printf("  Found dsh token: %s\n", m3[1])
	} else {
		fmt.Println("  dsh token: NOT FOUND")
	}

	// Check if we got the sign-in page
	if strings.Contains(body, "signin") || strings.Contains(body, "identifier") {
		fmt.Println("\n  → Google returned the sign-in page. Without JS execution, we cannot proceed.")
	}
	if strings.Contains(body, "javascript") || strings.Contains(body, "script") {
		fmt.Println("  → Page contains JavaScript. Must execute to get valid form tokens.")
	}

	fmt.Println("\n=== CONCLUSION ===")
	fmt.Println("HTTP-direct sign-in to Google is NOT feasible because:")
	fmt.Println("1. Google serves a JS-heavy page that renders tokens dynamically")
	fmt.Println("2. The 'at', 'dsh', 'part', 'rapt' tokens are session-bound and time-limited")
	fmt.Println("3. Missing cookies (SID, HSID, SSID, etc.) will trigger CAPTCHA or block")
	fmt.Println("4. Even with cookies, the browserinfo fingerprinting endpoint must be called")
	fmt.Println("5. Google actively detects and blocks non-browser user agents")
	fmt.Println("")
	fmt.Println("RECOMMENDATION: Use refresh_token flow for VM deployment.")
	fmt.Println("See test_refresh_token.go for the working HTTP-direct alternative.")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
