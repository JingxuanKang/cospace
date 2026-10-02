// Package power holds the Mac awake while any space is running.
package power

import (
	"os"
	"os/exec"
	"strconv"
	"sync"
)

type Keeper struct {
	// Start launches the keep-awake process and returns its stop func.
	// Defaults to `caffeinate -ims` (idle, disk, system sleep on AC).
	Start func() (stop func(), err error)

	mu   sync.Mutex
	stop func()
}

// defaultStart runs caffeinate bound to this process (-w): if the daemon
// dies without cleaning up (kill -9 in development, a panic), caffeinate
// exits with it instead of pinning the Mac awake forever.
func defaultStart() (func(), error) {
	cmd := exec.Command("caffeinate", "-ims", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go cmd.Wait()
	return func() { cmd.Process.Kill() }, nil
}

// Reconcile is idempotent: it keeps exactly one keep-awake process while
// anyAwake is true and none otherwise.
func (k *Keeper) Reconcile(anyAwake bool) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	switch {
	case anyAwake && k.stop == nil:
		start := k.Start
		if start == nil {
			start = defaultStart
		}
		stop, err := start()
		if err != nil {
			return err
		}
		k.stop = stop
	case !anyAwake && k.stop != nil:
		k.stop()
		k.stop = nil
	}
	return nil
}
