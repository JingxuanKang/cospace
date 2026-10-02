package spaces

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func (m *Manager) loadForUpdate(name string) (*Space, error) {
	if m.upgrading[name] {
		return nil, fmt.Errorf("%w: space is upgrading network controls; try again after it finishes", ErrConflict)
	}
	return m.load(name)
}

// NetworkUpgradeState stays visible across console refreshes while a snapshot runs.
func (m *Manager) NetworkUpgradeState(name string) (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.upgrading[name], m.upgradeErrors[name]
}

func (m *Manager) beginNetworkUpgrade(name string) (*Space, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.loadForUpdate(name)
	if err != nil {
		return nil, err
	}
	if r.NetworkControlVersion >= 1 {
		return nil, nil
	}
	if m.upgrading == nil {
		m.upgrading = map[string]bool{}
	}
	if m.upgradeErrors == nil {
		m.upgradeErrors = map[string]string{}
	}
	m.upgrading[name] = true
	delete(m.upgradeErrors, name)
	return r, nil
}

// StartNetworkUpgrade returns before the filesystem snapshot/build completes.
// Status is exposed separately so reverse-proxy timeouts cannot interrupt the UI.
func (m *Manager) StartNetworkUpgrade(name string) error {
	r, err := m.beginNetworkUpgrade(name)
	if err != nil || r == nil {
		return err
	}
	go func() { _ = m.upgradeNetwork(r) }()
	return nil
}

// UpgradeNetwork is the synchronous variant used by integration checks.
func (m *Manager) UpgradeNetwork(name string) error {
	r, err := m.beginNetworkUpgrade(name)
	if err != nil || r == nil {
		return err
	}
	return m.upgradeNetwork(r)
}

// upgradeNetwork snapshots the complete root filesystem before replacing the
// immutable VM capabilities. The workspace mount and SSH identity are preserved.
func (m *Manager) upgradeNetwork(r *Space) (err error) {
	name := r.Name
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.upgrading, name)
		if err != nil {
			m.upgradeErrors[name] = err.Error()
		}
	}()

	state, found, err := m.containerState(name)
	if err != nil {
		return err
	}
	if !found {
		// An earlier attempt deleted the old VM and died before recreating it.
		// Nothing is left to snapshot; rebuild from the image it recorded.
		if r.RecoveryImage == "" {
			return fmt.Errorf("%w: %s has no VM to snapshot; delete the space or recreate it", ErrVMMissing, name)
		}
		return m.finishUpgrade(r, r.RecoveryImage, "", false)
	}
	wasRunning := state == "running"
	stamp := time.Now().UTC().Format("20060102-150405")
	backup := filepath.Join(m.Dir, "backups", "network", name+"-"+stamp)
	if err := os.MkdirAll(backup, 0700); err != nil {
		return err
	}
	archive := filepath.Join(backup, "rootfs.tar")
	f, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	f.Close()
	if wasRunning {
		if err := m.C.Stop(name); err != nil {
			return err
		}
	}
	deleted := false
	defer func() {
		if err != nil && !deleted && wasRunning {
			_ = m.C.Start(name)
		}
	}()
	if _, err = m.C.R.Run("export", "--output", archive, name); err != nil {
		return fmt.Errorf("snapshot space: %w", err)
	}
	fi, err := os.Stat(archive)
	if err != nil || fi.Size() < 1024 {
		return fmt.Errorf("space snapshot is missing or empty; original container retained")
	}
	entry := "#!/bin/sh\nset -eu\nif [ -f /etc/cospace-network.nft ]; then nft -f /etc/cospace-network.nft; fi\nexec /usr/sbin/sshd -D -e\n"
	if err = os.WriteFile(filepath.Join(backup, "entrypoint.sh"), []byte(entry), 0700); err != nil {
		return err
	}
	dockerfile := "FROM scratch\nADD rootfs.tar /\nCOPY --chmod=755 entrypoint.sh /usr/local/sbin/cospace-entrypoint\nCMD [\"/usr/local/sbin/cospace-entrypoint\"]\n"
	if err = os.WriteFile(filepath.Join(backup, "Dockerfile"), []byte(dockerfile), 0600); err != nil {
		return err
	}
	image := "cospace-network-" + name + "-" + stamp
	if _, err = m.C.R.Run("build", "--cpus", "2", "--memory", "1g", "--progress", "plain", "--tag", image, backup); err != nil {
		return fmt.Errorf("build recovery image: %w", err)
	}
	// Record the recovery image BEFORE the old VM goes away: from here on a
	// failure (or a daemon restart) leaves a space that Start/retry can rebuild
	// instead of one that is permanently missing.
	r.RecoveryImage = image
	unlock := m.lock()
	err = m.save(r)
	unlock()
	if err != nil {
		return err
	}
	if err = m.C.Delete(name); err != nil {
		return err
	}
	deleted = true
	return m.finishUpgrade(r, image, backup, wasRunning)
}

// finishUpgrade recreates the space's VM from a recovery image and brings it
// back to its previous power state.
func (m *Manager) finishUpgrade(r *Space, image, backup string, wasRunning bool) (err error) {
	name := r.Name
	where := ""
	if backup != "" {
		where = " (snapshot " + backup + ")"
	}
	if err = m.C.RunDetached(m.runSpec(r, image)); err != nil {
		return fmt.Errorf("restore space failed; retry or Start the space%s: %w", where, err)
	}
	if err = m.C.WaitReady(name); err != nil {
		return fmt.Errorf("space restored but not ready%s: %w", where, err)
	}
	if err = m.syncRuntime(r); err != nil {
		return fmt.Errorf("space restored; runtime setup needs attention%s: %w", where, err)
	}
	r.NetworkControlVersion = 1
	r.RecoveryImage = ""
	unlock := m.lock()
	err = m.save(r)
	unlock()
	if err != nil {
		return err
	}
	if !wasRunning {
		return m.C.Stop(name)
	}
	return nil
}
