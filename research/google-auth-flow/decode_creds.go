package main

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
)

const oauthXorKey = byte(0x5A)

func xorDecode(enc []byte) string {
	out := make([]byte, len(enc))
	for i, b := range enc {
		out[i] = b ^ oauthXorKey
	}
	return string(out)
}

func pkce() (verifier, challenge string) {
	b := make([]byte, 32)
	// fixed for reproducibility in research
	for i := range b {
		b[i] = byte(i)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

func main() {
	clientID := xorDecode([]byte{
		0x6c, 0x68, 0x6c, 0x6f, 0x62, 0x6b, 0x6d, 0x6f, 0x6e, 0x6b, 0x63, 0x6d, 0x77,
		0x2c, 0x62, 0x68, 0x2a, 0x3b, 0x2c, 0x38, 0x36, 0x30, 0x6d, 0x2e, 0x3d, 0x31,
		0x6c, 0x3b, 0x2a, 0x63, 0x35, 0x2f, 0x2b, 0x38, 0x33, 0x63, 0x36, 0x2c, 0x62,
		0x68, 0x6b, 0x36, 0x6c, 0x2b, 0x35, 0x74, 0x3b, 0x2a, 0x2a, 0x29, 0x74, 0x3d,
		0x35, 0x35, 0x3d, 0x36, 0x3f, 0x2f, 0x29, 0x3f, 0x28, 0x39, 0x35, 0x34, 0x2e,
		0x3f, 0x34, 0x2e, 0x74, 0x39, 0x35, 0x37,
	})
	clientSecret := os.Getenv("GOOGLE_CLIENT_SECRET")

	fmt.Println("client_id:", clientID)
	if clientSecret == "" {
		fmt.Println("client_secret: <set GOOGLE_CLIENT_SECRET to inspect locally>")
	} else {
		fmt.Println("client_secret: <loaded from GOOGLE_CLIENT_SECRET>")
	}

	_, challenge := pkce()
	authURL := "https://accounts.google.com/o/oauth2/v2/auth" + "?" + url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {"http://127.0.0.1:61120/callback"},
		"response_type":         {"code"},
		"scope":                 {"email profile openid"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"access_type":           {"offline"},
		"prompt":                {"select_account"},
	}.Encode()

	fmt.Println("\nauth_url:", authURL)
}
