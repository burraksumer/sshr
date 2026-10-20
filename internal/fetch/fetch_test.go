package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAllowlistPorts(t *testing.T) {
	c := New(Options{Hosts: []string{"example.com", "LocalHost:8080", "[::1]:9000"}})
	cases := map[string]bool{
		"https://example.com/a":             true,
		"http://example.com/a":              true,
		"https://example.com:443/a":         true,
		"http://example.com:9090/":          false,
		"https://EXAMPLE.com/a":             true,
		"http://localhost:8080/":            true,
		"http://localhost/":                 false,
		"http://localhost:6379/":            false,
		"http://[::1]:9000/":                true,
		"http://[::1]/":                     false,
		"ftp://example.com/":                false,
		"https://evil.example/":             false,
		"https://example.com@evil.example/": false,
	}
	for raw, want := range cases {
		if got := c.checkURL(mustURL(t, raw)) == nil; got != want {
			t.Errorf("%s allowed = %v, want %v", raw, got, want)
		}
	}
}

func testServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, New(Options{Hosts: []string{mustURL(t, srv.URL).Host}, Rate: 1000, Burst: 100})
}

func TestUnconditional304IsAnError(t *testing.T) {
	srv, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	if _, err := c.Get(context.Background(), mustURL(t, srv.URL+"/x"), "", true); err == nil {
		t.Error("304 without validators should be an error")
	}
}

func TestRedirectRespectsRobots(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("User-agent: *\nDisallow: /private/\nCrawl-delay: 2\n"))
	})
	mux.HandleFunc("/p/1", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/private/draft", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/private/draft", func(w http.ResponseWriter, r *http.Request) {
		t.Error("fetched a page robots.txt disallows")
	})
	srv, c := testServer(t, mux.ServeHTTP)
	if _, err := c.LoadRobots(context.Background(), mustURL(t, srv.URL)); err != nil {
		t.Fatal(err)
	}
	if got := c.limiter.Limit(); got != rate.Every(2*time.Second) || c.limiter.Burst() != 1 {
		t.Errorf("crawl-delay not applied: limit %v burst %d", got, c.limiter.Burst())
	}
	// The delay would slow the test down. Lift it; robots rules still apply.
	c.limiter.SetLimit(rate.Inf)
	_, err := c.Get(context.Background(), mustURL(t, srv.URL+"/p/1"), "", false)
	if !errors.Is(err, ErrDisallowed) {
		t.Errorf("err = %v, want ErrDisallowed", err)
	}
}

func TestValidatorsPruned(t *testing.T) {
	srv, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"x"`)
	})
	ctx := context.Background()
	c.StartRound()
	c.Get(ctx, mustURL(t, srv.URL+"/a"), "", false)
	c.Get(ctx, mustURL(t, srv.URL+"/b"), "", false)
	c.EndRound()
	c.StartRound()
	c.Get(ctx, mustURL(t, srv.URL+"/a"), "", false)
	c.EndRound()
	if _, ok := c.validators[srv.URL+"/b"]; ok {
		t.Error("validator for /b survived a round without requests")
	}
	if _, ok := c.validators[srv.URL+"/a"]; !ok {
		t.Error("validator for /a was dropped")
	}
}

func TestRobotsUnreachableDisallowsAll(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		// Close the connection without answering.
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		t.Error("fetched a page although robots.txt was unreachable")
	})
	srv, c := testServer(t, mux.ServeHTTP)
	if _, err := c.LoadRobots(context.Background(), mustURL(t, srv.URL)); err == nil {
		t.Error("LoadRobots: no error for an unreachable robots.txt")
	}
	if _, err := c.Get(context.Background(), mustURL(t, srv.URL+"/page"), "", false); !errors.Is(err, ErrDisallowed) {
		t.Errorf("err = %v, want ErrDisallowed", err)
	}
}

func TestRobotsPerHost(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Write([]byte("User-agent: *\nAllow: /\n"))
		}
	}))
	defer site.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Write([]byte("User-agent: sshr\nDisallow: /\n"))
			return
		}
		t.Errorf("fetched %s on a host whose robots.txt disallows sshr", r.URL.Path)
	}))
	defer other.Close()

	c := New(Options{Hosts: []string{mustURL(t, site.URL).Host, mustURL(t, other.URL).Host}, Rate: 1000, Burst: 100})
	ctx := context.Background()
	c.LoadRobots(ctx, mustURL(t, site.URL))
	if _, err := c.Get(ctx, mustURL(t, other.URL+"/post"), "", false); !errors.Is(err, ErrDisallowed) {
		t.Errorf("err = %v, want ErrDisallowed from the other host's robots.txt", err)
	}
}

func TestRobots5xxDisallowsAll(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		t.Error("fetched a page although robots.txt answered 503")
	})
	srv, c := testServer(t, mux.ServeHTTP)
	c.LoadRobots(context.Background(), mustURL(t, srv.URL))
	if _, err := c.Get(context.Background(), mustURL(t, srv.URL+"/page"), "", false); !errors.Is(err, ErrDisallowed) {
		t.Errorf("err = %v, want ErrDisallowed", err)
	}
}

// robots.txt that redirects to a host sshr won't contact counts as missing.
func TestRobotsRedirectOffAllowlist(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://cdn.elsewhere.example/robots.txt", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {})
	srv, c := testServer(t, mux.ServeHTTP)
	c.LoadRobots(context.Background(), mustURL(t, srv.URL))
	if _, err := c.Get(context.Background(), mustURL(t, srv.URL+"/page"), "", false); err != nil {
		t.Errorf("err = %v, want the page to be allowed", err)
	}
}

// A redirect hop waits on the rate limiter outside the request timeout, so a
// slow rate, as with a Crawl-delay, doesn't make redirected fetches fail.
func TestRedirectWaitNotInTimeout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/b", http.StatusMovedPermanently) })
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	// One request per 400ms, and each request may take 200ms.
	c := New(Options{Hosts: []string{mustURL(t, srv.URL).Host}, Rate: 2.5, Burst: 1, Timeout: 200 * time.Millisecond})
	res, err := c.Get(context.Background(), mustURL(t, srv.URL+"/a"), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Body) != "ok" || res.URL.Path != "/b" {
		t.Errorf("got %q from %s", res.Body, res.URL)
	}
}

func TestRobotsRedirectLoopAllows(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/robots.txt", http.StatusFound) })
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {})
	srv, c := testServer(t, mux.ServeHTTP)
	c.LoadRobots(context.Background(), mustURL(t, srv.URL))
	if _, err := c.Get(context.Background(), mustURL(t, srv.URL+"/page"), "", false); err != nil {
		t.Errorf("err = %v, want the page allowed after a robots.txt redirect loop", err)
	}
}

// A robots.txt over the read limit is cut, not treated as unreachable.
func TestLargeRobotsTruncated(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("User-agent: *\nDisallow: /private\n"))
		w.Write([]byte("# " + strings.Repeat("x", 6<<20) + "\n"))
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {})
	srv, c := testServer(t, mux.ServeHTTP)
	if _, err := c.LoadRobots(context.Background(), mustURL(t, srv.URL)); err != nil {
		t.Fatalf("LoadRobots: %v", err)
	}
	if _, err := c.Get(context.Background(), mustURL(t, srv.URL+"/page"), "", false); err != nil {
		t.Errorf("page blocked: %v", err)
	}
	if _, err := c.Get(context.Background(), mustURL(t, srv.URL+"/private"), "", false); !errors.Is(err, ErrDisallowed) {
		t.Errorf("rules from the first part ignored: %v", err)
	}
}

func TestParseHost(t *testing.T) {
	for in, want := range map[string]string{
		"CDN.example.com": "cdn.example.com 80,443", "cdn.example.com:8443": "cdn.example.com 8443",
		"[::1]:9000": "::1 9000", "192.0.2.1": "192.0.2.1 80,443",
	} {
		h, ports, err := ParseHost(in)
		if got := h + " " + strings.Join(ports, ","); err != nil || got != want {
			t.Errorf("ParseHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"https://x", "x/y", "x:", "x:0", ""} {
		if _, _, err := ParseHost(bad); err == nil {
			t.Errorf("ParseHost(%q): no error", bad)
		}
	}
}

// A page in windows-1254, as many Turkish sites use, comes back as UTF-8.
func TestCharsetToUTF8(t *testing.T) {
	// "Çay ğüşı" in windows-1254.
	body := []byte{0xC7, 'a', 'y', ' ', 0xF0, 0xFC, 0xFE, 0xFD}
	srv, c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1254")
		w.Write(append([]byte("<p>"), append(body, []byte("</p>")...)...))
	})
	res, err := c.Get(context.Background(), mustURL(t, srv.URL+"/"), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Body), "Çay ğüşı") {
		t.Errorf("body = %q", res.Body)
	}
}

// Validators for a redirect target aren't sent when the caller asked for
// the redirecting URL, so its stale copy isn't kept by a 304 meant for the
// target.
func TestNoValidatorsOnLaterHops(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/new", http.StatusMovedPermanently) })
	mux.HandleFunc("/new", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"n"`)
		if r.Header.Get("If-None-Match") == `"n"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte("new page"))
	})
	srv, c := testServer(t, mux.ServeHTTP)
	ctx := context.Background()
	c.Get(ctx, mustURL(t, srv.URL+"/new"), "", false) // stores /new's ETag
	res, err := c.Get(ctx, mustURL(t, srv.URL+"/old"), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.NotModified || string(res.Body) != "new page" {
		t.Errorf("got 304 = %v, body %q, want the full new page", res.NotModified, res.Body)
	}
}

// UTF-8 with no declared charset stays UTF-8 even when the first kilobyte is
// plain ASCII.
func TestUTF8WithoutCharsetUnchanged(t *testing.T) {
	body := strings.Repeat("a", 1100) + " Sümer"
	for _, ct := range []string{"text/markdown", "text/html", "text/plain"} {
		if got := string(toUTF8([]byte(body), ct, ct)); got != body {
			t.Errorf("%s: got %q", ct, got[len(got)-12:])
		}
	}
}

// After a permanent redirect, a conditional request goes straight to the new
// URL and can get a 304.
func TestMovedURLRequestedDirectly(t *testing.T) {
	var oldHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/post", func(w http.ResponseWriter, r *http.Request) {
		oldHits.Add(1)
		http.Redirect(w, r, "/post/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/post/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"p"`)
		if r.Header.Get("If-None-Match") == `"p"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte("page"))
	})
	srv, c := testServer(t, mux.ServeHTTP)
	ctx := context.Background()
	c.StartRound()
	if _, err := c.Get(ctx, mustURL(t, srv.URL+"/post"), "", false); err != nil {
		t.Fatal(err)
	}
	c.EndRound()
	c.StartRound()
	res, err := c.Get(ctx, mustURL(t, srv.URL+"/post"), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.NotModified || oldHits.Load() != 1 {
		t.Errorf("304 = %v, old URL hits = %d, want a 304 and one hit", res.NotModified, oldHits.Load())
	}
}

func TestUnicodeHostMatchesPunycode(t *testing.T) {
	c := New(Options{Hosts: []string{"bücher.de"}})
	for _, raw := range []string{"https://xn--bcher-kva.de/", "https://bücher.de/", "https://BÜCHER.de/"} {
		if err := c.checkURL(mustURL(t, raw)); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	if err := c.checkURL(mustURL(t, "https://bucher.de/")); err == nil {
		t.Error("a different host matched")
	}
}

// A robots.txt answering 429 means slow down, not "no rules".
func TestRobots429Unavailable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	})
	srv, c := testServer(t, mux.ServeHTTP)
	if _, err := c.LoadRobots(context.Background(), mustURL(t, srv.URL)); !errors.Is(err, ErrRobotsUnavailable) {
		t.Errorf("err = %v, want ErrRobotsUnavailable", err)
	}
	if _, err := c.Get(context.Background(), mustURL(t, srv.URL+"/x"), "", false); !errors.Is(err, ErrRobotsUnavailable) {
		t.Errorf("fetch err = %v, want ErrRobotsUnavailable", err)
	}
}

// A remembered redirect whose target is gone falls back to the original URL.
func TestMovedTargetGone(t *testing.T) {
	var moved atomic.Bool
	moved.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
		if moved.Load() {
			http.Redirect(w, r, "/feed.xml", http.StatusMovedPermanently)
			return
		}
		w.Write([]byte("feed at the old place"))
	})
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		if !moved.Load() {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("feed"))
	})
	srv, c := testServer(t, mux.ServeHTTP)
	ctx := context.Background()
	c.Get(ctx, mustURL(t, srv.URL+"/feed"), "", false)
	moved.Store(false)
	res, err := c.Get(ctx, mustURL(t, srv.URL+"/feed"), "", true)
	if err != nil || string(res.Body) != "feed at the old place" {
		t.Errorf("got %q, %v", res.Body, err)
	}
}

// U+0130 lowercases to a plain i in Go, but IDNA maps it to i plus a
// combining dot, which is a different domain. Such hosts must not pass as
// the ASCII one, and a redirect to one must not reach another server.
func TestDottedCapitalINotAllowed(t *testing.T) {
	c := New(Options{Hosts: []string{"i.com", "mysite.example"}})
	for _, raw := range []string{"http://İ.com/", "http://mySİte.example/secret"} {
		if err := c.checkURL(mustURL(t, raw)); err == nil {
			t.Errorf("%s passed the allowlist", raw)
		}
	}
	if HostKey("İ.com") == "i.com" {
		t.Error("HostKey folds İ to i")
	}
	if got := ASCIIHost(mustURL(t, "https://bücher.de:8443/x")).Host; got != "xn--bcher-kva.de:8443" {
		t.Errorf("asciiHost = %q", got)
	}
}

// Robots rules are stored under the host they came from, so a host with İ
// can't lend its robots.txt to the ASCII host it lowercases to.
func TestRobotsOriginUsesDialedHost(t *testing.T) {
	if got := origin(mustURL(t, "http://xİ.example/a")); got == "http://xi.example" {
		t.Errorf("origin = %q, the lowercased host", got)
	}
	if got := origin(mustURL(t, "https://Bücher.de/x")); got != "https://xn--bcher-kva.de" {
		t.Errorf("origin = %q", got)
	}
}

// XML keeps its own encoding declaration for its parser, whatever the
// Content-Type says.
func TestToUTF8LeavesXML(t *testing.T) {
	body := append([]byte(`<?xml version="1.0" encoding="ISO-8859-9"?><rss><t>`), 0xE7, '<', '/', 't', '>')
	if got := toUTF8(body, "text/plain", "text/plain; charset=iso-8859-9"); string(got) != string(body) {
		t.Errorf("XML converted: %q", got)
	}
}

// A passing failure at a remembered redirect target keeps the mapping.
func TestMovedKeptOnPassingFailure(t *testing.T) {
	var down atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/post", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/post/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/post/", func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("page"))
	})
	srv, c := testServer(t, mux.ServeHTTP)
	ctx := context.Background()
	c.Get(ctx, mustURL(t, srv.URL+"/post"), "", false)
	down.Store(true)
	c.Get(ctx, mustURL(t, srv.URL+"/post"), "", true)
	c.mu.Lock()
	_, ok := c.moved[srv.URL+"/post"]
	c.mu.Unlock()
	if !ok {
		t.Error("remembered redirect dropped on a 503")
	}
}

func TestXHTMLWithDeclarationConverted(t *testing.T) {
	body := append([]byte(`<?xml version="1.0" encoding="iso-8859-9"?><html><body><p>`), 0xFE, 0xF0, '<', '/', 'p', '>')
	got := toUTF8(body, "application/xhtml+xml", "application/xhtml+xml; charset=iso-8859-9")
	if !strings.Contains(string(got), "şğ") {
		t.Errorf("XHTML not converted: %q", got)
	}
}

func TestXHTMLEncodingFromDeclaration(t *testing.T) {
	body := append([]byte(`<?xml version="1.0" encoding="ISO-8859-9"?><html><body><p>`), 0xFE, 0xF0, '<', '/', 'p', '>')
	if got := toUTF8(body, "application/xhtml+xml", "application/xhtml+xml"); !strings.Contains(string(got), "şğ") {
		t.Errorf("declared encoding ignored: %q", got)
	}
}
