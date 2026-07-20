package logging

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
)

func TestLevelsFilter(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelWarn)
	l.Debug("dbg")
	l.Info("inf")
	l.Warn("wrn")
	l.Error("err")

	out := buf.String()
	if strings.Contains(out, "dbg") || strings.Contains(out, "inf") {
		t.Fatalf("expected debug/info to be filtered, got: %q", out)
	}
	if !strings.Contains(out, "WARN wrn") || !strings.Contains(out, "ERROR err") {
		t.Fatalf("expected warn+error lines, got: %q", out)
	}
}

func TestFieldsAndSorting(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelDebug).With("provider", "xai")
	l.Info("request", "model", "grok-4.5", "account_id", "acc1")

	line := buf.String()
	// Keys must be sorted: account_id, model, provider.
	ai := strings.Index(line, "account_id=acc1")
	mi := strings.Index(line, "model=grok-4.5")
	pi := strings.Index(line, "provider=xai")
	if ai < 0 || mi < 0 || pi < 0 {
		t.Fatalf("missing fields in %q", line)
	}
	if !(ai < mi && mi < pi) {
		t.Fatalf("fields not sorted: %q", line)
	}
}

func TestMasking(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelDebug)
	l.Info("creds", "api_key", "sk-kimi-abcdefghijklmnop", "refresh_token", "rt-secret-value-123456")

	out := buf.String()
	if strings.Contains(out, "abcdefghijklmnop") || strings.Contains(out, "rt-secret-value") {
		t.Fatalf("secret leaked: %q", out)
	}
	if !strings.Contains(out, "api_key=") || !strings.Contains(out, "…") {
		t.Fatalf("expected masked api_key, got: %q", out)
	}
}

func TestMaskSecretShort(t *testing.T) {
	if MaskSecret("") != "" {
		t.Fatal("empty should stay empty")
	}
	if MaskSecret("abc") != "****" {
		t.Fatal("short secret should be fully masked")
	}
}

func TestRequestIDContext(t *testing.T) {
	var buf bytes.Buffer
	old := std
	std = New(&buf, LevelDebug)
	defer func() { std = old }()

	ctx := WithRequestID(context.Background(), "req-123")
	FromContext(ctx).Info("hello")
	if !strings.Contains(buf.String(), "req_id=req-123") {
		t.Fatalf("expected req_id from context, got: %q", buf.String())
	}
}

func TestConcurrencySafe(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelDebug)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			l.Info("line", "n", n, "token", "secret-value-xyz")
		}(i)
	}
	wg.Wait()
	lines := strings.Count(buf.String(), "\n")
	if lines != 50 {
		t.Fatalf("expected 50 lines, got %d", lines)
	}
	if strings.Contains(buf.String(), "secret-value-xyz") {
		t.Fatal("secret leaked under concurrency")
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"debug": LevelDebug, "INFO": LevelInfo, "warn": LevelWarn,
		"error": LevelError, "bogus": LevelInfo, "": LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Fatalf("ParseLevel(%q)=%v want %v", in, got, want)
		}
	}
}
