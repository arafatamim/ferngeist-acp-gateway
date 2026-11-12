package workspace

import (
	"net/http"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func validateArtifactPlatformPath(path string) error {
	if len(path) <= 3 || path[1:3] != `:\` || !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", rune(path[0])) ||
		strings.ContainsAny(path[3:], `/:*?"<>|`) {
		return errStatus(http.StatusBadRequest, "invalid Windows artifact path")
	}
	for _, part := range strings.Split(path[3:], `\`) {
		base, _, _ := strings.Cut(part, ".")
		base = strings.TrimRight(base, " ")
		// IsLocal may allow device names with extensions on newer Windows;
		// checking the bare stem keeps this endpoint policy independent of that.
		if (base != "" && !filepath.IsLocal(base)) || !filepath.IsLocal(part) || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || strings.EqualFold(base, "CLOCK$") {
			return errStatus(http.StatusBadRequest, "invalid Windows artifact component")
		}
	}
	return nil
}

func artifactVolumePath(volume string) (string, error) {
	name, err := windows.UTF16PtrFromString(volume)
	if err != nil {
		return "", artifactFileError(err)
	}
	var buf [512]uint16
	n, err := windows.QueryDosDevice(name, &buf[0], uint32(len(buf)))
	if err != nil {
		return "", artifactFileError(err)
	}
	target := windows.UTF16ToString(buf[:n])
	if !localArtifactVolumeTarget(target) {
		return "", errStatus(http.StatusBadRequest, "artifact drive must map directly to a local disk")
	}
	// Type-check the pinned mapping, not the mutable drive-letter alias. Never
	// open the alias after checking it: it could have been redirected to SMB.
	root, err := windows.UTF16PtrFromString(`\\?\GLOBALROOT` + target + `\`)
	if err != nil {
		return "", artifactFileError(err)
	}
	kind := windows.GetDriveType(root)
	if kind != windows.DRIVE_FIXED && kind != windows.DRIVE_REMOVABLE {
		return "", errStatus(http.StatusBadRequest, "artifact drive must be a local disk")
	}
	return target + `\`, nil
}

func localArtifactVolumeTarget(target string) bool {
	number, ok := strings.CutPrefix(target, `\Device\HarddiskVolume`)
	return ok && number != "" && strings.IndexFunc(number, func(r rune) bool { return r < '0' || r > '9' }) < 0
}
