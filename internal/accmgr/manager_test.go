package accmgr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCodeFromText(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"six digits in subject", "Your Accio verification code is 483920", "483920"},
		{"six digits with label", "Accio code: 012345", "012345"},
		{"code only in body", "Welcome!\nUse 774210 to verify your email\nThanks", "774210"},
		{"html body", "<p>Seu código é <b>555019</b></p>", "555019"},
		{"four digit fallback", "code 4821", "4821"},
		{"eight digit fallback", "code 91827364", "91827364"},
		{"six preferred over eight", "order 12345678 code 483920", "483920"},
		{"no digits", "Welcome to Accio", ""},
		{"three digits rejected", "code 123", ""},
		{"nine digits rejected", "id 123456789", ""},
		{"separated by hyphen not matched", "483-920", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codeFromText(tc.text); got != tc.want {
				t.Fatalf("codeFromText(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestCodeFromSubjectCompat(t *testing.T) {
	if got := codeFromSubject("Accio: 123456"); got != "123456" {
		t.Fatalf("codeFromSubject = %q", got)
	}
	if got := codeFromSubject("no code"); got != "" {
		t.Fatalf("codeFromSubject = %q", got)
	}
}

func withCreditTimings(total, every time.Duration, fn func()) {
	oldTotal, oldEvery := creditWaitTotal, creditPollEvery
	creditWaitTotal, creditPollEvery = total, every
	defer func() { creditWaitTotal, creditPollEvery = oldTotal, oldEvery }()
	fn()
}

func TestPollCreditsApprovesAfterDelay(t *testing.T) {
	withCreditTimings(500*time.Millisecond, 50*time.Millisecond, func() {
		calls := 0
		fetch := func(context.Context) (map[string]any, error) {
			calls++
			if calls < 3 {
				return map[string]any{"remaining": 0}, nil
			}
			return map[string]any{"remaining": 20}, nil
		}
		rem, err := pollCredits(context.Background(), fetch)
		if err != nil {
			t.Fatalf("pollCredits err: %v", err)
		}
		if rem != 20 {
			t.Fatalf("remaining = %d, want 20", rem)
		}
		if calls < 3 {
			t.Fatalf("expected polling, got %d call(s)", calls)
		}
	})
}

func TestPollCreditsZeroForeverFails(t *testing.T) {
	withCreditTimings(150*time.Millisecond, 40*time.Millisecond, func() {
		fetch := func(context.Context) (map[string]any, error) {
			return map[string]any{"remaining": 0}, nil
		}
		_, err := pollCredits(context.Background(), fetch)
		if err == nil || !strings.Contains(err.Error(), "still 0") {
			t.Fatalf("expected 'still 0' error, got %v", err)
		}
	})
}

func TestPollCreditsReadErrorSurfaces(t *testing.T) {
	withCreditTimings(150*time.Millisecond, 40*time.Millisecond, func() {
		fetch := func(context.Context) (map[string]any, error) {
			return nil, errors.New("HTTP 500")
		}
		_, err := pollCredits(context.Background(), fetch)
		if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
			t.Fatalf("expected read error, got %v", err)
		}
	})
}

func TestPollCreditsRespectsContextCancel(t *testing.T) {
	withCreditTimings(time.Minute, 40*time.Millisecond, func() {
		ctx, cancel := context.WithCancel(context.Background())
		fetch := func(context.Context) (map[string]any, error) {
			cancel()
			return map[string]any{"remaining": 0}, nil
		}
		_, err := pollCredits(ctx, fetch)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})
}

func TestPruneable(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-time.Minute).UnixMilli()
	old := now.Add(-time.Hour).UnixMilli()
	cases := []struct {
		name string
		bi   balanceInfo
		want bool
	}{
		{"approved with credits", balanceInfo{remaining: 20, total: 20, savedAt: old}, false},
		{"exhausted but approved", balanceInfo{remaining: 0, total: 20, savedAt: old}, false},
		{"fresh unapproved kept", balanceInfo{remaining: 0, total: 0, savedAt: fresh}, false},
		{"old unapproved pruned", balanceInfo{remaining: 0, total: 0, savedAt: old}, true},
		{"no timestamp pruned", balanceInfo{remaining: 0, total: 0, savedAt: 0}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pruneable(tc.bi, now); got != tc.want {
				t.Fatalf("pruneable(%+v) = %v, want %v", tc.bi, got, tc.want)
			}
		})
	}
}

func TestBackoffCooldown(t *testing.T) {
	base := time.Minute
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, time.Minute},
		{1, 2 * time.Minute},
		{2, 4 * time.Minute},
		{5, 32 * time.Minute},
		{6, time.Hour}, // loop cap kicks in
		{20, time.Hour}, // absolute cap
	}
	for _, tc := range cases {
		if got := backoffCooldown(base, tc.failures); got != tc.want {
			t.Fatalf("backoffCooldown(%d failures) = %s, want %s", tc.failures, got, tc.want)
		}
	}
}

func TestDefaultConfig(t *testing.T) {
	t.Setenv("ACCIO_USE_WARP", "")
	cfg := DefaultConfig()
	if cfg.WARP {
		t.Fatal("WARP must default to off (opt-in via ACCIO_USE_WARP=1)")
	}
	if cfg.CreateCooldown < 5*time.Minute {
		t.Fatalf("CreateCooldown default = %s, want >= 5m (signup velocity heats the risk score and gets accounts limited/blocked)", cfg.CreateCooldown)
	}
	t.Setenv("ACCIO_USE_WARP", "1")
	if !DefaultConfig().WARP {
		t.Fatal("ACCIO_USE_WARP=1 must enable WARP")
	}
}

func ExamplecodeFromText() {
	fmt.Println(codeFromText("Your code is 483920. It expires soon."))
	// Output: 483920
}

func TestMaskMatch(t *testing.T) {
	cases := []struct {
		masked, real string
		want         bool
	}{
		{"a*************0@emalupe.com", "accs22350795200@emalupe.com", true},
		{"d**************4@snapmailnow.com", "deepduskbuyer04@snapmailnow.com", true},
		{"f*********l@gmail.com", "flamengobrasil@gmail.com", true},
		{"f*********l@gmail.com", "flamengo@gmail.com", false},
		{"a*************0@emalupe.com", "bxcs22350795200@emalupe.com", false},
		{"a*************0@emalupe.com", "accs22350795200@other.com", false},
		{"a*************1@emalupe.com", "accs22350795200@emalupe.com", false},
		{"*@emalupe.com", "accs22350795200@emalupe.com", true},
		{"", "accs22350795200@emalupe.com", false},
		{"a***@emalupe.com", "", false},
	}
	for _, c := range cases {
		if got := maskMatch(c.masked, c.real); got != c.want {
			t.Errorf("maskMatch(%q, %q) = %v, want %v", c.masked, c.real, got, c.want)
		}
	}
}
