package api

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JingxuanKang/cospace/internal/container"
	"github.com/JingxuanKang/cospace/internal/dist"
	"github.com/JingxuanKang/cospace/internal/imagesync"
	"github.com/JingxuanKang/cospace/internal/spaces"
	"github.com/JingxuanKang/cospace/internal/usage"
)

func TestInviteUIOffersWindowsInstructions(t *testing.T) {
	page, err := fs.ReadFile(webFS, "web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	for _, want := range []string{
		`data-action="pick-invite-platform"`,
		`guestInstallCommand(invite, state.invitePlatform)`,
		`invite.install`,
		`invite — ${terminalShell}`,
		`class="hair-row guest-row"`,
		`class="text-button guest-revoke`,
		`class="sponsor-grid"`,
		`data-action="toggle-sponsor"`,
		`Memory used`,
		`AI for all spaces`,
		`sleeping · Host wake required`,
		`A provider also has to be on under AI for all spaces on the overview.`,
		`data-action="toggle-fullauto"`,
		`Full-auto agents`,
		`data-action="save-template"`,
		`id="new-space-template"`,
		`data-action="delete-template"`,
		`This space's switches are saved independently.`,
		`.provider-logo.anthropic`,
		`.provider-logo.openai`,
		`.provider-logo.xai`,
		`<title id="anthropic-logo-title">Anthropic</title>`,
		`<title id="openai-logo-title">OpenAI</title>`,
		`alt="xAI"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("invite UI missing %q", want)
		}
	}
}

type fakeRunner struct{ out map[string]string }

func (f *fakeRunner) Run(args ...string) (string, error) {
	if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "cat /etc/ssh/ssh_host_ed25519_key.pub") {
		return "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPojEOfYY/ehOeqXAffycIiI3cPQA71LbOnLeD8hqmDK space", nil
	}
	if v, ok := f.out[args[0]]; ok {
		return v, nil
	}
	return "", nil
}

type fakeInviter struct{ fail bool }

func (f fakeInviter) Invite(space string, ttl time.Duration) (string, string, time.Time, error) {
	return "K7M4-Q2PX", "gr pair tcABC K7M4-Q2PX", time.Now().Add(ttl), nil
}

func (f fakeInviter) Peek(code string) (string, string, time.Time, error) {
	if code != "K7M4-Q2PX" {
		return "", "", time.Time{}, errors.New("invalid invite code")
	}
	return "acme", "cospace pair tcABC K7M4-Q2PX", time.Now().Add(9 * time.Minute), nil
}

func newServer(t *testing.T) (*Server, *spaces.Manager) {
	t.Helper()
	f := &fakeRunner{out: map[string]string{"list": `[
	  {"status":{"state":"running","networks":[{"ipv4Address":"192.168.64.4/24"}]},"configuration":{"id":"acme"}}
	]`}}
	m := &spaces.Manager{Dir: t.TempDir(), C: container.Client{R: f}, Image: "cospace-base", GatewayURL: "http://g"}
	us, err := usage.Open(filepath.Join(t.TempDir(), "u.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Spaces:     m,
		Usage:      us,
		Inviter:    fakeInviter{},
		MemTotalGB: func() int { return 32 },
		OnBattery:  func() bool { return true },
	}
	return s, m
}

func doJSON(t *testing.T, h http.Handler, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestSpaceLifecycleOverHTTP(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()

	w, created := doJSON(t, h, "POST", "/api/spaces", `{"name":"acme","memory_gb":2,"cpus":4}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if created["name"] != "acme" || !strings.HasPrefix(created["token"].(string), "cs_") {
		t.Fatalf("create body: %v", created)
	}
	s.Usage.RecordTokens("acme", "anthropic", 100, 40)

	req := httptest.NewRequest("GET", "/api/spaces", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var list []map[string]any
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 || list[0]["state"] != "running" || list[0]["ip"] != "192.168.64.4" {
		t.Fatalf("list: %s", w.Body.String())
	}
	ut := list[0]["usage_today"].(map[string]any)
	if ut["tokens_out"].(float64) != 40 {
		t.Fatalf("usage_today: %v", ut)
	}

	if w, _ := doJSON(t, h, "POST", "/api/spaces/acme/stop", ""); w.Code != 204 {
		t.Fatalf("stop: %d", w.Code)
	}
	req = httptest.NewRequest("GET", "/api/spaces", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 || list[0]["manual_sleep"] != true {
		t.Fatalf("manual sleep missing from list: %s", w.Body.String())
	}
	if w, _ := doJSON(t, h, "POST", "/api/spaces/acme/start", ""); w.Code != 204 {
		t.Fatalf("start: %d", w.Code)
	}
	if w, _ := doJSON(t, h, "DELETE", "/api/spaces/acme", ""); w.Code != 204 {
		t.Fatalf("delete: %d", w.Code)
	}
	if w, _ := doJSON(t, h, "DELETE", "/api/spaces/ghost", ""); w.Code != 404 {
		t.Fatalf("delete missing space: want 404 got %d", w.Code)
	}
}

func TestMemberRevokeAndInvite(t *testing.T) {
	s, m := newServer(t)
	h := s.Handler()
	m.Create("acme", 2, 4)
	m.AddMember("acme", "Alice", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPojEOfYY/ehOeqXAffycIiI3cPQA71LbOnLeD8hqmDK a@e")

	if w, _ := doJSON(t, h, "DELETE", "/api/spaces/acme/members/Alice", ""); w.Code != 204 {
		t.Fatalf("revoke: %d", w.Code)
	}
	if w, _ := doJSON(t, h, "DELETE", "/api/spaces/acme/members/Alice", ""); w.Code != 404 {
		t.Fatalf("revoke twice: want 404 got %d", w.Code)
	}

	w, inv := doJSON(t, h, "POST", "/api/spaces/acme/invites", "")
	if w.Code != 201 || inv["command"] != "gr pair tcABC K7M4-Q2PX" {
		t.Fatalf("invite: %d %v", w.Code, inv)
	}
	if install, _ := inv["install"].(map[string]any); install["posix"] != dist.GuestInstallPOSIX || install["windows"] != dist.GuestInstallWindows {
		t.Fatalf("invite install commands: %v", inv["install"])
	}

	s.Inviter = nil
	if w, _ := doJSON(t, h, "POST", "/api/spaces/acme/invites", ""); w.Code != 501 {
		t.Fatalf("invite without transport: want 501 got %d", w.Code)
	}
}

func TestHostAndUsageEndpoints(t *testing.T) {
	s, m := newServer(t)
	h := s.Handler()
	m.Create("acme", 2, 4)
	s.Usage.RecordTokens("acme", "anthropic", 100, 40)

	w, host := doJSON(t, h, "GET", "/api/host", "")
	if w.Code != 200 || host["memory_total_gb"].(float64) != 32 || host["on_battery"] != true || host["awake"].(float64) != 1 {
		t.Fatalf("host: %v", host)
	}

	req := httptest.NewRequest("GET", "/api/spaces/acme/usage?days=14", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req)
	var days []map[string]any
	json.Unmarshal(w2.Body.Bytes(), &days)
	if len(days) != 1 || days[0]["tokens_in"].(float64) != 100 {
		t.Fatalf("usage: %s", w2.Body.String())
	}
}

func TestValidationErrors(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	// "Bad Name" is normalized to "bad-name" and succeeds (friendly UX).
	if w, got := doJSON(t, h, "POST", "/api/spaces", `{"name":"Bad Name"}`); w.Code != 201 || got["name"] != "bad-name" {
		t.Fatalf("normalized name: want 201/bad-name got %d/%v", w.Code, got["name"])
	}
	// Input with no usable letters/digits still fails.
	if w, _ := doJSON(t, h, "POST", "/api/spaces", `{"name":"!!!"}`); w.Code != 400 {
		t.Fatalf("unnormalizable name: want 400 got %d", w.Code)
	}
	if w, _ := doJSON(t, h, "POST", "/api/spaces", `not json`); w.Code != 400 {
		t.Fatalf("bad json: want 400 got %d", w.Code)
	}
}

type fakeSponsor struct{ state map[string]ProviderSponsor }

func (f *fakeSponsor) State() map[string]ProviderSponsor { return f.state }
func (f *fakeSponsor) Set(provider string, on bool) error {
	entry := f.state[provider]
	entry.Enabled = on
	f.state[provider] = entry
	return nil
}

func (f *fakeSponsor) SetAll(on bool) error {
	for p, e := range f.state {
		e.Enabled = on
		f.state[p] = e
	}
	return nil
}

func TestSponsorToggle(t *testing.T) {
	s, _ := newServer(t)
	s.Sponsor = &fakeSponsor{state: map[string]ProviderSponsor{
		"anthropic": {Enabled: true, CredPresent: true},
		"openai":    {Enabled: false, CredPresent: false},
	}}
	h := s.Handler()

	w, _ := doJSON(t, h, "GET", "/api/sponsor", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"cred_present":true`) {
		t.Fatalf("get sponsor: %d %s", w.Code, w.Body.String())
	}
	w, out := doJSON(t, h, "POST", "/api/sponsor", `{"enabled":false}`)
	if w.Code != 200 || out["anthropic"].(map[string]any)["enabled"] != false || out["openai"].(map[string]any)["enabled"] != false {
		t.Fatalf("master toggle not applied: %d %v", w.Code, out)
	}
	if w, out := doJSON(t, h, "POST", "/api/sponsor", `{"provider":"openai","enabled":true}`); w.Code != 200 {
		t.Fatalf("per-provider set: %d %v", w.Code, out)
	}
	if w, _ := doJSON(t, h, "POST", "/api/sponsor", `{"provider":"openai"}`); w.Code != 400 {
		t.Fatalf("per-provider host toggle: want 400 got %d", w.Code)
	}
	if w, _ := doJSON(t, h, "POST", "/api/sponsor", `{}`); w.Code != 400 {
		t.Fatalf("missing enabled: want 400 got %d", w.Code)
	}
}

func TestServesConsole(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "CoSpace") {
		t.Fatalf("console index: %d", w.Code)
	}
}

func TestFullAutoAndTemplatesOverHTTP(t *testing.T) {
	s, _ := newServer(t)
	s.Templates = spaces.NewTemplateStore(filepath.Join(t.TempDir(), "templates.json"))
	h := s.Handler()

	res, _ := doJSON(t, h, "POST", "/api/spaces", `{"name":"acme"}`)
	if res.Code != 201 {
		t.Fatalf("create: %d %s", res.Code, res.Body)
	}

	listSpaces := func() []map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/spaces", nil))
		var views []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &views); err != nil {
			t.Fatalf("spaces list: %v: %s", err, w.Body)
		}
		return views
	}
	if views := listSpaces(); len(views) != 1 || views[0]["full_auto"] != true {
		t.Fatalf("new space should report full_auto=true: %+v", views)
	}

	if res, _ := doJSON(t, h, "PUT", "/api/spaces/acme/full_auto", `{"enabled":false}`); res.Code != 204 {
		t.Fatalf("set full_auto: %d %s", res.Code, res.Body)
	}
	if views := listSpaces(); views[0]["full_auto"] != false {
		t.Fatalf("full_auto did not stick: %+v", views)
	}

	if res, _ := doJSON(t, h, "PUT", "/api/templates/classroom", `{"from_space":"acme"}`); res.Code != 201 {
		t.Fatalf("save template: %d %s", res.Code, res.Body)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/templates", nil))
	var templates []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &templates); err != nil || len(templates) != 1 {
		t.Fatalf("templates list: %v: %s", err, w.Body)
	}
	if templates[0]["name"] != "classroom" || templates[0]["full_auto"] != false {
		t.Fatalf("template did not capture the space's settings: %+v", templates[0])
	}

	if res, _ := doJSON(t, h, "POST", "/api/spaces", `{"name":"acme-two","template":"classroom"}`); res.Code != 201 {
		t.Fatalf("create from template: %d %s", res.Code, res.Body)
	}
	for _, view := range listSpaces() {
		if view["name"] == "acme-two" && view["full_auto"] != false {
			t.Fatalf("template full_auto not applied: %+v", view)
		}
	}

	if res, _ := doJSON(t, h, "POST", "/api/spaces", `{"name":"x9","template":"missing"}`); res.Code != 404 {
		t.Fatalf("missing template should 404: %d %s", res.Code, res.Body)
	}
	if res, _ := doJSON(t, h, "DELETE", "/api/templates/classroom", ""); res.Code != 204 {
		t.Fatalf("delete template: %d %s", res.Code, res.Body)
	}
}

func TestInvitePageServesLiveAndDeadLinks(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/i/K7M4-Q2PX", nil))
	if w.Code != 200 {
		t.Fatalf("live invite page: %d", w.Code)
	}
	live := w.Body.String()
	for _, want := range []string{"cospace pair tcABC K7M4-Q2PX", "acme", dist.GuestInstallPOSIX, "install.ps1", "Settings → Connections → SSH", "environment picker"} {
		if !strings.Contains(live, want) {
			t.Fatalf("live invite page missing %q", want)
		}
	}
	if got := w.Header().Get("X-Robots-Tag"); got != "noindex" {
		t.Fatalf("X-Robots-Tag = %q", got)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/i/AAAA-AAAA", nil))
	if w.Code != 404 || !strings.Contains(w.Body.String(), "This invite has expired") {
		t.Fatalf("dead invite page: %d", w.Code)
	}

	s.Inviter = nil
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/i/K7M4-Q2PX", nil))
	if w.Code != 404 {
		t.Fatalf("no-inviter invite page: %d", w.Code)
	}
}

func TestSpaceCreationWaitsForBaseImage(t *testing.T) {
	s, _ := newServer(t)
	s.Image = &imagesync.Syncer{Ref: "ghcr.io/example/base:1"} // not started: still checking
	h := s.Handler()
	w, host := doJSON(t, h, "GET", "/api/host", "")
	image, _ := host["image"].(map[string]any)
	if w.Code != 200 || image["state"] != "checking" || image["ref"] != "ghcr.io/example/base:1" {
		t.Fatalf("host image status: %d %v", w.Code, host["image"])
	}
	if w, body := doJSON(t, h, "POST", "/api/spaces", `{"name":"early"}`); w.Code != http.StatusConflict {
		t.Fatalf("create before image ready: want 409 got %d %v", w.Code, body)
	}
}
