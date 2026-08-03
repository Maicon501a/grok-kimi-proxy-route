package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const sqliteFileName = "grokdesktop.db"

func (s *Store) dbPath() string {
	return filepath.Join(s.root, sqliteFileName)
}

func openSQLite(path string) (*sql.DB, error) {
	// WAL + busy timeout for concurrent proxy + UI
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite write serialization; simple + safe for desktop
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func migrateSchema(db *sql.DB) error {
	schema := `
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
  id   INTEGER PRIMARY KEY CHECK (id = 1),
  json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS accounts (
  id                 TEXT PRIMARY KEY,
  provider           TEXT NOT NULL DEFAULT 'xai',
  label              TEXT NOT NULL DEFAULT '',
  email              TEXT NOT NULL DEFAULT '',
  team_id            TEXT NOT NULL DEFAULT '',
  user_id            TEXT NOT NULL DEFAULT '',
  access_token       TEXT NOT NULL DEFAULT '',
  refresh_token      TEXT NOT NULL DEFAULT '',
  expires_at         TEXT NOT NULL DEFAULT '',
  api_key            TEXT NOT NULL DEFAULT '',
  device_id          TEXT NOT NULL DEFAULT '',
  source             TEXT NOT NULL DEFAULT '',
  exhausted_at       TEXT NOT NULL DEFAULT '',
  exhaust_reason     TEXT NOT NULL DEFAULT '',
  auth_denied_at     TEXT NOT NULL DEFAULT '',
  auth_denied_reason TEXT NOT NULL DEFAULT '',
  client_id          TEXT NOT NULL DEFAULT '',
  issuer             TEXT NOT NULL DEFAULT '',
  scope              TEXT NOT NULL DEFAULT '',
  google_refresh_token TEXT NOT NULL DEFAULT '',
  created_at         TEXT NOT NULL DEFAULT '',
  updated_at         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_accounts_provider ON accounts(provider);
CREATE INDEX IF NOT EXISTS idx_accounts_user ON accounts(user_id);

CREATE TABLE IF NOT EXISTS usage (
  account_id         TEXT PRIMARY KEY,
  prompt_tokens      INTEGER NOT NULL DEFAULT 0,
  completion_tokens  INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens   INTEGER NOT NULL DEFAULT 0,
  cached_tokens      INTEGER NOT NULL DEFAULT 0,
  total_tokens       INTEGER NOT NULL DEFAULT 0,
  requests           INTEGER NOT NULL DEFAULT 0,
  cost_usd           REAL NOT NULL DEFAULT 0,
  latency_sum_ms     INTEGER NOT NULL DEFAULT 0,
  ttft_sum_ms        INTEGER NOT NULL DEFAULT 0,
  latency_samples    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS history (
  id                 TEXT PRIMARY KEY,
  at                 TEXT NOT NULL,
  account_id         TEXT NOT NULL DEFAULT '',
  model              TEXT NOT NULL DEFAULT '',
  prompt_tokens      INTEGER NOT NULL DEFAULT 0,
  completion_tokens  INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens   INTEGER NOT NULL DEFAULT 0,
  cached_tokens      INTEGER NOT NULL DEFAULT 0,
  total_tokens       INTEGER NOT NULL DEFAULT 0,
  cost_usd           REAL NOT NULL DEFAULT 0,
  latency_ms         INTEGER NOT NULL DEFAULT 0,
  ttft_ms            INTEGER NOT NULL DEFAULT 0,
  estimated          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_history_at ON history(at DESC);
`
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}
	// Migration: add google_refresh_token if missing (added 2026-07-17)
	_, _ = db.Exec(`ALTER TABLE accounts ADD COLUMN google_refresh_token TEXT NOT NULL DEFAULT ''`)
	return nil
}

func (s *Store) initDB() error {
	db, err := openSQLite(s.dbPath())
	if err != nil {
		return err
	}
	s.db = db
	return nil
}

func timeToSQL(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func timeFromSQL(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

func cleanDeviceID(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "<nil>" || v == "nil" || strings.EqualFold(v, "null") {
		return ""
	}
	return v
}

func (s *Store) loadFromSQLite() error {
	if s.db == nil {
		return fmt.Errorf("db not open")
	}
	// settings
	var settingsJSON string
	err := s.db.QueryRow(`SELECT json FROM settings WHERE id = 1`).Scan(&settingsJSON)
	if err == nil && settingsJSON != "" {
		var st Settings
		if json.Unmarshal([]byte(settingsJSON), &st) == nil {
			s.settings = mergeSettings(defaultSettings(), st)
		}
	} else if err != nil && err != sql.ErrNoRows {
		return err
	}

	// accounts
	rows, err := s.db.Query(`SELECT id, provider, label, email, team_id, user_id,
		access_token, refresh_token, expires_at, api_key, device_id, source,
		exhausted_at, exhaust_reason, auth_denied_at, auth_denied_reason,
		client_id, issuer, scope, google_refresh_token, created_at, updated_at FROM accounts`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var a Account
		var exp, exh, auth, created, updated string
		if err := rows.Scan(
			&a.ID, &a.Provider, &a.Label, &a.Email, &a.TeamID, &a.UserID,
			&a.AccessToken, &a.RefreshToken, &exp, &a.APIKey, &a.DeviceID, &a.Source,
			&exh, &a.ExhaustReason, &auth, &a.AuthDeniedReason,
			&a.ClientID, &a.Issuer, &a.Scope, &a.GoogleRefreshToken, &created, &updated,
		); err != nil {
			continue
		}
		a.DeviceID = cleanDeviceID(a.DeviceID)
		a.ExpiresAt = timeFromSQL(exp)
		a.ExhaustedAt = timeFromSQL(exh)
		a.AuthDeniedAt = timeFromSQL(auth)
		a.CreatedAt = timeFromSQL(created)
		a.UpdatedAt = timeFromSQL(updated)
		if strings.TrimSpace(a.Provider) == "" {
			a.Provider = ProviderXAI
		}
		a.Provider = a.NormalizedProvider()
		if a.AccessToken == "" && a.APIKey == "" {
			continue
		}
		if a.NormalizedProvider() == ProviderKimiWork && a.APIKey == "" && strings.HasPrefix(a.AccessToken, "sk-kimi-") {
			a.APIKey = a.AccessToken
		}
		s.accounts[a.ID] = a
	}

	// usage
	urows, err := s.db.Query(`SELECT account_id, prompt_tokens, completion_tokens, reasoning_tokens,
		cached_tokens, total_tokens, requests, cost_usd, latency_sum_ms, ttft_sum_ms, latency_samples FROM usage`)
	if err != nil {
		return err
	}
	defer urows.Close()
	for urows.Next() {
		var id string
		var u UsageTotals
		if err := urows.Scan(&id, &u.PromptTokens, &u.CompletionTokens, &u.ReasoningTokens,
			&u.CachedTokens, &u.TotalTokens, &u.Requests, &u.CostUSD, &u.LatencySumMs, &u.TTFTSumMs, &u.LatencySamples); err != nil {
			continue
		}
		s.usage[id] = u
	}

	// history (newest first, cap 200)
	hrows, err := s.db.Query(`SELECT id, at, account_id, model, prompt_tokens, completion_tokens, reasoning_tokens,
		cached_tokens, total_tokens, cost_usd, latency_ms, ttft_ms, estimated FROM history ORDER BY at DESC LIMIT 200`)
	if err != nil {
		return err
	}
	defer hrows.Close()
	for hrows.Next() {
		var h RequestSample
		var est int
		if err := hrows.Scan(&h.ID, &h.At, &h.AccountID, &h.Model, &h.PromptTokens, &h.CompletionTokens, &h.ReasoningTokens,
			&h.CachedTokens, &h.TotalTokens, &h.CostUSD, &h.LatencyMs, &h.TTFTMs, &est); err != nil {
			continue
		}
		h.Estimated = est != 0
		s.history = append(s.history, h)
	}
	return nil
}

func (s *Store) saveSettingsDB(st Settings) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO settings(id, json) VALUES(1, ?)
		ON CONFLICT(id) DO UPDATE SET json=excluded.json`, string(b))
	return err
}

func (s *Store) saveAccountDB(a Account) error {
	a.DeviceID = cleanDeviceID(a.DeviceID)
	_, err := s.db.Exec(`INSERT INTO accounts(
		id, provider, label, email, team_id, user_id, access_token, refresh_token, expires_at,
		api_key, device_id, source, exhausted_at, exhaust_reason, auth_denied_at, auth_denied_reason,
		client_id, issuer, scope, google_refresh_token, created_at, updated_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(id) DO UPDATE SET
		provider=excluded.provider, label=excluded.label, email=excluded.email, team_id=excluded.team_id,
		user_id=excluded.user_id, access_token=excluded.access_token, refresh_token=excluded.refresh_token,
		expires_at=excluded.expires_at, api_key=excluded.api_key, device_id=excluded.device_id,
		source=excluded.source, exhausted_at=excluded.exhausted_at, exhaust_reason=excluded.exhaust_reason,
		auth_denied_at=excluded.auth_denied_at, auth_denied_reason=excluded.auth_denied_reason,
		client_id=excluded.client_id, issuer=excluded.issuer, scope=excluded.scope,
		google_refresh_token=excluded.google_refresh_token,
		created_at=excluded.created_at, updated_at=excluded.updated_at`,
		a.ID, a.NormalizedProvider(), a.Label, a.Email, a.TeamID, a.UserID,
		a.AccessToken, a.RefreshToken, timeToSQL(a.ExpiresAt),
		a.APIKey, a.DeviceID, a.Source, timeToSQL(a.ExhaustedAt), a.ExhaustReason,
		timeToSQL(a.AuthDeniedAt), a.AuthDeniedReason,
		a.ClientID, a.Issuer, a.Scope, a.GoogleRefreshToken, timeToSQL(a.CreatedAt), timeToSQL(a.UpdatedAt),
	)
	return err
}

func (s *Store) deleteAccountDB(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM history WHERE account_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM usage WHERE account_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM accounts WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) saveUsageDB(usage map[string]UsageTotals) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM usage`); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO usage(account_id, prompt_tokens, completion_tokens, reasoning_tokens,
		cached_tokens, total_tokens, requests, cost_usd, latency_sum_ms, ttft_sum_ms, latency_samples)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for id, u := range usage {
		if _, err := stmt.Exec(id, u.PromptTokens, u.CompletionTokens, u.ReasoningTokens,
			u.CachedTokens, u.TotalTokens, u.Requests, u.CostUSD, u.LatencySumMs, u.TTFTSumMs, u.LatencySamples); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) insertHistoryDB(h RequestSample) error {
	est := 0
	if h.Estimated {
		est = 1
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO history(
		id, at, account_id, model, prompt_tokens, completion_tokens, reasoning_tokens,
		cached_tokens, total_tokens, cost_usd, latency_ms, ttft_ms, estimated
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		h.ID, h.At, h.AccountID, h.Model, h.PromptTokens, h.CompletionTokens, h.ReasoningTokens,
		h.CachedTokens, h.TotalTokens, h.CostUSD, h.LatencyMs, h.TTFTMs, est,
	)
	if err != nil {
		return err
	}
	// keep last 200
	_, _ = s.db.Exec(`DELETE FROM history WHERE id NOT IN (
		SELECT id FROM history ORDER BY at DESC LIMIT 200
	)`)
	return nil
}

// migrateJSONToSQLite imports legacy settings/accounts/usage/history JSON once.
func (s *Store) migrateJSONToSQLite() (int, error) {
	if s.db == nil {
		return 0, fmt.Errorf("db not open")
	}
	var flag string
	_ = s.db.QueryRow(`SELECT value FROM meta WHERE key = 'json_migrated_v1'`).Scan(&flag)
	if flag == "1" {
		return 0, nil
	}

	// If DB already has accounts, just mark migrated (avoid double import).
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM accounts`).Scan(&n)
	if n > 0 {
		_, _ = s.db.Exec(`INSERT INTO meta(key,value) VALUES('json_migrated_v1','1')
			ON CONFLICT(key) DO UPDATE SET value='1'`)
		return 0, nil
	}

	imported := 0

	// settings.json
	if b, err := os.ReadFile(s.settingsPath()); err == nil {
		b = stripUTF8BOM(b)
		var st Settings
		if json.Unmarshal(b, &st) == nil {
			s.settings = mergeSettings(defaultSettings(), st)
			_ = s.saveSettingsDB(s.settings)
		}
	}

	// accounts/*.json
	if entries, err := os.ReadDir(s.accountsDir()); err == nil {
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(s.accountsDir(), e.Name()))
			if err != nil {
				continue
			}
			var a Account
			if json.Unmarshal(b, &a) != nil || a.ID == "" {
				continue
			}
			if strings.TrimSpace(a.Provider) == "" {
				a.Provider = ProviderXAI
			}
			a.Provider = a.NormalizedProvider()
			a.DeviceID = cleanDeviceID(a.DeviceID)
			if a.AccessToken == "" && a.APIKey == "" {
				continue
			}
			if a.NormalizedProvider() == ProviderKimiWork && a.APIKey == "" && strings.HasPrefix(a.AccessToken, "sk-kimi-") {
				a.APIKey = a.AccessToken
			}
			if err := s.saveAccountDB(a); err == nil {
				s.accounts[a.ID] = a
				imported++
			}
		}
	}

	// usage.json
	if b, err := os.ReadFile(s.usagePath()); err == nil {
		var u map[string]UsageTotals
		if json.Unmarshal(b, &u) == nil && u != nil {
			s.usage = u
			_ = s.saveUsageDB(u)
		}
	}

	// history.json
	if b, err := os.ReadFile(s.historyPath()); err == nil {
		var h []RequestSample
		if json.Unmarshal(b, &h) == nil {
			for _, sample := range h {
				_ = s.insertHistoryDB(sample)
			}
			s.history = h
			if len(s.history) > 200 {
				s.history = s.history[:200]
			}
		}
	}

	_, _ = s.db.Exec(`INSERT INTO meta(key,value) VALUES('json_migrated_v1','1')
		ON CONFLICT(key) DO UPDATE SET value='1'`)
	return imported, nil
}
