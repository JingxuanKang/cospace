package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// setupServeArgs decides which daemon options the service gets. Explicit
// options win; with none, an existing service keeps its own, because an
// upgrade run of host.sh passes nothing and must not silently drop
// -console-hosts and friends; -reset installs the defaults. The result is
// parsed the way the daemon parses it, so a bad option fails setup here
// instead of leaving launchd or systemd restarting a daemon that exits at
// once.
func setupServeArgs(given []string, reset bool, servicePath string, previous func([]byte) []string, out io.Writer) ([]string, error) {
	args := given
	if len(args) == 0 && !reset {
		if b, err := os.ReadFile(servicePath); err == nil {
			if args = previous(b); len(args) > 0 {
				fmt.Fprintf(out, "Keeping the installed service's options: %s\n", strings.Join(args, " "))
			}
		}
	}
	if err := validateServeArgs(args); err != nil {
		return nil, fmt.Errorf("setup: the daemon would not start with these options (%w); options after -- go to  cospaced serve", err)
	}
	return args, nil
}

// serveFlagsOf returns the arguments after "serve" in a service command line.
func serveFlagsOf(programArgs []string) []string {
	for i, a := range programArgs {
		if a == "serve" {
			return programArgs[i+1:]
		}
	}
	return nil
}

// launchdServeArgs reads the daemon options back from a plist written by
// launchdPlist.
func launchdServeArgs(plist []byte) []string {
	dec := xml.NewDecoder(bytes.NewReader(plist))
	var key string
	var args []string
	inArgs := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "key":
				if err := dec.DecodeElement(&key, &t); err != nil {
					return nil
				}
			case "array":
				inArgs = key == "ProgramArguments"
			case "string":
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return nil
				}
				if inArgs {
					args = append(args, s)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "array" && inArgs {
				return serveFlagsOf(args)
			}
		}
	}
}

// systemdServeArgs reads the daemon options back from a unit written by
// systemdUnitFile.
func systemdServeArgs(unit []byte) []string {
	for _, line := range strings.Split(string(unit), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart="); ok {
			return serveFlagsOf(systemdSplit(v))
		}
	}
	return nil
}

// systemdSplit splits an ExecStart line quoted by systemdQuote back into
// arguments.
func systemdSplit(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote, have := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQuote && c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case inQuote && c == '$' && i+1 < len(s) && s[i+1] == '$':
			i++
			cur.WriteByte('$')
		case c == '"':
			inQuote = !inQuote
			have = true
		case !inQuote && (c == ' ' || c == '\t'):
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

// tailLines returns the last n lines of text.
func tailLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func tailFile(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return tailLines(string(b), n)
}

var launchdExitRe = regexp.MustCompile(`last exit code = (\S+)`)

// launchdCrashed reports whether launchd saw the daemon exit with an error,
// which after a fresh bootstrap means it cannot start at all.
func launchdCrashed(domain string) (string, bool) {
	b, _ := exec.Command("launchctl", "print", domain+"/"+launchLabel).CombinedOutput()
	m := launchdExitRe.FindStringSubmatch(string(b))
	if m == nil || m[1] == "0" || strings.HasPrefix(m[1], "(") {
		return "", false
	}
	return m[1], true
}

// systemdCrashed reports whether systemd has already had to restart the
// daemon since it was started.
func systemdCrashed() (string, bool) {
	b, err := exec.Command("systemctl", "--user", "show", "-p", "NRestarts", "--value", systemdUnit).Output()
	if err != nil {
		return "", false
	}
	n := strings.TrimSpace(string(b))
	if n == "" || n == "0" {
		return "", false
	}
	return n, true
}

func journalTail(n int) string {
	b, _ := exec.Command("journalctl", "--user", "-u", systemdUnit, "-n", fmt.Sprint(n), "--no-pager", "-o", "cat").CombinedOutput()
	return strings.TrimSpace(string(b))
}
