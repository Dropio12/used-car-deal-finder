// Package store defines the storage the pipeline needs, as small interfaces.
//
// The pipeline depends on these, never on SQLite. sqlitestore is the one
// implementation; tests use in-memory fakes.
package store

import (
	"context"
	"time"

	"carbuyer/crawler/internal/listing"
)

// Scope says which query found a listing, so removal detection can later be
// limited to exactly what a crawl covered.
type Scope struct {
	MakeSlug  *string
	ModelSlug *string
	GeoSlug   *string
	SeenAt    string // ISO-8601 UTC, e.g. 2026-09-23T02:03:13.123Z
}

// SaveStats counts what a batch of writes changed.
type SaveStats struct {
	Seen, Added, Relisted, PriceDrops, PriceRises int
}

// Writer stores listings (insert or update, with price history).
type Writer interface {
	Save(ctx context.Context, listings []listing.Listing, scope Scope) (SaveStats, error)
}

// Filter selects stored listings. Empty strings mean "any".
type Filter struct {
	SellerType     string // "Dealer", "PrivateSeller" or "" for both
	Source         string
	Make, Model    string
	IncludeRemoved bool
}

// Stored is a listing as read back, with what only repeated crawls can know.
type Stored struct {
	listing.Listing
	FirstPrice   *float64 `json:"firstPrice"`
	PriceChanges int      `json:"priceChanges"`
	FirstSeen    string   `json:"firstSeen"`
	LastSeen     string   `json:"lastSeen"`
}

// Reader loads listings back out, in insertion order.
type Reader interface {
	LoadListings(ctx context.Context, f Filter) ([]Stored, error)
}

// RemovalScope is every dimension a crawl was scoped by. Leaving one out lets
// the update reach listings the crawl never looked at and call them sold.
type RemovalScope struct {
	Source     string
	MakeSlug   *string
	ModelSlug  *string
	GeoSlug    *string
	SellerType string // "P", "D" or ""
	Before, At string
}

// Remover marks listings that a complete crawl should have seen but did not.
type Remover interface {
	MarkRemoved(ctx context.Context, s RemovalScope) (int, error)
}

// CrawlStats is written to the crawls table when a crawl ends.
type CrawlStats struct {
	Shards, Seen, Added, PriceDrops, PriceRises, Removed, Truncated, Unpriced int
	Error                                                                     *string
}

// CrawlLog records crawl runs.
type CrawlLog interface {
	StartCrawl(ctx context.Context, scope any) (id int64, err error)
	FinishCrawl(ctx context.Context, id int64, stats CrawlStats) error
}

// Store is everything the pipeline uses, composed from the small interfaces.
type Store interface {
	Writer
	Reader
	Remover
	CrawlLog
}

// ISOTime formats t like JavaScript's Date.prototype.toISOString().
func ISOTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
