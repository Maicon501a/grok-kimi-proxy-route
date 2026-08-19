package httpserver

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"grok-desktop/internal/aistudio/chat"
	"grok-desktop/internal/aistudio/converter"
	"grok-desktop/internal/aistudio/models"
	"grok-desktop/internal/aistudio/runtime"
)

const defaultImageGenerationModel = "models/gemini-2.5-flash-image"
const defaultImageGenerationSize = "1024x1024"

var supportedImageAspectRatios = map[string]bool{
	"1:1":  true,
	"9:16": true,
	"16:9": true,
	"3:4":  true,
	"4:3":  true,
	"3:2":  true,
	"2:3":  true,
	"5:4":  true,
	"4:5":  true,
	"21:9": true,
}

type generatedImageStore struct {
	mu    sync.Mutex
	blobs map[string]generatedImageBlob
}

type generatedImageBlob struct {
	MimeType string
	Data     []byte
}

func newGeneratedImageStore() *generatedImageStore {
	return &generatedImageStore{blobs: map[string]generatedImageBlob{}}
}

func (s *generatedImageStore) put(mimeType, b64 string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	s.mu.Lock()
	s.blobs[id] = generatedImageBlob{MimeType: mimeType, Data: data}
	s.mu.Unlock()
	return id, nil
}

func (s *generatedImageStore) get(id string) (generatedImageBlob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	blob, ok := s.blobs[id]
	return blob, ok
}

// imagesGenerations handles POST /v1/images/generations.
func (s *Server) imagesGenerations(w http.ResponseWriter, r *http.Request) {
	var body models.ImageGenerationRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON invalido: "+err.Error(), "invalid_request_error")
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt e obrigatorio", "invalid_request_error")
		return
	}
	if body.N != nil && *body.N != 1 {
		writeError(w, http.StatusBadRequest, "apenas n=1 e suportado pelo proxy em modo AI Studio", "invalid_request_error")
		return
	}
	if size := strings.TrimSpace(body.Size); size != "" && size != defaultImageGenerationSize {
		writeError(w, http.StatusBadRequest, "apenas size=1024x1024 e suportado atualmente", "invalid_request_error")
		return
	}
	aspectRatio, ok := normalizeImageAspectRatio(body.AspectRatio)
	if !ok {
		writeError(w, http.StatusBadRequest, "aspect_ratio suportado: auto, 1:1, 9:16, 16:9, 3:4, 4:3, 3:2, 2:3, 5:4, 4:5, 21:9", "invalid_request_error")
		return
	}

	responseFormat := strings.TrimSpace(body.ResponseFormat)
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	if responseFormat != "b64_json" && responseFormat != "url" {
		writeError(w, http.StatusBadRequest, "response_format suportado: b64_json ou url", "invalid_request_error")
		return
	}

	profileID := strings.TrimSpace(body.ProfileID)
	if profileID == "" {
		profileID = s.readProfileID(r, nil)
	}

	activeProfileID, explicitProfile, rt, triedProfileIDs, err := s.resolveImageRuntime(profileID)
	if err != nil {
		writeError(w, errStatus(err), err.Error(), "server_error")
		return
	}

	modelName := strings.TrimSpace(body.Model)
	if modelName == "" {
		modelName = defaultImageGenerationModel
	}

	opts := converter.RequestOptions{
		Model:            modelName,
		Messages:         []models.Message{{Role: "user", Content: jsonStringRaw(body.Prompt)}},
		ImageAspectRatio: aspectRatio,
		Stream:           false,
	}

	cfg := s.mgr.Config()
	maxAttempts := cfg.MaxRetries + 1

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		s.mgr.NoteRequest(activeProfileID)
		result, reqErr := rt.Chat.GenerateContent(r.Context(), chat.GenerateOptions{
			RequestID:      "img-" + uuid.NewString(),
			RequestOptions: opts,
		})
		if reqErr != nil {
			s.mgr.NoteFailure(activeProfileID, reqErr.Error(), cooldownForError(reqErr, cfg))
			if !explicitProfile && cfg.Migration.Enabled && shouldMigrate(reqErr, nil) && attempt < maxAttempts {
				nextProfile, nextRuntime, switched := s.trySwitchImageProfile(activeProfileID, reqErr.Error(), triedProfileIDs)
				if switched {
					activeProfileID = nextProfile
					rt = nextRuntime
					triedProfileIDs = appendUniqueString(triedProfileIDs, nextProfile)
					continue
				}
			}
			if attempt < maxAttempts {
				select {
				case <-time.After(time.Duration(cfg.RetryDelayMs) * time.Millisecond):
				case <-r.Context().Done():
					writeError(w, http.StatusBadGateway, r.Context().Err().Error(), "server_error")
					return
				}
				continue
			}
			writeError(w, http.StatusBadGateway, reqErr.Error(), "server_error")
			return
		}

		if result.Status != http.StatusOK {
			s.mgr.NoteFailure(activeProfileID, "upstream_status_"+itoa(result.Status), cooldownForStatus(result.Status, cfg))
			if !explicitProfile && cfg.Migration.Enabled && shouldMigrateStatus(result.Status) && attempt < maxAttempts {
				nextProfile, nextRuntime, switched := s.trySwitchImageProfile(activeProfileID, "upstream_status_"+itoa(result.Status), triedProfileIDs)
				if switched {
					activeProfileID = nextProfile
					rt = nextRuntime
					triedProfileIDs = appendUniqueString(triedProfileIDs, nextProfile)
					continue
				}
			}
			if shouldRetryStatus(result.Status) && attempt < maxAttempts {
				select {
				case <-time.After(time.Duration(cfg.RetryDelayMs) * time.Millisecond):
				case <-r.Context().Done():
					writeError(w, http.StatusBadGateway, r.Context().Err().Error(), "server_error")
					return
				}
				continue
			}
			writeError(w, statusOrDefault(result.Status), "AI Studio retornou status "+itoa(result.Status), "upstream_error")
			return
		}

		s.mgr.NoteSuccess(activeProfileID)
		parsed := converter.ParseGenerateContentResponse(result.Body, converter.ToolParseOptions{})
		if len(parsed.Images) == 0 {
			writeError(w, http.StatusBadGateway, "AI Studio respondeu sem imagens inline neste GenerateContent", "upstream_error")
			return
		}

		w.Header().Set("X-Profile-Id", activeProfileID)
		if responseFormat == "url" {
			resp, buildErr := s.buildOpenAIImageURLResponse(r, parsed)
			if buildErr != nil {
				writeError(w, http.StatusBadGateway, "falha ao preparar URLs de imagem: "+buildErr.Error(), "server_error")
				return
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
		writeJSON(w, http.StatusOK, converter.ToOpenAIImageResponse(parsed))
		return
	}

	writeError(w, http.StatusBadGateway, "Todas as tentativas falharam", "server_error")
}

func normalizeImageAspectRatio(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	if value == "" || strings.EqualFold(value, "auto") {
		return "", true
	}
	if supportedImageAspectRatios[value] {
		return value, true
	}
	return "", false
}

func (s *Server) resolveImageRuntime(profileID string) (activeProfileID string, explicit bool, rt *runtime.Runtime, triedProfileIDs []string, err error) {
	if profileID != "" {
		p, resolveErr := s.mgr.Profiles().Resolve(profileID)
		if resolveErr != nil {
			return "", true, nil, nil, &routeError{
				status:  profileRouteStatus(resolveErr),
				message: resolveErr.Error(),
			}
		}
		rt, err = s.mgr.GetRuntime(p.ID)
		if err != nil {
			return "", true, nil, nil, err
		}
		return p.ID, true, rt, []string{p.ID}, nil
	}

	p, getErr := s.mgr.GetActiveChatProfile(nil)
	if getErr != nil {
		return "", false, nil, nil, getErr
	}
	rt, err = s.mgr.GetRuntime(p.ID)
	if err != nil {
		return "", false, nil, nil, err
	}
	return p.ID, false, rt, []string{p.ID}, nil
}

func (s *Server) trySwitchImageProfile(fromProfileID, reason string, excludedIDs []string) (string, *runtime.Runtime, bool) {
	next, err := s.mgr.SwitchActiveChatProfile(fromProfileID, reason, excludedIDs)
	if err != nil || next == nil {
		return "", nil, false
	}
	rt, err := s.mgr.GetRuntime(next.ID)
	if err != nil {
		return "", nil, false
	}
	return next.ID, rt, true
}

func (s *Server) buildOpenAIImageURLResponse(r *http.Request, parsed converter.ParsedResponse) (models.ImageGenerationResponse, error) {
	resp := models.ImageGenerationResponse{
		Created: time.Now().Unix(),
		Data:    make([]models.GeneratedImage, 0, len(parsed.Images)),
	}
	baseURL := requestBaseURL(r)
	for _, img := range parsed.Images {
		id, err := s.images.put(img.MimeType, img.Data)
		if err != nil {
			return models.ImageGenerationResponse{}, err
		}
		resp.Data = append(resp.Data, models.GeneratedImage{
			URL: baseURL + "/v1/images/generated/" + id,
		})
	}
	return resp, nil
}

func (s *Server) generatedImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Metodo nao suportado", "invalid_request_error")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/images/generated/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "image id obrigatorio", "invalid_request_error")
		return
	}
	blob, ok := s.images.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "Imagem nao encontrada", "not_found")
		return
	}
	w.Header().Set("Content-Type", blob.MimeType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob.Data)
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		scheme = forwarded
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	return scheme + "://" + host
}
