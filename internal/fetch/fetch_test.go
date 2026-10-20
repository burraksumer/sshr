package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
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
