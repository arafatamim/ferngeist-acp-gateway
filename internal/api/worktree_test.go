package api

import (
	"encoding/json"
	"testing"
)

// TestUnmanagedWorktreeResponseWireShape pins the list-unmanaged wire shape,
// which used to come from the struct tags on
// workspace.UnmanagedWorktree: path is always present and branch, head and
// detached are omitted when zero.
func TestUnmanagedWorktreeResponseWireShape(t *testing.T) {
	t.Run("populated", func(t *testing.T) {
		out := []unmanagedWorktreeResponse{{
			Path:     "/repo/.external-wt",
			Branch:   "feat/ext",
			Head:     "9a5f4c1",
			Detached: true,
		}}
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if want := `[{"path":"/repo/.external-wt","branch":"feat/ext","head":"9a5f4c1","detached":true}]`; string(b) != want {
			t.Fatalf("got %s\nwant %s", b, want)
		}
	})
	t.Run("all zero except path", func(t *testing.T) {
		out := []unmanagedWorktreeResponse{{Path: "/repo/.external-wt"}}
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if want := `[{"path":"/repo/.external-wt"}]`; string(b) != want {
			t.Fatalf("got %s\nwant %s", b, want)
		}
	})
}
