package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	stdlog "log"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"grok-desktop/internal/aistudio/chat"
	"grok-desktop/internal/aistudio/converter"
	"grok-desktop/internal/aistudio/models"
	"grok-desktop/internal/aistudio/profile"
	"grok-desktop/internal/aistudio/prompt"
	"grok-desktop/internal/aistudio/runtime"
	"grok-desktop/internal/aistudio/session"
)

// chatOpts wraps a converter.RequestOptions into a chat.GenerateOptions with no
// streaming callback.
func chatOpts(requestID string, opts converter.RequestOptions) chat.GenerateOptions {
	return chat.GenerateOptions{RequestID: requestID, RequestOptions: opts}
}

var defaultLogger = stdlog.Default()

// chatCompletions handles both streaming and non-streaming chat requests.
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewString()

	var req models.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "JSON invalido: "+err.Error(), "invalid_request_error")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages e obrigatorio e deve ser um array", "invalid_request_error")
		return
	}
	req.ToolCallingMode = effectiveToolCallingMode(req.ToolCallingMode, string(s.mgr.Config().ToolCalling.Mode))

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	chatCtx, rt, err := s.resolveChatContext(r, &req)
	if err != nil {
		writeError(w, errStatus(err), err.Error(), "server_error")
		return
	}

	injector := prompt.New(s.mgr.Config().PromptInjection.Enabled)
	enhanced := req.Messages
	nativeSystemInstruction := ""
	if shouldUsePromptBridge(req.Tools, req.ToolCallingMode, s.mgr.Config().PromptInjection.Enabled) {
		prepared := injector.PrepareRequest(req.Messages, req.Tools, req.ToolChoice)
		enhanced = prepared.Messages
		nativeSystemInstruction = prepared.SystemInstruction
	}
	if s.mgr.Config().Debug.MessageFlow {
		defaultLogger.Printf(
			"[REQ %s] msgflow handler session=%s inbound=%d enhanced=%d tools=%d promptBridge=%t roles_in=%s roles_enhanced=%s sig_in=%s sig_enhanced=%s",
			requestID,
			chatCtx.SessionID,
			len(req.Messages),
			len(enhanced),
			len(req.Tools),
			shouldUsePromptBridge(req.Tools, req.ToolCallingMode, s.mgr.Config().PromptInjection.Enabled),
			messageRoleSummary(req.Messages),
			messageRoleSummary(enhanced),
			messageFlowSummary(req.Messages),
			messageFlowSummary(enhanced),
		)
	}

	opts := converter.RequestOptions{
		Model:             req.Model,
		Messages:          enhanced,
		SystemInstruction: nativeSystemInstruction,
		ThinkingLevel:     req.ThinkingLevel,
		Tools:             req.Tools,
		ToolChoice:        req.ToolChoice,
		ToolCallingMode:   req.ToolCallingMode,
		Temperature:       req.Temperature,
		TopP:              req.TopP,
		MaxTokens:         req.MaxTokens,
		Stream:            req.Stream,
		SafetySettings:    req.SafetySettings,
	}

	defaultLogger.Printf("[REQ %s] profile=%s session=%s model=%s tools=%d stream=%t",
		requestID, chatCtx.ProfileID, chatCtx.SessionID, orModel(req.Model, s.mgr.Config().DefaultModel), len(req.Tools), req.Stream)

	if req.Stream {
		s.handleStream(ctx, w, requestID, opts, req.Tools, chatCtx, rt, injector)
		return
	}
	s.handleStandard(ctx, w, requestID, opts, req.Tools, chatCtx, rt, injector)
}

type resolvedContext struct {
	SessionID             string
	ProfileID             string
	ExplicitProfileLocked bool
	TriedProfileIDs       []string
}

// resolveChatContext builds the per-request routing context and ensures the
// session exists.
func (s *Server) resolveChatContext(r *http.Request, req *models.ChatRequest) (*resolvedContext, *runtime.Runtime, error) {
	mgr := s.mgr
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		sessionID = firstNonEmptyHeader(r, "X-Session-Id", "X-Session-Affinity", "X-Deepsproxy-Session-Id")
	}
	if sessionID == "" {
		sessionID = uuid.NewString()
	}

	explicitProfile := strings.TrimSpace(req.ProfileID)
	if explicitProfile == "" {
		explicitProfile = s.readProfileID(r, nil)
	}
	if sess := mgr.Sessions().Get(sessionID); sess != nil && explicitProfile != "" && sess.ProfileID != "" && sess.ProfileID != explicitProfile {
		return nil, nil, &routeError{
			status:  http.StatusConflict,
			message: "A sessao " + sessionID + " ja esta vinculada ao perfil " + sess.ProfileID + "; nao pode usar " + explicitProfile + ".",
		}
	}

	var profileID string
	if sess := mgr.Sessions().Get(sessionID); sess != nil && explicitProfile == "" && sess.ProfileID != "" {
		if p, perr := mgr.Profiles().Resolve(sess.ProfileID); perr == nil {
			profileID = p.ID
		}
	}
	if explicitProfile != "" {
		p, perr := mgr.Profiles().Resolve(explicitProfile)
		if perr != nil {
			return nil, nil, &routeError{
				status:  profileRouteStatus(perr),
				message: perr.Error(),
			}
		}
		profileID = p.ID
	}
	if profileID == "" {
		p, perr := mgr.GetActiveChatProfile(nil)
		if perr != nil {
			return nil, nil, perr
		}
		profileID = p.ID
	}

	mgr.Sessions().EnsureSession(sessionID, session.SessionMeta{
		ProfileID: profileID,
		LastModel: orModel(req.Model, s.mgr.Config().DefaultModel),
	})

	rt, err := mgr.GetRuntime(profileID)
	if err != nil {
		return nil, nil, err
	}
	return &resolvedContext{
		SessionID:             sessionID,
		ProfileID:             profileID,
		ExplicitProfileLocked: explicitProfile != "",
		TriedProfileIDs:       []string{profileID},
	}, rt, nil
}

type routeError struct {
	status  int
	message string
}

func (e *routeError) Error() string { return e.message }

func errStatus(err error) int {
	if re, ok := err.(*routeError); ok {
		return re.status
	}
	return http.StatusInternalServerError
}

func profileRouteStatus(err error) int {
	switch {
	case errors.Is(err, profile.ErrUnknownProfile):
		return http.StatusBadRequest
	case errors.Is(err, profile.ErrProfileUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func firstNonEmptyHeader(r *http.Request, names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(r.Header.Get(n)); v != "" {
			return v
		}
	}
	return ""
}

func orModel(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func shouldUsePromptBridge(tools []models.Tool, toolCallingMode string, enabled bool) bool {
	return len(tools) > 0 && enabled && strings.ToLower(strings.TrimSpace(toolCallingMode)) != "native_first"
}

func effectiveToolCallingMode(requested, configured string) string {
	switch mode := strings.ToLower(strings.TrimSpace(requested)); mode {
	case "bridge_first", "native_first":
		return mode
	}
	switch mode := strings.ToLower(strings.TrimSpace(configured)); mode {
	case "bridge_first", "native_first":
		return mode
	}
	return "native_first"
}

func messageRoleSummary(messages []models.Message) string {
	if len(messages) == 0 {
		return "<empty>"
	}
	parts := make([]string, 0, len(messages))
	limit := len(messages)
	if limit > 24 {
		limit = 24
	}
	for i := 0; i < limit; i++ {
		role := strings.TrimSpace(messages[i].Role)
		if role == "" {
			role = "?"
		}
		parts = append(parts, role)
	}
	if len(messages) > limit {
		parts = append(parts, "...(+"+itoa(len(messages)-limit)+")")
	}
	return strings.Join(parts, ">")
}

func messageFlowSummary(messages []models.Message) string {
	if len(messages) == 0 {
		return "<empty>"
	}
	parts := make([]string, 0, len(messages))
	limit := len(messages)
	if limit > 16 {
		limit = 16
	}
	for i := 0; i < limit; i++ {
		parts = append(parts, messageSignature(i, messages[i]))
	}
	if len(messages) > limit {
		parts = append(parts, fmt.Sprintf("...(+%d)", len(messages)-limit))
	}
	return strings.Join(parts, " | ")
}

func messageSignature(index int, msg models.Message) string {
	role := strings.TrimSpace(msg.Role)
	if role == "" {
		role = "?"
	}
	if s := plainContent(msg.Content); s != "" {
		return fmt.Sprintf("%d:%s:text:%d:%s", index, role, len(s), shortHash(s))
	}
	if len(msg.ToolCalls) > 0 {
		return fmt.Sprintf("%d:%s:toolcalls:%d:%s", index, role, len(msg.ToolCalls), shortHash(string(msg.Content)))
	}
	if len(msg.Content) > 0 {
		return fmt.Sprintf("%d:%s:json:%d:%s", index, role, len(msg.Content), shortHash(string(msg.Content)))
	}
	if msg.ToolCallID != "" {
		return fmt.Sprintf("%d:%s:tool-result:%s", index, role, shortHash(msg.ToolCallID))
	}
	return fmt.Sprintf("%d:%s:empty", index, role)
}

func shortHash(value string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(value))
	return fmt.Sprintf("%08x", h.Sum32())
}
