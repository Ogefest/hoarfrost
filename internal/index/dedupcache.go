package index

import (
	"container/list"
	"sync"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/recorder"
)

// dedupCacheSize bounds the deduplication cache. An entry takes a few
// hundred bytes, so the cache stays within tens of megabytes.
const dedupCacheSize = 100_000

type dedupKey struct {
	storage string
	epoch   epoch.ID
	digest  string
}

type dedupEntry struct {
	key dedupKey
	ref recorder.Ref
}

// dedupCache remembers the records found by Lookup. The same resources
// (scripts, styles, logos) are looked up for most captures of a site and
// matching them in the records table gets slower with every revisit, while
// the answer stays valid: a committed record is only removed together with
// its epoch (DeleteEpoch), which drops the epoch's entries. Misses are not
// cached, since the record may be committed later.
type dedupCache struct {
	mu    sync.Mutex
	max   int
	order *list.List // front: most recently used
	items map[dedupKey]*list.Element
}

func newDedupCache(max int) *dedupCache {
	return &dedupCache{max: max, order: list.New(), items: map[dedupKey]*list.Element{}}
}

func (c *dedupCache) get(k dedupKey) (recorder.Ref, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return recorder.Ref{}, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*dedupEntry).ref, true
}

func (c *dedupCache) put(k dedupKey, ref recorder.Ref) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.order.MoveToFront(el)
		return // keep the record found first
	}
	c.items[k] = c.order.PushFront(&dedupEntry{key: k, ref: ref})
	for c.order.Len() > c.max {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*dedupEntry).key)
	}
}

// dropEpoch forgets the entries of an epoch of a storage.
func (c *dedupCache) dropEpoch(storage string, ep epoch.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, el := range c.items {
		if k.storage == storage && k.epoch == ep {
			c.order.Remove(el)
			delete(c.items, k)
		}
	}
}
