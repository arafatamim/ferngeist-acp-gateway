package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestArtifactWindowsOplockDoesNotHoldOpening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	release := holdArtifactOplock(t, path)
	defer release()
	result, done := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(done)
		f, _, err := OpenArtifact(t.Context(), path)
		if f != nil {
			f.Close()
		}
		result <- err
	}()
	t.Cleanup(func() {
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("opening did not finish after releasing the fixture oplock")
		}
	})
	select {
	case err := <-result:
		wantWorkspaceStatus(t, err, 409)
	case <-time.After(2 * time.Second):
		t.Fatal("artifact opening waited for oplock acknowledgment")
	}
}

func holdArtifactOplock(t *testing.T, path string) func() {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		t.Fatal(err)
	}
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		if closeErr := windows.CloseHandle(h); closeErr != nil {
			t.Errorf("close fixture file after event failure: %v", closeErr)
		}
		t.Fatal(err)
	}
	ov := &windows.Overlapped{HEvent: event}
	var pin runtime.Pinner
	pin.Pin(ov)
	var once sync.Once
	pending := false
	release := func() {
		once.Do(func() {
			if pending {
				cancelArtifactOplock(t, h, ov)
			}
			if err := windows.CloseHandle(h); err != nil {
				t.Errorf("close fixture file: %v", err)
			}
			if err := windows.CloseHandle(event); err != nil {
				t.Errorf("close fixture event: %v", err)
			}
			pin.Unpin()
		})
	}
	t.Cleanup(release)
	// FSCTL_REQUEST_OPLOCK_LEVEL_1 is an asynchronous exclusive file oplock;
	// pending is the successful held state until another opener breaks it.
	const requestLevel1 = 0x00090000
	var returned uint32
	err = windows.DeviceIoControl(h, requestLevel1, nil, 0, nil, 0, &returned, ov)
	if !errors.Is(err, windows.ERROR_IO_PENDING) {
		t.Fatalf("request fixture oplock: %v", err)
	}
	pending = true
	return release
}

func cancelArtifactOplock(t *testing.T, h windows.Handle, ov *windows.Overlapped) {
	t.Helper()
	// Cancel and join the outstanding native request before unpinning memory.
	if err := windows.CancelIoEx(h, ov); err != nil && !errors.Is(err, windows.ERROR_NOT_FOUND) {
		t.Errorf("cancel fixture oplock: %v", err)
	}
	var transferred uint32
	err := windows.GetOverlappedResult(h, ov, &transferred, true)
	if err != nil && !errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
		t.Errorf("finish fixture oplock: %v", err)
	}
}
