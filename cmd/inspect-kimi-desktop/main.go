package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"grok-desktop/internal/kimi"
)

func main() {
	doLogout := flag.Bool("logout", false, "call the official Kimi Desktop logout RPC")
	deactivate := flag.Bool("deactivate", false, "call the official permanent account-deactivation RPC")
	flag.Parse()
	access, payload, err := kimi.ExtractAccessFromDesktop()
	if err != nil {
		panic(err)
	}
	fmt.Printf("desktop_user_id=%s jwt_exp=%d\n", payload.Sub, payload.Exp)
	refreshToken := inspectTokens()
	if refreshToken != "" {
		if refreshed, refreshErr := kimi.RefreshAccessToken(refreshToken); refreshErr != nil {
			fmt.Printf("desktop_refresh_error=%v\n", refreshErr)
		} else {
			access = refreshed.AccessToken
			if next, decodeErr := kimi.DecodeJWT(access); decodeErr == nil && next != nil {
				payload = next
			}
			fmt.Printf("desktop_refresh_ok user_id=%s jwt_exp=%d\n", payload.Sub, payload.Exp)
		}
	}

	body := bytes.NewReader([]byte(`{}`))
	req, err := http.NewRequest(http.MethodPost, kimi.DefaultKimiURL+"/apiv2/kimi.gateway.account.v1.UserService/GetCurrentUser", body)
	if err != nil {
		panic(err)
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 KimiDesktop/3.1.10 Chrome/134.0.0.0 Safari/537.36")
	req.Header.Set("x-msh-platform", "windows")
	req.Header.Set("x-msh-version", "3.1.10")
	req.Header.Set("X-Language", "en-US")
	req.Header.Set("R-Timezone", "America/Sao_Paulo")
	req.Header.Set("Origin", kimi.DefaultKimiURL)
	req.Header.Set("Referer", kimi.DefaultKimiURL+"/")
	if did := kimi.DeviceIDString(payload.DeviceID); did != "" && did != "<nil>" {
		req.Header.Set("x-msh-device-id", did)
	}
	if payload.SSID != "" {
		req.Header.Set("x-msh-session-id", payload.SSID)
	}
	if payload.Sub != "" {
		req.Header.Set("X-Traffic-Id", payload.Sub)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var obj any
	if json.Unmarshal(raw, &obj) == nil {
		b, _ := json.Marshal(redact(obj))
		fmt.Printf("current_user_status=%d body=%s\n", resp.StatusCode, b)
	} else {
		fmt.Printf("current_user_status=%d body=%s\n", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	thirdReq, err := http.NewRequest(http.MethodPost, kimi.DefaultKimiURL+"/apiv2/kimi.gateway.account.v1.SecurityService/ListThirdAccounts", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		panic(err)
	}
	for key, values := range req.Header {
		for _, value := range values {
			thirdReq.Header.Add(key, value)
		}
	}
	thirdResp, err := (&http.Client{}).Do(thirdReq)
	if err != nil {
		panic(err)
	}
	defer thirdResp.Body.Close()
	thirdRaw, _ := io.ReadAll(io.LimitReader(thirdResp.Body, 1<<20))
	var thirdObj any
	if json.Unmarshal(thirdRaw, &thirdObj) == nil {
		b, _ := json.Marshal(redact(thirdObj))
		fmt.Printf("third_accounts_status=%d body=%s\n", thirdResp.StatusCode, b)
	} else {
		fmt.Printf("third_accounts_status=%d body=%s\n", thirdResp.StatusCode, strings.TrimSpace(string(thirdRaw)))
	}
	if *doLogout {
		logoutReq, err := http.NewRequest(http.MethodPost, kimi.DefaultKimiURL+"/apiv2/account.gateway.v1.AuthService/Logout", bytes.NewReader([]byte{}))
		if err != nil {
			panic(err)
		}
		for key, values := range req.Header {
			for _, value := range values {
				logoutReq.Header.Add(key, value)
			}
		}
		logoutReq.Header.Set("Content-Type", "application/proto")
		logoutResp, err := (&http.Client{}).Do(logoutReq)
		if err != nil {
			panic(err)
		}
		defer logoutResp.Body.Close()
		logoutRaw, _ := io.ReadAll(io.LimitReader(logoutResp.Body, 1<<20))
		fmt.Printf("remote_logout_status=%d body=%s\n", logoutResp.StatusCode, strings.TrimSpace(string(logoutRaw)))
	}
	if *deactivate {
		if err := kimi.LogoffAccount(access); err != nil {
			fmt.Printf("deactivate_status=error error=%v\n", err)
		} else {
			fmt.Println("deactivate_status=ok")
		}
	}
}

func inspectTokens() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return ""
	}
	dir := filepath.Join(appData, "kimi-desktop", "Local Storage", "leveldb")
	re := regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
	seen := map[string]bool{}
	bestRefresh := ""
	bestExp := int64(0)
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		for _, token := range re.FindAll(raw, -1) {
			text := string(token)
			if seen[text] {
				continue
			}
			seen[text] = true
			p, err := kimi.DecodeJWT(text)
			if err == nil && p != nil {
				fmt.Printf("desktop_token typ=%s sub=%s exp=%d\n", p.Typ, p.Sub, p.Exp)
				if p.Typ == "refresh" && p.Exp > bestExp {
					bestExp = p.Exp
					bestRefresh = text
				}
			}
		}
	}
	return bestRefresh
}

func redact(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			if strings.Contains(strings.ToLower(k), "token") || strings.Contains(strings.ToLower(k), "password") || strings.Contains(strings.ToLower(k), "api_key") || strings.Contains(strings.ToLower(k), "apikey") {
				out[k] = "<redacted>"
			} else {
				out[k] = redact(value)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = redact(value)
		}
		return out
	default:
		return v
	}
}
