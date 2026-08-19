// Package profile loads and manages the set of AI Studio profiles configured in
// profiles.json (with an automatic fallback to .browser-connection.json).
package profile

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/storage"
)

// Profile is a normalized AI Studio connection profile.
type Profile struct {
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

	AllowGlobalEndpoint bool `json:"-"`
}

// Registry is a file-backed profile registry with mtime-based caching.
type Registry struct {
	cfg                *config.Config
	mu                 sync.RWMutex
	cache              []Profile
	cacheMtime         int64
	cacheValidatedAt   time.Time
	loadedFromFallback bool
	defaultID          string
}

const endpointValidationTTL = 5 * time.Second

// New creates a registry.
func New(cfg *config.Config) *Registry {
	r := &Registry{cfg: cfg, defaultID: cfg.Profiles.DefaultID}
	r.load()
	return r
}

// List returns all profiles.
func (r *Registry) List() []Profile {
	r.load()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Profile, len(r.cache))
	copy(out, r.cache)
	return out
}

// IDs returns profile ids only.
func (r *Registry) IDs() []string {
	profiles := r.List()
	out := make([]string, len(profiles))
	for i, p := range profiles {
		out[i] = p.ID
	}
	return out
}

// Get returns a profile by id.
func (r *Registry) Get(id string) *Profile {
	profiles := r.List()
	for _, p := range profiles {
		if p.ID == id {
			cp := p
			return &cp
		}
	}
	return nil
}

// Resolve returns the requested profile, or the default, or the first
// available. Returns ErrUnknownProfile only when an explicit id is unknown.
func (r *Registry) Resolve(id string) (*Profile, error) {
	if id != "" {
		if p := r.Get(id); p != nil {
			if !isProfileValid(*p) {
				return nil, ErrProfileUnavailable
			}
			return p, nil
		}
		return nil, ErrUnknownProfile
	}
	if p := r.Get(r.defaultID); p != nil {
		if isProfileValid(*p) {
			return p, nil
		}
	}
	profiles := r.List()
	if len(profiles) == 0 {
		return nil, ErrNoProfiles
	}
	for _, p := range profiles {
		if isProfileValid(p) {
			cp := p
			return &cp, nil
		}
	}
	cp := profiles[0]
	return &cp, nil
}

// IsUsingFallback reports whether the registry is operating without a
// profiles.json (auto-detected connection file).
func (r *Registry) IsUsingFallback() bool {
	r.load()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.loadedFromFallback
}

// DefaultID returns the active default profile id.
func (r *Registry) DefaultID() string {
	r.load()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultID
}

// SaveProfiles persists the explicit profiles.json payload and refreshes the
// in-memory cache immediately.
func (r *Registry) SaveProfiles(profiles []Profile, defaultProfileID string) error {
	r.load()

	type payload struct {
		DefaultProfileID string    `json:"default_profile_id"`
		Profiles         []Profile `json:"profiles"`
	}

	if strings.TrimSpace(defaultProfileID) == "" {
		defaultProfileID = r.defaultID
	}
	if strings.TrimSpace(defaultProfileID) == "" && len(profiles) > 0 {
		defaultProfileID = profiles[0].ID
	}

	normalized := make([]Profile, 0, len(profiles))
	for i, p := range profiles {
		raw := rawProfile{
			ID:              p.ID,
			Label:           p.Label,
			ConnectionFile:  p.ConnectionFile,
			WSEndpoint:      p.WSEndpoint,
			UserDataDir:     p.UserDataDir,
			Email:           p.Email,
			IsValid:         p.IsValid,
			CreatedAt:       p.CreatedAt,
			LastLoginAt:     p.LastLoginAt,
			LoginMode:       p.LoginMode,
			ValidationError: p.ValidationError,
		}
		if next := normalize(raw, i, defaultProfileID, r.cfg.AIStudio.ConnectionFile, r.cfg.AIStudio.WSEndpoint); next != nil {
			normalized = append(normalized, *next)
		}
	}

	encoded, err := json.MarshalIndent(payload{
		DefaultProfileID: defaultProfileID,
		Profiles:         normalized,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := storage.WriteAtomic(r.cfg.Profiles.File, encoded); err != nil {
		return err
	}

	mtime := int64(0)
	if info, err := os.Stat(r.cfg.Profiles.File); err == nil {
		mtime = info.ModTime().UnixNano()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = normalized
	r.cacheMtime = mtime
	r.cacheValidatedAt = time.Now()
	r.loadedFromFallback = false
	r.defaultID = defaultProfileID
	return nil
}

// RemoveProfile deletes a profile from the explicit registry.
func (r *Registry) RemoveProfile(profileID string) error {
	current := r.List()
	next := make([]Profile, 0, len(current))
	for _, p := range current {
		if p.ID != profileID {
			next = append(next, p)
		}
	}

	defaultProfileID := r.DefaultID()
	if defaultProfileID == profileID {
		if len(next) > 0 {
			defaultProfileID = next[0].ID
		} else {
			defaultProfileID = r.cfg.Profiles.DefaultID
		}
	}

	if len(next) == 0 {
		if err := os.Remove(r.cfg.Profiles.File); err != nil && !os.IsNotExist(err) {
			return err
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.cache = nil
		r.cacheMtime = 0
		r.cacheValidatedAt = time.Now()
		r.loadedFromFallback = false
		r.defaultID = defaultProfileID
		return nil
	}

	return r.SaveProfiles(next, defaultProfileID)
}

func (r *Registry) load() {
	info, err := os.Stat(r.cfg.Profiles.File)
	mtime := int64(0)
	if err == nil {
		mtime = info.ModTime().UnixNano()
	}

	r.mu.RLock()
	cacheMtime := r.cacheMtime
	cacheValidatedAt := r.cacheValidatedAt
	hasCache := r.cache != nil || r.loadedFromFallback
	r.mu.RUnlock()
	if cacheMtime == mtime && hasCache && time.Since(cacheValidatedAt) < endpointValidationTTL {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	raw, err := os.ReadFile(r.cfg.Profiles.File)
	if err == nil && len(raw) > 0 {
		loaded, defaultID := readProfilesFile(raw)
		if defaultID != "" && (r.defaultID == "" || r.defaultID == "default") {
			r.defaultID = defaultID
		}
		normalized := normalizeAll(loaded, r.defaultID, r.cfg.AIStudio.ConnectionFile, r.cfg.AIStudio.WSEndpoint)
		r.cache = normalized
		r.cacheMtime = mtime
		r.cacheValidatedAt = time.Now()
		r.loadedFromFallback = false
		return
	}

	// Fallback profiles.
	fallback := make([]Profile, 0, len(r.cfg.Profiles.FallbackProfiles))
	for _, src := range r.cfg.Profiles.FallbackProfiles {
		p := Profile{
			ID:                  src.ID,
			Label:               src.Label,
			ConnectionFile:      src.ConnectionFile,
			WSEndpoint:          src.WSEndpoint,
			UserDataDir:         src.UserDataDir,
			AllowGlobalEndpoint: true,
		}
		if isAvailable(p) {
			fallback = append(fallback, p)
		}
	}
	r.cache = fallback
	r.cacheMtime = 0
	r.cacheValidatedAt = time.Now()
	r.loadedFromFallback = true
}

func readProfilesFile(raw []byte) ([]rawProfile, string) {
	var asArray []rawProfile
	if err := json.Unmarshal(raw, &asArray); err == nil {
		return asArray, ""
	}
	var asObject struct {
		DefaultProfileID string       `json:"default_profile_id"`
		Profiles         []rawProfile `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &asObject); err == nil {
		return asObject.Profiles, asObject.DefaultProfileID
	}
	return nil, ""
}

type rawProfile struct {
	ID               string `json:"id"`
	Label            string `json:"label"`
	ConnectionFile   string `json:"connectionFile"`
	Connection_file  string `json:"connection_file"`
	WSEndpoint       string `json:"wsEndpoint"`
	WsEndpoint       string `json:"ws_endpoint"`
	UserDataDir      string `json:"userDataDir"`
	UserDataDirSnake string `json:"user_data_dir"`
	Email            string `json:"email"`
	IsValid          *bool  `json:"isValid"`
	CreatedAt        string `json:"createdAt"`
	Created_at       string `json:"created_at"`
	LastLoginAt      string `json:"lastLoginAt"`
	LastLogin_at     string `json:"last_login_at"`
	LoginMode        string `json:"loginMode"`
	Login_mode       string `json:"login_mode"`
	ValidationError  string `json:"validationError"`
	Validation_error string `json:"validation_error"`
}

func normalizeAll(raw []rawProfile, defaultID, globalConnectionFile, globalWSEndpoint string) []Profile {
	out := make([]Profile, 0, len(raw))
	for i, p := range raw {
		normalized := normalize(p, i, defaultID, globalConnectionFile, globalWSEndpoint)
		if normalized != nil {
			out = append(out, *normalized)
		}
	}
	return out
}

func normalize(p rawProfile, index int, defaultID, globalConnectionFile, globalWSEndpoint string) *Profile {
	id := p.ID
	if id == "" {
		id = "profile-" + itoa(index+1)
	}
	if id == "" {
		return nil
	}
	connectionFile := p.ConnectionFile
	if connectionFile == "" {
		connectionFile = p.Connection_file
	}
	wsEndpoint := p.WSEndpoint
	if wsEndpoint == "" {
		wsEndpoint = p.WsEndpoint
	}
	userDataDir := p.UserDataDir
	if userDataDir == "" {
		userDataDir = p.UserDataDirSnake
	}
	label := p.Label
	if label == "" {
		label = id
	}
	allowGlobalEndpoint := id == defaultID
	valid := p.IsValid
	available := isConnectable(connectionFile, wsEndpoint)
	if !available && allowGlobalEndpoint && globalWSEndpoint != "" && isLiveEndpoint(globalWSEndpoint) {
		wsEndpoint = globalWSEndpoint
		available = true
	}
	if !available && allowGlobalEndpoint && isConnectable(globalConnectionFile, "") {
		connectionFile = globalConnectionFile
		available = true
	}
	if valid == nil {
		valid = &available
	}
	return &Profile{
		ID:                  id,
		Label:               label,
		ConnectionFile:      connectionFile,
		WSEndpoint:          wsEndpoint,
		UserDataDir:         userDataDir,
		Email:               p.Email,
		IsValid:             valid,
		CreatedAt:           firstNonEmpty(p.CreatedAt, p.Created_at),
		LastLoginAt:         firstNonEmpty(p.LastLoginAt, p.LastLogin_at),
		LoginMode:           firstNonEmpty(p.LoginMode, p.Login_mode),
		ValidationError:     firstNonEmpty(p.ValidationError, p.Validation_error),
		AllowGlobalEndpoint: allowGlobalEndpoint,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func isAvailable(p Profile) bool {
	return isConnectable(p.ConnectionFile, p.WSEndpoint)
}

func isConnectable(connectionFile, wsEndpoint string) bool {
	if wsEndpoint != "" {
		return isLiveEndpoint(wsEndpoint)
	}
	if connectionFile != "" {
		if meta, err := readConnectionMeta(connectionFile); err == nil {
			return isLiveEndpoint(meta.endpointCandidate())
		}
	}
	return false
}

func isProfileValid(p Profile) bool {
	return p.IsValid == nil || *p.IsValid
}

type connectionMeta struct {
	WSEndpoint string `json:"wsEndpoint"`
	HTTPURL    string `json:"httpUrl"`
	DebugPort  int    `json:"debugPort"`
	Host       string `json:"host"`
}

func (m connectionMeta) endpointCandidate() string {
	if ws := strings.TrimSpace(m.WSEndpoint); ws != "" {
		return ws
	}
	if httpURL := strings.TrimSpace(m.HTTPURL); httpURL != "" {
		return httpURL
	}
	if host := strings.TrimSpace(m.Host); host != "" && m.DebugPort > 0 {
		return fmt.Sprintf("http://%s:%d", host, m.DebugPort)
	}
	if m.DebugPort > 0 {
		return fmt.Sprintf("http://127.0.0.1:%d", m.DebugPort)
	}
	return ""
}

func readConnectionMeta(path string) (*connectionMeta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta connectionMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func isLiveEndpoint(candidate string) bool {
	versionURL, err := browserVersionURL(candidate)
	if err != nil {
		return false
	}
	client := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(versionURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func browserVersionURL(candidate string) (string, error) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return "", fmt.Errorf("empty endpoint")
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported endpoint scheme: %s", parsed.Scheme)
	}
	parsed.Path = "/json/version"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// ProjectDir re-exports config.ProjectDir for callers that only import profile.
func ProjectDir() string { return config.ProjectDir() }
