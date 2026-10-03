package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func TestServiceFilesKeepServeArgs(t *testing.T) {
	args := []string{"-idle-timeout", "0", "-console-hosts", "space.example.com", "-openai-upstream", `https://x/v1?a=1&b="2" $3\4`}
	plist := launchdPlist(append([]string{"/opt/homebrew/bin/cospaced", "serve"}, args...), "/usr/bin", "/tmp/cospaced.log")
	if got := launchdServeArgs(plist); !reflect.DeepEqual(got, args) {
		t.Fatalf("launchd args = %q, want %q", got, args)
	}
	unit := systemdUnitFile(append([]string{"/home/me/.local/bin/cospaced", "serve"}, args...))
	if got := systemdServeArgs(unit); !reflect.DeepEqual(got, args) {
		t.Fatalf("systemd args = %q, want %q", got, args)
	}
	if got := launchdServeArgs([]byte("not a plist")); got != nil {
		t.Fatalf("garbage plist gave %q", got)
	}
	if got := systemdServeArgs([]byte("[Service]\nExecStart=/x/cospaced serve\n")); len(got) != 0 {
		t.Fatalf("service without options gave %q", got)
	}
}

func TestSetupServeArgs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cospaced.service")
	prev := []string{"-console-hosts", "space.example.com"}
	if err := os.WriteFile(path, systemdUnitFile(append([]string{"/x/cospaced", "serve"}, prev...)), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	got, err := setupServeArgs(nil, false, path, systemdServeArgs, &out)
	if err != nil || !reflect.DeepEqual(got, prev) || !strings.Contains(out.String(), "Keeping") {
		t.Fatalf("no options should keep the installed ones: %q %v %q", got, err, out.String())
	}
	want := []string{"-idle-timeout", "0"}
	if got, err := setupServeArgs(want, false, path, systemdServeArgs, &out); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit options should win: %q %v", got, err)
	}
	if got, err := setupServeArgs(nil, true, path, systemdServeArgs, &out); err != nil || len(got) != 0 {
		t.Fatalf("-reset should install the defaults: %q %v", got, err)
	}
	if got, err := setupServeArgs(nil, false, filepath.Join(dir, "missing"), systemdServeArgs, &out); err != nil || len(got) != 0 {
		t.Fatalf("fresh install: %q %v", got, err)
	}
	for _, bad := range [][]string{{"-no-open"}, {"-idle-timeout", "-1s"}, {"extra"}, {"-openai-upstream", "::"}} {
		if _, err := setupServeArgs(bad, false, path, systemdServeArgs, &out); err == nil {
			t.Fatalf("%q should be refused before the service is written", bad)
		}
	}
	if _, err := setupServeArgs([]string{"-no-open"}, false, path, systemdServeArgs, &out); err == nil || !strings.Contains(err.Error(), "-no-open") {
		t.Fatalf("error should name the option: %v", err)
	}
}

func TestSystemdSplitRoundTrip(t *testing.T) {
	in := []string{"/x/cospaced", "serve", "plain", "has space", `q"uote`, `back\slash`, "$dollar", ""}
	quoted := make([]string, len(in))
	for i, a := range in {
		quoted[i] = systemdQuote(a)
	}
	if got := systemdSplit(strings.Join(quoted, " ")); !reflect.DeepEqual(got, in) {
		t.Fatalf("split = %q, want %q", got, in)
	}
	if got := tailLines("a\nb\nc\n", 2); got != "b\nc" {
		t.Fatalf("tailLines = %q", got)
	}
}
