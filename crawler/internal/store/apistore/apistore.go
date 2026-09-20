// Package apistore is a store.Writer that pushes listings to the carbuyer
// Cloudflare Worker (POST /api/listings, see cloudflare/worker). The Worker
// upserts them into D1 with the same rules as sqlitestore.
//
// It is a write-only sink: the CLI combines it with the local SQLite store
// through store.Tee, so the pipeline itself does not change.
package apistore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/store"
)

// IngestPath is the Worker route that accepts listings.
const IngestPath = "/api/listings"

// DefaultBatchSize keeps each request (and the D1 batch it becomes) small.
const DefaultBatchSize = 50

// Stats is what the Worker reports, summed over every request.
type Stats struct {
	Seen       int `json:"seen"`
	Added      int `json:"added"`
	Relisted   int `json:"relisted"`
	PriceDrops int `json:"priceDrops"`
	PriceRises int `json:"priceRises"`
	// Listings the Worker refused because they had no id.
	Skipped int `json:"skipped"`
	// Strong new deals the Worker recorded in new_deal_alerts.
	NewAlerts int `json:"newAlerts"`
	// Set when the Worker stored the listings but could not raise alerts.
	AlertError string `json:"alertError,omitempty"`
}

// Client pushes listings to the Worker API.
type Client struct {
	Endpoint  string // full URL of POST /api/listings
	Token     string // bearer token (the Worker's INGEST_TOKEN secret)
	HTTP      *http.Client
	BatchSize int
	// Totals accumulates the Worker's answers across Save calls.
	Totals Stats
}

var _ store.Writer = (*Client)(nil)

// New checks the API URL and token. apiURL may be the Worker's base URL
// (https://carbuyer-api.example.workers.dev) or the full ingest URL. Plain
// http is only accepted for localhost, so the token never crosses the
// network in clear text.
func New(apiURL, token string) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("no ingest token: set CARBUYER_INGEST_TOKEN")
	}
	u, err := url.Parse(strings.TrimSpace(apiURL))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("--push: %q is not an http(s) URL", apiURL)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("--push: refusing plain http to %s (the token would travel unencrypted); use https", u.Host)
	}
	if !strings.HasSuffix(strings.TrimRight(u.Path, "/"), IngestPath) {
		u.Path = strings.TrimRight(u.Path, "/") + IngestPath
	}
	u.RawQuery, u.Fragment = "", ""
	return &Client{
		Endpoint:  u.String(),
		Token:     strings.TrimSpace(token),
		HTTP:      &http.Client{Timeout: 60 * time.Second},
		BatchSize: DefaultBatchSize,
	}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type wireScope struct {
	MakeSlug  *string `json:"makeSlug"`
	ModelSlug *string `json:"modelSlug"`
	GeoSlug   *string `json:"geoSlug"`
	SeenAt    string  `json:"seenAt,omitempty"`
}

type wireBody struct {
	Listings []listing.Listing `json:"listings"`
	Scope    wireScope         `json:"scope"`
}

// Save implements store.Writer: it posts the listings in batches and returns
// what the Worker changed. It stops at the first failed batch.
func (c *Client) Save(ctx context.Context, listings []listing.Listing, scope store.Scope) (store.SaveStats, error) {
	size := c.BatchSize
	if size <= 0 {
		size = DefaultBatchSize
	}
	var total Stats
	for start := 0; start < len(listings); start += size {
		end := min(start+size, len(listings))
		s, err := c.post(ctx, wireBody{
			Listings: listings[start:end],
			Scope:    wireScope{MakeSlug: scope.MakeSlug, ModelSlug: scope.ModelSlug, GeoSlug: scope.GeoSlug, SeenAt: scope.SeenAt},
		})
		if err != nil {
			return toSaveStats(total), fmt.Errorf("push listings %d-%d of %d: %w", start+1, end, len(listings), err)
		}
		total = add(total, s)
		c.Totals = add(c.Totals, s)
	}
	return toSaveStats(total), nil
}

func (c *Client) post(ctx context.Context, body wireBody) (Stats, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return Stats{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return Stats{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("User-Agent", "carbuyer-crawler")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Stats{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		// Cloudflare Access answers an unauthenticated request with a redirect
		// to its login page, which the client follows to an HTML page.
		if strings.Contains(msg, "<html") || strings.Contains(msg, "<!DOCTYPE") {
			msg = "got an HTML page, not the API (is Cloudflare Access blocking " + IngestPath + "?)"
		}
		return Stats{}, fmt.Errorf("%s: HTTP %d: %s", c.Endpoint, resp.StatusCode, msg)
	}
	var s Stats
	if err := json.Unmarshal(raw, &s); err != nil {
		return Stats{}, fmt.Errorf("%s: answer is not the expected JSON: %w", c.Endpoint, err)
	}
	return s, nil
}

func add(a, b Stats) Stats {
	return Stats{
		Seen: a.Seen + b.Seen, Added: a.Added + b.Added, Relisted: a.Relisted + b.Relisted,
		PriceDrops: a.PriceDrops + b.PriceDrops, PriceRises: a.PriceRises + b.PriceRises, Skipped: a.Skipped + b.Skipped,
		NewAlerts: a.NewAlerts + b.NewAlerts, AlertError: firstNonEmpty(a.AlertError, b.AlertError),
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func toSaveStats(s Stats) store.SaveStats {
	return store.SaveStats{Seen: s.Seen, Added: s.Added, Relisted: s.Relisted, PriceDrops: s.PriceDrops, PriceRises: s.PriceRises}
}
