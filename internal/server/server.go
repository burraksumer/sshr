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
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/ssh"
	"charm.land/wish/v2"
	bm "charm.land/wish/v2/bubbletea"
	"charm.land/wish/v2/ratelimiter"
	"github.com/burraksumer/sshr/internal/content"
	"github.com/burraksumer/sshr/internal/ui"
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
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 200
	}
	if cfg.MaxPerIP <= 0 {
		cfg.MaxPerIP = 5
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
			// Middleware runs bottom to top: the last one listed runs first.
			bm.Middleware(func(s ssh.Session) (tea.Model, []tea.ProgramOption) {
				return ui.New(store), nil
			}),
			requirePTY(),
			limitSessions(sessions),
			// New sessions per IP: a burst of MaxPerIP, then one per second.
			ratelimiter.Middleware(ratelimiter.NewRateLimiter(rate.Every(time.Second), cfg.MaxPerIP, 10_000)),
			logSessions(log),
		),
	)
	if err != nil {
		return nil, err
	}
	srv.HandshakeTimeout = 15 * time.Second
	srv.ConnCallback = func(ctx ssh.Context, c net.Conn) net.Conn {
		ip := hostOf(c.RemoteAddr())
		release, ok := conns.acquire(ip)
		if !ok {
			log.Warn("connection refused", "ip", ip, "reason", "limit")
			return nil
		}
		return &trackedConn{Conn: c, release: release}
	}
	// Explicit, even though these are the library defaults: no forwarding
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
			release, ok := l.acquire(hostOf(s.RemoteAddr()))
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
			log.Info("session start", "ip", ip, "term", pty.Term, "cols", pty.Window.Width, "rows", pty.Window.Height)
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

type trackedConn struct {
	net.Conn
	release func()
}

func (c *trackedConn) Close() error {
	c.release()
	return c.Conn.Close()
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
