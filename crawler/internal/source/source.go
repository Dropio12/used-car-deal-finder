// Package source defines what the pipeline needs from a listings site.
//
// The pipeline depends on this interface only. AutoHebdo is the one
// implementation today; Kijiji, LesPAC or Marketplace can be added as new
// implementations without touching the pipeline.
package source

import (
	"context"

	"carbuyer/crawler/internal/listing"
)

// Result is one walked query.
type Result struct {
	URL         string            `json:"url"`
	Total       *float64          `json:"total"`
	Pages       *float64          `json:"pages"`
	PagesWalked int               `json:"pagesWalked"`
	Listings    []listing.Listing `json:"listings"`
	Injected    int               `json:"injected"`
	ShortPages  int               `json:"shortPages"`
	// Shortfall: walked every page and still came up a few light (ordinary churn).
	Shortfall      int     `json:"shortfall"`
	ShortfallRatio float64 `json:"shortfallRatio"`
	// Truncated: the walk stopped before the end, so coverage is unknown.
	Truncated bool `json:"truncated"`
	// HitSiteCeiling: the *site* refused to paginate further (not the caller's cap).
	HitSiteCeiling bool `json:"hitSiteCeiling"`
	// LocallyFiltered is true when year bounds were applied client-side, so
	// the result no longer describes the whole server-side scope.
	LocallyFiltered bool `json:"locallyFiltered"`
}

// PageEvent is reported after each page, for progress output.
type PageEvent struct {
	Page, Of int
	Found    int
	Total    *float64
}

// Source is a listings site the pipeline can search.
type Source interface {
	// Name is the value stored in listings.source ("autohebdo").
	Name() string
	// Search walks up to q.MaxPages pages of a query and returns what it found.
	Search(ctx context.Context, q listing.Query, onPage func(PageEvent)) (Result, error)
}
