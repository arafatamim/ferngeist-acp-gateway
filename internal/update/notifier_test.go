package update

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
)

type fakeChecker struct {
	release Release
	err     error
}

func (f *fakeChecker) LatestStable(ctx context.Context) (Release, error) {
	return f.release, f.err
}

type fakePush struct {
	mu    sync.Mutex
	calls []string // deviceID:title:body
}

func (f *fakePush) Notify(ctx context.Context, deviceID string, n push.Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, deviceID+":"+n.Title+":"+n.Body)
	return nil
}

func (f *fakePush) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func TestCheckAndNotifyNewerVersion(t *testing.T) {
	n := &Notifier{
		Checker: &fakeChecker{release: Release{TagName: "v2.0.0"}},
		Push:    &fakePush{},
		DeviceIDs: func(ctx context.Context) ([]string, error) {
			return []string{"dev-1", "dev-2"}, nil
		},
	}
	err := n.CheckAndNotify(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	p := n.Push.(*fakePush)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (one per device)", len(p.calls))
	}
	if p.calls[0] != "dev-1:Ferngeist Gateway update available: v2.0.0:Run ferngeist-gateway update to install the latest version." {
		t.Fatalf("call[0] = %q", p.calls[0])
	}
}

func TestCheckAndNotifySameVersionNoPush(t *testing.T) {
	p := &fakePush{}
	n := &Notifier{
		Checker: &fakeChecker{release: Release{TagName: "v1.0.0"}},
		Push:    p,
		DeviceIDs: func(ctx context.Context) ([]string, error) {
			return []string{"dev-1"}, nil
		},
	}
	if err := n.CheckAndNotify(context.Background(), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) != 0 {
		t.Fatalf("calls = %d, want 0", len(p.calls))
	}
}

func TestCheckAndNotifyCheckerError(t *testing.T) {
	n := &Notifier{
		Checker: &fakeChecker{err: context.DeadlineExceeded},
		Push:    &fakePush{},
		DeviceIDs: func(ctx context.Context) ([]string, error) {
			return []string{"dev-1"}, nil
		},
	}
	err := n.CheckAndNotify(context.Background(), "v1.0.0")
	if err == nil {
		t.Fatal("expected error from checker")
	}
}

func TestCheckAndNotifyDeviceIDErrorNoPush(t *testing.T) {
	p := &fakePush{}
	n := &Notifier{
		Checker: &fakeChecker{release: Release{TagName: "v2.0.0"}},
		Push:    p,
		DeviceIDs: func(ctx context.Context) ([]string, error) {
			return nil, context.DeadlineExceeded
		},
	}
	if err := n.CheckAndNotify(context.Background(), "v1.0.0"); err == nil {
		t.Fatal("expected error")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) != 0 {
		t.Fatalf("calls = %d, want 0", len(p.calls))
	}
}

func TestNotifierRunTicks(t *testing.T) {
	p := &fakePush{}
	n := &Notifier{
		Checker:  &fakeChecker{release: Release{TagName: "v2.0.0"}},
		Push:     p,
		Interval: 10 * time.Millisecond,
		DeviceIDs: func(ctx context.Context) ([]string, error) {
			return []string{"dev-1"}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		n.Run(ctx, "v1.0.0")
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) < 2 {
		t.Fatalf("calls = %d, want >= 2 (initial + at least one tick)", len(p.calls))
	}
}

func TestCheckAndNotifyNewerVersionWithVPreFix(t *testing.T) {
	n := &Notifier{
		Checker: &fakeChecker{release: Release{TagName: "v2.0.0"}},
		Push:    &fakePush{},
		DeviceIDs: func(ctx context.Context) ([]string, error) {
			return []string{"dev-1"}, nil
		},
	}
	if err := n.CheckAndNotify(context.Background(), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	p := n.Push.(*fakePush)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(p.calls))
	}
}

func TestCheckAndNotifyDowngradeNoPush(t *testing.T) {
	p := &fakePush{}
	n := &Notifier{
		Checker: &fakeChecker{release: Release{TagName: "v1.0.0"}},
		Push:    p,
		DeviceIDs: func(ctx context.Context) ([]string, error) {
			return []string{"dev-1"}, nil
		},
	}
	if err := n.CheckAndNotify(context.Background(), "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) != 0 {
		t.Fatalf("calls = %d, want 0 (never notify for downgrade)", len(p.calls))
	}
}

func TestIsNewerVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"2.0.0", "1.0.0", true},
		{"1.10.0", "1.9.0", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "2.0.0", false},
		{"1.0.0", "1.0.1", false},
		{"1.0.1", "1.0.0", true},
		{"dev", "1.0.0", false},
		{"1.0.0", "dev", false},
		{"", "1.0.0", false},
	}
	for _, c := range cases {
		if got := isNewerVersion(c.a, c.b); got != c.want {
			t.Errorf("isNewerVersion(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want []int
		ok   bool
	}{
		{"1.0.0", []int{1, 0, 0}, true},
		{"1.10", []int{1, 10}, true},
		{"1", []int{1}, true},
		{"1.0.0-beta", nil, false},
		{"", nil, false},
		{"a.b", nil, false},
		{"1..0", nil, false},
	}
	for _, c := range cases {
		got, ok := parseVersion(c.in)
		if ok != c.ok {
			t.Errorf("parseVersion(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("parseVersion(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseVersion(%q)[%d] = %d, want %d", c.in, i, got[i], c.want[i])
			}
		}
	}
}
