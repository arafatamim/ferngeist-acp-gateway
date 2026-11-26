//go:build !unix

package workspace

import "os"

const workspaceReadFlags = os.O_RDONLY
