package accio

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type localGatewayEvent struct {
	Type    string         `json:"type"`
	Method  string         `json:"method"`
	Success *bool          `json:"success"`
	Code    any            `json:"code"`
	Message string         `json:"message"`
	Payload map[string]any `json:"payload"`
	Params  map[string]any `json:"params"`
}

// gatewayCLIConfig is the on-disk shape of ~/.accio/accounts/<id>/.accio/runtime/gateway-cli.json.
// The Accio Desktop process writes this file on startup with the local gateway
// URL and Basic-auth password (token). GrokDesktop.exe does not inherit the
// ACCIO_GATEWAY_TOKEN env var from the Accio Desktop parent, so this disk
// fallback is essential — without it the local gateway is skipped and every
// request hits the captcha-protected remote endpoint.
type gatewayCLIConfig struct {
	URL       string `json:"url"`
	WsURL     string `json:"wsUrl"`
	AuthMode  string `json:"authMode"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	PID       int    `json:"pid"`
	CreatedAt string `json:"createdAt"`
}

// readGatewayCLIConfig scans ~/.accio/accounts/*/.accio/runtime/gateway-cli.json
// and returns the password (token) and URL from the most recently created entry.
func readGatewayCLIConfig() (token, gwURL string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	base := filepath.Join(home, ".accio", "accounts")
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", ""
	}
	var best *gatewayCLIConfig
	var bestTime time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(base, entry.Name(), ".accio", "runtime", "gateway-cli.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg gatewayCLIConfig
		if json.Unmarshal(raw, &cfg) != nil {
			continue
		}
		t, _ := time.Parse(time.RFC3339, cfg.CreatedAt)
		if t.IsZero() {
			if info, err := os.Stat(path); err == nil {
				t = info.ModTime()
			}
		}
		if t.After(bestTime) {
			best = &cfg
			bestTime = t
		}
	}
	if best == nil {
		return "", ""
	}
	return best.Password, best.URL
}

// localGatewayConfig resolves the local gateway token and base URL.
// Env vars (set by the Accio Desktop parent) take priority; the disk fallback
// covers standalone GrokDesktop.exe processes that do not inherit them.
func localGatewayConfig() (token, baseURL string) {
	token = strings.TrimSpace(os.Getenv("ACCIO_GATEWAY_TOKEN"))
	baseURL = strings.TrimSpace(os.Getenv("ACCIO_LOCAL_GATEWAY_URL"))
	if token != "" && baseURL != "" {
		return token, baseURL
	}
	diskToken, diskURL := readGatewayCLIConfig()
	if token == "" {
		token = diskToken
	}
	if baseURL == "" {
		baseURL = diskURL
	}
	return token, baseURL
}

func localGatewayToken() string {
	t, _ := localGatewayConfig()
	return t
}

func localGatewayURL() string {
	_, base := localGatewayConfig()
	if base == "" {
		base = "http://127.0.0.1:4097"
	}
	base = strings.TrimRight(base, "/")
	return strings.Replace(base, "http://", "ws://", 1)
}

// localGatewayAvailable reports whether the local gateway should be tried at
// all. The Accio desktop app writes gateway-cli.json on startup even when its
// remote session is broken (PhoenixBridge without a sender), which makes the
// WebSocket accept queries but never deliver a response. After two timeouts
// the local path is disabled for a few minutes so chat falls straight back to
// the remote gateway instead of hanging for tens of seconds.
func localGatewayAvailable() bool {
	localGatewayCircuit.mu.Lock()
	defer localGatewayCircuit.mu.Unlock()
	if time.Now().Before(localGatewayCircuit.disabledUntil) {
		return false
	}
	return localGatewayToken() != ""
}

// localGatewayFailed records a local-gateway failure (timeout with no
// response). Two consecutive failures disable the local path temporarily.
func localGatewayFailed() {
	localGatewayCircuit.mu.Lock()
	defer localGatewayCircuit.mu.Unlock()
	localGatewayCircuit.consecutive++
	if localGatewayCircuit.consecutive >= 2 {
		localGatewayCircuit.disabledUntil = time.Now().Add(5 * time.Minute)
		localGatewayCircuit.consecutive = 0
		log.Printf("accio: local gateway disabled for 5m after %d timeouts", 2)
	}
}

type localGatewayCircuitState struct {
	mu            sync.Mutex
	consecutive   int
	disabledUntil time.Time
}

var localGatewayCircuit localGatewayCircuitState

func localAccountID() string {
	if value := strings.TrimSpace(os.Getenv("ACCIO_ACCOUNT_ID")); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, "AppData", "Roaming", "Accio", "remembered-accounts.json"))
	if err != nil {
		return ""
	}
	var accounts map[string]json.RawMessage
	if json.Unmarshal(b, &accounts) != nil {
		return ""
	}
	keys := make([]string, 0, len(accounts))
	for key := range accounts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		return keys[len(keys)-1]
	}
	return ""
}

func localUtdid() string {
	if value := strings.TrimSpace(os.Getenv("ACCIO_UTDID")); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, _ := os.ReadFile(filepath.Join(home, ".accio", "utdid"))
	return strings.TrimSpace(string(b))
}

func (c *Client) streamLocal(ctx context.Context, request map[string]any, emit func(text, reasoning string, call map[string]any, done bool)) error {
	if !localGatewayAvailable() {
		return errors.New("Accio local gateway is unavailable")
	}
	clientID := uuid.NewString()
	header := http.Header{}
	header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("phoenix:"+localGatewayToken())))
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := dialer.DialContext(ctx, localGatewayURL()+"/websocket/connect?clientId="+clientID, header)
	if err != nil {
		return fmt.Errorf("connect Accio local gateway: %w", err)
	}
	defer ws.Close()

	conversationID := firstString(request, "conversation_id", "conversationId")
	if conversationID == "" {
		conversationID = "conv-" + randomID()
	}
	agentID := strings.TrimSpace(os.Getenv("ACCIO_AGENT_ID"))
	if agentID == "" {
		agentID = "main"
	}
	accountID := localAccountID()
	utdid := localUtdid()
	message := map[string]any{
		"type":   "req",
		"method": "sendQuery",
		"params": map[string]any{
			"conversationId": conversationID,
			"chatType":       "direct",
			"question":       map[string]any{"query": localQuery(request)},
			"path":           "",
			"agentId":        agentID,
			"targetAgentList": []any{map[string]any{
				"agentId": agentID,
				"isTL":    true,
			}},
			"skills":   []any{},
			"language":  "pt-BR",
			"accountId": accountID,
			"utdid":     utdid,
			"ts":        time.Now().UnixMilli(),
			"extra":    map[string]any{},
			"source":   "clientGateWay",
			"atIds":    []any{},
		},
	}
	if model := strings.TrimSpace(firstString(request, "model")); model != "" {
		message["params"].(map[string]any)["model"] = strings.TrimPrefix(model, "accio/")
	}
	if err := ws.WriteJSON(message); err != nil {
		return fmt.Errorf("send Accio local query: %w", err)
	}

	finished := false
	gotContent := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// First content event should arrive quickly; afterwards deltas stream
		// fast. A broken local gateway accepts the query, sends "Thinking..."
		// and then never responds — cap the wait so the caller falls back to
		// the remote gateway instead of hanging.
		readTimeout := 20 * time.Second
		if !gotContent {
			readTimeout = 10 * time.Second
		}
		_ = ws.SetReadDeadline(time.Now().Add(readTimeout))
		_, raw, err := ws.ReadMessage()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				localGatewayFailed()
				return fmt.Errorf("Accio local gateway timed out with no response (disabled temporarily)")
			}
			return fmt.Errorf("read Accio local gateway: %w", err)
		}
		var event localGatewayEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			continue
		}
		if event.Type == "res" && event.Success != nil && !*event.Success {
			return fmt.Errorf("Accio local gateway rejected query (%v): %s", event.Code, event.Message)
		}
		if event.Type == "event" && (event.Method == "append" || event.Method == "finished" || event.Method == "message" || event.Method == "delta" || event.Method == "finalize" || event.Method == "turn.end") {
			text, reasoning, call := localEventContent(event.Payload)
			if text != "" || reasoning != "" || call != nil {
				gotContent = true
				emit(text, reasoning, call, false)
			}
			if event.Method == "finished" || event.Method == "finalize" || event.Method == "turn.end" {
				finished = true
				emit("", "", nil, true)
				return nil
			}
		}
		if event.Type == "row_batch" {
			if events, ok := event.Payload["events"].([]any); ok {
				for _, rawEvent := range events {
					item, _ := rawEvent.(map[string]any)
					method, _ := item["method"].(string)
					payload, _ := item["payload"].(map[string]any)
					text, reasoning, call := localEventContent(payload)
					if delta, ok := payload["contentDelta"].(string); ok {
						text = delta
					}
					if text != "" || reasoning != "" || call != nil {
						gotContent = true
						emit(text, reasoning, call, false)
					}
					if method == "finalize" {
						finished = true
						emit("", "", nil, true)
						return nil
					}
				}
			}
		}
		if event.Type == "ack" {
			if content, ok := event.Payload["content"].(string); ok && content != "Thinking..." && content != "" {
				gotContent = true
				emit(content, "", nil, false)
			}
		}
		if event.Method == "finished" || event.Method == "turn.end" {
			finished = true
			emit("", "", nil, true)
			return nil
		}
	}
	if !finished {
		emit("", "", nil, true)
	}
	return nil
}

func localQuery(request map[string]any) string {
	if messages, ok := request["messages"].([]any); ok {
		for i := len(messages) - 1; i >= 0; i-- {
			if m, ok := messages[i].(map[string]any); ok {
				if role, _ := m["role"].(string); role == "user" {
					if text := textContent(m["content"]); text != "" {
						return text
					}
				}
			}
		}
	}
	return textContent(request["prompt"])
}

func localEventContent(payload map[string]any) (string, string, map[string]any) {
	if payload == nil {
		return "", "", nil
	}
	text, _ := payload["content"].(string)
	if text == "" {
		text, _ = payload["contentDelta"].(string)
	}
	if text == "" {
		if data, ok := payload["data"].(map[string]any); ok {
			text, _ = data["content"].(string)
		}
	}
	reasoning, _ := payload["reasoning_content"].(string)
	if reasoning == "" {
		reasoning, _ = payload["reasoningContent"].(string)
	}
	var call map[string]any
	if calls, ok := payload["tool_calls"].([]any); ok && len(calls) > 0 {
		call, _ = calls[0].(map[string]any)
	}
	return text, reasoning, call
}
