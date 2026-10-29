package remote

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// lockCLIFile serializes the host endpoint check and update across gateways.
// The kernel releases the lock if the provisioning process crashes.
func lockCLIFile(file *os.File) error {
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

// openCLILock uses the shared application-data directory, independent of TEMP.
func openCLILock() (*os.File, error) {
	root, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "Ferngeist")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, "tailscale-cli.lock"), os.O_CREATE|os.O_RDONLY, 0644)
}
