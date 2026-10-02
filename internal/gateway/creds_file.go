package gateway

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// FileCreds reads the host's own subscription/API credentials straight from
// the files the local CLIs maintain, so the gateway always forwards the
// CURRENT OAuth access token instead of a stale learned snapshot. The local
// CLI refreshes these files; re-reading them means the gateway inherits every
// refresh and never expires while the host stays logged in.
//
// This is the "sponsor with my subscription" toggle: enabled → credentials
// come from these files; disabled → the provider has no cred and spaces get a
// clear 503 rather than silently using someone's key.
type FileCreds struct {
	// Paths maps a provider to the credential file to read.
	Paths map[string]string
	// Enabled reports whether sponsoring is currently on. nil ⇒ always on.
	Enabled func(provider string) bool

	mu    sync.Mutex
	cache map[string]fileCacheEntry
}

type fileCacheEntry struct {
	header  string
	modTime time.Time
	size    int64
}

func NewFileCreds(paths map[string]string) *FileCreds {
	return &FileCreds{Paths: paths, cache: map[string]fileCacheEntry{}}
}

// Get returns "Bearer <accessToken>" read from the provider's live file.
func (f *FileCreds) Get(provider string) (string, bool) {
	if f.Enabled != nil && !f.Enabled(provider) {
		return "", false
	}
	path, ok := f.Paths[provider]
	if !ok {
		return "", false
	}
	if provider == "anthropic" {
		// Claude Code keeps its OAuth credential in the macOS Keychain as well
		// as (or instead of) the file, and refreshes whichever copy it uses.
		// Serve the copy that expires last; see ClaudeOAuth.
		cred, err := ClaudeOAuth(path)
		if err != nil || cred.AccessToken == "" {
			return "", false
		}
		return "Bearer " + cred.AccessToken, true
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Re-read only when the file changed (the CLI rewrites it on refresh).
	if e, ok := f.cache[provider]; ok && e.modTime.Equal(fi.ModTime()) && e.size == fi.Size() && e.header != "" {
		return e.header, true
	}
	tok, err := readAccessToken(provider, path)
	if err != nil || tok == "" {
		return "", false
	}
	header := "Bearer " + tok
	f.cache[provider] = fileCacheEntry{header: header, modTime: fi.ModTime(), size: fi.Size()}
	return header, true
}

// Learn is a no-op: file-backed creds are never learned from traffic.
func (f *FileCreds) Learn(provider, authHeader string) {}

func readAccessToken(provider, path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	switch provider {
	case "anthropic":
		var doc struct {
			OAuth struct {
				AccessToken string `json:"accessToken"`
			} `json:"claudeAiOauth"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			return "", err
		}
		return doc.OAuth.AccessToken, nil
	case "openai":
		var doc struct {
			Tokens struct {
				AccessToken string `json:"access_token"`
			} `json:"tokens"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			return "", err
		}
		return doc.Tokens.AccessToken, nil
	case "xai":
		// grok's auth.json is keyed by a per-login "issuer::uuid" string; the
		// entry's "key" is the short-lived OIDC JWT that api.x.ai accepts.
		// The grok CLI refreshes it whenever the host uses grok. A host who
		// logged in more than once has several entries; map iteration order
		// is random, so pick the one that expires last rather than any.
		return LatestXAIKey(b)
	default:
		return "", nil
	}
}

// LatestXAIKey returns the grok OIDC JWT with the latest exp claim among the
// entries of an auth.json document (an entry without a parsable exp sorts
// first, so it is only used when nothing better exists).
func LatestXAIKey(doc []byte) (string, error) {
	var entries map[string]struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(doc, &entries); err != nil {
		return "", err
	}
	var best string
	var bestExp time.Time
	for _, entry := range entries {
		if entry.Key == "" {
			continue
		}
		exp, err := JWTExpiry(entry.Key)
		if err != nil {
			exp = time.Time{}
		}
		if best == "" || exp.After(bestExp) {
			best, bestExp = entry.Key, exp
		}
	}
	return best, nil
}

// JWTExpiry returns the exp claim of a JWT without verifying its signature;
// it schedules refreshes and picks the freshest copy, never authorizes.
func JWTExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}, errors.New("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("parse JWT claims: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, errors.New("JWT has no exp claim")
	}
	return time.Unix(claims.Exp, 0), nil
}
