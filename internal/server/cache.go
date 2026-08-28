package server

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/stephenwilliams/s3-registry/internal/index"
)

// indexFetcher is the store dependency the cache needs.
type indexFetcher interface {
	GetIndex(ctx context.Context, tool string) (*index.Index, string, error)
}

type cacheEntry struct {
	idx       *index.Index
	fetchedAt time.Time
}

// indexCache is a per-tool TTL cache. Concurrent misses collapse into a single
// upstream GetIndex via singleflight.
type indexCache struct {
	store indexFetcher
	ttl   time.Duration

	mu      sync.RWMutex
	entries map[string]cacheEntry
	sf      singleflight.Group
	now     func() time.Time
}

func newIndexCache(st indexFetcher, ttl time.Duration) *indexCache {
	return &indexCache{
		store:   st,
		ttl:     ttl,
		entries: map[string]cacheEntry{},
		now:     time.Now,
	}
}

// Get returns the tool's index, serving a fresh cached copy when within TTL.
func (c *indexCache) Get(ctx context.Context, tool string) (*index.Index, error) {
	c.mu.RLock()
	e, ok := c.entries[tool]
	c.mu.RUnlock()
	if ok && c.now().Sub(e.fetchedAt) < c.ttl {
		return e.idx, nil
	}

	v, err, _ := c.sf.Do(tool, func() (any, error) {
		idx, _, err := c.store.GetIndex(ctx, tool)
		if err != nil {
			return nil, err
		}
		// Sort once here so cached indexes are effectively read-only; request
		// handlers must not mutate the shared pointer afterwards.
		idx.SortVersions()
		c.mu.Lock()
		c.entries[tool] = cacheEntry{idx: idx, fetchedAt: c.now()}
		c.mu.Unlock()
		return idx, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*index.Index), nil
}
