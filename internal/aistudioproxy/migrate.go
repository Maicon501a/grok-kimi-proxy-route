package aistudioproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type legacyProfile struct {
	ID              string `json:"id"`
	Label           string `json:"label"`
	ConnectionFile  string `json:"connectionFile,omitempty"`
	WSEndpoint      string `json:"wsEndpoint,omitempty"`
	UserDataDir     string `json:"userDataDir,omitempty"`
	Email           string `json:"email,omitempty"`
	IsValid         *bool  `json:"isValid,omitempty"`
	CreatedAt       string `json:"createdAt,omitempty"`
	LastLoginAt     string `json:"lastLoginAt,omitempty"`
	LoginMode       string `json:"loginMode,omitempty"`
	ValidationError string `json:"validationError,omitempty"`
}

type legacyProfilesFile struct {
	DefaultProfileID string          `json:"default_profile_id"`
	Profiles         []legacyProfile `json:"profiles"`
}

// MigrateLegacy imports only the configured Chrome profiles and rewrites all
// runtime paths. Debug captures, process locks, sessions, and CDP endpoints are
// intentionally left behind.
func MigrateLegacy(sourceRoot, destinationRoot string) (bool, error) {
	destinationFile := filepath.Join(destinationRoot, "profiles.json")
	if _, err := os.Stat(destinationFile); err == nil {
		return false, nil
	}
	raw, err := os.ReadFile(filepath.Join(sourceRoot, "profiles.json"))
	if err != nil {
		return false, err
	}
	var payload legacyProfilesFile
	if err := json.Unmarshal(raw, &payload); err != nil || len(payload.Profiles) == 0 {
		var list []legacyProfile
		if arrayErr := json.Unmarshal(raw, &list); arrayErr != nil {
			return false, fmt.Errorf("parse legacy profiles: %w", err)
		}
		payload.Profiles = list
	}
	if len(payload.Profiles) == 0 {
		return false, fmt.Errorf("legacy profiles file is empty")
	}
	if err := os.MkdirAll(filepath.Join(destinationRoot, "state", "accounts"), 0o700); err != nil {
		return false, err
	}
	for i := range payload.Profiles {
		p := &payload.Profiles[i]
		if !safeProfileID(p.ID) {
			return false, fmt.Errorf("unsafe profile id %q", p.ID)
		}
		sourceUserData := p.UserDataDir
		if sourceUserData == "" {
			sourceUserData = filepath.Join(sourceRoot, "state", "accounts", p.ID, "user-data")
		}
		destAccount := filepath.Join(destinationRoot, "state", "accounts", p.ID)
		destUserData := filepath.Join(destAccount, "user-data")
		if _, err := os.Stat(sourceUserData); err == nil {
			if err := copyTree(sourceUserData, destUserData); err != nil {
				return false, fmt.Errorf("copy profile %s: %w", p.ID, err)
			}
		}
		p.ConnectionFile = filepath.Join(destAccount, "connection.json")
		p.UserDataDir = destUserData
		p.WSEndpoint = ""
	}
	if payload.DefaultProfileID == "" {
		payload.DefaultProfileID = payload.Profiles[0].ID
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return false, err
	}
	tmp := destinationFile + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, destinationFile); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}

func safeProfileID(id string) bool {
	id = strings.TrimSpace(id)
	return id != "" && id != "." && id != ".." && filepath.Base(id) == id && !strings.ContainsAny(id, `/\\`)
}

func copyTree(source, destination string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		base := strings.ToLower(info.Name())
		if base == "singletonlock" || base == "singletoncookie" || base == "singletonsocket" || base == "lockfile" {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return copyFile(path, target)
	})
}

func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
