package content

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Store holds the current snapshot of a site and refreshes it in the
// background. Sessions only read from the store and never cause a fetch.
type Store struct {
	disc *Discoverer
	log  *slog.Logger

	mu   sync.RWMutex
	site *Site
	err  error
}

// NewStore returns a Store that refreshes with d.
func NewStore(d *Discoverer, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{disc: d, log: log}
}

// Site returns the current snapshot, or nil before the first successful
// refresh. The second value is the error from the last refresh, if any.
func (s *Store) Site() (*Site, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.site, s.err
}

// Refresh fetches the site once and replaces the snapshot on success. On
// failure the previous snapshot stays in place.
func (s *Store) Refresh(ctx context.Context) error {
	prev, _ := s.Site()
	start := time.Now()
	site, err := s.disc.Discover(ctx, prev, func(partial *Site) {
		s.mu.Lock()
		if s.site == nil {
			s.site = partial
		}
		s.mu.Unlock()
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
	if err != nil {
		s.log.Error("refresh failed", "url", s.disc.Base.String(), "err", err)
		return err
	}
	s.site = site
	s.log.Info("refreshed", "url", site.URL, "entries", len(site.Entries), "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// Run refreshes every interval until ctx is done. It refreshes once right
// away, and retries sooner while the store has no snapshot.
func (s *Store) Run(ctx context.Context, interval time.Duration) {
	for {
		_ = s.Refresh(ctx)
		wait := interval
		if site, _ := s.Site(); site == nil {
			wait = min(interval, 30*time.Second)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
