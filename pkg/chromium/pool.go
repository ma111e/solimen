package chromium

import (
	"context"
	"fmt"
	"sync/atomic"

	"downlink/pkg/models"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

// ChromiumPool manages N ChromiumScraper instances and dispatches Scrape
// calls round-robin across them.
type ChromiumPool struct {
	scrapers []*ChromiumScraper
	counter  atomic.Uint64
	sf       singleflight.Group
}

// NewChromiumPool creates a pool from a pre-built slice of scrapers.
// The scrapers must not yet be started; Start() will start them all.
func NewChromiumPool(scrapers []*ChromiumScraper) *ChromiumPool {
	return &ChromiumPool{scrapers: scrapers}
}

// Start starts all scrapers. If any scraper fails to start, all already-started
// scrapers are stopped and the error from the failing one is returned.
func (p *ChromiumPool) Start() error {
	for i, s := range p.scrapers {
		if err := s.Start(); err != nil {
			log.WithError(err).WithField("index", i).Error("chromium pool: instance failed to start, rolling back")
			for j := range i {
				p.scrapers[j].Stop()
			}
			return fmt.Errorf("chromium pool: instance %d failed to start: %w", i, err)
		}
		log.WithField("index", i).Info("chromium pool: instance started")
	}
	return nil
}

// Stop stops all scrapers.
func (p *ChromiumPool) Stop() {
	for i, s := range p.scrapers {
		s.Stop()
		log.WithField("index", i).Info("chromium pool: instance stopped")
	}
}

// Scrape dispatches the request to the next scraper in round-robin order.
// Concurrent calls for the same URL are deduplicated: only one scrape command
// is sent to Chromium and all callers share the result.
func (p *ChromiumPool) Scrape(ctx context.Context, rawURL string, triggers models.HostTriggers) (ScrapeResult, error) {
	v, err, _ := p.sf.Do(rawURL, func() (any, error) {
		idx := p.counter.Add(1) % uint64(len(p.scrapers))
		return p.scrapers[idx].Scrape(ctx, rawURL, triggers)
	})
	if err != nil {
		return ScrapeResult{}, err
	}
	return v.(ScrapeResult), nil
}

// Connected returns true if at least one scraper instance has an active
// WebSocket connection.
func (p *ChromiumPool) Connected() bool {
	for _, s := range p.scrapers {
		if s.Connected() {
			return true
		}
	}
	return false
}
