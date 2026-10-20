package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burraksumer/sshr/internal/content"
	"github.com/burraksumer/sshr/internal/fetch"
	gossh "golang.org/x/crypto/ssh"
)

func TestClientKey(t *testing.T) {
	cases := map[string]string{
		"192.0.2.7":            "192.0.2.7",
		"2001:db8:1:2:aaaa::1": "2001:db8:1:2::/64",
		"2001:db8:1:2:bbbb::9": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":      "2001:db8:1:3::/64",
		"::ffff:192.0.2.7":     "::ffff:192.0.2.7",
		"not-an-ip":            "not-an-ip",
	}
	for in, want := range cases {
		if got := clientKey(in); got != want {
			t.Errorf("clientKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeTerm(t *testing.T) {
	if got := safeTerm("xterm-256color"); got != "xterm-256color" {
		t.Errorf("safeTerm = %q", got)
	}
	if got := safeTerm("x\u009d52;c;aGk=\u009c\x1b"); got != "x?52?c?aGk???" {
		t.Errorf("safeTerm = %q", got)
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(2, 3)
	r1, ok1 := l.acquire("a")
	_, ok2 := l.acquire("a")
	_, ok3 := l.acquire("a")
	_, ok4 := l.acquire("b")
	_, ok5 := l.acquire("c")
	if !ok1 || !ok2 || ok3 || !ok4 || ok5 {
		t.Fatalf("acquire results %v %v %v %v %v", ok1, ok2, ok3, ok4, ok5)
	}
	r1()
	r1() // releasing twice must not free two slots
	if _, ok := l.acquire("c"); !ok {
		t.Error("slot not freed")
	}
	if _, ok := l.acquire("d"); ok {
		t.Error("double release freed an extra slot")
	}
}

func startServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	base, _ := url.Parse("http://127.0.0.1:1/")
	store := content.NewStore(&content.Discoverer{Client: fetch.New(fetch.Options{Hosts: []string{base.Host}}), Base: base}, slog.New(slog.DiscardHandler))
	srv, err := New(Config{
		Listen: addr, HostKeyPath: filepath.Join(t.TempDir(), "key"),
		MaxSessions: 10, MaxPerIP: 5, IdleTimeout: time.Minute, MaxTimeout: time.Minute,
	}, store, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.ListenAndServe(ctx, nil)
	for range 50 {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return addr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server did not start")
	return ""
}

func dial(t *testing.T, addr string) *gossh.Client {
	t.Helper()
	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "reader",
		Auth:            []gossh.AuthMethod{gossh.KeyboardInteractive(func(string, string, []string, []bool) ([]string, error) { return nil, nil })},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestOneSessionChannelPerConnection(t *testing.T) {
	c := dial(t, startServer(t))
	if _, err := c.NewSession(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.NewSession(); err == nil {
		t.Error("second session channel was accepted")
	}
}

func TestEnvRequestsCapped(t *testing.T) {
	c := dial(t, startServer(t))
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	accepted := 0
	for i := range 40 {
		if s.Setenv(fmt.Sprintf("K%d", i), "v") == nil {
			accepted++
		}
	}
	if err := s.Setenv("BIG", strings.Repeat("x", 4096)); err == nil {
		t.Error("oversized env value accepted")
	}
	if accepted != maxEnv {
		t.Errorf("accepted %d env requests, want %d", accepted, maxEnv)
	}
}

// A connection whose only session is refused gets closed by the deadline.
func TestDeadlineAfterRefusedSession(t *testing.T) {
	old := sessionDeadline
	sessionDeadline = 500 * time.Millisecond
	defer func() { sessionDeadline = old }()

	c := dial(t, startServer(t))
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	// No PTY, so requirePTY refuses the session.
	s.Run("anything")
	done := make(chan struct{})
	go func() { c.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("connection still open after the deadline")
	}
}

func TestAgentForwardingRefused(t *testing.T) {
	c := dial(t, startServer(t))
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	ok, err := s.SendRequest("auth-agent-req@openssh.com", true, nil)
	if err != nil || ok {
		t.Errorf("agent forwarding request: ok=%v err=%v, want refused", ok, err)
	}
}
