package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"grok-desktop/internal/aistudio/converter"
	"grok-desktop/internal/aistudio/models"
	"grok-desktop/internal/aistudio/profile"
	"grok-desktop/internal/aistudio/prompt"
	"grok-desktop/internal/aistudio/tts"
)

// audioSpeech handles POST /v1/audio/speech.
func (s *Server) audioSpeech(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Input          string  `json:"input"`
		Voice          string  `json:"voice"`
		ResponseFormat string  `json:"response_format"`
		Speed          float64 `json:"speed"`
		ProfileID      string  `json:"profile_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON invalido: "+err.Error(), "invalid_request_error")
		return
	}
	if strings.TrimSpace(body.Input) == "" {
		writeError(w, http.StatusBadRequest, "input (texto) e obrigatorio", "invalid_request_error")
		return
	}
	if body.Voice == "" {
		body.Voice = "zephyr"
	}
	if body.ResponseFormat == "" {
		body.ResponseFormat = "wav"
	}
	if body.Speed == 0 {
		body.Speed = 1.0
	}

	profileID := body.ProfileID
	if profileID == "" {
		profileID = s.readProfileID(r, nil)
	}
	p, err := s.mgr.Profiles().Resolve(profileID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	// TTS com rotacao de contas: se o bootstrap/geracao falhar em um perfil
	// (ex.: timeout de botguard), tenta os demais perfis validos, como o chat.
	tried := map[string]bool{}
	var res *tts.SpeechResult
	var lastErr error
	const maxAttempts = 4
	for attempt := 0; attempt < maxAttempts && p != nil; attempt++ {
		tried[p.ID] = true
		rt, rtErr := s.mgr.GetRuntime(p.ID)
		if rtErr != nil {
			lastErr = rtErr
			p = nextUntriedValidProfile(s.mgr.ListProfiles(), tried)
			continue
		}

		s.mgr.NoteRequest(p.ID)
		res, lastErr = rt.TTS.GenerateSpeech(r.Context(), tts.SpeechOptions{
			Input: body.Input, Voice: body.Voice, ResponseFormat: body.ResponseFormat, Speed: body.Speed,
		})
		if lastErr == nil && res != nil && res.Audio != nil {
			break
		}
		if lastErr != nil {
			s.mgr.NoteFailure(p.ID, lastErr.Error(), 0)
		} else if res != nil {
			s.mgr.NoteFailure(p.ID, res.Error, 0)
			lastErr = errors.New(res.Error)
		}
		res = nil
		p = nextUntriedValidProfile(s.mgr.ListProfiles(), tried)
	}

	if lastErr != nil && res == nil {
		writeError(w, http.StatusBadGateway, lastErr.Error(), "server_error")
		return
	}

	if res.Audio == nil {
		msg := res.Error
		if msg == "" {
			msg = "Falha ao gerar audio"
		}
		s.mgr.NoteFailure(p.ID, msg, 0)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"message": msg, "type": "server_error", "code": 500},
		})
		return
	}
	s.mgr.NoteSuccess(p.ID)

	w.Header().Set("Content-Type", "audio/"+res.Format)
	w.Header().Set("X-Profile-Id", p.ID)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Audio)
}

// nextUntriedValidProfile retorna o primeiro perfil valido ainda nao tentado.
func nextUntriedValidProfile(profiles []profile.Profile, tried map[string]bool) *profile.Profile {
	for i := range profiles {
		p := profiles[i]
		if tried[p.ID] {
			continue
		}
		if p.IsValid != nil && !*p.IsValid {
			continue
		}
		return &p
	}
	return nil
}

// testTools handles POST /v1/chat/completions/test-tools.
func (s *Server) testTools(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewString()

	var body struct {
		Prompt          string          `json:"prompt"`
		Tools           []models.Tool   `json:"tools"`
		ToolChoice      json.RawMessage `json:"tool_choice"`
		ProfileID       string          `json:"profile_id"`
		ToolCallingMode string          `json:"tool_calling_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON invalido: "+err.Error(), "invalid_request_error")
		return
	}
	if len(body.Tools) == 0 {
		writeError(w, http.StatusBadRequest, "Envie tools para testar", "invalid_request_error")
		return
	}

	profileID := body.ProfileID
	if profileID == "" {
		profileID = s.readProfileID(r, nil)
	}
	p, err := s.mgr.Profiles().Resolve(profileID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	rt, err := s.mgr.GetRuntime(p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}

	promptText := body.Prompt
	if promptText == "" {
		promptText = "Use a ferramenta apropriada para ajudar o usuario."
	}
	body.ToolCallingMode = effectiveToolCallingMode(body.ToolCallingMode, string(s.mgr.Config().ToolCalling.Mode))

	injector := prompt.New(s.mgr.Config().PromptInjection.Enabled)
	messages := []models.Message{{Role: "user", Content: jsonStringRaw(promptText)}}
	if s.mgr.Config().PromptInjection.Enabled && strings.ToLower(strings.TrimSpace(body.ToolCallingMode)) != "native_first" {
		messages = injector.InjectToolInstructions(messages, body.Tools, body.ToolChoice)
	}

	opts := converter.RequestOptions{
		Model:           s.mgr.Config().DefaultModel,
		Messages:        messages,
		Tools:           body.Tools,
		ToolChoice:      body.ToolChoice,
		ToolCallingMode: body.ToolCallingMode,
	}

	result, err := rt.Chat.GenerateContent(r.Context(), chatOpts(requestID, opts))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "server_error")
		return
	}
	parsed := converter.ParseGenerateContentResponse(result.Body, converter.ToolParseOptions{Tools: body.Tools, ToolChoice: body.ToolChoice})
	if len(parsed.FunctionCalls) == 0 {
		if extracted := prompt.ExtractToolCallsFromFreeText(joinStrings(parsed.TextParts)); len(extracted) > 0 {
			parsed.FunctionCalls = extracted
		}
	}

	w.Header().Set("X-Profile-Id", p.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"request_id":     requestID,
		"profile_id":     p.ID,
		"status":         result.Status,
		"has_tool_calls": len(parsed.FunctionCalls) > 0,
		"tool_calls":     parsed.FunctionCalls,
		"text":           truncate(joinStrings(parsed.TextParts), 200),
	})
}

func jsonStringRaw(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func joinStrings(parts []string) string {
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(p)
	}
	return sb.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
