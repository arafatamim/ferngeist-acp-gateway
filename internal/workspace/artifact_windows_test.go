//go:build windows

package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestArtifactWindowsRejectsLexicalHazardsBeforeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, path := range []string{
		`\\localhost\share\video.mp4`, `\\?\C:\video.mp4`, `\\.\C:\video.mp4`,
		`\??\C:\video.mp4`, `\Device\HarddiskVolume1\video.mp4`, `C:video.mp4`,
		`C:\video.mp4:stream`, `C:\x:\video.mp4`, `C:\CON`, `C:\nul.mp4`,
		`C:\COM1.txt`, `C:\CON .txt`, `C:\LPT¹.txt`, `C:\CONIN$`, `C:\CONOUT$`,
		`C:\dir.\video.mp4`, `C:\dir \video.mp4`, `C:\video.mp4.`, `C:\video.mp4 `,
		`C:\dir\..\video.mp4`, `C:\dir\video?.mp4`, `C:\dir\video*.mp4`,
		`C:/dir/video.mp4`, `C:\dir\video".mp4`, `C:\dir\video|.mp4`,
	} {
		t.Run(path, func(t *testing.T) {
			_, _, err := OpenArtifact(ctx, path)
			wantWorkspaceStatus(t, err, 400)
		})
	}
	_, _, err := OpenArtifact(ctx, filepath.Join(t.TempDir(), "video.mp4"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("valid cancelled path = %v", err)
	}
}

func TestArtifactWindowsOpensLocalRegularFilesAndChecksSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path, []byte("video bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, info, err := OpenArtifact(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil || string(data) != "video bytes" || info.Size() != 11 {
		t.Fatalf("artifact = %q %v %v", data, info, err)
	}
	_, _, err = OpenArtifact(t.Context(), filepath.Dir(path))
	wantWorkspaceStatus(t, err, 400)
	_, _, err = OpenArtifact(t.Context(), path+"-missing")
	wantWorkspaceStatus(t, err, 404)
	if err := os.Truncate(path, MaxArtifactBytes+1); err != nil {
		t.Fatal(err)
	}
	_, _, err = OpenArtifact(t.Context(), path)
	wantWorkspaceStatus(t, err, 413)
}

func TestArtifactWindowsRejectsReparseBeforeMissingTargetLookup(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing-target")
	junction := filepath.Join(dir, "junction")
	cmd := exec.CommandContext(t.Context(), "cmd", "/c", "mklink", "/J", junction, missing)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create temp junction: %v: %s", err, out)
	}
	_, _, err := OpenArtifact(t.Context(), filepath.Join(junction, "video.mp4"))
	wantWorkspaceStatus(t, err, 400) // Following the missing target would report 404.
	link := filepath.Join(dir, "video.mp4")
	if err := os.Symlink(filepath.Join(missing, "video.mp4"), link); err != nil {
		t.Fatal(err)
	}
	_, _, err = OpenArtifact(t.Context(), link)
	wantWorkspaceStatus(t, err, 400)
	if strings.Contains(err.Error(), dir) {
		t.Fatal("reparse error contains a host path")
	}
}

func TestArtifactWindowsRejectsNetworkAndIndirectVolumeMappings(t *testing.T) {
	for _, target := range []string{
		`\Device\Mup\server\share`, `\Device\LanmanRedirector\server\share`,
		`\??\UNC\server\share`, `\??\C:\local-subst`, `\Device\HarddiskVolume1\extra`,
		`\Device\HarddiskVolume`, `\Device\HarddiskVolume١`, `\Device\CdRom0`,
	} {
		if localArtifactVolumeTarget(target) {
			t.Fatalf("unsafe target accepted: %q", target)
		}
	}
	if !localArtifactVolumeTarget(`\Device\HarddiskVolume12`) {
		t.Fatal("direct local disk target rejected")
	}
}

func TestArtifactWindowsInspectionPreventsReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(path, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	volume, err := artifactVolumePath(path[:2])
	if err != nil {
		t.Fatal(err)
	}
	// Use the same native open primitive as traversal, with a trusted temp name.
	parent, err := openWindowsArtifactPart(0, volume+dir[3:], true)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := os.Rename(dir, dir+"-moved"); err == nil {
		t.Fatal("pinned directory could be replaced")
	}
	inspected, err := openWindowsArtifactPart(windows.Handle(parent.Fd()), filepath.Base(path), false)
	if err != nil {
		t.Fatal(err)
	}
	defer inspected.Close()
	if err := os.Rename(path, path+"-moved"); err == nil {
		t.Fatal("inspected file could be replaced before identity recheck")
	}
}

func TestArtifactWindowsReadHandleRetainsAdmittedIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, before, err := OpenArtifact(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	if err != nil || string(data) != "original" {
		t.Fatalf("admitted bytes = %q, %v", data, err)
	}
	replacement, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = checkedArtifact(replacement, before)
	wantWorkspaceStatus(t, err, 409)
	if _, err := replacement.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("rejected handle was not closed: %v", err)
	}
}
