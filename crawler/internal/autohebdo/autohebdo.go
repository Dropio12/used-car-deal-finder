// Package autohebdo is the AutoHebdo implementation of source.Source.
// Port of the search / searchAll / verifyQueryResolved part of src/index.js.
//
// It depends on a fetch.Fetcher, not on HTTP, so the same code runs against
// the live site, a throttled client, or an offline fixture.
package autohebdo

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"carbuyer/crawler/internal/fetch"
	"carbuyer/crawler/internal/jstext"
	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/parse"
	"carbuyer/crawler/internal/searchurl"
	"carbuyer/crawler/internal/source"
)

const (
	// PageSize is listings per page.
	PageSize = 20
	// MaxPages is the hard server-side cap: page 201+ is unreachable.
	MaxPages = 200
)

// Source searches autohebdo.net.
type Source struct {
	fetcher fetch.Fetcher
	// RetryShortPages re-fetches a page that came back short once (injected
	// ads sometimes displace real listings). On by default.
	RetryShortPages bool
}

// New returns an AutoHebdo source that fetches through f.
func New(f fetch.Fetcher) *Source { return &Source{fetcher: f, RetryShortPages: true} }

var _ source.Source = (*Source)(nil)

// Name implements source.Source.
func (s *Source) Name() string { return "autohebdo" }

// QueryMismatchError means the site silently dropped part of the query.
type QueryMismatchError struct{ Msg string }

func (e *QueryMismatchError) Error() string { return e.Msg }

var (
	makeResolved  = regexp.MustCompile(`^ma\d+`)
	modelResolved = regexp.MustCompile(`^ma\d+gr\d+`)
)

func catString(v any) (text, display string) {
	switch c := v.(type) {
	case nil:
		return "", "undefined"
	case string:
		b, _ := json.Marshal(c)
		return c, string(b)
	case float64:
		return jstext.Number(c), jstext.Number(c)
	default:
		b, _ := json.Marshal(c)
		return fmt.Sprint(c), string(b)
	}
}

// VerifyQueryResolved confirms the site applied the make/model we asked for.
// A misspelled slug does not 404: /autos/toyota/not-a-real-model returns every
// Toyota, and a bad make returns the whole country. pageQuery.cat says what
// was resolved: "ma70gr201439" (make+model), "ma70" (make only), or nothing.
func VerifyQueryResolved(mk, model string, query map[string]any) error {
	cat, shown := catString(query["cat"])
	if mk != "" && !makeResolved.MatchString(cat) {
		return &QueryMismatchError{fmt.Sprintf(
			"make %q was not recognized — the site ignored it and searched everything (pageQuery.cat=%s)", mk, shown)}
	}
	if model != "" && !modelResolved.MatchString(cat) {
		return &QueryMismatchError{fmt.Sprintf(
			"model %q was not recognized — the site ignored it and returned all %s models (pageQuery.cat=%s)", model, mk, shown)}
	}
	return nil
}

// applyLocalFilters does year filtering here; the site's own year filter is inert.
func applyLocalFilters(listings []listing.Listing, q listing.Query) []listing.Listing {
	if q.YearFrom == nil && q.YearTo == nil {
		return listings
	}
	out := []listing.Listing{}
	for _, l := range listings {
		if l.Year == nil {
			continue
		}
		if q.YearFrom != nil && *l.Year < *q.YearFrom {
			continue
		}
		if q.YearTo != nil && *l.Year > *q.YearTo {
			continue
		}
		out = append(out, l)
	}
	return out
}

// PageResult is one fetched, parsed and locally filtered page.
type PageResult struct {
	URL           string
	Total, Pages  *float64
	Listings      []listing.Listing
	InjectedCount int
	LocalFiltered int
}

func urlOptions(q listing.Query, page int) searchurl.Options {
	return searchurl.Options{
		Make: q.Make, Model: q.Model, Geo: q.Geo, SellerType: q.SellerType, Page: page,
		Sort: q.Sort, Descending: q.Descending, PriceFrom: q.PriceFrom, PriceTo: q.PriceTo,
	}
}

// SearchPage fetches and parses one page of results (JS `search()`).
func (s *Source) SearchPage(ctx context.Context, q listing.Query, page int) (PageResult, error) {
	url, err := searchurl.Build(urlOptions(q, page))
	if err != nil {
		return PageResult{}, err
	}
	html, err := s.fetcher.Fetch(ctx, url)
	if err != nil {
		return PageResult{}, err
	}
	p, err := parse.SearchPage(html)
	if err != nil {
		return PageResult{}, err
	}
	if err := VerifyQueryResolved(q.Make, q.Model, p.Query); err != nil {
		return PageResult{}, err
	}
	filtered := applyLocalFilters(p.Listings, q)
	return PageResult{
		URL: url, Total: p.Total, Pages: p.Pages, Listings: filtered,
		InjectedCount: p.InjectedCount, LocalFiltered: len(p.Listings) - len(filtered),
	}, nil
}

// Search walks every page of a query (JS `searchAll()`), guarding against the
// 4000-result ceiling (reported as Truncated) and against injected ads
// displacing real listings (short pages are re-fetched once).
func (s *Source) Search(ctx context.Context, q listing.Query, onPage func(source.PageEvent)) (source.Result, error) {
	maxPages := q.MaxPages
	if maxPages <= 0 {
		maxPages = MaxPages
	}
	hasLocalFilter := q.YearFrom != nil || q.YearTo != nil

	var order []string
	collected := map[string]listing.Listing{}
	res := source.Result{LocallyFiltered: hasLocalFilter}

	for page := 1; page <= maxPages; page++ {
		result, err := s.SearchPage(ctx, q, page)
		if err != nil {
			return source.Result{}, err
		}
		if page == 1 {
			res.Total, res.Pages, res.URL = result.Total, result.Pages, result.URL
			if result.Pages != nil && *result.Pages < float64(maxPages) {
				maxPages = int(*result.Pages)
			}
		}

		rawCount := len(result.Listings) + result.LocalFiltered
		isLastPage := page == maxPages
		if s.RetryShortPages && !hasLocalFilter && rawCount < PageSize && !isLastPage {
			res.ShortPages++
			retry, err := s.SearchPage(ctx, q, page)
			if err != nil {
				return source.Result{}, err
			}
			if len(retry.Listings)+retry.LocalFiltered > rawCount {
				result = retry
			}
		}

		for _, l := range result.Listings {
			key := l.Key()
			if _, seen := collected[key]; !seen {
				order = append(order, key)
			}
			collected[key] = l
		}
		res.Injected += result.InjectedCount
		res.PagesWalked = page
		if onPage != nil {
			onPage(source.PageEvent{Page: page, Of: maxPages, Found: len(result.Listings), Total: result.Total})
		}
		if len(result.Listings) == 0 && result.LocalFiltered == 0 {
			break
		}
	}

	res.Listings = make([]listing.Listing, 0, len(order))
	for _, k := range order {
		res.Listings = append(res.Listings, collected[k])
	}

	total := 0.0
	if res.Total != nil {
		total = *res.Total
	}
	if !hasLocalFilter {
		if short := int(total) - len(res.Listings); short > 0 {
			res.Shortfall = short
		}
		res.Truncated = total > float64(maxPages*PageSize)
	}
	if total != 0 {
		res.ShortfallRatio = float64(res.Shortfall) / total
	}
	res.HitSiteCeiling = res.Truncated && res.Pages != nil && *res.Pages >= MaxPages
	return res, nil
}
