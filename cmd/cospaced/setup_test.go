package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchdPlistIsValidAndEscaped(t *testing.T) {
	plist := launchdPlist([]string{"/opt/homebrew/bin/cospaced", "serve", "-idle-timeout", "0", "-x", "a&b<c"},
		"/opt/homebrew/bin:/usr/bin", "/Users/me/Library/Logs/CoSpace/cospaced.log")
	text := string(plist)
	for _, want := range []string{"<string>dev.cospace.daemon</string>", "<string>serve</string>", "<string>a&amp;b&lt;c</string>", "<key>KeepAlive</key><true/>"} {
		if !strings.Contains(text, want) {
			t.Fatalf("plist missing %q:\n%s", want, text)
		}
	}
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("plutil not available")
	}
	path := filepath.Join(t.TempDir(), "test.plist")
	if err := os.WriteFile(path, plist, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v: %s", err, out)
	}
}

func TestUniqKeepsOrderAndDropsEmpty(t *testing.T) {
	got := strings.Join(uniq([]string{"/a", "", "/b", "/a", "/c"}), ":")
	if got != "/a:/b:/c" {
		t.Fatalf("uniq = %q", got)
	}
}
