package spaces

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A name that is not a plain space name must never reach the filesystem:
// the ServeMux decodes %2F into the path value, and <Dir>/spaces/<x>/workspace
// is a guest-writable mount.
func TestTraversalNamesAreRejected(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Create("acme", 2, 4); err != nil {
		t.Fatal(err)
	}
	// Plant a record under the guest-writable workspace mount that names a real space.
	evil := filepath.Join(m.Dir, "spaces", "acme", "workspace", "evil")
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evil, "space.json"), []byte(`{"name":"acme","token":"cs_x","network_mode":"open","members":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"acme/workspace/evil", "../acme", "acme/..", "ACME", "", "a", strings.Repeat("a", 40)} {
		if _, err := m.Get(name); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Get(%q) = %v, want ErrInvalid", name, err)
		}
		if err := m.SetFullAuto(name, false); !errors.Is(err, ErrInvalid) {
			t.Fatalf("SetFullAuto(%q) = %v, want ErrInvalid", name, err)
		}
		if err := m.Delete(name); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Delete(%q) = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "spaces", "acme", "space.json")); err != nil {
		t.Fatalf("real record disturbed: %v", err)
	}
}

// A record that does not name itself is corrupt, not authoritative.
func TestRecordMustNameItself(t *testing.T) {
	m, _ := newTestManager(t)
	r, _ := m.Create("acme", 2, 4)
	r.Name = "other"
	b := []byte(`{"name":"other","token":"` + r.Token + `"}`)
	if err := os.WriteFile(filepath.Join(m.Dir, "spaces", "acme", "space.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get("acme"); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("mismatched record accepted: %v", err)
	}
	if _, ok := m.LookupToken(r.Token); ok {
		t.Fatal("token from a mismatched record accepted")
	}
}

// Guest-supplied keys are canonicalized: no second line, no options, so a
// pairing request can never smuggle shell into the root-executed sync script
// or sshd directives into authorized_keys.
func TestPublicKeyCanonicalization(t *testing.T) {
	m, f := newTestManager(t)
	m.Create("acme", 2, 4)
	bad := []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPojEOfYY/ehOeqXAffycIiI3cPQA71LbOnLeD8hqmDK x\nGREOF\nnft flush ruleset\ncat >/dev/null <<'GREOF'",
		"command=\"/bin/sh\" " + vecPubKey,
		"environment=\"SPACE_MEMBER=root\" " + vecPubKey,
		"ssh-ed25519 not-base64",
		"",
	}
	for _, key := range bad {
		if _, err := m.AddMember("acme", "eve", key); !errors.Is(err, ErrInvalid) {
			t.Fatalf("AddMember accepted %q: %v", key, err)
		}
	}
	mem, err := m.AddMember("acme", "alice", "  "+vecPubKey+"   ")
	if err != nil {
		t.Fatal(err)
	}
	if mem.PubKey != vecPubKey || mem.Fingerprint != vecFP {
		t.Fatalf("canonical key = %q fp = %q", mem.PubKey, mem.Fingerprint)
	}
	if got := authorizedKeys(t, f.callsNamed("exec")); got != vecPubKey+"\n" {
		t.Fatalf("authorized_keys = %q", got)
	}
	for _, name := range []string{"", "bad name", "../x", "a/b", strings.Repeat("n", 33), "\n"} {
		if _, err := m.AddMember("acme", name, vecPubKey); !errors.Is(err, ErrInvalid) {
			t.Fatalf("member name %q accepted: %v", name, err)
		}
	}
}

// A space whose VM vanished outside CoSpace (runtime reset, manual `container
// delete`) must still be deletable, and must report a clear error on Start
// instead of a raw runtime failure.
func TestMissingVMIsDeletableAndStartIsClear(t *testing.T) {
	m, f := newTestManager(t)
	m.Create("acme", 2, 4)
	delete(f.state, "acme") // the VM is gone; the record stays
	sts, _ := m.List()
	if len(sts) != 1 || sts[0].State != "missing" {
		t.Fatalf("list = %+v", sts)
	}
	if err := m.Start("acme"); !errors.Is(err, ErrVMMissing) {
		t.Fatalf("Start on missing VM = %v", err)
	}
	if err := m.Stop("acme"); err != nil {
		t.Fatalf("Stop on missing VM = %v", err)
	}
	f.out["delete"] = "" // the runtime says not found; the fake just records the call
	if err := m.Delete("acme"); err != nil {
		t.Fatalf("Delete on missing VM = %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "spaces", "acme")); !os.IsNotExist(err) {
		t.Fatal("space dir not purged")
	}
}

// A gateway-only space that fail-closes records why and refuses
// wake-on-connect until the Host starts it explicitly.
func TestFailClosedBlocksWakeOnConnect(t *testing.T) {
	m, f := newTestManager(t)
	r, _ := m.Create("experiment", 1, 2)
	r.NetworkMode = "gateway-only"
	m.save(r)
	m.StopIdle("experiment")
	m.C.R = failNetworkRunner{f}
	if err := m.StartOnConnect("experiment"); err == nil {
		t.Fatal("failed start reported success")
	}
	got, _ := m.Get("experiment")
	if got.WakeBlocked == "" {
		t.Fatal("fail-closed stop did not record a wake block")
	}
	f.calls = nil
	if err := m.StartOnConnect("experiment"); !errors.Is(err, ErrConflict) || len(f.callsNamed("start")) != 0 {
		t.Fatalf("wake-on-connect retried a blocked space: err=%v calls=%v", err, f.calls)
	}
	m.C.R = f
	if err := m.Start("experiment"); err != nil {
		t.Fatal(err)
	}
	got, _ = m.Get("experiment")
	if got.WakeBlocked != "" {
		t.Fatal("Host start did not clear the wake block")
	}
}

// Deleting a space releases its transport node through the hook.
func TestDeleteRunsHook(t *testing.T) {
	m, _ := newTestManager(t)
	var dropped string
	m.OnDeleted = func(name string) { dropped = name }
	m.Create("acme", 2, 4)
	if err := m.Delete("acme"); err != nil {
		t.Fatal(err)
	}
	if dropped != "acme" {
		t.Fatalf("hook got %q", dropped)
	}
}
