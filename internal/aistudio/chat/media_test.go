package chat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"grok-desktop/internal/aistudio/models"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestPrepareContentPartRequiresAuthForVideoUpload(t *testing.T) {
	client := &Client{}
	part := models.ContentPart{
		Type:     "video_url",
		VideoURL: &models.MediaURLPart{URL: "data:video/mp4;base64,YWJj"},
	}

	_, err := client.prepareContentPart(context.Background(), part, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "contexto autenticado") {
		t.Fatalf("expected auth context error, got %v", err)
	}
}

func TestPrepareContentPartRequiresAuthForImageUpload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("abc"))
	}))
	defer server.Close()

	client := &Client{http: server.Client()}
	part := models.ContentPart{
		Type:     "image_url",
		ImageURL: &models.ImageURLPart{URL: server.URL + "/image.png"},
	}

	got, err := client.prepareContentPart(context.Background(), part, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "contexto autenticado") {
		t.Fatalf("expected auth context error, got %v", err)
	}
	if got.Type != "image_url" {
		t.Fatalf("expected original image part on error, got %#v", got)
	}
}

func TestUploadDriveFileUsesBrowserBase64Multipart(t *testing.T) {
	var requestBody string
	var contentType string
	var authorization string
	client := &Client{http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		requestBody = string(data)
		contentType = req.Header.Get("Content-Type")
		authorization = req.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"file-123"}`)),
			Request:    req,
		}, nil
	})}}

	fileID, err := client.uploadDriveFile(context.Background(), &mediaBlob{
		FileName: "clip.mp4",
		MimeType: "video/mp4",
		Data:     []byte("abc"),
	}, &uploadContext{
		AppFolderID: "folder-123",
		AccessToken: "token-123",
		APIKey:      "api-key-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if fileID != "file-123" {
		t.Fatalf("expected uploaded file id, got %q", fileID)
	}
	if authorization != "Bearer token-123" {
		t.Fatalf("unexpected authorization header %q", authorization)
	}
	if !strings.HasPrefix(contentType, "multipart/related; boundary=") {
		t.Fatalf("unexpected content type %q", contentType)
	}
	if !strings.Contains(requestBody, `"parents":["folder-123"]`) {
		t.Fatalf("expected Drive parent metadata in multipart body: %s", requestBody)
	}
	if !strings.Contains(requestBody, "Content-Transfer-Encoding: base64") {
		t.Fatalf("expected browser-compatible base64 transfer encoding: %s", requestBody)
	}
	if !strings.Contains(requestBody, "\r\nYWJj\r\n") {
		t.Fatalf("expected base64 media payload, got: %s", requestBody)
	}
}
