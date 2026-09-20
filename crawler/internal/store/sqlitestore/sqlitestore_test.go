package sqlitestore

import (
	"context"
	"path/filepath"
	"testing"

	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/store"
)

var ctx = context.Background()

func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func car(id string, price float64, seller string) listing.Listing {
	return listing.Listing{
		ID: listing.Str(id), Source: "autohebdo", URL: listing.Str("https://x/" + id),
		Make: listing.Str("Toyota"), Model: listing.Str("RAV4"), Year: listing.Num(2019), Km: listing.Num(60000),
		Price: listing.Num(price), TrimText: listing.Str("XLE"), SellerType: listing.Str(seller),
		Province: listing.Str("QC"), ImageURLs: []string{"https://img/1.jpg"}, ImageCount: 3,
	}
}

func scope(at string) store.Scope {
	return store.Scope{MakeSlug: listing.Str("toyota"), ModelSlug: listing.Str("rav4"), GeoSlug: listing.Str("reg_qc"), SeenAt: at}
}

func TestSchemaHasEveryMigratedColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	// Re-opening an existing database must migrate nothing and fail nothing.
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.SQL().Query("PRAGMA table_info(listings)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if len(names) != 31+len(migrations) || names[0] != "id" || names[len(names)-1] != "geo_slug" {
		t.Errorf("columns = %v", names)
	}
}

func TestSaveTracksPriceHistory(t *testing.T) {
	cases := []struct {
		name                          string
		price                         float64
		added, drops, rises, relisted int
		wantChanges                   int
	}{
		{"first sighting inserts", 25000, 1, 0, 0, 0, 0},
		{"same price changes nothing", 25000, 0, 0, 0, 0, 0},
		{"a price cut is recorded", 23500, 0, 1, 0, 0, 1},
		{"a price rise is recorded", 24000, 0, 0, 1, 0, 2},
	}
	db := open(t)
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			at := "2026-09-0" + string(rune('1'+i)) + "T00:00:00.000Z"
			s, err := db.Save(ctx, []listing.Listing{car("a", c.price, "Dealer")}, scope(at))
			if err != nil {
				t.Fatal(err)
			}
			if s.Added != c.added || s.PriceDrops != c.drops || s.PriceRises != c.rises || s.Relisted != c.relisted {
				t.Errorf("stats = %+v", s)
			}
			rows, _ := db.LoadListings(ctx, store.Filter{SellerType: "Dealer"})
			if len(rows) != 1 || rows[0].PriceChanges != c.wantChanges || *rows[0].Price != c.price || *rows[0].FirstPrice != 25000 {
				t.Errorf("row = %+v", rows)
			}
		})
	}
	var events int
	db.SQL().QueryRow("SELECT COUNT(*) FROM price_history").Scan(&events)
	if events != 3 {
		t.Errorf("price events = %d, want 3", events)
	}
}

func TestMarkRemovedIsScoped(t *testing.T) {
	db := open(t)
	if _, err := db.Save(ctx, []listing.Listing{car("old", 20000, "Dealer"), car("gone-private", 15000, "PrivateSeller")}, scope("2026-09-01T00:00:00.000Z")); err != nil {
		t.Fatal(err)
	}
	other := scope("2026-09-01T00:00:00.000Z")
	other.GeoSlug = listing.Str("cit_montreal")
	if _, err := db.Save(ctx, []listing.Listing{car("mtl", 20000, "Dealer")}, other); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Save(ctx, []listing.Listing{car("fresh", 21000, "Dealer")}, scope("2026-09-02T00:00:00.000Z")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		s    store.RemovalScope
		want int
	}{
		{"dealer-only crawl leaves private rows alone", store.RemovalScope{SellerType: "D"}, 1},
		{"then the private one", store.RemovalScope{SellerType: "P"}, 1},
		{"nothing left in this scope", store.RemovalScope{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rs := c.s
			rs.MakeSlug, rs.ModelSlug, rs.GeoSlug = listing.Str("toyota"), listing.Str("rav4"), listing.Str("reg_qc")
			rs.Before, rs.At = "2026-09-02T00:00:00.000Z", "2026-09-02T00:00:00.000Z"
			n, err := db.MarkRemoved(ctx, rs)
			if err != nil || n != c.want {
				t.Errorf("removed %d (%v), want %d", n, err, c.want)
			}
		})
	}
	active, _ := db.LoadListings(ctx, store.Filter{})
	ids := map[string]bool{}
	for _, r := range active {
		ids[r.Key()] = true
	}
	// The Montreal-scoped row was never in this crawl's scope, so it survives.
	if !ids["mtl"] || !ids["fresh"] || ids["old"] || len(active) != 2 {
		t.Errorf("active = %v", ids)
	}

	// Seeing a removed car again relists it.
	s, _ := db.Save(ctx, []listing.Listing{car("old", 20000, "Dealer")}, scope("2026-09-03T00:00:00.000Z"))
	if s.Relisted != 1 {
		t.Errorf("relisted = %d", s.Relisted)
	}
}

func TestLoadListingsFiltersAndOrder(t *testing.T) {
	db := open(t)
	cars := []listing.Listing{car("c1", 1, "Dealer"), car("c2", 2, "PrivateSeller"), car("c3", 3, "Dealer")}
	cars[2].IsDamaged = true
	if _, err := db.Save(ctx, cars, scope("2026-09-01T00:00:00.000Z")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		f    store.Filter
		want []string
	}{
		{store.Filter{}, []string{"c1", "c2", "c3"}},
		{store.Filter{SellerType: "Dealer"}, []string{"c1", "c3"}},
		{store.Filter{SellerType: "PrivateSeller", Source: "autohebdo"}, []string{"c2"}},
		{store.Filter{Make: "Honda"}, nil},
	}
	for _, c := range cases {
		rows, err := db.LoadListings(ctx, c.f)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range rows {
			got = append(got, r.Key())
		}
		if len(got) != len(c.want) {
			t.Errorf("%+v: got %v", c.f, got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%+v: got %v", c.f, got)
			}
		}
	}
	rows, _ := db.LoadListings(ctx, store.Filter{})
	if !rows[2].IsDamaged || rows[0].IsDamaged || len(rows[0].ImageURLs) != 1 || rows[0].ImageCount != 3 {
		t.Errorf("round trip lost fields: %+v", rows[0])
	}
}

func TestCrawlLog(t *testing.T) {
	db := open(t)
	id, err := db.StartCrawl(ctx, map[string]string{"make": "toyota"})
	if err != nil {
		t.Fatal(err)
	}
	msg := "boom"
	if err := db.FinishCrawl(ctx, id, store.CrawlStats{Seen: 20, Added: 5, Error: &msg}); err != nil {
		t.Fatal(err)
	}
	c, err := db.Stats(ctx)
	if err != nil || c.Crawls != 1 {
		t.Errorf("stats = %+v, %v", c, err)
	}
}
