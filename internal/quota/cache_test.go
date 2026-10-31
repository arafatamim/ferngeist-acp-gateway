package quota

import (
	"context"
	"encoding/json"
	"errors"
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
