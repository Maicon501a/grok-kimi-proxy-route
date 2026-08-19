package register

import (
	"fmt"
	"os"
	"strings"
)

// EmailProvider returns the configured upstream-compatible inbox provider.
func EmailProvider() string {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("EMAIL_PROVIDER")))
	if provider == "" {
		return "mailtm"
	}
	return provider
}

// CaptchaProvider identifies the solver used by the upstream production flow.
func CaptchaProvider() string {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("CAPTCHA_PROVIDER")))
	if provider != "" {
		return provider
	}
	if strings.TrimSpace(os.Getenv("YESCAPTCHA_KEY")) != "" {
		return "yescaptcha"
	}
	return "browser"
}

// ValidateEnvironment fails before OAuth/device setup or dependency downloads
// when the upstream signup engine cannot possibly run. Secret values are never
// included in errors or logs.
func ValidateEnvironment(provider string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch CaptchaProvider() {
	case "browser":
	case "yescaptcha":
		if strings.TrimSpace(os.Getenv("YESCAPTCHA_KEY")) == "" {
			return fmt.Errorf("configure YESCAPTCHA_KEY para CAPTCHA_PROVIDER=yescaptcha")
		}
	default:
		return fmt.Errorf("CAPTCHA_PROVIDER %q inválido; use browser ou yescaptcha", CaptchaProvider())
	}
	required := map[string][]string{
		"luckmail": {"LUCKMAIL_API_KEY"},
		"mailnest": {"MAILNEST_API_KEY"},
		"gmail":    {"GMAIL_BASE_EMAIL", "GMAIL_APP_PASSWORD"},
		"mailtm":   {},
	}
	vars, ok := required[provider]
	if !ok {
		return fmt.Errorf("EMAIL_PROVIDER %q inválido; use luckmail, mailnest, gmail ou mailtm", provider)
	}
	var missing []string
	for _, name := range vars {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("configure %s para EMAIL_PROVIDER=%s", strings.Join(missing, ", "), provider)
	}
	return nil
}
