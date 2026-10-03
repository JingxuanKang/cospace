package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/JingxuanKang/cospace/internal/container"
)

const systemdUnit = "cospaced.service"

// runSetupLinux installs cospaced as a per-user systemd service on a Linux
// host with Docker. Arguments after "--" are passed through to `cospaced
// serve`. The unit runs as the invoking user, who must be in the docker
// group; lingering is enabled so the service outlives the login session.
func runSetupLinux(serveArgs []string, noOpen, reset bool, stdout io.Writer) error {
	if err := (container.Docker{}).Available(); err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("setup: systemd is required to install the service; run  cospaced serve  under your own supervisor instead")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	unitPath := filepath.Join(unitDir, systemdUnit)
	prev, _ := os.ReadFile(unitPath) // nil on a fresh install
	serveArgs, err = setupServeArgs(serveArgs, reset, unitPath, systemdServeArgs, stdout)
	if err != nil {
		return err
	}
	if err := os.WriteFile(unitPath, systemdUnitFile(append([]string{exe, "serve"}, serveArgs...)), 0o644); err != nil {
		return err
	}
	// restart, not enable --now: an upgrade replaced the binary and may have
	// changed the options, and --now leaves a running service as it is.
	steps := [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", systemdUnit},
		{"systemctl", "--user", "restart", systemdUnit},
	}
	for _, step := range steps {
		if b, err := exec.Command(step[0], step[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", strings.Join(step, " "), err, strings.TrimSpace(string(b)))
		}
	}
	// Best effort: without lingering, systemd stops user services at logout.
	if b, err := exec.Command("loginctl", "enable-linger").CombinedOutput(); err != nil {
		fmt.Fprintf(stdout, "Note: could not enable lingering (%s); run  sudo loginctl enable-linger %s  so the daemon survives logout.\n", strings.TrimSpace(string(b)), os.Getenv("USER"))
	}
	fmt.Fprintf(stdout, "CoSpace daemon installed as a user service (logs: journalctl --user -u %s).\n", systemdUnit)
	if !waitForConsole(waitConsoleTimeout) {
		if n, crashed := systemdCrashed(); crashed {
			tail := journalTail(8)
			return fmt.Errorf("setup: the daemon exits right after starting (%s restarts); %s. journalctl --user -u %s:\n%s", n, rollbackSystemd(unitPath, prev), systemdUnit, tail)
		}
		fmt.Fprintf(stdout, "The console is not answering yet — check  journalctl --user -u %s , then open %s\n", systemdUnit, consoleURL)
		return nil
	}
	fmt.Fprintf(stdout, "Console: %s (loopback only; reach it over an SSH tunnel or Tailscale)\n", consoleURL)
	fmt.Fprintln(stdout, "The space image downloads in the background on first run; the console shows its progress.")
	if !noOpen {
		exec.Command("xdg-open", consoleURL).Run()
	}
	return nil
}

func runUninstallLinux(cfg config, stdout io.Writer) error {
	exec.Command("systemctl", "--user", "disable", "--now", systemdUnit).Run()
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	unitPath := filepath.Join(home, ".config", "systemd", "user", systemdUnit)
	if err := os.Remove(unitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Fprintf(stdout, "CoSpace daemon stopped and removed.\nSpaces and data are kept in %s; existing space containers are listed by  docker ps -a.\n", cfg.data)
	return nil
}

// rollbackSystemd undoes an install whose daemon cannot start: the previous
// service comes back, or a fresh one is removed. It returns what it did for
// the error message.
func rollbackSystemd(unitPath string, prev []byte) string {
	exec.Command("systemctl", "--user", "stop", systemdUnit).Run()
	if prev == nil {
		exec.Command("systemctl", "--user", "disable", systemdUnit).Run()
		os.Remove(unitPath)
		exec.Command("systemctl", "--user", "daemon-reload").Run()
		return "the service was removed"
	}
	os.WriteFile(unitPath, prev, 0o644)
	exec.Command("systemctl", "--user", "daemon-reload").Run()
	exec.Command("systemctl", "--user", "restart", systemdUnit).Run()
	return "the previous service was restored"
}

func systemdUnitFile(programArgs []string) []byte {
	quoted := make([]string, len(programArgs))
	for i, a := range programArgs {
		quoted[i] = systemdQuote(a)
	}
	return []byte(fmt.Sprintf(`[Unit]
Description=CoSpace daemon (credential gateway, console, space lifecycle)
After=network-online.target docker.service
Wants=network-online.target

[Service]
ExecStart=%s
Restart=always
RestartSec=5
Environment=PATH=/usr/local/bin:/usr/bin:/bin

[Install]
WantedBy=default.target
`, strings.Join(quoted, " ")))
}

// systemdQuote quotes one ExecStart argument per systemd.service(5).
func systemdQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`).Replace(s) + `"`
}
