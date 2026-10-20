package render

import (
	"container/list"
	"fmt"
	"sync"
)

// cacheBytes caps the rendered output kept across all sessions.
const cacheBytes = 32 << 20

type cacheKey struct {
	id    any
	base  string
	width int
	dark  bool
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
	// inflight holds renders in progress, so sessions that ask for the
	// same document at once wait for one render instead of each doing it.
	inflight map[cacheKey]*render
}

type render struct {
	done chan struct{}
	doc  Document
	err  error
}

var shared = newDocCache(cacheBytes)

// renderMarkdown is Markdown, swappable in tests.
var renderMarkdown = Markdown

func newDocCache(maxBytes int) *docCache {
	return &docCache{max: maxBytes, order: list.New(), items: map[cacheKey]*list.Element{}, inflight: map[cacheKey]*render{}}
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

func (c *docCache) put(k cacheKey, doc Document, extra int) {
	size := len(doc.Content)
	for _, l := range doc.Links {
		size += len(l.URL)
	}
	size += extra
	if size > c.max/2 {
		return // too big to hold without pushing out everything else
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

// Cached is Markdown with a cache shared by all callers. id names the
// document, and must change whenever md does. A content entry pointer
// works, since a refresh makes new entries.
func Cached(id any, md string, opts Options) (doc Document, err error) {
	k := cacheKey{id: id, base: opts.BaseURL, width: opts.Width, dark: opts.Dark}
	// Look in the cache and at renders in progress under one lock, so a
	// render that finishes in between isn't done twice.
	shared.mu.Lock()
	if el, ok := shared.items[k]; ok {
		shared.order.MoveToFront(el)
		doc = el.Value.(*cacheEntry).doc
		shared.mu.Unlock()
		return doc, nil
	}
	if r, ok := shared.inflight[k]; ok {
		shared.mu.Unlock()
		<-r.done
		return r.doc, r.err
	}
	r := &render{done: make(chan struct{})}
	shared.inflight[k] = r
	shared.mu.Unlock()

	// Waiters must be released even if rendering panics.
	defer func() {
		if p := recover(); p != nil {
			r.err = fmt.Errorf("render: panic: %v", p)
			doc, err = Document{}, r.err
		}
		shared.mu.Lock()
		delete(shared.inflight, k)
		shared.mu.Unlock()
		close(r.done)
	}()
	r.doc, r.err = renderMarkdown(md, opts)
	if r.err == nil {
		// The id keeps md alive while cached, so md counts too.
		shared.put(k, r.doc, len(md))
	}
	return r.doc, r.err
}
