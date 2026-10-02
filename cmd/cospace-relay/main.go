// cospace-relay is the dumb pipe that runs on the host's own VPS.
// It forwards encrypted SSH bytes and stores only a space→port map.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"

	"github.com/JingxuanKang/cospace/internal/relay"
)

func main() {
	listen := flag.String("listen", ":7300", "control listener for the host daemon")
	tokenFile := flag.String("token-file", "/etc/cospace-relay.token", "file holding the shared auth token")
	state := flag.String("state", "/var/lib/cospace-relay/ports.json", "space→port map persistence")
	flag.Parse()

	b, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cospace-relay:", err)
		os.Exit(1)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		fmt.Fprintln(os.Stderr, "cospace-relay: empty token file")
		os.Exit(1)
	}

	l, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cospace-relay:", err)
		os.Exit(1)
	}
	log.Printf("cospace-relay: control on %s", *listen)
	if err := relay.NewServer(token, *state).Serve(l); err != nil {
		fmt.Fprintln(os.Stderr, "cospace-relay:", err)
		os.Exit(1)
	}
}
