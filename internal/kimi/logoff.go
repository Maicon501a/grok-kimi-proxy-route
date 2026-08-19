package kimi

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// AuthLogoutURL is the consumer-session logout RPC used by Kimi Desktop.
	AuthLogoutURL = "https://auth.kimi.com/api/account.gateway.v1.AuthService/Logout"
	// DeactivateAccountURL is the generated Connect RPC for Settings → Deactivate
	// Account (kimi.gateway.account.v1.SecurityService/DeactivateAccount, verified
	// unchanged in the 2026-08-18 kimi.com web bundle and Desktop 3.2.0). Its
	// protobuf descriptor has no REST annotation, so the client posts an empty
	// message to the canonical Connect route under the /apiv2 base.
	DeactivateAccountURL = DefaultKimiURL + "/apiv2/kimi.gateway.account.v1.SecurityService/DeactivateAccount"
	// LegacyLogoffURL is retained only for older Kimi desktop builds whose new
	// RPC route is unavailable.
	LegacyLogoffURL = DefaultKimiURL + "/api/user/logoff"
)

// HasWebSession reports whether the account can call consumer APIs (logoff, etc.).
// sk-kimi keys alone are Work gateway credentials and cannot delete the user account.
func HasWebSession(accessToken, refreshToken string) bool {
	accessToken = strings.TrimPrefix(strings.TrimSpace(accessToken), "Bearer ")
	refreshToken = strings.TrimPrefix(strings.TrimSpace(refreshToken), "Bearer ")
	if refreshToken != "" {
		return true
	}
	if accessToken == "" || strings.HasPrefix(accessToken, "sk-kimi-") {
		return false
	}
	return strings.Count(accessToken, ".") == 2
}

// EnsureAccessToken returns a usable access JWT, refreshing when needed.
func EnsureAccessToken(accessToken, refreshToken string) (access, refresh string, err error) {
	accessToken = strings.TrimPrefix(strings.TrimSpace(accessToken), "Bearer ")
	refreshToken = strings.TrimPrefix(strings.TrimSpace(refreshToken), "Bearer ")
	if strings.HasPrefix(accessToken, "sk-kimi-") {
		accessToken = ""
	}
	needRefresh := accessToken == "" || refreshToken != ""
	if accessToken != "" {
		if p, perr := DecodeJWT(accessToken); perr == nil && p != nil && p.Exp > 0 {
			// refresh if expired or under 2 minutes left
			if time.Until(time.Unix(p.Exp, 0)) > 2*time.Minute {
				return accessToken, refreshToken, nil
			}
			needRefresh = true
		}
	}
	if !needRefresh {
		if accessToken != "" {
			return accessToken, refreshToken, nil
		}
		return "", refreshToken, fmt.Errorf("no web access_token or refresh_token")
	}
	if refreshToken == "" {
		if accessToken != "" {
			return accessToken, "", nil
		}
		return "", "", fmt.Errorf("access_token expired and no refresh_token")
	}
	s, err := RefreshAccessToken(refreshToken)
	if err != nil {
		// last chance: try existing access if still present
		if accessToken != "" {
			return accessToken, refreshToken, nil
		}
		return "", refreshToken, err
	}
	access = s.AccessToken
	refresh = s.RefreshToken
	if refresh == "" {
		refresh = refreshToken
	}
	return access, refresh, nil
}

// LogoffAccount permanently deletes the Kimi user account using the same
// SecurityService/DeactivateAccount Connect RPC as the current Kimi clients.
// Uses a consumer access JWT, not an sk-kimi Work key.
func LogoffAccount(accessToken string) error {
	accessToken = strings.TrimPrefix(strings.TrimSpace(accessToken), "Bearer ")
	if accessToken == "" || strings.HasPrefix(accessToken, "sk-kimi-") {
		return fmt.Errorf("web access_token JWT required for account logoff")
	}
	client := &http.Client{Timeout: 45 * time.Second}
	// Desktop runs Connect in binary mode. JSON variants are included for older
	// gateways that do not accept the binary media types.
	attempts := []struct {
		contentType string
		body        []byte
	}{
		{"application/proto", nil},
		{"application/connect+proto", nil},
		{"application/json", []byte("{}")},
		{"application/connect+json", []byte("{}")},
	}
	var codecErrs []string
	for _, attempt := range attempts {
		req, err := http.NewRequest(http.MethodPost, DeactivateAccountURL, bytes.NewReader(attempt.body))
		if err != nil {
			return err
		}
		setLogoffHeaders(req, accessToken)
		req.Header.Set("Content-Type", attempt.contentType)
		req.Header.Set("Connect-Protocol-Version", "1")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		// An unsupported codec is safe to retry with the next Connect shape.
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnsupportedMediaType || resp.StatusCode == http.StatusUnprocessableEntity {
			codecErrs = append(codecErrs, fmt.Sprintf("content-type %s HTTP %d (response content-type=%q accept-post=%q): %s", attempt.contentType, resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Accept-Post"), truncate(string(b), 240)))
			continue
		}
		// Older desktop releases used DELETE /api/user/logoff. Fall back only
		// when the current RPC route itself is unavailable.
		if resp.StatusCode == http.StatusNotFound {
			return logoffLegacyAccount(accessToken)
		}
		return fmt.Errorf("deactivate account HTTP %d: %s", resp.StatusCode, truncate(string(b), 240))
	}
	if len(codecErrs) > 0 {
		return fmt.Errorf("deactivate account codec attempts: %s", strings.Join(codecErrs, " | "))
	}
	return fmt.Errorf("deactivate account rejected every supported Connect media type")
}

// LogoutAccount revokes the current consumer session without deleting the
// Kimi user account. This is useful when local credentials are being removed
// but DeactivateAccount is blocked by Kimi's account-age policy.
func LogoutAccount(accessToken string) error {
	accessToken = strings.TrimPrefix(strings.TrimSpace(accessToken), "Bearer ")
	if accessToken == "" || strings.HasPrefix(accessToken, "sk-kimi-") {
		return fmt.Errorf("web access_token JWT required for session logout")
	}
	req, err := http.NewRequest(http.MethodPost, AuthLogoutURL, bytes.NewReader(nil))
	if err != nil {
		return err
	}
	setLogoffHeaders(req, accessToken)
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	resp, err := (&http.Client{Timeout: 45 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("session logout HTTP %d: %s", resp.StatusCode, truncate(string(b), 240))
}

// LogoutWithSession refreshes the consumer JWT if needed, then revokes the
// current Kimi session. It intentionally does not deactivate the account.
func LogoutWithSession(accessToken, refreshToken string) (usedAccess string, err error) {
	if !HasWebSession(accessToken, refreshToken) {
		return "", fmt.Errorf("account has no web session (only sk-kimi?) — cannot logout")
	}
	access, _, err := EnsureAccessToken(accessToken, refreshToken)
	if err != nil {
		return "", err
	}
	if err := LogoutAccount(access); err != nil {
		return access, err
	}
	return access, nil
}

func setLogoffHeaders(req *http.Request, accessToken string) {
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) KimiDesktop/3.2.0 Chrome/134.0.0.0 Safari/537.36")
	req.Header.Set("Origin", DefaultKimiURL)
	req.Header.Set("Referer", DefaultKimiURL+"/settings")
	req.Header.Set("x-msh-platform", "windows")
	req.Header.Set("x-msh-version", "3.2.0")
	req.Header.Set("X-Language", "en-US")
	req.Header.Set("R-Timezone", "America/Sao_Paulo")
	if p, err := DecodeJWT(accessToken); err == nil && p != nil {
		if did := DeviceIDString(p.DeviceID); did != "" && did != "<nil>" {
			req.Header.Set("x-msh-device-id", did)
		}
		if p.SSID != "" {
			req.Header.Set("x-msh-session-id", p.SSID)
		}
		if p.Sub != "" {
			req.Header.Set("X-Traffic-Id", p.Sub)
		}
	}
}

func logoffLegacyAccount(accessToken string) error {
	req, err := http.NewRequest(http.MethodDelete, LegacyLogoffURL, nil)
	if err != nil {
		return err
	}
	setLogoffHeaders(req, accessToken)
	resp, err := (&http.Client{Timeout: 45 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("legacy logoff HTTP %d: %s", resp.StatusCode, truncate(string(b), 240))
}

// LogoffWithSession refreshes the web JWT if needed, then deletes the account.
// Returns the access token used (for diagnostics) and error.
func LogoffWithSession(accessToken, refreshToken string) (usedAccess string, err error) {
	if !HasWebSession(accessToken, refreshToken) {
		return "", fmt.Errorf("account has no web session (only sk-kimi?) — cannot logoff")
	}
	access, _, err := EnsureAccessToken(accessToken, refreshToken)
	if err != nil {
		return "", err
	}
	if err := LogoffAccount(access); err != nil {
		return access, err
	}
	return access, nil
}
