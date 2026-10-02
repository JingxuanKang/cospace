package container

import "fmt"

// Runtime is everything the daemon asks of a container engine. Apple
// container (Client) is the only runtime on macOS; Docker is the runtime on
// Linux hosts. Both run the same Debian space image, so everything above this
// interface — runtime sync, pairing, the gateway — is engine-agnostic.
type Runtime interface {
	RunDetached(s RunSpec) error
	Start(name string) error
	Stop(name string) error
	Delete(name string) error
	Exists(name string) (bool, error)
	WaitReady(name string) error
	Exec(name, script string) (string, error)
	List() ([]Info, error)
	IP(name string) (string, error)
	ImageExists(ref string) (bool, error)
	Pull(ref string, report func(PullProgress)) error
	// Export writes a container's root filesystem as a tar archive, and Build
	// builds an image from a directory holding a Dockerfile; together they
	// back the network-controls upgrade of legacy VMs.
	Export(name, archive string) error
	Build(tag, dir string) error
}

// Export snapshots a container's root filesystem with Apple container.
func (c Client) Export(name, archive string) error {
	_, err := c.R.Run("export", "--output", archive, name)
	return err
}

// Build builds an image from dir with Apple container.
func (c Client) Build(tag, dir string) error {
	_, err := c.R.Run("build", "--cpus", "2", "--memory", "1g", "--progress", "plain", "--tag", tag, dir)
	return err
}

// Pull downloads an image through the Runner when it knows how (the real CLI
// does); test fakes that cannot pull report so instead of pretending.
func (c Client) Pull(ref string, report func(PullProgress)) error {
	if p, ok := c.R.(interface {
		Pull(ref string, report func(PullProgress)) error
	}); ok {
		return p.Pull(ref, report)
	}
	return fmt.Errorf("this container runner cannot pull images")
}

var _ Runtime = Client{}
