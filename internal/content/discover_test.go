package content

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/burraksumer/sshr/internal/fetch"
	"github.com/mmcdole/gofeed"
)

const article = `<!doctype html><html><head><title>%s</title>
<meta property="article:published_time" content="2024-05-01T10:00:00Z"></head>
<body><nav>menu menu menu</nav><article><h1>%s</h1>
<p>This is the first paragraph of a reasonably long article body, long enough that
readability treats it as the main content of the page and keeps it.</p>
<p>A second paragraph with a <a href="/other">relative link</a> and an escape
attempt: ` + "\x1b]0;pwned\x07" + ` that must not survive.</p>
<p>A third paragraph so the extractor has plenty of text to score and keep around.</p>
</article><footer>footer</footer></body></html>`

func fakeSite(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("User-agent: *\nDisallow: /private\nSitemap: /sitemap.xml\n"))
	})
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Home</title><meta property="og:site_name" content="Test Site">
<meta name="description" content="A site for tests">
<link rel="alternate" type="application/rss+xml" href="/rss.xml"></head><body>hi</body></html>`))
	})
	mux.HandleFunc("/rss.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>Test Feed</title>
<item><title>Full post</title><link>` + srv.URL + `/full</link><pubDate>Tue, 02 Jan 2024 00:00:00 GMT</pubDate>
<category>go</category><description>short</description>
<content:encoded xmlns:content="http://purl.org/rss/1.0/modules/content/"><![CDATA[<p>Full <strong>body</strong> with <a href="https://go.dev">a link</a>.</p>]]></content:encoded></item>
<item><title>Summary post</title><link>` + srv.URL + `/summary</link><pubDate>Mon, 01 Jan 2024 00:00:00 GMT</pubDate>
<description>Only a summary.</description></item>
</channel></rss>`))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<url><loc>` + srv.URL + `/</loc></url>
<url><loc>` + srv.URL + `/full</loc></url>
<url><loc>` + srv.URL + `/about</loc><lastmod>2024-01-05</lastmod></url>
<url><loc>` + srv.URL + `/private/secret</loc></url>
<url><loc>` + srv.URL + `/logo.png</loc></url>
<url><loc>https://elsewhere.example/page</loc></url>
</urlset>`))
	})
	page := func(title string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(strings.ReplaceAll(article, "%s", title)))
		}
	}
	mux.HandleFunc("/summary", page("Summary post page"))
	mux.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "text/markdown") {
			w.Header().Set("Content-Type", "text/markdown")
			w.Write([]byte("# About me\n\nI write tests.\n"))
			return
		}
		page("About")(w, r)
	})
	mux.HandleFunc("/private/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fetched %s despite robots.txt", r.URL.Path)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDiscover(t *testing.T) {
	srv := fakeSite(t)
	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}

	var partial *Site
	site, err := d.Discover(context.Background(), nil, func(s *Site) { partial = s })
	if err != nil {
		t.Fatal(err)
	}
	if site.Title != "Test Feed" || site.Description != "A site for tests" {
		t.Errorf("title %q, description %q", site.Title, site.Description)
	}
	if partial == nil || len(partial.Entries) != 1 {
		t.Errorf("partial snapshot should have the one full-content post, got %+v", partial)
	}

	var titles []string
	for _, e := range site.Entries {
		titles = append(titles, e.Title)
	}
	want := []string{"Full post", "Summary post", "About me"}
	if strings.Join(titles, "|") != strings.Join(want, "|") {
		t.Fatalf("entries = %q, want %q", titles, want)
	}

	full := site.Lookup(srv.URL + "/full/")
	if full == nil || !strings.Contains(full.Body, "**body**") || full.Tags[0] != "go" {
		t.Errorf("full post = %+v", full)
	}
	summary := site.Lookup(srv.URL + "/summary")
	if !strings.Contains(summary.Body, "first paragraph") || strings.Contains(summary.Body, "menu menu") {
		t.Errorf("summary post body was not extracted:\n%s", summary.Body)
	}
	if strings.ContainsRune(summary.Body, 0x1b) || strings.ContainsRune(summary.Body, 0x07) {
		t.Errorf("control bytes survived: %q", summary.Body)
	}
	if !strings.Contains(summary.Body, "("+srv.URL+"/other)") {
		t.Errorf("relative link not resolved:\n%s", summary.Body)
	}
	about := site.Lookup(srv.URL + "/about")
	if about.Kind != Page || !strings.Contains(about.Body, "I write tests") {
		t.Errorf("about = %+v", about)
	}

	// A second refresh sends conditional requests and keeps the feed entries.
	again, err := d.Discover(context.Background(), site, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Entries) != 3 || again.Lookup(srv.URL+"/full").Body != full.Body {
		t.Errorf("refresh lost entries: %d", len(again.Entries))
	}
}

func TestAllowlist(t *testing.T) {
	c := fetch.New(fetch.Options{Hosts: []string{"example.com"}})
	for _, raw := range []string{"http://169.254.169.254/latest/meta-data", "file:///etc/passwd", "https://evil.example/"} {
		u, _ := url.Parse(raw)
		if _, err := c.Get(context.Background(), u, "", false); err == nil {
			t.Errorf("fetched %s", raw)
		}
	}
}

func TestTitleFromURL(t *testing.T) {
	site := finish(&Site{}, []*Entry{
		{URL: "https://site.example/%1b%5d8%3b%3bevil%07click%1b%5b31mred", Kind: Page},
		{URL: "https://site.example/%C3%A7ay-demlemek/", Kind: Page},
	})
	for _, e := range site.Entries {
		for _, r := range e.Title {
			if r < 0x20 || r == 0x7f || r == utf8.RuneError {
				t.Errorf("title %q contains %U", e.Title, r)
			}
		}
	}
	if got := site.Lookup("https://site.example/%C3%A7ay-demlemek").Title; got != "Çay demlemek" {
		t.Errorf("title = %q, want %q", got, "Çay demlemek")
	}
}

func TestPlainText(t *testing.T) {
	cases := map[string]string{
		"see [this](https://x.com/a) and **bold** `code`":             "see this and bold code",
		"keep snake_case and a_b":                                     "keep snake_case and a_b",
		"## Heading\n> quote":                                         "Heading quote",
		"<p>Hello <a href=\"/x\">world</a></p><script>bad()</script>": "Hello world",
	}
	for in, want := range cases {
		// Summaries go through sanitize.Line, so whitespace doesn't matter.
		if got := strings.Join(strings.Fields(plainText(in)), " "); got != want {
			t.Errorf("plainText(%q) = %q, want %q", in, got, want)
		}
	}
}

// A broken advertised feed falls back to the common paths, and a sitemap
// index with relative child URLs is followed.
func TestFallbacks(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><link rel="alternate" type="application/rss+xml" href="/old-feed"></head></html>`))
	})
	mux.HandleFunc("/index.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>T</title><item><title>Post</title>
<link>` + srv.URL + `/post</link><content:encoded xmlns:content="http://purl.org/rss/1.0/modules/content/">Body text</content:encoded></item></channel></rss>`))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><sitemapindex><sitemap><loc>/sitemap-pages.xml</loc></sitemap></sitemapindex>`))
	})
	mux.HandleFunc("/sitemap-pages.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><urlset><url><loc>/now</loc></url></urlset>`))
	})
	mux.HandleFunc("/now", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte("What I'm doing now.\n"))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	site, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if site.Lookup(srv.URL+"/post") == nil {
		t.Error("feed at /index.xml was not used")
	}
	if now := site.Lookup(srv.URL + "/now"); now == nil || now.Title != "Now" {
		t.Errorf("page from child sitemap = %+v", now)
	}
}

// A post whose page fetch failed comes back on a later refresh, even when the
// feed answers 304.
func TestRefreshRetriesDroppedPost(t *testing.T) {
	var pageCalls atomic.Int32
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/feed.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"1"`)
		if r.Header.Get("If-None-Match") == `"1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/feed+json")
		w.Write([]byte(`{"version":"https://jsonfeed.org/version/1.1","title":"T","items":[
{"id":"1","url":"` + srv.URL + `/full","title":"Full","content_text":"Full body"},
{"id":"2","url":"` + srv.URL + `/flaky","title":"Flaky","summary":"s"}]}`))
	})
	mux.HandleFunc("/flaky", func(w http.ResponseWriter, r *http.Request) {
		if pageCalls.Add(1) == 1 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte("Flaky body\n"))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	first, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Lookup(srv.URL+"/flaky") != nil {
		t.Fatal("flaky post should be missing after the failed fetch")
	}
	second, err := d.Discover(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e := second.Lookup(srv.URL + "/flaky"); e == nil || !strings.Contains(e.Body, "Flaky body") {
		t.Errorf("flaky post not recovered: %+v", e)
	}
	if second.Lookup(srv.URL+"/full") == nil {
		t.Error("full post lost on 304")
	}
}

// With url = example.com/blog, feeds are looked up under /blog first and
// sitemap pages outside /blog are left out.
func TestSubpathScope(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/blog/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Blog</title></head></html>`))
	})
	mux.HandleFunc("/blog/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>Blog</title><item><title>Post</title>
<link>` + srv.URL + `/blog/post</link><content:encoded xmlns:content="http://purl.org/rss/1.0/modules/content/">Body</content:encoded></item></channel></rss>`))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><urlset><url><loc>/blog/about</loc></url><url><loc>/shop</loc></url></urlset>`))
	})
	page := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte("Page text\n"))
	}
	mux.HandleFunc("/blog/about", page)
	mux.HandleFunc("/shop", page)
	srv = httptest.NewServer(mux)
	defer srv.Close()

	base, _ := url.Parse(srv.URL + "/blog")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	site, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if site.Lookup(srv.URL+"/blog/post") == nil {
		t.Error("feed under /blog was not found")
	}
	if site.Lookup(srv.URL+"/blog/about") == nil {
		t.Error("in-scope sitemap page missing")
	}
	if site.Lookup(srv.URL+"/shop") != nil {
		t.Error("sitemap page outside /blog was included")
	}
}

// A page that answers 304 keeps the feed's date, and a server that answers
// 304 without being asked doesn't crash discovery.
func TestSortFeedsJSONLast(t *testing.T) {
	got := sortFeeds([]feedLink{{"/api", "application/json"}, {"/rss", "application/rss+xml"}, {"/atom", "application/atom+xml"}, {"/jf", "application/feed+json"}})
	if strings.Join(got, ",") != "/jf,/atom,/rss,/api" {
		t.Errorf("sortFeeds = %v", got)
	}
}

func TestPage304KeepsFeedDate(t *testing.T) {
	var feedCalls atomic.Int32
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/feed.json", func(w http.ResponseWriter, r *http.Request) {
		date := "2024-01-01T00:00:00Z"
		if feedCalls.Add(1) > 1 {
			date = "2024-06-01T00:00:00Z"
		}
		w.Header().Set("Content-Type", "application/feed+json")
		w.Write([]byte(`{"version":"https://jsonfeed.org/version/1.1","title":"T","items":[
{"id":"1","url":"` + srv.URL + `/post","title":"Post","summary":"s","date_modified":"` + date + `"}]}`))
	})
	mux.HandleFunc("/post", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"p"`)
		if r.Header.Get("If-None-Match") == `"p"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte("Post body\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	first, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.Discover(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := second.Lookup(srv.URL + "/post")
	if e == nil || e.Date.Month() != 6 || !strings.Contains(e.Body, "Post body") {
		t.Errorf("entry after 304 = %+v", e)
	}
}

// A page that fails with a 5xx keeps last refresh's copy, and www and bare
// hosts count as one site.
func TestTransientErrorKeepsPage(t *testing.T) {
	var fail atomic.Bool
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/feed.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/feed+json")
		w.Write([]byte(`{"version":"https://jsonfeed.org/version/1.1","title":"T","items":[
{"id":"1","url":"` + srv.URL + `/post","title":"Post","summary":"s"}]}`))
	})
	mux.HandleFunc("/post", func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte("Post body\n"))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	first, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	second, err := d.Discover(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e := second.Lookup(srv.URL + "/post"); e == nil || !strings.Contains(e.Body, "Post body") {
		t.Errorf("post dropped after a 503: %+v", e)
	}
}

func TestNormalizeFoldsWWW(t *testing.T) {
	a, _ := url.Parse("https://www.example.com/post/")
	b, _ := url.Parse("http://EXAMPLE.com/post#top")
	if normalize(a) != normalize(b) {
		t.Errorf("%q != %q", normalize(a), normalize(b))
	}
}

func TestReadablePath(t *testing.T) {
	for p, want := range map[string]bool{
		"/posts/go-1.22-released": true, "/blog/node.js-tips": true, "/about": true,
		"/index.html": true, "/a.php": true, "/logo.png": false, "/paper.PDF": false, "/feed.xml": false,
	} {
		if got := readablePath(p); got != want {
			t.Errorf("readablePath(%q) = %v, want %v", p, got, want)
		}
	}
	if got := titleFromURL("https://x.example/posts/go-1.22-released"); got != "Go 1.22 released" {
		t.Errorf("titleFromURL = %q", got)
	}
}

// Feeds that list oldest first keep their newest posts when cut, and the
// advertised type decides which feed wins.
func TestFeedOrderAndType(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head>
<link rel="alternate" type="application/json" href="/wp-json/wp/v2/pages/2">
<link rel="alternate" type="application/rss+xml" href="/rss">
<link rel="alternate" type="application/feed+json" href="/feed/json/"></head></html>`))
	})
	mux.HandleFunc("/rss", func(w http.ResponseWriter, r *http.Request) {
		t.Error("RSS fetched although a JSON Feed was advertised")
	})
	mux.HandleFunc("/wp-json/wp/v2/pages/2", func(w http.ResponseWriter, r *http.Request) {
		t.Error("plain application/json link fetched before the real feed")
	})
	mux.HandleFunc("/feed/json/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var items []string
		for i := 1; i <= 5; i++ {
			items = append(items, fmt.Sprintf(`{"id":"%d","url":"%s/p%d","title":"P%d","content_text":"b","date_published":"2024-01-0%dT00:00:00Z"}`, i, srv.URL, i, i, i))
		}
		w.Write([]byte(`{"version":"https://jsonfeed.org/version/1.1","title":"T","items":[` + strings.Join(items, ",") + `]}`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base, MaxEntries: 2}
	site, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, e := range site.Entries {
		titles = append(titles, e.Title)
	}
	if strings.Join(titles, ",") != "P5,P4" {
		t.Errorf("entries = %v, want the newest two", titles)
	}
}

// When the feed fails for a moment, its posts stay. When it answers 304,
// a newer homepage title isn't overwritten by an old feed title it never had.
func TestFeedOutageAndTitle(t *testing.T) {
	var feedDown atomic.Bool
	var siteName atomic.Value
	siteName.Store("Old Name")
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><meta property="og:site_name" content="` + siteName.Load().(string) + `">
<link rel="alternate" type="application/feed+json" href="/feed.json"></head></html>`))
	})
	mux.HandleFunc("/feed.json", func(w http.ResponseWriter, r *http.Request) {
		if feedDown.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.Header().Set("ETag", `"f"`)
		if r.Header.Get("If-None-Match") == `"f"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/feed+json")
		w.Write([]byte(`{"version":"https://jsonfeed.org/version/1.1","items":[{"id":"1","url":"` + srv.URL + `/p","title":"P","content_text":"body"}]}`))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><urlset><url><loc>/about</loc></url></urlset>`))
	})
	mux.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte("About\n"))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	first, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	siteName.Store("New Name")
	second, err := d.Discover(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Title != "New Name" {
		t.Errorf("title after 304 = %q, want the homepage's new name", second.Title)
	}
	feedDown.Store(true)
	third, err := d.Discover(context.Background(), second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if third.Lookup(srv.URL+"/p") == nil {
		t.Error("feed post dropped during a feed outage")
	}
}

func TestCapBody(t *testing.T) {
	long := strings.Repeat("line of text\n", maxBody/13+10)
	got := capBody(long)
	if len(got) > maxBody+100 || !strings.Contains(got, "cut short") {
		t.Errorf("capBody kept %d bytes", len(got))
	}
	if capBody("short") != "short" {
		t.Error("short body changed")
	}
}

func TestEntryURLsEncoded(t *testing.T) {
	feedURL, _ := url.Parse("https://x.example/feed")
	e := entryFromItem(&gofeed.Item{Link: "https://x.example/p?a=\u009c\u009b2J", Title: "t", Content: "b"}, feedURL)
	if strings.ContainsAny(e.URL, "\u009c\u009b") {
		t.Errorf("entry URL kept C1 controls: %q", e.URL)
	}
}

// An HTML page served in place of the feed keeps the previous posts.
func TestFeedReplacedByHTMLPage(t *testing.T) {
	var broken atomic.Bool
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html><body>Down for maintenance</body></html>"))
			return
		}
		w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>T</title><item><title>P</title>
<link>` + srv.URL + `/p</link><content:encoded xmlns:content="http://purl.org/rss/1.0/modules/content/">Body</content:encoded></item></channel></rss>`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	first, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	broken.Store(true)
	second, err := d.Discover(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Lookup(srv.URL+"/p") == nil || second.feedURL == "" {
		t.Errorf("posts or feed URL lost: feedURL %q", second.feedURL)
	}
}

// A site-wide feed found from a /blog site only contributes /blog posts, and
// out-of-scope sitemap URLs don't use up the sitemap URL cap.
func TestScopeAppliesToFeedAndSitemapCap(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>T</title>
<item><title>Blog</title><link>` + srv.URL + `/blog/a</link><content:encoded xmlns:content="http://purl.org/rss/1.0/modules/content/">x</content:encoded></item>
<item><title>Shop</title><link>` + srv.URL + `/shop/b</link><content:encoded xmlns:content="http://purl.org/rss/1.0/modules/content/">x</content:encoded></item>
</channel></rss>`))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><urlset>`)
		for i := range 2500 {
			fmt.Fprintf(&b, "<url><loc>/products/%d</loc></url>", i)
		}
		b.WriteString(`<url><loc>/blog/about</loc></url></urlset>`)
		w.Write([]byte(b.String()))
	})
	mux.HandleFunc("/blog/about", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte("About\n"))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/blog")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	site, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if site.Lookup(srv.URL+"/shop/b") != nil {
		t.Error("out-of-scope feed item included")
	}
	if site.Lookup(srv.URL+"/blog/a") == nil || site.Lookup(srv.URL+"/blog/about") == nil {
		t.Errorf("in-scope entries missing: %d entries", len(site.Entries))
	}
}

// The partial snapshot is published before sitemaps are fetched.
func TestPartialBeforeSitemaps(t *testing.T) {
	var published atomic.Bool
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/feed.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/feed+json")
		w.Write([]byte(`{"version":"https://jsonfeed.org/version/1.1","items":[{"id":"1","url":"` + srv.URL + `/p","title":"P","content_text":"body"}]}`))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		if !published.Load() {
			t.Error("sitemap fetched before the partial snapshot was published")
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	if _, err := d.Discover(context.Background(), nil, func(*Site) { published.Store(true) }); err != nil {
		t.Fatal(err)
	}
}

// When the homepage can't be read, last refresh's feed is still tried, so
// posts from a feed at an unusual path aren't lost.
func TestFeedTriedWhenHomepageDown(t *testing.T) {
	var homeDown atomic.Bool
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		if homeDown.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><link rel="alternate" type="application/rss+xml" href="/posts/index.rss"></head></html>`))
	})
	mux.HandleFunc("/posts/index.rss", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>T</title><item><title>P</title>
<link>` + srv.URL + `/p</link><content:encoded xmlns:content="http://purl.org/rss/1.0/modules/content/">Body</content:encoded></item></channel></rss>`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	first, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	homeDown.Store(true)
	second, err := d.Discover(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Lookup(srv.URL+"/p") == nil {
		t.Error("post lost while the homepage was down")
	}
}

// A robots.txt outage on a page's host keeps last refresh's copy.
func TestRobotsOutageKeepsPage(t *testing.T) {
	var robotsDown atomic.Bool
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		if robotsDown.Load() {
			http.Error(w, "down", http.StatusBadGateway)
		}
	})
	mux.HandleFunc("/feed.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/feed+json")
		w.Write([]byte(`{"version":"https://jsonfeed.org/version/1.1","items":[{"id":"1","url":"` + srv.URL + `/p","title":"P","summary":"s"}]}`))
	})
	mux.HandleFunc("/p", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte("Post body\n"))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	first, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	robotsDown.Store(true)
	second, err := d.Discover(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e := second.Lookup(srv.URL + "/p"); e == nil || !strings.Contains(e.Body, "Post body") {
		t.Errorf("post lost during a robots.txt outage: %+v", e)
	}
}

// During a robots.txt outage, and when a sitemap comes back as an HTML
// page, sitemap pages keep last refresh's copies.
func TestSitemapPagesSurviveOutages(t *testing.T) {
	var robotsDown, sitemapHTML atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		if robotsDown.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("User-agent: *\nAllow: /\nSitemap: /sitemap_index.xml\n"))
	})
	mux.HandleFunc("/sitemap_index.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><sitemapindex><sitemap><loc>/sitemap-pages.xml</loc></sitemap></sitemapindex>`))
	})
	mux.HandleFunc("/sitemap-pages.xml", func(w http.ResponseWriter, r *http.Request) {
		if sitemapHTML.Load() {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>maintenance</html>"))
			return
		}
		w.Write([]byte(`<?xml version="1.0"?><urlset><url><loc>/about</loc></url></urlset>`))
	})
	mux.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte("About\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	site, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sitemapHTML.Store(true)
	if site, err = d.Discover(context.Background(), site, nil); err != nil || site.Lookup(srv.URL+"/about") == nil {
		t.Fatalf("page lost when the sitemap served HTML: %v", err)
	}
	sitemapHTML.Store(false)
	robotsDown.Store(true)
	if site, err = d.Discover(context.Background(), site, nil); err != nil || site.Lookup(srv.URL+"/about") == nil {
		t.Fatalf("page lost during a robots.txt outage: %v", err)
	}
}

func TestDuplicateFeedItems(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/feed.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/feed+json")
		host := strings.TrimPrefix(srv.URL, "http://")
		w.Write([]byte(`{"version":"https://jsonfeed.org/version/1.1","items":[
{"id":"1","url":"` + srv.URL + `/p/","title":"P","content_text":"a"},
{"id":"2","url":"http://` + host + `/p","title":"P again","content_text":"b"}]}`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	site, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(site.Entries) != 1 || site.Entries[0].Title != "P" {
		t.Errorf("entries = %d, first %q", len(site.Entries), site.Entries[0].Title)
	}
}

// Past the page budget, pages keep last refresh's copies, and a feed entry
// stays a post even when its previous copy was a page.
func TestPageBudgetAndKind(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/feed.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/feed+json")
		w.Write([]byte(`{"version":"https://jsonfeed.org/version/1.1","items":[{"id":"1","url":"` + srv.URL + `/p","title":"P","summary":"s"}]}`))
	})
	mux.HandleFunc("/p", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte("Body\n"))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	first, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Pretend the previous copy was a page.
	first.Lookup(srv.URL + "/p").Kind = Page
	d.PageBudget = time.Nanosecond
	second, err := d.Discover(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := second.Lookup(srv.URL + "/p")
	if e == nil || e.Body == "" || e.Kind != Post {
		t.Errorf("entry past the budget = %+v", e)
	}
}

func TestScopeOfFileURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://x.example/":               "/",
		"https://x.example/blog":           "/blog/",
		"https://x.example/blog/":          "/blog/",
		"https://x.example/index.html":     "/",
		"https://x.example/blog/index.php": "/blog/",
		"https://x.example/~jane.doe":      "/~jane.doe/",
		"https://x.example/docs/v2.0":      "/docs/v2.0/",
	} {
		u, _ := url.Parse(raw)
		if got := (&Discoverer{Base: u}).scope(); got != want {
			t.Errorf("scope(%s) = %q, want %q", raw, got, want)
		}
	}
}

// A homepage outage keeps the site's name, a whitespace-only feed body is
// fetched from the page, and a URL in both feed and sitemap with non-ASCII
// in its query shows up once.
func TestHomepageOutageWhitespaceAndDedupe(t *testing.T) {
	var homeDown atomic.Bool
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		if homeDown.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Real Name</title></head></html>`))
	})
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel>
<item><title>Blank</title><link>` + srv.URL + `/blank</link><content:encoded xmlns:content="http://purl.org/rss/1.0/modules/content/"><![CDATA[<p>&nbsp;</p>]]></content:encoded></item>
<item><title>Q</title><link>` + srv.URL + `/?p=ü</link><content:encoded xmlns:content="http://purl.org/rss/1.0/modules/content/">q body</content:encoded></item>
</channel></rss>`))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><urlset><url><loc>` + srv.URL + `/?p=ü</loc></url></urlset>`))
	})
	mux.HandleFunc("/blank", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte("Real body\n"))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	first, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e := first.Lookup(srv.URL + "/blank"); e == nil || !strings.Contains(e.Body, "Real body") {
		t.Errorf("whitespace-only body not fetched: %+v", e)
	}
	if len(first.Entries) != 2 {
		t.Errorf("got %d entries, want 2 with the feed and sitemap URL merged", len(first.Entries))
	}
	homeDown.Store(true)
	second, err := d.Discover(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Title != "Real Name" {
		t.Errorf("title during homepage outage = %q", second.Title)
	}
}

// With a budget too small for every page, refreshes rotate through the
// pages, oldest copy first, instead of refetching the same ones.
func TestPageBudgetRotates(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/feed.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/feed+json")
		var items []string
		for i := range 4 {
			items = append(items, fmt.Sprintf(`{"id":"%d","url":"%s/p%d","title":"P%d","summary":"s"}`, i, srv.URL, i, i))
		}
		w.Write([]byte(`{"version":"https://jsonfeed.org/version/1.1","items":[` + strings.Join(items, ",") + `]}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte("body " + r.URL.Path + "\n"))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	site, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Mark p0..p3 as fetched at increasing times, then allow one fetch.
	for i, p := range []string{"/p3", "/p1", "/p2", "/p0"} {
		site.Lookup(srv.URL + p).fetchedAt = time.Unix(int64(i), 0)
	}
	mu.Lock()
	clear(hits)
	mu.Unlock()
	d.PageBudget = time.Millisecond
	if _, err := d.Discover(context.Background(), site, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["/p3"] != 1 {
		t.Errorf("page hits = %v, want /p3, the oldest copy, fetched", hits)
	}
}

// A title that a page fetch fills in for an untitled feed item stays out of
// the saved feed entries, and a feed that disappears gets the common paths
// probed again on the next refresh.
func TestFeedEntriesKeepFeedFields(t *testing.T) {
	var gone atomic.Bool
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		if gone.Load() {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>T</title><item>
<link>` + srv.URL + `/untitled</link><description>s</description></item></channel></rss>`))
	})
	mux.HandleFunc("/untitled", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte("# From the page\n\nBody\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><urlset><url><loc>/untitled</loc></url></urlset>`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	site, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if site.Lookup(srv.URL+"/untitled").Title != "From the page" {
		t.Fatalf("title = %q", site.Lookup(srv.URL+"/untitled").Title)
	}
	if site.feedEntries[0].Title != "" {
		t.Errorf("page title leaked into the feed entry: %q", site.feedEntries[0].Title)
	}
	gone.Store(true)
	next, err := d.Discover(context.Background(), site, nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.probeAge != reprobeEvery {
		t.Errorf("probeAge after losing the feed = %d, want %d", next.probeAge, reprobeEvery)
	}
}

// A post pushed out of the feed, whose page has no published time, stays a
// dated post.
func TestPostLeavingFeedStaysPost(t *testing.T) {
	var pushedOut atomic.Bool
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/feed.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/feed+json")
		item := `{"id":"1","url":"` + srv.URL + `/old","title":"Old","summary":"s","date_published":"2024-01-01T00:00:00Z"}`
		if pushedOut.Load() {
			item = `{"id":"2","url":"` + srv.URL + `/new","title":"New","content_text":"n","date_published":"2024-02-01T00:00:00Z"}`
		}
		w.Write([]byte(`{"version":"https://jsonfeed.org/version/1.1","items":[` + item + `]}`))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><urlset><url><loc>/old</loc></url></urlset>`))
	})
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte("Old body\n"))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	site, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pushedOut.Store(true)
	site, err = d.Discover(context.Background(), site, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := site.Lookup(srv.URL + "/old")
	if e == nil || e.Kind != Post || e.Date.Year() != 2024 {
		t.Errorf("old post = %+v, want a dated post", e)
	}
}
