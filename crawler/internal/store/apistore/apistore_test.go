package apistore

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/store"
)

const token = "t0ken-for-tests"

// fakeWorker mimics POST /api/listings: checks the bearer token and answers
// with stats computed from the body.
type fakeWorker struct {
	mu     sync.Mutex
	bodies []wireBody
	status int // non-zero forces this status
}

func (f *fakeWorker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != IngestPath {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+token {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"missing or wrong bearer token"}`)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		io.WriteString(w, "<!DOCTYPE html><html>boom</html>")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		http.Error(w, "content-type "+ct, http.StatusBadRequest)
		return
	}
	var b wireBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.bodies = append(f.bodies, b)
	f.mu.Unlock()
	json.NewEncoder(w).Encode(Stats{Seen: len(b.Listings), Added: len(b.Listings), PriceDrops: 1, NewAlerts: 1})
}

func listings(n int) []listing.Listing {
	out := make([]listing.Listing, n)
	for i := range out {
		out[i] = listing.Listing{ID: listing.Str(string(rune('a' + i))), Source: "autohebdo", Price: listing.Num(20000 + float64(i))}
	}
	return out
}

func TestPushesInBatchesWithTokenAndScope(t *testing.T) {
	fw := &fakeWorker{}
	srv := httptest.NewServer(fw)
	defer srv.Close()

	c, err := New(srv.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	c.BatchSize = 2
	scope := store.Scope{MakeSlug: listing.Str("toyota"), ModelSlug: listing.Str("rav4"), GeoSlug: listing.Str("reg_qc"), SeenAt: "2026-09-23T00:00:00.000Z"}
	stats, err := c.Save(context.Background(), listings(5), scope)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Seen != 5 || stats.Added != 5 || stats.PriceDrops != 3 {
		t.Errorf("stats = %+v", stats)
	}
	if len(fw.bodies) != 3 || len(fw.bodies[2].Listings) != 1 {
		t.Fatalf("batches = %d", len(fw.bodies))
	}
	b := fw.bodies[0]
	if *b.Scope.MakeSlug != "toyota" || *b.Scope.GeoSlug != "reg_qc" || b.Scope.SeenAt != scope.SeenAt {
		t.Errorf("scope = %+v", b.Scope)
	}
	if *b.Listings[1].ID != "b" || *b.Listings[1].Price != 20001 {
		t.Errorf("listing = %+v", b.Listings[1])
	}
	// Totals accumulate across Save calls.
	c.Save(context.Background(), listings(1), scope)
	if c.Totals.Seen != 6 || c.Totals.NewAlerts != c.Totals.PriceDrops || c.Totals.NewAlerts == 0 {
		t.Errorf("totals = %+v", c.Totals)
	}
}

func TestErrorsNeverLeakTheToken(t *testing.T) {
	fw := &fakeWorker{}
	srv := httptest.NewServer(fw)
	defer srv.Close()

	wrong, _ := New(srv.URL, "not-the-token")
	_, err := wrong.Save(context.Background(), listings(1), store.Scope{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "wrong bearer token") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "not-the-token") {
		t.Errorf("error leaks the token: %v", err)
	}

	fw.status = http.StatusForbidden
	c, _ := New(srv.URL, token)
	c.BatchSize = 1
	stats, err := c.Save(context.Background(), listings(3), store.Scope{})
	if err == nil || !strings.Contains(err.Error(), "Cloudflare Access") || !strings.Contains(err.Error(), "listings 1-1 of 3") {
		t.Errorf("err = %v", err)
	}
	if stats.Seen != 0 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestEmptySaveSendsNothing(t *testing.T) {
	fw := &fakeWorker{}
	srv := httptest.NewServer(fw)
	defer srv.Close()
	c, _ := New(srv.URL, token)
	if _, err := c.Save(context.Background(), nil, store.Scope{}); err != nil || len(fw.bodies) != 0 {
		t.Errorf("err=%v bodies=%d", err, len(fw.bodies))
	}
}

func TestCancelledContext(t *testing.T) {
	srv := httptest.NewServer(&fakeWorker{})
	defer srv.Close()
	c, _ := New(srv.URL, token)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Save(ctx, listings(1), store.Scope{}); err == nil {
		t.Error("want an error for a cancelled context")
	}
}

func TestNewValidates(t *testing.T) {
	cases := []struct {
		url, token, endpoint string
		ok                   bool
	}{
		{"https://carbuyer-api.me.workers.dev", "t", "https://carbuyer-api.me.workers.dev/api/listings", true},
		{"https://carbuyer-api.me.workers.dev/", "t", "https://carbuyer-api.me.workers.dev/api/listings", true},
		{"https://x.dev/api/listings?a=1", "t", "https://x.dev/api/listings", true},
		{"http://127.0.0.1:8787", "t", "http://127.0.0.1:8787/api/listings", true},
		{"http://localhost:8787", "t", "http://localhost:8787/api/listings", true},
		{"http://example.com", "t", "", false}, // token in clear text
		{"ftp://example.com", "t", "", false},
		{"not a url", "t", "", false},
		{"https://x.dev", "  ", "", false},
	}
	for _, c := range cases {
		cl, err := New(c.url, c.token)
		if (err == nil) != c.ok {
			t.Errorf("New(%q) err = %v", c.url, err)
			continue
		}
		if c.ok && cl.Endpoint != c.endpoint {
			t.Errorf("New(%q).Endpoint = %q, want %q", c.url, cl.Endpoint, c.endpoint)
		}
	}
}
