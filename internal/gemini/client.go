// Package gemini connects GrokDesktop to its supervised local AI Studio
// runtime. The runtime exposes an OpenAI-compatible API and owns browser/CDP,
// Botguard, account rotation, and native function calling.
package gemini

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"grok-desktop/internal/store"
)

var endpoint struct {
	sync.RWMutex
	baseURL string
}

func SetBaseURL(baseURL string) {
	endpoint.Lock()
	endpoint.baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	endpoint.Unlock()
}

func BaseURL() string {
	endpoint.RLock()
	defer endpoint.RUnlock()
	return endpoint.baseURL
}

type Client struct {
	HTTP    *http.Client
	BaseURL string
}

func New() *Client { return &Client{HTTP: http.DefaultClient, BaseURL: BaseURL()} }

func ListModels(ctx context.Context, settings store.Settings) []string {
	_ = ctx
	_ = settings
	return []string{
		"gemini-3.7-flash",
		"gemini-3.6-flash",
		"gemini-3.5-flash",
		"gemini-3.1-pro-preview",
		"gemini-2.5-pro",
		"gemini-2.5-flash",
		"gemini-2.5-flash-image",
	}
}

func (c *Client) StreamEvents(
	ctx context.Context,
	settings store.Settings,
	model string,
	messages []map[string]any,
	emit func(kind, text string),
	effort string,
) error {
	_ = settings
	body := map[string]any{"model": model, "messages": messages, "stream": true}
	if strings.TrimSpace(effort) != "" {
		body["reasoning_effort"] = effort
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatURL(), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError(resp)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if text := stringValue(delta["reasoning_content"]); text != "" && emit != nil {
			emit("thinking", text)
		}
		if text := stringValue(delta["reasoning"]); text != "" && emit != nil {
			emit("thinking", text)
		}
		if text := stringValue(delta["content"]); text != "" && emit != nil {
			emit("content", text)
		}
	}
	return scanner.Err()
}

func (c *Client) ChatCompletions(ctx context.Context, settings store.Settings, body map[string]any) (map[string]any, error) {
	_ = settings
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatURL(), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, responseError(resp)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) chatURL() string {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		base = BaseURL()
	}
	return base + "/v1/chat/completions"
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func responseError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return fmt.Errorf("AI Studio runtime %s: %s", resp.Status, strings.TrimSpace(string(raw)))
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func NormalizeModel(id string) string {
	id = strings.TrimSpace(id)
	if i := strings.LastIndex(id, "/models/"); i >= 0 {
		return id[i+len("/models/"):]
	}
	return id
}

var ErrLocalOnly = fmt.Errorf("AI Studio runtime unavailable")
