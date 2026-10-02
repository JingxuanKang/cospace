// Package transport embeds tailcat and maps each space's stable tailcat node to
// the space's SSH service and host-side pairing endpoint.
package transport

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
)

const identityFileName = "identity.json"

var spaceNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// SpaceBackend bridges transport connections to space lifecycle and membership.
type SpaceBackend interface {
	DialSSH(space string) (net.Conn, error)
	Pair(space, code, memberName, pubKey string) (PairResult, error)
}

type PairResult struct {
	HostKey     string
	Fingerprint string
}

// PairError is a rejection the guest tool may show verbatim (a name
// collision, a malformed key). Any other backend error is reported to the
// guest only as "forbidden", so a wrong code learns nothing.
type PairError struct {
	Status  int
	Message string
}

func (e *PairError) Error() string { return e.Message }

// pair attempts per space per minute. Invite codes carry 40 bits of
// entropy; this just keeps a flood from counting as space activity.
const pairBurst = 10

type spaceNode struct {
	server *tailcat.Server
	addr   string
}

type Manager struct {
	dataDir string
	backend SpaceBackend

	mu       sync.Mutex
	nodes    map[string]*spaceNode
	activity map[string]time.Time
	pairHits map[string]*hitWindow
}

type hitWindow struct {
	start time.Time
	count int
}

func New(dataDir string, backend SpaceBackend) *Manager {
	return &Manager{
		dataDir:  dataDir,
		backend:  backend,
		nodes:    make(map[string]*spaceNode),
		activity: make(map[string]time.Time),
		pairHits: make(map[string]*hitWindow),
	}
}

// Address returns the stable connection blob for space without starting its
// tailcat node. The first call creates and persists both the node key and fixed
// DERP region, which are the two inputs to a stable blob.
func (m *Manager) Address(space string) (string, error) {
	if err := validateSpace(space); err != nil {
		return "", err
	}
	id, err := loadOrCreateIdentity(m.identityPath(space), newPersistentIdentity)
	if err != nil {
		return "", err
	}
	return string(id.Public.ConnBlob()), nil
}

// EnsureSpace starts at most one tailcat server for space in this process.
func (m *Manager) EnsureSpace(space string) (addr string, err error) {
	if err := validateSpace(space); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if node := m.nodes[space]; node != nil {
		return node.addr, nil
	}

	id, err := loadOrCreateIdentity(m.identityPath(space), newPersistentIdentity)
	if err != nil {
		return "", err
	}
	s := &tailcat.Server{
		Key:   id.Private,
		Logf:  func(string, ...any) {},
		OnTCP: func(port uint16) func(net.Conn) { return m.handler(space, port) },
	}
	switch {
	case len(id.Public.Region) > 0:
		s.Region = id.Public.Region[0]
	case id.Public.RegionID > 0:
		s.RegionID = id.Public.RegionID
	default:
		return "", errors.New("transport identity has no fixed DERP region")
	}
	if err := s.Start(); err != nil {
		return "", fmt.Errorf("start tailcat space %s: %w", space, err)
	}
	addr = string(id.Public.ConnBlob())
	m.nodes[space] = &spaceNode{server: s, addr: addr}
	return addr, nil
}

// DropSpace stops the in-process node and removes its persisted identity.
func (m *Manager) DropSpace(space string) error {
	if err := validateSpace(space); err != nil {
		return err
	}
	m.mu.Lock()
	node := m.nodes[space]
	delete(m.nodes, space)
	delete(m.activity, space)
	m.mu.Unlock()
	if node != nil {
		if err := node.server.Close(); err != nil {
			return err
		}
	}
	return os.RemoveAll(filepath.Dir(m.identityPath(space)))
}

// Close stops every node while retaining identities for the next daemon run.
func (m *Manager) Close() error {
	m.mu.Lock()
	nodes := m.nodes
	m.nodes = make(map[string]*spaceNode)
	m.mu.Unlock()
	var joined error
	for _, node := range nodes {
		joined = errors.Join(joined, node.server.Close())
	}
	return joined
}

func (m *Manager) LastActivity(space string) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activity[space]
}

func (m *Manager) touch(space string) {
	m.mu.Lock()
	m.activity[space] = time.Now()
	m.mu.Unlock()
}

// Touch records activity for space from outside the transport (the gateway
// counts AI requests), so the idle reaper sees an unattended agent at work.
func (m *Manager) Touch(space string) { m.touch(space) }

func (m *Manager) pairAllowed(space string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	hw := m.pairHits[space]
	now := time.Now()
	if hw == nil || now.Sub(hw.start) > time.Minute {
		hw = &hitWindow{start: now}
		m.pairHits[space] = hw
	}
	hw.count++
	return hw.count <= pairBurst
}

func (m *Manager) handler(space string, port uint16) func(net.Conn) {
	switch port {
	case 22:
		return func(guest net.Conn) { m.handleSSH(space, guest) }
	case 80:
		return func(guest net.Conn) { m.handleHTTP(space, guest) }
	default:
		return nil
	}
}

func (m *Manager) handleSSH(space string, guest net.Conn) {
	m.touch(space)
	guest = &activityConn{Conn: guest, activity: func() { m.touch(space) }}
	backend, err := m.backend.DialSSH(space)
	if err != nil {
		log.Printf("transport: DialSSH %s failed: %v", space, err)
		guest.Close()
		return
	}
	tailcat.ProxyConns(guest, backend)
}

func (m *Manager) handleHTTP(space string, guest net.Conn) {
	// Pairing traffic is not space activity: an unpaired stranger probing
	// codes must not keep a space awake.
	_ = http.Serve(&oneConnListener{conn: guest}, m.pairHandler(space))
}

type pairRequest struct {
	Code   string `json:"code"`
	Member string `json:"member"`
	PubKey string `json:"pub_key"`
	// Protocol is the /pair wire-format version the guest tool speaks.
	// Absent (0) means a build from before the field existed, which speaks
	// protocol 1: the shape has not changed since the space rename.
	Protocol int    `json:"protocol"`
	Version  string `json:"version"`
}

// Pair protocol range this daemon accepts. Bump maxPairProtocol when the
// wire format changes; raise minPairProtocol only when old shapes can no
// longer be served. Guest tools outside the range get 426 with both bounds
// so they can print the right remedy (re-run the installer / ask the host).
const (
	minPairProtocol = 1
	maxPairProtocol = 1
)

type pairRejection struct {
	Error       string `json:"error"`
	MinProtocol int    `json:"min_protocol"`
	MaxProtocol int    `json:"max_protocol"`
}

type pairResponse struct {
	Space       string `json:"space"`
	HostKey     string `json:"host_key"`
	Fingerprint string `json:"fingerprint"`
}

func (m *Manager) pairHandler(space string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forbidden := func() {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Connection", "close")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"forbidden"}`+"\n")
		}
		if r.Method != http.MethodPost || r.URL.Path != "/pair" {
			forbidden()
			return
		}
		defer r.Body.Close()
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		var req pairRequest
		if err := dec.Decode(&req); err != nil || req.Code == "" || req.Member == "" || req.PubKey == "" {
			forbidden()
			return
		}
		if !m.pairAllowed(space) {
			w.Header().Set("Connection", "close")
			http.Error(w, "too many pairing attempts; try again in a minute", http.StatusTooManyRequests)
			return
		}
		if strings.ContainsAny(req.PubKey, "\r\n\x00") || len(req.PubKey) > 16*1024 || len(req.Member) > 64 {
			forbidden()
			return
		}
		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			forbidden()
			return
		}
		if req.Protocol == 0 {
			req.Protocol = 1
		}
		// Checked before the code is consulted, so the response depends only
		// on the client build and cannot be used to probe invite codes.
		if req.Protocol < minPairProtocol || req.Protocol > maxPairProtocol {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Connection", "close")
			w.WriteHeader(http.StatusUpgradeRequired)
			_ = json.NewEncoder(w).Encode(pairRejection{Error: "unsupported_protocol", MinProtocol: minPairProtocol, MaxProtocol: maxPairProtocol})
			return
		}
		result, err := m.backend.Pair(space, req.Code, req.Member, req.PubKey)
		if err != nil {
			var pe *PairError
			if errors.As(err, &pe) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Connection", "close")
				w.WriteHeader(pe.Status)
				_ = json.NewEncoder(w).Encode(pairRejection{Error: pe.Message})
				return
			}
			forbidden()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Connection", "close")
		_ = json.NewEncoder(w).Encode(pairResponse{Space: space, HostKey: result.HostKey, Fingerprint: result.Fingerprint})
	})
}

type activityConn struct {
	net.Conn
	activity func()
}

func (c *activityConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.activity()
	}
	return n, err
}

func (c *activityConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.activity()
	}
	return n, err
}

func (c *activityConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

type oneConnListener struct {
	mu   sync.Mutex
	conn net.Conn
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return nil, io.EOF
	}
	c := l.conn
	l.conn = nil
	return c, nil
}

func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return dummyAddr("tailcat") }

type dummyAddr string

func (a dummyAddr) Network() string { return string(a) }
func (a dummyAddr) String() string  { return string(a) }

func validateSpace(space string) error {
	if !spaceNameRE.MatchString(space) {
		return fmt.Errorf("invalid space name %q", space)
	}
	return nil
}

func (m *Manager) identityPath(space string) string {
	return filepath.Join(m.dataDir, "transport", space, identityFileName)
}

func newPersistentIdentity() (*tailcat.PrivateKey, error) {
	id := tailcat.NewPrivateKey()
	id.Public.RegionID = -1
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := id.Public.Expand(ctx, tailcat.ExpandForServer); err != nil {
		return nil, fmt.Errorf("select tailcat DERP region: %w", err)
	}
	if len(id.Public.Region) == 0 {
		return nil, errors.New("tailcat selected no DERP region")
	}
	// Two relays are enough and keep the invitation blob compact.
	if len(id.Public.Region[0].Nodes) > 2 {
		id.Public.Region[0].Nodes = id.Public.Region[0].Nodes[:2]
	}
	return id, nil
}

func loadOrCreateIdentity(path string, create func() (*tailcat.PrivateKey, error)) (*tailcat.PrivateKey, error) {
	id, err := readIdentity(path)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	id, err = create()
	if err != nil {
		return nil, err
	}
	if err := validateIdentity(id); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".identity-*.tmp")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	// Link publishes a complete file atomically without replacing an identity
	// another process may have won the race to create.
	if err := os.Link(tmpPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readIdentity(path)
		}
		return nil, err
	}
	return id, nil
}

func readIdentity(path string) (*tailcat.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var id tailcat.PrivateKey
	if err := json.Unmarshal(b, &id); err != nil {
		return nil, fmt.Errorf("parse transport identity %s: %w", path, err)
	}
	if err := validateIdentity(&id); err != nil {
		return nil, fmt.Errorf("invalid transport identity %s: %w", path, err)
	}
	return &id, nil
}

func validateIdentity(id *tailcat.PrivateKey) error {
	if id == nil || id.Private.IsZero() {
		return errors.New("missing private key")
	}
	got, want := id.Public.ServerPublic.NodePublic.AppendTo(nil), id.Private.Public().AppendTo(nil)
	if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
		return errors.New("public key does not match private key")
	}
	if len(id.Public.Region) == 0 && id.Public.RegionID <= 0 {
		return errors.New("missing fixed DERP region")
	}
	return nil
}
