package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

func TestCacheServesStaleSnapshotWhileRefreshingAndKeepsItOnFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type reply struct {
			data string
			err  error
		}
		replies := make(chan reply, 1)
		// The final stale Get may start a refresh; closing releases that collector.
		defer close(replies)
		cache := NewCache(func(context.Context) (json.RawMessage, error) {
			r, ok := <-replies
			if !ok {
				return nil, context.Canceled
			}
			if r.err != nil {
				return nil, r.err
			}
			return json.RawMessage(r.data), nil
		}, 0)
		get := func() (json.RawMessage, error) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			return cache.Get(ctx)
		}

		replies <- reply{data: "1"}
		if data, err := get(); err != nil || string(data) != "1" {
			t.Fatalf("first Get = %s, %v; want collected snapshot", data, err)
		}
		synctest.Wait()
		// The snapshot is stale (maxAge 0); Get must not wait for the refresh it starts.
		if data, err := get(); err != nil || string(data) != "1" {
			t.Fatalf("stale Get = %s, %v; want old snapshot", data, err)
		}
		replies <- reply{err: errors.New("codexbar failed")}
		// Wait for singleflight to remove the failed call before triggering its replacement.
		synctest.Wait()
		if data, err := get(); err != nil || string(data) != "1" {
			t.Fatalf("Get after failed refresh = %s, %v; want old snapshot", data, err)
		}
		replies <- reply{data: "2"}
		synctest.Wait()
		if data, err := get(); err != nil || string(data) != "2" {
			t.Fatalf("refreshed Get = %s, %v; want new snapshot", data, err)
		}
	})
}

func TestCacheReturnsCollectErrorWhenNoSnapshotExists(t *testing.T) {
	cache := NewCache(func(context.Context) (json.RawMessage, error) {
		return nil, errors.New("codexbar missing")
	}, time.Minute)
	if _, err := cache.Get(context.Background()); err == nil {
		t.Fatal("Get without a snapshot succeeded; want collect error")
	}
}

func TestCacheBoundsFailedCollectionsUntilNextRefresh(t *testing.T) {
	for _, seed := range []bool{false, true} {
		t.Run(fmt.Sprintf("snapshot=%v", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				failure := errors.New("codexbar unavailable")
				cache := NewCache(func(context.Context) (json.RawMessage, error) {
					calls++
					if seed && calls == 1 {
						return json.RawMessage(`1`), nil
					}
					return nil, failure
				}, time.Minute)
				if seed {
					if _, err := cache.Get(context.Background()); err != nil {
						t.Fatal(err)
					}
					time.Sleep(time.Minute) // advance the cache's freshness clock
				}
				for range 10 {
					data, err := cache.Get(context.Background())
					if seed && (string(data) != "1" || err != nil) {
						t.Fatalf("lost stale snapshot: %s, %v", data, err)
					}
					if !seed && !errors.Is(err, failure) {
						t.Fatalf("Get error = %v, want collection error", err)
					}
					synctest.Wait()
				}
				want := 1
				if seed {
					want++
				}
				if calls != want {
					t.Fatalf("%d collections; want %d despite repeated requests", calls, want)
				}
				time.Sleep(time.Minute) // permit the next collection attempt
				_, _ = cache.Get(context.Background())
				synctest.Wait()
				if calls != want+1 {
					t.Fatalf("collection did not retry after cooldown: %d calls", calls)
				}
			})
		})
	}
}

func TestCacheSharesCollectionAndSurvivesCancelledWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		calls := 0
		cache := NewCache(func(ctx context.Context) (json.RawMessage, error) {
			calls++
			select {
			case <-release:
				return json.RawMessage(`42`), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}, time.Minute)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cancelled := make(chan error, 1)
		go func() { _, err := cache.Get(ctx); cancelled <- err }()
		results := make(chan json.RawMessage, 8)
		for range 8 {
			go func() { data, _ := cache.Get(context.Background()); results <- data }()
		}
		synctest.Wait()
		cancel()
		if err := <-cancelled; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter = %v", err)
		}
		close(release)
		for range 8 {
			if data := <-results; string(data) != "42" {
				t.Fatalf("shared snapshot = %s", data)
			}
		}
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("concurrent requests ran %d collections", calls)
		}
	})
}
