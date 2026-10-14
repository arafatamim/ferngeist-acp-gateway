//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchBinary(t *testing.T) {
	write := func(path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(ctx context.Context) bool {
		select {
		case <-ctx.Done():
			return true
		case <-time.After(200 * time.Millisecond):
			return false
		}
	}

	t.Run("replaced by rename fires", func(t *testing.T) {
		dir := t.TempDir()
		bin := filepath.Join(dir, "ferngeist-gateway")
		write(bin)
		ctx, replaced := watchBinary(context.Background(), bin, 10*time.Millisecond)
		write(bin + ".new")
		if err := os.Rename(bin+".new", bin); err != nil {
			t.Fatal(err)
		}
		if !wait(ctx) || !replaced() {
			t.Fatal("want cancel and replaced() after the binary was renamed over")
		}
	})

	t.Run("unchanged or deleted does not fire", func(t *testing.T) {
		dir := t.TempDir()
		bin := filepath.Join(dir, "ferngeist-gateway")
		write(bin)
		ctx, replaced := watchBinary(context.Background(), bin, 10*time.Millisecond)
		if wait(ctx) {
			t.Fatal("cancelled with the binary unchanged")
		}
		if err := os.Remove(bin); err != nil {
			t.Fatal(err)
		}
		if wait(ctx) || replaced() {
			t.Fatal("cancelled after the binary was deleted")
		}
	})

	t.Run("parent cancel is not a replacement", func(t *testing.T) {
		dir := t.TempDir()
		bin := filepath.Join(dir, "ferngeist-gateway")
		write(bin)
		parent, cancel := context.WithCancel(context.Background())
		ctx, replaced := watchBinary(parent, bin, 10*time.Millisecond)
		cancel()
		if !wait(ctx) || replaced() {
			t.Fatal("want cancel without replaced()")
		}
	})
}
