// Package relay implements the BYO-VPS dumb pipe: the host daemon dials OUT
// to the relay and keeps a multiplexed session; the relay listens on one
// stable public TCP port per space and pipes guest connections back over the
// session. The relay never sees plaintext — everything it carries is SSH.
//
// Wire protocol (all control messages are single JSON lines):
//
//	daemon → relay on connect:   {"token":"..."}        → {"ok":true} | {"error":"..."}
//	then a yamux session; the daemon opens the FIRST stream as the control
//	channel and sends {"cmd":"listen","space":"acme"} → {"ok":true,"port":24317}.
//	For each guest connection the relay opens a NEW stream and sends
//	{"space":"acme"} followed by the raw byte pipe.
package relay

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

const basePort = 24300

type ctrlMsg struct {
	Cmd   string `json:"cmd,omitempty"`
	Space string `json:"space,omitempty"`
	Token string `json:"token,omitempty"`
	OK    bool   `json:"ok,omitempty"`
	Port  int    `json:"port,omitempty"`
	Error string `json:"error,omitempty"`
}

func writeMsg(w io.Writer, m ctrlMsg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func readMsg(r *bufio.Reader) (ctrlMsg, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return ctrlMsg{}, err
	}
	var m ctrlMsg
	if err := json.Unmarshal(line, &m); err != nil {
		return ctrlMsg{}, err
	}
	return m, nil
}

// Server is the VPS side.
type Server struct {
	Token     string
	StateFile string // persisted space→port map, so ports survive restarts

	mu        sync.Mutex
	ports     map[string]int
	listeners map[string]net.Listener
	session   *yamux.Session
}

func NewServer(token, stateFile string) *Server {
	s := &Server{Token: token, StateFile: stateFile, ports: map[string]int{}, listeners: map[string]net.Listener{}}
	if b, err := os.ReadFile(stateFile); err == nil {
		json.Unmarshal(b, &s.ports)
	}
	return s
}

func (s *Server) savePorts() {
	b, _ := json.Marshal(s.ports)
	os.WriteFile(s.StateFile, b, 0o600)
}

// Serve accepts daemon control connections on l. The newest authenticated
// daemon session wins; guest listeners persist across reconnects.
func (s *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go s.handleDaemon(conn)
	}
}

func (s *Server) handleDaemon(conn net.Conn) {
	defer conn.Close()
	// Anyone can connect to the control port; an unauthenticated peer gets a
	// few seconds to say hello, not a goroutine for life.
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	hello, err := readMsg(br)
	if err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})
	if subtle.ConstantTimeCompare([]byte(hello.Token), []byte(s.Token)) != 1 {
		writeMsg(conn, ctrlMsg{Error: "bad token"})
		return
	}
	if err := writeMsg(conn, ctrlMsg{OK: true}); err != nil {
		return
	}
	sess, err := yamux.Client(&bufConn{Conn: conn, r: br}, nil)
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.session != nil {
		s.session.Close()
	}
	s.session = sess
	s.mu.Unlock()

	// The daemon opens the control stream toward us.
	ctrl, err := sess.AcceptStream()
	if err != nil {
		return
	}
	cbr := bufio.NewReader(ctrl)
	for {
		m, err := readMsg(cbr)
		if err != nil {
			return
		}
		if m.Cmd != "listen" || m.Space == "" {
			writeMsg(ctrl, ctrlMsg{Error: "bad command"})
			continue
		}
		port, err := s.ensureListener(m.Space)
		if err != nil {
			writeMsg(ctrl, ctrlMsg{Error: err.Error()})
			continue
		}
		writeMsg(ctrl, ctrlMsg{OK: true, Port: port})
	}
}

func (s *Server) ensureListener(space string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, up := s.listeners[space]; up {
		return s.ports[space], nil
	}
	port := s.ports[space]
	l, err := listenPort(port)
	if err != nil && port != 0 {
		// stored port unavailable; allocate a fresh one
		l, err = listenPort(0)
	}
	if err != nil {
		return 0, err
	}
	port = l.Addr().(*net.TCPAddr).Port
	s.ports[space] = port
	s.listeners[space] = l
	s.savePorts()
	go s.acceptGuests(space, l)
	return port, nil
}

func listenPort(port int) (net.Listener, error) {
	if port == 0 {
		// allocate from the stable base range first for predictable firewalls
		for p := basePort; p < basePort+500; p++ {
			if l, err := net.Listen("tcp", fmt.Sprintf(":%d", p)); err == nil {
				return l, nil
			}
		}
		return net.Listen("tcp", ":0")
	}
	return net.Listen("tcp", fmt.Sprintf(":%d", port))
}

func (s *Server) acceptGuests(space string, l net.Listener) {
	for {
		guest, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer guest.Close()
			s.mu.Lock()
			sess := s.session
			s.mu.Unlock()
			if sess == nil || sess.IsClosed() {
				return // host offline
			}
			stream, err := sess.OpenStream()
			if err != nil {
				return
			}
			defer stream.Close()
			if writeMsg(stream, ctrlMsg{Space: space}) != nil {
				return
			}
			pipe(guest, stream)
		}()
	}
}

// Dialer resolves a space to a live TCP connection to its sshd.
type Dialer func(space string) (net.Conn, error)

// Client is the daemon side: one outbound session serving guest streams.
type Client struct {
	sess *yamux.Session
	ctrl *bufio.Reader
	cw   io.Writer
	mu   sync.Mutex
}

// Connect dials the relay and authenticates. Dial is invoked for every
// incoming guest stream (resolving the space fresh each time).
func Connect(addr, token string, dial Dialer) (*Client, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	if err := writeMsg(conn, ctrlMsg{Token: token}); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := readMsg(br)
	if err != nil || !resp.OK {
		conn.Close()
		if err == nil {
			err = fmt.Errorf("relay refused: %s", resp.Error)
		}
		return nil, err
	}
	sess, err := yamux.Server(&bufConn{Conn: conn, r: br}, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	ctrl, err := sess.OpenStream()
	if err != nil {
		sess.Close()
		return nil, err
	}
	c := &Client{sess: sess, ctrl: bufio.NewReader(ctrl), cw: ctrl}
	go c.acceptStreams(dial)
	return c, nil
}

// Register asks the relay for the space's stable public port.
func (c *Client) Register(space string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := writeMsg(c.cw, ctrlMsg{Cmd: "listen", Space: space}); err != nil {
		return 0, err
	}
	resp, err := readMsg(c.ctrl)
	if err != nil {
		return 0, err
	}
	if !resp.OK {
		return 0, fmt.Errorf("relay: %s", resp.Error)
	}
	return resp.Port, nil
}

func (c *Client) Close() error { return c.sess.Close() }

func (c *Client) acceptStreams(dial Dialer) {
	for {
		stream, err := c.sess.AcceptStream()
		if err != nil {
			return
		}
		go func() {
			defer stream.Close()
			br := bufio.NewReader(stream)
			head, err := readMsg(br)
			if err != nil || head.Space == "" {
				return
			}
			space, err := dial(head.Space)
			if err != nil {
				return
			}
			defer space.Close()
			pipe(&bufConn{Conn: stream, r: br}, space)
		}()
	}
}

func pipe(a, b io.ReadWriter) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// bufConn lets a bufio.Reader's buffered bytes stay part of the connection
// after the JSON handshake line has been consumed.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }
