// Command test-kimi-logoff executes Kimi's real consumer-account logoff
// endpoint for one configured Kimi Work account, then leaves its local row
// exhausted so the desktop app exercises its normal automatic re-login path.
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
	emailFlag := flag.String("email", "", "configured Kimi account email to log off")
	flag.Parse()
	email := strings.TrimSpace(*emailFlag)
	if email == "" {
		fatal("use -email <configured Kimi email>")
	}

	st, err := store.Open("")
	if err != nil {
		fatal("open store: %v", err)
	}
	defer st.Close()

	var target *store.Account
	for _, acc := range st.ListAccountsForProvider(store.ProviderKimiWork) {
		if strings.EqualFold(strings.TrimSpace(acc.Email), email) {
			copy := acc
			target = &copy
			break
		}
	}
	if target == nil {
		fatal("configured Kimi account not found: %s", email)
	}
	if !kimi.HasWebSession(target.AccessToken, target.RefreshToken) {
		fatal("account %s has no web JWT/refresh session; remote logoff was not attempted", email)
	}
	if strings.TrimSpace(target.GoogleRefreshToken) == "" {
		fatal("account %s has no Google refresh token; remote logoff was not attempted because automatic re-login could not be verified", email)
	}

	mode := "web_session"
	if _, err := kimi.LogoffWithSession(target.AccessToken, target.RefreshToken); err != nil {
		webErr := err
		gl, gerr := kimi.LoginWithGoogleRefresh(target.GoogleRefreshToken)
		if gerr != nil || gl == nil || strings.TrimSpace(gl.AccessToken) == "" {
			if gerr == nil {
				gerr = fmt.Errorf("Google refresh returned no Kimi access token")
			}
			fatal("web logoff failed (%v); Google session recovery failed for %s: %v", webErr, email, gerr)
		}
		if next := strings.TrimSpace(gl.GoogleRefreshToken); next != "" && next != target.GoogleRefreshToken {
			target.GoogleRefreshToken = next
			if err := st.UpsertAccount(*target); err != nil {
				fatal("could not persist rotated Google refresh token: %v", err)
			}
		}
		if err := kimi.LogoffAccount(gl.AccessToken); err != nil {
			fatal("web logoff failed (%v); recovered Google session logoff failed for %s: %v", webErr, email, err)
		}
		mode = "google_refresh"
	}
	if _, err := st.MarkExhausted(target.ID, "manual Kimi remote-logoff endpoint test; awaiting automatic re-login"); err != nil {
		fatal("remote logoff succeeded, but marking the local row exhausted failed: %v", err)
	}
	fmt.Printf("REMOTE_LOGOFF_OK account_id=%s email=%s mode=%s local_state=exhausted\n", target.ID, target.Email, mode)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
