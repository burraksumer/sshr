package render

import (
	"container/list"
	"sync"
)

// cacheBytes caps the rendered output kept across all sessions.
const cacheBytes = 32 << 20

type cacheKey struct {
	md, base string
	width    int
	dark     bool
}

type cacheEntry struct {
	key  cacheKey
	doc  Document
	size int
}

// docCache is a least-recently-used cache of rendered documents, shared by
// every session, so visitors reading the same entry at the same width
// render it once.
type docCache struct {
	mu    sync.Mutex
	max   int
	size  int
	order *list.List // front is most recently used
	items map[cacheKey]*list.Element
}

var shared = newDocCache(cacheBytes)

func newDocCache(maxBytes int) *docCache {
	return &docCache{max: maxBytes, order: list.New(), items: map[cacheKey]*list.Element{}}
}

func (c *docCache) get(k cacheKey) (Document, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return Document{}, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheEntry).doc, true
}

func (c *docCache) put(k cacheKey, doc Document) {
	// The key holds the markdown, which keeps it alive, so it counts too.
	size := len(k.md) + len(doc.Content)
	for _, l := range doc.Links {
		size += len(l.URL) + len(l.Text)
	}
	if size > c.max/4 {
		return // too big to be worth holding
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[k]; ok {
		return
	}
	c.items[k] = c.order.PushFront(&cacheEntry{key: k, doc: doc, size: size})
	c.size += size
	for c.size > c.max {
		el := c.order.Back()
		e := el.Value.(*cacheEntry)
		c.order.Remove(el)
		delete(c.items, e.key)
		c.size -= e.size
	}
}

// Cached is Markdown with a cache shared by all callers.
func Cached(md string, opts Options) (Document, error) {
	k := cacheKey{md: md, base: opts.BaseURL, width: opts.Width, dark: opts.Dark}
	if doc, ok := shared.get(k); ok {
		return doc, nil
	}
	doc, err := Markdown(md, opts)
	if err == nil {
		shared.put(k, doc)
	}
	return doc, err
}
