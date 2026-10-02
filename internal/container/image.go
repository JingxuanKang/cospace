package container

import (
	"bufio"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ImageExists reports whether ref is already in the local image store.
func (c Client) ImageExists(ref string) (bool, error) {
	_, err := c.R.Run("image", "inspect", ref)
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "not found") {
		return false, nil
	}
	return false, err
}

// PullProgress is one parsed line of `container image pull --progress plain`.
type PullProgress struct {
	Phase   string  // "fetching" or "unpacking"
	Percent int     // 0–100 within the phase; -1 when the line carries none
	DoneMB  float64 // bytes so far in the phase, when reported
	TotalMB float64
}

var (
	pullPhaseRe   = regexp.MustCompile(`^\[\d+/\d+\] (Fetching|Unpacking) image`)
	pullPercentRe = regexp.MustCompile(` (\d{1,3})% `)
	pullSizeRe    = regexp.MustCompile(`([\d.]+)/([\d.]+) (KB|MB|GB)`)
)

// ParsePullLine extracts progress from one plain-progress line.
func ParsePullLine(line string) (PullProgress, bool) {
	m := pullPhaseRe.FindStringSubmatch(line)
	if m == nil {
		return PullProgress{}, false
	}
	p := PullProgress{Phase: strings.ToLower(m[1]), Percent: -1}
	if pm := pullPercentRe.FindStringSubmatch(line); pm != nil {
		p.Percent, _ = strconv.Atoi(pm[1])
	}
	if sm := pullSizeRe.FindStringSubmatch(line); sm != nil {
		scale := map[string]float64{"KB": 1.0 / 1024, "MB": 1, "GB": 1024}[sm[3]]
		done, _ := strconv.ParseFloat(sm[1], 64)
		total, _ := strconv.ParseFloat(sm[2], 64)
		p.DoneMB, p.TotalMB = done*scale, total*scale
	}
	return p, true
}

// Pull downloads ref for the VM platform (linux/arm64), streaming parsed
// progress to report as the CLI prints it.
func (c CLI) Pull(ref string, report func(PullProgress)) error {
	bin := c.Bin
	if bin == "" {
		bin = "container"
	}
	cmd := exec.Command(bin, "image", "pull", "--progress", "plain", "--platform", "linux/arm64", ref)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("container image pull: %w", err)
	}
	var last string
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		last = line
		if p, ok := ParsePullLine(line); ok && report != nil {
			report(p)
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("container image pull %s: %w: %s", ref, err, last)
	}
	return nil
}
