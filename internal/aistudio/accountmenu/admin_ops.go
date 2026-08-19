package accountmenu

// Operações não-interativas usadas pelo dashboard admin (internal/admin).
// Reusam a mesma lógica do menu interativo, sem prompts de terminal.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	cdptypes "github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"grok-desktop/internal/aistudio/cdp"
	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/profile"
)

// PersistProfile exporta a persistência de perfil para o handler admin.
func (m *Menu) PersistProfile(next profile.Profile) error {
	return m.persistProfile(next)
}

// SetDefaultProfile altera o perfil padrão sem prompts.
func (m *Menu) SetDefaultProfile(profileID string) error {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return errors.New("profile_id obrigatorio")
	}
	if m.profiles.Get(profileID) == nil {
		return fmt.Errorf("perfil nao encontrado: %s", profileID)
	}
	if err := m.profiles.SaveProfiles(m.profiles.List(), profileID); err != nil {
		return err
	}
	return m.runtime.SetActiveChatProfile(profileID)
}

// RemoveAccountByID remove uma conta sem confirmação interativa.
func (m *Menu) RemoveAccountByID(profileID string) error {
	_ = m.CancelManualLogin(profileID)
	p := m.profiles.Get(strings.TrimSpace(profileID))
	if p == nil {
		return fmt.Errorf("perfil nao encontrado: %s", profileID)
	}
	m.runtime.DisconnectProfile(p.ID)
	m.runtime.Sessions().ClearProfileBindings(p.ID, "profile_removed")
	if err := m.profiles.RemoveProfile(p.ID); err != nil {
		return err
	}
	cleanupProfileArtifacts(*p)
	return nil
}

// StartManualLogin opens a managed visible Chrome profile without requiring a
// terminal. The caller completes the flow with CompleteManualLogin after the
// user signs into Google/AI Studio in the opened window.
func (m *Menu) StartManualLogin(ctx context.Context, profileID, label, email string) (profile.Profile, error) {
	m.manualMu.Lock()
	defer m.manualMu.Unlock()
	profileID = strings.TrimSpace(profileID)
	var p profile.Profile
	var original profile.Profile
	created := false
	if profileID != "" {
		existing := m.profiles.Get(profileID)
		if existing == nil {
			return p, fmt.Errorf("perfil nao encontrado: %s", profileID)
		}
		p = *existing
		original = *existing
	} else {
		p = m.buildNewProfile(len(m.listVisibleProfiles(false)))
		created = true
	}
	if strings.TrimSpace(label) != "" {
		p.Label = strings.TrimSpace(label)
	}
	if strings.TrimSpace(email) != "" {
		p.Email = strings.TrimSpace(email)
	}

	if _, exists := m.manualLogins[p.ID]; exists {
		return p, fmt.Errorf("login ja esta aberto para %s", p.ID)
	}

	m.runtime.DisconnectProfile(p.ID)
	cfg := cloneConfigWithBrowserMode(m.cfg, config.BrowserVisibleLegacy)
	client := cdp.New(&p, cfg)
	if err := client.RunExclusive(ctx, "admin-manual-login-open", func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.Navigate(cfg.AIStudio.URL))
	}); err != nil {
		client.Disconnect()
		if created {
			cleanupProfileArtifacts(p)
		}
		return p, err
	}

	valid := false
	p.IsValid = &valid
	p.LoginMode = "manual_browser_pending"
	p.ValidationError = "aguardando login no navegador"
	if err := m.persistProfile(p); err != nil {
		client.Disconnect()
		if created {
			cleanupProfileArtifacts(p)
		}
		return p, err
	}

	m.manualLogins[p.ID] = &manualLoginSession{profile: p, original: original, client: client, created: created}
	return p, nil
}

// CompleteManualLogin validates the visible browser session and persists it.
// Invalid sessions remain open so the user can finish login and retry.
func (m *Menu) CompleteManualLogin(ctx context.Context, profileID string) (profile.Profile, *validationResult, error) {
	profileID = strings.TrimSpace(profileID)
	m.manualMu.Lock()
	defer m.manualMu.Unlock()
	session := m.manualLogins[profileID]
	if session == nil {
		return profile.Profile{}, nil, fmt.Errorf("nenhum login aberto para %s", profileID)
	}

	result, err := validateLoggedInSessionContext(ctx, session.client)
	if err != nil {
		return session.profile, nil, err
	}
	if result == nil {
		result = &validationResult{OK: false, Reason: "validacao inconclusiva"}
	}
	next := session.profile
	valid := result.OK
	next.IsValid = &valid
	if !valid {
		next.ValidationError = firstNonEmpty(result.Reason, "sessao invalida")
		_ = m.persistProfile(next)
		return next, result, nil
	}

	next.Email = firstNonEmpty(result.Email, next.Email)
	next.Label = firstNonEmpty(next.Label, result.Email, next.ID)
	next.LastLoginAt = time.Now().UTC().Format(time.RFC3339)
	next.LoginMode = "manual_browser"
	next.ValidationError = ""
	if err := m.persistProfile(next); err != nil {
		return next, result, err
	}
	delete(m.manualLogins, profileID)
	session.client.Disconnect()
	m.warmUpProfile(profileID)
	return next, result, nil
}

// CancelManualLogin closes a pending visible browser login. Newly-created
// pending profiles are removed; existing profiles are left untouched.
func (m *Menu) CancelManualLogin(profileID string) error {
	profileID = strings.TrimSpace(profileID)
	m.manualMu.Lock()
	defer m.manualMu.Unlock()
	session := m.manualLogins[profileID]
	delete(m.manualLogins, profileID)
	if session == nil {
		return nil
	}
	session.client.Disconnect()
	if session.created {
		_ = m.profiles.RemoveProfile(profileID)
		cleanupProfileArtifacts(session.profile)
	} else {
		_ = m.persistProfile(session.original)
	}
	return nil
}

// ValidateAccount valida a sessão de um perfil via probe headless.
func (m *Menu) ValidateAccount(profileID string) (profile.Profile, *validationResult, error) {
	return m.ValidateAccountContext(context.Background(), profileID)
}

func (m *Menu) ValidateAccountContext(ctx context.Context, profileID string) (profile.Profile, *validationResult, error) {
	p := m.profiles.Get(strings.TrimSpace(profileID))
	if p == nil {
		return profile.Profile{}, nil, fmt.Errorf("perfil nao encontrado: %s", profileID)
	}
	result, err := m.validateAccountAutoContext(ctx, *p)
	if err != nil {
		return *p, nil, err
	}
	return *p, result, nil
}

// importedCookie é um cookie parseado de qualquer formato suportado.
type importedCookie struct {
	Name   string
	Value  string
	Domain string
	Path   string
	Secure bool
	Expiry int64
}

// ImportAccountFromCookiesText importa uma conta a partir de cookies
// exportados (JSON estilo EditThisCookie/Cookie-Editor, Netscape ou header
// "name=value; ..."). Injeta os cookies no perfil Chrome via CDP e valida a
// sessão. Retorna o perfil persistido e o resultado da validação.
func (m *Menu) ImportAccountFromCookiesText(cookiesText, profileID, label, email string) (profile.Profile, *validationResult, error) {
	cookies, err := parseCookiesText(cookiesText)
	if err != nil {
		return profile.Profile{}, nil, err
	}
	if len(cookies) == 0 {
		return profile.Profile{}, nil, errors.New("nenhum cookie reconhecido no texto informado")
	}

	profileID = strings.TrimSpace(profileID)
	var p profile.Profile
	if profileID != "" {
		if existing := m.profiles.Get(profileID); existing != nil {
			p = *existing
		}
	}
	if p.ID == "" {
		p = m.buildNewProfile(len(m.profiles.List()))
		if profileID != "" {
			p.ID = profileID
			accountDir := filepath.Join(m.accountsRoot, profileID)
			p.ConnectionFile = filepath.Join(accountDir, "connection.json")
			p.UserDataDir = filepath.Join(accountDir, "user-data")
		}
	}
	if strings.TrimSpace(label) != "" {
		p.Label = strings.TrimSpace(label)
	}
	if strings.TrimSpace(email) != "" {
		p.Email = strings.TrimSpace(email)
	}
	if err := os.MkdirAll(filepath.Join(p.UserDataDir, "Default"), 0o755); err != nil {
		return p, nil, err
	}

	// Injeta os cookies no perfil via CDP e valida a sessão.
	cfg := cloneConfigWithBrowserMode(m.cfg, config.BrowserHeadlessSpoof)
	client := cdp.New(&p, cfg)
	defer client.Disconnect()

	params := make([]*network.CookieParam, 0, len(cookies))
	for _, ck := range cookies {
		cp := &network.CookieParam{
			Name:   ck.Name,
			Value:  ck.Value,
			Secure: ck.Secure,
		}
		if ck.Domain != "" {
			cp.Domain = ck.Domain
		} else {
			cp.Domain = ".google.com"
		}
		if ck.Path != "" {
			cp.Path = ck.Path
		} else {
			cp.Path = "/"
		}
		if ck.Expiry > 0 {
			expiry := cdptypes.TimeSinceEpoch(time.Unix(ck.Expiry, 0))
			cp.Expires = &expiry
		}
		params = append(params, cp)
	}

	err = client.RunExclusive(context.Background(), "admin-import-cookies", func(ctx context.Context) error {
		return chromedp.Run(ctx,
			chromedp.Navigate("https://www.google.com/"),
			chromedp.Sleep(800*time.Millisecond),
			network.SetCookies(params),
			chromedp.Navigate(cfg.AIStudio.URL),
			chromedp.Sleep(1500*time.Millisecond),
		)
	})
	if err != nil {
		return p, &validationResult{OK: false, Reason: err.Error()}, nil
	}

	result, err := validateLoggedInSession(client)
	if err != nil {
		return p, nil, err
	}
	if result == nil {
		result = &validationResult{OK: false, Reason: "validacao inconclusiva"}
	}

	valid := result.OK
	p.IsValid = &valid
	p.LoginMode = "imported_cookies"
	if valid {
		p.Email = firstNonEmpty(result.Email, p.Email)
		p.Label = firstNonEmpty(result.Email, p.Label)
		p.LastLoginAt = time.Now().UTC().Format(time.RFC3339)
		p.ValidationError = ""
	} else {
		p.ValidationError = firstNonEmpty(result.Reason, "cookies importados nao abriram sessao valida")
	}
	if err := m.persistProfile(p); err != nil {
		return p, result, err
	}
	if valid {
		m.warmUpProfile(p.ID)
	}
	return p, result, nil
}

// parseCookiesText aceita JSON (array ou {"cookies": [...]}), formato
// Netscape (tab-separated) ou header HTTP "name=value; name2=value2".
func parseCookiesText(text string) ([]importedCookie, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("texto de cookies vazio")
	}

	if strings.HasPrefix(text, "[") || strings.HasPrefix(text, "{") {
		return parseJSONCookies(text)
	}

	// Netscape: linhas com 7 campos separados por tab.
	if strings.Contains(text, "\t") {
		var out []importedCookie
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Split(line, "\t")
			if len(fields) < 7 {
				continue
			}
			expiry, _ := strconv.ParseInt(strings.TrimSpace(fields[4]), 10, 64)
			out = append(out, importedCookie{
				Domain: strings.TrimSpace(fields[0]),
				Path:   strings.TrimSpace(fields[2]),
				Secure: strings.EqualFold(strings.TrimSpace(fields[3]), "TRUE"),
				Expiry: expiry,
				Name:   strings.TrimSpace(fields[5]),
				Value:  strings.TrimSpace(fields[6]),
			})
		}
		return out, nil
	}

	// Header style: "SID=abc; HSID=def; ..."
	var out []importedCookie
	for _, part := range strings.Split(text, ";") {
		part = strings.TrimSpace(part)
		name, value, found := strings.Cut(part, "=")
		if !found || strings.TrimSpace(name) == "" {
			continue
		}
		out = append(out, importedCookie{
			Name:  strings.TrimSpace(name),
			Value: strings.TrimSpace(value),
		})
	}
	return out, nil
}

func parseJSONCookies(text string) ([]importedCookie, error) {
	type jsonCookie struct {
		Name           string  `json:"name"`
		Value          string  `json:"value"`
		Domain         string  `json:"domain"`
		Path           string  `json:"path"`
		Secure         bool    `json:"secure"`
		ExpirationDate float64 `json:"expirationDate"`
		Expires        float64 `json:"expires"`
	}

	var list []jsonCookie
	if strings.HasPrefix(text, "[") {
		if err := json.Unmarshal([]byte(text), &list); err != nil {
			return nil, fmt.Errorf("JSON de cookies invalido: %w", err)
		}
	} else {
		var wrapped struct {
			Cookies []jsonCookie `json:"cookies"`
		}
		if err := json.Unmarshal([]byte(text), &wrapped); err != nil {
			return nil, fmt.Errorf("JSON de cookies invalido: %w", err)
		}
		list = wrapped.Cookies
	}

	out := make([]importedCookie, 0, len(list))
	for _, jc := range list {
		if jc.Name == "" {
			continue
		}
		expiry := int64(jc.ExpirationDate)
		if expiry == 0 {
			expiry = int64(jc.Expires)
		}
		out = append(out, importedCookie{
			Name:   jc.Name,
			Value:  jc.Value,
			Domain: jc.Domain,
			Path:   jc.Path,
			Secure: jc.Secure,
			Expiry: expiry,
		})
	}
	return out, nil
}
