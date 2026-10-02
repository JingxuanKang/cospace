package container

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls [][]string
	out   map[string]string // keyed by first arg
	err   error
}

func (f *fakeRunner) Run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if f.err != nil {
		return "", f.err
	}
	return f.out[args[0]], nil
}

func TestRunDetachedBuildsArgs(t *testing.T) {
	f := &fakeRunner{}
	c := Client{R: f}
	err := c.RunDetached(RunSpec{
		Name: "acme", Image: "cospace-base", MemoryGB: 2, CPUs: 4,
		Mounts: []Mount{{Host: "/data/acme/workspace", Guest: "/workspace"}},
		Env:    map[string]string{"ANTHROPIC_AUTH_TOKEN": "fake"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "-d", "--name", "acme", "--cap-add", "CAP_NET_ADMIN", "--memory", "2g", "--cpus", "4",
		"-v", "/data/acme/workspace:/workspace",
		"-e", "ANTHROPIC_AUTH_TOKEN=fake", "cospace-base"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("args\n got %v\nwant %v", f.calls[0], want)
	}
}

func TestListParsesStateAndIP(t *testing.T) {
	// fixture mirrors real `container list --all --format json` output (verified 2026-08-29)
	f := &fakeRunner{out: map[string]string{"list": `[
	  {"status":{"state":"running","networks":[{"hostname":"acme","ipv4Address":"192.168.64.4/24","ipv4Gateway":"192.168.64.1"}]},"configuration":{"id":"acme"}},
	  {"status":{"state":"stopped","networks":[]},"configuration":{"id":"thesis"}}
	]`}}
	c := Client{R: f}
	infos, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []Info{{Name: "acme", State: "running", IP: "192.168.64.4"}, {Name: "thesis", State: "stopped"}}
	if !reflect.DeepEqual(infos, want) {
		t.Fatalf("got %+v want %+v", infos, want)
	}
	if got := f.calls[0]; !reflect.DeepEqual(got, []string{"list", "--all", "--format", "json"}) {
		t.Fatalf("list args: %v", got)
	}
}

func TestIPRefusesStoppedAndUnknown(t *testing.T) {
	f := &fakeRunner{out: map[string]string{"list": `[
	  {"status":{"state":"stopped","networks":[]},"configuration":{"id":"acme"}}
	]`}}
	c := Client{R: f}
	if _, err := c.IP("acme"); err == nil || !strings.Contains(err.Error(), "no address") {
		t.Fatalf("want no-address error, got %v", err)
	}
	if _, err := c.IP("ghost"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not-found error, got %v", err)
	}
}

func TestRunErrorsPropagate(t *testing.T) {
	f := &fakeRunner{err: errors.New("boom")}
	c := Client{R: f}
	if err := c.Stop("acme"); err == nil {
		t.Fatal("want error")
	}
}

type inputRunner struct {
	fakeRunner
	input string
	args  []string
}

func (f *inputRunner) RunInput(input string, args ...string) (string, error) {
	f.input = input
	f.args = args
	return "ok", nil
}
func TestLargeScriptsUseStdin(t *testing.T) {
	f := &inputRunner{}
	c := Client{R: f}
	script := strings.Repeat("# config\n", 20000)
	if _, err := c.Exec("acme", script); err != nil {
		t.Fatal(err)
	}
	if f.input != script || !reflect.DeepEqual(f.args, []string{"exec", "-i", "acme", "sh"}) {
		t.Fatal("large script entered argv")
	}
}
