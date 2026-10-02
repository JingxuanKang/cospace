package spaces

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestFullAutoSyncAndToggle(t *testing.T) {
	m, f := newTestManager(t)
	r, err := m.Create("auto", 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !r.FullAuto {
		t.Fatal("new spaces should default to full-auto")
	}
	execs := strings.Join(flatten(f.callsNamed("exec")), " ")
	for _, want := range []string{
		`"defaultMode":"bypassPermissions"`,
		"/home/space/.claude/settings.json",
		`"statusLine"`,
		"COSPACE_SPACE=auto",
		`approval_policy = "never"`,
		`sandbox_mode = "danger-full-access"`,
	} {
		if !strings.Contains(execs, want) {
			t.Fatalf("full-auto sync missing %q in: %s", want, execs)
		}
	}

	f.calls = nil
	f.out["list"] = `[{"configuration":{"id":"auto"},"status":{"state":"running"}}]`
	if err := m.SetFullAuto("auto", false); err != nil {
		t.Fatal(err)
	}
	execs = strings.Join(flatten(f.callsNamed("exec")), " ")
	if strings.Contains(execs, `"defaultMode":"bypassPermissions"`) {
		t.Fatalf("disabling full-auto must drop bypassPermissions: %s", execs)
	}
	if !strings.Contains(execs, `"statusLine"`) {
		t.Fatalf("statusline must survive full-auto off: %s", execs)
	}
	if strings.Contains(execs, "approval_policy") {
		t.Fatalf("disabling full-auto must drop codex auto-approval: %s", execs)
	}
	got, err := m.Get("auto")
	if err != nil {
		t.Fatal(err)
	}
	if got.FullAuto {
		t.Fatal("full-auto flag did not persist")
	}
}

func TestCreateWithOptions(t *testing.T) {
	m, f := newTestManager(t)
	r, err := m.CreateWithOptions("locked", CreateOptions{
		MemoryGB:       4,
		CPUs:           2,
		Providers:      []string{"openai"},
		UsdLimit:       5,
		MaxConcurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.FullAuto {
		t.Fatal("options did not carry full-auto=false")
	}
	usd, conc := m.Limits("locked")
	if usd != 5 || conc != 2 {
		t.Fatalf("limits not applied: usd=%v conc=%v", usd, conc)
	}
	execs := strings.Join(flatten(f.callsNamed("exec")), " ")
	if strings.Contains(execs, "ANTHROPIC_BASE_URL") {
		t.Fatalf("openai-only space leaked anthropic env: %s", execs)
	}
	if !strings.Contains(execs, "OPENAI_API_KEY") {
		t.Fatalf("openai env missing: %s", execs)
	}
	if !strings.Contains(execs, "rm -f /home/space/.claude/settings.json") {
		t.Fatalf("anthropic-off space must remove managed claude settings: %s", execs)
	}
}

func TestClaudeSettingsJSON(t *testing.T) {
	full := claudeSettingsJSON(true, "claude-fable-5[1m]")
	for _, want := range []string{`"statusLine"`, "statusline-command.sh", `"model":"claude-fable-5[1m]"`, `"defaultMode":"bypassPermissions"`} {
		if !strings.Contains(full, want) {
			t.Fatalf("settings missing %q: %s", want, full)
		}
	}
	plain := claudeSettingsJSON(false, "")
	if strings.Contains(plain, "bypassPermissions") || strings.Contains(plain, `"model"`) {
		t.Fatalf("plain settings carry extras: %s", plain)
	}
	if !strings.Contains(plain, `"statusLine"`) {
		t.Fatalf("plain settings must keep the statusline: %s", plain)
	}
}

func TestTemplateStore(t *testing.T) {
	s := NewTemplateStore(filepath.Join(t.TempDir(), "templates.json"))
	saved, err := s.Save(Template{Name: "My Class", MemoryGB: 4, CPUs: 2, Providers: []string{"anthropic"}, UsdLimit: 10, FullAuto: true})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Name != "my-class" {
		t.Fatalf("template name not normalized: %q", saved.Name)
	}
	if _, err := s.Save(Template{Name: "!"}); err == nil {
		t.Fatal("junk template name accepted")
	}
	got, err := s.Get("my-class")
	if err != nil || got.MemoryGB != 4 || !got.FullAuto || len(got.Providers) != 1 {
		t.Fatalf("Get returned %+v, %v", got, err)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List returned %+v, %v", list, err)
	}
	if err := s.Delete("my-class"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("my-class"); err == nil {
		t.Fatal("deleting a missing template should fail")
	}
}

func TestTemplateFromSpaceCapturesSettings(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.CreateWithOptions("src", CreateOptions{MemoryGB: 8, CPUs: 6, Providers: []string{"openai", "xai"}, UsdLimit: 3, MaxConcurrency: 7, FullAuto: true}); err != nil {
		t.Fatal(err)
	}
	r, err := m.Get("src")
	if err != nil {
		t.Fatal(err)
	}
	usd, conc := m.Limits("src")
	tpl := TemplateFromSpace("classroom", r, usd, conc)
	opts := tpl.Options()
	if opts.MemoryGB != 8 || opts.CPUs != 6 || opts.UsdLimit != 3 || opts.MaxConcurrency != 7 || !opts.FullAuto {
		t.Fatalf("template lost settings: %+v", opts)
	}
	if len(opts.Providers) != 2 {
		t.Fatalf("template lost providers: %+v", opts.Providers)
	}
}

func TestSetHostIdentitySkipsKeygenNoise(t *testing.T) {
	r := &Space{}
	out := "ssh-keygen: generating new host keys: RSA ECDSA ED25519 \n" + vecPubKey + "\n"
	if err := setHostIdentity(r, out); err != nil {
		t.Fatal(err)
	}
	if r.HostKeyFP != vecFP {
		t.Fatalf("fingerprint mismatch: %s", r.HostKeyFP)
	}
	if err := setHostIdentity(&Space{}, "ssh-keygen: generating new host keys: RSA\n"); err == nil {
		t.Fatal("output without a key must fail")
	}
}
