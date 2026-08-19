package aistudioproxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagedRuntimeLive(t *testing.T) {
	if os.Getenv("AISTUDIO_PROXY_LIVE") != "1" {
		t.Skip("set AISTUDIO_PROXY_LIVE=1 to run the embedded runtime integration test")
	}
	root := filepath.Join(os.Getenv("LOCALAPPDATA"), "GrokDesktop")
	mgr := New(root)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := mgr.Start(ctx, `D:\proxy plus`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		if err := mgr.Stop(stopCtx); err != nil {
			t.Error(err)
		}
	}()
	accounts, err := mgr.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) == 0 {
		t.Fatal("migrated runtime returned no accounts")
	}
}
