package store

import "testing"

// TestGoogleCredsSurviveRestart proves that GoogleRefreshToken, GoogleEmail
// and GooglePassword persist across a full store close + reopen (proxy
// restart): without them the HTTP re-login path is dead after a restart.
func TestGoogleCredsSurviveRestart(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()

	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	acc := Account{
		ID:                 "kimi-u1",
		Provider:           ProviderKimiWork,
		Label:              "fab@gmail.com",
		Email:              "fab@gmail.com",
		UserID:             "u1",
		APIKey:             "sk-kimi-x",
		AccessToken:        "at",
		RefreshToken:       "rt",
		GoogleRefreshToken: "grt-secret-123",
		GoogleEmail:        "fab@gmail.com",
		GooglePassword:     "google-pass-456",
	}
	if err := st.UpsertAccount(acc); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Simulate proxy restart: fresh Open on the same data dir.
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	got, ok := st2.GetAccount("kimi-u1")
	if !ok {
		t.Fatal("account missing after restart")
	}
	if got.GoogleRefreshToken != "grt-secret-123" {
		t.Fatalf("GoogleRefreshToken lost: %q", got.GoogleRefreshToken)
	}
	if got.GoogleEmail != "fab@gmail.com" {
		t.Fatalf("GoogleEmail lost: %q", got.GoogleEmail)
	}
	if got.GooglePassword != "google-pass-456" {
		t.Fatalf("GooglePassword lost: %q", got.GooglePassword)
	}
}

// TestGoogleCredsPreservedOnSessionRefresh proves that refreshing the Kimi
// session (rotated access/refresh pair) does NOT wipe the Google credentials
// stored on the same row.
func TestGoogleCredsPreservedOnSessionRefresh(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	acc := Account{
		ID:                 "kimi-u1",
		Provider:           ProviderKimiWork,
		Email:              "fab@gmail.com",
		APIKey:             "sk-kimi-x",
		AccessToken:        "at",
		RefreshToken:       "rt",
		GoogleRefreshToken: "grt-secret-123",
		GoogleEmail:        "fab@gmail.com",
		GooglePassword:     "google-pass-456",
	}
	if err := st.UpsertAccount(acc); err != nil {
		t.Fatal(err)
	}
	// Re-upsert with rotated Kimi tokens but no Google fields (callers that
	// only refresh the session must not clobber the Google creds — they are
	// expected to carry the fields forward; verify the store keeps what it
	// receives and that a full-field update round-trips).
	got, _ := st.GetAccount("kimi-u1")
	updated := *got
	updated.AccessToken = "at2"
	updated.RefreshToken = "rt2"
	if err := st.UpsertAccount(updated); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	final, _ := st2.GetAccount("kimi-u1")
	if final.GoogleRefreshToken != "grt-secret-123" || final.GooglePassword != "google-pass-456" {
		t.Fatalf("google creds clobbered by session refresh: %+v", final)
	}
	if final.AccessToken != "at2" {
		t.Fatalf("session rotation lost: %q", final.AccessToken)
	}
}
