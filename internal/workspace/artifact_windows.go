package workspace

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openArtifact(path string) (*os.File, os.FileInfo, error) {
	volume, err := artifactVolumePath(path[:2])
	if err != nil {
		return nil, nil, err
	}
	parent, err := openWindowsArtifactPart(0, volume, true)
	if err != nil {
		return nil, nil, err
	}
	// Keeping every ancestor open without write/delete sharing prevents its
	// replacement or conversion to a reparse point while descendants are opened.
	dirs := []*os.File{parent}
	defer func() {
		for _, dir := range dirs {
			dir.Close()
		}
	}()
	parts := strings.Split(path[3:], `\`)
	for _, name := range parts[:len(parts)-1] {
		dir, err := openWindowsArtifactPart(windows.Handle(parent.Fd()), name, true)
		if err != nil {
			return nil, nil, err
		}
		dirs = append(dirs, dir)
		parent = dir
	}
	return openWindowsArtifactFile(windows.Handle(parent.Fd()), parts[len(parts)-1])
}

func openWindowsArtifactPart(parent windows.Handle, name string, directory bool) (*os.File, error) {
	access := uint32(windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
	if directory {
		access |= windows.FILE_TRAVERSE
	} else {
		// Attribute-only access does not participate in Windows sharing checks.
		access |= windows.FILE_READ_DATA
	}
	h, err := openWindowsArtifactHandle(parent, name, access, windows.FILE_SHARE_READ)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), name)
	if err := checkWindowsArtifactHandle(h, directory); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func openWindowsArtifactFile(parent windows.Handle, name string) (*os.File, os.FileInfo, error) {
	beforeFile, err := openWindowsArtifactPart(parent, name, false)
	if err != nil {
		return nil, nil, err
	}
	defer beforeFile.Close()
	before, err := beforeFile.Stat()
	if err != nil {
		return nil, nil, artifactFileError(err)
	}
	h, err := openWindowsArtifactHandle(parent, name, windows.FILE_GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(h), name)
	if err := checkWindowsArtifactHandle(h, false); err != nil {
		f.Close()
		return nil, nil, err
	}
	return checkedArtifact(f, before)
}

func openWindowsArtifactHandle(parent windows.Handle, name string, access, share uint32) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, artifactFileError(err)
	}
	attrs := windows.OBJECT_ATTRIBUTES{RootDirectory: parent, ObjectName: objectName, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var h windows.Handle
	// Do not wait for another process to acknowledge an oplock break; an
	// alternate success carries a handle that is closed and refused below.
	err = windows.NtCreateFile(&h, access, &attrs, &windows.IO_STATUS_BLOCK{}, nil, 0, share, windows.FILE_OPEN,
		windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_COMPLETE_IF_OPLOCKED, 0, 0)
	if err == nil {
		return h, nil
	}
	if h != 0 && h != windows.InvalidHandle {
		if closeErr := windows.CloseHandle(h); closeErr != nil {
			return 0, artifactFileError(closeErr)
		}
	}
	return 0, windowsArtifactError(err)
}

func checkWindowsArtifactHandle(h windows.Handle, directory bool) error {
	kind, err := windows.GetFileType(h)
	if err != nil {
		return artifactFileError(err)
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return artifactFileError(err)
	}
	if kind != windows.FILE_TYPE_DISK || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		(info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		return errStatus(http.StatusBadRequest, "artifact must use ordinary files and directories without reparse points")
	}
	return nil
}

func windowsArtifactError(err error) error {
	var status windows.NTStatus
	if errors.As(err, &status) {
		err = status.Errno()
	}
	switch {
	case errors.Is(err, windows.ERROR_CANT_ACCESS_FILE), errors.Is(err, windows.ERROR_REPARSE_TAG_INVALID),
		errors.Is(err, windows.ERROR_REPARSE_TAG_MISMATCH), errors.Is(err, windows.ERROR_DIRECTORY),
		status == windows.STATUS_REPARSE_POINT_ENCOUNTERED, status == windows.STATUS_STOPPED_ON_SYMLINK,
		status == windows.STATUS_IO_REPARSE_TAG_NOT_HANDLED, errors.Is(err, windows.ERROR_INVALID_NAME), errors.Is(err, windows.ERROR_FILENAME_EXCED_RANGE):
		return errStatus(http.StatusBadRequest, "artifact reparse or nonregular path is forbidden")
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION), errors.Is(err, windows.ERROR_DELETE_PENDING), status == windows.STATUS_OPLOCK_BREAK_IN_PROGRESS:
		return errStatus(http.StatusConflict, "artifact changed or is busy while opening")
	default:
		return artifactFileError(err)
	}
}
