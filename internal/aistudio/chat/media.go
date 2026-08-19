package chat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"grok-desktop/internal/aistudio/auth"
	"grok-desktop/internal/aistudio/botguard"
	"grok-desktop/internal/aistudio/models"
)

const uploadContextTTL = 50 * time.Minute

type uploadContext struct {
	AppFolderID        string
	AccessToken        string
	APIKey             string
	AccountFingerprint string
}

type mediaBlob struct {
	FileName string
	MimeType string
	Data     []byte
}

func (c *Client) prepareMessages(ctx context.Context, messages []models.Message, capture *botguard.Capture, authInfo *auth.RuntimeAuth) ([]models.Message, error) {
	result := make([]models.Message, 0, len(messages))
	for _, message := range messages {
		var parts []models.ContentPart
		if err := json.Unmarshal(message.Content, &parts); err != nil || len(parts) == 0 {
			result = append(result, message)
			continue
		}

		prepared := make([]models.ContentPart, 0, len(parts))
		for _, part := range parts {
			next, err := c.prepareContentPart(ctx, part, capture, authInfo)
			if err != nil {
				return nil, err
			}
			prepared = append(prepared, next)
		}
		encoded, _ := json.Marshal(prepared)
		message.Content = encoded
		result = append(result, message)
	}
	return result, nil
}

func (c *Client) prepareContentPart(ctx context.Context, part models.ContentPart, capture *botguard.Capture, authInfo *auth.RuntimeAuth) (models.ContentPart, error) {
	if part.Type == "aistudio_file" && part.FileID != "" {
		return part, nil
	}
	if part.Type == "text" || part.Type == "native_tool_call" || part.Type == "native_tool_result" {
		return part, nil
	}
	source := mediaSource(part)
	if source == "" {
		return part, nil
	}
	if shouldUploadMediaPart(part) {
		if capture == nil || authInfo == nil {
			return part, fmt.Errorf("multimodal: upload de midia requer contexto autenticado do AI Studio")
		}
		blob, err := c.resolveMediaBlob(ctx, part, source)
		if err != nil {
			return part, err
		}
		uploadCtx, err := c.getUploadContext(ctx, capture, authInfo)
		if err != nil {
			return part, fmt.Errorf("multimodal: upload de midia requer Google Drive ativo na conta: %w", err)
		}
		fileID, err := c.uploadDriveFile(ctx, blob, uploadCtx)
		if err != nil {
			return part, err
		}
		return models.ContentPart{
			Type:     "aistudio_file",
			FileID:   fileID,
			MimeType: blob.MimeType,
			Name:     blob.FileName,
		}, nil
	}

	if strings.HasPrefix(source, "data:") {
		return part, nil
	}
	if !shouldInlineMediaPart(part) {
		return part, nil
	}
	blob, err := c.resolveMediaBlob(ctx, part, source)
	if err != nil {
		return part, err
	}
	return inlineMediaPart(part, blob), nil
}

func mediaSource(part models.ContentPart) string {
	if part.VideoURL != nil {
		return part.VideoURL.URL
	}
	if part.AudioURL != nil {
		return part.AudioURL.URL
	}
	if part.ImageURL != nil {
		return part.ImageURL.URL
	}
	if part.URL != "" {
		return part.URL
	}
	return ""
}

func shouldUploadMediaPart(part models.ContentPart) bool {
	if part.Type == "image_url" || part.Type == "image" || part.ImageURL != nil {
		return true
	}
	if part.Type == "video_url" || part.Type == "video" || part.VideoURL != nil {
		return true
	}
	if part.Type == "audio_url" || part.Type == "audio" || part.AudioURL != nil {
		return true
	}
	return strings.HasPrefix(part.MimeType, "image/") ||
		strings.HasPrefix(part.MimeType, "video/") ||
		strings.HasPrefix(part.MimeType, "audio/")
}

func shouldInlineMediaPart(part models.ContentPart) bool {
	return part.Type == "image_url" || part.ImageURL != nil
}

func inlineMediaPart(part models.ContentPart, blob *mediaBlob) models.ContentPart {
	dataURL := "data:" + blob.MimeType + ";base64," + base64.StdEncoding.EncodeToString(blob.Data)
	part.URL = ""
	part.FileID = ""
	part.Data = ""
	part.MimeType = blob.MimeType
	part.Name = blob.FileName

	switch {
	case part.Type == "image_url" || part.ImageURL != nil:
		part.Type = "image_url"
		part.ImageURL = &models.ImageURLPart{URL: dataURL}
	case part.Type == "audio_url" || part.Type == "audio" || part.AudioURL != nil || strings.HasPrefix(blob.MimeType, "audio/"):
		part.Type = "audio_url"
		part.AudioURL = &models.MediaURLPart{URL: dataURL}
	case part.Type == "video_url" || part.Type == "video" || part.VideoURL != nil || strings.HasPrefix(blob.MimeType, "video/"):
		part.Type = "video_url"
		part.VideoURL = &models.MediaURLPart{URL: dataURL}
	}
	return part
}

func (c *Client) resolveMediaBlob(ctx context.Context, part models.ContentPart, source string) (*mediaBlob, error) {
	if strings.HasPrefix(source, "data:") {
		return blobFromDataURL(part, source)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("multimodal: falha ao baixar midia: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("multimodal: download de midia retornou %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	mimeType := resp.Header.Get("Content-Type")
	if idx := strings.Index(mimeType, ";"); idx >= 0 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}
	if mimeType == "" {
		mimeType = guessMimeType(part)
	}
	return &mediaBlob{FileName: deriveFileName(part, source, mimeType), MimeType: mimeType, Data: data}, nil
}

func blobFromDataURL(part models.ContentPart, dataURL string) (*mediaBlob, error) {
	header, payload, ok := strings.Cut(dataURL, ",")
	if !ok || payload == "" {
		return nil, fmt.Errorf("multimodal: data URL invalida")
	}
	mimeType := guessMimeType(part)
	if strings.HasPrefix(header, "data:") {
		if semi := strings.Index(header, ";"); semi > len("data:") {
			mimeType = header[len("data:"):semi]
		}
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("multimodal: base64 invalido: %w", err)
	}
	return &mediaBlob{FileName: deriveFileName(part, "", mimeType), MimeType: mimeType, Data: data}, nil
}

func (c *Client) getUploadContext(ctx context.Context, capture *botguard.Capture, authInfo *auth.RuntimeAuth) (*uploadContext, error) {
	c.uploadMu.Lock()
	if c.uploadContext != nil && c.uploadContext.AccountFingerprint == authInfo.AccountFingerprint && time.Since(c.uploadContextAt) < uploadContextTTL {
		ctx := *c.uploadContext
		c.uploadMu.Unlock()
		return &ctx, nil
	}
	c.uploadMu.Unlock()

	apiKey := firstHeader(authInfo.Headers, "X-Goog-Api-Key")
	if apiKey == "" {
		apiKey = firstHeader(capture.RequestHeaders, "X-Goog-Api-Key")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("multimodal: X-Goog-Api-Key indisponivel")
	}

	baseURL := "https://alkalimakersuite-pa.clients6.google.com/$rpc/google.internal.alkali.applications.makersuite.v1.MakerSuiteService"
	appFolder, err := c.callUploadRPC(ctx, baseURL+"/GetAppFolder", "[]", authInfo)
	if err != nil {
		return nil, err
	}
	accessToken, err := c.callUploadRPC(ctx, baseURL+"/GenerateAccessToken", `["users/me"]`, authInfo)
	if err != nil {
		return nil, err
	}
	if len(appFolder) == 0 || len(accessToken) == 0 {
		return nil, fmt.Errorf("multimodal: bootstrap de upload incompleto")
	}
	next := &uploadContext{
		AppFolderID:        stringValue(appFolder[0]),
		AccessToken:        stringValue(accessToken[0]),
		APIKey:             apiKey,
		AccountFingerprint: authInfo.AccountFingerprint,
	}
	if next.AppFolderID == "" || next.AccessToken == "" {
		return nil, fmt.Errorf("multimodal: contexto de upload invalido")
	}

	c.uploadMu.Lock()
	c.uploadContext = next
	c.uploadContextAt = time.Now()
	c.uploadMu.Unlock()
	return next, nil
}

func (c *Client) callUploadRPC(ctx context.Context, endpoint, body string, authInfo *auth.RuntimeAuth) ([]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range authInfo.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Cookie", authInfo.CookieString)
	req.Header.Set("Origin", "https://aistudio.google.com")
	req.Header.Set("Referer", "https://aistudio.google.com/")
	req.Header.Set("Accept", "*/*")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("multimodal: %s retornou %d: %s", path.Base(endpoint), resp.StatusCode, truncateText(string(data), 300))
	}
	var arr []any
	if err := json.Unmarshal(data, &arr); err != nil {
		return nil, fmt.Errorf("multimodal: resposta invalida de %s: %w", path.Base(endpoint), err)
	}
	return arr, nil
}

func (c *Client) uploadDriveFile(ctx context.Context, blob *mediaBlob, uploadCtx *uploadContext) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	metadata := map[string]any{
		"name":     blob.FileName,
		"parents":  []string{uploadCtx.AppFolderID},
		"mimeType": blob.MimeType,
	}
	metaHeader := textprotoHeader("application/json; charset=UTF-8")
	metaPart, err := writer.CreatePart(metaHeader)
	if err != nil {
		return "", err
	}
	if err := json.NewEncoder(metaPart).Encode(metadata); err != nil {
		return "", err
	}

	fileHeader := textprotoHeader(blob.MimeType)
	fileHeader["Content-Transfer-Encoding"] = []string{"base64"}
	filePart, err := writer.CreatePart(fileHeader)
	if err != nil {
		return "", err
	}
	encoder := base64.NewEncoder(base64.StdEncoding, filePart)
	if _, err := encoder.Write(blob.Data); err != nil {
		return "", err
	}
	if err := encoder.Close(); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	uploadURL := "https://content.googleapis.com/upload/drive/v3/files?uploadType=multipart&key=" + url.QueryEscape(uploadCtx.APIKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+uploadCtx.AccessToken)
	req.Header.Set("Content-Type", "multipart/related; boundary="+writer.Boundary())
	req.Header.Set("Origin", "https://aistudio.google.com")
	req.Header.Set("Referer", "https://aistudio.google.com/")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-JavaScript-User-Agent", "google-api-javascript-client/1.1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("multimodal: upload retornou %d: %s", resp.StatusCode, truncateText(string(data), 300))
	}
	var uploaded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &uploaded); err != nil {
		return "", err
	}
	if uploaded.ID == "" {
		return "", fmt.Errorf("multimodal: upload sem file id")
	}
	return uploaded.ID, nil
}

func textprotoHeader(contentType string) map[string][]string {
	return map[string][]string{"Content-Type": {contentType}}
}

func firstHeader(headers map[string]string, name string) string {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func guessMimeType(part models.ContentPart) string {
	if part.MimeType != "" {
		return part.MimeType
	}
	switch part.Type {
	case "video_url", "video":
		return "video/mp4"
	case "audio_url", "audio":
		return "audio/mpeg"
	case "image_url":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

func deriveFileName(part models.ContentPart, sourceURL, mimeType string) string {
	if sourceURL != "" && !strings.HasPrefix(sourceURL, "data:") {
		if parsed, err := url.Parse(sourceURL); err == nil {
			if name := strings.TrimSpace(path.Base(parsed.Path)); name != "" && name != "." && name != "/" {
				return name
			}
		}
	}
	ext := extensionFromMimeType(mimeType)
	switch part.Type {
	case "video_url", "video":
		return "upload-video" + ext
	case "audio_url", "audio":
		return "upload-audio" + ext
	case "image_url":
		return "upload-image" + ext
	default:
		return "upload-file" + ext
	}
}

func extensionFromMimeType(mimeType string) string {
	if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	switch mimeType {
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	default:
		return ""
	}
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (c *Client) invalidateUploadContext() {
	c.uploadMu.Lock()
	defer c.uploadMu.Unlock()
	c.uploadContext = nil
	c.uploadContextAt = time.Time{}
}
