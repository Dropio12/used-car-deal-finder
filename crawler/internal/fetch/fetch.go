// Package fetch is a polite HTTP client. Port of src/fetch.js.
//
// This is a nightly comps refresh, not a hot-path scraper: requests go out at
// human browsing speed, with a normal browser User-Agent.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// UserAgent is a current desktop Chrome string.
const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// DefaultDelay spaces out requests made through a Throttle.
const DefaultDelay = 1500 * time.Millisecond

// Fetcher gets a page body. The AutoHebdo source depends on this, not on HTTP,
// so tests (and an offline mode) can hand it a fixture instead of the network.
type Fetcher interface {
	Fetch(ctx context.Context, url string) (string, error)
}

// HTTPError means the server answered with a non-2xx status.
type HTTPError struct {
	Status int
	URL    string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d for %s", e.Status, e.URL) }

// ParseError marks "a body arrived that we could not read" (a login wall, a
// page that changed shape). Defined here so Answered can see it without the
// fetch package depending on the parser.
type ParseError struct{ Msg string }

func (e *ParseError) Error() string { return e.Msg }

// Answered reports whether the other end replied at all: an HTTP status or an
// unreadable body means the site is talking to us; anything else (DNS miss,
// dropped socket, timeout) is our side of the wire.
func Answered(err error) bool {
	if err == nil {
		return false
	}
	var h *HTTPError
	var p *ParseError
	return errors.As(err, &h) || errors.As(err, &p)
}

// HTTPFetcher fetches pages as a browser would, retrying only what is worth
// retrying: 5xx, 429 and network errors. Other 4xx are returned at once; a 404
// means the URL shape is wrong and hammering it will not change that.
type HTTPFetcher struct {
	Client  *http.Client
	Retries int           // extra attempts after the first (default 3)
	Timeout time.Duration // per attempt (default 30s)
	Headers map[string]string
	// Sleep waits between attempts; tests replace it to avoid real backoff.
	Sleep func(time.Duration)
}

// NewHTTPFetcher returns a fetcher with the JS defaults.
func NewHTTPFetcher() *HTTPFetcher {
	return &HTTPFetcher{Client: http.DefaultClient, Retries: 3, Timeout: 30 * time.Second, Sleep: time.Sleep}
}

// Fetch implements Fetcher.
func (f *HTTPFetcher) Fetch(ctx context.Context, url string) (string, error) {
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	sleep := f.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	var lastErr error
	for attempt := 0; attempt <= f.Retries; attempt++ {
		if attempt > 0 {
			sleep(time.Duration(1000*(1<<(attempt-1))) * time.Millisecond)
		}
		body, status, err := f.once(ctx, client, url, timeout)
		if err == nil && status >= 200 && status < 300 {
			return body, nil
		}
		if err == nil {
			httpErr := &HTTPError{Status: status, URL: url}
			if status >= 400 && status < 500 && status != 429 {
				return "", httpErr
			}
			lastErr = httpErr
			continue
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		lastErr = err
	}
	return "", lastErr
}

func (f *HTTPFetcher) once(ctx context.Context, client *http.Client, url string, timeout time.Duration) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept-Language", "fr-CA,fr;q=0.9,en-CA;q=0.8")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	for k, v := range f.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode, err
	}
	return string(body), resp.StatusCode, nil
}

// Throttle serializes requests and spaces them out: each one starts at least
// Delay after the previous one finished, whether it succeeded or not.
type Throttle struct {
	Delay time.Duration
	Sleep func(time.Duration)

	mu       sync.Mutex
	lastDone time.Time
	now      func() time.Time
}

// NewThrottle returns a throttle with the given spacing.
func NewThrottle(delay time.Duration) *Throttle {
	return &Throttle{Delay: delay, Sleep: time.Sleep, now: time.Now}
}

// Wrap returns a Fetcher whose calls go through the throttle.
func (t *Throttle) Wrap(f Fetcher) Fetcher { return throttled{t: t, f: f} }

type throttled struct {
	t *Throttle
	f Fetcher
}

func (w throttled) Fetch(ctx context.Context, url string) (string, error) {
	w.t.mu.Lock()
	defer w.t.mu.Unlock()
	now := w.t.now
	if now == nil {
		now = time.Now
	}
	if !w.t.lastDone.IsZero() {
		if wait := w.t.Delay - now().Sub(w.t.lastDone); wait > 0 {
			sleep := w.t.Sleep
			if sleep == nil {
				sleep = time.Sleep
			}
			sleep(wait)
		}
	}
	defer func() { w.t.lastDone = now() }()
	return w.f.Fetch(ctx, url)
}

// Static returns the same body for every URL. It backs the CLI's --offline
// mode, which replays a saved page instead of touching the network.
type Static struct{ Body string }

// Fetch implements Fetcher.
func (s Static) Fetch(context.Context, string) (string, error) { return s.Body, nil }
