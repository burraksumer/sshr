// Package fetch is the only way sshr talks to the network. It enforces a host
// allowlist, robots.txt, a request rate, and response size limits, and it
// remembers ETag and Last-Modified validators for conditional requests.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/temoto/robotstxt"
	"golang.org/x/time/rate"
)

// UserAgent identifies sshr to the sites it reads. Site owners can match
// "sshr" in robots.txt.
const UserAgent = "sshr/0.1 (+https://sshr.dev)"

const robotsAgent = "sshr"

var (
	// ErrNotAllowed means the URL's host is not on the allowlist.
	ErrNotAllowed = errors.New("fetch: host not allowed")
	// ErrDisallowed means robots.txt asks sshr not to fetch the URL.
	ErrDisallowed = errors.New("fetch: disallowed by robots.txt")
)

// Options configures a Client.
type Options struct {
	// Hosts lists the hostnames the client may contact. Requests and redirects
	// to any other host fail with ErrNotAllowed.
	Hosts []string
	// Rate is the steady request rate per second. Burst allows short spikes.
	Rate  float64
	Burst int
	// MaxBytes caps the size of a response body.
	MaxBytes int64
	// Timeout bounds a single request, including reading the body.
	Timeout time.Duration
}

// Response is a fetched document.
type Response struct {
	URL         *url.URL // final URL after redirects
	Status      int
	ContentType string // media type only, lowercased
	Body        []byte
	// NotModified is true when the server answered 304 to a conditional
	// request. Body is empty, and the caller should keep its previous result.
	NotModified bool
}

type validators struct {
	etag, lastModified string
}

// Client fetches documents from a fixed set of hosts.
type Client struct {
	http     *http.Client
	hosts    map[string]bool
	limiter  *rate.Limiter
	maxBytes int64

	mu         sync.Mutex
	robots     *robotstxt.Group
	validators map[string]validators
}

// New returns a Client limited to opts.Hosts.
func New(opts Options) *Client {
	if opts.Rate <= 0 {
		opts.Rate = 4
	}
	if opts.Burst <= 0 {
		opts.Burst = 4
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 5 << 20
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}
	c := &Client{
		hosts:      map[string]bool{},
		limiter:    rate.NewLimiter(rate.Limit(opts.Rate), opts.Burst),
		maxBytes:   opts.MaxBytes,
		validators: map[string]validators{},
	}
	for _, h := range opts.Hosts {
		c.hosts[strings.ToLower(h)] = true
	}
	c.http = &http.Client{
		Timeout: opts.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("fetch: too many redirects")
			}
			return c.checkURL(req.URL)
		},
	}
	return c
}

// Allowed reports whether u is on the allowlist and not blocked by robots.txt.
func (c *Client) Allowed(u *url.URL) bool {
	return c.checkURL(u) == nil && c.robotsAllow(u)
}

func (c *Client) checkURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q", ErrNotAllowed, u.Scheme)
	}
	if !c.hosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("%w: %s", ErrNotAllowed, u.Hostname())
	}
	return nil
}

func (c *Client) robotsAllow(u *url.URL) bool {
	c.mu.Lock()
	g := c.robots
	c.mu.Unlock()
	if g == nil {
		return true
	}
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return g.Test(p)
}

// LoadRobots fetches robots.txt for base and applies it to later requests.
// It returns the Sitemap URLs listed in the file. A missing robots.txt allows
// everything.
func (c *Client) LoadRobots(ctx context.Context, base *url.URL) ([]string, error) {
	u := base.ResolveReference(&url.URL{Path: "/robots.txt"})
	resp, err := c.get(ctx, u, "text/plain", false, false, c.maxBytes)
	if err != nil {
		return nil, err
	}
	data, err := robotstxt.FromStatusAndBytes(resp.Status, resp.Body)
	if err != nil {
		// An unparseable file is treated like a missing one.
		data, _ = robotstxt.FromStatusAndBytes(http.StatusNotFound, nil)
	}
	c.mu.Lock()
	c.robots = data.FindGroup(robotsAgent)
	c.mu.Unlock()
	return data.Sitemaps, nil
}

// Get fetches u. When conditional is true and the client has validators from
// an earlier response, it sends If-None-Match and If-Modified-Since.
func (c *Client) Get(ctx context.Context, u *url.URL, accept string, conditional bool) (*Response, error) {
	return c.get(ctx, u, accept, conditional, true, c.maxBytes)
}

// GetLarge is Get with a larger size limit, for feeds that carry the full
// text of many posts.
func (c *Client) GetLarge(ctx context.Context, u *url.URL, accept string, conditional bool) (*Response, error) {
	return c.get(ctx, u, accept, conditional, true, 5*c.maxBytes)
}

func (c *Client) get(ctx context.Context, u *url.URL, accept string, conditional, checkRobots bool, maxBytes int64) (*Response, error) {
	if err := c.checkURL(u); err != nil {
		return nil, err
	}
	if checkRobots && !c.robotsAllow(u) {
		return nil, fmt.Errorf("%w: %s", ErrDisallowed, u)
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	key := u.String()
	if conditional {
		c.mu.Lock()
		v, ok := c.validators[key]
		c.mu.Unlock()
		if ok {
			if v.etag != "" {
				req.Header.Set("If-None-Match", v.etag)
			}
			if v.lastModified != "" {
				req.Header.Set("If-Modified-Since", v.lastModified)
			}
		}
	}

	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	out := &Response{URL: res.Request.URL, Status: res.StatusCode}
	if res.StatusCode == http.StatusNotModified {
		out.NotModified = true
		return out, nil
	}
	if mt, _, err := mime.ParseMediaType(res.Header.Get("Content-Type")); err == nil {
		out.ContentType = strings.ToLower(mt)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("fetch: %s is larger than %d bytes", u, maxBytes)
	}
	out.Body = body

	if res.StatusCode == http.StatusOK {
		v := validators{etag: res.Header.Get("ETag"), lastModified: res.Header.Get("Last-Modified")}
		c.mu.Lock()
		if v.etag != "" || v.lastModified != "" {
			c.validators[key] = v
		} else {
			delete(c.validators, key)
		}
		c.mu.Unlock()
	}
	return out, nil
}
