package gateway

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// KeychainCreds reads a host-owned Sub2API key. Neither the key nor its lookup
// output is sent to a space, persisted by CoSpace, or included in errors.
type KeychainCreds struct {
	Service string
	Enabled func(string) bool
	mu      sync.Mutex
	header  string
	expires time.Time
}

func (k *KeychainCreds) Get(provider string) (string, bool) {
	if k.Enabled != nil && !k.Enabled(provider) {
		return "", false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if time.Now().Before(k.expires) {
		return k.header, k.header != ""
	}
	b, err := readHostSecret(k.Service)
	k.header = ""
	if err == nil && strings.TrimSpace(string(b)) != "" {
		k.header = "Bearer " + strings.TrimSpace(string(b))
	}
	k.expires = time.Now().Add(30 * time.Second)
	return k.header, k.header != ""
}

// readHostSecret fetches a host-owned secret by service name: the macOS
// Keychain on macOS, and on other systems a mode-0600 file named after the
// service under $XDG_CONFIG_HOME/cospace/secrets (default
// ~/.config/cospace/secrets).
func readHostSecret(service string) ([]byte, error) {
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", service, "-w").Output()
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		base = filepath.Join(home, ".config")
	}
	path := filepath.Join(base, "cospace", "secrets", filepath.Base(service))
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must be mode 0600", path)
	}
	return os.ReadFile(path)
}
func (k *KeychainCreds) Learn(string, string) {}
