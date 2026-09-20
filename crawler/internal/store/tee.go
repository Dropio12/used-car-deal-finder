package store

import (
	"context"
	"fmt"

	"carbuyer/crawler/internal/listing"
)

// Tee returns a Store that behaves exactly like primary, except that every
// batch primary saves is then also written to each mirror (for example the
// Cloudflare Worker API, see store/apistore). Reads, removals and the crawl
// log stay on primary.
//
// The pipeline does not know about mirrors: the CLI wraps its store with Tee
// when --push is given. The stats returned are primary's. A mirror error is
// returned (wrapped) after primary has committed, so the caller sees that the
// push failed while the local database stays correct.
func Tee(primary Store, mirrors ...Writer) Store {
	if len(mirrors) == 0 {
		return primary
	}
	return tee{Store: primary, mirrors: mirrors}
}

type tee struct {
	Store
	mirrors []Writer
}

func (t tee) Save(ctx context.Context, listings []listing.Listing, scope Scope) (SaveStats, error) {
	stats, err := t.Store.Save(ctx, listings, scope)
	if err != nil {
		return stats, err
	}
	for _, m := range t.mirrors {
		if _, err := m.Save(ctx, listings, scope); err != nil {
			return stats, fmt.Errorf("saved locally, but mirror failed: %w", err)
		}
	}
	return stats, nil
}
