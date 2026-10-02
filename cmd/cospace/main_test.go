package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPojEOfYY/ehOeqXAffycIiI3cPQA71LbOnLeD8hqmDK space"
	testHostFP  = "SHA256:Tf4bv6JoEnVnFJL1Ko9h583AYwxvkUZHbyEYWCrbYSc"
)

func TestRewriteSSHConfigFresh(t *testing.T) {
	hostAlias := managedHostKeyAlias("alpha")
	got, err := rewriteSSHConfig(nil, "alpha", "tc-address", "/Users/g/Downloads/cospace", "/home/g/.ssh/id_ed25519", "/home/g/.ssh/cospace_known_hosts", hostAlias, true)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`# >>> cospace:alpha >>>
Host alpha
  User space
  IdentityFile /home/g/.ssh/id_ed25519
  IdentitiesOnly yes
  HostKeyAlias %s
  UserKnownHostsFile /home/g/.ssh/cospace_known_hosts
  StrictHostKeyChecking yes
  HostKeyAlgorithms ssh-ed25519
  ControlMaster auto
  ControlPath ~/.ssh/cospace-%%C.sock
  ControlPersist 10m
  ProxyCommand /Users/g/Downloads/cospace connect tc-address 22
  ExitOnForwardFailure no
  LocalForward 3000 localhost:3000
  LocalForward 5173 localhost:5173
  LocalForward 8000 localhost:8000
  LocalForward 8080 localhost:8080
  LocalForward 8888 localhost:8888
# <<< cospace:alpha <<<
`, hostAlias)
	if string(got) != want {
		t.Fatalf("config =\n%s\nwant =\n%s", got, want)
	}
}

func TestRewriteSSHConfigPathHandling(t *testing.T) {
	// empty path falls back to bare "cospace"
	got, _ := rewriteSSHConfig(nil, "a", "addr", "", "", "/tmp/known", managedHostKeyAlias("a"), false)
	if !strings.Contains(string(got), "ProxyCommand cospace connect") {
		t.Fatalf("empty path should fall back to cospace:\n%s", got)
	}
	// a path with spaces gets quoted
	got, _ = rewriteSSHConfig(nil, "a", "addr", "/Apps/My Tools/cospace", "", "/tmp/known", managedHostKeyAlias("a"), false)
	if !strings.Contains(string(got), `ProxyCommand "/Apps/My Tools/cospace" connect`) {
		t.Fatalf("spaced path should be quoted:\n%s", got)
	}
}

func TestSSHConfigPathWindows(t *testing.T) {
	got := sshConfigPath(`C:\Users\Alice Example\AppData\Local\Programs\CoSpace\bin\cospace.exe`, "windows")
	want := `C:/Users/Alice Example/AppData/Local/Programs/CoSpace/bin/cospace.exe`
	if got != want {
		t.Fatalf("sshConfigPath() = %q, want %q", got, want)
	}
	if got := sshConfigPath(`/Users/alice/cospace`, "darwin"); got != `/Users/alice/cospace` {
		t.Fatalf("non-Windows path changed: %q", got)
	}
}

func TestRewriteSSHConfigReplacesSameAlias(t *testing.T) {
	in := `Host github.com
  User git

# >>> cospace:alpha >>>
Host alpha
  User old
  ProxyCommand cospace connect old-address 22
# <<< cospace:alpha <<<

Host after
  User somebody
`
	got, err := rewriteSSHConfig([]byte(in), "alpha", "new-address", "cospace", "", "/tmp/known", managedHostKeyAlias("alpha"), true)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.Count(s, "# >>> cospace:alpha >>>") != 1 {
		t.Fatalf("got duplicate block:\n%s", s)
	}
	if strings.Contains(s, "old-address") || !strings.Contains(s, "new-address") {
		t.Fatalf("block was not replaced:\n%s", s)
	}
	if !strings.Contains(s, "Host github.com\n  User git") || !strings.Contains(s, "Host after\n  User somebody") {
		t.Fatalf("unrelated config was not preserved:\n%s", s)
	}
}

func TestRewriteSSHConfigPreservesUnrelatedContent(t *testing.T) {
	in := []byte("# my ssh config\nServerAliveInterval 30\nInclude ~/.ssh/conf.d/*\nHost *\n  StrictHostKeyChecking no\nHost existing\n  HostName example.com\n")
	got, err := rewriteSSHConfig(in, "beta", "tc-beta", "cospace", "", "/tmp/known", managedHostKeyAlias("beta"), true)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	// Every original line survives…
	for _, line := range strings.Split(strings.TrimSpace(string(in)), "\n") {
		if !strings.Contains(out, line) {
			t.Fatalf("existing line %q lost:\n%s", line, out)
		}
	}
	// …and the managed block sits before the user's first stanza, so a leading
	// "Host *" (StrictHostKeyChecking no, its own ProxyCommand) cannot override
	// the pinned host key: ssh takes the first value it sees for each option.
	block := strings.Index(out, "Host beta")
	include := strings.Index(out, "Include ~/.ssh/conf.d/*")
	star := strings.Index(out, "Host *")
	if block < 0 || block > include || block > star {
		t.Fatalf("managed block not before the first stanza:\n%s", out)
	}
	if !strings.HasPrefix(out, "# my ssh config\nServerAliveInterval 30\n") {
		t.Fatalf("leading global options moved:\n%s", out)
	}
}

func TestPostPairSuccess(t *testing.T) {
	var got pairRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/pair" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pairResponse{Space: "alpha", HostKey: testHostKey, Fingerprint: testHostFP})
	}))
	defer srv.Close()

	wantReq := pairRequest{Code: "K7M4-Q2PX", Member: "alice", PubKey: "ssh-ed25519 AAAA", Protocol: pairProtocol, Version: "1.2.3"}
	resp, err := postPair(context.Background(), srv.Client(), srv.URL+"/pair", wantReq)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantReq {
		t.Fatalf("request = %+v, want %+v", got, wantReq)
	}
	if resp.Space != "alpha" || resp.HostKey != strings.TrimSuffix(testHostKey, " space") || resp.Fingerprint != testHostFP {
		t.Fatalf("response = %+v", resp)
	}
}

func TestPostPairRejectsHostKeyFingerprintMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(pairResponse{Space: "alpha", HostKey: testHostKey, Fingerprint: "SHA256:wrong"})
	}))
	defer srv.Close()
	if _, err := postPair(context.Background(), srv.Client(), srv.URL, pairRequest{}); err == nil {
		t.Fatal("mismatched host key unexpectedly accepted")
	}
}

func TestRewriteKnownHostsRotatesOnlyManagedAlias(t *testing.T) {
	alias := managedHostKeyAlias("alpha")
	old := "github.com ssh-ed25519 AAAA\n\n# >>> cospace:alpha >>>\n" + alias + " ssh-ed25519 OLD\n# <<< cospace:alpha <<<\n"
	got, err := rewriteKnownHosts([]byte(old), "alpha", alias, testHostKey)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "github.com ssh-ed25519 AAAA") || strings.Contains(s, " OLD") {
		t.Fatalf("unrelated entry changed or old key retained:\n%s", s)
	}
	if strings.Count(s, "# >>> cospace:alpha >>>") != 1 || !strings.Contains(s, strings.TrimSuffix(testHostKey, " space")) {
		t.Fatalf("managed key not replaced exactly once:\n%s", s)
	}
}

func TestPostPairExplainsProtocolMismatch(t *testing.T) {
	cases := []struct {
		name     string
		have     int
		min, max int
		want     string
	}{
		{"too old", pairProtocol, pairProtocol + 1, pairProtocol + 1, "installer again"},
		{"too new", pairProtocol + 3, pairProtocol, pairProtocol, "ask the host"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUpgradeRequired)
			json.NewEncoder(w).Encode(pairRejection{Error: "unsupported_protocol", MinProtocol: tc.min, MaxProtocol: tc.max})
		}))
		_, err := postPair(context.Background(), srv.Client(), srv.URL, pairRequest{Protocol: tc.have})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want mention of %q", tc.name, err, tc.want)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"version"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "cospace dev (") {
		t.Fatalf("version output = %q", out.String())
	}
}

func TestPostPairRejectsForbiddenAndBadJSON(t *testing.T) {
	for _, handler := range []http.Handler{
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "forbidden", http.StatusForbidden) }),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("{")) }),
	} {
		srv := httptest.NewServer(handler)
		_, err := postPair(context.Background(), srv.Client(), srv.URL, pairRequest{})
		srv.Close()
		if err == nil {
			t.Fatal("postPair unexpectedly succeeded")
		}
	}
}
