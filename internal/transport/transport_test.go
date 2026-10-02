package transport

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
)

type fakeBackend struct {
	mu          sync.Mutex
	sshPeer     net.Conn
	dialSpaces  []string
	pairSpace   string
	pairCode    string
	pairMember  string
	pairKey     string
	hostKey     string
	fingerprint string
	pairErr     error
}

func (f *fakeBackend) DialSSH(space string) (net.Conn, error) {
	server, peer := net.Pipe()
	f.mu.Lock()
	f.sshPeer = peer
	f.dialSpaces = append(f.dialSpaces, space)
	f.mu.Unlock()
	return server, nil
}

func (f *fakeBackend) Pair(space, code, memberName, pubKey string) (PairResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pairSpace, f.pairCode, f.pairMember, f.pairKey = space, code, memberName, pubKey
	return PairResult{HostKey: f.hostKey, Fingerprint: f.fingerprint}, f.pairErr
}

func TestPortRoutingAndSSHProxy(t *testing.T) {
	backend := &fakeBackend{}
	m := New(t.TempDir(), backend)
	if m.handler("alpha", 22) == nil || m.handler("alpha", 80) == nil {
		t.Fatal("ports 22 and 80 must be routed")
	}
	if m.handler("alpha", 443) != nil {
		t.Fatal("unexpected route for port 443")
	}

	guest, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		m.handler("alpha", 22)(server)
		close(done)
	}()
	defer guest.Close()

	deadline := time.Now().Add(time.Second)
	var peer net.Conn
	for time.Now().Before(deadline) {
		backend.mu.Lock()
		peer = backend.sshPeer
		backend.mu.Unlock()
		if peer != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if peer == nil {
		t.Fatal("DialSSH was not called")
	}
	defer peer.Close()

	want := []byte("ssh bytes")
	go guest.Write(want)
	got := make([]byte, len(want))
	if _, err := io.ReadFull(peer, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("proxied bytes = %q, want %q", got, want)
	}
	backend.mu.Lock()
	spaces := append([]string(nil), backend.dialSpaces...)
	backend.mu.Unlock()
	if len(spaces) != 1 || spaces[0] != "alpha" {
		t.Fatalf("DialSSH spaces = %v, want [alpha]", spaces)
	}
	guest.Close()
	peer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SSH proxy did not finish")
	}
}

func TestPairHandlerSuccessAndGenericErrors(t *testing.T) {
	backend := &fakeBackend{hostKey: "ssh-ed25519 AAAA", fingerprint: "SHA256:abc"}
	m := New(t.TempDir(), backend)
	h := m.pairHandler("alpha")

	body := `{"code":"K7M4-Q2PX","member":"alice","pub_key":"ssh-ed25519 AAAA alice"}`
	req := httptest.NewRequest(http.MethodPost, "/pair", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got pairResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Space != "alpha" || got.HostKey != "ssh-ed25519 AAAA" || got.Fingerprint != "SHA256:abc" {
		t.Fatalf("response = %+v", got)
	}
	backend.mu.Lock()
	args := []string{backend.pairSpace, backend.pairCode, backend.pairMember, backend.pairKey}
	backend.mu.Unlock()
	want := []string{"alpha", "K7M4-Q2PX", "alice", "ssh-ed25519 AAAA alice"}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Fatalf("Pair args = %q, want %q", args, want)
	}

	backend.pairErr = errors.New("space alpha does not exist")
	badCases := []*http.Request{
		httptest.NewRequest(http.MethodPost, "/pair", strings.NewReader(body)),
		httptest.NewRequest(http.MethodPost, "/pair", strings.NewReader("{")),
		httptest.NewRequest(http.MethodGet, "/pair", nil),
		httptest.NewRequest(http.MethodPost, "/other", strings.NewReader(body)),
	}
	var genericBody string
	for i, bad := range badCases {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, bad)
		if r.Code != http.StatusForbidden {
			t.Errorf("case %d status = %d, want 403", i, r.Code)
		}
		if i == 0 {
			genericBody = r.Body.String()
		} else if r.Body.String() != genericBody {
			t.Errorf("case %d leaked a distinguishable error: %q vs %q", i, r.Body.String(), genericBody)
		}
	}
}

func TestPairHandlerRejectsUnsupportedProtocolBeforeCode(t *testing.T) {
	backend := &fakeBackend{hostKey: "ssh-ed25519 AAAA", fingerprint: "SHA256:abc"}
	m := New(t.TempDir(), backend)
	h := m.pairHandler("alpha")

	// A build from before the protocol field existed speaks protocol 1.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pair", strings.NewReader(`{"code":"K7M4-Q2PX","member":"alice","pub_key":"ssh-ed25519 AAAA alice"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy client status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pair", strings.NewReader(`{"code":"K7M4-Q2PX","member":"alice","pub_key":"ssh-ed25519 AAAA alice","protocol":99,"version":"9.9.9"}`)))
	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("future client status = %d, want 426", rec.Code)
	}
	var rej pairRejection
	if err := json.Unmarshal(rec.Body.Bytes(), &rej); err != nil {
		t.Fatal(err)
	}
	if rej.Error != "unsupported_protocol" || rej.MinProtocol != minPairProtocol || rej.MaxProtocol != maxPairProtocol {
		t.Fatalf("rejection = %+v", rej)
	}
	// The rejected request must never reach the pairing backend, so a 426
	// cannot be used to consume or probe an invite code.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pair", strings.NewReader(`{"code":"NEVER-SEEN","member":"alice","pub_key":"ssh-ed25519 AAAA alice","protocol":99}`)))
	backend.mu.Lock()
	code := backend.pairCode
	backend.mu.Unlock()
	if code == "NEVER-SEEN" {
		t.Fatal("unsupported protocol still reached the pairing backend")
	}
}

func TestHTTPRouteServesPairProtocol(t *testing.T) {
	backend := &fakeBackend{hostKey: "ssh-ed25519 AAAA", fingerprint: "SHA256:pair"}
	m := New(t.TempDir(), backend)
	client, server := net.Pipe()
	go m.handler("alpha", 80)(server)
	defer client.Close()

	req, err := http.NewRequest(http.MethodPost, "http://cospace/pair", strings.NewReader(
		`{"code":"ABCD-EFGH","member":"bob","pub_key":"ssh-ed25519 AAAA"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := req.Write(client); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
}

func TestLastActivityTracksConnectionsAndData(t *testing.T) {
	backend := &fakeBackend{}
	m := New(t.TempDir(), backend)
	if got := m.LastActivity("alpha"); !got.IsZero() {
		t.Fatalf("initial LastActivity = %v, want zero", got)
	}

	guest, server := net.Pipe()
	go m.handler("alpha", 22)(server)
	defer guest.Close()
	deadline := time.Now().Add(time.Second)
	for m.LastActivity("alpha").IsZero() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	connectedAt := m.LastActivity("alpha")
	if connectedAt.IsZero() {
		t.Fatal("connection did not update LastActivity")
	}

	backend.mu.Lock()
	peer := backend.sshPeer
	backend.mu.Unlock()
	if peer == nil {
		t.Fatal("missing backend peer")
	}
	defer peer.Close()
	time.Sleep(2 * time.Millisecond)
	go guest.Write([]byte("x"))
	if _, err := io.ReadFull(peer, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for !m.LastActivity("alpha").After(connectedAt) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := m.LastActivity("alpha"); !got.After(connectedAt) {
		t.Fatalf("data activity = %v, want after %v", got, connectedAt)
	}
}

type closeWriteConn struct {
	net.Conn
	called bool
}

func (c *closeWriteConn) CloseWrite() error {
	c.called = true
	return nil
}

func TestActivityConnPreservesHalfClose(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	base := &closeWriteConn{Conn: left}
	tracked := &activityConn{Conn: base, activity: func() {}}
	if err := tracked.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if !base.called {
		t.Fatal("CloseWrite was not forwarded to the wrapped transport connection")
	}
}

func TestStableIdentityFileReuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transport", "alpha", identityFileName)
	created := 0
	create := func() (*tailcat.PrivateKey, error) {
		created++
		id := tailcat.NewPrivateKey()
		id.Public.RegionID = 7
		return id, nil
	}
	one, err := loadOrCreateIdentity(path, create)
	if err != nil {
		t.Fatal(err)
	}
	two, err := loadOrCreateIdentity(path, create)
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("identity creator called %d times, want 1", created)
	}
	if one.Public.ConnBlob() != two.Public.ConnBlob() {
		t.Fatalf("address changed across reload: %s != %s", one.Public.ConnBlob(), two.Public.ConnBlob())
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity mode = %o, want 600", info.Mode().Perm())
	}
}
