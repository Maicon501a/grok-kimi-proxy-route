package admin

import "sync"

// ProfileMetrics agrega métricas operacionais de um perfil.
type ProfileMetrics struct {
	Requests        int64  `json:"requests"`
	Successes       int64  `json:"successes"`
	Failures        int64  `json:"failures"`
	TotalTokens     int64  `json:"total_tokens"`
	AvgLatencyMs    int64  `json:"avg_latency_ms"`
	TokensSupported bool   `json:"tokens_supported"`
	LastError       string `json:"last_error,omitempty"`
	totalLatencyMs  int64
}

// MetricsSnapshot é a view imutável exposta pelo dashboard.
type MetricsSnapshot struct {
	ByProfile map[string]*ProfileMetrics `json:"by_profile"`
}

// MetricsStore é um agregador thread-safe de métricas por perfil.
type MetricsStore struct {
	mu        sync.Mutex
	byProfile map[string]*ProfileMetrics
}

func NewMetricsStore() *MetricsStore {
	return &MetricsStore{byProfile: make(map[string]*ProfileMetrics)}
}

// Record registra o resultado de uma request: latência, sucesso/falha,
// tokens (quando o upstream informa) e último erro.
func (s *MetricsStore) Record(profileID string, latencyMs int64, success bool, totalTokens int64, tokensKnown bool, errMsg string) {
	if s == nil || profileID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.byProfile[profileID]
	if m == nil {
		m = &ProfileMetrics{}
		s.byProfile[profileID] = m
	}
	m.Requests++
	if success {
		m.Successes++
	} else {
		m.Failures++
	}
	m.totalLatencyMs += latencyMs
	m.AvgLatencyMs = m.totalLatencyMs / m.Requests
	if tokensKnown {
		m.TokensSupported = true
		m.TotalTokens += totalTokens
	}
	if errMsg != "" {
		m.LastError = errMsg
	}
}

// Snapshot retorna uma cópia profunda das métricas atuais.
func (s *MetricsStore) Snapshot() MetricsSnapshot {
	snap := MetricsSnapshot{ByProfile: make(map[string]*ProfileMetrics)}
	if s == nil {
		return snap
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, m := range s.byProfile {
		cp := *m
		snap.ByProfile[id] = &cp
	}
	return snap
}
