// Package fetch is the only way sshr talks to the network. It enforces a host
// allowlist, robots.txt, a request rate, and response size limits, and it
// remembers ETag and Last-Modified validators for conditional requests.
package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/temoto/robotstxt"
	"golang.org/x/net/html/charset"
	"golang.org/x/time/rate"
)

// maxRobotsBytes is how much of a robots.txt is read. RFC 9309 asks
// crawlers to parse at least 500 KiB and ignore the rest.
const maxRobotsBytes = 512 << 10

// MaxCrawlDelay caps the Crawl-delay sshr honors. Longer delays would make a
// refresh of a few hundred pages take hours.
const MaxCrawlDelay = 30 * time.Second

// UserAgent identifies sshr to the sites it reads. Site owners can match
// "sshr" in robots.txt.
const UserAgent = "sshr/0.1 (+https://sshr.dev)"

const robotsAgent = "sshr"

var (
	// ErrNotAllowed means the URL's host and port are not on the allowlist.
	ErrNotAllowed = errors.New("fetch: host not allowed")
	// ErrDisallowed means robots.txt asks sshr not to fetch the URL.
	ErrDisallowed = errors.New("fetch: disallowed by robots.txt")
	// ErrRobotsUnavailable comes wrapped with ErrDisallowed when the reason
	// is that robots.txt couldn't be fetched this round. Callers may treat it
	// as a passing outage.
	ErrRobotsUnavailable = errors.New("fetch: robots.txt unavailable")
)

// Options configures a Client.
type Options struct {
	// Hosts lists where the client may connect, as "host" or "host:port". A
	// bare host allows ports 80 and 443 only. Requests and redirects anywhere
	// else fail with ErrNotAllowed.
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
	round              int
}

// Client fetches documents from a fixed set of hosts.
type Client struct {
	http     *http.Client
	timeout  time.Duration   // per request, not counting rate limiter waits
	hosts    map[string]bool // "host:port"
	limiter  *rate.Limiter
	rate     rate.Limit
	burst    int
	maxBytes int64

	mu         sync.Mutex
	robots     map[string]robotsEntry // by origin
	validators map[string]validators
	moved      map[string]moved // permanent redirects, by original URL
	round      int

	// robotsMu serializes robots.txt fetches so each origin is fetched once
	// per round.
	robotsMu sync.Mutex
}

// moved records where a URL redirected to permanently.
type moved struct {
	to    string
	round int
}

type robotsEntry struct {
	data *robotstxt.RobotsData
	// unavailable means robots.txt failed with a network error or a 5xx,
	// and data disallows everything for that reason.
	unavailable bool
	round       int
}

// disallowAll is used when robots.txt can't be fetched. RFC 9309 treats an
// unreachable robots.txt as a full disallow, like a 5xx. The rule lives on
// RobotsData, not on its groups, so callers must use TestAgent.
var disallowAll, _ = robotstxt.FromStatusAndBytes(http.StatusServiceUnavailable, nil)

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
		rate:       rate.Limit(opts.Rate),
		burst:      opts.Burst,
		maxBytes:   opts.MaxBytes,
		robots:     map[string]robotsEntry{},
		validators: map[string]validators{},
		moved:      map[string]moved{},
	}
	for _, h := range opts.Hosts {
		host, ports, err := ParseHost(h)
		if err != nil {
			continue // config validation reports these
		}
		for _, p := range ports {
			c.hosts[net.JoinHostPort(host, p)] = true
		}
	}
	c.timeout = opts.Timeout
	c.http = &http.Client{
		// get follows redirects itself.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c
}

// StartRound begins a refresh. robots.txt files are fetched again, and
// validators not used again before EndRound are forgotten, so URLs that left
// the site don't pile up.
func (c *Client) StartRound() {
	c.mu.Lock()
	c.round++
	c.mu.Unlock()
}

// Keep marks validators for rawURL as used this round, for URLs whose
// earlier copy is kept without a request, so EndRound doesn't drop them.
func (c *Client) Keep(rawURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := c.moved[rawURL]; ok {
		m.round = c.round
		c.moved[rawURL] = m
		rawURL = m.to
	}
	if v, ok := c.validators[rawURL]; ok {
		v.round = c.round
		c.validators[rawURL] = v
	}
}

// EndRound drops validators for URLs not requested since StartRound.
func (c *Client) EndRound() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.validators {
		if v.round < c.round {
			delete(c.validators, k)
		}
	}
	for k, m := range c.moved {
		if m.round < c.round {
			delete(c.moved, k)
		}
	}
}

const hostFormat = "should be a host like cdn.example.com or cdn.example.com:8443, without a scheme or path"

// ParseHost reads an allowlist entry, "host" or "host:port", and returns the
// lowercase host and the ports it allows. A bare host allows 80 and 443.
func ParseHost(h string) (host string, ports []string, err error) {
	h = strings.ToLower(strings.TrimSpace(h))
	if strings.ContainsAny(h, "/@?# ") {
		return "", nil, fmt.Errorf("%q %s", h, hostFormat)
	}
	host, port, splitErr := net.SplitHostPort(h)
	if splitErr != nil {
		host, port = strings.Trim(h, "[]"), ""
	}
	if host == "" || strings.ContainsAny(host, "/:@?#% ") && net.ParseIP(host) == nil {
		return "", nil, fmt.Errorf("%q %s", h, hostFormat)
	}
	if splitErr != nil {
		return host, []string{"80", "443"}, nil
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", nil, fmt.Errorf("%q has an invalid port", h)
	}
	return host, []string{port}, nil
}

// MayFetch reports whether u is on the allowlist and robots.txt for its host
// doesn't refuse it. While robots.txt is unavailable it says yes; the fetch
// itself then fails with ErrRobotsUnavailable, and callers can fall back to
// a copy from an earlier refresh.
func (c *Client) MayFetch(ctx context.Context, u *url.URL) bool {
	if c.checkURL(u) != nil {
		return false
	}
	allowed, unavailable := c.robotsAllow(ctx, u)
	return allowed || unavailable
}

func (c *Client) checkURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q", ErrNotAllowed, u.Scheme)
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	if !c.hosts[net.JoinHostPort(strings.ToLower(u.Hostname()), port)] {
		return fmt.Errorf("%w: %q", ErrNotAllowed, u.Host)
	}
	return nil
}

func robotsError(u *url.URL, unavailable bool) error {
	if unavailable {
		return fmt.Errorf("%w: %w: %q", ErrDisallowed, ErrRobotsUnavailable, u)
	}
	return fmt.Errorf("%w: %q", ErrDisallowed, u)
}

// origin identifies whose robots.txt applies to u.
func origin(u *url.URL) string {
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// robotsFor returns the robots.txt rules for u's origin, fetching them once
// per round.
func (c *Client) robotsFor(ctx context.Context, u *url.URL) robotsEntry {
	key := origin(u)
	c.mu.Lock()
	e, ok := c.robots[key]
	round := c.round
	c.mu.Unlock()
	if ok && e.round == round {
		return e
	}
	c.robotsMu.Lock()
	defer c.robotsMu.Unlock()
	c.mu.Lock()
	e, ok = c.robots[key]
	c.mu.Unlock()
	if ok && e.round == round {
		return e
	}
	e, _ = c.fetchRobots(ctx, u)
	return e
}

// fetchRobots fetches and stores robots.txt for u's origin.
func (c *Client) fetchRobots(ctx context.Context, u *url.URL) (robotsEntry, error) {
	ru := u.ResolveReference(&url.URL{Path: "/robots.txt"})
	e := robotsEntry{data: disallowAll, unavailable: true}
	resp, err := c.get(ctx, ru, "text/plain", false, false, -maxRobotsBytes)
	switch {
	case errors.Is(err, ErrNotAllowed), errors.Is(err, errTooManyRedirects):
		// robots.txt redirects to a host sshr won't contact, like a CDN,
		// or redirects too many times. RFC 9309 treats a robots.txt that
		// can't be followed as missing, which allows everything. Only
		// network errors and 5xx disallow.
		e.data, _ = robotstxt.FromStatusAndBytes(http.StatusNotFound, nil)
		e.unavailable, err = false, nil
	case err == nil:
		data, perr := robotstxt.FromStatusAndBytes(resp.Status, resp.Body)
		if perr != nil {
			// An unparseable file is treated like a missing one.
			data, _ = robotstxt.FromStatusAndBytes(http.StatusNotFound, nil)
		}
		e.data, e.unavailable = data, resp.Status >= 500
	}
	c.mu.Lock()
	e.round = c.round
	c.robots[origin(u)] = e
	c.mu.Unlock()
	return e, err
}

// robotsAllow reports whether robots.txt for u's origin allows u, and whether
// a refusal is only because robots.txt is unavailable right now.
func (c *Client) robotsAllow(ctx context.Context, u *url.URL) (allowed, unavailable bool) {
	e := c.robotsFor(ctx, u)
	data := e.data
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	if data.TestAgent(p, robotsAgent) {
		return true, false
	}
	return false, e.unavailable
}

// LoadRobots fetches robots.txt for the site's own origin, applies its
// Crawl-delay, and returns the Sitemap URLs it lists. A missing robots.txt
// allows everything. One that can't be fetched disallows everything, and
// LoadRobots returns the error.
func (c *Client) LoadRobots(ctx context.Context, base *url.URL) ([]string, error) {
	c.robotsMu.Lock()
	e, err := c.fetchRobots(ctx, base)
	c.robotsMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRobotsUnavailable, err)
	}
	if e.unavailable {
		return nil, ErrRobotsUnavailable
	}
	data := e.data
	if g := data.FindGroup(robotsAgent); g != nil && g.CrawlDelay > 0 {
		c.limiter.SetLimit(min(rate.Every(min(g.CrawlDelay, MaxCrawlDelay)), c.rate))
		c.limiter.SetBurst(1)
	} else {
		c.limiter.SetLimit(c.rate)
		c.limiter.SetBurst(c.burst)
	}
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
	// A URL that moved permanently is asked for at its new place, so a
	// conditional request can get a 304 instead of a redirect.
	start := u.String()
	if conditional {
		c.mu.Lock()
		if m, ok := c.moved[start]; ok {
			m.round = c.round
			c.moved[start] = m
			if to, err := url.Parse(m.to); err == nil {
				u = to
			}
		}
		c.mu.Unlock()
	}
	permanent := true
	// Redirects are followed here rather than by net/http, so every hop gets
	// the allowlist and robots.txt checks, waits on the rate limiter, and has
	// its own timeout that doesn't include that wait.
	for hop := 0; ; hop++ {
		if hop > maxRedirects {
			return nil, errTooManyRedirects
		}
		if err := c.checkURL(u); err != nil {
			return nil, err
		}
		if checkRobots {
			if allowed, unavailable := c.robotsAllow(ctx, u); !allowed {
				return nil, robotsError(u, unavailable)
			}
		}
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}
		// Validators only go with the URL the caller asked for. A later hop
		// may be a different page with its own validators, and a 304 for
		// it would make the caller keep a stale copy.
		res, next, status, err := c.do(ctx, u, accept, conditional && hop == 0, maxBytes)
		if err != nil {
			return nil, err
		}
		if next == nil {
			if hop > 0 && permanent && res.Status == http.StatusOK {
				c.mu.Lock()
				c.moved[start] = moved{to: u.String(), round: c.round}
				c.mu.Unlock()
			}
			return res, nil
		}
		permanent = permanent && (status == http.StatusMovedPermanently || status == http.StatusPermanentRedirect)
		u = next
	}
}

// toUTF8 converts HTML, markdown and plain text in another charset, like
// windows-1254 or ISO-8859-9, to UTF-8. The charset comes from the
// Content-Type header or, for HTML, a meta tag. XML feeds and sitemaps are
// left to their parsers, which handle encodings themselves.
func toUTF8(body []byte, mediaType, header string) []byte {
	switch mediaType {
	case "text/html", "application/xhtml+xml", "text/markdown", "text/x-markdown", "text/plain", "":
	default:
		return body
	}
	// Without a declared charset, valid UTF-8 stays as it is. The charset
	// package only looks at the first 1024 bytes and would call an ASCII
	// start windows-1252, garbling UTF-8 further down.
	_, params, _ := mime.ParseMediaType(header)
	if params["charset"] == "" && utf8.Valid(body) {
		return body
	}
	if strings.EqualFold(params["charset"], "utf-8") {
		return body
	}
	r, err := charset.NewReader(bytes.NewReader(body), header)
	if err != nil {
		return body
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return body
	}
	return out
}

const maxRedirects = 5

var errTooManyRedirects = errors.New("fetch: too many redirects")

// do sends one request. For a redirect it returns the next URL instead of
// a response. A positive maxBytes makes a larger body an error, and a
// negative one cuts the body to -maxBytes bytes.
func (c *Client) do(ctx context.Context, u *url.URL, accept string, conditional bool, maxBytes int64) (*Response, *url.URL, int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("User-Agent", UserAgent)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	key := u.String()
	sent := false
	c.mu.Lock()
	if v, ok := c.validators[key]; ok {
		v.round = c.round
		c.validators[key] = v
		if conditional {
			if v.etag != "" {
				req.Header.Set("If-None-Match", v.etag)
			}
			if v.lastModified != "" {
				req.Header.Set("If-Modified-Since", v.lastModified)
			}
			sent = true
		}
	}
	c.mu.Unlock()

	res, err := c.http.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		next, err := u.Parse(res.Header.Get("Location"))
		if err != nil || res.Header.Get("Location") == "" {
			return nil, nil, 0, fmt.Errorf("fetch: %q redirects to an invalid location", u)
		}
		return nil, next, res.StatusCode, nil
	case http.StatusNotModified:
		// Only trust a 304 that answers validators we sent. Callers keep
		// their previous result, which doesn't exist otherwise.
		if !sent {
			return nil, nil, 0, fmt.Errorf("fetch: %q answered 304 to an unconditional request", u)
		}
		return &Response{URL: u, Status: res.StatusCode, NotModified: true}, nil, res.StatusCode, nil
	}

	out := &Response{URL: u, Status: res.StatusCode}
	if mt, _, err := mime.ParseMediaType(res.Header.Get("Content-Type")); err == nil {
		out.ContentType = strings.ToLower(mt)
	}
	limit := maxBytes
	if limit < 0 {
		limit = -limit
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, nil, 0, err
	}
	if int64(len(body)) > limit {
		if maxBytes > 0 {
			return nil, nil, 0, fmt.Errorf("fetch: %q is larger than %d bytes", u, limit)
		}
		// Cut at the last line break, so a rule isn't left half written.
		body = body[:limit]
		if i := bytes.LastIndexByte(body, '\n'); i >= 0 {
			body = body[:i+1]
		}
	}
	out.Body = toUTF8(body, out.ContentType, res.Header.Get("Content-Type"))

	if res.StatusCode == http.StatusOK {
		v := validators{etag: res.Header.Get("ETag"), lastModified: res.Header.Get("Last-Modified")}
		c.mu.Lock()
		v.round = c.round
		if v.etag != "" || v.lastModified != "" {
			c.validators[key] = v
		} else {
			delete(c.validators, key)
		}
		c.mu.Unlock()
	}
	return out, nil, res.StatusCode, nil
}
