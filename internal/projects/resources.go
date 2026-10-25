package projects

import (
	"context"
	"time"
)

// startMaintenanceLocked owns cache expiry independently of
// requesting devices. A lister used only for checkout resolution does not need it.
func (l *Lister) startMaintenanceLocked() {
	if l.maintenanceCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.maintenanceCancel = cancel
	l.maintenanceDone = make(chan struct{})
	go func() {
		defer close(l.maintenanceDone)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case now := <-tick.C:
				l.maintain(now)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (l *Lister) maintain(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	for id, cached := range l.cached {
		if now.Sub(cached.at) >= cacheTTL {
			delete(l.cached, id)
		}
	}
}
