package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExecutionControlsAPI(t *testing.T) {
	s, m := newServer(t)
	m.GatewayURL = "http://192.168.64.1:18930"
	m.Create("acme", 1, 2)
	s.CodexRoutes = []CodexRouteOption{{"official", "Official", true}, {"sub2api-i", "Sub2API I", true}, {"sub2api-ii", "Sub2API II", false}}
	h := s.Handler()
	for _, c := range []struct {
		path, body string
		status     int
	}{
		{"codex", `{"route":"sub2api-i","model":"gpt-6-astra"}`, 204},
		{"codex", `{"route":"sub2api-ii","model":"gpt-6-astra"}`, 400},
		{"network", `{}`, 400},
		{"network", `{"internet_access":false}`, 204},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("PUT", "/api/spaces/acme/"+c.path, strings.NewReader(c.body)))
		if w.Code != c.status {
			t.Fatalf("%s: %d %s", c.path, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/spaces", nil))
	for _, want := range []string{`"codex_route":"sub2api-i"`, `"codex_model":"gpt-6-astra"`, `"network_mode":"gateway-only"`, `"network_control_ready":true`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing %s", want)
		}
	}
}
