package store

import (
	"context"
	"sync"
)

// inflightKey is the context key for *InflightTracker.
type inflightKey struct{}

// InflightTracker records how many times each account's in-flight request
// counter was incremented during one request lifecycle, so the caller can
// decrement exactly those IDs when the request completes (no leaks, no
// double-decrements). A successful ensure implies exactly one Inc per return;
// ensure error paths already Dec internally and must NOT be tracked.
type InflightTracker struct {
	mu     sync.Mutex
	counts map[string]int
}

// WithInflightTracker attaches a fresh tracker to ctx.
func WithInflightTracker(ctx context.Context) (context.Context, *InflightTracker) {
	t := &InflightTracker{counts: map[string]int{}}
	return context.WithValue(ctx, inflightKey{}, t), t
}

// InflightFrom returns the tracker attached to ctx, or nil.
func InflightFrom(ctx context.Context) *InflightTracker {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(inflightKey{}).(*InflightTracker)
	return t
}

// TrackInflight records one Inc of id in the tracker attached to ctx (if any).
// Call right after a successful ensure that returned this account.
func TrackInflight(ctx context.Context, id string) {
	t := InflightFrom(ctx)
	if t == nil || id == "" {
		return
	}
	t.mu.Lock()
	t.counts[id]++
	t.mu.Unlock()
}

// UntrackInflight drops all recorded Incs of id (e.g. when the request rotates
// to another account and the previous counter is decremented immediately).
// Returns how many Incs were dropped.
func UntrackInflight(ctx context.Context, id string) int {
	t := InflightFrom(ctx)
	if t == nil || id == "" {
		return 0
	}
	t.mu.Lock()
	n := t.counts[id]
	delete(t.counts, id)
	t.mu.Unlock()
	return n
}

// Remove drops all recorded Incs of id; returns how many were dropped.
func (t *InflightTracker) Remove(id string) int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	n := t.counts[id]
	delete(t.counts, id)
	t.mu.Unlock()
	return n
}

// DecAll decrements every tracked account exactly as many times as it was
// tracked. Idempotent: the tracker is emptied on first call.
func (t *InflightTracker) DecAll(s *Store) {
	if t == nil || s == nil {
		return
	}
	t.mu.Lock()
	counts := t.counts
	t.counts = map[string]int{}
	t.mu.Unlock()
	for id, n := range counts {
		for i := 0; i < n; i++ {
			s.DecAccountRequestCount(id)
		}
	}
}
