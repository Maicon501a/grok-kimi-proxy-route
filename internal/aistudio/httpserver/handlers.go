package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"grok-desktop/internal/aistudio/converter"
	"grok-desktop/internal/aistudio/models"
	"grok-desktop/internal/aistudio/profile"
)

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	profiles := s.mgr.ListProfiles()
	accounts := s.accountSummaries(profiles)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"service":   "proxy-ai-studio-plus-go",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"profiles":  profiles,
		"accounts":  accounts,
	})
}

func (s *Server) profiles(w http.ResponseWriter, r *http.Request) {
	profiles := s.mgr.ListProfiles()
	summaries := s.accountSummaries(profiles)
	byID := map[string]map[string]any{}
	for _, sm := range summaries {
		byID[sm.ProfileID] = map[string]any{
			"active_sessions": sm.ActiveSessions,
			"total_requests":  sm.TotalRequests,
			"cooldown_until":  sm.CooldownUntil,
			"available":       sm.Available,
			"last_error":      sm.LastError,
		}
	}

	data := make([]map[string]any, 0, len(profiles))
	for _, p := range profiles {
		entry := map[string]any{
			"id":              p.ID,
			"label":           p.Label,
			"connection_file": p.ConnectionFile,
			"has_ws_endpoint": p.WSEndpoint != "",
			"is_valid":        profileIsValid(p),
		}
		if sm, ok := byID[p.ID]; ok {
			for k, v := range sm {
				entry[k] = v
			}
		} else {
			entry["active_sessions"] = 0
			entry["total_requests"] = 0
			entry["cooldown_until"] = nil
			entry["available"] = true
			entry["last_error"] = nil
		}
		data = append(data, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	summaries := s.accountSummaries(s.mgr.ListProfiles())
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": summaries})
}

func (s *Server) accountSummaries(profiles []profile.Profile) []models.AccountSummary {
	ids := make([]string, 0, len(profiles))
	validByID := make(map[string]bool, len(profiles))
	for _, p := range profiles {
		ids = append(ids, p.ID)
		validByID[p.ID] = profileIsValid(p)
	}

	summaries := s.mgr.Accounts().Summarize(ids, s.mgr.Sessions().CountByProfile())
	for i := range summaries {
		if validByID[summaries[i].ProfileID] {
			continue
		}
		summaries[i].Available = false
		if summaries[i].LastError == "" {
			summaries[i].LastError = "profile_endpoint_unavailable"
		}
	}
	return summaries
}

func profileIsValid(p profile.Profile) bool {
	return p.IsValid == nil || *p.IsValid
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "sessionId obrigatorio", "invalid_request_error")
		return
	}
	sess := s.mgr.Sessions().Get(id)
	if sess == nil {
		writeError(w, http.StatusNotFound, "Sessao nao encontrada: "+id, "not_found")
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	// The original scrapes models from AI Studio; the high-throughput path
	// returns the known default set. A future iteration can re-add the scraper
	// via the CDP client.
	defaults := []map[string]any{
		{
			"id":     "models/gemini-3.1-pro-preview",
			"object": "model", "created": 1700000000, "owned_by": "google",
			"permission": []any{}, "root": "models/gemini-3.1-pro-preview", "parent": nil,
		},
		{
			"id":     "models/gemini-3.7-flash",
			"object": "model", "created": 1700000000, "owned_by": "google",
			"permission": []any{}, "root": "models/gemini-3.7-flash", "parent": nil,
		},
		{
			"id":     "models/gemini-3.6-flash",
			"object": "model", "created": 1700000000, "owned_by": "google",
			"permission": []any{}, "root": "models/gemini-3.6-flash", "parent": nil,
		},
		{
			"id":     "models/gemini-3.5-flash",
			"object": "model", "created": 1700000000, "owned_by": "google",
			"permission": []any{}, "root": "models/gemini-3.5-flash", "parent": nil,
		},
		{
			"id":     "models/gemini-2.5-pro",
			"object": "model", "created": 1700000000, "owned_by": "google",
			"permission": []any{}, "root": "models/gemini-2.5-pro", "parent": nil,
		},
		{
			"id":     "models/gemini-2.5-flash-image",
			"object": "model", "created": 1700000000, "owned_by": "google",
			"permission": []any{}, "root": "models/gemini-2.5-flash-image", "parent": nil,
		},
		{
			"id":     "models/gemini-3.1-flash-tts-preview",
			"object": "model", "created": 1700000000, "owned_by": "google",
			"permission": []any{}, "root": "models/gemini-3.1-flash-tts-preview", "parent": nil,
		},
		{
			"id":     "models/gemma-4-31b-it",
			"object": "model", "created": 1700000000, "owned_by": "google",
			"permission": []any{}, "root": "models/gemma-4-31b-it", "parent": nil,
		},
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": defaults})
}

func (s *Server) limits(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusInternalServerError, "Limites nao expostos pela UI do AI Studio no modo HTTP-first.", "upstream_error")
}

func (s *Server) imagesUpload(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	image, _ := body["image"].(string)
	imageURL, _ := body["image_url"].(string)
	video, _ := body["video"].(string)
	videoURL, _ := body["video_url"].(string)
	mimeType, _ := body["mime_type"].(string)

	if image != "" || imageURL != "" {
		url := imageURL
		if url == "" {
			mt := mimeType
			if mt == "" {
				mt = "image/png"
			}
			url = "data:" + mt + ";base64," + image
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": url},
		})
		return
	}
	if video != "" || videoURL != "" {
		url := videoURL
		if url == "" {
			mt := mimeType
			if mt == "" {
				mt = "video/mp4"
			}
			url = "data:" + mt + ";base64," + video
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"type":      "video_url",
			"video_url": map[string]any{"url": url},
		})
		return
	}
	writeError(w, http.StatusBadRequest, "Envie image, image_url, video, ou video_url", "invalid_request_error")
}

func (s *Server) debugLastResponse(w http.ResponseWriter, r *http.Request) {
	// Without the per-request debug dir wired through the runtime, return empty.
	writeJSON(w, http.StatusOK, map[string]any{"count": 0, "responses": []any{}})
}

// silence unused import warning until chat handlers reference converter helpers.
var _ = converter.ParsedResponse{}
