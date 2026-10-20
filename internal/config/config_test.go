package config

import (
	"os"
	"path/filepath"
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
