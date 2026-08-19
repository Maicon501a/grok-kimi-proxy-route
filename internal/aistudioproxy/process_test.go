package aistudioproxy

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestManagerStartsInProcessLoopbackServer(t *testing.T) {
	mgr := New(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := mgr.Start(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if base := mgr.BaseURL(); base == "" {
		t.Fatal("BaseURL is empty after Start")
	}
	if err := mgr.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Accounts(ctx); err != nil {
		t.Fatalf("authenticated admin client failed: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mgr.BaseURL()+"/admin/api/accounts", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admin request without token returned %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	if err := mgr.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if base := mgr.BaseURL(); base != "" {
		t.Fatalf("BaseURL after Stop = %q, want empty", base)
	}
}
