package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"grok-desktop/internal/aistudio/accountmenu"
	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/logstream"
	"grok-desktop/internal/aistudio/profile"
	"grok-desktop/internal/aistudio/runtime"
)

type Handler struct {
	mgr      *runtime.Manager
	menu     *accountmenu.Menu
	metrics  *MetricsStore
	token    string
	shutdown func()
}

func NewHandler(mgr *runtime.Manager, metrics *MetricsStore) *Handler {
	return NewHandlerWithToken(mgr, metrics, os.Getenv("AISTUDIO_ADMIN_TOKEN"))
}

// NewHandlerWithToken constructs an admin handler without relying on
// process-global environment state. The standalone proxy keeps using
// NewHandler, while the desktop integration supplies its per-instance token.
func NewHandlerWithToken(mgr *runtime.Manager, metrics *MetricsStore, token string) *Handler {
	return &Handler{
		mgr:     mgr,
		menu:    accountmenu.New(mgr),
		metrics: metrics,
		token:   strings.TrimSpace(token),
	}
}

// SetShutdown registers the graceful process shutdown callback used by a
// supervising desktop application.
func (h *Handler) SetShutdown(fn func()) { h.shutdown = fn }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.token != "" && r.Header.Get("X-AIStudio-Admin-Token") != h.token {
		writeError(w, http.StatusUnauthorized, "admin token invalido", "unauthorized")
		return
	}
	if r.URL.Path == "/admin" || r.URL.Path == "/admin/" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dashboardHTML))
		return
	}

	switch r.URL.Path {
	case "/admin/api/stats":
		h.handleStats(w, r)
	case "/admin/api/accounts":
		h.handleAccounts(w, r)
	case "/admin/api/accounts/import":
		h.handleImportAccount(w, r)
	case "/admin/api/accounts/delete":
		h.handleDeleteAccount(w, r)
	case "/admin/api/accounts/default":
		h.handleSetDefault(w, r)
	case "/admin/api/accounts/validate":
		h.handleValidateAccount(w, r)
	case "/admin/api/accounts/rename":
		h.handleRenameAccount(w, r)
	case "/admin/api/accounts/login/start":
		h.handleLoginStart(w, r)
	case "/admin/api/accounts/login/complete":
		h.handleLoginComplete(w, r)
	case "/admin/api/accounts/login/cancel":
		h.handleLoginCancel(w, r)
	case "/admin/api/config":
		h.handleConfig(w, r)
	case "/admin/api/models":
		h.handleModels(w, r)
	case "/admin/api/readme":
		h.handleReadme(w, r)
	case "/admin/api/logs":
		h.handleLogs(w, r)
	case "/admin/api/restart":
		h.handleRestart(w, r)
	case "/admin/api/shutdown":
		h.handleShutdown(w, r)
	case "/admin/api/vnc/status":
		h.handleVNCStatus(w, r)
	case "/admin/api/vnc/attach":
		h.handleVNCAttach(w, r)
	case "/admin/api/vnc/detach":
		h.handleVNCDetach(w, r)
	case "/admin/api/accounts/new":
		h.handleNewAccount(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) handleRenameAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "metodo nao permitido", "method_not_allowed")
		return
	}
	var body struct {
		ProfileID string `json:"profile_id"`
		Label     string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON invalido: "+err.Error(), "invalid_request_error")
		return
	}
	p := h.mgr.Profiles().Get(strings.TrimSpace(body.ProfileID))
	if p == nil {
		writeError(w, http.StatusNotFound, "perfil nao encontrado", "not_found")
		return
	}
	if strings.TrimSpace(body.Label) == "" {
		writeError(w, http.StatusBadRequest, "label obrigatorio", "invalid_request_error")
		return
	}
	next := *p
	next.Label = strings.TrimSpace(body.Label)
	if err := h.menu.PersistProfile(next); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (h *Handler) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "metodo nao permitido", "method_not_allowed")
		return
	}
	var body struct {
		ProfileID string `json:"profile_id"`
		Label     string `json:"label"`
		Email     string `json:"email"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	p, err := h.menu.StartManualLogin(r.Context(), body.ProfileID, body.Label, body.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "login_start_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "profile_id": p.ID, "label": p.Label, "email": p.Email,
		"message": "Chrome aberto. Entre no Google/AI Studio e conclua o login no aplicativo.",
	})
}

func (h *Handler) handleLoginComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "metodo nao permitido", "method_not_allowed")
		return
	}
	var body struct {
		ProfileID string `json:"profile_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON invalido: "+err.Error(), "invalid_request_error")
		return
	}
	p, result, err := h.menu.CompleteManualLogin(r.Context(), body.ProfileID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "login_complete_failed")
		return
	}
	if result == nil || !result.OK {
		message := "login ainda nao detectado"
		if result != nil && strings.TrimSpace(result.Reason) != "" {
			message = result.Reason
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "warning", "profile_id": p.ID, "message": message})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "profile_id": p.ID, "email": p.Email, "label": p.Label})
}

func (h *Handler) handleLoginCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "metodo nao permitido", "method_not_allowed")
		return
	}
	var body struct {
		ProfileID string `json:"profile_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := h.menu.CancelManualLogin(body.ProfileID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (h *Handler) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "metodo nao permitido", "method_not_allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	if h.shutdown != nil {
		go func() { time.Sleep(100 * time.Millisecond); h.shutdown() }()
	}
}

func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	type accountCard struct {
		ID             string `json:"id"`
		Label          string `json:"label"`
		LoggedIn       bool   `json:"logged_in"`
		Active         bool   `json:"active"`
		CooldownUntil  string `json:"cooldown_until,omitempty"`
		Requests       int    `json:"requests"`
		LastError      string `json:"last_error,omitempty"`
		ValidationNote string `json:"validation_note,omitempty"`
	}

	profiles := h.mgr.ListProfiles()
	summaries := h.mgr.Accounts().Summarize(profileIDs(profiles), h.mgr.Sessions().CountByProfile())
	summaryByID := make(map[string]map[string]any, len(summaries))
	for _, summary := range summaries {
		summaryByID[summary.ProfileID] = map[string]any{
			"available":      summary.Available,
			"cooldown_until": summary.CooldownUntil,
			"total_requests": summary.TotalRequests,
			"last_error":     summary.LastError,
		}
	}

	metrics := h.metrics.Snapshot()
	cards := make([]accountCard, 0, len(profiles))
	loggedIn := 0
	active := 0
	totalRequests := int64(0)
	totalTokens := int64(0)
	totalLatency := int64(0)
	successes := int64(0)
	failures := int64(0)
	tokensSupported := false

	for _, p := range profiles {
		valid := p.IsValid == nil || *p.IsValid
		if valid {
			loggedIn++
		}
		available := false
		cooldownUntil := ""
		lastError := strings.TrimSpace(p.ValidationError)
		requests := 0
		if summary, ok := summaryByID[p.ID]; ok {
			available, _ = summary["available"].(bool)
			cooldownUntil, _ = summary["cooldown_until"].(string)
			if v, ok := summary["last_error"].(string); ok && strings.TrimSpace(v) != "" {
				lastError = v
			}
			if v, ok := summary["total_requests"].(int); ok {
				requests = v
			}
		}
		if available {
			active++
		}
		if metric, ok := metrics.ByProfile[p.ID]; ok {
			totalRequests += metric.Requests
			totalTokens += metric.TotalTokens
			totalLatency += metric.AvgLatencyMs
			successes += metric.Successes
			failures += metric.Failures
			tokensSupported = tokensSupported || metric.TokensSupported
			requests = int(metric.Requests)
		}
		cards = append(cards, accountCard{
			ID:             p.ID,
			Label:          firstNonEmpty(p.Label, p.Email, p.ID),
			LoggedIn:       valid,
			Active:         available,
			CooldownUntil:  cooldownUntil,
			Requests:       requests,
			LastError:      lastError,
			ValidationNote: p.LoginMode,
		})
	}

	avgLatency := int64(0)
	if len(metrics.ByProfile) > 0 {
		avgLatency = totalLatency / int64(len(metrics.ByProfile))
	}
	successRate := 100.0
	if successes+failures > 0 {
		successRate = float64(successes) * 100 / float64(successes+failures)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "ok",
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"cards":        cards,
		"stats": map[string]any{
			"logged_accounts":   loggedIn,
			"active_accounts":   active,
			"requests":          totalRequests,
			"tokens_used":       totalTokens,
			"tokens_supported":  tokensSupported,
			"avg_latency_ms":    avgLatency,
			"success_rate":      successRate,
			"prompt_cache_mode": "indisponivel_no_upstream",
		},
		"prompt_cache": map[string]any{
			"status": "indisponivel",
			"docs":   "https://docs.cloud.google.com/gemini-enterprise-agent-platform/models/context-cache/context-cache-overview?hl=pt-br",
		},
		"metrics": metrics,
	})
}

func (h *Handler) handleAccounts(w http.ResponseWriter, r *http.Request) {
	profiles := h.mgr.ListProfiles()
	summaries := h.mgr.Accounts().Summarize(profileIDs(profiles), h.mgr.Sessions().CountByProfile())
	summaryByID := make(map[string]profileSummary, len(summaries))
	for _, summary := range summaries {
		summaryByID[summary.ProfileID] = profileSummary{
			ActiveSessions: summary.ActiveSessions,
			Requests:       summary.TotalRequests,
			CooldownUntil:  summary.CooldownUntil,
			Available:      summary.Available,
			LastError:      summary.LastError,
		}
	}
	metrics := h.metrics.Snapshot()

	rows := make([]map[string]any, 0, len(profiles))
	defaultID := h.mgr.Profiles().DefaultID()
	for _, p := range profiles {
		valid := p.IsValid == nil || *p.IsValid
		summary := summaryByID[p.ID]
		metric := metrics.ByProfile[p.ID]
		if metric == nil {
			metric = &ProfileMetrics{}
		}
		requests := metric.Requests
		if int64(summary.Requests) > requests {
			requests = int64(summary.Requests)
		}
		rows = append(rows, map[string]any{
			"id":               p.ID,
			"label":            firstNonEmpty(p.Label, p.Email, p.ID),
			"email":            p.Email,
			"default":          p.ID == defaultID,
			"is_valid":         valid,
			"login_mode":       p.LoginMode,
			"validation_error": p.ValidationError,
			"last_login_at":    p.LastLoginAt,
			"created_at":       p.CreatedAt,
			"active_sessions":  summary.ActiveSessions,
			"available":        summary.Available,
			"cooldown_until":   summary.CooldownUntil,
			"last_error":       firstNonEmpty(summary.LastError, metric.LastError),
			"requests":         requests,
			"successes":        metric.Successes,
			"failures":         metric.Failures,
			"avg_latency_ms":   metric.AvgLatencyMs,
			"total_tokens":     metric.TotalTokens,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"default_profile_id": defaultID,
		"profiles":           rows,
	})
}

func (h *Handler) handleImportAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "metodo nao permitido", "method_not_allowed")
		return
	}
	var body struct {
		ProfileID   string `json:"profile_id"`
		Label       string `json:"label"`
		Email       string `json:"email"`
		CookiesText string `json:"cookies_text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON invalido: "+err.Error(), "invalid_request_error")
		return
	}
	profileRow, result, err := h.menu.ImportAccountFromCookiesText(body.CookiesText, body.ProfileID, body.Label, body.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	log.Printf("[ADMIN] conta importada via cookies: %s", profileRow.ID)
	if !result.OK {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "warning",
			"profile_id": profileRow.ID,
			"email":      firstNonEmpty(result.Email, profileRow.Email, body.Email),
			"message":    firstNonEmpty(result.Reason, "cookies importados, mas a conta nao passou no probe operacional"),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"profile_id": profileRow.ID,
		"email":      firstNonEmpty(result.Email, profileRow.Email, body.Email),
		"message":    "cookies importados, sessao validada e conta operacional",
	})
}

func (h *Handler) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "metodo nao permitido", "method_not_allowed")
		return
	}
	var body struct {
		ProfileID string `json:"profile_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON invalido: "+err.Error(), "invalid_request_error")
		return
	}
	if err := h.menu.RemoveAccountByID(body.ProfileID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	log.Printf("[ADMIN] conta removida: %s", body.ProfileID)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (h *Handler) handleSetDefault(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "metodo nao permitido", "method_not_allowed")
		return
	}
	var body struct {
		ProfileID string `json:"profile_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON invalido: "+err.Error(), "invalid_request_error")
		return
	}
	if err := h.menu.SetDefaultProfile(body.ProfileID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	log.Printf("[ADMIN] conta padrao alterada: %s", body.ProfileID)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (h *Handler) handleValidateAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "metodo nao permitido", "method_not_allowed")
		return
	}
	var body struct {
		ProfileID string `json:"profile_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON invalido: "+err.Error(), "invalid_request_error")
		return
	}
	p, result, err := h.menu.ValidateAccountContext(r.Context(), body.ProfileID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if !result.OK {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "warning",
			"message": firstNonEmpty(result.Reason, "sessao invalida"),
			"email":   p.Email,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"message": "conta validada",
		"email":   firstNonEmpty(result.Email, p.Email),
	})
}

func (h *Handler) handleConfig(w http.ResponseWriter, r *http.Request) {
	configPath := config.WebOverridesFile(h.mgr.Config().StateDir)
	if r.Method == http.MethodGet {
		values := config.LoadWebOverrides(configPath)
		writeJSON(w, http.StatusOK, map[string]any{
			"browser_mode":       firstNonEmpty(values["AISTUDIO_BROWSER_MODE"], string(h.mgr.Config().AIStudio.BrowserMode)),
			"conversation_mode":  firstNonEmpty(values["AISTUDIO_CONVERSATION_MODE"], string(h.mgr.Config().Conversation.Mode)),
			"tool_calling_mode":  firstNonEmpty(values["AISTUDIO_TOOL_CALLING_MODE"], string(h.mgr.Config().ToolCalling.Mode)),
			"tool_stream_mode":   firstNonEmpty(values["AISTUDIO_TOOL_STREAM_MODE"], string(h.mgr.Config().Streaming.ToolMode)),
			"debug_message_flow": firstNonEmpty(values["AISTUDIO_DEBUG_MESSAGE_FLOW"], boolString(h.mgr.Config().Debug.MessageFlow)),
			"migration_hops":     firstNonEmpty(values["AISTUDIO_MAX_MIGRATION_HOPS"], fmt.Sprintf("%d", h.mgr.Config().Migration.MaxHopsPerRequest)),
			"default_profile":    firstNonEmpty(values["AISTUDIO_DEFAULT_PROFILE"], h.mgr.Profiles().DefaultID(), h.mgr.Config().Profiles.DefaultID),
			"eager_boot":         firstNonEmpty(values["AISTUDIO_EAGER_BOOT"], h.mgr.Config().EagerBoot),
			"cdp_ws_endpoint":    firstNonEmpty(values["CDP_WS_ENDPOINT"], h.mgr.Config().AIStudio.WSEndpoint),
		})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "metodo nao permitido", "method_not_allowed")
		return
	}

	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON invalido: "+err.Error(), "invalid_request_error")
		return
	}
	values := map[string]string{
		"AISTUDIO_BROWSER_MODE":       strings.TrimSpace(body["browser_mode"]),
		"AISTUDIO_CONVERSATION_MODE":  strings.TrimSpace(body["conversation_mode"]),
		"AISTUDIO_TOOL_CALLING_MODE":  strings.TrimSpace(body["tool_calling_mode"]),
		"AISTUDIO_TOOL_STREAM_MODE":   strings.TrimSpace(body["tool_stream_mode"]),
		"AISTUDIO_DEBUG_MESSAGE_FLOW": strings.TrimSpace(body["debug_message_flow"]),
		"AISTUDIO_MAX_MIGRATION_HOPS": strings.TrimSpace(body["migration_hops"]),
		"AISTUDIO_DEFAULT_PROFILE":    strings.TrimSpace(body["default_profile"]),
		"AISTUDIO_EAGER_BOOT":         strings.TrimSpace(body["eager_boot"]),
		"CDP_WS_ENDPOINT":             strings.TrimSpace(body["cdp_ws_endpoint"]),
	}
	if defaultProfile := strings.TrimSpace(body["default_profile"]); defaultProfile != "" {
		if err := h.menu.SetDefaultProfile(defaultProfile); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
	}
	if err := config.SaveWebOverrides(configPath, values); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	log.Printf("[ADMIN] configuracoes persistidas")
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "message": "configuracoes salvas; reinicie o proxy para aplicar"})
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "/v1/models", nil)
	rec := &responseRecorder{header: make(http.Header)}
	h.proxyModels(rec, req)
	for k, values := range rec.header {
		for _, value := range values {
			w.Header().Add(k, value)
		}
	}
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	w.WriteHeader(rec.status)
	_, _ = w.Write(rec.body)
}

func (h *Handler) proxyModels(w http.ResponseWriter, r *http.Request) {
	summaries := map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": "models/gemini-3.1-pro-preview", "name": "Gemini 3.1 Pro", "type": "text"},
			{"id": "models/gemini-3.7-flash", "name": "Gemini 3.7 Flash", "type": "text"},
			{"id": "models/gemini-3.6-flash", "name": "Gemini 3.6 Flash", "type": "text"},
			{"id": "models/gemini-3.5-flash", "name": "Gemini 3.5 Flash", "type": "text"},
			{"id": "models/gemini-2.5-pro", "name": "Gemini 2.5 Pro", "type": "text"},
			{"id": "models/gemini-2.5-flash-image", "name": "Gemini 2.5 Flash Image", "type": "image"},
			{"id": "models/gemini-3.1-flash-tts-preview", "name": "Gemini 3.1 Flash TTS", "type": "audio"},
		},
	}
	writeJSON(w, http.StatusOK, summaries)
}

func (h *Handler) handleReadme(w http.ResponseWriter, r *http.Request) {
	candidates := []string{
		filepath.Join(h.mgr.Config().StateDir, "..", "README.md"),
		filepath.Join(filepath.Dir(mustExecutablePath()), "README.md"),
		"README.md",
	}
	for _, candidate := range candidates {
		if data, err := os.ReadFile(candidate); err == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"content": string(data),
				"path":    candidate,
			})
			return
		}
	}
	writeError(w, http.StatusNotFound, "README nao encontrado", "not_found")
}

func (h *Handler) handleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming nao suportado", "server_error")
		return
	}
	history := logstream.Default().History()
	for _, entry := range history {
		encoded, _ := json.Marshal(entry)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
	}
	flusher.Flush()

	events, cancel := logstream.Default().Subscribe()
	defer cancel()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case entry, ok := <-events:
			if !ok {
				return
			}
			encoded, _ := json.Marshal(entry)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
			flusher.Flush()
		case <-ticker.C:
			_, _ = w.Write([]byte(": keepalive\n\n"))
			flusher.Flush()
		}
	}
}

func (h *Handler) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "metodo nao permitido", "method_not_allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "message": "reinicio solicitado"})
	go func() {
		time.Sleep(500 * time.Millisecond)
		proc, err := os.FindProcess(os.Getpid())
		if err != nil {
			log.Printf("[ADMIN] falha ao localizar processo para reinicio: %v", err)
			return
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			log.Printf("[ADMIN] falha ao sinalizar reinicio: %v", err)
		}
	}()
}

type profileSummary struct {
	ActiveSessions int
	Requests       int
	CooldownUntil  string
	Available      bool
	LastError      string
}

type responseRecorder struct {
	header http.Header
	body   []byte
	status int
}

func (r *responseRecorder) Header() http.Header    { return r.header }
func (r *responseRecorder) WriteHeader(status int) { r.status = status }
func (r *responseRecorder) Write(p []byte) (int, error) {
	r.body = append(r.body, p...)
	return len(p), nil
}

func profileIDs(profiles []profile.Profile) []string {
	out := make([]string, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, p.ID)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func mustExecutablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return exe
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message, errType string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    status,
		},
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// VNC endpoints — per-profile interactive login via noVNC
// ─────────────────────────────────────────────────────────────────────────────

func (h *Handler) handleVNCStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "metodo nao permitido", "method_not_allowed")
		return
	}
	script := resolveScript("vnc-status.sh")
	if script == "" {
		writeError(w, http.StatusInternalServerError, "script vnc-status.sh nao encontrado", "server_error")
		return
	}
	out, err := runShellScript(script)
	if err != nil {
		log.Printf("[ADMIN] vnc-status falhou: %v (output=%s)", err, out)
		writeJSON(w, http.StatusOK, map[string]any{"attached": false, "profile_id": nil})
		return
	}
	var payload map[string]any
	if jsonErr := json.Unmarshal([]byte(out), &payload); jsonErr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"attached": false, "profile_id": nil, "raw": out})
		return
	}
	// Enriquecer com label/email do perfil, se houver um perfil ativo.
	if pid, _ := payload["profile_id"].(string); pid != "" {
		if p := h.mgr.Profiles().Get(pid); p != nil {
			payload["label"] = firstNonEmpty(p.Label, p.Email, p.ID)
			payload["email"] = p.Email
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

func (h *Handler) handleVNCAttach(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "metodo nao permitido", "method_not_allowed")
		return
	}
	var body struct {
		ProfileID string `json:"profile_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON invalido: "+err.Error(), "invalid_request_error")
		return
	}
	if strings.TrimSpace(body.ProfileID) == "" {
		writeError(w, http.StatusBadRequest, "profile_id obrigatorio", "invalid_request_error")
		return
	}
	p := h.mgr.Profiles().Get(body.ProfileID)
	if p == nil {
		writeError(w, http.StatusNotFound, "perfil nao encontrado: "+body.ProfileID, "not_found")
		return
	}
	if strings.TrimSpace(p.UserDataDir) == "" {
		writeError(w, http.StatusBadRequest, "perfil sem userDataDir configurado", "invalid_request_error")
		return
	}
	script := resolveScript("vnc-attach.sh")
	if script == "" {
		writeError(w, http.StatusInternalServerError, "script vnc-attach.sh nao encontrado", "server_error")
		return
	}
	// Desconecta o runtime desse perfil para liberar o Chrome headless.
	h.mgr.DisconnectProfile(p.ID)
	out, err := runShellScript(script, p.ID, p.UserDataDir)
	if err != nil {
		log.Printf("[ADMIN] vnc-attach falhou para %s: %v (output=%s)", p.ID, err, out)
		writeError(w, http.StatusInternalServerError, "vnc-attach falhou: "+err.Error(), "server_error")
		return
	}
	log.Printf("[ADMIN] VNC attached para perfil %s (%s)", p.ID, firstNonEmpty(p.Email, p.Label))
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"profile_id": p.ID,
		"email":      p.Email,
		"label":      firstNonEmpty(p.Label, p.Email, p.ID),
		"vnc_url":    "/vnc.html",
		"message":    "Chrome visivel conectado ao VNC. Abra http://<host>:6080/vnc.html para fazer login.",
	})
}

func (h *Handler) handleVNCDetach(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "metodo nao permitido", "method_not_allowed")
		return
	}
	var body struct {
		ProfileID string `json:"profile_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON invalido: "+err.Error(), "invalid_request_error")
		return
	}
	if strings.TrimSpace(body.ProfileID) == "" {
		writeError(w, http.StatusBadRequest, "profile_id obrigatorio", "invalid_request_error")
		return
	}
	p := h.mgr.Profiles().Get(body.ProfileID)
	if p == nil {
		writeError(w, http.StatusNotFound, "perfil nao encontrado: "+body.ProfileID, "not_found")
		return
	}
	script := resolveScript("vnc-detach.sh")
	if script == "" {
		writeError(w, http.StatusInternalServerError, "script vnc-detach.sh nao encontrado", "server_error")
		return
	}
	out, err := runShellScript(script, p.ID, p.UserDataDir)
	if err != nil {
		log.Printf("[ADMIN] vnc-detach falhou para %s: %v (output=%s)", p.ID, err, out)
		writeError(w, http.StatusInternalServerError, "vnc-detach falhou: "+err.Error(), "server_error")
		return
	}
	// Marca o login como feito via VNC e valida a sessao.
	_, result, validateErr := h.menu.ValidateAccount(p.ID)
	loginMode := "vnc_browser"
	valid := validateErr == nil && result != nil && result.OK
	if validateErr != nil {
		log.Printf("[ADMIN] pos-detach validate falhou para %s: %v", p.ID, validateErr)
	}
	// Atualiza o profile com loginMode + isValid e persiste.
	next := *p
	next.LoginMode = loginMode
	next.LastLoginAt = time.Now().UTC().Format(time.RFC3339)
	next.IsValid = &valid
	if valid {
		next.ValidationError = ""
		if result != nil && result.Email != "" {
			next.Email = result.Email
		}
	} else {
		reason := "sessao VNC invalida"
		if validateErr != nil {
			reason = validateErr.Error()
		} else if result != nil && result.Reason != "" {
			reason = result.Reason
		}
		next.ValidationError = reason
	}
	if persistErr := persistProfileViaMenu(h.menu, next); persistErr != nil {
		log.Printf("[ADMIN] persistProfile pos-detach falhou: %v", persistErr)
	}
	log.Printf("[ADMIN] VNC detached para perfil %s (valid=%t)", p.ID, valid)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"profile_id": p.ID,
		"valid":      valid,
		"message":    condMsg(valid, "sessao validada; proxy retornara ao modo headless_spoof", "sessao VNC invalida; verifique o login e tente novamente"),
	})
}

// handleNewAccount cria um perfil vazio com o próximo ID disponível, pronto
// para login via noVNC. O usuário clica em "Adicionar nova conta" e depois
// usa "Login via noVNC" no novo card.
func (h *Handler) handleNewAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "metodo nao permitido", "method_not_allowed")
		return
	}
	var body struct {
		Email string `json:"email"`
		Label string `json:"label"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	existing := h.mgr.ListProfiles()
	usedIDs := make(map[string]bool, len(existing))
	for _, p := range existing {
		usedIDs[p.ID] = true
	}
	newID := ""
	for i := 1; i <= 999; i++ {
		candidate := fmt.Sprintf("%d", i)
		if !usedIDs[candidate] {
			newID = candidate
			break
		}
	}
	if newID == "" {
		writeError(w, http.StatusBadRequest, "nenhum ID disponivel", "invalid_request_error")
		return
	}

	accountsRoot := filepath.Join(h.mgr.Config().StateDir, "accounts")
	accountDir := filepath.Join(accountsRoot, newID)
	if err := os.MkdirAll(filepath.Join(accountDir, "user-data", "Default"), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}

	label := strings.TrimSpace(body.Label)
	if label == "" {
		label = strings.TrimSpace(body.Email)
	}
	if label == "" {
		label = "Conta " + newID
	}

	newProfile := profile.Profile{
		ID:             newID,
		Label:          label,
		Email:          strings.TrimSpace(body.Email),
		ConnectionFile: filepath.Join(accountDir, "connection.json"),
		UserDataDir:    filepath.Join(accountDir, "user-data"),
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		LoginMode:      "vnc_pending",
	}
	valid := false
	newProfile.IsValid = &valid
	newProfile.ValidationError = "aguardando login via noVNC"

	if err := persistProfileViaMenu(h.menu, newProfile); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	log.Printf("[ADMIN] nova conta criada: %s (%s)", newID, label)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"profile_id": newID,
		"label":      label,
		"email":      newProfile.Email,
		"message":    "Conta criada. Clique em 'Login via noVNC' para autenticar.",
	})
}

// resolveScript localiza um script auxiliar em /app/scripts/ (container) ou
// ao lado do executável (dev). Retorna "" se não for encontrado.
func resolveScript(name string) string {
	candidates := []string{
		filepath.Join("/app/scripts", name),
		filepath.Join(filepath.Dir(mustExecutablePath()), "scripts", name),
		filepath.Join("scripts", name),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// runShellScript executa um script com argumentos e retorna stdout + error.
// Impõe timeout de 30s para não bloquear o handler HTTP indefinidamente
// caso o script trave ou espere I/O que nunca chega (ex.: Chrome em
// background mantendo stdout aberto via pipes herdados).
func runShellScript(script string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmdArgs := append([]string{script}, args...)
	cmd := exec.CommandContext(ctx, "bash", cmdArgs...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return strings.TrimSpace(string(out)), fmt.Errorf("timeout de 30s executando script (output parcial: %s)", string(out))
	}
	return strings.TrimSpace(string(out)), err
}

// persistProfileViaMenu usa o menu existente para persistir um perfil sem
// duplicar a lógica de gravação de profiles.json.
func persistProfileViaMenu(menu *accountmenu.Menu, p profile.Profile) error {
	return menu.PersistProfile(p)
}

func condMsg(cond bool, ifTrue, ifFalse string) string {
	if cond {
		return ifTrue
	}
	return ifFalse
}
