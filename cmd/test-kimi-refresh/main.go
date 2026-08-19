package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"grok-desktop/internal/kimi"
	"grok-desktop/internal/store"
)

func main() {
	emailFlag := flag.String("email", "", "Kimi account email")
	flag.Parse()
	email := strings.TrimSpace(*emailFlag)
	if email == "" {
		fail("use -email <email>")
	}
	st, err := store.Open("")
	if err != nil {
		fail("open store: %v", err)
	}
	defer st.Close()
	for _, acc := range st.ListAccountsForProvider(store.ProviderKimiWork) {
		if !strings.EqualFold(strings.TrimSpace(acc.Email), email) {
			continue
		}
		s, err := kimi.RefreshAccessToken(acc.RefreshToken)
		if err != nil {
			fail("refresh failed: %v", err)
		}
		fmt.Printf("REFRESH_OK email=%s user_id=%s exp=%d new_refresh=%v\n", acc.Email, s.UserID, s.Exp, s.RefreshToken != acc.RefreshToken)
		return
	}
	fail("Kimi account not found: %s", email)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
