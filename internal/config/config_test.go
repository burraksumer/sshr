package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	ok := Default()
	ok.URL = "example.com"
	if err := ok.Validate(); err != nil {
		t.Fatalf("default config: %v", err)
	}
	bad := map[string]func(*Config){
		"zero refresh":   func(c *Config) { c.Refresh = 0 },
		"short refresh":  func(c *Config) { c.Refresh = Duration(10 * time.Second) },
		"negative idle":  func(c *Config) { c.IdleTimeout = Duration(-time.Second) },
		"zero per ip":    func(c *Config) { c.MaxPerIP = 0 },
		"bad log format": func(c *Config) { c.LogFormat = "xml" },
		"no url":         func(c *Config) { c.URL = "" },
		"non-http url":   func(c *Config) { c.URL = "ftp://example.com" },
	}
	for name, mutate := range bad {
		c := ok
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestLoadRequired(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.toml")
	if _, err := Load(missing, false); err != nil {
		t.Errorf("optional missing file: %v", err)
	}
	if _, err := Load(missing, true); err == nil {
		t.Error("required missing file: no error")
	}
	path := filepath.Join(t.TempDir(), "sshr.toml")
	os.WriteFile(path, []byte("url = \"example.com\"\nrefresh = \"5m\"\n"), 0o600)
	c, err := Load(path, true)
	if err != nil || c.URL != "example.com" || c.Refresh.D() != 5*time.Minute {
		t.Errorf("Load = %+v, %v", c, err)
	}
}

func TestValidateProxy(t *testing.T) {
	c := Default()
	c.URL = "example.com"
	c.ProxyProtocol = true
	if c.Validate() == nil {
		t.Error("proxy_protocol without trusted_proxies: no error")
	}
	c.TrustedProxies = []string{"10.0.0.0/8", "192.0.2.1"}
	if err := c.Validate(); err != nil {
		t.Errorf("valid proxies: %v", err)
	}
	c.TrustedProxies = []string{"10.0.0.0/33"}
	if c.Validate() == nil {
		t.Error("bad CIDR: no error")
	}
}

func TestHostsKeepPort(t *testing.T) {
	c := Config{AllowedHosts: []string{"cdn.example.com"}}
	u, _ := ParseSiteURL("http://localhost:8080")
	if got := c.Hosts(u); got[0] != "localhost:8080" || len(got) != 2 {
		t.Errorf("Hosts = %v", got)
	}
	u, _ = ParseSiteURL("example.com")
	if got := c.Hosts(u); len(got) != 3 || got[0] != "example.com" || got[1] != "www.example.com" {
		t.Errorf("Hosts = %v", got)
	}
}

func TestValidateAllowedHosts(t *testing.T) {
	c := Default()
	c.URL = "example.com"
	for _, ok := range []string{"cdn.example.com", "cdn.example.com:8443", "192.0.2.1", "[::1]:9000"} {
		c.AllowedHosts = []string{ok}
		if err := c.Validate(); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"https://cdn.example.com", "cdn.example.com/feeds", "cdn.example.com:0", "cdn.example.com:http", "cdn.example.com:", ""} {
		c.AllowedHosts = []string{bad}
		if c.Validate() == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sshr.toml")
	os.WriteFile(path, []byte("url = \"example.com\"\nmax_per_ipp = 2\n"), 0o600)
	if _, err := Load(path, true); err == nil || !strings.Contains(err.Error(), "max_per_ipp") {
		t.Errorf("err = %v, want unknown setting max_per_ipp", err)
	}
}
