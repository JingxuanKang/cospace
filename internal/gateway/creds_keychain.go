package gateway

import (
	"context"
	"os/exec"
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", k.Service, "-w").Output()
	k.header = ""
	if err == nil && strings.TrimSpace(string(b)) != "" {
		k.header = "Bearer " + strings.TrimSpace(string(b))
	}
	k.expires = time.Now().Add(30 * time.Second)
	return k.header, k.header != ""
}
func (k *KeychainCreds) Learn(string, string) {}
