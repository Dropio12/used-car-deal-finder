package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A throwaway local server, never the real site.
func serve(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, hit int)) (string, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, int(atomic.AddInt32(&hits, 1)))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &hits
}

func noSleep(time.Duration) {}

func fetcher() *HTTPFetcher {
	f := NewHTTPFetcher()
	f.Sleep = noSleep
	return f
}

func TestFetch(t *testing.T) {
	cases := []struct {
		name     string
		handler  func(w http.ResponseWriter, hit int)
		wantBody string
		wantErr  int // expected HTTP status in the error, 0 = none
		wantHits int32
	}{
		{"ok", func(w http.ResponseWriter, _ int) { fmt.Fprint(w, "<html>hello</html>") }, "<html>hello</html>", 0, 1},
		{"500 then recovers", func(w http.ResponseWriter, hit int) {
			if hit < 3 {
				w.WriteHeader(500)
				return
			}
			fmt.Fprint(w, "recovered")
		}, "recovered", 0, 3},
		{"404 is not retried", func(w http.ResponseWriter, _ int) { w.WriteHeader(404) }, "", 404, 1},
		{"429 is retried then gives up", func(w http.ResponseWriter, _ int) { w.WriteHeader(429) }, "", 429, 4},
		{"503 gives up after retries", func(w http.ResponseWriter, _ int) { w.WriteHeader(503) }, "", 503, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url, hits := serve(t, func(w http.ResponseWriter, _ *http.Request, hit int) { c.handler(w, hit) })
			body, err := fetcher().Fetch(context.Background(), url)
			if c.wantErr == 0 {
				if err != nil || body != c.wantBody {
					t.Fatalf("got %q, %v", body, err)
				}
			} else {
				var h *HTTPError
				if !errors.As(err, &h) || h.Status != c.wantErr {
					t.Fatalf("err = %v, want HTTP %d", err, c.wantErr)
				}
				if !Answered(err) {
					t.Error("an HTTP status means the site answered")
				}
			}
			if got := atomic.LoadInt32(hits); got != c.wantHits {
				t.Errorf("hits = %d, want %d", got, c.wantHits)
			}
		})
	}
}

func TestFetchSendsBrowserHeaders(t *testing.T) {
	var ua, lang string
	url, _ := serve(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		ua, lang = r.Header.Get("User-Agent"), r.Header.Get("Accept-Language")
	})
	if _, err := fetcher().Fetch(context.Background(), url); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ua, "Mozilla/5.0") || !strings.HasPrefix(lang, "fr-CA") {
		t.Errorf("ua=%q lang=%q", ua, lang)
	}
}

func TestBackoffDoubles(t *testing.T) {
	url, _ := serve(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(500) })
	var waits []time.Duration
	f := NewHTTPFetcher()
	f.Sleep = func(d time.Duration) { waits = append(waits, d) }
	_, _ = f.Fetch(context.Background(), url)
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Errorf("waits = %v, want %v", waits, want)
	}
}

func TestAnswered(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&HTTPError{Status: 403}, true},
		{&ParseError{Msg: "login wall"}, true},
		{fmt.Errorf("wrapped: %w", &ParseError{Msg: "x"}), true},
		{errors.New("fetch failed"), false},
	}
	for _, c := range cases {
		if got := Answered(c.err); got != c.want {
			t.Errorf("Answered(%v) = %v", c.err, got)
		}
	}
}

type countingFetcher struct{ calls int }

func (c *countingFetcher) Fetch(context.Context, string) (string, error) {
	c.calls++
	return "ok", nil
}

func TestThrottleSpacesCalls(t *testing.T) {
	clock := time.Unix(0, 0)
	var slept []time.Duration
	th := NewThrottle(1500 * time.Millisecond)
	th.now = func() time.Time { return clock }
	th.Sleep = func(d time.Duration) { slept = append(slept, d); clock = clock.Add(d) }
	inner := &countingFetcher{}
	f := th.Wrap(inner)
	for i := 0; i < 3; i++ {
		if _, err := f.Fetch(context.Background(), "u"); err != nil {
			t.Fatal(err)
		}
	}
	if inner.calls != 3 {
		t.Fatalf("calls = %d", inner.calls)
	}
	// First call is immediate; each later one waits the full delay.
	if len(slept) != 2 || slept[0] != 1500*time.Millisecond {
		t.Errorf("slept = %v", slept)
	}
}
