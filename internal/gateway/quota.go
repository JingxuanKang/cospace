package gateway

import "sync"

// SpacePolicy supplies a space's spend limit and concurrency cap. A usdLimit of
// 0 means unlimited (meter only, never block); maxConcurrency of 0 means
// unlimited concurrent requests. Implemented by the spaces manager.
type SpacePolicy interface {
	Limits(space string) (usdLimit float64, maxConcurrency int)
}

// quotaTracker holds the live per-space usage state the gateway enforces:
// cumulative equivalent-dollar spend (never reset — a lifetime total) and the
// count of in-flight requests. Spend is seeded from history at startup and
// accumulated as requests complete; it is the source of truth for admission.
type quotaTracker struct {
	mu       sync.Mutex
	spent    map[string]float64
	inflight map[string]int
}

func newQuotaTracker(seed map[string]float64) *quotaTracker {
	if seed == nil {
		seed = map[string]float64{}
	}
	return &quotaTracker{spent: seed, inflight: map[string]int{}}
}

// spentUSD returns a space's cumulative spend.
func (q *quotaTracker) spentUSD(space string) float64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.spent[space]
}

// overLimit reports whether a space has already reached its spend limit. A
// limit of 0 is unlimited. Enforcement is pre-flight against accumulated spend,
// so a request in progress can push spend past the limit — the next request is
// what gets refused (at most one request's worth of overshoot).
func (q *quotaTracker) overLimit(space string, usdLimit float64) bool {
	if usdLimit <= 0 {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.spent[space] >= usdLimit
}

// add accumulates a completed request's cost into a space's lifetime spend.
func (q *quotaTracker) add(space string, cost float64) {
	if cost <= 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.spent[space] += cost
}

// enter reserves a concurrency slot, returning false when the space is already
// at maxConcurrency (0 = unlimited). A caller that gets true MUST call leave.
func (q *quotaTracker) enter(space string, maxConcurrency int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if maxConcurrency > 0 && q.inflight[space] >= maxConcurrency {
		return false
	}
	q.inflight[space]++
	return true
}

// leave releases a concurrency slot.
func (q *quotaTracker) leave(space string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.inflight[space] > 0 {
		q.inflight[space]--
	}
}
