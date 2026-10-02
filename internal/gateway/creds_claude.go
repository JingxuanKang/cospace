package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ClaudeCredential is the subset of Claude Code's OAuth record the gateway and
// the keepalive need.
type ClaudeCredential struct {
	AccessToken string
	ExpiresAt   time.Time
	Source      string // "file" or "keychain"
}

// claudeKeychainService is the macOS Keychain item Claude Code maintains.
const claudeKeychainService = "Claude Code-credentials"

// keychainReader is swapped in tests. It returns the raw JSON of the
// Keychain item, or an error when there is none / not on macOS.
var keychainReader = readClaudeKeychain

var (
	keychainMu      sync.Mutex
	keychainCache   []byte
	keychainCacheAt time.Time
	keychainTTL     = 30 * time.Second
)

// ClaudeOAuth returns the live Claude credential. Claude Code on macOS stores
// the OAuth record in the Keychain and only falls back to
// ~/.claude/.credentials.json where the Keychain is unavailable; the two
// copies are independent and each is refreshed by whichever process reads
// it. Any single source therefore goes stale behind the host's back — the
// keepalive pinged the CLI for weeks and watched the file never advance
// while the Keychain copy was renewed every time. Reading both and serving
// the one that expires last follows the CLI wherever it keeps the record,
// without ever refreshing a token ourselves.
func ClaudeOAuth(path string) (ClaudeCredential, error) {
	var best ClaudeCredential
	var errs []error
	if b, err := os.ReadFile(path); err == nil {
		if c, err := parseClaudeCredential(b, "file"); err == nil {
			best = c
		} else {
			errs = append(errs, err)
		}
	} else {
		errs = append(errs, err)
	}
	if b, err := cachedKeychain(); err == nil {
		if c, err := parseClaudeCredential(b, "keychain"); err == nil {
			if best.AccessToken == "" || c.ExpiresAt.After(best.ExpiresAt) {
				best = c
			}
		} else {
			errs = append(errs, err)
		}
	} else {
		errs = append(errs, err)
	}
	if best.AccessToken == "" {
		return best, errors.Join(errs...)
	}
	return best, nil
}

func parseClaudeCredential(b []byte, source string) (ClaudeCredential, error) {
	var doc struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return ClaudeCredential{}, err
	}
	if doc.OAuth.AccessToken == "" {
		return ClaudeCredential{}, errors.New("no claudeAiOauth.accessToken")
	}
	var exp time.Time
	if doc.OAuth.ExpiresAt > 0 {
		exp = time.UnixMilli(doc.OAuth.ExpiresAt)
	}
	return ClaudeCredential{AccessToken: doc.OAuth.AccessToken, ExpiresAt: exp, Source: source}, nil
}

// cachedKeychain memoizes the Keychain read: `security` is a subprocess and
// the gateway asks on every request.
func cachedKeychain() ([]byte, error) {
	keychainMu.Lock()
	defer keychainMu.Unlock()
	if keychainCache != nil && time.Since(keychainCacheAt) < keychainTTL {
		return keychainCache, nil
	}
	b, err := keychainReader()
	if err != nil {
		return nil, err
	}
	keychainCache, keychainCacheAt = b, time.Now()
	return b, nil
}

// InvalidateClaudeKeychainCache forces the next ClaudeOAuth to re-read the
// Keychain; the keepalive calls it right after a ping so it can see the
// renewed copy immediately.
func InvalidateClaudeKeychainCache() {
	keychainMu.Lock()
	keychainCache, keychainCacheAt = nil, time.Time{}
	keychainMu.Unlock()
}

func readClaudeKeychain() ([]byte, error) {
	if runtime.GOOS != "darwin" {
		return nil, errors.New("no keychain on this platform")
	}
	if testing.Testing() {
		// Never read the developer's real credential during `go test`.
		return nil, errors.New("keychain disabled under test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", claudeKeychainService, "-w").Output()
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil, errors.New("empty keychain item")
	}
	return []byte(s), nil
}
