//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

func TestArtifactOpeningRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(file, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.mp4")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "special")
	if err := os.Symlink("/dev", alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, link, fifo, "/dev/null", "/proc/self/environ", "/sys/x", filepath.Join(alias, "null"), file + "/../video.mp4", "relative", file + "\u200b"} {
		t.Run(path, func(t *testing.T) {
			f, _, err := OpenArtifact(context.Background(), path)
			if f != nil {
				f.Close()
				t.Fatal("unsafe file opened")
			}
			wantWorkspaceStatus(t, err, http.StatusBadRequest)
		})
	}
}

func TestArtifactFinalOpenNeverFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "target"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	f, err := openArtifactBasename(root, "link")
	if f != nil {
		f.Close()
		t.Fatal("final symlink followed")
	}
	if err == nil {
		t.Fatal("symlink open succeeded")
	}
}

func TestArtifactOpeningSurvivesConcurrentFifoAndIdentityReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact")
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	ready := make(chan struct{})
	var started sync.Once
	wg.Go(func() {
		defer started.Do(func() { close(ready) })
		for ctx.Err() == nil {
			staging := filepath.Join(dir, "next")
			if err := os.WriteFile(staging, []byte("regular"), 0o600); err != nil {
				t.Errorf("stage regular file: %v", err)
				return
			}
			if err := os.Rename(staging, path); err != nil {
				t.Errorf("replace regular file: %v", err)
				return
			}
			if err := syscall.Mkfifo(staging, 0o600); err != nil {
				t.Errorf("stage FIFO: %v", err)
				return
			}
			if err := os.Rename(staging, path); err != nil {
				t.Errorf("replace FIFO: %v", err)
				return
			}
			started.Do(func() { close(ready) })
		}
	})
	defer wg.Wait()
	defer cancel()
	<-ready
	for range 300 {
		f, info, err := OpenArtifact(ctx, path)
		if err != nil {
			var rejection Error
			if !errors.As(err, &rejection) || (rejection.StatusCode() != 400 && rejection.StatusCode() != 404 && rejection.StatusCode() != 409) {
				t.Fatalf("unexpected race rejection: %v", err)
			}
			continue
		}
		f.Close()
		if !info.Mode().IsRegular() {
			t.Fatal("raced FIFO admitted")
		}
	}
}

func TestArtifactSpecialTreeDenialUsesComponentBoundaries(t *testing.T) {
	for path, want := range map[string]bool{"/proc": true, "/proc/self/root/tmp/a": true, "/dev/null": true, "/sys/x": true, "/procfoo/video": false, "/device/video": false, "/system/video": false} {
		if got := specialArtifactPath(path); got != want {
			t.Fatalf("special path %q = %v, want %v", path, got, want)
		}
	}
}

func TestArtifactOpeningChecksSizeAndCurrentParent(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
	for _, path := range []string{first, second} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "video.mp4"), []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(second, alias); err != nil {
		t.Fatal(err)
	}
	f, info, err := OpenArtifact(context.Background(), filepath.Join(alias, "video.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if info.Size() != int64(len(second)) {
		t.Fatalf("size = %d, want current parent's file", info.Size())
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, 512<<20+1); err != nil {
		t.Fatal(err)
	}
	_, _, err = OpenArtifact(context.Background(), big)
	wantWorkspaceStatus(t, err, http.StatusRequestEntityTooLarge)
	_, _, err = OpenArtifact(context.Background(), filepath.Join(dir, "missing"))
	wantWorkspaceStatus(t, err, http.StatusNotFound)
}
