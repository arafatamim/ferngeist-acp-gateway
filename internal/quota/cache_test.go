package quota

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCacheServesStaleSnapshotWhileRefreshingAndKeepsItOnFailure(t *testing.T) {
	type reply struct {
		data string
		err  error
	}
	replies := make(chan reply)
	cache := NewCache(func(context.Context) (json.RawMessage, error) {
		r := <-replies
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

	go func() { replies <- reply{data: "1"} }()
	if data, err := get(); err != nil || string(data) != "1" {
		t.Fatalf("first Get = %s, %v; want collected snapshot", data, err)
	}
	// The snapshot is stale (maxAge 0); Get must not wait for the refresh it starts.
	if data, err := get(); err != nil || string(data) != "1" {
		t.Fatalf("stale Get = %s, %v; want old snapshot", data, err)
	}
	replies <- reply{err: errors.New("codexbar failed")}
	if data, err := get(); err != nil || string(data) != "1" {
		t.Fatalf("Get after failed refresh = %s, %v; want old snapshot", data, err)
	}
	replies <- reply{data: "2"}
	deadline := time.Now().Add(time.Second)
	for {
		if data, _ := get(); string(data) == "2" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("refreshed snapshot never served")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCacheReturnsCollectErrorWhenNoSnapshotExists(t *testing.T) {
	cache := NewCache(func(context.Context) (json.RawMessage, error) {
		return nil, errors.New("codexbar missing")
	}, time.Minute)
	if _, err := cache.Get(context.Background()); err == nil {
		t.Fatal("Get without a snapshot succeeded; want collect error")
	}
}
