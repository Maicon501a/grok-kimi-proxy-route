package store

import "testing"

func kimiAcc(id, email, googleEmail, googleRT string) Account {
	return Account{
		ID:                 id,
		Provider:           ProviderKimiWork,
		Email:              email,
		GoogleEmail:        googleEmail,
		GoogleRefreshToken: googleRT,
		APIKey:             "sk-kimi-" + id,
	}
}

func TestUpsertKimiDedupByGoogleRefreshToken(t *testing.T) {
	isolateHome(t)
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// same Google account re-registered after logoff → new Kimi user id
	old := kimiAcc("kimi-userA", "fab@gmail.com", "fab@gmail.com", "grt-123")
	if err := st.UpsertAccount(old); err != nil {
		t.Fatal(err)
	}
	fresh := kimiAcc("kimi-userB", "fab@gmail.com", "fab@gmail.com", "grt-123")
	if err := st.UpsertAccount(fresh); err != nil {
		t.Fatal(err)
	}
	accs := st.ListAccountsForProvider(ProviderKimiWork)
	if len(accs) != 1 || accs[0].ID != "kimi-userB" {
		t.Fatalf("expected only kimi-userB, got %+v", accs)
	}
}

func TestUpsertKimiDedupByNormalizedGmail(t *testing.T) {
	isolateHome(t)
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// gmail dot/tag variants are the same Google account
	a := kimiAcc("kimi-u1", "Fab.Ricio+tag@gmail.com", "", "")
	if err := st.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}
	b := kimiAcc("kimi-u2", "fabricio@gmail.com", "", "")
	if err := st.UpsertAccount(b); err != nil {
		t.Fatal(err)
	}
	accs := st.ListAccountsForProvider(ProviderKimiWork)
	if len(accs) != 1 || accs[0].ID != "kimi-u2" {
		t.Fatalf("expected only kimi-u2, got %+v", accs)
	}
}

func TestUpsertKimiKeepsDistinctAccounts(t *testing.T) {
	isolateHome(t)
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := kimiAcc("kimi-u1", "ana@gmail.com", "ana@gmail.com", "grt-A")
	b := kimiAcc("kimi-u2", "bia@gmail.com", "bia@gmail.com", "grt-B")
	if err := st.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(b); err != nil {
		t.Fatal(err)
	}
	if n := len(st.ListAccountsForProvider(ProviderKimiWork)); n != 2 {
		t.Fatalf("expected 2 distinct accounts, got %d", n)
	}
}

func TestUpsertKimiReloginSameRowKeepsIt(t *testing.T) {
	isolateHome(t)
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := kimiAcc("kimi-u1", "ana@gmail.com", "ana@gmail.com", "grt-A")
	if err := st.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}
	// plain re-login of the SAME row id (same Kimi user) must survive
	a.APIKey = "sk-kimi-rotated"
	if err := st.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}
	accs := st.ListAccountsForProvider(ProviderKimiWork)
	if len(accs) != 1 || accs[0].APIKey != "sk-kimi-rotated" {
		t.Fatalf("expected same row updated, got %+v", accs)
	}
}

func TestUpsertKimiDedupMovesActiveAccount(t *testing.T) {
	isolateHome(t)
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	old := kimiAcc("kimi-userA", "fab@gmail.com", "fab@gmail.com", "grt-123")
	if err := st.UpsertAccount(old); err != nil {
		t.Fatal(err)
	}
	if err := st.SetActiveAccount("kimi-userA"); err != nil {
		t.Fatal(err)
	}
	fresh := kimiAcc("kimi-userB", "fab@gmail.com", "fab@gmail.com", "grt-123")
	if err := st.UpsertAccount(fresh); err != nil {
		t.Fatal(err)
	}
	if got := st.Settings().ActiveAccountID; got != "kimi-userB" {
		t.Fatalf("active should move to kimi-userB, got %q", got)
	}
}
