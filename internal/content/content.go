// Package content builds a readable snapshot of a website from what it already
// publishes: robots.txt, feeds, sitemaps and the pages themselves.
package content

import (
	"net/url"
	"strings"
	"time"
)

// Kind says whether an entry is a dated post or a standalone page.
type Kind int

const (
	Post Kind = iota
	Page
)

// Entry is one readable document. Every string field is sanitized.
type Entry struct {
	URL     string
	Title   string
	Summary string
	Date    time.Time
	Tags    []string
	Kind    Kind
	// Body is the document as markdown.
	Body string

	fromFeed bool
	// pageBody means Body came from fetching the page, not from the feed.
	pageBody bool
}

// Site is an immutable snapshot of a website's content. Sessions share it and
// never modify it, so it is safe to read without locks.
type Site struct {
	URL         string
	Title       string
	Description string
	Entries     []*Entry
	FetchedAt   time.Time

	feedURL                    string
	feedTitle, feedDescription string
	// feedEntries holds every entry from the feed, including those dropped
	// because their page couldn't be fetched, so a refresh that gets 304
	// for the feed can retry them.
	feedEntries []*Entry
	// sitemaps holds parsed sitemaps by URL for conditional requests.
	sitemaps map[string]cachedSitemap
	// probeAge counts refreshes since the common feed paths were probed.
	probeAge int
	byURL    map[string]*Entry
}

// Lookup returns the entry for rawURL, or nil if the site has none.
func (s *Site) Lookup(rawURL string) *Entry {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	return s.byURL[normalize(u)]
}

func (s *Site) index() {
	s.byURL = make(map[string]*Entry, len(s.Entries))
	for _, e := range s.Entries {
		if u, err := url.Parse(e.URL); err == nil {
			s.byURL[normalize(u)] = e
		}
	}
}

// normalize returns a comparison key for u. It uses https and a lowercase
// host without "www.", and drops the fragment and any trailing slash. sshr
// treats a site's www and bare hosts as the same site.
func normalize(u *url.URL) string {
	c := *u
	c.Scheme = strings.ToLower(c.Scheme)
	c.Host = strings.TrimPrefix(strings.ToLower(c.Host), "www.")
	c.Fragment = ""
	c.RawFragment = ""
	c.Path = strings.TrimSuffix(c.Path, "/")
	c.RawPath = ""
	if c.Scheme == "http" {
		c.Scheme = "https"
	}
	return c.String()
}
