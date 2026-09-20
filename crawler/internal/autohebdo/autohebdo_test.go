package autohebdo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/searchurl"
	"carbuyer/crawler/internal/source"
)

// fakeFetcher serves canned pages by URL and counts requests. Never the network.
type fakeFetcher struct {
	pages map[string][]string // url -> successive responses (last one repeats)
	calls map[string]int
}

func (f *fakeFetcher) Fetch(_ context.Context, url string) (string, error) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	bodies, ok := f.pages[url]
	if !ok {
		return "", fmt.Errorf("unexpected url %s", url)
	}
	i := f.calls[url]
	f.calls[url]++
	if i >= len(bodies) {
		i = len(bodies) - 1
	}
	return bodies[i], nil
}

// page builds a search page with n organic cars (ids prefix-0..n-1) and extra injected ones.
func page(prefix string, n, injected int, total, pages float64, cat any) string {
	var ls []any
	for i := 0; i < n; i++ {
		ls = append(ls, map[string]any{
			"id": fmt.Sprintf("%s-%d", prefix, i), "searchResultType": "Organic",
			"price":    map[string]any{"priceRaw": 20000 + i},
			"vehicle":  map[string]any{"make": "Toyota", "modelGroup": "RAV4", "modelYear": 2015 + i%8},
			"tracking": map[string]any{"mileage": fmt.Sprint(50000 + i)},
			"location": map[string]any{"provinceCode": "QC"},
			"seller":   map[string]any{"type": "Dealer"},
		})
	}
	for i := 0; i < injected; i++ {
		ls = append(ls, map[string]any{"id": fmt.Sprintf("inj-%s-%d", prefix, i), "searchResultType": "Deliverable"})
	}
	pq := map[string]any{}
	if cat != nil {
		pq["cat"] = cat
	}
	data := map[string]any{"props": map[string]any{"pageProps": map[string]any{
		"listings": ls, "numberOfResults": total, "numberOfPages": pages, "pageQuery": pq,
	}}}
	b, _ := json.Marshal(data)
	return `<script id="__NEXT_DATA__" type="application/json">` + string(b) + `</script>`
}

func urlFor(t *testing.T, q listing.Query, p int) string {
	t.Helper()
	u, err := searchurl.Build(urlOptions(q, p))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

var rav4 = listing.Query{Make: "toyota", Model: "rav4"}

func TestSearchFixtureOnePage(t *testing.T) {
	html, err := os.ReadFile(filepath.Join("..", "..", "testdata", "rav4-qc.html"))
	if err != nil {
		t.Fatal(err)
	}
	q := rav4
	q.MaxPages = 1
	f := &fakeFetcher{pages: map[string][]string{urlFor(t, q, 1): {string(html)}}}
	res, err := New(f).Search(context.Background(), q, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Listings) != 20 || res.PagesWalked != 1 {
		t.Errorf("listings=%d pages=%d", len(res.Listings), res.PagesWalked)
	}
	// 861 results exist; a 1-page walk is capped by the caller, not the site.
	if !res.Truncated || res.HitSiteCeiling {
		t.Errorf("truncated=%v ceiling=%v", res.Truncated, res.HitSiteCeiling)
	}
}

func TestSearchWalks(t *testing.T) {
	cases := []struct {
		name          string
		pages         map[int][]string
		wantListings  int
		wantShort     int
		wantShortfall int
		wantInjected  int
		wantTruncated bool
		wantCalls     map[int]int
	}{
		{
			name:         "complete walk of a 51-listing query",
			pages:        map[int][]string{1: {page("a", 20, 0, 51, 3, "ma70gr1")}, 2: {page("b", 20, 0, 51, 3, "ma70gr1")}, 3: {page("c", 11, 0, 51, 3, "ma70gr1")}},
			wantListings: 51, wantCalls: map[int]int{1: 1, 2: 1, 3: 1},
		},
		{
			name: "short page with injected ads is re-fetched once",
			pages: map[int][]string{
				1: {page("a", 16, 4, 51, 3, "ma70gr1"), page("a", 20, 0, 51, 3, "ma70gr1")},
				2: {page("b", 20, 0, 51, 3, "ma70gr1")}, 3: {page("c", 11, 0, 51, 3, "ma70gr1")},
			},
			wantListings: 51, wantShort: 1, wantCalls: map[int]int{1: 2, 2: 1, 3: 1},
		},
		{
			name:         "a car sold mid-walk is a shortfall, not truncation",
			pages:        map[int][]string{1: {page("a", 20, 0, 41, 3, "ma70gr1")}, 2: {page("b", 20, 0, 41, 3, "ma70gr1")}, 3: {page("c", 0, 2, 41, 3, "ma70gr1")}},
			wantListings: 40, wantShortfall: 1, wantInjected: 2, wantCalls: map[int]int{1: 1, 2: 1, 3: 1},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeFetcher{pages: map[string][]string{}}
			for p, bodies := range c.pages {
				f.pages[urlFor(t, rav4, p)] = bodies
			}
			var events int
			res, err := New(f).Search(context.Background(), rav4, func(source.PageEvent) { events++ })
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Listings) != c.wantListings || res.ShortPages != c.wantShort || res.Shortfall != c.wantShortfall ||
				res.Injected != c.wantInjected || res.Truncated != c.wantTruncated {
				t.Errorf("got listings=%d short=%d shortfall=%d injected=%d truncated=%v",
					len(res.Listings), res.ShortPages, res.Shortfall, res.Injected, res.Truncated)
			}
			for p, n := range c.wantCalls {
				if got := f.calls[urlFor(t, rav4, p)]; got != n {
					t.Errorf("page %d fetched %d times, want %d", p, got, n)
				}
			}
			if events != res.PagesWalked {
				t.Errorf("onPage called %d times for %d pages", events, res.PagesWalked)
			}
		})
	}
}

func TestYearFilterIsLocalAndDisablesShortfall(t *testing.T) {
	q := rav4
	from, to := 2016.0, 2017.0
	q.YearFrom, q.YearTo = &from, &to
	f := &fakeFetcher{pages: map[string][]string{
		urlFor(t, q, 1): {page("a", 20, 0, 25, 2, "ma70gr1")},
		urlFor(t, q, 2): {page("b", 5, 0, 25, 2, "ma70gr1")},
	}}
	res, err := New(f).Search(context.Background(), q, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range res.Listings {
		if *l.Year < 2016 || *l.Year > 2017 {
			t.Errorf("year %v escaped the local filter", *l.Year)
		}
	}
	if strings.Contains(urlFor(t, q, 1), "freg") {
		t.Error("the inert year filter must never be sent")
	}
	if res.Shortfall != 0 || res.Truncated || !res.LocallyFiltered {
		t.Errorf("shortfall=%d truncated=%v", res.Shortfall, res.Truncated)
	}
}

func TestVerifyQueryResolved(t *testing.T) {
	cases := []struct {
		name, mk, model string
		cat             any
		wantErr         string
	}{
		{"make+model", "toyota", "rav4", "ma70gr201439", ""},
		{"make only", "toyota", "", "ma70", ""},
		{"nothing asked", "", "", nil, ""},
		{"bad model", "toyota", "not-a-real-model", "ma70", `model "not-a-real-model" was not recognized`},
		{"bad make", "nonexistentmake", "", nil, `make "nonexistentmake" was not recognized — the site ignored it and searched everything (pageQuery.cat=undefined)`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := map[string]any{}
			if c.cat != nil {
				q["cat"] = c.cat
			}
			err := VerifyQueryResolved(c.mk, c.model, q)
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected %v", err)
				}
				return
			}
			var m *QueryMismatchError
			if !errors.As(err, &m) || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestSearchRejectsAWidenedQuery(t *testing.T) {
	f := &fakeFetcher{pages: map[string][]string{urlFor(t, rav4, 1): {page("a", 20, 0, 18096, 200, "ma70")}}}
	_, err := New(f).Search(context.Background(), rav4, nil)
	var m *QueryMismatchError
	if !errors.As(err, &m) {
		t.Fatalf("err = %v", err)
	}
}
