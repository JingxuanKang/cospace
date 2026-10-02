package gateway

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

var errNoKeychain = errors.New("no keychain")

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// jwtWithExp builds an unsigned JWT whose only claim is exp.
func jwtWithExp(exp time.Time) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + itoa(exp.Unix()) + `}`))
	return hdr + "." + payload + "."
}

// A space token must only ever reach paths below the provider's configured
// base: "/openai/../../x" cleans to "/x" and is not a provider at all.
func TestPathTraversalCannotEscapeUpstreamBase(t *testing.T) {
	var gotAuth, gotPath string
	up := newUpstream(t, &gotAuth, &gotPath, nil)
	defer up.Close()
	base, _ := url.Parse(up.URL + "/backend-api/codex")
	gw, _ := newGW(t, up)
	gw.Upstreams["openai"] = base
	seedCred(t, gw)
	gw.Creds.Learn("openai", "Bearer eyJ-chatgpt-fake")

	for _, target := range []string{
		"/openai/../../backend-api/me",
		"/openai/%2e%2e/%2e%2e/backend-api/me",
		"/openai/responses/../../auth/session",
	} {
		gotPath = ""
		w := httptest.NewRecorder()
		gw.ServeHTTP(w, spaceReq("GET", target, "cs_space1tok", ""))
		if w.Code != http.StatusNotFound || gotPath != "" {
			t.Fatalf("%s: code=%d upstream path=%q (want 404, no upstream call)", target, w.Code, gotPath)
		}
	}
	// Dot segments inside the provider prefix still land below the base.
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("GET", "/openai/./responses//x", "cs_space1tok", ""))
	if w.Code != 200 || gotPath != "/backend-api/codex/responses/x" {
		t.Fatalf("cleaned path: code=%d upstream path=%q", w.Code, gotPath)
	}
}

// A client that asks for gzip must still be metered: the gateway strips
// Accept-Encoding so the response it scans is plain text.
func TestGzipResponsesAreStillMetered(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := []byte(`{"model":"claude-sonnet-4","usage":{"input_tokens":120,"output_tokens":30}}`)
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			var buf bytes.Buffer
			z := gzip.NewWriter(&buf)
			z.Write(body)
			z.Close()
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(buf.Bytes())
			return
		}
		w.Write(body)
	}))
	defer up.Close()
	gw, _ := newGW(t, up)
	rec := &tokenRec{}
	gw.Usage = rec
	seedCred(t, gw)

	r := spaceReq("POST", "/anthropic/v1/messages", "cs_space1tok", `{"stream":false}`)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("code %d", w.Code)
	}
	if rec.in != 120 || rec.out != 30 {
		t.Fatalf("metered in=%d out=%d, want 120/30", rec.in, rec.out)
	}
	if w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("space received compressed bytes: %v", w.Header())
	}
	if !strings.Contains(w.Body.String(), `"input_tokens":120`) {
		t.Fatalf("body not passed through: %s", w.Body.String())
	}
}

// Prompt-cache tokens are the bulk of an agent session's input and must be
// priced and charted.
func TestCacheTokensAreBilled(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"claude-sonnet-4","usage":{"input_tokens":10,"cache_creation_input_tokens":2000,"cache_read_input_tokens":100000,"output_tokens":500}}`))
	}))
	defer up.Close()
	gw, _ := newGW(t, up)
	rec := &costRec{}
	gw.Usage = rec
	seedCred(t, gw)
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("POST", "/anthropic/v1/messages", "cs_space1tok", `{}`))
	if rec.in != 102010 || rec.out != 500 {
		t.Fatalf("tokens in=%d out=%d", rec.in, rec.out)
	}
	// sonnet: $3/M in, $15/M out; cache read 0.1×, cache write 1.25×
	want := 10*3e-6 + 100000*3e-6*0.1 + 2000*3e-6*1.25 + 500*15e-6
	if rec.cost < want*0.999 || rec.cost > want*1.001 {
		t.Fatalf("cost %.6f want %.6f", rec.cost, want)
	}
	// OpenAI reports cached tokens as a subset of input.
	in, _, cost := billTokens("gpt-5.6-sol", "openai", 1000, 10, 0, 0, 900)
	wantOA := 100*1.25e-6 + 900*1.25e-6*0.1 + 10*10e-6
	if in != 1000 || cost < wantOA*0.999 || cost > wantOA*1.001 {
		t.Fatalf("openai cached: in=%d cost=%.8f want %.8f", in, cost, wantOA)
	}
}

type costRec struct {
	in, out int
	cost    float64
}

func (c *costRec) Record(string, string, int) {}
func (c *costRec) RecordCost(_, _, _ string, in, out int, cost float64) {
	c.in, c.out, c.cost = in, out, cost
}

// Streamed chat completions get stream_options.include_usage so they carry a
// usage block to meter.
func TestStreamedChatCompletionsRequestUsage(t *testing.T) {
	r := httptest.NewRequest("POST", "/openai/chat/completions", strings.NewReader(`{"model":"gpt-5.6-sol","stream":true,"messages":[]}`))
	out, err := prepareRequest(r, "openai", "official", true)
	if err != nil {
		t.Fatal(err)
	}
	buf := new(bytes.Buffer)
	buf.ReadFrom(out.Body)
	if !strings.Contains(buf.String(), `"stream_options":{"include_usage":true}`) {
		t.Fatalf("include_usage not injected: %s", buf.String())
	}
	r = httptest.NewRequest("POST", "/xai/chat/completions", strings.NewReader(`{"model":"grok-4","stream":true,"stream_options":{"include_usage":false}}`))
	out, _ = prepareRequest(r, "xai", "official", true)
	buf.Reset()
	buf.ReadFrom(out.Body)
	if !strings.Contains(buf.String(), `"include_usage":false`) {
		t.Fatalf("explicit client choice overridden: %s", buf.String())
	}
}

// Offline mode rejects fetches, not data: a URL inside an earlier tool call's
// input or a tool result must not poison the rest of the conversation.
func TestOfflineAllowsURLsInToolHistory(t *testing.T) {
	allowed := []string{
		`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"WebFetch","input":{"url":"https://example.org"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"fetched https://example.org"}]}]}`,
		`{"input":[{"type":"function_call","name":"fetch","arguments":"{\"url\":\"https://example.org\"}"}]}`,
		`{"input":[{"type":"function_call_output","call_id":"c","output":"{\"url\":\"https://example.org\"}"}]}`,
		`{"messages":[{"role":"user","content":"see https://example.org"}]}`,
	}
	for _, body := range allowed {
		r := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(body))
		if _, err := prepareRequest(r, "anthropic", "official", false); err != nil {
			t.Fatalf("history rejected: %v\n%s", err, body)
		}
	}
	rejected := []string{
		`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.org/a.png"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"url","url":"https://example.org/a.pdf"}}]}]}`,
		`{"input":[{"type":"input_image","image_url":"https://example.org/a.png"}]}`,
		`{"mcp_servers":[{"type":"url","url":"https://mcp.example.org","name":"x"}]}`,
	}
	for _, body := range rejected {
		r := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(body))
		if _, err := prepareRequest(r, "anthropic", "official", false); err == nil {
			t.Fatalf("remote fetch allowed offline:\n%s", body)
		}
	}
}

// Upstream failures do not echo the upstream host to the space, and a
// redirect is handed back rather than followed with the host's credential.
func TestUpstreamErrorsStayGeneric(t *testing.T) {
	var gotAuth, gotPath string
	dead := newUpstream(t, &gotAuth, &gotPath, nil)
	gw, _ := newGW(t, dead)
	seedCred(t, gw)
	dead.Close() // connection refused from now on
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, spaceReq("POST", "/anthropic/v1/messages", "cs_space1tok", `{}`))
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "127.0.0.1") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://attacker.example/collect", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	gw2, _ := newGW(t, redirector)
	gw2.Creds.Learn("anthropic", "Bearer sk-ant-fake")
	w = httptest.NewRecorder()
	gw2.ServeHTTP(w, spaceReq("POST", "/anthropic/v1/messages", "cs_space1tok", `{}`))
	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("redirect was followed: code=%d", w.Code)
	}
}

// Claude credentials: the copy with the later expiry wins, wherever it lives.
func TestClaudeOAuthPrefersFreshestCopy(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/.credentials.json"
	fileExp := time.Now().Add(time.Hour).UnixMilli()
	keyExp := time.Now().Add(3 * time.Hour).UnixMilli()
	mustWrite(t, path, `{"claudeAiOauth":{"accessToken":"file-token","expiresAt":`+itoa(fileExp)+`}}`)

	old := keychainReader
	defer func() { keychainReader = old; InvalidateClaudeKeychainCache() }()
	keychainReader = func() ([]byte, error) {
		return []byte(`{"claudeAiOauth":{"accessToken":"keychain-token","expiresAt":` + itoa(keyExp) + `}}`), nil
	}
	InvalidateClaudeKeychainCache()
	c, err := ClaudeOAuth(path)
	if err != nil || c.AccessToken != "keychain-token" || c.Source != "keychain" {
		t.Fatalf("got %+v err=%v", c, err)
	}
	// The file is renewed past the keychain copy → file wins.
	mustWrite(t, path, `{"claudeAiOauth":{"accessToken":"file-token-2","expiresAt":`+itoa(keyExp+1000)+`}}`)
	c, _ = ClaudeOAuth(path)
	if c.AccessToken != "file-token-2" || c.Source != "file" {
		t.Fatalf("got %+v", c)
	}
	// No keychain at all (Linux, or an item that was never created) → file.
	keychainReader = func() ([]byte, error) { return nil, errNoKeychain }
	InvalidateClaudeKeychainCache()
	c, err = ClaudeOAuth(path)
	if err != nil || c.AccessToken != "file-token-2" {
		t.Fatalf("got %+v err=%v", c, err)
	}
	fc := NewFileCreds(map[string]string{"anthropic": path})
	if h, ok := fc.Get("anthropic"); !ok || h != "Bearer file-token-2" {
		t.Fatalf("FileCreds anthropic = %q %v", h, ok)
	}
}

// grok: the entry expiring last is the live one, regardless of map order.
func TestLatestXAIKey(t *testing.T) {
	soon := jwtWithExp(time.Now().Add(time.Hour))
	later := jwtWithExp(time.Now().Add(5 * time.Hour))
	doc := `{"issuer::a":{"key":"` + soon + `"},"issuer::b":{"key":"` + later + `"},"issuer::c":{"key":""}}`
	for i := 0; i < 20; i++ {
		got, err := LatestXAIKey([]byte(doc))
		if err != nil || got != later {
			t.Fatalf("iteration %d: got %q err=%v", i, got, err)
		}
	}
}
