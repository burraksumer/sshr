// Command sshr serves a website's content as a terminal reader over SSH.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/burraksumer/sshr/internal/config"
	"github.com/burraksumer/sshr/internal/content"
	"github.com/burraksumer/sshr/internal/fetch"
	"github.com/burraksumer/sshr/internal/sanitize"
	"github.com/burraksumer/sshr/internal/server"
	"github.com/burraksumer/sshr/internal/ui"
)

var version = "dev"

const usage = `sshr serves a website's content as a terminal reader over SSH.

Usage:
  sshr serve [--config sshr.toml] [--url URL] [--listen :2222]
  sshr read URL     read a site in this terminal
  sshr check URL    list what sshr finds on a site
  sshr version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = serve(ctx, args)
	case "read":
		err = read(ctx, args)
	case "check":
		err = check(ctx, args)
	case "version", "--version", "-v":
		fmt.Println("sshr", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sshr:", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fs.String("config", envOr("SSHR_CONFIG", "sshr.toml"), "config file")
	siteURL := fs.String("url", "", "site to serve (overrides config)")
	listen := fs.String("listen", "", "address to listen on (overrides config)")
	_ = fs.Parse(args)

	// A config file named with --config or SSHR_CONFIG must exist. The
	// default one is optional.
	explicit := os.Getenv("SSHR_CONFIG") != ""
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "config" })
	cfg, err := config.Load(*path, explicit)
	if err != nil {
		return err
	}
	if *siteURL != "" {
		cfg.URL = *siteURL
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	log := newLogger(cfg.LogFormat)

	store, err := newStore(cfg, log)
	if err != nil {
		return err
	}
	go store.Run(ctx, cfg.Refresh.D())

	srv, err := server.New(server.Config{
		Listen:         cfg.Listen,
		HostKeyPath:    cfg.HostKey,
		MaxSessions:    cfg.MaxSessions,
		MaxPerIP:       cfg.MaxPerIP,
		IdleTimeout:    cfg.IdleTimeout.D(),
		MaxTimeout:     cfg.MaxSession.D(),
		ProxyProtocol:  cfg.ProxyProtocol,
		TrustedProxies: cfg.TrustedProxies,
	}, store, log)
	if err != nil {
		return err
	}
	return srv.ListenAndServe(ctx)
}

func read(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: sshr read URL")
	}
	cfg := config.Default()
	cfg.URL = args[0]
	store, err := newStore(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		return err
	}
	// The reader shows a loading screen and picks up posts as soon as the
	// first ones are ready, so the refresh runs alongside it.
	go store.Run(ctx, cfg.Refresh.D())
	_, err = tea.NewProgram(ui.New(store), tea.WithContext(ctx)).Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func check(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: sshr check URL")
	}
	cfg := config.Default()
	cfg.URL = args[0]
	store, err := newStore(cfg, newLogger("text"))
	if err != nil {
		return err
	}
	start := time.Now()
	if err := store.Refresh(ctx); err != nil {
		return err
	}
	site, _ := store.Site()
	fmt.Printf("%s\n%s\n%d entries in %s\n\n", site.Title, site.Description, len(site.Entries), time.Since(start).Round(time.Millisecond))
	for _, e := range site.Entries {
		kind := "post"
		if e.Kind == content.Page {
			kind = "page"
		}
		date := "          "
		if !e.Date.IsZero() {
			date = e.Date.Format("2006-01-02")
		}
		fmt.Printf("%s  %s  %6d B  %s\n      %s\n", kind, date, len(e.Body), e.Title, e.URL)
	}
	return nil
}

func newStore(cfg config.Config, log *slog.Logger) (*content.Store, error) {
	base, err := cfg.SiteURL()
	if err != nil {
		return nil, err
	}
	client := fetch.New(fetch.Options{Hosts: cfg.Hosts(base)})
	disc := &content.Discoverer{Client: client, Base: base, MaxEntries: cfg.MaxEntries, Log: log}
	return content.NewStore(disc, log), nil
}

func newLogger(format string) *slog.Logger {
	// Log values can carry text from the fetched site, like URLs. The JSON
	// handler escapes C0 controls but not C1 controls or bidi overrides.
	opts := &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		switch v := a.Value.Any().(type) {
		case string:
			return slog.String(a.Key, sanitize.Text(v))
		case error:
			return slog.String(a.Key, sanitize.Text(v.Error()))
		}
		return a
	}}
	if os.Getenv("SSHR_DEBUG") != "" {
		opts.Level = slog.LevelDebug
	}
	if format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
