// Package config loads sshr.toml and applies environment overrides.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/burraksumer/sshr/internal/content"
)

// Config is the full sshr configuration. Only URL is required.
type Config struct {
	// URL is the site to serve.
	URL string `toml:"url"`
	// AllowedHosts are extra hosts sshr may fetch from, for sites that serve
	// their feed or pages from another domain.
	AllowedHosts []string `toml:"allowed_hosts"`
	// Refresh is how often the site is re-read.
	Refresh Duration `toml:"refresh"`
	// MaxEntries caps how many posts and pages are kept.
	MaxEntries int `toml:"max_entries"`

	Listen         string   `toml:"listen"`
	HostKey        string   `toml:"host_key"`
	MaxSessions    int      `toml:"max_sessions"`
	MaxPerIP       int      `toml:"max_per_ip"`
	IdleTimeout    Duration `toml:"idle_timeout"`
	MaxSession     Duration `toml:"max_session"`
	ProxyProtocol  bool     `toml:"proxy_protocol"`
	TrustedProxies []string `toml:"trusted_proxies"`

	// LogFormat is "json" or "text".
	LogFormat string `toml:"log_format"`
}

// Default returns the configuration used when a field is not set.
func Default() Config {
	return Config{
		Refresh:     Duration(15 * time.Minute),
		MaxEntries:  content.DefaultMaxEntries,
		Listen:      ":2222",
		HostKey:     ".sshr/host_ed25519",
		MaxSessions: 200,
		MaxPerIP:    5,
		IdleTimeout: Duration(15 * time.Minute),
		MaxSession:  Duration(2 * time.Hour),
		LogFormat:   "json",
	}
}

// Load reads path, if it exists, over the defaults, then applies SSHR_*
// environment variables. A missing file is not an error unless required.
func Load(path string, required bool) (Config, error) {
	cfg := Default()
	if path != "" {
		md, err := toml.DecodeFile(path, &cfg)
		switch {
		case errors.Is(err, os.ErrNotExist) && !required:
		case err != nil:
			return cfg, fmt.Errorf("%s: %w", path, err)
		default:
			// A misspelled key would otherwise leave a default in place
			// without anyone noticing.
			if keys := md.Undecoded(); len(keys) > 0 {
				names := make([]string, len(keys))
				for i, k := range keys {
					names[i] = k.String()
				}
				return cfg, fmt.Errorf("%s: unknown setting %s", path, strings.Join(names, ", "))
			}
		}
	}
	env := map[string]*string{
		"SSHR_URL":        &cfg.URL,
		"SSHR_LISTEN":     &cfg.Listen,
		"SSHR_HOST_KEY":   &cfg.HostKey,
		"SSHR_LOG_FORMAT": &cfg.LogFormat,
	}
	for k, p := range env {
		if v := os.Getenv(k); v != "" {
			*p = v
		}
	}
	return cfg, nil
}

// MinRefresh is the shortest allowed refresh interval, so a typo can't make
// sshr crawl a site in a loop.
const MinRefresh = time.Minute

// Validate checks values that would make sshr misbehave rather than fail.
func (c Config) Validate() error {
	if _, err := c.SiteURL(); err != nil {
		return err
	}
	if c.Refresh.D() < MinRefresh {
		return fmt.Errorf("refresh must be at least %s, got %s", MinRefresh, c.Refresh.D())
	}
	for name, d := range map[string]Duration{"idle_timeout": c.IdleTimeout, "max_session": c.MaxSession} {
		if d < 0 {
			return fmt.Errorf("%s can't be negative. Use \"0s\" for no limit", name)
		}
	}
	for name, n := range map[string]int{"max_entries": c.MaxEntries, "max_sessions": c.MaxSessions, "max_per_ip": c.MaxPerIP} {
		if n <= 0 {
			return fmt.Errorf("%s must be positive, got %d", name, n)
		}
	}
	for _, h := range c.AllowedHosts {
		if err := checkHost(h); err != nil {
			return fmt.Errorf("allowed_hosts: %w", err)
		}
	}
	if c.ProxyProtocol && len(c.TrustedProxies) == 0 {
		return errors.New("proxy_protocol needs trusted_proxies, or every connection is rejected")
	}
	for _, p := range c.TrustedProxies {
		if _, _, err := net.ParseCIDR(p); err != nil && net.ParseIP(p) == nil {
			return fmt.Errorf("trusted_proxies: %q is not an IP address or CIDR range", p)
		}
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		return fmt.Errorf(`log_format must be "json" or "text", got %q`, c.LogFormat)
	}
	return nil
}

// SiteURL validates URL and returns it parsed.
func (c Config) SiteURL() (*url.URL, error) {
	return ParseSiteURL(c.URL)
}

// ParseSiteURL accepts "example.com" or a full http(s) URL.
func ParseSiteURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("no site URL set. Set url in sshr.toml, SSHR_URL or --url")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("site URL must be http or https, got %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("site URL %q has no host", raw)
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u, nil
}

// Hosts returns where sshr may fetch from, which is the site's host, its www
// or bare variant, and AllowedHosts. A "host" entry allows ports 80 and 443,
// and "host:port" allows that port. A site URL with an explicit port allows
// only that port.
func (c Config) Hosts(site *url.URL) []string {
	h := strings.ToLower(site.Hostname())
	withPort := func(host string) string {
		if p := site.Port(); p != "" {
			return net.JoinHostPort(host, p)
		}
		return host
	}
	hosts := []string{withPort(h)}
	if bare, ok := strings.CutPrefix(h, "www."); ok {
		hosts = append(hosts, withPort(bare))
	} else if strings.Contains(h, ".") && net.ParseIP(h) == nil {
		hosts = append(hosts, withPort("www."+h))
	}
	for _, a := range c.AllowedHosts {
		hosts = append(hosts, strings.ToLower(strings.TrimSpace(a)))
	}
	return hosts
}

const hostFormat = "should be a host like cdn.example.com or cdn.example.com:8443, without a scheme or path"

// checkHost accepts "host" or "host:port", without a scheme or path.
func checkHost(h string) error {
	h = strings.TrimSpace(h)
	if strings.ContainsAny(h, "/@?# ") {
		return fmt.Errorf("%q %s", h, hostFormat)
	}
	host, port, err := net.SplitHostPort(h)
	if err != nil {
		host, port = strings.Trim(h, "[]"), ""
	}
	if host == "" || strings.ContainsAny(host, "/:@?#% ") && net.ParseIP(host) == nil {
		return fmt.Errorf("%q %s", h, hostFormat)
	}
	if err == nil {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%q has an invalid port", h)
		}
	}
	return nil
}

// Duration is a time.Duration that reads from TOML strings like "15m".
type Duration time.Duration

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }
