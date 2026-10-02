package keepalive

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// AnthropicExpiry reads the access token expiry (ms epoch) from claude's
// credential file.
func AnthropicExpiry(path string) (time.Time, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	var doc struct {
		OAuth struct {
			ExpiresAt int64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return time.Time{}, err
	}
	if doc.OAuth.ExpiresAt == 0 {
		return time.Time{}, errors.New("no expiresAt in claude credentials")
	}
	return time.UnixMilli(doc.OAuth.ExpiresAt), nil
}

// OpenAIExpiry decodes the exp claim of codex's access-token JWT.
func OpenAIExpiry(path string) (time.Time, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	var doc struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return time.Time{}, err
	}
	return JWTExpiry(doc.Tokens.AccessToken)
}

// XAIExpiry decodes the exp claim of grok's OIDC JWT. The file is keyed by a
// dynamic per-login "issuer::uuid" string; the entry expiring last is live.
func XAIExpiry(path string) (time.Time, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	var doc map[string]struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return time.Time{}, err
	}
	// A host who logged in more than once has several entries; the live one
	// is whichever expires last, never "the first the map happens to yield".
	var best time.Time
	found := false
	for _, entry := range doc {
		if entry.Key == "" {
			continue
		}
		exp, err := JWTExpiry(entry.Key)
		if err != nil {
			continue
		}
		if !found || exp.After(best) {
			best, found = exp, true
		}
	}
	if !found {
		return time.Time{}, errors.New("no key in grok credentials")
	}
	return best, nil
}

// JWTExpiry returns the exp claim of a JWT without verifying its signature —
// good enough to schedule a refresh; nothing here makes an auth decision.
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
