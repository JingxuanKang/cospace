package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

type fakePolicy struct {
	usd  float64
	conc int
	key  string
}

func (p fakePolicy) Limits(string) (float64, int) { return p.usd, p.conc }
func (p fakePolicy) QuotaKey(string) string       { return p.key }

// newQuotaGW builds a gateway with quota enforcement over the given upstream.
func newQuotaGW(t *testing.T, up *httptest.Server, pol fakePolicy, seed map[string]float64) *Gateway {
	t.Helper()
	u, _ := url.Parse(up.URL)
	gw := &Gateway{
		Auth:      fakeAuth{"cs_space1tok": "space1"},
		Creds:     NewLearner(),
		Upstreams: map[string]*url.URL{"anthropic": u},
		Usage:     &recUsage{},
		Policy:    pol,
	}
	gw.InitQuota(seed)
	return gw
}

// TestSpendLimitBlocks: a space already over its cap is refused pre-flight.
func TestSpendLimitBlocks(t *testing.T) {
	var auth, path string
	up := newUpstream(t, &auth, &path, nil)
	defer up.Close()
	// Seeded spend 100 against a 50 cap → over budget from the first request.
	gw := newQuotaGW(t, up, fakePolicy{usd: 50, conc: 5}, map[string]float64{"space1": 100})
	seedCred(t, gw)

	w := httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("POST", "/anthropic/v1/messages", "cs_space1tok", "{}"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget space: got %d, want 429", w.Code)
	}
	if !strings.Contains(w.Body.String(), "spend limit") {
		t.Fatalf("body = %q, want spend-limit message", w.Body.String())
	}
}

func TestQuotaGenerationDoesNotInheritReusedSpaceName(t *testing.T) {
	var auth, path string
	up := newUpstream(t, &auth, &path, nil)
	defer up.Close()
	gw := newQuotaGW(t, up, fakePolicy{usd: 50, conc: 5, key: "new-generation"}, map[string]float64{"space1": 100})
	seedCred(t, gw)

	w := httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("POST", "/anthropic/v1/messages", "cs_space1tok", "{}"))
	if w.Code != http.StatusOK {
		t.Fatalf("new generation inherited old space-name quota: got %d", w.Code)
	}
}

// TestUnderLimitPasses: a space under its cap is served, and its cost accrues.
func TestUnderLimitPasses(t *testing.T) {
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-3-5-sonnet","usage":{"input_tokens":1000000,"output_tokens":0}}}` + "\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse))
	}))
	defer up.Close()
	gw := newQuotaGW(t, up, fakePolicy{usd: 50, conc: 5}, nil)
	seedCred(t, gw)

	w := httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("POST", "/anthropic/v1/messages", "cs_space1tok", "{}"))
	if w.Code != 200 {
		t.Fatalf("under-budget space: got %d, want 200", w.Code)
	}
	// 1,000,000 sonnet input tokens = $3.
	if got := gw.SpentUSD("space1"); !approx(got, 3) {
		t.Fatalf("accrued spend = %v, want 3", got)
	}
}

// TestConcurrencyLimitBlocks: with a cap of 1, a second in-flight request is
// refused while the first is still running.
func TestConcurrencyLimitBlocks(t *testing.T) {
	var armed atomic.Bool
	started := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if armed.Load() {
			started <- struct{}{}
			<-release
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	gw := newQuotaGW(t, up, fakePolicy{usd: 0, conc: 1}, nil)
	seedCred(t, gw) // runs while disarmed, returns immediately

	armed.Store(true)
	// First request occupies the only slot and blocks in the upstream.
	firstCode := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		gw.ServeHTTP(w, spaceReq("POST", "/anthropic/v1/messages", "cs_space1tok", "{}"))
		firstCode <- w.Code
	}()
	<-started // first request is now in-flight, holding the slot

	// Second request must be refused for want of a concurrency slot.
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("POST", "/anthropic/v1/messages", "cs_space1tok", "{}"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second concurrent request: got %d, want 429", w.Code)
	}
	if !strings.Contains(w.Body.String(), "concurrent") {
		t.Fatalf("body = %q, want concurrency message", w.Body.String())
	}

	close(release)
	if code := <-firstCode; code != 200 {
		t.Fatalf("first request: got %d, want 200", code)
	}
	// Slot freed: a later request is admitted again.
	armed.Store(false)
	w = httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("POST", "/anthropic/v1/messages", "cs_space1tok", "{}"))
	if w.Code != 200 {
		t.Fatalf("after slot freed: got %d, want 200", w.Code)
	}
}
