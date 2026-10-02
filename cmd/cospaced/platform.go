package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/JingxuanKang/cospace/internal/container"
)

// defaultRuntime is the container engine for this OS. Apple container is the
// only runtime on macOS; Linux hosts run spaces in Docker.
func defaultRuntime() string {
	if runtime.GOOS == "linux" {
		return "docker"
	}
	return "apple"
}

// validateRuntime refuses engine/OS combinations the product does not
// support: Docker on a Mac is explicitly out (Apple container is free, signed
// and VM-isolated there), and Apple container only exists on macOS.
func validateRuntime(cfg config) error {
	switch cfg.runtime {
	case "apple":
		if runtime.GOOS != "darwin" {
			return errors.New("the apple runtime only exists on macOS; use -runtime docker on Linux")
		}
	case "docker":
		if runtime.GOOS == "darwin" {
			return errors.New("the docker runtime is not supported on macOS: CoSpace uses Apple container there")
		}
	default:
		return fmt.Errorf("unknown runtime %q (want apple or docker)", cfg.runtime)
	}
	return nil
}

func newRuntime(cfg config) container.Runtime {
	if cfg.runtime == "docker" {
		return container.Docker{}
	}
	return container.Client{R: container.CLI{}}
}

// resolveGatewayURL keeps an explicit -gateway-url, and otherwise, on
// Docker, asks the engine for the bridge gateway address so the default
// matches a non-standard bridge subnet.
func resolveGatewayURL(cfg config, stderr io.Writer) string {
	if cfg.runtime != "docker" || cfg.gatewayURL != dockerGatewayURL {
		return cfg.gatewayURL
	}
	gw, err := container.Docker{}.BridgeGateway()
	if err != nil {
		fmt.Fprintf(stderr, "gateway URL: keeping %s (%v)\n", cfg.gatewayURL, err)
		return cfg.gatewayURL
	}
	u, _ := url.Parse(cfg.gatewayURL)
	u.Host = net.JoinHostPort(gw, u.Port())
	return u.String()
}

// spaceDialer picks how the daemon reaches a space's SSH port. On macOS the
// long-lived daemon may predate the vmnet bridge and cannot route to it, so a
// freshly spawned `nc` does the dialing (see ncDial). Linux has no such
// problem: the docker bridge is an ordinary interface.
func spaceDialer(cfg config) func(network, address string) (net.Conn, error) {
	if cfg.runtime == "docker" {
		return net.Dial
	}
	return ncDial
}

// hostMemoryGB returns the host's physical memory in GB (0 on failure).
func hostMemoryGB() int {
	if runtime.GOOS == "linux" {
		total, _ := linuxMemInfo()
		return int(total / (1024 * 1024 * 1024))
	}
	out, err := execOutput("sysctl", "-n", "hw.memsize")
	if err != nil {
		return 0
	}
	bytes, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0
	}
	return int(bytes / (1024 * 1024 * 1024))
}

// hostMemoryUsedGB approximates memory in use. macOS: active + wired +
// compressor pages (Activity Monitor's "Memory Used"); Linux: total minus
// MemAvailable. 0 on failure.
func hostMemoryUsedGB() int {
	if runtime.GOOS == "linux" {
		total, avail := linuxMemInfo()
		if total == 0 {
			return 0
		}
		return int((total - avail) / (1024 * 1024 * 1024))
	}
	out, err := execOutput("vm_stat")
	if err != nil {
		return 0
	}
	pageSize := 16384
	var pages int64
	for _, line := range strings.Split(out, "\n") {
		if _, err := fmt.Sscanf(line, "Mach Virtual Memory Statistics: (page size of %d bytes)", &pageSize); err == nil {
			continue
		}
		for _, key := range []string{"Pages active:", "Pages wired down:", "Pages occupied by compressor:"} {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), key); ok {
				var n int64
				if _, err := fmt.Sscanf(strings.TrimSuffix(strings.TrimSpace(rest), "."), "%d", &n); err == nil {
					pages += n
				}
			}
		}
	}
	return int(pages * int64(pageSize) / (1024 * 1024 * 1024))
}

// linuxMemInfo reads MemTotal and MemAvailable (bytes) from /proc/meminfo.
func linuxMemInfo() (total, available int64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			available = kb * 1024
		}
	}
	return total, available
}

// onBattery reports whether the host is running on battery power. Servers
// without a battery report false.
func onBattery() bool {
	if runtime.GOOS == "linux" {
		matches, _ := filepath.Glob("/sys/class/power_supply/*/status")
		for _, m := range matches {
			if b, err := os.ReadFile(m); err == nil && strings.TrimSpace(string(b)) == "Discharging" {
				return true
			}
		}
		return false
	}
	out, err := execOutput("pmset", "-g", "batt")
	if err != nil {
		return false
	}
	return strings.Contains(out, "Battery Power")
}
