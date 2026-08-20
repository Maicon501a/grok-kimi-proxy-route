// probe-kimi-bridge: E2E test of kimi.LoginWithGoogleBridge — the production
// integration of the browser-as-transport Google login.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"grok-desktop/internal/kimi"
)

func main() {
	email := flag.String("email", "", "google account email")
	password := flag.String("password", "", "google account password")
	flag.Parse()
	if *email == "" || *password == "" {
		fmt.Fprintln(os.Stderr, "need -email and -password")
		os.Exit(1)
	}
	root, _ := os.Getwd()
	n := 2 // two sequential logins on the SAME shared Chrome process
	for i := 1; i <= n; i++ {
		t0 := time.Now()
		sess, err := kimi.LoginWithGoogleBridge(root, *email, *password, 4*time.Minute)
		if err != nil {
			fmt.Fprintln(os.Stderr, "[!] LoginWithGoogleBridge:", err)
			os.Exit(1)
		}
		fmt.Printf("[login %d] %s — user=%s email=%s source=%s googleRT=%v\n",
			i, time.Since(t0).Round(time.Second), sess.UserID, sess.Email, sess.Source, sess.GoogleRefreshToken != "")
		if i == n {
			b, _ := json.MarshalIndent(sess, "", "  ")
			os.WriteFile("tmp_probe/kimi_bridge_session.json", b, 0o600)
		}
	}
	kimi.CloseGoogleBridge()
	fmt.Println("\n=== LoginWithGoogleBridge shared-browser E2E OK ===")
}
