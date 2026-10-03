package main

import (
	"fmt"
	"io"
	"runtime"
)

// version is the build version, injected at release time with
// -ldflags "-X main.version=<VERSION>". Development builds report "dev".
var version = "dev"

func runVersion(stdout io.Writer) error {
	_, err := fmt.Fprintf(stdout, "cospaced %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
	return err
}
