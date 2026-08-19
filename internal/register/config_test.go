package register

import (
	"strings"
	"testing"
)

func TestValidateEnvironment(t *testing.T) {
	t.Setenv("CAPTCHA_PROVIDER", "yescaptcha")
	t.Setenv("YESCAPTCHA_KEY", "")
	if err := ValidateEnvironment("mailtm"); err == nil || !strings.Contains(err.Error(), "YESCAPTCHA_KEY") {
		t.Fatalf("missing captcha key: %v", err)
	}
	t.Setenv("YESCAPTCHA_KEY", "test")
	if err := ValidateEnvironment("mailtm"); err != nil {
		t.Fatalf("mailtm should need no extra key: %v", err)
	}
	if err := ValidateEnvironment("luckmail"); err == nil || !strings.Contains(err.Error(), "LUCKMAIL_API_KEY") {
		t.Fatalf("missing luckmail key: %v", err)
	}
	t.Setenv("LUCKMAIL_API_KEY", "test")
	if err := ValidateEnvironment("luckmail"); err != nil {
		t.Fatalf("configured luckmail: %v", err)
	}
	if err := ValidateEnvironment("unknown"); err == nil {
		t.Fatal("unknown provider should fail")
	}
}

func TestEmailProviderDefault(t *testing.T) {
	t.Setenv("EMAIL_PROVIDER", "")
	if got := EmailProvider(); got != "mailtm" {
		t.Fatalf("provider=%q", got)
	}
	t.Setenv("EMAIL_PROVIDER", " Gmail ")
	if got := EmailProvider(); got != "gmail" {
		t.Fatalf("provider=%q", got)
	}
}

func TestCaptchaProviderDefaultsToBrowserWithoutKey(t *testing.T) {
	t.Setenv("CAPTCHA_PROVIDER", "")
	t.Setenv("YESCAPTCHA_KEY", "")
	if got := CaptchaProvider(); got != "browser" {
		t.Fatalf("captcha=%q", got)
	}
	t.Setenv("YESCAPTCHA_KEY", "test")
	if got := CaptchaProvider(); got != "yescaptcha" {
		t.Fatalf("captcha=%q", got)
	}
}
