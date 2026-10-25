package projects

import (
	"context"
	"time"
)

// startMaintenanceLocked owns expiry and pressure sampling independently of
// requesting devices. A lister used only for checkout resolution needs neither.
func (l *Lister) startMaintenanceLocked() {
	if l.maintenanceCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.maintenanceCancel = cancel
	l.maintenanceDone = make(chan struct{})
	go func() {
		defer close(l.maintenanceDone)
		pressure := memoryPressureSources()
		pressure.increased() // Establish a baseline; old stalls must not cause eviction.
		// Sample once a second to bound overhead and reclaim one idle process at a
		// time, allowing the next sample to observe whether pressure has subsided.
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case now := <-tick.C:
				l.maintain(now, pressure.increased())
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (l *Lister) maintain(now time.Time, pressure bool) {
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
	if !pressure {
		return
	}
	var oldest *agentConn
	var oldestID string
	for id, agent := range l.warm {
		if oldest == nil || agent.lastUsed.Before(oldest.lastUsed) {
			oldest, oldestID = agent, id
		}
	}
	if oldest != nil {
		delete(l.warm, oldestID)
		oldest.timer.Stop()
		l.discardLocked(oldest)
	}
}
