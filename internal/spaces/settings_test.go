package spaces

import (
	"errors"
	"github.com/JingxuanKang/cospace/internal/container"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutionSettingsPersistAndTemplate(t *testing.T) {
	m, f := newTestManager(t)
	r, err := m.Create("experiment", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if r.EffectiveCodexRoute() != "official" || r.EffectiveNetworkMode() != "open" {
		t.Fatal("legacy defaults changed")
	}
	if err = m.SetCodex(r.Name, "sub2api-i", "gpt-6-astra"); err != nil {
		t.Fatal(err)
	}
	f.out["list"] = `[{"configuration":{"id":"experiment"},"status":{"state":"running"}}]`
	if err = m.SetNetwork(r.Name, "gateway-only"); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(r.Name)
	opts := TemplateFromSpace("copy", got, 0, 1).Options()
	if opts.CodexRoute != "sub2api-i" || opts.CodexModel != "gpt-6-astra" || opts.NetworkMode != "gateway-only" {
		t.Fatal(opts)
	}
	route, internet, err := m.ExecutionPolicy(r.Name)
	if err != nil || route != "sub2api-i" || internet {
		t.Fatal(route, internet, err)
	}
	if err = m.SetCodex(r.Name, "unknown", "gpt-6-astra"); err == nil {
		t.Fatal("accepted invalid route")
	}
	if err = m.SetNetwork(r.Name, "unknown"); err == nil {
		t.Fatal("accepted invalid network")
	}
	got.NetworkControlVersion = 0
	m.save(got)
	if err = m.SetNetwork(r.Name, "gateway-only"); err == nil {
		t.Fatal("legacy accepted unsupported isolation")
	}
}

type failNetworkRunner struct{ base *fakeRunner }

func (f failNetworkRunner) Run(args ...string) (string, error) {
	if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "nft -f") {
		return "", errors.New("firewall unavailable")
	}
	return f.base.Run(args...)
}
func TestFailedNetworkChangeDoesNotClaimOffline(t *testing.T) {
	m, f := newTestManager(t)
	m.Create("experiment", 1, 2)
	f.out["list"] = `[{"configuration":{"id":"experiment"},"status":{"state":"running"}}]`
	m.C = container.Client{R: failNetworkRunner{f}}
	if err := m.SetNetwork("experiment", "gateway-only"); err == nil {
		t.Fatal("failure hidden")
	}
	got, _ := m.Get("experiment")
	if got.EffectiveNetworkMode() != "open" {
		t.Fatal("saved unapplied policy")
	}
	got.NetworkMode = "gateway-only"
	m.save(got)
	if err := m.Start("experiment"); err == nil {
		t.Fatal("failed closed start should fail")
	}
	if len(f.callsNamed("stop")) == 0 {
		t.Fatal("failed offline start left VM running")
	}
}

func TestSleepingSpaceSettingsDeferred(t *testing.T) {
	m, f := newTestManager(t)
	m.Create("experiment", 1, 2)
	if err := m.StopIdle("experiment"); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := m.SetFullAuto("experiment", false); err != nil {
		t.Fatal(err)
	}
	if err := m.SetProviders("experiment", []string{"openai"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetCodex("experiment", "sub2api-i", "gpt-6-astra"); err != nil {
		t.Fatal(err)
	}
	if len(f.callsNamed("exec")) != 0 {
		t.Fatal("tried to exec in sleeping VM")
	}
	if err := m.SetNetwork("experiment", "gateway-only"); err == nil {
		t.Fatal("must wake before changing firewall")
	}
	r, _ := m.Get("experiment")
	if r.FullAuto || len(r.Providers) != 1 || r.CodexModel != "gpt-6-astra" || r.EffectiveNetworkMode() != "open" {
		t.Fatal("saved settings incorrect")
	}
}

type failConfigRunner struct{ base *fakeRunner }

func (f failConfigRunner) Run(args ...string) (string, error) {
	if args[0] == "exec" && strings.Contains(strings.Join(args, " "), "/etc/profile.d/cospace.sh") {
		return "", errors.New("config write failed")
	}
	return f.base.Run(args...)
}
func TestFailedLiveSettingsRetainStateAndStop(t *testing.T) {
	m, f := newTestManager(t)
	m.Create("experiment", 1, 2)
	f.out["list"] = `[{"configuration":{"id":"experiment"},"status":{"state":"running"}}]`
	m.C = container.Client{R: failConfigRunner{f}}
	if err := m.SetFullAuto("experiment", false); err == nil {
		t.Fatal("failure hidden")
	}
	r, _ := m.Get("experiment")
	if !r.FullAuto {
		t.Fatal("unapplied setting saved")
	}
	if len(f.callsNamed("stop")) != 1 {
		t.Fatal("failed rollback must stop VM")
	}
}

func TestFailedLegacySnapshotKeepsOriginal(t *testing.T) {
	m, f := newTestManager(t)
	r, _ := m.Create("experiment", 1, 2)
	r.NetworkControlVersion = 0
	m.save(r)
	f.out["list"] = `[{"configuration":{"id":"experiment"},"status":{"state":"running"}}]`
	f.calls = nil
	if err := m.UpgradeNetwork("experiment"); err == nil {
		t.Fatal("empty snapshot accepted")
	}
	if len(f.callsNamed("delete")) != 0 || len(f.callsNamed("start")) != 1 {
		t.Fatal("original not preserved/restarted")
	}
	running, errText := m.NetworkUpgradeState("experiment")
	if running || errText == "" {
		t.Fatal("upgrade status missing")
	}
}

func TestFailedOfflineCreationStopsVM(t *testing.T) {
	m, f := newTestManager(t)
	m.C = container.Client{R: failNetworkRunner{f}}
	if _, err := m.CreateWithOptions("experiment", CreateOptions{NetworkMode: "gateway-only"}); err == nil {
		t.Fatal("failure hidden")
	}
	// An incomplete space is torn down completely: a leftover VM would adopt
	// the image's shared host key on its next Start.
	if len(f.callsNamed("delete")) != 1 {
		t.Fatal("incomplete offline VM left behind")
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "spaces", "experiment")); !os.IsNotExist(err) {
		t.Fatal("incomplete space dir left behind")
	}
}
