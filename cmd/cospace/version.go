package main

import (
	"fmt"
	"io"
	"runtime"

	"github.com/JingxuanKang/cospace/internal/dist"
)

// version is the build version, injected at release time with
// -ldflags "-X main.version=<VERSION>". Development builds report "dev".
var version = "dev"

// pairProtocol is the version of the /pair wire format this build speaks.
// The daemon rejects a pair attempt whose protocol it does not support with
// 426 and the range it accepts, so an outdated guest tool gets a clear
// "re-run the installer" message instead of a generic rejection. Bump it only
// when the request or response shape changes incompatibly.
const pairProtocol = 1

func runVersion(stdout io.Writer) error {
	_, err := fmt.Fprintf(stdout, "cospace %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
	return err
}

// installCommand is the OS-appropriate installer line. Re-running it on a
// machine that already has cospace only updates when a newer build exists.
func installCommand() string {
	if runtime.GOOS == "windows" {
		return dist.GuestInstallWindows
	}
	return dist.GuestInstallPOSIX
}
