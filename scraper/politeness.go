package scraper

import (
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// crawlDelay parses the configured delay. An empty or invalid value falls back
// to the polite default.
func crawlDelay() time.Duration {
	raw := strings.TrimSpace(os.Getenv("SCRAPE_DELAY"))
	if raw == "" {
		log.Fatal("SCRAPE_DELAY is required. Set it in the .env file or as an environment variable.")
	}

	delay, err := time.ParseDuration(raw)
	if err != nil || delay < 0 {
		log.Fatal("SCRAPE_DELAY is invalid. Set it in the .env file or as an environment variable.")
	}
	return delay
}

// politeTransport spaces every outgoing request by at least delay. Mutexes are
// held while waiting and while recording the start time, so concurrent callers
// cannot start two requests in the same slot.
type politeTransport struct {
	base  http.RoundTripper
	delay time.Duration

	mu   sync.Mutex
	last time.Time
}

func (t *politeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.mu.Lock()
	if !t.last.IsZero() {
		wait := t.delay - time.Since(t.last)
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-request.Context().Done():
				timer.Stop()
				t.mu.Unlock()
				return nil, request.Context().Err()
			}
		}
	}

	t.last = time.Now()
	t.mu.Unlock()

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(request)
}

// sharedCrawlTransport is used by every scrape. Keeping one transport means the
// crawl delay also applies between two consecutive scrapes, not just between
// requests inside one scrape.
var sharedCrawlTransport = &politeTransport{base: http.DefaultTransport}

// newCrawlClient returns an HTTP client whose transport enforces the crawl
// delay between all requests. The timeout applies to the request itself, so the
// delay is added to it: otherwise a long configured crawl delay would make the
// client time out while waiting politely.
func newCrawlClient(timeout, delay time.Duration) *http.Client {
	if delay < 0 {
		delay = 0
	}
	if timeout > 0 {
		timeout += delay
	}

	sharedCrawlTransport.mu.Lock()
	sharedCrawlTransport.delay = delay
	sharedCrawlTransport.mu.Unlock()

	return &http.Client{
		Timeout:   timeout,
		Transport: sharedCrawlTransport,
	}
}
