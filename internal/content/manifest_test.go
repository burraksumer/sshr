package content

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/burraksumer/sshr/internal/fetch"
)

func TestMergePrefersLocal(t *testing.T) {
	off := false
	remote := &Manifest{Title: "Remote", Description: "remote desc", Accent: "#111111", Pages: []string{"/a"}, Nav: []NavItem{{Label: "R", Show: "all"}}}
	local := &Manifest{Title: "Local", Accent: " ", Pages: []string{}, Sitemap: &off}
	m := merge(local, remote)
	if m.Title != "Local" || m.Description != "remote desc" || m.Accent != "#111111" {
		t.Errorf("text fields = %+v", m)
	}
	// An empty list in the config clears the site's, unlike a missing one.
	if m.Pages == nil || len(m.Pages) != 0 || len(m.Nav) != 1 || m.Sitemap == nil || *m.Sitemap {
		t.Errorf("lists = %+v", m)
	}
	if m := merge(nil, nil); m.Title != "" || m.Nav != nil {
		t.Errorf("empty merge = %+v", m)
	}
}

func TestManifestCheck(t *testing.T) {
	base, _ := url.Parse("https://example.com/")
	var nav []NavItem
	for range 12 {
		nav = append(nav, NavItem{Label: "Posts", Show: "posts"})
	}
	s := Manifest{
		Title:  "T\x1b]0;pwned\x07itle",
		Logo:   "\n\n  /\\ \x1b[2J\n\t|‮|  \n\n",
		Accent: "red",
		Feed:   "javascript:alert(1)",
		Pages:  []string{"/about#me", "ftp://example.com/x", "", "https://other.example/p q"},
		Nav: append([]NavItem{
			{Label: "Bad", Show: "drafts"},
			{Label: "", Show: "all"},
			{Label: "Script", URL: "javascript:alert(1)"},
			{Label: "Relative", URL: "/relative"},
			{Label: "Mail", URL: "mailto:me@example.com"},
			{Label: "Me\x1b[31m", Page: "/me"},
			{Label: strings.Repeat("x", 60), URL: "https://github.com/x"},
		}, nav...),
	}.check(base)
	if s.title != "T]0;pwneditle" {
		t.Errorf("title = %q", s.title)
	}
	if got := strings.Join(s.logo, "|"); got != "  /\\ [2J|    ||" {
		t.Errorf("logo = %q", got)
	}
	if s.accent != "" || s.feed != "" || !s.sitemap {
		t.Errorf("accent %q, feed %q, sitemap %v", s.accent, s.feed, s.sitemap)
	}
	want := []string{"https://example.com/about", "https://other.example/p%20q", "https://example.com/me"}
	if strings.Join(s.pages, " ") != strings.Join(want, " ") {
		t.Errorf("pages = %q", s.pages)
	}
	if len(s.nav) != maxNav {
		t.Fatalf("nav has %d items: %+v", len(s.nav), s.nav)
	}
	if n := s.nav[0]; n.Kind != NavLink || n.URL != "mailto:me@example.com" {
		t.Errorf("nav[0] = %+v", n)
	}
	if n := s.nav[1]; n.Kind != NavPage || n.Label != "Me[31m" || n.URL != "https://example.com/me" {
		t.Errorf("nav[1] = %+v", n)
	}
	if n := s.nav[2]; n.Kind != NavLink || len([]rune(n.Label)) != 41 {
		t.Errorf("nav[2] = %+v", n)
	}
	if n := s.nav[3]; n.Kind != NavPosts {
		t.Errorf("nav[3] = %+v", n)
	}

	if s := (Manifest{Accent: "#AbCdEf"}).check(base); s.accent != "#abcdef" {
		t.Errorf("accent = %q", s.accent)
	}
}

func TestLogoLimits(t *testing.T) {
	logo := strings.Repeat(strings.Repeat("é", 150)+"\n", 30)
	lines := logoLines(logo)
	if len(lines) != maxLogoLines || len([]rune(lines[0])) != maxLogoWidth {
		t.Errorf("%d lines, first %d runes", len(lines), len([]rune(lines[0])))
	}
}

// manifestSite serves a site with a feed, a sitemap and a manifest whose
// status the test controls.
func manifestSite(t *testing.T, manifest string, status *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("User-agent: *\nDisallow: /private\nSitemap: /sitemap.xml\n"))
	})
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Home</title><link rel="alternate" type="application/rss+xml" href="/rss.xml"></head></html>`))
	})
	feed := func(title string, items ...string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/rss+xml")
			var b strings.Builder
			for _, it := range items {
				b.WriteString(`<item><title>` + it + `</title><link>` + srv.URL + `/` + it + `</link><description>` + it + ` body</description></item>`)
			}
			w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>` + title + `</title>` + b.String() + `</channel></rss>`))
		}
	}
	mux.HandleFunc("/rss.xml", feed("Advertised", "advertised"))
	mux.HandleFunc("/alt.xml", feed("Alt", "kept", "drafts/hidden"))
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<url><loc>` + srv.URL + `/contact</loc></url>
<url><loc>` + srv.URL + `/tags/go</loc></url>
<url><loc>` + srv.URL + `/about</loc></url>
</urlset>`))
	})
	mux.HandleFunc(ManifestPath, func(w http.ResponseWriter, r *http.Request) {
		if s := int(status.Load()); s != 200 {
			http.Error(w, "nope", s)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"m1"`)
		if r.Header.Get("If-None-Match") == `"m1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte(manifest))
	})
	md := func(title string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/markdown")
			w.Write([]byte("# " + title + "\n\nText.\n"))
		}
	}
	for _, p := range []string{"contact", "tags/go", "about", "now", "zebra", "private/x", "kept", "advertised", "drafts/hidden"} {
		mux.HandleFunc("/"+p, md(p))
	}
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func titles(s *Site) string {
	var out []string
	for _, e := range s.Entries {
		out = append(out, e.Title)
	}
	return strings.Join(out, "|")
}

func TestManifestDiscover(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	srv := manifestSite(t, `{
  "$schema": "https://example.com/schema.json",
  "title": "From manifest",
  "accent": "#E8590C",
  "logo": "LOGO\u001b[2J",
  "feed": "/alt.xml",
  "pages": ["/zebra", "/now", "/private/x"],
  "exclude": ["/tags/", "/drafts/"],
  "nav": [{"label": "Pages", "show": "pages"}, {"label": "About", "page": "/about"}],
  "unknown": true
}`, &status)
	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
	site, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The manifest's feed wins, excluded entries are gone, and manifest
	// pages lead in the manifest's order, the menu page after them.
	if got := titles(site); got != "kept|zebra|now|about|contact" {
		t.Errorf("entries = %s", got)
	}
	if !site.Manifest || site.Title != "From manifest" || site.Accent != "#e8590c" || strings.Join(site.Logo, "") != "LOGO[2J" {
		t.Errorf("site = %q %q %q", site.Title, site.Accent, site.Logo)
	}
	if len(site.Nav) != 2 || site.Nav[1].URL != srv.URL+"/about" || site.Lookup(site.Nav[1].URL) == nil {
		t.Errorf("nav = %+v", site.Nav)
	}

	// An unchanged manifest and a moment's outage both keep it.
	for _, s := range []int32{200, 503} {
		status.Store(s)
		if site, err = d.Discover(context.Background(), site, nil); err != nil {
			t.Fatal(err)
		}
		if site.Title != "From manifest" || len(site.Nav) != 2 {
			t.Errorf("after %d: title %q, nav %+v", s, site.Title, site.Nav)
		}
	}
	// A removed manifest takes its settings with it.
	status.Store(404)
	if site, err = d.Discover(context.Background(), site, nil); err != nil {
		t.Fatal(err)
	}
	if site.Manifest || site.Title != "Advertised" || site.Nav != nil || site.Accent != "" {
		t.Errorf("after 404: %q %+v %q", site.Title, site.Nav, site.Accent)
	}
}

func TestManifestSitemapOffAndLocal(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	srv := manifestSite(t, `{"title": "Remote", "sitemap": false, "pages": ["/now"]}`, &status)
	base, _ := url.Parse(srv.URL + "/")
	d := &Discoverer{
		Client:   fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}),
		Base:     base,
		Manifest: &Manifest{Title: "Local", Nav: []NavItem{{Label: "Contact", Page: "/contact"}}},
	}
	site, err := d.Discover(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(site); got != "advertised|now|contact" {
		t.Errorf("entries = %s", got)
	}
	if site.Title != "Local" || len(site.Nav) != 1 {
		t.Errorf("title %q, nav %+v", site.Title, site.Nav)
	}
}

func TestBadManifestIgnored(t *testing.T) {
	for name, body := range map[string]string{
		"not json": `{"title": `,
		"array":    `[1, 2]`,
		"too big":  `{"title": "` + strings.Repeat("x", maxManifestBytes) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var status atomic.Int32
			status.Store(200)
			srv := manifestSite(t, body, &status)
			base, _ := url.Parse(srv.URL + "/")
			d := &Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}, Rate: 1000, Burst: 100}), Base: base}
			site, err := d.Discover(context.Background(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if site.Manifest || site.Title != "Advertised" {
				t.Errorf("manifest %v, title %q", site.Manifest, site.Title)
			}
		})
	}
}
