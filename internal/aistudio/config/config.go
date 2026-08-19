// Package config holds the central, immutable configuration for the proxy.
// It is constructed once at startup from environment variables, CLI flags and
// persisted state files.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BrowserMode controls how the managed Chrome instance is launched.
type BrowserMode string

const (
	BrowserHeadlessSpoof BrowserMode = "headless_spoof"
	BrowserHeadlessRaw   BrowserMode = "headless_raw"
	BrowserVisibleLegacy BrowserMode = "visible_legacy"
)

// ToolCallingMode controls whether prompt-injection bridging or native tool
// calling is preferred.
type ToolCallingMode string

const (
	ToolCallingBridgeFirst ToolCallingMode = "bridge_first"
	ToolCallingNativeFirst ToolCallingMode = "native_first"
)

// ConversationMode controls whether conversation history is persisted.
type ConversationMode string

const (
	ConversationStateful  ConversationMode = "stateful"
	ConversationStateless ConversationMode = "stateless"
)

// ToolStreamMode controls how tool calls are delivered during streaming.
type ToolStreamMode string

const (
	StreamBuffered ToolStreamMode = "buffered"
	StreamHybrid   ToolStreamMode = "hybrid"
	StreamLive     ToolStreamMode = "live"
)

// Config is the fully-resolved application configuration.
type Config struct {
	Port        int
	Host        string
	StateDir    string
	AccountFile string

	AIStudio AIStudioConfig
	Profiles ProfilesConfig

	DefaultModel string

	MaxTokensDefault   int
	MaxTokensMax       int
	TemperatureDefault float64
	TopKDefault        int
	TopPDefault        float64

	GenerateTimeout       int // milliseconds (hard ceiling safety, applied via context)
	GenerateHeaderTimeout int // milliseconds to wait for upstream response headers
	GenerateIdleReadMs    int // milliseconds between upstream frames before aborting
	BrowserConnectTimeout int
	PageLoadTimeout       int

	PromptInjection PromptInjectionConfig

	MaxRetries   int
	RetryDelayMs int

	Streaming    StreamingConfig
	ToolCalling  ToolCallingConfig
	Conversation ConversationConfig
	Debug        DebugConfig

	Migration MigrationConfig

	EagerBoot     string
	CDPSessionTTL int

	BrowserModes      map[string]BrowserMode
	ToolCallingModes  map[string]ToolCallingMode
	ConversationModes map[string]ConversationMode
}

type AIStudioConfig struct {
	URL                         string
	WSEndpoint                  string
	ConnectionFile              string
	BrowserModeFile             string
	BrowserMode                 BrowserMode
	ManagedHeadless             bool
	ManagedHeadlessSpoofVisible bool
	VisibleUserAgent            string
	VisibleViewportWidth        int
	VisibleViewportHeight       int
	VisibleDeviceScale          float64
}

type ProfilesConfig struct {
	File             string
	DefaultID        string
	FallbackProfiles []ProfileSource
}

type ProfileSource struct {
	ID             string
	Label          string
	ConnectionFile string
	UserDataDir    string
	WSEndpoint     string
}

type PromptInjectionConfig struct {
	Enabled           bool
	SystemPrefixTools string
	UserSuffixTools   string
}

type StreamingConfig struct {
	ToolMode    ToolStreamMode
	KeepaliveMs int // SSE keepalive interval during upstream silence; 0 disables
}

type ToolCallingConfig struct {
	Mode ToolCallingMode
}

type ConversationConfig struct {
	Mode ConversationMode
}

type DebugConfig struct {
	MessageFlow bool
}

type MigrationConfig struct {
	Enabled           bool
	MaxHopsPerRequest int
	CooldownMs        MigrationCooldown
}

type MigrationCooldown struct {
	Auth    int
	Quota   int
	Runtime int
}

// Load builds the configuration from the environment, CLI args and the
// optional persisted browser-mode file. projectDir is the directory containing
// the executable/config (the go-proxy checkout root).
func Load(projectDir string, args []string) (*Config, error) {
	stateDir := filepath.Join(projectDir, "state")
	browserModeFile := filepath.Join(stateDir, "browser-mode.json")
	browserMode := resolveBrowserMode(browserModeFile, os.Getenv("AISTUDIO_BROWSER_MODE"))
	debugMessageFlow := false
	if value := parseBoolEnv(os.Getenv("AISTUDIO_DEBUG_MESSAGE_FLOW")); value != nil {
		debugMessageFlow = *value
	}

	toolStreamMode := normalizeToolStreamMode(os.Getenv("AISTUDIO_TOOL_STREAM_MODE"))
	if toolStreamMode == "" {
		toolStreamMode = StreamBuffered
	}

	toolCallingModeArg := readCliArgValue(args, "--tool-calling-mode", "--aistudio-tool-calling-mode")
	toolCallingMode := normalizeToolCallingMode(os.Getenv("AISTUDIO_TOOL_CALLING_MODE"))
	if toolCallingMode == "" {
		toolCallingMode = normalizeToolCallingMode(toolCallingModeArg)
	}
	if toolCallingMode == "" {
		toolCallingMode = ToolCallingNativeFirst
	}

	conversationMode := normalizeConversationMode(os.Getenv("AISTUDIO_CONVERSATION_MODE"))
	if conversationMode == "" {
		conversationMode = ConversationStateless
	}

	maxHops := parseIntEnv(os.Getenv("AISTUDIO_MAX_MIGRATION_HOPS"), 2)

	c := &Config{
		Port:        parseIntEnv(os.Getenv("PROXY_PORT"), 3001),
		Host:        envOrDefault(os.Getenv("PROXY_HOST"), "0.0.0.0"),
		StateDir:    stateDir,
		AccountFile: filepath.Join(stateDir, "accounts.json"),

		AIStudio: AIStudioConfig{
			URL:                         "https://aistudio.google.com/prompts/new_chat",
			WSEndpoint:                  os.Getenv("CDP_WS_ENDPOINT"),
			ConnectionFile:              filepath.Join(projectDir, ".browser-connection.json"),
			BrowserModeFile:             browserModeFile,
			BrowserMode:                 browserMode,
			ManagedHeadless:             browserMode != BrowserVisibleLegacy,
			ManagedHeadlessSpoofVisible: browserMode == BrowserHeadlessSpoof,
			VisibleUserAgent: envOrDefault(os.Getenv("AISTUDIO_VISIBLE_USER_AGENT"),
				"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"),
			VisibleViewportWidth:  parseIntEnv(os.Getenv("AISTUDIO_VISIBLE_VIEWPORT_WIDTH"), 1680),
			VisibleViewportHeight: parseIntEnv(os.Getenv("AISTUDIO_VISIBLE_VIEWPORT_HEIGHT"), 1002),
			VisibleDeviceScale:    parseFloatEnv(os.Getenv("AISTUDIO_VISIBLE_DEVICE_SCALE"), 1),
		},
		Profiles: ProfilesConfig{
			File:      filepath.Join(projectDir, "profiles.json"),
			DefaultID: envOrDefault(os.Getenv("AISTUDIO_DEFAULT_PROFILE"), "default"),
			FallbackProfiles: []ProfileSource{
				{
					ID:             "default",
					Label:          "Sessao principal (auto)",
					ConnectionFile: filepath.Join(projectDir, ".browser-connection.json"),
					UserDataDir:    filepath.Join(projectDir, ".browser-profile"),
					WSEndpoint:     os.Getenv("CDP_WS_ENDPOINT"),
				},
			},
		},

		DefaultModel:       "models/gemini-3.1-pro-preview",
		MaxTokensDefault:   65536,
		MaxTokensMax:       131072,
		TemperatureDefault: 0.95,
		TopKDefault:        64,
		TopPDefault:        1,

		GenerateTimeout:       parseIntEnv(os.Getenv("AISTUDIO_GENERATE_TIMEOUT_MS"), 600000),
		GenerateHeaderTimeout: parseIntEnv(os.Getenv("AISTUDIO_GENERATE_HEADER_TIMEOUT_MS"), 60000),
		GenerateIdleReadMs:    parseIntEnv(os.Getenv("AISTUDIO_GENERATE_IDLE_READ_MS"), 45000),
		BrowserConnectTimeout: parseIntEnv(os.Getenv("AISTUDIO_BROWSER_CONNECT_TIMEOUT_MS"), 30000),
		PageLoadTimeout:       parseIntEnv(os.Getenv("AISTUDIO_PAGE_LOAD_TIMEOUT_MS"), 30000),

		PromptInjection: PromptInjectionConfig{
			Enabled:           true,
			SystemPrefixTools: "[SYSTEM_MODE: TOOL_USE_REQUIRED]\nYou MUST respond using tool calls in the specified JSON format only.\nDo not output natural language before or after the tool call JSON block.\nFormat each tool call as:\n```json\n{\"tool_execution\": \"function_name\", \"arguments\": {...}}\n```\n",
			UserSuffixTools:   "\n\n[FORMAT REQUIREMENT]\nResponda APENAS com uma chamada de ferramenta no formato:\n```json\n{\"tool_execution\": \"<function_name>\", \"arguments\": {<params>}}\n```\nNão inclua nenhum outro texto antes ou depois do bloco JSON.",
		},

		MaxRetries:   2,
		RetryDelayMs: 2000,

		Streaming:    StreamingConfig{ToolMode: toolStreamMode, KeepaliveMs: parseIntEnv(os.Getenv("AISTUDIO_STREAM_KEEPALIVE_MS"), 15000)},
		ToolCalling:  ToolCallingConfig{Mode: toolCallingMode},
		Conversation: ConversationConfig{Mode: conversationMode},
		Debug:        DebugConfig{MessageFlow: debugMessageFlow},

		Migration: MigrationConfig{
			Enabled:           true,
			MaxHopsPerRequest: maxHops,
			CooldownMs: MigrationCooldown{
				Auth:    5 * 60 * 1000,
				Quota:   2 * 60 * 1000,
				Runtime: 60 * 1000,
			},
		},

		EagerBoot:     envOrDefault(os.Getenv("AISTUDIO_EAGER_BOOT"), "default"),
		CDPSessionTTL: 600000,

		BrowserModes: map[string]BrowserMode{
			"headless_spoof": BrowserHeadlessSpoof,
			"headless_raw":   BrowserHeadlessRaw,
			"visible_legacy": BrowserVisibleLegacy,
		},
		ToolCallingModes: map[string]ToolCallingMode{
			"bridge_first": ToolCallingBridgeFirst,
			"native_first": ToolCallingNativeFirst,
		},
		ConversationModes: map[string]ConversationMode{
			"stateful":  ConversationStateful,
			"stateless": ConversationStateless,
		},
	}

	// Overrides do dashboard admin (state/web-overrides.json) aplicados por
	// cima dos defaults; env/CLI continuam com precedencia maior.
	c.applyWebOverrides(stateDir)

	return c, nil
}

// LoadEmbedded resolves the normal persisted configuration and then applies
// the desktop integration's fixed runtime policy. Keeping this explicit avoids
// mutating process-wide environment variables in the host application.
func LoadEmbedded(projectDir string) (*Config, error) {
	c, err := Load(projectDir, nil)
	if err != nil {
		return nil, err
	}
	c.Host = "127.0.0.1"
	c.Port = 0
	c.AIStudio.BrowserMode = BrowserHeadlessSpoof
	c.AIStudio.ManagedHeadless = true
	c.AIStudio.ManagedHeadlessSpoofVisible = true
	c.Conversation.Mode = ConversationStateless
	c.ToolCalling.Mode = ToolCallingNativeFirst
	c.Streaming.ToolMode = StreamBuffered
	c.EagerBoot = "default"
	return c, nil
}

// ProjectDir returns the directory of the running executable, falling back to
// the current working directory. This lets the proxy locate state/ and
// profiles.json relative to itself.
func ProjectDir() string {
	for _, envName := range []string{"AISTUDIO_PROJECT_DIR", "PROXY_PROJECT_DIR"} {
		if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
			return filepath.Clean(value)
		}
	}

	candidates := make([]string, 0, 2)
	if wd, err := os.Getwd(); err == nil && strings.TrimSpace(wd) != "" {
		candidates = append(candidates, wd)
	}
	if exe, err := os.Executable(); err == nil && strings.TrimSpace(exe) != "" {
		candidates = append(candidates, filepath.Dir(exe))
	}

	if resolved := resolveRuntimeRoot(candidates); resolved != "" {
		return resolved
	}
	if len(candidates) > 0 {
		return filepath.Clean(candidates[0])
	}
	return "."
}

func resolveRuntimeRoot(candidates []string) string {
	seen := map[string]bool{}
	for _, start := range candidates {
		dir := filepath.Clean(strings.TrimSpace(start))
		if dir == "" {
			continue
		}
		for {
			if seen[dir] {
				break
			}
			seen[dir] = true
			if looksLikeRuntimeRoot(dir) {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return ""
}

func looksLikeRuntimeRoot(dir string) bool {
	return fileExists(filepath.Join(dir, "profiles.json")) ||
		dirExists(filepath.Join(dir, "state")) ||
		fileExists(filepath.Join(dir, ".browser-connection.json")) ||
		dirExists(filepath.Join(dir, ".browser-profile"))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func resolveBrowserMode(modeFile, envValue string) BrowserMode {
	if explicit := normalizeBrowserMode(envValue); explicit != "" {
		return explicit
	}
	envHeadless := parseBoolEnv(os.Getenv("AISTUDIO_MANAGED_HEADLESS"))
	envSpoof := parseBoolEnv(os.Getenv("AISTUDIO_MANAGED_HEADLESS_SPOOF_VISIBLE"))
	if envHeadless != nil && !*envHeadless {
		return BrowserVisibleLegacy
	}
	if envHeadless != nil && *envHeadless {
		if envSpoof != nil && !*envSpoof {
			return BrowserHeadlessRaw
		}
		return BrowserHeadlessSpoof
	}

	if data, err := os.ReadFile(modeFile); err == nil {
		var parsed struct {
			Mode string `json:"mode"`
		}
		if json.Unmarshal(data, &parsed) == nil {
			if m := normalizeBrowserMode(parsed.Mode); m != "" {
				return m
			}
		}
	}
	return BrowserHeadlessSpoof
}

func normalizeBrowserMode(value string) BrowserMode {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "headless_spoof":
		return BrowserHeadlessSpoof
	case "headless_raw":
		return BrowserHeadlessRaw
	case "visible_legacy":
		return BrowserVisibleLegacy
	}
	return ""
}

func normalizeToolStreamMode(value string) ToolStreamMode {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "buffered":
		return StreamBuffered
	case "hybrid":
		return StreamHybrid
	case "live":
		return StreamLive
	}
	return ""
}

func normalizeToolCallingMode(value string) ToolCallingMode {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "bridge_first":
		return ToolCallingBridgeFirst
	case "native_first":
		return ToolCallingNativeFirst
	}
	return ""
}

func normalizeConversationMode(value string) ConversationMode {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "stateful":
		return ConversationStateful
	case "stateless":
		return ConversationStateless
	}
	return ""
}

func parseBoolEnv(value string) *bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	lower := strings.ToLower(value)
	switch lower {
	case "1", "true", "yes":
		b := true
		return &b
	case "0", "false", "no":
		b := false
		return &b
	}
	return nil
}

func parseIntEnv(value string, fallback int) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

func parseFloatEnv(value string, fallback float64) float64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return f
}

func envOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func readCliArgValue(args []string, flags ...string) string {
	for i := 0; i < len(args); i++ {
		current := args[i]
		for _, flag := range flags {
			if current == flag {
				if i+1 < len(args) {
					return args[i+1]
				}
				return ""
			}
			prefix := flag + "="
			if strings.HasPrefix(current, prefix) {
				return strings.TrimPrefix(current, prefix)
			}
		}
	}
	return ""
}

// String returns a human-readable summary of the relevant configuration fields.
func (c *Config) String() string {
	return fmt.Sprintf(
		"port=%d host=%s model=%s browser=%s promptInjection=%t stream=%s keepalive=%dms toolCalling=%s conversation=%s debugMessageFlow=%t retries=%d generateTimeoutMs=%d headerTimeoutMs=%d idleReadMs=%d",
		c.Port, c.Host, c.DefaultModel, c.AIStudio.BrowserMode,
		c.PromptInjection.Enabled, c.Streaming.ToolMode, c.Streaming.KeepaliveMs, c.ToolCalling.Mode,
		c.Conversation.Mode, c.Debug.MessageFlow, c.MaxRetries, c.GenerateTimeout, c.GenerateHeaderTimeout, c.GenerateIdleReadMs,
	)
}
