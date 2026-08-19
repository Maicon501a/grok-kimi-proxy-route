package accmgr

// Chat-with-limited-account probe. Gated by ACCIO_CHAT_PROBE=1.
//
// Question: an account whose token is NOT_LOGIN at the entitlement API might
// STILL work for chat (different endpoint, different gate). If yes, the
// credits gate is moot and hot-IP creations stay usable — the cadence problem
// disappears. This probe creates one account, reads its entitlement, lists
// models, and sends a real 1-token chat request with the fresh token.
//
//	ACCIO_CHAT_PROBE=1 go test ./internal/accmgr -run TestChatWithFreshAccount -v -timeout 15m

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"grok-desktop/internal/accio"
	"grok-desktop/internal/logging"
)

func TestChatWithFreshAccount(t *testing.T) {
	if os.Getenv("ACCIO_CHAT_PROBE") == "" {
		t.Skip("set ACCIO_CHAT_PROBE=1 to run")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	acc, err := accio.New(dataDir)
	if err != nil {
		t.Fatalf("accio.New: %v", err)
	}

	// Create the account (retry through code poisoning; Node exchange is
	// automatic in the client now).
	var rec accio.TokenRecord
	for attempt := 1; attempt <= 5; attempt++ {
		if attempt > 1 {
			time.Sleep(20 * time.Second)
		}
		inbox, err := NewInbox(ctx)
		if err != nil {
			t.Fatalf("inbox: %v", err)
		}
		rec, err = runLoginPass(ctx, acc, signupProfileDir(), inbox)
		if err == nil {
			break
		}
		t.Logf("attempt %d: %v", attempt, err)
	}
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	t.Logf("account: id=%s access=%s…", rec.ID, rec.AccessToken[:8])

	// 1. Entitlement (expected: NOT_LOGIN / blocked on a hot IP).
	cctx, ccancel := context.WithTimeout(ctx, 20*time.Second)
	credits, cerr := acc.CreditsFor(cctx, rec)
	ccancel()
	t.Logf("entitlement: credits=%v err=%v", credits, cerr)

	// 2. Models list with the fresh token.
	mctx, mcancel := context.WithTimeout(ctx, 30*time.Second)
	models, merr := acc.Models(mctx)
	mcancel()
	if merr != nil {
		t.Logf("models: %v", merr)
	} else {
		names := make([]string, 0, 5)
		for _, m := range models {
			if len(names) < 5 {
				names = append(names, m.ID)
			}
		}
		t.Logf("models: %d available, first: %s", len(models), strings.Join(names, ", "))
	}

	// 3. Real chat request with the fresh token.
	modelID := ""
	for _, m := range models {
		if m.ID != "" {
			modelID = m.ID
			break
		}
	}
	if modelID == "" {
		// The catalog endpoint 404s for limited accounts; fall back to the
		// known default model so the chat test still runs.
		modelID = accio.DefaultModel
		t.Logf("catalog unavailable, using default model %s", modelID)
	}
	t.Logf("chat model: %s", modelID)
	var got strings.Builder
	chatCtx, chatCancel := context.WithTimeout(ctx, 90*time.Second)
	chatErr := acc.StreamWithToken(chatCtx, map[string]any{
		"model":    modelID,
		"messages": []any{map[string]any{"role": "user", "content": "Responda apenas com a palavra: oi"}},
	}, rec.AccessToken, func(text, reasoning string, call map[string]any, done bool) {
		got.WriteString(text)
	})
	chatCancel()
	t.Logf("CHAT RESULT: err=%v text=%q", chatErr, got.String())
	if chatErr == nil && got.Len() > 0 {
		logging.Info("probe.chat_works", "text", got.String())
		t.Log(">>> CHAT FUNCIONA com conta limitada — o gate de créditos é opcional!")
	}

	// 4. Tool calling (requisito do usuário: IDE precisa de tool calls reais).
	var callEmitted map[string]any
	var toolText strings.Builder
	toolCtx, toolCancel := context.WithTimeout(ctx, 90*time.Second)
	toolErr := acc.StreamWithToken(toolCtx, map[string]any{
		"model":    modelID,
		"messages": []any{map[string]any{"role": "user", "content": "Qual a temperatura em São Paulo? Use a ferramenta get_weather para responder."}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "Obtém o clima atual de uma cidade",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"city": map[string]any{"type": "string", "description": "nome da cidade"}},
					"required":   []string{"city"},
				},
			},
		}},
	}, rec.AccessToken, func(text, reasoning string, call map[string]any, done bool) {
		toolText.WriteString(text)
		if call != nil {
			callEmitted = call
		}
	})
	toolCancel()
	t.Logf("TOOL CALL RESULT: err=%v call=%v text=%q", toolErr, callEmitted, toolText.String())
	if callEmitted != nil {
		t.Log(">>> TOOL CALL FUNCIONA com a conta nova!")
	}
}
