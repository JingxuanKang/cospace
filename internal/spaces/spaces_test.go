package spaces

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/JingxuanKang/cospace/internal/container"
)

const (
	vecPubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPojEOfYY/ehOeqXAffycIiI3cPQA71LbOnLeD8hqmDK alice@example"
	vecFP     = "SHA256:Tf4bv6JoEnVnFJL1Ko9h583AYwxvkUZHbyEYWCrbYSc"
)

// fakeRunner stands in for the Apple container CLI. It tracks the containers
// a test creates (run / start / stop / delete) and answers `list` from that
// state unless a test pins out["list"] explicitly.
type fakeRunner struct {
	calls [][]string
	out   map[string]string
	state map[string]string // container name -> running | stopped
}

func (f *fakeRunner) Run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if f.state == nil {
		f.state = map[string]string{}
	}
	switch args[0] {
	case "run":
		for i, a := range args {
			if a == "--name" && i+1 < len(args) {
				f.state[args[i+1]] = "running"
			}
		}
	case "start":
		f.state[args[len(args)-1]] = "running"
	case "stop":
		f.state[args[len(args)-1]] = "stopped"
	case "delete":
		delete(f.state, args[len(args)-1])
	case "list":
		if pinned, ok := f.out["list"]; ok {
			return pinned, nil
		}
		var rows []string
		for name, st := range f.state {
			rows = append(rows, `{"configuration":{"id":"`+name+`"},"status":{"state":"`+st+`","networks":[{"ipv4Address":"192.168.64.9/24"}]}}`)
		}
		return "[" + strings.Join(rows, ",") + "]", nil
	}
	if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "cat /etc/ssh/ssh_host_ed25519_key.pub") {
		return vecPubKey, nil
	}
	return f.out[args[0]], nil
}

// authorizedKeys decodes the authorized_keys payload the runtime sync shipped
// in its most recent exec script (keys travel base64-encoded, never raw).
func authorizedKeys(t *testing.T, calls [][]string) string {
	t.Helper()
	re := regexp.MustCompile(`printf '%s' '([A-Za-z0-9+/=]*)' \| base64 -d > /home/space/.ssh/authorized_keys`)
	var last string
	for _, c := range calls {
		if m := re.FindStringSubmatch(strings.Join(c, " ")); m != nil {
			last = m[1]
		}
	}
	if last == "" {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(last)
	if err != nil {
		t.Fatalf("authorized_keys payload is not base64: %v", err)
	}
	return string(b)
}

func (f *fakeRunner) callsNamed(verb string) [][]string {
	var got [][]string
	for _, c := range f.calls {
		if c[0] == verb {
			got = append(got, c)
		}
	}
	return got
}

func newTestManager(t *testing.T) (*Manager, *fakeRunner) {
	t.Helper()
	f := &fakeRunner{out: map[string]string{}}
	m := &Manager{
		Dir:        t.TempDir(),
		C:          container.Client{R: f},
		Image:      "cospace-base",
		GatewayURL: "http://192.168.64.1:18930",
	}
	return m, f
}

func TestCreateSpace(t *testing.T) {
	m, f := newTestManager(t)
	r, err := m.Create("acme-web", 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.Token, "cs_") || len(r.Token) < 20 {
		t.Fatalf("weak token: %q", r.Token)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "spaces", "acme-web", "workspace")); err != nil {
		t.Fatalf("workspace dir missing: %v", err)
	}
	runs := f.callsNamed("run")
	if len(runs) != 1 {
		t.Fatalf("want 1 container run, got %v", f.calls)
	}
	args := strings.Join(runs[0], " ")
	for _, want := range []string{"--name acme-web", "--memory 2g", fmt.Sprintf("--cpus %d", min(4, runtime.NumCPU())), "/workspace", "cospace-base"} {
		if !strings.Contains(args, want) {
			t.Fatalf("run args missing %q: %s", want, args)
		}
	}
	// runtime sync must inject the per-space fake token and gateway URLs, never a real credential
	execs := strings.Join(flatten(f.callsNamed("exec")), " ")
	for _, want := range []string{r.Token, "http://192.168.64.1:18930/anthropic", "http://192.168.64.1:18930/openai", "OPENAI_API_KEY", "model_provider = \"cospace\"", "supports_websockets = false", "GROK_MODELS_BASE_URL=http://192.168.64.1:18930/xai", "XAI_API_KEY"} {
		if !strings.Contains(execs, want) {
			t.Fatalf("runtime sync missing %q in: %s", want, execs)
		}
	}
	// reload from disk
	m2 := &Manager{Dir: m.Dir, C: m.C, Image: m.Image, GatewayURL: m.GatewayURL}
	got, err := m2.Get("acme-web")
	if err != nil || got.Token != r.Token {
		t.Fatalf("persisted space mismatch: %+v err=%v", got, err)
	}
}

func TestCreateRegeneratesHostKeys(t *testing.T) {
	m, f := newTestManager(t)
	r, err := m.Create("acme", 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	execs := strings.Join(flatten(f.callsNamed("exec")), "\n")
	// image-baked host keys are shared across every space built from it;
	// creation must replace them and make sshd pick the new ones up
	for _, want := range []string{"rm -f /etc/ssh/ssh_host_", "ssh-keygen -A", "kill -HUP 1"} {
		if !strings.Contains(execs, want) {
			t.Fatalf("host key regen missing %q in:\n%s", want, execs)
		}
	}
	if r.HostKey != strings.TrimSuffix(vecPubKey, " alice@example") || r.HostKeyFP != vecFP {
		t.Fatalf("fingerprint not captured: %q", r.HostKeyFP)
	}
	// persisted
	got, _ := m.Get("acme")
	if got.HostKey != strings.TrimSuffix(vecPubKey, " alice@example") || got.HostKeyFP != vecFP {
		t.Fatalf("fingerprint not persisted: %q", got.HostKeyFP)
	}
}

func TestHostIdentityBackfillsLegacySpace(t *testing.T) {
	m, _ := newTestManager(t)
	r, err := m.Create("acme", 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	r.HostKey = ""
	if err := m.save(r); err != nil {
		t.Fatal(err)
	}
	key, fp, err := m.HostIdentity("acme")
	if err != nil {
		t.Fatal(err)
	}
	if key != strings.TrimSuffix(vecPubKey, " alice@example") || fp != vecFP {
		t.Fatalf("backfilled identity = %q %q", key, fp)
	}
}

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"Shopfront":           "shopfront",
		"My Project":          "my-project",
		"acme_web":            "acme-web",
		"  Trim Me  ":         "trim-me",
		"a//b..c":             "a-b-c",
		"ALLCAPS":             "allcaps",
		"already-ok":          "already-ok",
		"--leading-trailing-": "leading-trailing",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
	// unrecoverable input still errors at Create
	if Normalize("!!!") != "" {
		t.Errorf("Normalize(%q) should be empty", "!!!")
	}
}

func TestCreateNormalizesName(t *testing.T) {
	m, _ := newTestManager(t)
	r, err := m.Create("Shopfront", 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "shopfront" {
		t.Fatalf("Create did not normalize: %q", r.Name)
	}
	if _, err := m.Get("shopfront"); err != nil {
		t.Fatalf("normalized space not retrievable: %v", err)
	}
}

func TestCreateValidation(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Create("!!!", 2, 4); err == nil {
		t.Fatal("want invalid-name error for unnormalizable input")
	}
	if _, err := m.Create("acme", 2, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("acme", 2, 4); err == nil {
		t.Fatal("want duplicate error")
	}
}

func TestSetProvidersScopesSpace(t *testing.T) {
	m, f := newTestManager(t)
	if _, err := m.Create("acme", 2, 4); err != nil {
		t.Fatal(err)
	}
	f.out["list"] = `[{"configuration":{"id":"acme"},"status":{"state":"running"}}]`
	f.calls = nil
	// openai-only space: no anthropic env, but codex config present
	if err := m.SetProviders("acme", []string{"openai"}); err != nil {
		t.Fatal(err)
	}
	execs := strings.Join(flatten(f.callsNamed("exec")), " ")
	if strings.Contains(execs, "ANTHROPIC_BASE_URL") {
		t.Fatal("openai-only space should not get anthropic env")
	}
	if !strings.Contains(execs, "OPENAI_BASE_URL") || !strings.Contains(execs, "model_provider = \"cospace\"") {
		t.Fatal("openai-only space missing openai env / codex config")
	}
	if r, _ := m.Get("acme"); len(r.Providers) != 1 || r.Providers[0] != "openai" {
		t.Fatalf("providers not persisted: %+v", r)
	}
	// anthropic-only space: no openai/codex
	f.calls = nil
	if err := m.SetProviders("acme", []string{"anthropic"}); err != nil {
		t.Fatal(err)
	}
	execs = strings.Join(flatten(f.callsNamed("exec")), " ")
	if strings.Contains(execs, "OPENAI_BASE_URL") || strings.Contains(execs, "model_provider") {
		t.Fatal("anthropic-only space should not get openai env / codex config")
	}
	if err := m.SetProviders("acme", []string{"bogus"}); err == nil {
		t.Fatal("want error for unknown provider")
	}
	// Explicitly empty means every tool is off, not the legacy default of all.
	f.calls = nil
	if err := m.SetProviders("acme", []string{}); err != nil {
		t.Fatal(err)
	}
	execs = strings.Join(flatten(f.callsNamed("exec")), " ")
	for _, marker := range []string{"ANTHROPIC_BASE_URL", "OPENAI_BASE_URL", "GROK_MODELS_BASE_URL", "model_provider"} {
		if strings.Contains(execs, marker) {
			t.Fatalf("all-off space runtime still contains %s", marker)
		}
	}
	r, err := m.Get("acme")
	if err != nil || r.Providers == nil || len(r.Providers) != 0 {
		t.Fatalf("explicit all-off providers not persisted: space=%+v err=%v", r, err)
	}
	if m.ProviderEnabled("acme", "anthropic") || m.ProviderEnabled("acme", "openai") || m.ProviderEnabled("acme", "xai") {
		t.Fatal("all-off space must be denied for every provider")
	}
}

func TestMembersLifecycle(t *testing.T) {
	m, f := newTestManager(t)
	if _, err := m.Create("acme", 2, 4); err != nil {
		t.Fatal(err)
	}
	mem, err := m.AddMember("acme", "Alice", vecPubKey)
	if err != nil {
		t.Fatal(err)
	}
	if mem.Fingerprint != vecFP {
		t.Fatalf("fingerprint got %q want %q", mem.Fingerprint, vecFP)
	}
	// key must be synced into the container
	if !strings.Contains(authorizedKeys(t, f.callsNamed("exec")), "AAAAC3NzaC1lZDI1NTE5AAAAIPojEOfYY") {
		t.Fatal("authorized key not synced to container")
	}
	// re-pairing the same guest (same name + same key) is idempotent success
	if _, err := m.AddMember("acme", "Alice", vecPubKey); err != nil {
		t.Fatalf("idempotent re-pair should succeed, got %v", err)
	}
	if r, _ := m.Get("acme"); len(r.Members) != 1 {
		t.Fatalf("re-pair duplicated the member: %+v", r.Members)
	}
	// revoke removes key from later syncs
	f.calls = nil
	if err := m.RevokeMember("acme", "Alice"); err != nil {
		t.Fatal(err)
	}
	execs := flatten(f.callsNamed("exec"))
	if strings.Contains(authorizedKeys(t, f.callsNamed("exec")), "AAAAC3NzaC1lZDI1NTE5AAAAIPojEOfYY") {
		t.Fatal("revoked key still present in sync")
	}
	// revocation must drop active ssh sessions immediately, not just future ones
	if !strings.Contains(strings.Join(execs, " "), "pkill") {
		t.Fatal("revoke did not kill active ssh sessions")
	}
	r, _ := m.Get("acme")
	if len(r.Members) != 0 {
		t.Fatalf("member not removed: %+v", r.Members)
	}
}

func TestAddMemberRejectsGarbageKey(t *testing.T) {
	m, _ := newTestManager(t)
	m.Create("acme", 2, 4)
	if _, err := m.AddMember("acme", "Eve", "not a key"); err == nil {
		t.Fatal("want invalid key error")
	}
}

func TestLookupToken(t *testing.T) {
	m, _ := newTestManager(t)
	r, _ := m.Create("acme", 2, 4)
	if name, ok := m.LookupToken(r.Token); !ok || name != "acme" {
		t.Fatalf("lookup failed: %q %v", name, ok)
	}
	if _, ok := m.LookupToken("cs_wrong"); ok {
		t.Fatal("bogus token accepted")
	}
}

func TestStartSyncsRuntime(t *testing.T) {
	m, f := newTestManager(t)
	m.Create("acme", 2, 4)
	m.AddMember("acme", "Alice", vecPubKey)
	if err := m.StopIdle("acme"); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := m.Start("acme"); err != nil {
		t.Fatal(err)
	}
	if len(f.callsNamed("start")) != 1 {
		t.Fatalf("want container start, got %v", f.calls)
	}
	if !strings.Contains(authorizedKeys(t, f.callsNamed("exec")), "AAAAC3NzaC1lZDI1NTE5AAAAIPojEOfYY") {
		t.Fatal("start must re-sync keys (IPs and files can change across restarts)")
	}
}

func TestManualSleepBlocksConnectionWakeUntilHostStarts(t *testing.T) {
	m, f := newTestManager(t)
	if _, err := m.Create("acme", 2, 4); err != nil {
		t.Fatal(err)
	}

	f.calls = nil
	if err := m.Stop("acme"); err != nil {
		t.Fatal(err)
	}
	space, err := m.Get("acme")
	if err != nil || !space.ManualSleep {
		t.Fatalf("manual sleep not persisted: space=%+v err=%v", space, err)
	}
	if len(f.callsNamed("stop")) != 1 {
		t.Fatalf("manual sleep did not stop container: %v", f.calls)
	}

	f.calls = nil
	if err := m.StartOnConnect("acme"); err == nil || !strings.Contains(err.Error(), "until the Host wakes it") {
		t.Fatalf("connection wake during manual sleep = %v", err)
	}
	if len(f.callsNamed("start")) != 0 {
		t.Fatalf("connection wake started manually sleeping space: %v", f.calls)
	}

	if err := m.Start("acme"); err != nil {
		t.Fatal(err)
	}
	space, err = m.Get("acme")
	if err != nil || space.ManualSleep {
		t.Fatalf("Host wake did not clear manual sleep: space=%+v err=%v", space, err)
	}

	f.calls = nil
	if err := m.StopIdle("acme"); err != nil {
		t.Fatal(err)
	}
	space, err = m.Get("acme")
	if err != nil || space.ManualSleep {
		t.Fatalf("idle stop changed manual sleep: space=%+v err=%v", space, err)
	}
	if err := m.StartOnConnect("acme"); err != nil {
		t.Fatalf("idle-stopped space did not wake on connection: %v", err)
	}
}

func TestDeletePurges(t *testing.T) {
	m, f := newTestManager(t)
	m.Create("acme", 2, 4)
	if err := m.Delete("acme"); err != nil {
		t.Fatal(err)
	}
	if len(f.callsNamed("delete")) != 1 {
		t.Fatalf("container delete not called: %v", f.calls)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "spaces", "acme")); !os.IsNotExist(err) {
		t.Fatal("space dir not purged")
	}
	if _, err := m.Get("acme"); err == nil {
		t.Fatal("space still gettable after delete")
	}
}

func flatten(calls [][]string) []string {
	var out []string
	for _, c := range calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func TestCreateClampsCPUsToHost(t *testing.T) {
	m, _ := newTestManager(t)
	r, err := m.Create("wide", 2, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if r.CPUs != runtime.NumCPU() {
		t.Fatalf("CPUs = %d, want host count %d", r.CPUs, runtime.NumCPU())
	}
}
