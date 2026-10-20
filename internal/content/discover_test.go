package content

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/burraksumer/sshr/internal/fetch"
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
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Hostname()}, Rate: 1000, Burst: 100}), Base: base}

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
