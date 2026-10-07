package update

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
)

// UpdateChecker fetches the latest stable release. Satisfied by *Checker.
type UpdateChecker interface {
	LatestStable(ctx context.Context) (Release, error)
}

// Notifier periodically checks for a newer stable release and pushes an
// update-available notification to every paired device via the push service.
// It never applies updates — the user runs `ferngeist-gateway update`.
type Notifier struct {
	Checker UpdateChecker
	Push    push.PushService
	// DeviceIDs returns the IDs of all paired devices (storage-backed in
	// production; injected for tests).
	DeviceIDs func(ctx context.Context) ([]string, error)
	// Interval between checks. The first check runs immediately.
	Interval time.Duration

	mu           sync.Mutex
	lastNotified string // tag last pushed; dedupes re-notifying within the process
}

func NewNotifier(checker UpdateChecker, pushSvc push.PushService, deviceIDs func(ctx context.Context) ([]string, error)) *Notifier {
	return &Notifier{
		Checker:   checker,
		Push:      pushSvc,
		DeviceIDs: deviceIDs,
		Interval:  24 * time.Hour,
	}
}

// Run blocks until ctx is cancelled. It checks once immediately, then on the
// configured interval. Failures are logged (best-effort) and never crash the
// daemon.
func (n *Notifier) Run(ctx context.Context, currentVersion string) {
	// First check immediately. Run itself is expected to be launched in its
	// own goroutine (see daemon.Run), so a slow network does not block boot
	// and the context cancels it when the daemon shuts down.
	_ = n.CheckAndNotify(ctx, currentVersion)
	ticker := time.NewTicker(n.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = n.CheckAndNotify(ctx, currentVersion)
		}
	}
}

// CheckAndNotify fetches the latest stable release and, if it is newer than
// currentVersion, pushes an update-available notification to all paired
// devices, once per tag per process. A non-parseable current version (e.g.
// "dev") never triggers.
func (n *Notifier) CheckAndNotify(ctx context.Context, currentVersion string) error {
	release, err := n.Checker.LatestStable(ctx)
	if err != nil {
		return fmt.Errorf("check for updates: %w", err)
	}
	latest := strings.TrimPrefix(release.TagName, "v")
	current := strings.TrimPrefix(currentVersion, "v")
	if latest == "" || latest == current {
		return nil
	}
	// Only notify for a newer-looking version; never for a downgrade.
	if !IsNewer(latest, current) {
		return nil
	}
	n.mu.Lock()
	seen := n.lastNotified == release.TagName
	n.mu.Unlock()
	if seen {
		return nil
	}

	deviceIDs, err := n.DeviceIDs(ctx)
	if err != nil {
		return fmt.Errorf("list paired devices for update notification: %w", err)
	}

	if len(deviceIDs) > 0 {
		n.mu.Lock()
		n.lastNotified = release.TagName
		n.mu.Unlock()
	}
	for _, deviceID := range deviceIDs {
		_ = n.Push.Notify(ctx, deviceID, push.Notification{
			Title:    "Ferngeist Gateway update available: " + release.TagName,
			Body:     "Run ferngeist-gateway update to install the latest version.",
			Category: push.CategoryProgress,
		})
	}
	return nil
}

// IsNewer reports whether version a is strictly newer than b. Versions are
// dot-separated numerics; a pre-release/build suffix ("-dirty", "+meta") is
// ignored, so 0.11.3-dirty is not older than 0.11.3. Returns false when either
// side is unparseable (never false-positives an upgrade).
func IsNewer(a, b string) bool {
	ap, aok := parseVersion(a)
	bp, bok := parseVersion(b)
	if !aok || !bok {
		return false
	}
	for i := 0; i < len(ap) || i < len(bp); i++ {
		av, bv := 0, 0
		if i < len(ap) {
			av = ap[i]
		}
		if i < len(bp) {
			bv = bp[i]
		}
		if av != bv {
			return av > bv
		}
	}
	return false
}

// ValidVersion reports whether v (a leading "v" is tolerated) is parseable.
func ValidVersion(v string) bool {
	_, ok := parseVersion(strings.TrimPrefix(v, "v"))
	return ok
}

func parseVersion(v string) ([]int, bool) {
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var parts []int
	for _, p := range strings.Split(v, ".") {
		if p == "" {
			return nil, false
		}
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return nil, false
			}
			n = n*10 + int(c-'0')
		}
		parts = append(parts, n)
	}
	return parts, true
}
