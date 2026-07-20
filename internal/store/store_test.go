package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// isolateHome points the OS home dir at an empty temp dir so Open's
// SyncFromGrokCLI does not import real ~/.grok/auth.json accounts into tests.
func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
}

func TestExhaustedFlag(t *testing.T) {
	a := Account{ExhaustedAt: time.Now().UTC()}
	if !a.Exhausted() {
		t.Fatal("expected exhausted when ExhaustedAt is set")
	}
	a.ExhaustedAt = time.Time{}
	if a.Exhausted() {
		t.Fatal("expected not exhausted after clear")
	}
}

func TestMarkAndClearExhausted(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	old := Account{
		ID:          "acc1",
		Label:       "t",
		AccessToken: "tok",
		ClientID:    DefaultClientID,
		Issuer:      DefaultIssuer,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := st.UpsertAccount(old); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkExhausted("acc1", "quota test"); err != nil {
		t.Fatal(err)
	}
	got, ok := st.GetAccount("acc1")
	if !ok {
		t.Fatal("missing account")
	}
	if !got.Exhausted() {
		t.Fatal("should be exhausted after MarkExhausted")
	}
	if got.ExhaustReason != "quota test" {
		t.Fatalf("reason=%q", got.ExhaustReason)
	}
	pub := st.PublicAccounts()
	if len(pub) != 1 {
		t.Fatalf("want 1 account, got %d", len(pub))
	}
	if pub[0]["exhausted"] != true {
		t.Fatalf("exhausted flag: %v", pub[0]["exhausted"])
	}
	if err := st.ClearExhausted("acc1"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetAccount("acc1")
	if got.Exhausted() {
		t.Fatal("should have recovered after ClearExhausted")
	}
	pub = st.PublicAccounts()
	if pub[0]["exhausted"] != false {
		t.Fatal("clear failed")
	}
}

func TestRecordRequestCost(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.UpsertAccount(Account{
		ID: "a1", Label: "one", AccessToken: "x",
		ClientID: DefaultClientID, Issuer: DefaultIssuer,
	})
	err = st.RecordRequest(RequestSample{
		ID: "r1", At: time.Now().UTC().Format(time.RFC3339),
		AccountID: "a1", Model: DefaultModel,
		PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500,
		CostUSD: 0.005,
	})
	if err != nil {
		t.Fatal(err)
	}
	u := st.UsageSnapshot()
	if u["a1"].CostUSD <= 0 || u["_global"].CostUSD <= 0 {
		t.Fatalf("cost not recorded: %+v", u)
	}
	if u["a1"].Requests != 1 {
		t.Fatalf("requests=%d", u["a1"].Requests)
	}
}

func TestSettingsPersistRoundtrip(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.Settings().ProxyAPIKey != "" {
		t.Fatal("proxy api key should default empty")
	}
	// settings file exists after update
	if err := st.UpdateSettings(func(s *Settings) { s.ProxyAPIKey = "k-test" }); err != nil {
		t.Fatal(err)
	}
	if st.Settings().ProxyAPIKey != "k-test" {
		t.Fatal("update failed")
	}
	// reload
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if st2.Settings().ProxyAPIKey != "k-test" {
		t.Fatal("persist failed")
	}
	// ensure settings.json path
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil {
		t.Fatal(err)
	}
}

func TestPublicAccountsBadges(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// expired access, no refresh → expired badge
	_ = st.UpsertAccount(Account{
		ID: "sso1", Label: "sso", AccessToken: "x",
		ExpiresAt: time.Now().UTC().Add(-time.Hour),
		ClientID:  DefaultClientID, Issuer: DefaultIssuer,
	})
	// expired access, has refresh → has_refresh badge
	_ = st.UpsertAccount(Account{
		ID: "oauth1", Label: "oauth", AccessToken: "y", RefreshToken: "rt",
		ExpiresAt: time.Now().UTC().Add(-time.Hour),
		ClientID:  DefaultClientID, Issuer: DefaultIssuer,
	})
	// exhausted
	_ = st.UpsertAccount(Account{
		ID: "ex1", Label: "ex", AccessToken: "z",
		ExhaustedAt: time.Now().UTC(),
		ClientID:    DefaultClientID, Issuer: DefaultIssuer,
	})
	pub := st.PublicAccounts()
	byID := map[string]map[string]any{}
	for _, m := range pub {
		byID[m["id"].(string)] = m
	}
	if byID["sso1"]["expired"] != true {
		t.Fatalf("sso expired: %+v", byID["sso1"])
	}
	if byID["sso1"]["has_refresh"] != false {
		t.Fatalf("sso has_refresh: %+v", byID["sso1"])
	}
	if byID["oauth1"]["has_refresh"] != true {
		t.Fatal("has_refresh")
	}
	if byID["ex1"]["exhausted"] != true {
		t.Fatal("exhausted")
	}
}

func TestAuthDeniedLifecycle(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.UpsertAccount(Account{
		ID: "d1", Label: "denied", AccessToken: "x",
		ClientID: DefaultClientID, Issuer: DefaultIssuer,
	})
	if _, err := st.MarkAuthDenied("d1", "Forbidden: Access to the chat endpoint is denied"); err != nil {
		t.Fatal(err)
	}
	got, ok := st.GetAccount("d1")
	if !ok || !got.AuthDenied() {
		t.Fatal("expected auth denied")
	}
	if got.Usable() {
		t.Fatal("auth-denied account must not be usable")
	}
	pub := st.PublicAccounts()
	var row map[string]any
	for _, m := range pub {
		if m["id"] == "d1" {
			row = m
			break
		}
	}
	if row["auth_denied"] != true {
		t.Fatalf("%+v", row)
	}
	if err := st.ClearAuthState("d1"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetAccount("d1")
	if got.AuthDenied() {
		t.Fatal("clear should remove auth denied")
	}
}

func TestInflightTracker(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.UpsertAccount(Account{
		ID: "a1", Label: "one", AccessToken: "x",
		ClientID: DefaultClientID, Issuer: DefaultIssuer,
	})
	_ = st.UpsertAccount(Account{
		ID: "a2", Label: "two", AccessToken: "y",
		ClientID: DefaultClientID, Issuer: DefaultIssuer,
	})
	count := func(id string) int64 {
		st.mu.RLock()
		defer st.mu.RUnlock()
		return st.accounts[id].requestCount
	}
	ctx, tr := WithInflightTracker(context.Background())
	// Two Incs of a1 (initial + force-refresh), one of a2 (rotation).
	st.IncAccountRequestCount("a1")
	TrackInflight(ctx, "a1")
	st.IncAccountRequestCount("a1")
	TrackInflight(ctx, "a1")
	st.IncAccountRequestCount("a2")
	TrackInflight(ctx, "a2")
	if count("a1") != 2 || count("a2") != 1 {
		t.Fatalf("counts a1=%d a2=%d", count("a1"), count("a2"))
	}
	// Rotation: untrack a2 and Dec it immediately.
	if n := UntrackInflight(ctx, "a2"); n != 1 {
		t.Fatalf("untracked %d, want 1", n)
	}
	st.DecAccountRequestCount("a2")
	// DecAll releases exactly what remains tracked, once.
	tr.DecAll(st)
	tr.DecAll(st)
	if count("a1") != 0 || count("a2") != 0 {
		t.Fatalf("after DecAll a1=%d a2=%d", count("a1"), count("a2"))
	}
}
