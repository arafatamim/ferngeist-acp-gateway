//go:build linux || darwin

package caffeine

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
)

// keeper runs as the inhibitor's child: echo signals that the inhibitor is
// held, and cat lives until stdin closes, which is on release or when the
// gateway dies, so a crashed gateway never leaves the host pinned awake.
var keeper = []string{"sh", "-c", "echo; exec cat"}

func inhibitorArgv() []string {
	if runtime.GOOS == "darwin" {
		return append([]string{"caffeinate", "-i"}, keeper...)
	}
	return append([]string{"systemd-inhibit", "--what=idle:sleep", "--mode=block",
		"--who=ferngeist-gateway", "--why=An agent turn is running"}, keeper...)
}

func hold() (func(), error) {
	argv := inhibitorArgv()
	cmd := exec.Command(argv[0], argv[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("%s stdin: %w", argv[0], err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%s stdout: %w", argv[0], err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", argv[0], err)
	}
	// The keeper only runs once the inhibitor is taken; EOF first means it was refused.
	if _, err := io.ReadFull(stdout, make([]byte, 1)); err != nil {
		_ = stdin.Close() // only unblocks Wait; the process already exited or is exiting
		waitErr := cmd.Wait()
		return nil, fmt.Errorf("%s: %v: %s", argv[0], waitErr, strings.TrimSpace(stderr.String()))
	}
	return func() {
		_ = stdin.Close() // a close error leaves nothing to undo; Wait reaps either way
		_ = cmd.Wait()    // the keeper exits non-zero only on a signal, which still drops the lock
	}, nil
}
