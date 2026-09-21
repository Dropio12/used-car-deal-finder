package main

import (
	"context"
	"fmt"
	"strings"

	"carbuyer/crawler/internal/autohebdo"
	"carbuyer/crawler/internal/cargurus"
	"carbuyer/crawler/internal/craigslist"
	"carbuyer/crawler/internal/facebook"
	"carbuyer/crawler/internal/fetch"
	"carbuyer/crawler/internal/kijiji"
	"carbuyer/crawler/internal/lespac"
	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/searches"
	"carbuyer/crawler/internal/source"
	"carbuyer/crawler/internal/store/sqlitestore"
)

// sourceKit builds the concrete sources. All of them share one Throttle, so
// requests never overlap and stay spaced across sites, not just within one.
type sourceKit struct {
	throttle *fetch.Throttle
	// offline replaces every network fetcher (--offline).
	offline fetch.Fetcher
	db      *sqlitestore.DB
	vocab   *lespac.Vocabulary
}

func (k *sourceKit) fetcher(client *fetch.HTTPFetcher) fetch.Fetcher {
	if k.offline != nil {
		return k.offline
	}
	return k.throttle.Wrap(client)
}

// vocabulary is read once per run: the makes and models already stored by
// the structured sources, which title-only sources match against.
func (k *sourceKit) vocabulary(ctx context.Context) (lespac.Vocabulary, error) {
	if k.vocab != nil {
		return *k.vocab, nil
	}
	makes, models, err := k.db.Vocabulary(ctx)
	if err != nil {
		return lespac.Vocabulary{}, err
	}
	v := lespac.Vocabulary{Makes: makes}
	for _, m := range models {
		v.Models = append(v.Models, lespac.Model{Make: m.Make, Model: m.Model, As: m.Model})
	}
	k.vocab = &v
	return v, nil
}

func (k *sourceKit) build(ctx context.Context, name string) (source.Source, error) {
	switch name {
	case "autohebdo":
		return autohebdo.New(k.fetcher(fetch.NewHTTPFetcher())), nil
	case "kijiji":
		return withoutPrice{kijiji.New(k.fetcher(fetch.NewHTTPFetcher()))}, nil
	case "cargurus":
		return cargurus.New(k.fetcher(cargurus.NewHTTPFetcher())), nil
	}
	v, err := k.vocabulary(ctx)
	if err != nil {
		return nil, err
	}
	match := lespac.NewMatcher(v)
	switch name {
	case "lespac":
		return lespac.New(k.fetcher(fetch.NewHTTPFetcher()), lespac.WithMatcher(match)), nil
	case "craigslist":
		var f fetch.Fetcher = craigslist.HTTP{Client: fetch.NewHTTPFetcher(), Throttle: k.throttle}
		if k.offline != nil {
			f = k.offline
		}
		// The feed is one request; each post page read is another. 20 per run
		// keeps a scheduled crawl small (the JS watcher reads up to 100).
		return craigslist.New(f, craigslist.WithReadPages(20), craigslist.WithMatcher(func(title string) craigslist.Vehicle {
			m := match(title, "")
			return craigslist.Vehicle{Make: m.Make, Model: m.Model, Year: m.Year}
		})), nil
	case "marketplace":
		return facebook.New(k.fetcher(facebook.NewHTTPFetcher()), facebook.WithMatcher(func(title, desc string) facebook.Vehicle {
			m := match(title, desc)
			return facebook.Vehicle{Make: m.Make, Model: m.Model, Year: m.Year}
		})), nil
	}
	return nil, fmt.Errorf("--source %q: use one of %s", name, strings.Join(searches.Sources, ", "))
}

// withoutPrice drops the price bounds before asking Kijiji, which cannot
// filter on them and refuses a query that has them. Like the other sources
// that ignore price, it then stores every car it walked, which only adds comps.
type withoutPrice struct{ *kijiji.Source }

func (w withoutPrice) Search(ctx context.Context, q listing.Query, onPage func(source.PageEvent)) (source.Result, error) {
	q.PriceFrom, q.PriceTo = nil, nil
	return w.Source.Search(ctx, q, onPage)
}
