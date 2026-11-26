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
// clients are asking. A snapshot older than maxServedAge waits for its refresh.
// A failed refresh keeps the previous snapshot and retries no sooner than
// maxAge, even if there is no snapshot yet.
type Cache struct {
	collect func(context.Context) (json.RawMessage, error)
	maxAge  time.Duration
	// Providers are only polled while clients ask, so after an idle stretch the snapshot
	// is as old as the idle time; serving it would show usage from that long ago.
	maxServedAge time.Duration
	group        singleflight.Group
	mu           sync.Mutex
	data         json.RawMessage
	at           time.Time
	err          error
}

func NewCache(collect func(context.Context) (json.RawMessage, error), maxAge, maxServedAge time.Duration) *Cache {
	return &Cache{collect: collect, maxAge: maxAge, maxServedAge: maxServedAge}
}

// Get returns the cached snapshot, collecting synchronously when none exists yet
// or the last one is older than maxServedAge.
func (c *Cache) Get(ctx context.Context) (json.RawMessage, error) {
	c.mu.Lock()
	data, age, err := c.data, time.Since(c.at), c.err
	c.mu.Unlock()
	if data != nil && age < c.maxServedAge {
		if age >= c.maxAge {
			// The buffered result is dropped; refresh stores the snapshot itself.
			c.group.DoChan("", c.refresh)
		}
		return data, nil
	}
	if data == nil && age < c.maxAge {
		return nil, err
	}
	select {
	case result := <-c.group.DoChan("", c.refresh):
		if result.Err != nil && data != nil {
			return data, nil
		}
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
	// A Get can observe stale data just before another refresh completes and
	// enter singleflight just after it leaves. Recheck freshness here too.
	c.mu.Lock()
	if time.Since(c.at) < c.maxAge {
		data, err := c.data, c.err
		c.mu.Unlock()
		return data, err
	}
	c.mu.Unlock()
	data, err := c.collect(context.Background())
	c.mu.Lock()
	c.at, c.err = time.Now(), err
	if err == nil {
		c.data = data
	}
	c.mu.Unlock()
	return data, err
}
