// Package tts renders text to speech through the AI Studio MakerSuite
// GenerateContent endpoint using the HTTP direct path (same transport and
// Botguard signing as chat). No browser UI is involved: the payload template
// abaixo foi derivado do request real que o frontend do AI Studio monta.
package tts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"grok-desktop/internal/aistudio/chat"
)

const ttsModel = "models/gemini-3.1-flash-tts-preview"

// SpeechOptions configures a TTS request.
type SpeechOptions struct {
	Input          string
	Voice          string
	ResponseFormat string
	Speed          float64
	Scene          string
}

// SpeechResult is the produced audio.
type SpeechResult struct {
	Audio    []byte
	Format   string
	MimeType string
	Error    string
}

// Client renders speech via the signed HTTP direct path.
type Client struct {
	chat     *chat.Client
	stateDir string
}

// New creates a TTS client bound to the profile's chat client.
func New(chatClient *chat.Client) *Client {
	return &Client{chat: chatClient, stateDir: chatClient.StateDir()}
}

// GenerateSpeech produces audio for the given options over pure HTTP.
func (c *Client) GenerateSpeech(ctx context.Context, opts SpeechOptions) (*SpeechResult, error) {
	if strings.TrimSpace(opts.Input) == "" {
		return nil, errors.New("tts: input (texto) e obrigatorio")
	}
	if opts.ResponseFormat == "" {
		opts.ResponseFormat = "wav"
	}
	if opts.ResponseFormat != "wav" && opts.ResponseFormat != "pcm" {
		return nil, fmt.Errorf("tts: response_format nao suportado: %s", opts.ResponseFormat)
	}
	if opts.Voice == "" {
		opts.Voice = "zephyr"
	}
	voiceName := normalizeVoice(opts.Voice)

	// Template capturado do request real do frontend (generate-speech):
	//   [0] model
	//   [1] mensagens (mesmo formato nested do chat)
	//   [2] safety (null para TTS)
	//   [3] config: [4]=topP 1, [5]=temp 0.95, [6]=topK 64,
	//       [14]=[3] (modalidade de resposta audio), [15]=voz
	//   [4] assinatura Botguard (preenchida pelo chat.Client)
	transcript := "## Transcript:\n" + opts.Input
	if scene := strings.TrimSpace(opts.Scene); scene != "" {
		transcript = scene + "\n\n" + transcript
	}
	payload := []any{
		ttsModel,
		[]any{[]any{[]any{[]any{nil, transcript}}, "user"}},
		nil,
		[]any{
			nil, nil, nil, nil,
			1, 0.95, 64,
			nil, nil, nil, nil, nil, nil, nil,
			[]any{3},
			[]any{[]any{[]any{voiceName}}},
		},
		nil,
	}

	result, err := c.chat.GenerateCustom(ctx, payload, "")
	if err != nil {
		return nil, err
	}
	dumpResponseBody(c.stateDir, result.Body)
	if result.Status != http.StatusOK {
		return &SpeechResult{
			Format: opts.ResponseFormat,
			Error:  fmt.Sprintf("tts: AI Studio retornou status %d: %s", result.Status, snippet(result.Body)),
		}, nil
	}

	audioPart, parseErr := extractAllAudioParts(result.Body)
	if parseErr != nil {
		return &SpeechResult{
			Format: opts.ResponseFormat,
			Error:  parseErr.Error(),
		}, nil
	}

	// O upstream fatia o audio em varios chunks base64; decodifica e concatena
	// na ordem do documento.
	var pcmData []byte
	for _, data := range audioPart.DataChunks {
		chunk, decErr := base64.StdEncoding.DecodeString(data)
		if decErr != nil {
			return nil, fmt.Errorf("tts: base64 decode falhou: %w", decErr)
		}
		pcmData = append(pcmData, chunk...)
	}
	if len(pcmData) == 0 {
		return &SpeechResult{Format: opts.ResponseFormat, Error: "tts: audio vazio"}, nil
	}

	if opts.ResponseFormat == "pcm" {
		return &SpeechResult{
			Audio:    pcmData,
			Format:   "pcm",
			MimeType: audioPart.MimeType,
		}, nil
	}

	wav := pcmToWav(pcmData, audioPart.SampleRate, audioPart.Channels, 16)
	return &SpeechResult{
		Audio:    wav,
		Format:   "wav",
		MimeType: "audio/wav",
	}, nil
}

func normalizeVoice(voice string) string {
	v := strings.ToLower(strings.TrimSpace(voice))
	switch v {
	case "zephyr":
		return "Zephyr"
	case "charon":
		return "Charon"
	}
	return voice
}

// dumpResponseBody grava o body cru do GenerateContent de TTS para inspecao
// (AISTUDIO_DEBUG_TTS_DUMP=1). Usado para validar o shape da resposta HTTP.
func dumpResponseBody(stateDir, body string) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AISTUDIO_DEBUG_TTS_DUMP")))
	if v != "1" && v != "true" {
		return
	}
	path := os.Getenv("AISTUDIO_DEBUG_TTS_DUMP_RESPONSE")
	if path == "" {
		if stateDir == "" {
			return
		}
		path = filepath.Join(stateDir, "debug", "tts-response.json")
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(body), 0o644)
}

func snippet(body string) string {
	const max = 200
	body = strings.TrimSpace(body)
	if len(body) <= max {
		return body
	}
	return body[:max] + "..."
}

var sampleRateRe = regexp.MustCompile(`rate=(\d+)`)
var channelsRe = regexp.MustCompile(`channels=(\d+)`)

type audioPart struct {
	MimeType   string
	DataChunks []string
	SampleRate int
	Channels   int
}

// extractAllAudioParts percorre a resposta em ordem de documento (DFS) e
// coleta TODOS os chunks de audio inline. O GenerateContent de TTS streama o
// audio fatiado: cada chunk e um par ["audio/l16; rate=...", "<base64>"].
func extractAllAudioParts(responseText string) (*audioPart, error) {
	var parsed any
	if err := json.Unmarshal([]byte(responseText), &parsed); err != nil {
		return nil, fmt.Errorf("tts: resposta invalida: %w", err)
	}
	result := &audioPart{}
	var walk func(node any)
	walk = func(node any) {
		arr, ok := node.([]any)
		if !ok {
			return
		}
		if len(arr) >= 2 {
			mime, ok1 := arr[0].(string)
			data, ok2 := arr[1].(string)
			if ok1 && ok2 && strings.HasPrefix(strings.ToLower(mime), "audio/") {
				if result.MimeType == "" {
					result.MimeType = mime
					result.SampleRate, result.Channels = parseAudioMime(mime)
				}
				result.DataChunks = append(result.DataChunks, data)
				return
			}
		}
		for _, item := range arr {
			walk(item)
		}
	}
	walk(parsed)
	if result.MimeType == "" {
		return nil, errors.New("tts: audio inline nao localizado")
	}
	return result, nil
}

func parseAudioMime(mime string) (sampleRate, channels int) {
	sampleRate = 24000
	channels = 1
	if m := sampleRateRe.FindStringSubmatch(mime); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil {
			sampleRate = n
		}
	}
	if m := channelsRe.FindStringSubmatch(mime); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil {
			channels = n
		}
	}
	return
}

// pcmToWav wraps raw PCM bytes in a canonical RIFF/WAVE container.
func pcmToWav(pcm []byte, sampleRate, channels, bitsPerSample int) []byte {
	blockAlign := channels * (bitsPerSample / 8)
	byteRate := sampleRate * blockAlign
	dataSize := len(pcm)
	buf := make([]byte, 44+dataSize)

	copy(buf[0:4], []byte("RIFF"))
	leUint32(buf[4:8], 36+dataSize)
	copy(buf[8:12], []byte("WAVE"))
	copy(buf[12:16], []byte("fmt "))
	leUint32(buf[16:20], 16)
	leUint16(buf[20:22], 1)
	leUint16(buf[22:24], channels)
	leUint32(buf[24:28], sampleRate)
	leUint32(buf[28:32], byteRate)
	leUint16(buf[32:34], blockAlign)
	leUint16(buf[34:36], bitsPerSample)
	copy(buf[36:40], []byte("data"))
	leUint32(buf[40:44], dataSize)
	copy(buf[44:], pcm)
	return buf
}

func leUint32(b []byte, v int) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func leUint16(b []byte, v int) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
}
