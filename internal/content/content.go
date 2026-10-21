// Package content builds a readable snapshot of a website from what it already
// publishes: robots.txt, feeds, sitemaps and the pages themselves.
package content

import (
	"net/url"
	"strings"
	"time"

	"github.com/burraksumer/sshr/internal/fetch"
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
	// fetchedAt is when the page was last fetched or confirmed unchanged.
	fetchedAt time.Time
	bodyHash  uint64
	hashed    bool // bodyHash is set for the current Body
	// stale counts refreshes in a row that kept this copy because the page
	// couldn't be read.
	stale int
	// base is the URL the page was read from after redirects, which its
	// relative links resolve against.
	base string
	// textKey identifies a text-only entry, which has no URL.
	textKey string
	// order is the entry's place in the manifest's page list, from 1, or 0.
	order int
}

// renderID identifies an entry's content across refreshes.
type renderID struct {
	url  string
	hash uint64
}

// Key identifies the entry within a site across refreshes: its URL, or for
// a text-only feed item, its GUID, title and date.
func (e *Entry) Key() string {
	if e.URL != "" {
		return e.URL
	}
	return e.textKey
}

// BaseURL is what the entry's links resolve against: the page's final URL
// after redirects, or the entry URL.
func (e *Entry) BaseURL() string {
	if e.base != "" {
		return e.base
	}
	return e.URL
}

// RenderID is a comparable key for caching renders of the entry. It stays
// the same across refreshes while the URL and body don't change.
func (e *Entry) RenderID() any {
	return renderID{e.Key(), e.bodyHash}
}

// Site is an immutable snapshot of a website's content. Sessions share it and
// never modify it, so it is safe to read without locks.
type Site struct {
	URL         string
	Title       string
	Description string
	Entries     []*Entry
	// Logo is ASCII art to show above the title, without control
	// characters. Accent is a "#rrggbb" color or "". Nav is the menu. All
	// come from the site manifest and may be empty.
	Logo   []string
	Accent string
	Nav    []Nav

	// Manifest is whether the site publishes a manifest at ManifestPath.
	Manifest bool

	// remoteManifest is the manifest the site published, kept for a
	// conditional request and reused when the fetch fails for a moment.
	remoteManifest *Manifest

	feedURL                    string
	feedStale                  int // refreshes in a row the feed was unreadable
	feedTitle, feedDescription string
	// feedEntries holds every entry from the feed, including those dropped
	// because their page couldn't be fetched, so a refresh that gets 304
	// for the feed can retry them.
	feedEntries []*Entry
	// home is what the homepage's head said, reused on a 304 or an outage.
	home *headMeta
	// robotsSitemaps is the sitemap list from robots.txt, reused when
	// robots.txt can't be fetched.
	robotsSitemaps []string
	// sitemaps holds parsed sitemaps by URL for conditional requests.
	sitemaps map[string]cachedSitemap
	// probeAge counts refreshes since the common feed paths were probed.
	probeAge int
	byURL    map[string]*Entry
}

// Lookup returns the entry for rawURL, or nil if the site has none.
func (s *Site) Lookup(rawURL string) *Entry {
	k, ok := key(rawURL)
	if !ok {
		return nil
	}
	if s.byURL != nil {
		return s.byURL[k]
	}
	// A site built by hand has no index.
	for _, e := range s.Entries {
		if ek, ok := key(e.URL); ok && e.URL != "" && ek == k {
			return e
		}
	}
	return nil
}

func (s *Site) index() {
	s.byURL = make(map[string]*Entry, len(s.Entries))
	for _, e := range s.Entries {
		// Text-only entries have no URL to be found by.
		if e.URL == "" {
			continue
		}
		if k, ok := key(e.URL); ok {
			s.byURL[k] = e
		}
	}
}

// key is the comparison key for a raw URL, see normalize.
func key(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	return normalize(u), true
}

// normalize returns a comparison key for u. It uses https and a lowercase
// host without "www.", and drops the fragment and any trailing slash. sshr
// treats a site's www and bare hosts as the same site.
func normalize(u *url.URL) string {
	c := *u
	c.Scheme = strings.ToLower(c.Scheme)
	// Hosts compare in the form fetching uses, so links written with a
	// Unicode host find entries read from the punycode one.
	c.Host = strings.TrimPrefix(fetch.ASCIIHost(u).Host, "www.")
	c.Fragment = ""
	c.RawFragment = ""
	c.Path = strings.TrimSuffix(c.Path, "/")
	c.RawPath = ""
	if c.Scheme == "http" {
		c.Scheme = "https"
	}
	return c.String()
}
