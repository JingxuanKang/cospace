package gateway

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

type routePolicy struct {
	route             string
	internet, enabled bool
}

func (p *routePolicy) Limits(string) (float64, int)                 { return 0, 0 }
func (p *routePolicy) ProviderEnabled(string, string) bool          { return p.enabled }
func (p *routePolicy) ExecutionPolicy(string) (string, bool, error) { return p.route, p.internet, nil }

func TestExecutionRouteEnforcedByHost(t *testing.T) {
	var officialCalls, subCalls int
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { officialCalls++; w.Write([]byte(`{}`)) }))
	defer official.Close()
	sub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subCalls++
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer sub-credential" || r.Header.Get("Chatgpt-Account-Id") != "" {
			t.Errorf("wrong routing or auth")
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "gpt-6-astra" {
			t.Errorf("model=%v", body["model"])
		}
		w.Write([]byte(`{}`))
	}))
	defer sub.Close()
	gw, _ := newGW(t, official)
	u, _ := url.Parse(sub.URL + "/v1")
	creds := NewLearner()
	creds.Learn("openai", "Bearer sub-credential")
	gw.Creds.Learn("openai", "Bearer official-credential")
	gw.OpenAIRoutes = map[string]OpenAIRoute{"sub2api-i": {u, creds}}
	policy := &routePolicy{"sub2api-i", true, true}
	gw.Policy = policy
	call := func(body string) int {
		r := spaceReq("POST", "/openai/responses", "cs_space1tok", body)
		r.Header.Set("Chatgpt-Account-Id", "guest-account")
		w := httptest.NewRecorder()
		gw.ServeHTTP(w, r)
		return w.Code
	}
	if got := call(`{"model":"sub2api-ii/gpt-6-astra"}`); got != 200 {
		t.Fatal(got)
	}
	if subCalls != 1 || officialCalls != 0 {
		t.Fatal("guest changed route")
	}
	policy.route = "sub2api-ii" // unconfigured must not fall back to official
	if got := call(`{"model":"gpt-6-astra"}`); got != 503 {
		t.Fatal(got)
	}
	policy.route = "official"
	if got := call(`{"model":"gpt-6-astra"}`); got != 200 || officialCalls != 1 {
		t.Fatal(got, officialCalls)
	}
	policy.enabled = false
	if got := call(`{"model":"gpt-6-astra"}`); got != 403 {
		t.Fatal(got)
	}
	if officialCalls != 1 {
		t.Fatal("provider disable bypassed")
	}
}

func TestOfflineModelRequests(t *testing.T) {
	cases := []struct {
		name, path, body string
		allow            bool
	}{
		{"plain", "/openai/responses", `{"model":"gpt-6-astra","input":"draw a mountain"}`, true},
		{"local functions", "/openai/responses", `{"tools":[{"type":"function","name":"exec","parameters":{"properties":{"url":{"const":"https://example.org"}}}}]}`, true},
		{"namespaced local", "/openai/responses", `{"tools":[{"type":"namespace","tools":[{"type":"function","name":"exec"}]}]}`, true},
		{"local shell", "/openai/responses", `{"tools":[{"type":"shell","environment":{"type":"local"}}]}`, true},
		{"inline image", "/openai/responses", `{"input":[{"image_url":"data:image/png;base64,AAAA"}]}`, true},
		{"web search", "/openai/responses", `{"tools":[{"type":"web_search"}]}`, false},
		{"nested web search", "/openai/responses", `{"tools":[{"type":"namespace","tools":[{"type":"web_search_preview"}]}]}`, false},
		{"hosted shell", "/openai/responses", `{"tools":[{"type":"shell","environment":{"type":"container_auto"}}]}`, false},
		{"mcp", "/openai/responses", `{"tools":[{"type":"mcp"}]}`, false},
		{"remote media", "/openai/responses", `{"input":[{"image_url":"https://example.org/a.png"}]}`, false},
		{"chat image", "/openai/chat/completions", `{"messages":[{"content":[{"image_url":{"url":"https://example.org/a.png"}}]}]}`, false},
		{"unknown cloud context", "/openai/responses", `{"previous_response_id":"resp_old"}`, false},
		{"file endpoints", "/openai/files", `{}`, false},
		{"claude local", "/anthropic/v1/messages", `{"tools":[{"name":"exec","input_schema":{}}]}`, true},
		{"claude search", "/anthropic/v1/messages", `{"tools":[{"name":"web_search","type":"web_search_20250305"}]}`, false},
		{"xai search", "/xai/responses", `{"search_parameters":{"mode":"auto"}}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			provider := strings.Split(c.path, "/")[1]
			r := httptest.NewRequest("POST", c.path, strings.NewReader(c.body))
			_, err := prepareRequest(r, provider, "official", false)
			if (err == nil) != c.allow {
				t.Fatalf("allow=%v err=%v", c.allow, err)
			}
		})
	}
}

func TestCompressedCodexRequest(t *testing.T) {
	for _, encoding := range []string{"gzip", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			var buf bytes.Buffer
			if encoding == "gzip" {
				w := gzip.NewWriter(&buf)
				w.Write([]byte(`{"model":"sub2api-i/gpt-6-astra","n":9007199254740993}`))
				w.Close()
			} else {
				w, _ := zstd.NewWriter(&buf)
				w.Write([]byte(`{"model":"sub2api-i/gpt-6-astra","n":9007199254740993}`))
				w.Close()
			}
			r := httptest.NewRequest("POST", "/openai/responses", &buf)
			r.Header.Set("Content-Encoding", encoding)
			out, err := prepareRequest(r, "openai", "sub2api-i", false)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(out.Body)
			if !strings.Contains(string(b), `"n":9007199254740993`) || out.Header.Get("Content-Encoding") != "" || out.ContentLength != int64(len(b)) {
				t.Fatal("compressed payload changed incorrectly")
			}
		})
	}
}
