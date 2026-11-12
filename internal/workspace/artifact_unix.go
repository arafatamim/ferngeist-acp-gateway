//go:build linux || darwin

package workspace

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func openArtifact(path string) (*os.File, os.FileInfo, error) {
	if specialArtifactPath(path) {
		return nil, nil, errStatus(http.StatusBadRequest, "special artifact path is forbidden")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, nil, artifactFileError(err)
	}
	if specialArtifactPath(filepath.Join(parent, filepath.Base(path))) {
		return nil, nil, errStatus(http.StatusBadRequest, "special artifact path is forbidden")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, nil, artifactFileError(err)
	}
	defer root.Close()
	return openRootedArtifact(root, filepath.Base(path))
}

func openRootedArtifact(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, artifactFileError(err)
	}
	if !before.Mode().IsRegular() {
		return nil, nil, errStatus(http.StatusBadRequest, "artifact must be a regular file")
	}
	// NONBLOCK prevents a raced FIFO from stalling; NOFOLLOW rejects a raced
	// final symlink even when its target would stay inside the opened parent.
	f, err := openArtifactBasename(root, name)
	if errors.Is(err, unix.ELOOP) {
		return nil, nil, errStatus(http.StatusConflict, "artifact changed while opening")
	}
	if err != nil {
		return nil, nil, artifactFileError(err)
	}
	return checkedArtifact(f, before)
}

func openArtifactBasename(root *os.Root, name string) (*os.File, error) {
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

func specialArtifactPath(path string) bool {
	for _, prefix := range []string{"/proc", "/sys", "/dev"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func validateArtifactPlatformPath(string) error {
	return nil
}
