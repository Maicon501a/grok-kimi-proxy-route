package aistudioproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type Account struct {
	ID              string `json:"id"`
	Label           string `json:"label"`
	Email           string `json:"email"`
	Default         bool   `json:"default"`
	IsValid         bool   `json:"is_valid"`
	LoginMode       string `json:"login_mode"`
	ValidationError string `json:"validation_error"`
	Available       bool   `json:"available"`
	CooldownUntil   string `json:"cooldown_until"`
	LastError       string `json:"last_error"`
	Requests        int64  `json:"requests"`
	TotalTokens     int64  `json:"total_tokens"`
}

type accountsResponse struct {
	DefaultProfileID string    `json:"default_profile_id"`
	Profiles         []Account `json:"profiles"`
}

func (m *Manager) Accounts(ctx context.Context) ([]map[string]any, error) {
	raw, err := m.adminJSON(ctx, http.MethodGet, "/admin/api/accounts", nil)
	if err != nil {
		return nil, err
	}
	var payload accountsResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(payload.Profiles))
	for _, a := range payload.Profiles {
		out = append(out, map[string]any{
			"id": "gemini:" + a.ID, "profile_id": a.ID, "provider": "gemini",
			"label": a.Label, "email": a.Email, "active": a.Default, "default": a.Default,
			"is_valid": a.IsValid, "available": a.Available, "login_mode": a.LoginMode,
			"validation_error": a.ValidationError, "cooldown_until": a.CooldownUntil,
			"last_error": a.LastError, "exhausted": !a.Available && a.IsValid,
			"usage": map[string]any{"total_tokens": a.TotalTokens, "requests": a.Requests, "cost_usd": 0},
		})
	}
	return out, nil
}

func (m *Manager) SetDefault(ctx context.Context, id string) error {
	_, err := m.adminJSON(ctx, http.MethodPost, "/admin/api/accounts/default", map[string]any{"profile_id": rawProfileID(id)})
	return err
}

func (m *Manager) Delete(ctx context.Context, id string) error {
	_, err := m.adminJSON(ctx, http.MethodPost, "/admin/api/accounts/delete", map[string]any{"profile_id": rawProfileID(id)})
	return err
}

func (m *Manager) Rename(ctx context.Context, id, label string) error {
	_, err := m.adminJSON(ctx, http.MethodPost, "/admin/api/accounts/rename", map[string]any{"profile_id": rawProfileID(id), "label": label})
	return err
}

func (m *Manager) Validate(ctx context.Context, id string) (map[string]any, error) {
	raw, err := m.adminJSON(ctx, http.MethodPost, "/admin/api/accounts/validate", map[string]any{"profile_id": rawProfileID(id)})
	return decodeMap(raw, err)
}

func (m *Manager) StartLogin(ctx context.Context, id, label, email string) (map[string]any, error) {
	raw, err := m.adminJSON(ctx, http.MethodPost, "/admin/api/accounts/login/start", map[string]any{
		"profile_id": rawProfileID(id), "label": label, "email": email,
	})
	return decodeMap(raw, err)
}

func (m *Manager) CompleteLogin(ctx context.Context, id string) (map[string]any, error) {
	raw, err := m.adminJSON(ctx, http.MethodPost, "/admin/api/accounts/login/complete", map[string]any{"profile_id": rawProfileID(id)})
	return decodeMap(raw, err)
}

func (m *Manager) CancelLogin(ctx context.Context, id string) error {
	_, err := m.adminJSON(ctx, http.MethodPost, "/admin/api/accounts/login/cancel", map[string]any{"profile_id": rawProfileID(id)})
	return err
}

func rawProfileID(id string) string { return strings.TrimPrefix(strings.TrimSpace(id), "gemini:") }
func IsAccountID(id string) bool    { return strings.HasPrefix(strings.TrimSpace(id), "gemini:") }

func decodeMap(raw []byte, err error) (map[string]any, error) {
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (m *Manager) adminJSON(ctx context.Context, method, path string, body any) ([]byte, error) {
	base := m.BaseURL()
	if base == "" {
		return nil, fmt.Errorf("AI Studio runtime is not started")
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	token := m.token
	m.mu.RUnlock()
	req.Header.Set("X-AIStudio-Admin-Token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("AI Studio admin %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}
