// Package pipeline is the search -> store -> score flow behind the CLI.
//
// It depends only on interfaces (source.Source, store.Store, scoring.Scorer).
// The concrete AutoHebdo source, SQLite store and exec-based Rust scorer are
// built in cmd/carbuyer and passed in, so tests can use fakes and a new site
// only needs a new source.Source.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/scoring"
	"carbuyer/crawler/internal/searchurl"
	"carbuyer/crawler/internal/source"
	"carbuyer/crawler/internal/store"
)

// MaxShortfallRatio is how much of a scope can go unseen before "it wasn't in
// the results" stops meaning "it sold".
const MaxShortfallRatio = 0.02

// Coverage is the verdict on whether removal detection is safe.
type Coverage struct {
	OK     bool
	Ratio  float64
	Reason string
}

// ShouldDetectRemovals ports the JS rule: never after a truncated walk, and
// never when more than 2% of the scope went unseen.
func ShouldDetectRemovals(truncated, seen, shortfall int) Coverage {
	if truncated > 0 {
		return Coverage{Reason: "the crawl hit the page ceiling"}
	}
	observed := seen + shortfall
	ratio := 0.0
	if observed > 0 {
		ratio = float64(shortfall) / float64(observed)
	}
	if ratio > MaxShortfallRatio {
		return Coverage{Ratio: ratio, Reason: fmt.Sprintf("%.1f%% of the scope went unseen", ratio*100)}
	}
	return Coverage{OK: true, Ratio: ratio}
}

// Pipeline wires a source, a store and a scorer together.
type Pipeline struct {
	Source source.Source
	Store  store.Store
	Scorer scoring.Scorer
	Now    func() time.Time
}

// New builds a pipeline from its three collaborators.
func New(src source.Source, st store.Store, sc scoring.Scorer) *Pipeline {
	return &Pipeline{Source: src, Store: st, Scorer: sc, Now: time.Now}
}

// Deal is a found listing with its score (nil when it could not be priced).
type Deal struct {
	Listing listing.Listing `json:"listing"`
	Score   *scoring.Score  `json:"score"`
}

// Report is everything one run produced.
type Report struct {
	Search         source.Result   `json:"search"`
	Saved          store.SaveStats `json:"saved"`
	Removed        int             `json:"removed"`
	RemovalSkipped string          `json:"removalSkipped,omitempty"`
	// Deals are the listings this search found, best discount first; unscored last.
	Deals     []Deal          `json:"deals"`
	Scored    int             `json:"scored"`
	Comps     int             `json:"comps"`
	Appraiser json.RawMessage `json:"appraiser"`
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Run searches, stores what it found, and scores it against every active
// listing from the same source.
func (p *Pipeline) Run(ctx context.Context, q listing.Query, onPage func(source.PageEvent)) (Report, error) {
	now := p.Now
	if now == nil {
		now = time.Now
	}
	geo := q.Geo
	if geo == "" {
		geo = searchurl.GeoQuebec
	}
	scope := store.Scope{MakeSlug: strPtr(q.Make), ModelSlug: strPtr(q.Model), GeoSlug: &geo, SeenAt: store.ISOTime(now())}

	crawlID, err := p.Store.StartCrawl(ctx, map[string]any{
		"source": p.Source.Name(), "make": q.Make, "model": q.Model, "geo": geo, "sellerType": q.SellerType,
	})
	if err != nil {
		return Report{}, err
	}
	var stats store.CrawlStats
	fail := func(err error) (Report, error) {
		msg := err.Error()
		stats.Error = &msg
		_ = p.Store.FinishCrawl(ctx, crawlID, stats)
		return Report{}, err
	}

	res, err := p.Source.Search(ctx, q, onPage)
	if err != nil {
		return fail(err)
	}
	saved, err := p.Store.Save(ctx, res.Listings, scope)
	if err != nil {
		return fail(err)
	}
	report := Report{Search: res, Saved: saved}
	stats = store.CrawlStats{Shards: 1, Seen: saved.Seen, Added: saved.Added, PriceDrops: saved.PriceDrops, PriceRises: saved.PriceRises}
	if res.Truncated {
		stats.Truncated = 1
	}

	// Removal detection only when this walk covered its whole scope. A year or
	// price filter narrows the scope beyond what the stored slugs describe, so
	// a missing car would not be proof of a sale.
	switch {
	case res.LocallyFiltered || q.PriceFrom != nil || q.PriceTo != nil:
		report.RemovalSkipped = "a year or price filter was applied, so the walk did not cover the whole scope"
	default:
		cov := ShouldDetectRemovals(stats.Truncated, saved.Seen, res.Shortfall)
		if !cov.OK {
			report.RemovalSkipped = cov.Reason
			break
		}
		n, err := p.Store.MarkRemoved(ctx, store.RemovalScope{
			Source: p.Source.Name(), MakeSlug: scope.MakeSlug, ModelSlug: scope.ModelSlug, GeoSlug: scope.GeoSlug,
			SellerType: q.SellerType, Before: scope.SeenAt, At: scope.SeenAt,
		})
		if err != nil {
			return fail(err)
		}
		report.Removed, stats.Removed = n, n
	}
	if err := p.Store.FinishCrawl(ctx, crawlID, stats); err != nil {
		return Report{}, err
	}

	// Comps: every active listing from this source, dealers and private sellers.
	stored, err := p.Store.LoadListings(ctx, store.Filter{Source: p.Source.Name()})
	if err != nil {
		return Report{}, err
	}
	comps := make([]listing.Listing, len(stored))
	for i, s := range stored {
		comps[i] = s.Listing
	}
	report.Comps = len(comps)
	scored, err := p.Scorer.Score(ctx, comps)
	if err != nil {
		return Report{}, err
	}
	report.Appraiser = scored.Appraiser

	for _, l := range res.Listings {
		d := Deal{Listing: l, Score: scored.Scores[l.Key()]}
		if d.Score != nil {
			report.Scored++
		}
		report.Deals = append(report.Deals, d)
	}
	sort.SliceStable(report.Deals, func(i, j int) bool {
		a, b := report.Deals[i].Score, report.Deals[j].Score
		if (a == nil) != (b == nil) {
			return a != nil
		}
		return a != nil && a.DiscountPct > b.DiscountPct
	})
	return report, nil
}
