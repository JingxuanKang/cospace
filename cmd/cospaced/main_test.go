package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JingxuanKang/cospace/internal/container"
	"github.com/JingxuanKang/cospace/internal/pairing"
	"github.com/JingxuanKang/cospace/internal/spaces"
	"github.com/JingxuanKang/cospace/internal/transport"
	"github.com/tailscale/tailcat"
)

func TestRunRejectsUnknownCommand(t *testing.T) {
	err := run([]string{"unknown"}, io.Discard)
	if err == nil || err.Error() != `unknown command "unknown"` {
		t.Fatalf("got %v, want unknown-command error", err)
	}
}

func TestRunRejectsMissingSpaceName(t *testing.T) {
	for _, args := range [][]string{
		{"space", "create"},
		{"space", "start"},
		{"space", "stop"},
		{"space", "delete"},
	} {
		err := run(args, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "missing name") {
			t.Errorf("run(%q) got %v, want missing-name error", args, err)
		}
	}
}

func TestRunInvitePrintsPasteLineFromPersistedIdentity(t *testing.T) {
	dataDir := t.TempDir()
	spaceDir := filepath.Join(dataDir, "spaces", "alpha")
	if err := os.MkdirAll(spaceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	spaceJSON, err := json.Marshal(spaces.Space{Name: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spaceDir, "space.json"), spaceJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	id := tailcat.NewPrivateKey()
	id.Public.RegionID = 7
	idJSON, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	identityDir := filepath.Join(dataDir, "transport", "alpha")
	if err := os.MkdirAll(identityDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(identityDir, "identity.json"), idJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := run([]string{"-data", dataDir, "invite", "alpha", "-ttl", "5m"}, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	// The invite prints both installer choices and the exact guest-facing pair
	// command derived from the persisted identity.
	wantPrefix := "cospace pair " + string(id.Public.ConnBlob()) + " "
	i := strings.Index(text, wantPrefix)
	if i < 0 {
		t.Fatalf("invite output missing pair line %q:\n%s", wantPrefix, text)
	}
	rest := text[i+len(wantPrefix):]
	code := strings.SplitN(rest, "\n", 2)[0]
	if !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`).MatchString(code) {
		t.Fatalf("invite code = %q", code)
	}
	if !strings.Contains(text, "expires in 5m0s") {
		t.Fatalf("expiry note missing:\n%s", text)
	}
	if !strings.Contains(text, "ssh alpha") {
		t.Fatalf("connect hint missing:\n%s", text)
	}
	if !strings.Contains(text, "install.ps1") || !strings.Contains(text, "install.sh") {
		t.Fatalf("platform installers missing:\n%s", text)
	}
}

func TestFormatSpaceList(t *testing.T) {
	created := time.Date(2026, time.August, 29, 9, 10, 11, 0, time.FixedZone("BST", 3600))
	statuses := []spaces.Status{
		{
			Space: spaces.Space{
				Name:      "alpha",
				MemoryGB:  2,
				CPUs:      4,
				CreatedAt: created,
				Members:   []spaces.Member{{Name: "Alice"}, {Name: "Bob"}},
			},
			State: "running",
			IP:    "192.168.64.4",
		},
		{
			Space: spaces.Space{Name: "beta", MemoryGB: 8, CPUs: 6},
			State: "stopped",
		},
	}

	var got bytes.Buffer
	if err := formatSpaceList(&got, statuses); err != nil {
		t.Fatal(err)
	}
	wantRows := [][]string{
		{"NAME", "STATE", "IP", "GUESTS", "MEM", "CPUS", "CREATED"},
		{"alpha", "running", "192.168.64.4", "2", "2GB", "4", "2026-08-29T08:10:11Z"},
		{"beta", "stopped", "-", "0", "8GB", "6", "-"},
	}
	lines := strings.Split(strings.TrimSpace(got.String()), "\n")
	if len(lines) != len(wantRows) {
		t.Fatalf("got %d rows, want %d:\n%s", len(lines), len(wantRows), got.String())
	}
	for i, line := range lines {
		if fields := strings.Fields(line); !equalStrings(fields, wantRows[i]) {
			t.Errorf("row %d fields = %q, want %q", i, fields, wantRows[i])
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type fakeSpaceOps struct {
	mu            sync.Mutex
	started       []string
	autoStarted   []string
	stopped       []string
	idleStopped   []string
	connectionErr error
	addedSpace    string
	addedName     string
	addedKey      string
	addErr        error
	checkErr      error
	checked       []string
	statuses      []spaces.Status
}

func (f *fakeSpaceOps) CanAddMember(space, name, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked = append(f.checked, space+"/"+name)
	return f.checkErr
}

func (f *fakeSpaceOps) Start(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, name)
	return nil
}

func (f *fakeSpaceOps) StartOnConnect(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.autoStarted = append(f.autoStarted, name)
	return nil
}

func (f *fakeSpaceOps) ConnectionAllowed(string) error {
	return f.connectionErr
}

func (f *fakeSpaceOps) Stop(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, name)
	return nil
}

func (f *fakeSpaceOps) StopIdle(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idleStopped = append(f.idleStopped, name)
	return nil
}

func (f *fakeSpaceOps) AddMember(space, name, key string) (*spaces.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addedSpace, f.addedName, f.addedKey = space, name, key
	if f.addErr != nil {
		return nil, f.addErr
	}
	return &spaces.Member{Fingerprint: "SHA256:member"}, nil
}

func (f *fakeSpaceOps) HostIdentity(string) (string, string, error) {
	return "ssh-ed25519 HOST", "SHA256:host", nil
}

func (f *fakeSpaceOps) List() ([]spaces.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]spaces.Status(nil), f.statuses...), nil
}

type fakeContainerRuntime struct {
	mu      sync.Mutex
	infos   []container.Info
	ips     []string
	ipCalls int
}

func (f *fakeContainerRuntime) List() ([]container.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]container.Info(nil), f.infos...), nil
}

func (f *fakeContainerRuntime) IP(string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ips) == 0 {
		return "", errors.New("not ready")
	}
	i := min(f.ipCalls, len(f.ips)-1)
	f.ipCalls++
	return f.ips[i], nil
}

func TestSponsorKeepsMixedStateAndSetsPerProvider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sponsor.json")
	if err := os.WriteFile(path, []byte(`{"anthropic":true,"openai":false,"xai":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newSponsor(path, map[string]string{
		"anthropic": filepath.Join(dir, "anthropic.json"),
		"openai":    filepath.Join(dir, "openai.json"),
		"xai":       filepath.Join(dir, "xai.json"),
	})
	// The host gate is per provider: a mixed state is valid and preserved.
	if !s.enabled("anthropic") || s.enabled("openai") || !s.enabled("xai") {
		t.Fatalf("mixed state not preserved: %v", s.on)
	}
	if err := s.Set("openai", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("nope", true); err == nil {
		t.Fatal("unknown provider accepted")
	}
	if !s.enabled("openai") {
		t.Fatal("per-provider set did not stick")
	}
	if err := s.SetAll(true); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"anthropic", "openai", "xai"} {
		if !s.enabled(provider) {
			t.Fatalf("master enable did not turn on %s", provider)
		}
	}
}

func TestSpaceBackendWakesAndResolvesIPFreshEveryDial(t *testing.T) {
	rm := &fakeSpaceOps{}
	runtime := &fakeContainerRuntime{
		infos: []container.Info{{Name: "alpha", State: "stopped"}},
		ips:   []string{"192.0.2.10", "192.0.2.11"},
	}
	var dialed []string
	b := &daemonSpaceBackend{
		spaces:     rm,
		containers: runtime,
		dial: func(_, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			left, right := net.Pipe()
			right.Close()
			return left, nil
		},
		sleep: func(time.Duration) {},
		wait:  time.Second,
	}
	for range 2 {
		conn, err := b.DialSSH("alpha")
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		// The second call represents a restarted space with a new address.
		runtime.mu.Lock()
		runtime.infos[0].State = "running"
		runtime.mu.Unlock()
	}
	if strings.Join(dialed, ",") != "192.0.2.10:22,192.0.2.11:22" {
		t.Fatalf("dialed = %v", dialed)
	}
	if len(rm.autoStarted) != 1 || rm.autoStarted[0] != "alpha" {
		t.Fatalf("auto-started = %v, want [alpha]", rm.autoStarted)
	}
}

func TestSpaceBackendRejectsConnectionWhileHostSleeping(t *testing.T) {
	rm := &fakeSpaceOps{connectionErr: errors.New("space alpha is sleeping until the Host wakes it")}
	runtime := &fakeContainerRuntime{
		infos: []container.Info{{Name: "alpha", State: "running"}},
		ips:   []string{"192.0.2.10"},
	}
	b := &daemonSpaceBackend{
		spaces: rm, containers: runtime,
		dial: func(_, _ string) (net.Conn, error) {
			t.Fatal("manual sleep reached network dial")
			return nil, nil
		},
	}
	if _, err := b.DialSSH("alpha"); err == nil || !strings.Contains(err.Error(), "Host wakes it") {
		t.Fatalf("manual sleep connection = %v", err)
	}
}

// A wrong code learns nothing (no membership check runs); a valid code is
// consumed only once the guest's name and key have been accepted, so a name
// collision does not burn the invite.
func TestSpaceBackendPairValidatesBeforeRedeeming(t *testing.T) {
	store := pairing.NewStore(filepath.Join(t.TempDir(), "invites.json"))
	code, err := store.Create("alpha", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rm := &fakeSpaceOps{checkErr: fmt.Errorf("%w: member \"alice\" already exists in alpha with a different key", spaces.ErrConflict)}
	b := &daemonSpaceBackend{spaces: rm, invites: store}
	if _, err := b.Pair("alpha", "WRONG-CODE", "alice", "key"); err == nil {
		t.Fatal("wrong code unexpectedly succeeded")
	}
	if len(rm.checked) != 0 || rm.addedSpace != "" {
		t.Fatalf("membership consulted before code validation: %v %q", rm.checked, rm.addedSpace)
	}
	_, err = b.Pair("alpha", code, "alice", "key")
	var pe *transport.PairError
	if !errors.As(err, &pe) || pe.Status != 409 || !strings.Contains(pe.Message, "different key") {
		t.Fatalf("name collision = %v, want a 409 PairError the guest can read", err)
	}
	if rm.addedSpace != "" {
		t.Fatal("rejected pairing reached AddMember")
	}
	if _, _, err := store.Lookup(code); err != nil {
		t.Fatalf("rejected pairing burned the code: %v", err)
	}
	// Now the guest retries with another name and the same code.
	rm.checkErr = nil
	if _, err := b.Pair("alpha", code, "alice-laptop", "key"); err != nil {
		t.Fatal(err)
	}
	if rm.addedSpace != "alpha" || rm.addedName != "alice-laptop" {
		t.Fatalf("accepted pairing did not reach AddMember: %q %q", rm.addedSpace, rm.addedName)
	}
	if _, err := store.Redeem(code); !errors.Is(err, pairing.ErrAlreadyUsed) {
		t.Fatalf("accepted pairing did not consume the code: %v", err)
	}
}

func TestSpaceBackendPairReturnsSpaceHostIdentity(t *testing.T) {
	store := pairing.NewStore(filepath.Join(t.TempDir(), "invites.json"))
	code, err := store.Create("alpha", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rm := &fakeSpaceOps{}
	b := &daemonSpaceBackend{spaces: rm, invites: store}
	got, err := b.Pair("alpha", code, "alice", "key")
	if err != nil {
		t.Fatal(err)
	}
	if got.HostKey != "ssh-ed25519 HOST" || got.Fingerprint != "SHA256:host" {
		t.Fatalf("pair returned member identity instead of space host identity: %+v", got)
	}
}

type fakeActivity struct{ times map[string]time.Time }

func (f fakeActivity) LastActivity(space string) time.Time { return f.times[space] }

func TestReapIdleRunningSpaces(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rm := &fakeSpaceOps{statuses: []spaces.Status{
		{Space: spaces.Space{Name: "idle"}, State: "running"},
		{Space: spaces.Space{Name: "active"}, State: "running"},
		{Space: spaces.Space{Name: "already-stopped"}, State: "stopped"},
		{Space: spaces.Space{Name: "never-seen"}, State: "running"},
	}}
	activity := fakeActivity{times: map[string]time.Time{
		"idle":            now.Add(-31 * time.Minute),
		"active":          now.Add(-29 * time.Minute),
		"already-stopped": now.Add(-time.Hour),
	}}
	if err := reapIdleSpaces(rm, activity, 30*time.Minute, now); err != nil {
		t.Fatal(err)
	}
	if len(rm.idleStopped) != 1 || rm.idleStopped[0] != "idle" {
		t.Fatalf("idle-stopped = %v, want [idle]", rm.idleStopped)
	}
}

var _ transport.SpaceBackend = (*daemonSpaceBackend)(nil)
