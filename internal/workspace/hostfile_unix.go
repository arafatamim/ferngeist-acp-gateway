//go:build linux || darwin

package workspace

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func openHostFile(path string) (*os.File, os.FileInfo, error) {
	if specialHostFilePath(path) {
		return nil, nil, errStatus(http.StatusBadRequest, "special file path is forbidden")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, nil, hostFileError(err)
	}
	if specialHostFilePath(filepath.Join(parent, filepath.Base(path))) {
		return nil, nil, errStatus(http.StatusBadRequest, "special file path is forbidden")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, nil, hostFileError(err)
	}
	defer root.Close()
	return openRootedHostFile(root, filepath.Base(path))
}

func openRootedHostFile(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, hostFileError(err)
	}
	if !before.Mode().IsRegular() {
		return nil, nil, errStatus(http.StatusBadRequest, "path must be a regular file")
	}
	// NONBLOCK prevents a raced FIFO from stalling; NOFOLLOW rejects a raced
	// final symlink even when its target would stay inside the opened parent.
	f, err := openHostFileBasename(root, name)
	if errors.Is(err, unix.ELOOP) {
		return nil, nil, errStatus(http.StatusConflict, "file changed while opening")
	}
	if err != nil {
		return nil, nil, hostFileError(err)
	}
	return checkedHostFile(f, before)
}

func openHostFileBasename(root *os.Root, name string) (*os.File, error) {
	// Root.OpenFile resolves symlinks itself even with O_NOFOLLOW. Obtain a
	// directory handle inside the root and let the kernel enforce the final
	// component policy. name is only filepath.Base of the validated path.
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func specialHostFilePath(path string) bool {
	for _, prefix := range []string{"/proc", "/sys", "/dev"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func validateHostFilePlatformPath(string) error {
	return nil
}

func hostFileVersion(f *os.File) string {
	conn, err := f.SyscallConn()
	if err != nil {
		return ""
	}
	var st unix.Stat_t
	var statErr error
	if err := conn.Control(func(fd uintptr) { statErr = unix.Fstat(int(fd), &st) }); err != nil || statErr != nil {
		return ""
	}
	// ctime moves on every write even when mtime is restored (touch -d, cp -p).
	return fmt.Sprintf("%d:%d:%d:%d:%d", st.Dev, st.Ino, st.Size, st.Mtim.Nano(), st.Ctim.Nano())
}
