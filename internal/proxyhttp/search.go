package proxyhttp

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"grok-desktop/internal/logging"
	"grok-desktop/internal/store"
	"grok-desktop/internal/upstream"
)

// SearchRequest is a thin OpenAI-ish body for native xAI web/x search.
//
//	POST /v1/search
//	{
//	  "query": "what is grok 4.5",
//	  "model": "grok-4.5",           // optional
//	  "sources": ["web","x"],        // optional, default both
//	  "max_results": 10,             // optional (hint for response shaping)
//	  "reasoning_effort": "low"      // optional
//	}
type SearchRequest struct {
	Query           string   `json:"query"`
	Q               string   `json:"q"` // alias
	Model           string   `json:"model"`
	Sources         []string `json:"sources"`
	MaxResults      int      `json:"max_results"`
	ReasoningEffort string   `json:"reasoning_effort"`
	Stream          bool     `json:"stream"` // ignored for now (always non-stream JSON)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// also allow GET ?q=
		if r.Method == http.MethodGet {
			q := strings.TrimSpace(r.URL.Query().Get("q"))
			if q == "" {
				q = strings.TrimSpace(r.URL.Query().Get("query"))
			}
			if q == "" {
				http.Error(w, `{"error":{"message":"missing query","type":"invalid_request_error"}}`, http.StatusBadRequest)
				return
			}
			s.runSearch(w, r, SearchRequest{
				Query:   q,
				Model:   r.URL.Query().Get("model"),
				Sources: splitCSV(r.URL.Query().Get("sources")),
			})
			return
		}
		http.Error(w, `{"error":{"message":"method not allowed"}}`, http.StatusMethodNotAllowed)
		return
	}
	if !s.gate(r) {
		http.Error(w, `{"error":{"message":"unauthorized","type":"invalid_request_error"}}`, http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxProxyBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req SearchRequest
	if len(body) > 0 {
		_ = json.Unmarshal(body, &req)
	}
	if strings.TrimSpace(req.Query) == "" {
		req.Query = strings.TrimSpace(req.Q)
	}
	if strings.TrimSpace(req.Query) == "" {
		// also accept OpenAI chat-like {"messages":[{"role":"user","content":"..."}]}
		var loose map[string]any
		if json.Unmarshal(body, &loose) == nil {
			if msgs, ok := loose["messages"].([]any); ok {
				for i := len(msgs) - 1; i >= 0; i-- {
					m, _ := msgs[i].(map[string]any)
					if m == nil {
						continue
					}
					if asString(m["role"]) == "user" {
						req.Query = contentToString(m["content"])
						break
					}
				}
			}
			if req.Query == "" {
				req.Query = asString(loose["input"])
			}
		}
	}
	if strings.TrimSpace(req.Query) == "" {
		http.Error(w, `{"error":{"message":"query is required","type":"invalid_request_error","param":"query"}}`, http.StatusBadRequest)
		return
	}
	s.runSearch(w, r, req)
}

func (s *Server) runSearch(w http.ResponseWriter, r *http.Request, req SearchRequest) {
	if !s.gate(r) {
		http.Error(w, `{"error":{"message":"unauthorized"}}`, http.StatusUnauthorized)
		return
	}
	// Route by the CLIENT-sent model, exactly like the chat endpoints — never
	// inherit the UI global provider. Native search (web_search/x_search) is
	// xAI-only, so only models that route to xAI may use it; anything else gets
	// a clear 400 instead of a silent rewrite to the global default model.
	clientModel := strings.TrimSpace(req.Model)
	routeSettings := s.store.Settings().WithProviderForModel(clientModel)
	if routeProv := routeSettings.NormalizedProvider(); routeProv != store.ProviderXAI {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "Native /v1/search (xAI web_search/x_search) requires a Grok model; model \"" + clientModel + "\" routes to provider " + routeProv + ". Send a grok model (e.g. grok-4.5) to use search.",
				"type":    "invalid_request_error",
				"code":    "search_requires_xai_model",
			},
		})
		return
	}
	ctx := WithRouteProvider(r.Context(), store.ProviderXAI)
	ctx = logging.WithRequestID(ctx, uuid.NewString()[:8])
	srchLog := logging.FromContext(ctx).With("provider", store.ProviderXAI, "path", "/v1/search")
	// Track accounts Inc'd by ensure (initial + rotation retries) for exact Dec.
	ctx, inflight := store.WithInflightTracker(ctx)
	defer inflight.DecAll(s.store)
	token, acc, _, err := s.ensure(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if acc != nil {
		store.TrackInflight(ctx, acc.ID)
	}
	settings := routeSettings
	// Honor the client model 100%: only empty/alias ids resolve to the routed
	// provider default (which is always a grok model on the xAI route).
	model := routeSettings.ResolveModelForClient(clientModel)
	effort := req.ReasoningEffort
	if effort == "" {
		effort = "low"
	}
	sources := normalizeSearchSources(req.Sources)
	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = 12
	}

	accountID := ""
	if acc != nil {
		accountID = acc.ID
	}
	srchLog.Info("proxy.request.start", "model", model, "account_id", accountID)
	// rotateCreds swaps only token/acc on retry (settings stay pinned to xAI);
	// returns false when the rotated account is not xAI — fail, don't cross providers.
	rotateCreds := func(tok2 string, acc2 *store.Account) bool {
		if acc2 != nil && acc2.NormalizedProvider() != store.ProviderXAI {
			return false
		}
		if acc2 != nil && acc2.ID != accountID {
			if n := inflight.Remove(accountID); n > 0 {
				for i := 0; i < n; i++ {
					s.store.DecAccountRequestCount(accountID)
				}
			}
		}
		if acc2 != nil {
			store.TrackInflight(ctx, acc2.ID)
		}
		token, acc = tok2, acc2
		if acc2 != nil {
			accountID = acc2.ID
		}
		return true
	}
	t0 := time.Now()
	var result *upstream.SearchResponse
	authRetried := false
	for attempt := 0; attempt < 3; attempt++ {
		result, err = s.upstream.NativeSearch(r.Context(), token, settings, model, effort, req.Query, sources)
		if err == nil {
			break
		}
		// bubble upstream status-ish messages as 402/429 when quota
		msg := err.Error()
		low := strings.ToLower(msg)
		// Quota FIRST: Kimi may return 403 with resource_exhausted — must not be treated as auth.
		quota := strings.Contains(low, "http 402") || strings.Contains(low, "balance exhausted") ||
			strings.Contains(low, "usage limit") || strings.Contains(low, "resource_exhausted") ||
			strings.Contains(low, "access_terminated") || strings.Contains(low, "billing cycle") ||
			strings.Contains(low, "upgrade to get more") ||
			(strings.Contains(low, "http 429") && (strings.Contains(low, "exhausted") || strings.Contains(low, "quota") || strings.Contains(low, "balance")))
		if quota && accountID != "" {
			if fn := s.quotaHandler(); fn != nil {
				if rotated := fn(accountID, msg); rotated {
					tok2, acc2, _, err2 := s.ensure(ctx)
					if err2 == nil && tok2 != "" && (acc2 == nil || acc2.Usable()) {
						if !rotateCreds(tok2, acc2) {
							writeCrossProviderError(w, store.ProviderXAI, acc2)
							return
						}
						continue
					}
				}
			} else {
				_, _ = s.store.MarkExhausted(accountID, msg)
				if next := s.store.NextUsableAccountID(accountID); next != "" {
					_ = s.store.SetActiveAccount(next)
					tok2, acc2, _, err2 := s.ensure(ctx)
					if err2 == nil {
						if !rotateCreds(tok2, acc2) {
							writeCrossProviderError(w, store.ProviderXAI, acc2)
							return
						}
						continue
					}
				}
			}
		}
		auth := strings.Contains(low, "http 401") || strings.Contains(low, "http 403") ||
			strings.Contains(low, "permission-denied") || strings.Contains(low, "invalid or expired credentials") ||
			strings.Contains(low, "no auth context") || strings.Contains(low, "access to the chat endpoint is denied")
		if auth && accountID != "" {
			if !authRetried {
				authRetried = true
				if fr := s.forceRefreshFn(); fr != nil {
					tok2, acc2, _, err2 := fr(ctx, accountID)
					if err2 == nil && tok2 != "" && tok2 != token {
						if !rotateCreds(tok2, acc2) {
							writeCrossProviderError(w, store.ProviderXAI, acc2)
							return
						}
						continue
					}
				}
			}
			if fn := s.authFailHandler(); fn != nil {
				if rotated := fn(accountID, msg); rotated {
					tok2, acc2, _, err2 := s.ensure(ctx)
					if err2 == nil && (acc2 == nil || acc2.ID != accountID) {
						if !rotateCreds(tok2, acc2) {
							writeCrossProviderError(w, store.ProviderXAI, acc2)
							return
						}
						continue
					}
				}
			}
		}
		status := http.StatusBadGateway
		if strings.Contains(low, "http 402") || strings.Contains(low, "balance exhausted") {
			status = http.StatusPaymentRequired
		} else if strings.Contains(low, "http 429") {
			status = http.StatusTooManyRequests
		} else if strings.Contains(low, "http 401") {
			status = http.StatusUnauthorized
		} else if strings.Contains(low, "http 403") || strings.Contains(low, "permission-denied") {
			status = http.StatusForbidden
		}
		srchLog.Error("proxy.request.error", "model", model, "account_id", accountID, "status", status, "reason", truncateForLog(msg, 200))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": msg,
				"type":    "upstream_error",
			},
		})
		return
	}
	if result == nil {
		http.Error(w, `{"error":{"message":"empty search result"}}`, http.StatusBadGateway)
		return
	}

	// trim results
	if len(result.Results) > maxResults {
		result.Results = result.Results[:maxResults]
	}

	email := ""
	if acc != nil {
		email = acc.Email
	}

	// OpenAI-compatible-ish envelope + rich search payload
	out := map[string]any{
		"object":  "xai.search",
		"id":      result.ID,
		"created": time.Now().Unix(),
		"model":   firstNonEmpty(result.Model, model),
		"query":   req.Query,
		"sources": sources,
		// primary list (easy for any client)
		"results": result.Results,
		// aliases some UIs expect
		"data": result.Results,
		// optional model prose if xAI returned any
		"answer": result.Answer,
		"usage": map[string]any{
			"prompt_tokens":     result.Usage.PromptTokens,
			"completion_tokens": result.Usage.CompletionTokens,
			"reasoning_tokens":  result.Usage.ReasoningTokens,
			"total_tokens":      result.Usage.TotalTokens,
		},
		"latency_ms": time.Since(t0).Milliseconds(),
		"account":    email,
		// OpenAI Responses-shaped convenience block
		"output": []map[string]any{
			{
				"type":   "search_results",
				"query":  req.Query,
				"status": "completed",
				"results": result.Results,
			},
		},
	}
	if result.Answer != "" {
		out["output"] = append(out["output"].([]map[string]any), map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{
				{"type": "output_text", "text": result.Answer},
			},
		})
	}

	srchLog.Info("proxy.request.done", "model", model, "account_id", accountID, "duration_ms", time.Since(t0).Milliseconds(), "status", 200)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func normalizeSearchSources(in []string) []string {
	if len(in) == 0 {
		return []string{"web", "x"}
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		switch s {
		case "web", "web_search", "internet":
			s = "web"
		case "x", "x_search", "twitter":
			s = "x"
		default:
			continue
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return []string{"web", "x"}
	}
	return out
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
