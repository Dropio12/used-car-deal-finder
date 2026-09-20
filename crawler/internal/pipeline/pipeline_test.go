package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/scoring"
	"carbuyer/crawler/internal/source"
	"carbuyer/crawler/internal/store"
)

// --- fakes: the pipeline only ever sees the interfaces ---

type fakeSource struct {
	result source.Result
	err    error
	got    listing.Query
}

func (f *fakeSource) Name() string { return "autohebdo" }
func (f *fakeSource) Search(_ context.Context, q listing.Query, onPage func(source.PageEvent)) (source.Result, error) {
	f.got = q
	if onPage != nil {
		onPage(source.PageEvent{Page: 1, Of: 1, Found: len(f.result.Listings)})
	}
	return f.result, f.err
}

type fakeStore struct {
	rows      []store.Stored
	saved     []listing.Listing
	scope     store.Scope
	removals  []store.RemovalScope
	finished  []store.CrawlStats
	loadedFor store.Filter
}

func (s *fakeStore) Save(_ context.Context, ls []listing.Listing, sc store.Scope) (store.SaveStats, error) {
	s.saved, s.scope = ls, sc
	for _, l := range ls {
		s.rows = append(s.rows, store.Stored{Listing: l})
	}
	return store.SaveStats{Seen: len(ls), Added: len(ls)}, nil
}
func (s *fakeStore) LoadListings(_ context.Context, f store.Filter) ([]store.Stored, error) {
	s.loadedFor = f
	return s.rows, nil
}
func (s *fakeStore) MarkRemoved(_ context.Context, r store.RemovalScope) (int, error) {
	s.removals = append(s.removals, r)
	return 2, nil
}
func (s *fakeStore) StartCrawl(context.Context, any) (int64, error) { return 7, nil }
func (s *fakeStore) FinishCrawl(_ context.Context, _ int64, st store.CrawlStats) error {
	s.finished = append(s.finished, st)
	return nil
}

type fakeScorer struct {
	scores map[string]*scoring.Score
	got    []listing.Listing
}

func (f *fakeScorer) Score(_ context.Context, ls []listing.Listing) (scoring.Result, error) {
	f.got = ls
	return scoring.Result{Scores: f.scores}, nil
}

func car(id string) listing.Listing { return listing.Listing{ID: listing.Str(id), Source: "autohebdo"} }

func total(n float64) *float64 { return &n }

func TestRun(t *testing.T) {
	found := []listing.Listing{car("a"), car("b"), car("c")}
	cases := []struct {
		name        string
		result      source.Result
		query       listing.Query
		wantRemoval bool
		wantSkip    string
	}{
		{"complete walk detects removals", source.Result{Listings: found, Total: total(3)}, listing.Query{Make: "toyota"}, true, ""},
		{"truncated walk does not", source.Result{Listings: found, Total: total(900), Truncated: true}, listing.Query{Make: "toyota"}, false, "the crawl hit the page ceiling"},
		{"big shortfall does not", source.Result{Listings: found, Total: total(10), Shortfall: 7}, listing.Query{}, false, "70.0% of the scope went unseen"},
		{"price filter does not", source.Result{Listings: found, Total: total(3)}, listing.Query{PriceTo: total(9000)}, false, "a year or price filter was applied, so the walk did not cover the whole scope"},
		{"year filter does not", source.Result{Listings: found, Total: total(3), LocallyFiltered: true}, listing.Query{}, false, "a year or price filter was applied, so the walk did not cover the whole scope"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := &fakeSource{result: c.result}
			st := &fakeStore{rows: []store.Stored{{Listing: car("old-comp")}}}
			sc := &fakeScorer{scores: map[string]*scoring.Score{
				"a": {DiscountPct: 5}, "b": nil, "c": {DiscountPct: 25}, "old-comp": {DiscountPct: 99},
			}}
			p := New(src, st, sc)
			p.Now = func() time.Time { return time.Date(2026, 9, 23, 1, 2, 3, 4_000_000, time.UTC) }

			pages := 0
			rep, err := p.Run(context.Background(), c.query, func(source.PageEvent) { pages++ })
			if err != nil {
				t.Fatal(err)
			}
			if pages != 1 || len(st.saved) != 3 || st.scope.SeenAt != "2026-09-23T01:02:03.004Z" || *st.scope.GeoSlug != "reg_qc" {
				t.Errorf("pages=%d saved=%d scope=%+v", pages, len(st.saved), st.scope)
			}
			if (len(st.removals) == 1) != c.wantRemoval || rep.RemovalSkipped != c.wantSkip {
				t.Errorf("removals=%v skipped=%q", st.removals, rep.RemovalSkipped)
			}
			if st.loadedFor.Source != "autohebdo" || st.loadedFor.SellerType != "" || len(sc.got) != 4 {
				t.Errorf("comps: filter=%+v scored=%d", st.loadedFor, len(sc.got))
			}
			// Only this search's cars are reported, best discount first, unscored last.
			if len(rep.Deals) != 3 || rep.Deals[0].Listing.Key() != "c" || rep.Deals[1].Listing.Key() != "a" ||
				rep.Deals[2].Score != nil || rep.Scored != 2 {
				t.Errorf("deals = %+v", rep.Deals)
			}
			if len(st.finished) != 1 || st.finished[0].Seen != 3 || st.finished[0].Error != nil {
				t.Errorf("finished = %+v", st.finished)
			}
		})
	}
}

func TestRunRecordsSourceErrors(t *testing.T) {
	st := &fakeStore{}
	p := New(&fakeSource{err: errors.New("HTTP 503")}, st, &fakeScorer{})
	if _, err := p.Run(context.Background(), listing.Query{}, nil); err == nil {
		t.Fatal("want error")
	}
	if len(st.finished) != 1 || st.finished[0].Error == nil || *st.finished[0].Error != "HTTP 503" {
		t.Errorf("crawl not closed with the error: %+v", st.finished)
	}
}

func TestShouldDetectRemovals(t *testing.T) {
	cases := []struct {
		truncated, seen, shortfall int
		ok                         bool
	}{
		{0, 2935, 1, true}, {1, 4000, 0, false}, {0, 90, 10, false}, {0, 0, 0, true},
	}
	for _, c := range cases {
		if got := ShouldDetectRemovals(c.truncated, c.seen, c.shortfall); got.OK != c.ok {
			t.Errorf("%+v: %+v", c, got)
		}
	}
}
