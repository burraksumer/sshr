// Package server serves the reader over SSH to anonymous visitors. It never
// runs a shell or any other program, and it refuses SFTP, port forwarding and
// agent forwarding.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/ssh"
	"charm.land/wish/v2"
	bm "charm.land/wish/v2/bubbletea"
	"charm.land/wish/v2/ratelimiter"
	"github.com/burraksumer/sshr/internal/content"
	"github.com/burraksumer/sshr/internal/ui"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/pires/go-proxyproto"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/time/rate"
)

// Config holds the server's network and limit settings.
type Config struct {
	Listen      string
	HostKeyPath string
	// MaxSessions caps concurrent sessions across all visitors.
	MaxSessions int
	// MaxPerIP caps concurrent connections and sessions from one address.
	MaxPerIP    int
	IdleTimeout time.Duration
	MaxTimeout  time.Duration
	// ProxyProtocol makes the listener require a PROXY protocol header from
	// TrustedProxies and reject every other sender.
	ProxyProtocol  bool
	TrustedProxies []string
}

// Server is a running SSH server.
type Server struct {
	srv *ssh.Server
	cfg Config
	log *slog.Logger
}

// New builds a server that shows store's content.
func New(cfg Config, store *content.Store, log *slog.Logger) (*Server, error) {
	if cfg.MaxSessions <= 0 || cfg.MaxPerIP <= 0 {
		return nil, fmt.Errorf("server: MaxSessions and MaxPerIP must be positive")
	}
	if dir := filepath.Dir(cfg.HostKeyPath); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("host key dir: %w", err)
		}
	}

	conns := newLimiter(cfg.MaxPerIP, cfg.MaxSessions)
	sessions := newLimiter(cfg.MaxPerIP, cfg.MaxSessions)

	srv, err := wish.NewServer(
		wish.WithAddress(cfg.Listen),
		wish.WithHostKeyPath(cfg.HostKeyPath),
		wish.WithVersion("SSH-2.0-sshr"),
		// Anyone may read. Public keys are still signature-checked by the
		// SSH layer, and visitors without keys get keyboard-interactive
		// with no questions.
		wish.WithPublicKeyAuth(func(ssh.Context, ssh.PublicKey) bool { return true }),
		wish.WithKeyboardInteractiveAuth(func(ssh.Context, gossh.KeyboardInteractiveChallenge) bool { return true }),
		wish.WithIdleTimeout(cfg.IdleTimeout),
		wish.WithMaxTimeout(cfg.MaxTimeout),
		wish.WithMiddleware(
			// Middleware runs bottom to top, so the last one listed runs first.
			bm.Middleware(func(s ssh.Session) (tea.Model, []tea.ProgramOption) {
				return ui.New(store), nil
			}),
			requirePTY(),
			limitSessions(sessions),
			// New sessions per IP: a burst of MaxPerIP, then one per second.
			ratelimiter.Middleware(newRateLimiter(rate.Every(time.Second), cfg.MaxPerIP)),
			logSessions(log),
			markSession(),
		),
	)
	if err != nil {
		return nil, err
	}
	srv.HandshakeTimeout = 15 * time.Second
	srv.ConnCallback = func(ctx ssh.Context, c net.Conn) net.Conn {
		ip := hostOf(c.RemoteAddr())
		release, ok := conns.acquire(clientKey(ip))
		if !ok {
			log.Warn("connection refused", "ip", ip, "reason", "limit")
			return nil
		}
		tc := &trackedConn{Conn: c, release: release}
		// Authentication needs no prompts, so a client could hold a slot
		// without ever reading. Close connections that don't open a
		// session soon after connecting.
		tc.timer = time.AfterFunc(sessionDeadline, func() {
			if !tc.started.Load() {
				log.Info("closing connection without a session", "ip", ip)
				_ = tc.Close()
			}
		})
		ctx.SetValue(connKey{}, tc)
		return tc
	}
	// These match the library defaults but are set explicitly. No forwarding
	// of any kind and no subsystems such as SFTP.
	srv.LocalPortForwardingCallback = nil
	srv.ReversePortForwardingCallback = nil
	srv.SubsystemHandlers = map[string]ssh.SubsystemHandler{}
	srv.ChannelHandlers = map[string]ssh.ChannelHandler{"session": ssh.DefaultSessionHandler}
	srv.RequestHandlers = map[string]ssh.RequestHandler{}

	return &Server{srv: srv, cfg: cfg, log: log}, nil
}

// ListenAndServe serves until ctx is done, then shuts down gracefully.
func (s *Server) ListenAndServe(ctx context.Context) error {
	l, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	if s.cfg.ProxyProtocol {
		policy, err := proxyproto.TrustProxyHeaderFromRanges(s.cfg.TrustedProxies)
		if err != nil {
			return fmt.Errorf("trusted_proxies: %w", err)
		}
		l = &proxyproto.Listener{Listener: l, ConnPolicy: policy, ReadHeaderTimeout: 5 * time.Second}
	}
	s.log.Info("listening", "addr", l.Addr().String(), "proxy_protocol", s.cfg.ProxyProtocol)

	errc := make(chan error, 1)
	go func() { errc <- s.srv.Serve(l) }()
	select {
	case err := <-errc:
		if errors.Is(err, ssh.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// Readers never finish on their own, so give open sessions a moment
		// and then close them. Either way the shutdown is clean.
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.srv.Shutdown(sctx); errors.Is(err, context.DeadlineExceeded) {
			s.log.Info("closing open sessions")
			if err := s.srv.Close(); err != nil && !errors.Is(err, ssh.ErrServerClosed) {
				return err
			}
			return nil
		} else if err != nil && !errors.Is(err, ssh.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func requirePTY() wish.Middleware {
	return func(next ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			if _, _, ok := s.Pty(); !ok {
				wish.Fatalln(s, "sshr is an interactive reader and needs a terminal. Connect without a command, or add -t.")
				return
			}
			next(s)
		}
	}
}

func limitSessions(l *limiter) wish.Middleware {
	return func(next ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			release, ok := l.acquire(clientKey(hostOf(s.RemoteAddr())))
			if !ok {
				wish.Fatalln(s, "Too many readers right now. Try again in a minute.")
				return
			}
			defer release()
			next(s)
		}
	}
}

// logSessions logs session start and end. It doesn't log key fingerprints.
func logSessions(log *slog.Logger) wish.Middleware {
	return func(next ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			start := time.Now()
			pty, _, _ := s.Pty()
			ip := hostOf(s.RemoteAddr())
			log.Info("session start", "ip", ip, "term", safeTerm(pty.Term), "cols", pty.Window.Width, "rows", pty.Window.Height)
			next(s)
			log.Info("session end", "ip", ip, "seconds", int(time.Since(start).Seconds()))
		}
	}
}

// limiter counts concurrent holders per IP and in total.
type limiter struct {
	mu            sync.Mutex
	perIP         map[string]int
	total         int
	maxIP, maxAll int
}

func newLimiter(maxIP, maxAll int) *limiter {
	return &limiter{perIP: map[string]int{}, maxIP: maxIP, maxAll: maxAll}
}

func (l *limiter) acquire(ip string) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= l.maxAll || l.perIP[ip] >= l.maxIP {
		return nil, false
	}
	l.total++
	l.perIP[ip]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.total--
			if l.perIP[ip]--; l.perIP[ip] <= 0 {
				delete(l.perIP, ip)
			}
		})
	}, true
}

// sessionDeadline is how long a connection may stay open before it starts a
// session.
const sessionDeadline = 30 * time.Second

type connKey struct{}

type trackedConn struct {
	net.Conn
	release func()
	timer   *time.Timer
	started atomic.Bool
}

func (c *trackedConn) Close() error {
	if c.timer != nil {
		c.timer.Stop()
	}
	c.release()
	return c.Conn.Close()
}

// markSession records that a connection started a session, which keeps
// it from being closed by sessionDeadline.
func markSession() wish.Middleware {
	return func(next ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			if tc, ok := s.Context().Value(connKey{}).(*trackedConn); ok {
				tc.started.Store(true)
			}
			next(s)
		}
	}
}

// clientKey is what the per-client limits count. IPv6 clients are grouped by
// /64, the smallest block an ISP usually hands out, so one host can't look
// like millions of clients.
func clientKey(ip string) string {
	addr := net.ParseIP(ip)
	if addr == nil || addr.To4() != nil {
		return ip
	}
	return addr.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// safeTerm keeps the visitor-supplied TERM value to characters terminal names
// use, so logs can't carry control characters.
func safeTerm(t string) string {
	t = strings.Map(func(r rune) rune {
		if r < 0x80 && (r == '-' || r == '.' || r == '_' || r == '+' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return r
		}
		return '?'
	}, t)
	if len(t) > 64 {
		t = t[:64]
	}
	return t
}

// rateLimiter limits new sessions per client key to a burst, then a steady rate.
type rateLimiter struct {
	rate  rate.Limit
	burst int
	cache *lru.Cache[string, *rate.Limiter]
}

func newRateLimiter(r rate.Limit, burst int) *rateLimiter {
	cache, _ := lru.New[string, *rate.Limiter](10_000)
	return &rateLimiter{rate: r, burst: burst, cache: cache}
}

func (r *rateLimiter) Allow(s ssh.Session) error {
	key := clientKey(hostOf(s.RemoteAddr()))
	l, ok := r.cache.Get(key)
	if !ok {
		l = rate.NewLimiter(r.rate, r.burst)
		r.cache.Add(key, l)
	}
	if !l.Allow() {
		return ratelimiter.ErrRateLimitExceeded
	}
	return nil
}

func hostOf(a net.Addr) string {
	if a == nil {
		return ""
	}
	if host, _, err := net.SplitHostPort(a.String()); err == nil {
		return host
	}
	return a.String()
}
