package gateway

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFileCredsReadsLiveToken(t *testing.T) {
	dir := t.TempDir()
	claude := writeFile(t, dir, "claude.json", `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-AAA","expiresAt":123}}`)
	codex := writeFile(t, dir, "codex.json", `{"tokens":{"access_token":"eyJ-BBB"}}`)
	grok := writeFile(t, dir, "grok.json", `{"https://auth.x.ai::uuid-1":{"key":"eyJ-CCC","refresh_token":"r","auth_mode":"oidc"}}`)
	fc := NewFileCreds(map[string]string{"anthropic": claude, "openai": codex, "xai": grok})

	if h, ok := fc.Get("anthropic"); !ok || h != "Bearer sk-ant-oat01-AAA" {
		t.Fatalf("anthropic = %q %v", h, ok)
	}
	if h, ok := fc.Get("openai"); !ok || h != "Bearer eyJ-BBB" {
		t.Fatalf("openai = %q %v", h, ok)
	}
	if h, ok := fc.Get("xai"); !ok || h != "Bearer eyJ-CCC" {
		t.Fatalf("xai = %q %v", h, ok)
	}
	if _, ok := fc.Get("unknown"); ok {
		t.Fatal("unknown provider must not resolve")
	}
}

func TestFileCredsPicksUpRefresh(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "claude.json", `{"claudeAiOauth":{"accessToken":"OLD"}}`)
	fc := NewFileCreds(map[string]string{"anthropic": p})
	if h, _ := fc.Get("anthropic"); h != "Bearer OLD" {
		t.Fatalf("got %q", h)
	}
	// simulate the CLI refreshing the token (new content + newer mtime)
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(p, []byte(`{"claudeAiOauth":{"accessToken":"NEW"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, time.Now().Add(time.Second), time.Now().Add(time.Second))
	if h, _ := fc.Get("anthropic"); h != "Bearer NEW" {
		t.Fatalf("did not pick up refresh, got %q", h)
	}
}

func TestFileCredsToggle(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "claude.json", `{"claudeAiOauth":{"accessToken":"AAA"}}`)
	on := true
	fc := NewFileCreds(map[string]string{"anthropic": p})
	fc.Enabled = func(string) bool { return on }
	if _, ok := fc.Get("anthropic"); !ok {
		t.Fatal("want enabled")
	}
	on = false
	if _, ok := fc.Get("anthropic"); ok {
		t.Fatal("disabled toggle must withhold the credential")
	}
}

func TestFileCredsMissingFile(t *testing.T) {
	fc := NewFileCreds(map[string]string{"anthropic": "/no/such/file"})
	if _, ok := fc.Get("anthropic"); ok {
		t.Fatal("missing file must not resolve")
	}
}
