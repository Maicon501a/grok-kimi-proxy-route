// probe-kimi-refresh: repeat Kimi logins via Google refresh_token (pure HTTP)
// and print the resulting user ids — proves whether Kimi binds the Google
// account consistently or auto-registers a new account per login.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"grok-desktop/internal/kimi"
)

func jwtSub(t string) (sub, abstract string) {
	parts := splitDot(t)
	if len(parts) < 2 {
		return "", ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	sub, _ = m["sub"].(string)
	abstract, _ = m["abstract_user_id"].(string)
	return
}

func splitDot(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == '.' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(c)
		}
	}
	return append(out, cur)
}

func main() {
	b, err := os.ReadFile("tmp_probe/google_tokens.json")
	if err != nil {
		fmt.Fprintln(os.Stderr, "read google_tokens.json:", err)
		os.Exit(1)
	}
	var tok map[string]string
	json.Unmarshal(b, &tok)
	rt := tok["refresh_token"]
	if rt == "" {
		fmt.Fprintln(os.Stderr, "no refresh_token")
		os.Exit(1)
	}
	for i := 1; i <= 3; i++ {
		sess, err := kimi.LoginWithGoogleRefresh(rt)
		if err != nil {
			fmt.Printf("[!] login %d: %v\n", i, err)
			continue
		}
		gsub, _ := jwtSub(sess.IDToken)
		ksub, kabs := jwtSub(sess.AccessToken)
		fmt.Printf("[login %d] google_sub=%s kimi_sub=%s kimi_abstract=%s email=%s\n",
			i, gsub, ksub, kabs, sess.Email)
	}
}
