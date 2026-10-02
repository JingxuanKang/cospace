// Package gateway is the credential edge: spaces hold only per-space fake
// tokens; this proxy authenticates them, swaps in the host's real credential,
// and forwards to the provider. Real credentials never enter a space.
// Production credentials come from the host CLI files or Keychain. Learner
// remains available for tests; file/Keychain stores do not learn from traffic.
package gateway

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type SpaceAuth interface {
	LookupToken(token string) (space string, ok bool)
}

type CredentialStore interface {
	Get(provider string) (authHeader string, ok bool)
	Learn(provider, authHeader string)
}

type UsageSink interface {
	Record(space, provider string, status int)
}

// TokenSink is optionally implemented by a UsageSink that wants token counts
// parsed (best-effort) from provider responses.
type TokenSink interface {
	RecordTokens(space, provider string, in, out int)
}

// Learner caches one real credential per provider, learned from the host's
// own loopback traffic.
type Learner struct {
	mu    sync.RWMutex
	creds map[string]string
}

func NewLearner() *Learner { return &Learner{creds: map[string]string{}} }

func (l *Learner) Get(provider string) (string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	c, ok := l.creds[provider]
	return c, ok
}

func (l *Learner) Learn(provider, authHeader string) {
	if authHeader == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.creds[provider] = authHeader
}

type Gateway struct {
	Auth         SpaceAuth
	Creds        CredentialStore
	Upstreams    map[string]*url.URL
	OpenAIRoutes map[string]OpenAIRoute // optional host-configured Sub2API lanes
	Usage        UsageSink
	Policy       SpacePolicy  // optional: per-space spend/concurrency limits
	Client       *http.Client // defaults to defaultClient (no redirects)
	// Logf receives upstream failures (never credentials). nil = silent.
	Logf func(format string, v ...any)

	quota *quotaTracker
}

// defaultClient never follows redirects on a space's behalf: a 3xx from the
// upstream goes back to the client as-is instead of the gateway re-sending
// the host's credential to wherever Location points. No overall timeout, as
// completions stream for minutes; the response read is bounded by the
// client's own context.
var defaultClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// bodyReadTimeout bounds how long a space may take to upload one request
// body. It protects the concurrency slots from slow-loris uploads.
const bodyReadTimeout = 2 * time.Minute

// spaceProviderPolicy is optionally implemented by Policy. Keeping it
// separate from SpacePolicy preserves compatibility with quota-only callers.
type spaceProviderPolicy interface {
	ProviderEnabled(space, provider string) bool
}

// CostSink is optionally implemented by a UsageSink that wants the
// equivalent-dollar cost of a request recorded alongside its token counts.
type CostSink interface {
	RecordCost(space, provider, model string, in, out int, cost float64)
}

// InitQuota enables per-space spend/concurrency enforcement, seeding each space's
// cumulative spend from history (so limits survive a restart). Without it the
// gateway meters but never blocks.
func (g *Gateway) InitQuota(seed map[string]float64) { g.quota = newQuotaTracker(seed) }

// SpentUSD reports a space's cumulative equivalent-dollar spend (0 if quota
// tracking is disabled).
func (g *Gateway) SpentUSD(space string) float64 {
	if g.quota == nil {
		return 0
	}
	return g.quota.spentUSD(g.quotaKey(space))
}

func (g *Gateway) quotaKey(space string) string {
	if p, ok := g.Policy.(interface{ QuotaKey(string) string }); ok {
		if key := p.QuotaKey(space); key != "" {
			return key
		}
	}
	return space
}

// hop-by-hop headers must not be forwarded (RFC 9110 §7.6.1).
var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Normalize the path before any decision is made on it. The gateway is
	// served directly (no ServeMux cleaning), and url.JoinPath resolves ".."
	// against the upstream base, so an uncleaned "/openai/../../x" would reach
	// an arbitrary endpoint of the upstream host carrying the host's real
	// credential. After cleaning, the provider segment is authoritative and
	// the remainder can only point below the configured base path.
	cleaned := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	if strings.HasSuffix(r.URL.Path, "/") && cleaned != "/" {
		cleaned += "/"
	}
	if cleaned != r.URL.Path {
		r.URL.Path, r.URL.RawPath = cleaned, ""
	}
	provider, rest, ok := splitProvider(r.URL.Path)
	upstream := g.Upstreams[provider]
	if !ok || upstream == nil {
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}

	token := bearer(r)
	if space, isSpace := g.Auth.LookupToken(token); isSpace {
		if policy, ok := g.Policy.(spaceProviderPolicy); ok && !policy.ProviderEnabled(space, provider) {
			http.Error(w, "provider is disabled for this space", http.StatusForbidden)
			return
		}
		quotaKey := g.quotaKey(space)
		creds := g.Creds
		var route string
		internet := true
		if policy, ok := g.Policy.(executionPolicy); ok {
			var err error
			route, internet, err = policy.ExecutionPolicy(space)
			if err != nil {
				http.Error(w, "space settings unavailable", http.StatusServiceUnavailable)
				return
			}
			if provider == "openai" && route != "" && route != "official" {
				selected, ok := g.OpenAIRoutes[route]
				if !ok || selected.Upstream == nil || selected.Creds == nil {
					http.Error(w, "Codex route is not configured on this host", http.StatusServiceUnavailable)
					return
				}
				upstream, creds = selected.Upstream, selected.Creds
			}
		}
		cred, have := creds.Get(provider)
		if !have {
			http.Error(w, "gateway has no credential for "+provider+" yet", http.StatusServiceUnavailable)
			return
		}
		// Pre-flight: concurrency slot then cumulative-spend limit, BEFORE the
		// request body is read, so the body buffer counts against the slot and
		// a flood of slow uploads cannot bypass max_concurrency. Spend is
		// checked against accumulated history (streaming token counts aren't
		// known until the response finishes), so an over-budget space is caught
		// on its next request — at most one request's worth of overshoot.
		if g.quota != nil {
			var usdLimit float64
			var maxConc int
			if g.Policy != nil {
				usdLimit, maxConc = g.Policy.Limits(space)
			}
			if !g.quota.enter(quotaKey, maxConc) {
				http.Error(w, "space is at its concurrent-request limit", http.StatusTooManyRequests)
				return
			}
			defer g.quota.leave(quotaKey)
			if g.quota.overLimit(quotaKey, usdLimit) {
				http.Error(w, "space has reached its spend limit", http.StatusTooManyRequests)
				return
			}
		}
		if _, ok := g.Policy.(executionPolicy); ok {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyReadTimeout))
			var err error
			r, err = prepareRequest(r, provider, route, internet)
			if err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
		}
		scan := &tokenScan{}
		status := g.forward(w, r, upstream, rest, cred, scan)
		if g.Usage != nil {
			g.Usage.Record(space, provider, status)
		}
		if scan.any() {
			in, out, cost := scan.bill(provider)
			if g.quota != nil {
				g.quota.add(quotaKey, cost)
			}
			if cs, ok := g.Usage.(CostSink); ok {
				cs.RecordCost(space, provider, scan.model, in, out, cost)
			} else if ts, ok := g.Usage.(TokenSink); ok {
				ts.RecordTokens(space, provider, in, out)
			}
		}
		return
	}

	if isLoopback(r.RemoteAddr) {
		// Host's own call: learn its credential and pass through unchanged.
		g.Creds.Learn(provider, r.Header.Get("Authorization"))
		g.forward(w, r, upstream, rest, r.Header.Get("Authorization"), nil)
		return
	}

	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, upstream *url.URL, path, authHeader string, scan *tokenScan) int {
	out := upstream.JoinPath(path)
	out.RawQuery = r.URL.RawQuery
	req, err := http.NewRequestWithContext(r.Context(), r.Method, out.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return http.StatusBadGateway
	}
	req.Header = r.Header.Clone()
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
	req.Header.Del("X-Api-Key")
	if scan != nil {
		// Metering reads the response bytes, so they must arrive uncompressed.
		// Without the client's Accept-Encoding, Go's transport negotiates gzip
		// itself and transparently decompresses, dropping Content-Encoding and
		// Content-Length so what the space receives stays consistent. A client
		// that asked for gzip explicitly would otherwise get opaque bytes the
		// token scan cannot read — and an uncounted request.
		req.Header.Del("Accept-Encoding")
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	req.Host = upstream.Host

	client := g.Client
	if client == nil {
		client = defaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// The error text names the upstream host, which for a host-configured
		// alternate route is the host's private endpoint: log it, don't echo it.
		if g.Logf != nil {
			g.Logf("gateway: upstream %s: %v", upstream.Host, err)
		}
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return http.StatusBadGateway
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		if isHopHeader(k) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flushCopy(w, resp.Body, scan)
	return resp.StatusCode
}

func isHopHeader(name string) bool {
	for _, h := range hopHeaders {
		if strings.EqualFold(h, name) {
			return true
		}
	}
	return false
}

// flushCopy streams the body, flushing after each chunk so SSE responses
// (both providers stream completions) reach the space without buffering.
// When scan is non-nil each chunk is also fed to the token parser.
func flushCopy(w http.ResponseWriter, r io.Reader, scan *tokenScan) {
	f, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if scan != nil {
				scan.feed(buf[:n])
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if f != nil {
				f.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

var (
	inTokRe         = regexp.MustCompile(`"input_tokens"\s*:\s*(\d+)`)
	outTokRe        = regexp.MustCompile(`"output_tokens"\s*:\s*(\d+)`)
	cacheReadRe     = regexp.MustCompile(`"cache_read_input_tokens"\s*:\s*(\d+)`)
	cacheCreationRe = regexp.MustCompile(`"cache_creation_input_tokens"\s*:\s*(\d+)`)
	cachedTokRe     = regexp.MustCompile(`"cached_tokens"\s*:\s*(\d+)`)
	modelRe         = regexp.MustCompile(`"model"\s*:\s*"([^"]+)"`)
)

// tokenScan extracts usage figures from a provider response as it streams by.
// The last occurrence wins: Anthropic's message_delta carries the cumulative
// output_tokens at end of stream. A small tail is carried across chunk
// boundaries so a usage object split between two chunks still parses. The model
// name (used to price the request) is taken from its first occurrence — it
// appears near the top of the response and doesn't change within one.
//
// Prompt caching is what makes agent sessions affordable, and it is also
// where naive metering goes wrong: on a 100k-token Claude Code context,
// input_tokens is ~10 per turn while cache_read_input_tokens is ~100k.
// Anthropic reports cache tokens separately from input_tokens; OpenAI folds
// cached_tokens into input_tokens and breaks them out as a detail.
type tokenScan struct {
	carry      []byte
	in, out    int
	cacheRead  int // Anthropic: tokens served from the prompt cache
	cacheWrite int // Anthropic: tokens written to the prompt cache
	cached     int // OpenAI/xAI: cached subset of input_tokens
	model      string
}

func (t *tokenScan) feed(chunk []byte) {
	buf := append(t.carry, chunk...)
	if t.model == "" {
		if m := modelRe.FindSubmatch(buf); m != nil {
			t.model = string(m[1])
		}
	}
	last := func(re *regexp.Regexp, dst *int) {
		if m := re.FindAllSubmatch(buf, -1); m != nil {
			if v, err := strconv.Atoi(string(m[len(m)-1][1])); err == nil {
				*dst = v
			}
		}
	}
	last(inTokRe, &t.in)
	last(outTokRe, &t.out)
	last(cacheReadRe, &t.cacheRead)
	last(cacheCreationRe, &t.cacheWrite)
	last(cachedTokRe, &t.cached)
	const tail = 64
	if len(buf) > tail {
		buf = buf[len(buf)-tail:]
	}
	t.carry = append([]byte(nil), buf...)
}

func (t *tokenScan) any() bool {
	return t.in > 0 || t.out > 0 || t.cacheRead > 0 || t.cacheWrite > 0
}

// bill returns the input tokens actually processed (cache traffic included,
// so the usage chart reflects real context volume), the output tokens, and
// the equivalent-dollar cost with cache pricing applied.
func (t *tokenScan) bill(provider string) (in, out int, cost float64) {
	return billTokens(t.model, provider, t.in, t.out, t.cacheRead, t.cacheWrite, t.cached)
}

func splitProvider(path string) (provider, rest string, ok bool) {
	p := strings.TrimPrefix(path, "/")
	i := strings.IndexByte(p, '/')
	if i <= 0 {
		return p, "/", p != ""
	}
	return p[:i], p[i:], true
}

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.Header.Get("X-Api-Key")
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
