package container

import (
	"bufio"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Docker drives a Docker-compatible engine (docker or podman) on a Linux
// host. It is the Linux counterpart of Client: one container per space, the
// same image, CAP_NET_ADMIN for the in-VM firewall, the workspace bind-mounted
// at /workspace. It is deliberately not offered on macOS: Apple container is
// the only runtime there.
type Docker struct {
	Bin string // defaults to "docker"
}

func (d Docker) bin() string {
	if d.Bin == "" {
		return "docker"
	}
	return d.Bin
}

func (d Docker) run(args ...string) (string, error) {
	out, err := exec.Command(d.bin(), args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", d.bin(), strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (d Docker) RunDetached(s RunSpec) error {
	args := []string{"run", "-d", "--name", s.Name,
		"--cap-add", "NET_ADMIN",
		"--restart", "no", // the daemon reconciles power state; the engine must not race it after a reboot
		"--memory", fmt.Sprintf("%dg", s.MemoryGB),
		"--cpus", fmt.Sprintf("%d", s.CPUs)}
	for _, m := range s.Mounts {
		args = append(args, "-v", m.Host+":"+m.Guest)
	}
	for k, v := range s.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, s.Image)
	_, err := d.run(args...)
	return err
}

func (d Docker) Start(name string) error {
	_, err := d.run("start", name)
	if err != nil {
		if found, listErr := d.Exists(name); listErr == nil && !found {
			return fmt.Errorf("%w: %s", ErrNotFound, name)
		}
	}
	return err
}

func (d Docker) Stop(name string) error {
	_, err := d.run("stop", name)
	if err != nil {
		if found, listErr := d.Exists(name); listErr == nil && !found {
			return nil
		}
	}
	return err
}

func (d Docker) Delete(name string) error {
	_, err := d.run("rm", "-f", name)
	if err != nil {
		if found, listErr := d.Exists(name); listErr == nil && !found {
			return nil
		}
		if isNotFound(err) || strings.Contains(strings.ToLower(err.Error()), "no such container") {
			return nil
		}
	}
	return err
}

func (d Docker) Exists(name string) (bool, error) {
	all, err := d.List()
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

func (d Docker) WaitReady(name string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := d.Exec(name, ":")
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Exec runs a shell script inside a running container, delivered on stdin
// so size never hits an argv limit.
func (d Docker) Exec(name, script string) (string, error) {
	cmd := exec.Command(d.bin(), "exec", "-i", name, "sh")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s exec %s: %w: %s", d.bin(), name, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// List reads every container with one `docker ps -a` and resolves the
// addresses of the running ones with one `docker inspect`.
func (d Docker) List() ([]Info, error) {
	out, err := d.run("ps", "-a", "--format", "{{.Names}}\t{{.State}}")
	if err != nil {
		return nil, err
	}
	infos := parseDockerPS(out)
	var running []string
	for _, in := range infos {
		if in.State == "running" {
			running = append(running, in.Name)
		}
	}
	if len(running) == 0 {
		return infos, nil
	}
	args := append([]string{"inspect", "-f", `{{.Name}} {{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}`}, running...)
	out, err = d.run(args...)
	if err != nil {
		return infos, nil // addresses are best-effort; state is what matters
	}
	ips := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			ips[strings.TrimPrefix(fields[0], "/")] = fields[1]
		}
	}
	for i := range infos {
		infos[i].IP = ips[infos[i].Name]
	}
	return infos, nil
}

// parseDockerPS maps docker's container states onto the two the daemon uses.
func parseDockerPS(out string) []Info {
	var infos []Info
	for _, line := range strings.Split(out, "\n") {
		name, state, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || name == "" {
			continue
		}
		st := "stopped"
		if strings.EqualFold(strings.TrimSpace(state), "running") {
			st = "running"
		}
		infos = append(infos, Info{Name: name, State: st})
	}
	return infos
}

func (d Docker) IP(name string) (string, error) {
	infos, err := d.List()
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

func (d Docker) ImageExists(ref string) (bool, error) {
	_, err := d.run("image", "inspect", ref)
	if err == nil {
		return true, nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "no such image") || isNotFound(err) {
		return false, nil
	}
	return false, err
}

// Pull downloads ref for the host's own architecture. Docker's progress
// lines are per layer, so only coarse phases are reported.
func (d Docker) Pull(ref string, report func(PullProgress)) error {
	cmd := exec.Command(d.bin(), "pull", ref)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s pull: %w", d.bin(), err)
	}
	if report != nil {
		report(PullProgress{Phase: "fetching", Percent: -1})
	}
	var last string
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			last = line
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s pull %s: %w: %s", d.bin(), ref, err, last)
	}
	if report != nil {
		report(PullProgress{Phase: "unpacking", Percent: 100})
	}
	return nil
}

func (d Docker) Export(name, archive string) error {
	_, err := d.run("export", "-o", archive, name)
	return err
}

func (d Docker) Build(tag, dir string) error {
	_, err := d.run("build", "--tag", tag, dir)
	return err
}

// BridgeGateway returns the address containers use to reach the host on the
// default bridge network (172.17.0.1 unless the engine is configured
// otherwise). That is where spaces find the credential gateway on Linux.
func (d Docker) BridgeGateway() (string, error) {
	out, err := d.run("network", "inspect", "bridge", "-f", "{{(index .IPAM.Config 0).Gateway}}")
	if err != nil {
		return "", err
	}
	gw := strings.TrimSpace(out)
	if gw == "" {
		return "", errors.New("docker bridge network has no gateway address")
	}
	return gw, nil
}

// Available reports whether the engine answers; the error explains what to
// install or which group the user is missing.
func (d Docker) Available() error {
	if _, err := exec.LookPath(d.bin()); err != nil {
		return fmt.Errorf("%s is not installed; install Docker Engine (or podman with the docker CLI shim) and run again", d.bin())
	}
	if _, err := d.run("info", "--format", "{{.ServerVersion}}"); err != nil {
		return fmt.Errorf("%s is installed but not usable: %v (is the daemon running, and is this user in the docker group?)", d.bin(), err)
	}
	return nil
}

var _ Runtime = Docker{}
