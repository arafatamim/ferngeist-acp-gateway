//go:build unix

package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func isSingleLinkLog(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && stat.Nlink == 1
}

func (s *Service) openActiveLog(path string) (*os.File, error) {
	const flags = os.O_APPEND | os.O_WRONLY
	file, err := os.OpenFile(path, flags|os.O_CREATE|os.O_EXCL, s.fileMode)
	if errors.Is(err, os.ErrExist) {
		info, err := statLog(path)
		if err != nil {
			return nil, err
		}
		return openExistingLog(path, info, flags, s.fileMode)
	}
	return file, err
}

func statLog(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !isSingleLinkLog(info) {
		return nil, fmt.Errorf("log must be a regular file with one link: %s", path)
	}
	return info, nil
}

func openExistingLog(path string, before os.FileInfo, flags int, mode os.FileMode) (*os.File, error) {
	file, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err := restrictExistingLog(file, before, mode); err != nil {
		_ = file.Close() // No records were written before validation.
		return nil, err
	}
	return file, nil
}

func restrictExistingLog(file *os.File, before os.FileInfo, mode os.FileMode) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, info) || !isSingleLinkLog(info) {
		return fmt.Errorf("log changed while opening: %s", file.Name())
	}
	// Tighten existing files through their verified descriptor and preserve a
	// stricter mode applied by the operator.
	if current := info.Mode().Perm(); current&^mode.Perm() != 0 {
		return file.Chmod(current & mode.Perm())
	}
	return nil
}

func (s *Service) restrictBackups() error {
	active := filepath.Join(s.dir, s.fileName)
	dir := filepath.Dir(active)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	// Rotation limits can change, so old generations may be outside the
	// current limit or no longer numbered contiguously.
	prefix := filepath.Base(active) + "."
	for _, entry := range entries {
		suffix, ok := strings.CutPrefix(entry.Name(), prefix)
		if !ok || strings.HasPrefix(suffix, "0") {
			continue
		}
		if _, err := strconv.ParseUint(suffix, 10, 64); err != nil {
			continue
		}
		if err := s.restrictBackup(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) restrictBackup(path string) error {
	info, err := statLog(path)
	if err != nil || info.Mode().Perm()&^s.fileMode.Perm() == 0 {
		return err
	}
	file, err := openExistingLog(path, info, os.O_RDONLY, s.fileMode)
	if err != nil {
		return err
	}
	return file.Close()
}
