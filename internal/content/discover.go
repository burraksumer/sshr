package content

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	readability "codeberg.org/readeck/go-readability/v2"
	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/burraksumer/sshr/internal/fetch"
	"github.com/burraksumer/sshr/internal/sanitize"
	"github.com/mmcdole/gofeed"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const (
	acceptHTML = "text/html, application/xhtml+xml;q=0.9, */*;q=0.1"
	acceptPage = "text/markdown, text/html;q=0.9, application/xhtml+xml;q=0.9, */*;q=0.1"
	acceptFeed = "application/feed+json, application/atom+xml, application/rss+xml, application/xml;q=0.9, text/xml;q=0.9, */*;q=0.1"
	acceptXML  = "application/xml, text/xml;q=0.9, */*;q=0.1"
)

// Feed paths tried after the feeds the homepage links, relative to the site's
// path and then to the root.
var fallbackFeeds = []string{"feed.json", "feed.xml", "atom.xml", "rss.xml", "index.xml", "feed"}

// maxBody caps an entry's markdown. Longer bodies are cut, so one huge page
// can't make every render slow.
const maxBody = 1 << 20

// capBody cuts s to maxBody bytes at a line break and notes the cut.
func capBody(s string) string {
	if len(s) <= maxBody {
		return s
	}
	cut := s[:maxBody]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return strings.ToValidUTF8(cut, "") + "\n\n*This entry was cut short because it is very long.*\n"
}

// maxStale is how many refreshes in a row a copy is kept for a page that
// answers but can't be read.
const maxStale = 8

// DefaultPageBudget is how long a refresh spends fetching pages unless
// configured otherwise. With a long Crawl-delay, the rest come in later
// refreshes.
const DefaultPageBudget = 10 * time.Minute

// DefaultMaxEntries is how many posts and pages a site keeps unless
// configured otherwise.
const DefaultMaxEntries = 200

// reprobeEvery is how many refreshes pass between probes of the common feed
// paths on a site that has no feed, about six hours at the default interval.
const reprobeEvery = 24

// scope is the directory the site lives in: "/" for a whole domain, or
// "/blog/" when the site URL is example.com/blog.
func (d *Discoverer) scope() string {
	p := d.Base.Path
	// A URL that names a page file, like /blog/index.html, means its
	// directory. Other dots, as in /~jane.doe or /docs/v2.0, are part of
	// the directory name.
	if pageExts[strings.ToLower(path.Ext(p))] {
		p = path.Dir(p)
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// atScopes resolves name under the site's directory and then under the root.
func (d *Discoverer) atScopes(scope, name string) []string {
	out := []string{d.Base.ResolveReference(&url.URL{Path: scope + name}).String()}
	if scope != "/" {
		out = append(out, d.Base.ResolveReference(&url.URL{Path: "/" + name}).String())
	}
	return out
}

// inScope reports whether u is inside the site's directory.
func inScope(u *url.URL, scope string) bool {
	return scope == "/" || strings.HasPrefix(u.Path, scope) || u.Path+"/" == scope
}

// Discoverer builds a Site for one origin.
type Discoverer struct {
	Client *fetch.Client
	Base   *url.URL
	// PageBudget bounds how long one refresh spends fetching pages.
	PageBudget time.Duration
	// MaxEntries caps how many documents a site may have.
	MaxEntries int
	Log        *slog.Logger
}

// Discover fetches the site and returns a new snapshot. prev is the last
// snapshot, used for conditional requests, and may be nil. When prev is nil
// and publish is set, Discover calls publish with a partial snapshot as soon
// as the entries that came with full content are ready, before fetching
// individual pages.
func (d *Discoverer) Discover(ctx context.Context, prev *Site, publish func(*Site)) (*Site, error) {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	maxEntries := d.MaxEntries
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	d.Client.StartRound()
	defer d.Client.EndRound()
	scope := d.scope()

	sitemaps, err := d.Client.LoadRobots(ctx, d.Base)
	if err != nil {
		log.Warn("robots.txt", "err", err)
		// Use last round's sitemap list, so cached sitemaps are found.
		if prev != nil {
			sitemaps = prev.robotsSitemaps
		}
	}

	site := &Site{URL: d.Base.String(), Title: d.Base.Hostname(), robotsSitemaps: sitemaps}
	var feedLinks []feedLink
	// The homepage is asked for conditionally, and its head is reused when
	// it hasn't changed.
	var meta *headMeta
	res, err := d.Client.Get(ctx, d.Base, acceptHTML, prev != nil && prev.home != nil)
	switch {
	case err == nil && res.NotModified && prev != nil:
		meta = prev.home
	case err == nil && res.Status == 200:
		m := parseHead(res.Body)
		for i, f := range m.feeds {
			if u, err := res.URL.Parse(f.href); err == nil {
				m.feeds[i].href = sanitize.URL(u.String())
			}
		}
		meta = &m
	default:
		if err != nil {
			log.Warn("homepage", "err", err)
		}
		// Keep last round's head rather than showing the bare host name
		// while the homepage is down.
		if prev != nil {
			meta = prev.home
		}
	}
	site.home = meta
	if meta != nil {
		if meta.title != "" {
			site.Title = meta.title
		}
		site.Description = meta.description
		feedLinks = meta.feeds
	}

	// Feeds give posts with dates and, often, full content.
	entries, feedURL := d.loadFeed(ctx, site, feedLinks, prev, scope, log)
	site.feedURL = feedURL
	// Some feeds list oldest first. Keep the newest posts when cutting.
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i].Date, entries[j].Date
		return !a.IsZero() && (b.IsZero() || a.After(b))
	})
	if len(entries) > maxEntries {
		entries = entries[:maxEntries]
	}
	// feedEntries keeps what the feed said. Copies, so titles and dates
	// that page fetches fill in below don't end up in it.
	feedEntries := make([]*Entry, len(entries))
	for i, e := range entries {
		c := *e
		feedEntries[i] = &c
	}

	// Posts that came with full content can be shown before sitemaps and
	// pages are fetched, which can take minutes on a slow site.
	if prev == nil && publish != nil {
		var ready []*Entry
		for _, e := range entries {
			if strings.TrimSpace(e.Body) != "" {
				c := *e
				ready = append(ready, &c)
			}
		}
		if len(ready) > 0 {
			partial := *site
			publish(finish(&partial, ready))
		}
	}

	// The sitemap adds pages the feed doesn't list.
	seen := map[string]bool{normalize(d.Base): true}
	// When the site URL names a file, its directory is the same listing.
	seen[normalize(d.Base.ResolveReference(&url.URL{Path: scope}))] = true
	if feedURL != "" {
		if u, err := url.Parse(feedURL); err == nil {
			seen[normalize(u)] = true
		}
	}
	for _, e := range entries {
		if u, err := url.Parse(e.URL); err == nil {
			seen[normalize(u)] = true
		}
	}
	if room := maxEntries - len(entries); room > 0 {
		pages, cache := d.loadSitemaps(ctx, sitemaps, prev, scope, log)
		site.sitemaps = cache
		for _, p := range pages {
			if room == 0 {
				break
			}
			// Encoded like feed entry URLs, so the same URL matches.
			raw := sanitize.URL(p.loc.String())
			loc, err := url.Parse(raw)
			if err != nil {
				continue
			}
			key := normalize(loc)
			if seen[key] || !d.Client.MayFetch(ctx, p.loc) {
				continue
			}
			seen[key] = true
			entries = append(entries, &Entry{URL: raw, Kind: Page, Date: p.lastmod})
			room--
		}
	}

	d.fillBodies(ctx, entries, prev, log)
	// An interrupted refresh would publish a cut-down snapshot.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	site.feedEntries = feedEntries
	// Anything kept from last round without a request keeps its
	// validators: pages, sitemaps and the feed.
	for _, e := range entries {
		d.Client.Keep(e.URL)
	}
	for raw := range site.sitemaps {
		d.Client.Keep(raw)
	}
	d.Client.Keep(site.feedURL)

	var kept []*Entry
	for _, e := range entries {
		if strings.TrimSpace(e.Body) != "" {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		return nil, errors.New("no readable content found. The site has no feed, sitemap or extractable pages")
	}
	return finish(site, kept), nil
}

// finish sorts entries, fills missing titles and indexes the site.
func finish(site *Site, kept []*Entry) *Site {
	for _, e := range kept {
		h := fnv.New64a()
		h.Write([]byte(e.Body))
		e.bodyHash = h.Sum64()
		if e.Title == "" {
			// The URL path is percent-decoded and can hold control bytes.
			e.Title = sanitize.Line(titleFromURL(e.URL))
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if a.Kind != b.Kind {
			return a.Kind == Post
		}
		if a.Kind == Post && !a.Date.Equal(b.Date) {
			return a.Date.After(b.Date)
		}
		if a.Kind == Page {
			return strings.ToLower(a.Title) < strings.ToLower(b.Title)
		}
		return false
	})
	site.Entries = kept
	site.FetchedAt = time.Now()
	site.index()
	return site
}

// loadFeed tries feed candidates in order and returns the entries of the first
// one that parses with at least one item.
func (d *Discoverer) loadFeed(ctx context.Context, site *Site, links []feedLink, prev *Site, scope string, log *slog.Logger) ([]*Entry, string) {
	// Advertised feeds first, then the common paths, in case the advertised
	// ones are broken.
	candidates := sortFeeds(links)
	// The feed that worked last time is tried even when the homepage
	// couldn't be read this round, but after any feeds the homepage
	// advertises, which may have become better.
	if prev != nil && prev.feedURL != "" && !slices.Contains(candidates, prev.feedURL) {
		candidates = append(candidates, prev.feedURL)
	}
	// A site that had no feed last time isn't probed at the common paths on
	// every refresh, only every reprobeEvery refreshes.
	if prev == nil || prev.feedURL != "" || prev.probeAge >= reprobeEvery {
		for _, p := range fallbackFeeds {
			for _, c := range d.atScopes(scope, p) {
				if !slices.Contains(candidates, c) {
					candidates = append(candidates, c)
				}
			}
		}
	} else {
		site.probeAge = prev.probeAge + 1
	}
	parser := gofeed.NewParser()
	prevFailed, garbage := false, false
	for _, raw := range candidates {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		// Only ask for a 304 when there are feed entries to fall back on.
		conditional := prev != nil && prev.feedURL == raw && prev.feedEntries != nil
		res, err := d.Client.GetLarge(ctx, u, acceptFeed, conditional)
		if err != nil {
			log.Debug("feed", "url", raw, "err", err)
			if prev != nil && raw == prev.feedURL && !permanent(err) {
				prevFailed = true
			}
			continue
		}
		if res.NotModified && prev != nil {
			return reuseFeed(site, prev), raw
		}
		if res.Status != 200 {
			if prev != nil && raw == prev.feedURL && transient(res.Status) {
				prevFailed = true
			}
			continue
		}
		feed, err := parser.Parse(bytes.NewReader(res.Body))
		if err != nil {
			// An HTML maintenance page in place of the feed, say. Keep the
			// old entries for a few refreshes, not forever.
			d.Client.Forget(raw)
			if prev != nil && raw == prev.feedURL && prev.feedStale < maxStale {
				prevFailed, garbage = true, true
			}
			continue
		}
		if len(feed.Items) == 0 {
			continue
		}
		entries := make([]*Entry, 0, len(feed.Items))
		seenItem := map[string]bool{}
		for _, item := range feed.Items {
			e := entryFromItem(item, res.URL)
			if e == nil {
				continue
			}
			// Feeds sometimes list a post twice, or under both www and bare
			// hosts. Keep the first.
			if u, err := url.Parse(e.URL); e.URL != "" && err == nil {
				if key := normalize(u); seenItem[key] {
					continue
				} else {
					seenItem[key] = true
				}
			}
			// A site-wide feed found from a /blog site only counts for
			// its /blog posts. Items without a URL can't be placed, so a
			// scoped site leaves them out.
			if e.URL == "" {
				if scope != "/" {
					continue
				}
			} else if u, err := url.Parse(e.URL); err != nil || !inScope(u, scope) {
				continue
			}
			entries = append(entries, e)
		}
		if len(entries) == 0 {
			continue
		}
		applyFeedMeta(site, sanitize.Line(feed.Title), sanitize.Line(feed.Description))
		log.Info("feed", "url", raw, "items", len(entries))
		return entries, raw
	}
	// The feed that worked last time failed for a moment. Keep its entries
	// rather than dropping every post until the next refresh.
	if prevFailed && prev.feedEntries != nil {
		log.Warn("feed unavailable, keeping previous entries", "url", prev.feedURL)
		kept := reuseFeed(site, prev)
		site.feedStale = prev.feedStale
		if garbage {
			site.feedStale++
		}
		return kept, prev.feedURL
	}
	// The feed went away, for good or for a moment. Probe the common paths
	// again next refresh instead of waiting reprobeEvery refreshes.
	if prev != nil && prev.feedURL != "" {
		site.probeAge = reprobeEvery
	}
	return nil, ""
}

// reuseFeed returns copies of prev's feed entries and its feed title and
// description, for a feed that answered 304 or failed for a moment.
func reuseFeed(site, prev *Site) []*Entry {
	var kept []*Entry
	for _, e := range prev.feedEntries {
		c := *e
		kept = append(kept, &c)
	}
	applyFeedMeta(site, prev.feedTitle, prev.feedDescription)
	return kept
}

// applyFeedMeta records the feed's title and description. They win over the
// homepage's when set.
func applyFeedMeta(site *Site, title, description string) {
	site.feedTitle, site.feedDescription = title, description
	if title != "" {
		site.Title = title
	}
	if description != "" {
		site.Description = description
	}
}

// permanent reports whether a fetch error means the URL is off limits, as
// opposed to failing for a moment. A robots.txt that can't be fetched right
// now counts as a passing failure.
func permanent(err error) bool {
	if errors.Is(err, fetch.ErrRobotsUnavailable) {
		return false
	}
	return errors.Is(err, fetch.ErrDisallowed) || errors.Is(err, fetch.ErrNotAllowed)
}

// transient reports whether an HTTP status is worth keeping old data over.
func transient(status int) bool {
	return status >= 500 || status == http.StatusTooManyRequests
}

func entryFromItem(item *gofeed.Item, feedURL *url.URL) *Entry {
	e := &Entry{Kind: Post, fromFeed: true}
	link := item.Link
	if link == "" && strings.HasPrefix(item.GUID, "http") {
		link = item.GUID
	}
	if link != "" {
		if u, err := feedURL.Parse(link); err == nil {
			e.URL = sanitize.URL(u.String())
		}
	}
	e.Title = sanitize.Line(item.Title)
	switch {
	case item.PublishedParsed != nil:
		e.Date = *item.PublishedParsed
	case item.UpdatedParsed != nil:
		e.Date = *item.UpdatedParsed
	}
	for _, c := range item.Categories {
		if c = sanitize.Line(c); c != "" {
			e.Tags = append(e.Tags, c)
		}
	}
	base := e.URL
	if base == "" {
		base = feedURL.String()
	}
	if item.Description != "" {
		e.Summary = sanitize.Line(plainText(item.Description))
		e.Summary = truncate(e.Summary, 300)
	}
	// Full content wins. Some RSS feeds put the whole post in <description>,
	// so long descriptions count too. Otherwise the page gets fetched later.
	body := item.Content
	if body == "" && len(item.Description) > 1500 {
		body = item.Description
	}
	if body != "" {
		e.Body = capBody(sanitize.Text(toMarkdown(body, base)))
	}
	if e.URL == "" && e.Body == "" {
		return nil
	}
	return e
}

// fillBodies fetches the page for every entry that has no body yet.
func (d *Discoverer) fillBodies(ctx context.Context, entries []*Entry, prev *Site, log *slog.Logger) {
	const workers = 4
	budget := d.PageBudget
	if budget <= 0 {
		budget = DefaultPageBudget
	}
	deadline := time.Now().Add(budget)
	// Entries with no earlier copy go first, so a budget that runs out
	// doesn't keep skipping the same new entries every refresh.
	var todo []*Entry
	for _, e := range entries {
		if strings.TrimSpace(e.Body) == "" && e.URL != "" {
			todo = append(todo, e)
		}
	}
	// After those, the copies fetched longest ago, so the budget rotates
	// through the rest instead of always refetching the same ones.
	olds := make(map[*Entry]*Entry, len(todo))
	for _, e := range todo {
		if prev != nil {
			if old := prev.Lookup(e.URL); old != nil && old.Body != "" {
				olds[e] = old
			}
		}
	}
	sort.SliceStable(todo, func(i, j int) bool {
		a, b := olds[todo[i]], olds[todo[j]]
		switch {
		case a == nil || b == nil:
			return a == nil && b != nil
		}
		return a.fetchedAt.Before(b.fetchedAt)
	})
	jobs := make(chan *Entry)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for e := range jobs {
				if err := d.fillBody(ctx, e, olds[e]); err != nil {
					log.Debug("page", "url", e.URL, "err", err)
				}
			}
		})
	}
	// The budget also bounds waiting for a free worker.
	timer := time.NewTimer(time.Until(deadline))
	passed := false
	skipped := 0
	for _, e := range todo {
		// Past the budget, entries keep last refresh's copy, if there is
		// one, and get fetched in a later refresh.
		if !passed {
			select {
			case jobs <- e:
				continue
			case <-timer.C:
				passed = true
			case <-ctx.Done():
				continue
			}
		}
		skipped++
		if old := olds[e]; old != nil {
			reuse(e, old)
		}
	}
	timer.Stop()
	close(jobs)
	wg.Wait()
	if skipped > 0 {
		log.Info("page budget used up, the rest wait for the next refresh", "skipped", skipped)
	}
}

func (d *Discoverer) fillBody(ctx context.Context, e *Entry, old *Entry) error {
	u, err := url.Parse(e.URL)
	if err != nil {
		return err
	}
	hasOld := old != nil && old.Body != ""
	res, err := d.Client.Get(ctx, u, acceptPage, hasOld)
	switch {
	case keepPrevious(res, err, hasOld):
		reuse(e, old)
		if err != nil {
			return fmt.Errorf("kept previous copy: %w", err)
		}
		if res.NotModified {
			// Validators only survive readable responses, so a 304
			// means the copy is current.
			e.fetchedAt, e.stale = time.Now(), 0
		}
		return nil
	case err != nil:
		return err
	case res.Status != 200:
		return fmt.Errorf("status %d", res.Status)
	}
	if err := d.readPage(e, res); err != nil {
		// A 304 must not vouch for this response later.
		d.Client.Forget(e.URL)
		// The page answered but couldn't be read, which may not get
		// better. Keep the old copy for a few refreshes, not forever.
		if hasOld && old.stale < maxStale {
			reuse(e, old)
			e.stale = old.stale + 1
			return fmt.Errorf("kept previous copy: %w", err)
		}
		return err
	}
	// A post that left the feed, say pushed out by newer ones, stays a
	// dated post even when its page has no published time.
	if e.Kind == Page && old != nil && old.Kind == Post {
		e.Kind = Post
		if e.Date.IsZero() || !old.Date.IsZero() {
			e.Date = old.Date
		}
	}
	return nil
}

// keepPrevious reports whether a fetch result means an earlier copy should
// be kept: a 304, a 5xx or 429, or an error that isn't a refusal. It needs
// an earlier copy to keep.
func keepPrevious(res *fetch.Response, err error, hasOld bool) bool {
	switch {
	case !hasOld:
		return false
	case err != nil:
		return !permanent(err)
	}
	return res.NotModified || transient(res.Status)
}

// readPage fills e from a fetched page, as markdown or extracted HTML.
func (d *Discoverer) readPage(e *Entry, res *fetch.Response) error {

	switch res.ContentType {
	case "text/markdown", "text/x-markdown", "text/plain":
		body := sanitize.Text(string(res.Body))
		title, date, body := frontMatter(body)
		if e.Title == "" {
			e.Title = title
		}
		if e.Title == "" {
			e.Title = firstHeading(body)
		}
		if t := parseDate(date); !t.IsZero() {
			if e.Date.IsZero() || !e.fromFeed {
				e.Date = t
			}
			e.Kind = Post
		}
		e.Body, e.fetchedAt = capBody(body), time.Now()
		return nil
	case "text/html", "application/xhtml+xml", "":
	default:
		return fmt.Errorf("unsupported content type %q", res.ContentType)
	}

	article, err := readability.FromReader(bytes.NewReader(res.Body), res.URL)
	if err != nil {
		return err
	}
	if article.Node == nil {
		return errors.New("no article content")
	}
	md, err := htmltomarkdown.ConvertNode(article.Node, converter.WithDomain(res.URL.String()))
	if err != nil {
		return err
	}
	e.Body, e.fetchedAt = capBody(sanitize.Text(string(md))), time.Now()
	if e.Title == "" {
		e.Title = sanitize.Line(article.Title())
	}
	if e.Summary == "" {
		e.Summary = sanitize.Line(article.Excerpt())
	}
	// A page with a published date in its metadata is a post the feed
	// didn't include, such as an older one.
	if t, err := article.PublishedTime(); err == nil && !t.IsZero() {
		if e.Date.IsZero() || !e.fromFeed {
			e.Date = t
		}
		e.Kind = Post
	}
	return nil
}

// reuse copies old's page content into e, keeping the feed's own fields.
func reuse(e, old *Entry) {
	e.Body, e.fetchedAt, e.stale = old.Body, old.fetchedAt, old.stale
	// Feed entries are posts whatever the page looked like last time.
	if !e.fromFeed {
		e.Kind = old.Kind
	}
	if e.Title == "" {
		e.Title = old.Title
	}
	if e.Summary == "" {
		e.Summary = old.Summary
	}
	// Keep the feed's date, as a fresh fetch would. Pages take the date the
	// article metadata gave last time.
	if e.Date.IsZero() || (old.Kind == Post && !e.fromFeed) {
		e.Date = old.Date
	}
}

type sitemapURL struct {
	loc     *url.URL
	lastmod time.Time
}

type sitemapDoc struct {
	XMLName xml.Name
	URLs    []struct {
		Loc     string `xml:"loc"`
		LastMod string `xml:"lastmod"`
	} `xml:"url"`
	Sitemaps []struct {
		Loc string `xml:"loc"`
	} `xml:"sitemap"`
}

// loadSitemaps reads the given sitemaps, following sitemap indexes, and
// returns page URLs newest first.
func (d *Discoverer) loadSitemaps(ctx context.Context, roots []string, prev *Site, scope string, log *slog.Logger) ([]sitemapURL, map[string]cachedSitemap) {
	const maxFiles, maxURLs = 10, 2000
	// robots.txt Sitemap lines can be relative or broken, like an unrendered
	// template. Keep the ones that resolve to an allowed URL.
	var queue []string
	for _, r := range roots {
		if u, err := d.Base.Parse(strings.TrimSpace(r)); err == nil && d.Client.MayFetch(ctx, u) {
			queue = append(queue, u.String())
		}
	}
	if len(queue) == 0 {
		queue = d.atScopes(scope, "sitemap.xml")
	}
	visited := map[string]bool{}
	cache := map[string]cachedSitemap{}
	var out []sitemapURL
	// Every file is read before cutting to maxURLs, so the newest pages win
	// even when they sit in the last of several sitemaps.
	for len(queue) > 0 && len(visited) < maxFiles {
		raw := queue[0]
		queue = queue[1:]
		if visited[raw] || strings.HasSuffix(raw, ".gz") {
			continue
		}
		visited[raw] = true
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		var old cachedSitemap
		var hasOld bool
		if prev != nil {
			old, hasOld = prev.sitemaps[raw]
		}
		res, err := d.Client.Get(ctx, u, acceptXML, hasOld)
		var c cachedSitemap
		switch {
		case keepPrevious(res, err, hasOld):
			c = old
		case err != nil:
			log.Debug("sitemap", "url", raw, "err", err)
			continue
		case res.Status != 200:
			continue
		default:
			var doc sitemapDoc
			// Sitemaps may declare another encoding, like ISO-8859-9.
			dec := xml.NewDecoder(bytes.NewReader(res.Body))
			dec.CharsetReader = charset.NewReaderLabel
			err := dec.Decode(&doc)
			isSitemap := err == nil && (doc.XMLName.Local == "urlset" || doc.XMLName.Local == "sitemapindex")
			switch {
			case isSitemap:
				c = parseSitemap(doc, res.URL, scope, maxURLs)
			case hasOld && old.stale < maxStale:
				// Not a sitemap, like an error page sent with status 200.
				// Well-formed HTML parses as XML, so the root element
				// decides. Keep the old copy for a few refreshes.
				d.Client.Forget(raw)
				c = old
				c.stale++
			default:
				continue
			}
		}
		cache[raw] = c
		for _, child := range c.children {
			if u, err := url.Parse(child); err == nil && d.Client.MayFetch(ctx, u) {
				queue = append(queue, child)
			}
		}
		out = append(out, c.pages...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].lastmod.After(out[j].lastmod) })
	if len(out) > maxURLs {
		out = out[:maxURLs]
	}
	return out, cache
}

// cachedSitemap is what sshr keeps from a sitemap for conditional requests:
// its child sitemaps and the in-scope pages, not the whole document.
type cachedSitemap struct {
	children []string
	pages    []sitemapURL
	stale    int // refreshes in a row it came back unreadable
}

// parseSitemap resolves a sitemap's URLs against base and keeps child
// sitemaps and the pages inside scope that look like pages, not files.
func parseSitemap(doc sitemapDoc, base *url.URL, scope string, limit int) cachedSitemap {
	var c cachedSitemap
	for _, s := range doc.Sitemaps {
		if child, err := base.Parse(strings.TrimSpace(s.Loc)); err == nil {
			c.children = append(c.children, sanitize.URL(child.String()))
		}
	}
	for _, x := range doc.URLs {
		if len(c.pages) >= limit {
			break
		}
		loc, err := base.Parse(strings.TrimSpace(x.Loc))
		if err != nil || !inScope(loc, scope) || !readablePath(loc.Path) {
			continue
		}
		c.pages = append(c.pages, sitemapURL{loc: loc, lastmod: parseDate(x.LastMod)})
	}
	return c
}

type headMeta struct {
	title, description string
	feeds              []feedLink
}

// feedLink is a feed advertised in a page's head.
type feedLink struct {
	href, typ string
}

// parseHead reads <title>, the description and feed links from an HTML head.
func parseHead(body []byte) headMeta {
	var m headMeta
	var siteName, docTitle string
	z := html.NewTokenizer(bytes.NewReader(body))
	inTitle := false
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			goto done
		case html.TextToken:
			if inTitle {
				docTitle += string(z.Text())
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "title":
				inTitle = false
			case "head":
				goto done
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			attrs := map[string]string{}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				attrs[string(k)] = string(v)
			}
			switch string(name) {
			case "body":
				goto done
			case "title":
				inTitle = tt == html.StartTagToken
			case "meta":
				switch {
				case attrs["property"] == "og:site_name":
					siteName = attrs["content"]
				case strings.EqualFold(attrs["name"], "description") && m.description == "":
					m.description = attrs["content"]
				}
			case "link":
				rels := strings.Fields(strings.ToLower(attrs["rel"]))
				if slices.Contains(rels, "alternate") && isFeedType(attrs["type"]) && attrs["href"] != "" {
					m.feeds = append(m.feeds, feedLink{href: attrs["href"], typ: strings.ToLower(strings.TrimSpace(attrs["type"]))})
				}
			}
		}
	}
done:
	m.title = sanitize.Line(siteName)
	if m.title == "" {
		m.title = sanitize.Line(docTitle)
	}
	m.description = sanitize.Line(m.description)
	return m
}

func isFeedType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "application/rss+xml", "application/atom+xml", "application/feed+json", "application/json":
		return true
	}
	return false
}

// sortFeeds orders feed links JSON Feed first, then Atom, then RSS. JSON Feed
// and Atom are more likely to carry full content.
func sortFeeds(links []feedLink) []string {
	rank := func(t string) int {
		switch t {
		case "application/feed+json":
			return 0
		case "application/json":
			// Also used for things that aren't feeds, like WordPress's
			// REST API link, so it goes last.
			return 3
		case "application/atom+xml":
			return 1
		}
		return 2
	}
	sorted := slices.Clone(links)
	sort.SliceStable(sorted, func(i, j int) bool { return rank(sorted[i].typ) < rank(sorted[j].typ) })
	out := make([]string, len(sorted))
	for i, l := range sorted {
		out[i] = l.href
	}
	return out
}

var htmlTag = regexp.MustCompile(`(?i)<(p|div|br|a|h[1-6]|ul|ol|li|img|pre|code|blockquote|em|strong|span|table|figure|section|article)[\s>/]`)

// toMarkdown converts s to markdown if it looks like HTML, and returns it
// unchanged otherwise.
func toMarkdown(s, base string) string {
	if !htmlTag.MatchString(s) {
		return s
	}
	md, err := htmltomarkdown.ConvertString(s, converter.WithDomain(base))
	if err != nil {
		return s
	}
	return md
}

var (
	mdLink   = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	mdMarker = regexp.MustCompile("(?m)^\\s*(#+|>+)\\s*|\\*\\*|`")
)

// plainText turns an HTML or markdown summary into plain text for search.
func plainText(s string) string {
	if htmlTag.MatchString(s) {
		return htmlText(s)
	}
	s = mdLink.ReplaceAllString(s, "$1")
	return mdMarker.ReplaceAllString(s, "")
}

func htmlText(s string) string {
	var b strings.Builder
	z := html.NewTokenizer(strings.NewReader(s))
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return b.String()
		case html.StartTagToken:
			if name, _ := z.TagName(); string(name) == "script" || string(name) == "style" {
				skip++
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); (string(name) == "script" || string(name) == "style") && skip > 0 {
				skip--
			}
		case html.TextToken:
			if skip == 0 {
				b.Write(z.Text())
				b.WriteByte(' ')
			}
		}
	}
}

// frontMatter splits YAML front matter, as static site generators write it,
// off a markdown body, and returns its title and date fields.
func frontMatter(md string) (title, date, body string) {
	rest, ok := strings.CutPrefix(md, "---\n")
	if !ok {
		return "", "", md
	}
	head, body, ok := strings.Cut(rest, "\n---\n")
	if !ok || !looksLikeYAML(head) {
		return "", "", md
	}
	for line := range strings.SplitSeq(head, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "title":
			title = sanitize.Line(v)
		case "date", "pubDate", "published":
			date = v
		}
	}
	return title, date, body
}

var yamlKey = regexp.MustCompile(`^[A-Za-z_][\w-]*:(\s|$)`)

// looksLikeYAML reports whether every line of head is a YAML key, a list
// item or an indented continuation, so a page that opens with a thematic
// break isn't mistaken for front matter.
func looksLikeYAML(head string) bool {
	keys := 0
	for line := range strings.SplitSeq(head, "\n") {
		switch {
		case strings.TrimSpace(line) == "", strings.HasPrefix(line, " "), strings.HasPrefix(line, "- "), strings.HasPrefix(line, "#"):
		case yamlKey.MatchString(line):
			keys++
		default:
			return false
		}
	}
	return keys > 0
}

func firstHeading(md string) string {
	for line := range strings.SplitSeq(md, "\n") {
		if t, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok {
			return sanitize.Line(t)
		}
	}
	return ""
}

func titleFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	base := path.Base(strings.TrimSuffix(u.Path, "/"))
	if base == "." || base == "/" || base == "" {
		return u.Hostname()
	}
	if ext := path.Ext(base); pageExts[strings.ToLower(ext)] {
		base = strings.TrimSuffix(base, ext)
	}
	base = strings.NewReplacer("-", " ", "_", " ").Replace(base)
	if base == "" {
		return u.Hostname()
	}
	r, size := utf8.DecodeRuneInString(base)
	return string(unicode.ToUpper(r)) + base[size:]
}

// fileExts are extensions of sitemap entries that are files, not pages.
// Anything else is treated as a page, so slugs like "go-1.22-released" work.
var fileExts = map[string]bool{}

func init() {
	for _, e := range strings.Fields(`.7z .apk .atom .avif .bmp .css .csv .dmg .doc .docx .eot .epub .exe .gif .gz
		.ico .iso .jpeg .jpg .js .json .m4a .map .mjs .mov .mp3 .mp4 .ogg .otf .pdf .png .ppt .pptx .rar
		.rss .svg .tar .tgz .ttf .wav .webm .webp .woff .woff2 .xls .xlsx .xml .zip`) {
		fileExts[e] = true
	}
}

// readablePath filters out sitemap entries that are files rather than pages.
func readablePath(p string) bool {
	return !fileExts[strings.ToLower(path.Ext(p))]
}

// pageExts are extensions that titleFromURL strips from a slug.
var pageExts = map[string]bool{".html": true, ".htm": true, ".php": true, ".md": true, ".txt": true}

func parseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		time.RFC3339, "2006-01-02T15:04Z07:00", "2006-01-02",
		// Jekyll and Hugo front matter.
		"2006-01-02 15:04:05 -0700", "2006-01-02 15:04:05", "2006-01-02 15:04",
		time.RFC1123Z, time.RFC1123,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
