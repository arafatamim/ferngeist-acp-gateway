// Package caffeine keeps the host from idle-sleeping while an agent turn runs,
// so the agent keeps working and a remote client can still reach the gateway.
package caffeine

import (
	"context"
	"log/slog"
	"time"
)

// pollInterval bounds how late an inhibitor engages or drops after a turn
// starts or ends. Idle-sleep timeouts are minutes, so seconds of lag is fine.
const pollInterval = 30 * time.Second

// Run holds a sleep inhibitor whenever busy reports true until ctx is
// cancelled, then releases it before returning.
func Run(ctx context.Context, busy func() bool, logger *slog.Logger) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	run(ctx, ticker.C, busy, hold, logger)
}

func run(ctx context.Context, tick <-chan time.Time, busy func() bool, hold func() (func(), error), logger *slog.Logger) {
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	warned := false
	for {
		switch isBusy := busy(); {
		case isBusy && release == nil:
			r, err := hold()
			if err != nil && !warned {
				// Warn once: the cause (missing tool, polkit denial) won't change between polls.
				logger.Warn("cannot keep the host awake during agent turns", slog.String("error", err.Error()))
				warned = true
			}
			release = r
		case !isBusy && release != nil:
			release()
			release = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
	}
}
