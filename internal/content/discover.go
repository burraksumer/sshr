package content

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
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
)

const (
	acceptHTML = "text/html, application/xhtml+xml;q=0.9, */*;q=0.1"
	acceptPage = "text/markdown, text/html;q=0.9, application/xhtml+xml;q=0.9, */*;q=0.1"
	acceptFeed = "application/feed+json, application/atom+xml, application/rss+xml, application/xml;q=0.9, text/xml;q=0.9, */*;q=0.1"
	acceptXML  = "application/xml, text/xml;q=0.9, */*;q=0.1"
)

// Feed paths tried when the homepage doesn't link a feed.
var fallbackFeeds = []string{"/feed.json", "/feed.xml", "/atom.xml", "/rss.xml", "/index.xml", "/feed"}

// Discoverer builds a Site for one origin.
type Discoverer struct {
	Client *fetch.Client
	Base   *url.URL
	// MaxEntries caps how many documents a site may have.
	MaxEntries int
	// Workers is how many pages are fetched at once.
	Workers int
	Log     *slog.Logger
}

// Discover fetches the site and returns a new snapshot. prev is the last
// snapshot, used for conditional requests; it may be nil. When prev is nil
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
		maxEntries = 200
	}

	sitemaps, err := d.Client.LoadRobots(ctx, d.Base)
	if err != nil {
		log.Warn("robots.txt", "err", err)
	}

	site := &Site{URL: d.Base.String(), Title: d.Base.Hostname()}
	var feedLinks []string
	if res, err := d.Client.Get(ctx, d.Base, acceptHTML, false); err != nil {
		log.Warn("homepage", "err", err)
	} else if res.Status == 200 {
		meta := parseHead(res.Body)
		if meta.title != "" {
			site.Title = meta.title
		}
		site.Description = meta.description
		for _, f := range meta.feeds {
			if u, err := res.URL.Parse(f); err == nil {
				feedLinks = append(feedLinks, u.String())
			}
		}
	}

	// Feeds give posts with dates and, often, full content.
	entries, feedURL := d.loadFeed(ctx, site, feedLinks, prev, log)
	site.feedURL = feedURL
	if len(entries) > maxEntries {
		entries = entries[:maxEntries]
	}
	feedEntries := slices.Clone(entries)

	// The sitemap adds pages the feed doesn't list.
	seen := map[string]bool{normalize(d.Base): true}
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
		for _, p := range d.loadSitemaps(ctx, sitemaps, log) {
			if room == 0 {
				break
			}
			key := normalize(p.loc)
			if seen[key] || !d.Client.Allowed(p.loc) || !readablePath(p.loc.Path) {
				continue
			}
			seen[key] = true
			entries = append(entries, &Entry{URL: p.loc.String(), Kind: Page, Date: p.lastmod})
			room--
		}
	}

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

	d.fillBodies(ctx, entries, prev, log)
	site.feedEntries = feedEntries

	var kept []*Entry
	for _, e := range entries {
		if strings.TrimSpace(e.Body) != "" {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		return nil, errors.New("no readable content found (no feed, sitemap or extractable pages)")
	}
	return finish(site, kept), nil
}

// finish sorts entries, fills missing titles and indexes the site.
func finish(site *Site, kept []*Entry) *Site {
	for _, e := range kept {
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
func (d *Discoverer) loadFeed(ctx context.Context, site *Site, links []string, prev *Site, log *slog.Logger) ([]*Entry, string) {
	// Advertised feeds first, then the common paths, in case the advertised
	// ones are broken.
	candidates := sortFeeds(links)
	for _, p := range fallbackFeeds {
		if c := d.Base.ResolveReference(&url.URL{Path: p}).String(); !slices.Contains(candidates, c) {
			candidates = append(candidates, c)
		}
	}
	parser := gofeed.NewParser()
	for _, raw := range candidates {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		conditional := prev != nil && prev.feedURL == raw
		res, err := d.Client.GetLarge(ctx, u, acceptFeed, conditional)
		if err != nil {
			log.Debug("feed", "url", raw, "err", err)
			continue
		}
		if res.NotModified {
			src := prev.feedEntries
			if src == nil {
				for _, e := range prev.Entries {
					if e.fromFeed {
						src = append(src, e)
					}
				}
			}
			var kept []*Entry
			for _, e := range src {
				c := *e
				// Bodies fetched from pages get rechecked with a conditional
				// request, and entries whose fetch failed get retried.
				if c.pageBody {
					c.Body, c.pageBody = "", false
				}
				kept = append(kept, &c)
			}
			if prev.Title != "" {
				site.Title = prev.Title
			}
			if prev.Description != "" {
				site.Description = prev.Description
			}
			return kept, raw
		}
		if res.Status != 200 {
			continue
		}
		feed, err := parser.Parse(bytes.NewReader(res.Body))
		if err != nil || len(feed.Items) == 0 {
			continue
		}
		if t := sanitize.Line(feed.Title); t != "" {
			site.Title = t
		}
		if desc := sanitize.Line(feed.Description); desc != "" {
			site.Description = desc
		}
		entries := make([]*Entry, 0, len(feed.Items))
		for _, item := range feed.Items {
			if e := entryFromItem(item, res.URL); e != nil {
				entries = append(entries, e)
			}
		}
		log.Info("feed", "url", raw, "items", len(entries))
		return entries, raw
	}
	return nil, ""
}

func entryFromItem(item *gofeed.Item, feedURL *url.URL) *Entry {
	e := &Entry{Kind: Post, fromFeed: true}
	link := item.Link
	if link == "" && strings.HasPrefix(item.GUID, "http") {
		link = item.GUID
	}
	if link != "" {
		if u, err := feedURL.Parse(link); err == nil {
			e.URL = u.String()
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
		e.Body = sanitize.Text(toMarkdown(body, base))
	}
	if e.URL == "" && e.Body == "" {
		return nil
	}
	return e
}

// fillBodies fetches the page for every entry that has no body yet.
func (d *Discoverer) fillBodies(ctx context.Context, entries []*Entry, prev *Site, log *slog.Logger) {
	workers := d.Workers
	if workers <= 0 {
		workers = 4
	}
	jobs := make(chan *Entry)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for e := range jobs {
				var old *Entry
				if prev != nil {
					old = prev.Lookup(e.URL)
				}
				if err := d.fillBody(ctx, e, old); err != nil {
					log.Debug("page", "url", e.URL, "err", err)
				}
			}
		})
	}
	for _, e := range entries {
		if e.Body == "" && e.URL != "" {
			select {
			case jobs <- e:
			case <-ctx.Done():
			}
		}
	}
	close(jobs)
	wg.Wait()
}

func (d *Discoverer) fillBody(ctx context.Context, e *Entry, old *Entry) error {
	u, err := url.Parse(e.URL)
	if err != nil {
		return err
	}
	res, err := d.Client.Get(ctx, u, acceptPage, old != nil && old.Body != "")
	if err != nil {
		return err
	}
	if res.NotModified {
		e.Body, e.Kind, e.pageBody = old.Body, old.Kind, true
		if e.Title == "" {
			e.Title = old.Title
		}
		if e.Summary == "" {
			e.Summary = old.Summary
		}
		if e.Date.IsZero() || old.Kind == Post {
			e.Date = old.Date
		}
		return nil
	}
	if res.Status != 200 {
		return fmt.Errorf("status %d", res.Status)
	}

	switch res.ContentType {
	case "text/markdown", "text/x-markdown", "text/plain":
		body := sanitize.Text(string(res.Body))
		if e.Title == "" {
			e.Title = firstHeading(body)
		}
		e.Body, e.pageBody = body, true
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
	e.Body, e.pageBody = sanitize.Text(string(md)), true
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

type sitemapURL struct {
	loc     *url.URL
	lastmod time.Time
}

type sitemapDoc struct {
	URLs []struct {
		Loc     string `xml:"loc"`
		LastMod string `xml:"lastmod"`
	} `xml:"url"`
	Sitemaps []struct {
		Loc string `xml:"loc"`
	} `xml:"sitemap"`
}

// loadSitemaps reads the given sitemaps, following sitemap indexes, and
// returns page URLs newest first.
func (d *Discoverer) loadSitemaps(ctx context.Context, roots []string, log *slog.Logger) []sitemapURL {
	const maxFiles, maxURLs = 10, 2000
	// robots.txt Sitemap lines can be relative or broken (an unrendered
	// template, say). Keep the ones that resolve to an allowed URL.
	var queue []string
	for _, r := range roots {
		if u, err := d.Base.Parse(strings.TrimSpace(r)); err == nil && d.Client.Allowed(u) {
			queue = append(queue, u.String())
		}
	}
	if len(queue) == 0 {
		queue = []string{d.Base.ResolveReference(&url.URL{Path: "/sitemap.xml"}).String()}
	}
	visited := map[string]bool{}
	var out []sitemapURL
	for len(queue) > 0 && len(visited) < maxFiles && len(out) < maxURLs {
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
		res, err := d.Client.Get(ctx, u, acceptXML, false)
		if err != nil || res.Status != 200 {
			if err != nil {
				log.Debug("sitemap", "url", raw, "err", err)
			}
			continue
		}
		var doc sitemapDoc
		if err := xml.Unmarshal(res.Body, &doc); err != nil {
			continue
		}
		for _, s := range doc.Sitemaps {
			if child, err := res.URL.Parse(strings.TrimSpace(s.Loc)); err == nil && d.Client.Allowed(child) {
				queue = append(queue, child.String())
			}
		}
		for _, x := range doc.URLs {
			loc, err := res.URL.Parse(strings.TrimSpace(x.Loc))
			if err != nil {
				continue
			}
			out = append(out, sitemapURL{loc: loc, lastmod: parseDate(x.LastMod)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].lastmod.After(out[j].lastmod) })
	return out
}

type headMeta struct {
	title, description string
	feeds              []string
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
					m.feeds = append(m.feeds, attrs["href"])
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
func sortFeeds(links []string) []string {
	rank := func(s string) int {
		switch {
		case strings.HasSuffix(s, ".json"):
			return 0
		case strings.Contains(s, "atom"):
			return 1
		}
		return 2
	}
	out := append([]string(nil), links...)
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
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
	base = strings.TrimSuffix(base, path.Ext(base))
	base = strings.NewReplacer("-", " ", "_", " ").Replace(base)
	if base == "" {
		return u.Hostname()
	}
	r, size := utf8.DecodeRuneInString(base)
	return string(unicode.ToUpper(r)) + base[size:]
}

// readablePath filters out sitemap entries that are files rather than pages.
func readablePath(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case "", ".html", ".htm", ".md", ".php", ".txt":
		return true
	}
	return false
}

func parseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00", "2006-01-02"} {
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
