package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// WebOverridesFile retorna o caminho do arquivo de overrides gravado pelo
// dashboard admin (state/web-overrides.json).
func WebOverridesFile(stateDir string) string {
	return filepath.Join(stateDir, "web-overrides.json")
}

// LoadWebOverrides lê o arquivo de overrides. Tolerante a arquivo ausente ou
// inválido: nesses casos retorna mapa vazio.
func LoadWebOverrides(path string) map[string]string {
	values := make(map[string]string)
	raw, err := os.ReadFile(path)
	if err != nil {
		return values
	}
	var parsed map[string]string
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return values
	}
	for k, v := range parsed {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k != "" && v != "" {
			values[k] = v
		}
	}
	return values
}

// SaveWebOverrides persiste o mapa de overrides, removendo chaves vazias.
// Escreve de forma atômica (tmp + rename) para não corromper o arquivo em
// caso de interrupção.
func SaveWebOverrides(path string, values map[string]string) error {
	clean := make(map[string]string, len(values))
	for k, v := range values {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k != "" && v != "" {
			clean[k] = v
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(clean, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// applyWebOverrides aplica os valores do arquivo de overrides sobre a config
// carregada. Variáveis de ambiente e flags de CLI têm precedência: um override
// só é aplicado quando a env correspondente não está definida.
func (c *Config) applyWebOverrides(stateDir string) {
	values := LoadWebOverrides(WebOverridesFile(stateDir))
	if len(values) == 0 {
		return
	}

	if v := values["AISTUDIO_BROWSER_MODE"]; v != "" && os.Getenv("AISTUDIO_BROWSER_MODE") == "" {
		if mode := normalizeBrowserMode(v); mode != "" {
			c.AIStudio.BrowserMode = mode
			c.AIStudio.ManagedHeadless = mode != BrowserVisibleLegacy
			c.AIStudio.ManagedHeadlessSpoofVisible = mode == BrowserHeadlessSpoof
		}
	}
	if v := values["AISTUDIO_CONVERSATION_MODE"]; v != "" && os.Getenv("AISTUDIO_CONVERSATION_MODE") == "" {
		if mode := normalizeConversationMode(v); mode != "" {
			c.Conversation.Mode = mode
		}
	}
	if v := values["AISTUDIO_TOOL_CALLING_MODE"]; v != "" && os.Getenv("AISTUDIO_TOOL_CALLING_MODE") == "" {
		if mode := normalizeToolCallingMode(v); mode != "" {
			c.ToolCalling.Mode = mode
		}
	}
	if v := values["AISTUDIO_TOOL_STREAM_MODE"]; v != "" && os.Getenv("AISTUDIO_TOOL_STREAM_MODE") == "" {
		if mode := normalizeToolStreamMode(v); mode != "" {
			c.Streaming.ToolMode = mode
		}
	}
	if v := values["AISTUDIO_DEBUG_MESSAGE_FLOW"]; v != "" && os.Getenv("AISTUDIO_DEBUG_MESSAGE_FLOW") == "" {
		if parsed := parseBoolEnv(v); parsed != nil {
			c.Debug.MessageFlow = *parsed
		}
	}
	if v := values["AISTUDIO_MAX_MIGRATION_HOPS"]; v != "" && os.Getenv("AISTUDIO_MAX_MIGRATION_HOPS") == "" {
		c.Migration.MaxHopsPerRequest = parseIntEnv(v, c.Migration.MaxHopsPerRequest)
	}
	if v := values["AISTUDIO_DEFAULT_PROFILE"]; v != "" && os.Getenv("AISTUDIO_DEFAULT_PROFILE") == "" {
		c.Profiles.DefaultID = v
	}
	if v := values["AISTUDIO_EAGER_BOOT"]; v != "" && os.Getenv("AISTUDIO_EAGER_BOOT") == "" {
		c.EagerBoot = v
	}
	if v := values["CDP_WS_ENDPOINT"]; v != "" && os.Getenv("CDP_WS_ENDPOINT") == "" {
		c.AIStudio.WSEndpoint = v
	}
}
