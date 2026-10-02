package relay

import (
	"bufio"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// startEchoSpace is a stand-in for a space's sshd: greets then echoes lines.
func startEchoSpace(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				fmt.Fprintf(c, "greeting\n")
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadBytes('\n')
					if err != nil {
						return
					}
					c.Write(line)
				}
			}()
		}
	}()
	return l
}

func startRelay(t *testing.T, token, state string) (*Server, string) {
	t.Helper()
	srv := NewServer(token, state)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() { l.Close() })
	return srv, l.Addr().String()
}

func TestGuestReachesSpaceThroughRelay(t *testing.T) {
	space := startEchoSpace(t)
	defer space.Close()
	state := filepath.Join(t.TempDir(), "ports.json")
	_, addr := startRelay(t, "sekrit", state)

	dial := func(name string) (net.Conn, error) {
		if name != "acme" {
			return nil, fmt.Errorf("unknown space %q", name)
		}
		return net.Dial("tcp", space.Addr().String())
	}
	c, err := Connect(addr, "sekrit", dial)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	port, err := c.Register("acme")
	if err != nil {
		t.Fatal(err)
	}

	guest, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	guest.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(guest)
	if line, _ := br.ReadString('\n'); line != "greeting\n" {
		t.Fatalf("greeting = %q", line)
	}
	fmt.Fprintf(guest, "ping over relay\n")
	if line, _ := br.ReadString('\n'); line != "ping over relay\n" {
		t.Fatalf("echo = %q", line)
	}
}

func TestBadTokenRejected(t *testing.T) {
	state := filepath.Join(t.TempDir(), "ports.json")
	_, addr := startRelay(t, "sekrit", state)
	if _, err := Connect(addr, "wrong", nil); err == nil {
		t.Fatal("want auth failure")
	}
}

func TestPortStableAcrossReconnect(t *testing.T) {
	space := startEchoSpace(t)
	defer space.Close()
	state := filepath.Join(t.TempDir(), "ports.json")
	_, addr := startRelay(t, "sekrit", state)
	dial := func(string) (net.Conn, error) { return net.Dial("tcp", space.Addr().String()) }

	c1, err := Connect(addr, "sekrit", dial)
	if err != nil {
		t.Fatal(err)
	}
	p1, err := c1.Register("acme")
	if err != nil {
		t.Fatal(err)
	}
	c1.Close()

	c2, err := Connect(addr, "sekrit", dial)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	p2, err := c2.Register("acme")
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("port changed across reconnect: %d -> %d", p1, p2)
	}

	// and the new session actually serves traffic
	guest, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p2), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	guest.SetDeadline(time.Now().Add(5 * time.Second))
	if line, _ := bufio.NewReader(guest).ReadString('\n'); line != "greeting\n" {
		t.Fatalf("after reconnect greeting = %q", line)
	}
}
