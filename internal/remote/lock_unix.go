//go:build !windows

package remote

import (
	"errors"
	"os"
	"syscall"
)

// lockCLIFile serializes the host endpoint check and update across gateways.
// The kernel releases the lock if the provisioning process crashes.
func lockCLIFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// openCLILock uses a host-wide path unaffected by a gateway's TMPDIR.
func openCLILock() (*os.File, error) {
	const path = "/tmp/ferngeist-tailscale-cli.lock"
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0644)
	if errors.Is(err, os.ErrExist) {
		return os.Open(path)
	}
	if err != nil {
		return nil, err
	}
	// Every local user must be able to lock this host-wide inode, even when
	// the creator is a service running under a different UID or restrictive umask.
	if err := file.Chmod(0644); err != nil {
		_ = file.Close() // Close cannot recover the permission failure.
		return nil, err
	}
	return file, nil
}
