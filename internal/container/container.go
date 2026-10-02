// Package container wraps the Apple `container` CLI behind a small interface
// so the rest of the daemon can be tested without a real VM runtime.
package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Runner executes the container CLI. Implemented by CLI for production and by
// fakes in tests.
type Runner interface {
	Run(args ...string) (string, error)
}

// CLI shells out to the real `container` binary.
type CLI struct {
	Bin string // defaults to "container" when empty
}

func (c CLI) Run(args ...string) (string, error) {
	bin := c.Bin
	if bin == "" {
		bin = "container"
	}
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("container %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// RunInput sends generated files over stdin rather than a shell argument.
// Model catalogs can exceed Linux's per-argument size limit.
func (c CLI) RunInput(input string, args ...string) (string, error) {
	bin := c.Bin
	if bin == "" {
		bin = "container"
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("container %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Client provides the container operations cospaced needs.
type Client struct {
	R Runner
}

// RunSpec describes a new space container.
type RunSpec struct {
	Name     string
	Image    string
	MemoryGB int
	CPUs     int
	Mounts   []Mount // host -> guest bind mounts
	Env      map[string]string
}

type Mount struct{ Host, Guest string }

func (c Client) RunDetached(s RunSpec) error {
	args := []string{"run", "-d", "--name", s.Name,
		"--cap-add", "CAP_NET_ADMIN",
		"--memory", fmt.Sprintf("%dg", s.MemoryGB),
		"--cpus", fmt.Sprintf("%d", s.CPUs)}
	for _, m := range s.Mounts {
		args = append(args, "-v", m.Host+":"+m.Guest)
	}
	for k, v := range s.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, s.Image)
	_, err := c.R.Run(args...)
	return err
}

// ErrNotFound reports that the runtime has no container by that name. Apple
// container exits 1 for every failure, so callers must not read exit codes;
// Stop/Delete re-check the listing and callers can errors.Is against this.
var ErrNotFound = errors.New("container not found")

func (c Client) Start(name string) error {
	_, err := c.R.Run("start", name)
	if err != nil {
		if found, listErr := c.Exists(name); listErr == nil && !found {
			return fmt.Errorf("%w: %s", ErrNotFound, name)
		}
	}
	return err
}

// Stop is idempotent: a container that is already stopped, or that no longer
// exists at all (deleted outside CoSpace), counts as stopped.
func (c Client) Stop(name string) error {
	_, err := c.R.Run("stop", name)
	if err != nil {
		// Apple container may report a closing RPC error after the VM has stopped.
		if all, listErr := c.List(); listErr == nil {
			for _, st := range all {
				if st.Name == name {
					if st.State == "stopped" {
						return nil
					}
					return err
				}
			}
			return nil // gone entirely: nothing left to stop
		}
	}
	return err
}

// Exists reports whether the runtime knows a container by that name.
func (c Client) Exists(name string) (bool, error) {
	all, err := c.List()
	if err != nil {
		return false, err
	}
	for _, st := range all {
		if st.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// WaitReady handles the short gap between VM start and a usable exec channel.
func (c Client) WaitReady(name string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := c.Exec(name, ":")
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Delete is idempotent: a container that was already removed (by hand, by a
// runtime reset, or by a half-finished upgrade) is a successful delete, so the
// daemon can always purge the space record that pointed at it.
func (c Client) Delete(name string) error {
	_, err := c.R.Run("delete", "--force", name)
	if err != nil {
		if found, listErr := c.Exists(name); listErr == nil && !found {
			return nil
		}
		if isNotFound(err) {
			return nil
		}
	}
	return err
}

func isNotFound(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "notfound") || strings.Contains(msg, "not found")
}

// Exec runs a shell command inside a running container.
func (c Client) Exec(name, script string) (string, error) {
	if r, ok := c.R.(interface {
		RunInput(string, ...string) (string, error)
	}); ok {
		return r.RunInput(script, "exec", "-i", name, "sh")
	}
	return c.R.Run("exec", name, "sh", "-c", script)
}

// Info is the subset of `container list --format json` cospaced reads.
type Info struct {
	Name  string
	State string
	IP    string // empty when not running
}

// List returns all containers known to the runtime.
func (c Client) List() ([]Info, error) {
	out, err := c.R.Run("list", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Status struct {
			State    string `json:"state"`
			Networks []struct {
				IPv4Address string `json:"ipv4Address"`
			} `json:"networks"`
		} `json:"status"`
		Configuration struct {
			ID string `json:"id"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse container list: %w", err)
	}
	infos := make([]Info, 0, len(raw))
	for _, r := range raw {
		in := Info{Name: r.Configuration.ID, State: r.Status.State}
		if len(r.Status.Networks) > 0 {
			in.IP = strings.SplitN(r.Status.Networks[0].IPv4Address, "/", 2)[0]
		}
		infos = append(infos, in)
	}
	return infos, nil
}

// IP returns the current address of a running container. Container IPs change
// across restarts, so callers must not cache the result.
func (c Client) IP(name string) (string, error) {
	infos, err := c.List()
	if err != nil {
		return "", err
	}
	for _, in := range infos {
		if in.Name == name {
			if in.IP == "" {
				return "", fmt.Errorf("container %s has no address (state %s)", name, in.State)
			}
			return in.IP, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNotFound, name)
}
