package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The mux decodes %2F into {name}; the handler must refuse anything that is
// not a plain space name before the manager sees it.
func TestTraversalNamesAre400(t *testing.T) {
	s, m := newServer(t)
	h := s.Handler()
	m.Create("acme", 2, 4)
	for _, path := range []string{
		"/api/spaces/acme%2Fworkspace%2Fevil/limits",
		"/api/spaces/..%2F..%2Fx/limits",
		"/api/spaces/ACME/limits",
		"/api/spaces/acme%2Fworkspace%2Fevil/providers",
		"/api/spaces/acme%2Fworkspace%2Fevil/full_auto",
		"/api/spaces/acme%2Fworkspace%2Fevil/codex",
		"/api/spaces/acme%2Fworkspace%2Fevil/network",
	} {
		req := httptest.NewRequest("PUT", path, strings.NewReader(`{"enabled":true,"usd_limit":1,"providers":[],"route":"official","model":"gpt-5.6-sol","internet_access":true}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatalf("%s: want 400 got %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/spaces/acme%2Fworkspace%2Fevil/start"},
		{"POST", "/api/spaces/acme%2Fworkspace%2Fevil/invites"},
		{"POST", "/api/spaces/..%2Fx/upgrade-network"},
		{"DELETE", "/api/spaces/..%2Fx"},
		{"DELETE", "/api/spaces/acme%2Fworkspace%2Fevil"},
		{"DELETE", "/api/spaces/acme%2Fworkspace%2Fevil/members/bob"},
		{"GET", "/api/spaces/acme%2Fworkspace%2Fevil/usage"},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatalf("%s %s: want 400 got %d", c.method, c.path, w.Code)
		}
	}
	req := httptest.NewRequest("DELETE", "/api/spaces/acme/members/..%2Fx", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("member traversal: want 400 got %d", w.Code)
	}
}

// Browser-initiated cross-site requests carry Sec-Fetch-Site / Origin; the
// console must refuse them. Scripts without those headers still work.
func TestMutationsRefuseCrossSite(t *testing.T) {
	s, m := newServer(t)
	s.AllowedHosts = []string{"console.example.net"}
	s.StrictHost = true
	h := s.Handler()
	m.Create("acme", 2, 4)

	try := func(host string, hdr map[string]string) int {
		req := httptest.NewRequest("POST", "/api/spaces/acme/stop", nil)
		req.Host = host
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if c := try("127.0.0.1:18931", nil); c != 204 {
		t.Fatalf("plain script request: want 204 got %d", c)
	}
	if c := try("127.0.0.1:18931", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:18931"}); c != 204 {
		t.Fatalf("same-origin: want 204 got %d", c)
	}
	if c := try("localhost:18931", map[string]string{"Origin": "http://localhost:18931"}); c != 204 {
		t.Fatalf("localhost origin: want 204 got %d", c)
	}
	if c := try("console.example.net", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://console.example.net"}); c != 204 {
		t.Fatalf("published host: want 204 got %d", c)
	}
	if c := try("127.0.0.1:18931", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}); c != 403 {
		t.Fatalf("cross-site: want 403 got %d", c)
	}
	if c := try("127.0.0.1:18931", map[string]string{"Origin": "https://evil.example"}); c != 403 {
		t.Fatalf("foreign origin: want 403 got %d", c)
	}
	if c := try("127.0.0.1:18931", map[string]string{"Origin": "null"}); c != 403 {
		t.Fatalf("null origin: want 403 got %d", c)
	}
	// DNS rebinding: a Host we do not serve.
	if c := try("rebind.attacker.example", nil); c != 403 {
		t.Fatalf("unexpected host: want 403 got %d", c)
	}
	// GETs are never blocked by the origin rule (they mutate nothing).
	req := httptest.NewRequest("GET", "/api/spaces", nil)
	req.Host = "127.0.0.1:18931"
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("cross-site GET: want 200 got %d", w.Code)
	}
}

// The list payload identifies members by fingerprint only.
func TestListOmitsPublicKeys(t *testing.T) {
	s, m := newServer(t)
	h := s.Handler()
	m.Create("acme", 2, 4)
	m.AddMember("acme", "alice", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPojEOfYY/ehOeqXAffycIiI3cPQA71LbOnLeD8hqmDK a@e")
	w, _ := doJSON(t, h, "GET", "/api/spaces", "")
	body := w.Body.String()
	if strings.Contains(body, "AAAAC3NzaC1lZDI1NTE5") || strings.Contains(body, `"pub_key"`) || strings.Contains(body, `"token"`) {
		t.Fatalf("list leaks key material: %s", body)
	}
	if !strings.Contains(body, `"fingerprint":"SHA256:`) {
		t.Fatalf("list lacks fingerprint: %s", body)
	}
}

// Oversized JSON bodies are refused before they are decoded.
func TestBodiesAreBounded(t *testing.T) {
	s, m := newServer(t)
	h := s.Handler()
	m.Create("acme", 2, 4)
	big := `{"usd_limit":1,"pad":"` + strings.Repeat("x", maxBodyBytes+1) + `"}`
	req := httptest.NewRequest("PUT", "/api/spaces/acme/limits", strings.NewReader(big))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("oversized body: want 400 got %d", w.Code)
	}
}

// The public invite page is rate limited per client and carries a CSP.
func TestInvitePageIsRateLimited(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	var last int
	for i := 0; i < inviteBurst+5; i++ {
		req := httptest.NewRequest("GET", "/i/K7M4-Q2PX", nil)
		req.RemoteAddr = "203.0.113.9:4444"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		last = w.Code
		if i == 0 {
			if w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" {
				t.Fatalf("first page: %d csp=%q", w.Code, w.Header().Get("Content-Security-Policy"))
			}
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after burst: want 429 got %d", last)
	}
	// Another client is unaffected.
	req := httptest.NewRequest("GET", "/i/K7M4-Q2PX", nil)
	req.RemoteAddr = "203.0.113.10:4444"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("other client: want 200 got %d", w.Code)
	}
}

// The invite page follows ?lang= first, then Accept-Language, and defaults
// to English; both renderings carry the pair command and a switch link.
func TestInvitePageLanguage(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	get := func(target, acceptLanguage string) (int, string) {
		req := httptest.NewRequest("GET", target, nil)
		if acceptLanguage != "" {
			req.Header.Set("Accept-Language", acceptLanguage)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	code, body := get("/i/K7M4-Q2PX", "")
	if code != 200 || !strings.Contains(body, `lang="en"`) || !strings.Contains(body, "You're invited") || !strings.Contains(body, "cospace pair tcABC K7M4-Q2PX") {
		t.Fatalf("default page: %d %s", code, body[:200])
	}
	code, body = get("/i/K7M4-Q2PX", "zh-CN,zh;q=0.9,en;q=0.8")
	if code != 200 || !strings.Contains(body, `lang="zh-CN"`) || !strings.Contains(body, "邀请你加入") || !strings.Contains(body, "cospace pair tcABC K7M4-Q2PX") || !strings.Contains(body, `href="?lang=en"`) {
		t.Fatalf("zh page: %d %s", code, body[:200])
	}
	code, body = get("/i/K7M4-Q2PX?lang=en", "zh-CN")
	if code != 200 || !strings.Contains(body, `lang="en"`) || strings.Contains(body, "邀请你加入") {
		t.Fatalf("?lang=en must win over Accept-Language: %d", code)
	}
	code, body = get("/i/NOPE-NOPE?lang=zh", "")
	if code != 404 || !strings.Contains(body, "这个邀请已失效") {
		t.Fatalf("dead page zh: %d", code)
	}
}
