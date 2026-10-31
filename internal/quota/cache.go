package quota

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Cache serves the last collected snapshot immediately and refreshes it in the
// background once it is older than maxAge, so providers are only polled while
// clients are asking. A failed refresh keeps the previous snapshot.
type Cache struct {
	collect func(context.Context) (json.RawMessage, error)
	maxAge  time.Duration
	group   singleflight.Group
	mu      sync.Mutex
	data    json.RawMessage
	at      time.Time
}

func NewCache(collect func(context.Context) (json.RawMessage, error), maxAge time.Duration) *Cache {
	return &Cache{collect: collect, maxAge: maxAge}
}

// Get returns the cached snapshot, collecting synchronously only when none exists yet.
func (c *Cache) Get(ctx context.Context) (json.RawMessage, error) {
	c.mu.Lock()
	data, stale := c.data, time.Since(c.at) >= c.maxAge
	c.mu.Unlock()
	if data != nil {
		if stale {
			// The buffered result is dropped; refresh stores the snapshot itself.
			c.group.DoChan("", c.refresh)
		}
		return data, nil
	}
	select {
	case result := <-c.group.DoChan("", c.refresh):
		if result.Err != nil {
			return nil, result.Err
		}
		return result.Val.(json.RawMessage), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// refresh runs detached from any request so a disconnecting client cannot cancel the shared collection.
func (c *Cache) refresh() (any, error) {
	data, err := c.collect(context.Background())
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.data, c.at = data, time.Now()
	c.mu.Unlock()
	return data, nil
}
