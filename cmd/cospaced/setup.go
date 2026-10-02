package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/JingxuanKang/cospace/internal/container"
)

const (
	launchLabel = "dev.cospace.daemon"
	consoleURL  = "http://127.0.0.1:18931"
)

// ensureContainerSystem starts Apple container's services when they are not
// running — after a reboot they stay down, and on a fresh Mac the first start
// also installs the default Linux kernel without prompting.
func ensureContainerSystem(out io.Writer) error {
	if exec.Command("container", "system", "status").Run() == nil {
		return nil
	}
	fmt.Fprintln(out, "Starting Apple container services…")
	b, err := exec.Command("container", "system", "start", "--enable-kernel-install").CombinedOutput()
	if err != nil {
		return fmt.Errorf("container system start: %w: %s", err, strings.TrimSpace(string(b)))
	}
	return nil
}

// ensureSpaceImage pulls the base image for CLI space creation when it is not
// in the local store yet. The daemon does the same in the background.
var ensureSpaceImage = func(c container.Client, ref string, out io.Writer) error {
	ok, err := c.ImageExists(ref)
	if err != nil || ok {
		return err
	}
	fmt.Fprintf(out, "Downloading the space image %s (first run only)…\n", ref)
	last := -1
	return container.CLI{}.Pull(ref, func(p container.PullProgress) {
		if p.Percent < 0 || p.Percent/10 == last/10 && p.Percent != 100 {
			return
		}
		last = p.Percent
		fmt.Fprintf(out, "  %s %d%%\n", p.Phase, p.Percent)
	})
}

// runSetup installs cospaced as a per-user launchd service and opens the
// console. Arguments after "--" are passed through to `cospaced serve`.
func runSetup(args []string, stdout io.Writer) error {
	fs := newFlagSet("setup")
	noOpen := fs.Bool("no-open", false, "do not open the console in the browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return errors.New("setup: the CoSpace host runs on Apple Silicon Macs (macOS 26 or later)")
	}
	containerBin, err := exec.LookPath("container")
	if err != nil {
		return errors.New("setup: Apple container is not installed — run  brew install container  (or install the signed package from https://github.com/apple/container/releases), then run setup again")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if err := ensureContainerSystem(stdout); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	logDir := filepath.Join(home, "Library", "Logs", "CoSpace")
	agents := filepath.Join(home, "Library", "LaunchAgents")
	for _, d := range []string{logDir, agents} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	plistPath := filepath.Join(agents, launchLabel+".plist")
	pathEnv := strings.Join(uniq([]string{filepath.Dir(containerBin), filepath.Dir(exe),
		"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"}), ":")
	plist := launchdPlist(append([]string{exe, "serve"}, fs.Args()...), pathEnv, filepath.Join(logDir, "cospaced.log"))
	if err := os.WriteFile(plistPath, plist, 0o644); err != nil {
		return err
	}

	domain := fmt.Sprintf("gui/%d", os.Getuid())
	exec.Command("launchctl", "bootout", domain+"/"+launchLabel).Run() // fine if it was not loaded
	// bootout returns before the old instance is fully gone; bootstrap fails
	// with an I/O error until it is, so retry briefly.
	var bootErr error
	for i := 0; i < 10; i++ {
		b, err := exec.Command("launchctl", "bootstrap", domain, plistPath).CombinedOutput()
		if err == nil {
			bootErr = nil
			break
		}
		bootErr = fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(b)))
		time.Sleep(time.Second)
	}
	if bootErr != nil {
		return bootErr
	}
	fmt.Fprintf(stdout, "CoSpace daemon installed (starts at login; logs in %s).\n", logDir)

	if !waitForConsole(30 * time.Second) {
		fmt.Fprintf(stdout, "The console is not answering yet — check %s, then open %s\n", filepath.Join(logDir, "cospaced.log"), consoleURL)
		return nil
	}
	fmt.Fprintf(stdout, "Console: %s\n", consoleURL)
	fmt.Fprintln(stdout, "The space image downloads in the background on first run; the console shows its progress.")
	if !*noOpen {
		exec.Command("open", consoleURL).Run()
	}
	return nil
}

// runUninstall removes the launchd service. Spaces, images and data stay.
func runUninstall(args []string, cfg config, stdout io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("uninstall: unexpected arguments: %s", strings.Join(args, " "))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchLabel)).Run()
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist")
	if err := os.Remove(plistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Fprintf(stdout, "CoSpace daemon stopped and removed from login items.\nSpaces and data are kept in %s; existing space VMs are listed by  container list --all.\n", cfg.data)
	return nil
}

func launchdPlist(programArgs []string, pathEnv, logPath string) []byte {
	esc := func(s string) string {
		var b bytes.Buffer
		xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var argsXML strings.Builder
	for _, a := range programArgs {
		fmt.Fprintf(&argsXML, "    <string>%s</string>\n", esc(a))
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>%s</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchLabel, argsXML.String(), esc(pathEnv), esc(logPath), esc(logPath)))
}

func waitForConsole(limit time.Duration) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	for deadline := time.Now().Add(limit); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if resp, err := client.Get(consoleURL + "/api/host"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
	}
	return false
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
