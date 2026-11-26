//go:build unix

package workspace

import "golang.org/x/sys/unix"

// A FIFO swapped in after Stat must not stall the open.
const workspaceReadFlags = unix.O_RDONLY | unix.O_NONBLOCK
