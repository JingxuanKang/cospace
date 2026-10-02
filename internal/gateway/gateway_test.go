package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type fakeAuth map[string]string // token -> space

func (f fakeAuth) LookupToken(tok string) (string, bool) { r, ok := f[tok]; return r, ok }

type providerPolicy struct{ enabled map[string]bool }

func (p providerPolicy) Limits(string) (float64, int) { return 0, 0 }
func (p providerPolicy) ProviderEnabled(space, provider string) bool {
	return p.enabled[space+"/"+provider]
}

type recUsage struct {
	mu   sync.Mutex
	rows []string
}

func (r *recUsage) Record(space, provider string, status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, space+"/"+provider)
}

func newUpstream(t *testing.T, gotAuth *string, gotPath *string, gotBody *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotAuth = r.Header.Get("Authorization")
		*gotPath = r.URL.Path
		if gotBody != nil {
			b, _ := io.ReadAll(r.Body)
			*gotBody = string(b)
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
}

func newGW(t *testing.T, up *httptest.Server) (*Gateway, *recUsage) {
	t.Helper()
	u, _ := url.Parse(up.URL)
	usage := &recUsage{}
	gw := &Gateway{
		Auth:      fakeAuth{"cs_space1tok": "space1"},
		Creds:     NewLearner(),
		Upstreams: map[string]*url.URL{"anthropic": u, "openai": u},
		Usage:     usage,
	}
	return gw, usage
}

func spaceReq(method, target, token, body string) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	r.RemoteAddr = "192.168.64.4:50000" // from inside a space, not loopback
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func seedCred(t *testing.T, gw *Gateway) {
	t.Helper()
	// host's own local call: unknown token from loopback is learned and forwarded
	r := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader("{}"))
	r.RemoteAddr = "127.0.0.1:49000"
	r.Header.Set("Authorization", "Bearer sk-ant-oat01-real")
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("seed call failed: %d %s", w.Code, w.Body.String())
	}
}

func TestSwapForSpaceToken(t *testing.T) {
	var auth, path, body string
	up := newUpstream(t, &auth, &path, &body)
	defer up.Close()
	gw, usage := newGW(t, up)
	seedCred(t, gw)

	w := httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("POST", "/anthropic/v1/messages?beta=true", "cs_space1tok", `{"model":"x"}`))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if auth != "Bearer sk-ant-oat01-real" {
		t.Fatalf("upstream auth = %q, want swapped real cred", auth)
	}
	if path != "/v1/messages" {
		t.Fatalf("upstream path = %q", path)
	}
	if body != `{"model":"x"}` {
		t.Fatalf("body not passed through: %q", body)
	}
	if len(usage.rows) == 0 || usage.rows[len(usage.rows)-1] != "space1/anthropic" {
		t.Fatalf("usage rows: %v", usage.rows)
	}
}

func TestUnknownTokenRejected(t *testing.T) {
	var auth, path string
	up := newUpstream(t, &auth, &path, nil)
	defer up.Close()
	gw, _ := newGW(t, up)
	seedCred(t, gw)
	path = "" // reset marker written by the seed call

	w := httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("POST", "/anthropic/v1/messages", "cs_stolen", "{}"))
	if w.Code != 401 {
		t.Fatalf("want 401, got %d", w.Code)
	}
	if path == "/v1/messages" {
		t.Fatal("upstream must not be hit for unknown token")
	}
}

func TestDisabledSpaceProviderRejectedBeforeCredentialLookup(t *testing.T) {
	var auth, path string
	up := newUpstream(t, &auth, &path, nil)
	defer up.Close()
	gw, _ := newGW(t, up)
	seedCred(t, gw)
	path = ""
	gw.Policy = providerPolicy{enabled: map[string]bool{"space1/openai": true}}

	w := httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("POST", "/anthropic/v1/messages", "cs_space1tok", "{}"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("disabled provider: want 403 got %d", w.Code)
	}
	if path != "" {
		t.Fatal("disabled provider request must not reach upstream")
	}
}

func TestNoCredYet(t *testing.T) {
	var auth, path string
	up := newUpstream(t, &auth, &path, nil)
	defer up.Close()
	gw, _ := newGW(t, up) // no seed
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("POST", "/anthropic/v1/messages", "cs_space1tok", "{}"))
	if w.Code != 503 {
		t.Fatalf("want 503 before any credential learned, got %d", w.Code)
	}
}

func TestRevocationImmediate(t *testing.T) {
	var auth, path string
	up := newUpstream(t, &auth, &path, nil)
	defer up.Close()
	gw, _ := newGW(t, up)
	seedCred(t, gw)
	auth2 := fakeAuth{"cs_space1tok": "space1"}
	gw.Auth = auth2
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("POST", "/anthropic/v1/messages", "cs_space1tok", "{}"))
	if w.Code != 200 {
		t.Fatalf("pre-revoke should pass: %d", w.Code)
	}
	delete(auth2, "cs_space1tok")
	w = httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("POST", "/anthropic/v1/messages", "cs_space1tok", "{}"))
	if w.Code != 401 {
		t.Fatalf("revoked token must 401, got %d", w.Code)
	}
}

func TestProviderRouting(t *testing.T) {
	var auth, path string
	up := newUpstream(t, &auth, &path, nil)
	defer up.Close()
	gw, usage := newGW(t, up)
	seedCred(t, gw)
	// openai side learns separately
	r := httptest.NewRequest("POST", "/openai/v1/responses", strings.NewReader("{}"))
	r.RemoteAddr = "127.0.0.1:49001"
	r.Header.Set("Authorization", "Bearer eyJ-chatgpt-real")
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("openai seed: %d", w.Code)
	}

	w = httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("POST", "/openai/v1/responses", "cs_space1tok", "{}"))
	if w.Code != 200 || auth != "Bearer eyJ-chatgpt-real" || path != "/v1/responses" {
		t.Fatalf("openai swap failed: code=%d auth=%q path=%q", w.Code, auth, path)
	}
	if usage.rows[len(usage.rows)-1] != "space1/openai" {
		t.Fatalf("usage rows: %v", usage.rows)
	}

	w = httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("GET", "/nonsense/x", "cs_space1tok", ""))
	if w.Code != 404 {
		t.Fatalf("unknown provider prefix: want 404 got %d", w.Code)
	}
}

type tokenRec struct {
	recUsage
	in, out int
}

func (r *tokenRec) RecordTokens(space, provider string, in, out int) { r.in, r.out = in, out }

func TestTokenParsingFromSSE(t *testing.T) {
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":1200,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":340}}` + "\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// write in two chunks, splitting inside the second usage object,
		// to prove cross-chunk parsing
		mid := strings.Index(sse, `"output_tokens":34`) + 5
		w.Write([]byte(sse[:mid]))
		w.(http.Flusher).Flush()
		w.Write([]byte(sse[mid:]))
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	sink := &tokenRec{}
	gw := &Gateway{
		Auth:      fakeAuth{"cs_space1tok": "space1"},
		Creds:     NewLearner(),
		Upstreams: map[string]*url.URL{"anthropic": u},
		Usage:     sink,
	}
	seedCred(t, gw)
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("POST", "/anthropic/v1/messages", "cs_space1tok", "{}"))
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "message_delta") {
		t.Fatal("body not passed through intact")
	}
	if sink.in != 1200 || sink.out != 340 {
		t.Fatalf("tokens parsed in=%d out=%d, want 1200/340", sink.in, sink.out)
	}
}

func TestCredsIsolatedPerProvider(t *testing.T) {
	var auth, path string
	up := newUpstream(t, &auth, &path, nil)
	defer up.Close()
	gw, _ := newGW(t, up)
	seedCred(t, gw) // anthropic only
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("POST", "/openai/v1/responses", "cs_space1tok", "{}"))
	if w.Code != 503 {
		t.Fatalf("openai must not reuse anthropic cred: got %d", w.Code)
	}
}
