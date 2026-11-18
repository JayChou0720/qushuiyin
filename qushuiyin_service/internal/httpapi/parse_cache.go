package httpapi

import (
	"container/list"
	"context"
	"sync"
	"time"

	"qushuiyin/service/internal/parse"
)

// parseCache shares normalized fixture data. Job, result, and asset IDs are assigned separately.
type parseCache struct {
	mu       sync.Mutex
	byKey    map[string]*list.Element
	recent   list.List
	pending  map[string]*pendingParse
	ttl      time.Duration
	capacity int
}

type cachedParse struct {
	key     string
	value   *parse.Result
	expires time.Time
}

type pendingParse struct {
	done      chan struct{}
	value     *parse.Result
	err       error
	abandoned bool
}

func newParseCache(ttl time.Duration, capacity int) *parseCache {
	return &parseCache{byKey: make(map[string]*list.Element), pending: make(map[string]*pendingParse), ttl: ttl, capacity: capacity}
}

func (c *parseCache) resolve(ctx context.Context, key string, refresh bool, load func() (*parse.Result, error)) (*parse.Result, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if item := c.byKey[key]; item != nil && !refresh {
			cached := item.Value.(cachedParse)
			if time.Now().Before(cached.expires) {
				c.recent.MoveToFront(item)
				c.mu.Unlock()
				return cached.value, nil
			}
			c.remove(item)
		}
		if flight := c.pending[key]; flight != nil {
			c.mu.Unlock()
			select {
			case <-flight.done:
				if refresh || flight.abandoned {
					continue
				}
				return flight.value, flight.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		flight := &pendingParse{done: make(chan struct{})}
		c.pending[key] = flight
		c.mu.Unlock()

		parsed, err := load()
		c.mu.Lock()
		flight.value, flight.err, flight.abandoned = parsed, err, ctx.Err() != nil
		if err == nil && !flight.abandoned && c.capacity > 0 && c.ttl > 0 {
			now := time.Now()
			if previous := c.byKey[key]; previous != nil {
				c.remove(previous)
			}
			if len(c.byKey) >= c.capacity {
				c.evict(now)
			}
			c.byKey[key] = c.recent.PushFront(cachedParse{key, parsed, now.Add(c.ttl)})
		}
		delete(c.pending, key)
		close(flight.done)
		c.mu.Unlock()
		return parsed, err
	}
}

func (c *parseCache) remove(item *list.Element) {
	delete(c.byKey, item.Value.(cachedParse).key)
	c.recent.Remove(item)
}

func (c *parseCache) evict(now time.Time) {
	oldest := c.recent.Back()
	for item := oldest; item != nil; item = item.Prev() {
		if !now.Before(item.Value.(cachedParse).expires) {
			c.remove(item)
			return
		}
	}
	c.remove(oldest)
}
