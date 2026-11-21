package main

import (
	"fmt"
	"os"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/logging"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/service"
)

func runLogs(tail int, follow bool) error {
	// An explicit log dir covers foreground `daemon run`; otherwise read the installed service's.
	dir := os.Getenv("FERNGEIST_GATEWAY_LOG_DIR")
	if dir == "" {
		serviceDir, err := service.NewManager().LogDir()
		if err != nil {
			return fmt.Errorf("resolve daemon log directory (set FERNGEIST_GATEWAY_LOG_DIR): %w", err)
		}
		dir = serviceDir
	}

	lines, err := logging.ReadGatewayLog(dir, tail)
	if err != nil {
		return fmt.Errorf("read gateway log: %w", err)
	}
	if len(lines) == 0 && !follow {
		fmt.Printf("No gateway log in %s\n", dir)
		return nil
	}
	for _, line := range lines {
		logging.WritePrettyLogLine(os.Stdout, line)
	}
	if !follow {
		return nil
	}

	ctx, stop := waitForSignal()
	defer stop()
	// shortcut: lines written between the read above and the follow's seek are skipped; fine for interactive use.
	err = logging.FollowGatewayLog(ctx, dir, 500*time.Millisecond, func(line string) {
		logging.WritePrettyLogLine(os.Stdout, line)
	})
	if err != nil {
		return fmt.Errorf("follow gateway log: %w", err)
	}
	return nil
}
