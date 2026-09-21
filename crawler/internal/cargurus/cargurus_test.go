package cargurus

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"carbuyer/crawler/internal/fetch"
	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/source"
)

// raw is a synthetic listing in the feed's shape. No real car, seller or VIN.
func raw(overrides map[string]any) map[string]any {
	r := map[string]any{
		"id":                    float64(1000001),
		"inclusionType":         "DEFAULT",
		"listingTitle":          "2019 Toyota RAV4 LE AWD",
		"makeName":              "Toyota",
		"modelName":             "RAV4",
		"carYear":               float64(2019),
		"trimName":              "LE AWD",
		"localizedTransmission": "Automatic",
		"unitMileage":           map[string]any{"value": float64(129000), "unit": "KILOMETERS"},
		"mileage":               float64(129000),
		"price":                 float64(22995),
		"expectedPrice":         float64(23600),
		"daysOnMarket":          float64(3),
		"dealRating":            "GOOD_PRICE",
		"pictureCount":          float64(24),
		"originalPictureData":   map[string]any{"url": "https://img.example.test/car.jpeg"},
		"sellerId":              float64(42),
		"sellerType":            "DEALER",
		"sellerCity":            "Exampleville, QC",
		"sellerRegion":          "QC",
		"sellerPostalCode":      "A1A 1A1",
		"serviceProviderName":   "Example Motors",
		"localizedFuelType":     "Gasoline",
		"vin":                   "TESTVIN0000000000",
	}
	for k, v := range overrides {
		if v == nil {
			delete(r, k)
		} else {
			r[k] = v
		}
	}
	return r
}

// deleted marks a key to remove from raw().
var deleted any

var clock = time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)

func deref(p *string) string { return listing.Deref(p) }

func numStr(p *float64) string {
	if p == nil {
		return "<nil>"
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}

func TestBuildURL(t *testing.T) {
	y := 2019
	tests := []struct {
		name string
		opts URLOptions
		want map[string]string // "" means absent
	}{
		{"year shard and offset", URLOptions{StartYear: &y, EndYear: &y, Offset: 96}, map[string]string{
			"sortType": "AGE_IN_DAYS", "sortDir": "ASC", "startYear": "2019", "endYear": "2019",
			"offset": "96", "maxResults": "48", "zip": "H2X2L5", "distance": "100",
			// minYear/maxYear are accepted and ignored by CarGurus.
			"minYear": "", "maxYear": "",
		}},
		{"no years", URLOptions{}, map[string]string{"startYear": "", "endYear": "", "offset": "0"}},
		{"toronto", URLOptions{Scope: Toronto}, map[string]string{"zip": "M5V2T6", "distance": "100"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(BuildURL(tt.opts))
			if err != nil {
				t.Fatal(err)
			}
			for k, want := range tt.want {
				if got := u.Query().Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
		})
	}

	// Same parameter order as the JS URLSearchParams.
	want := "https://www.cargurus.ca/Cars/searchResults.action?zip=H2X2L5&distance=100" +
		"&inventorySearchWidgetType=AUTO&shopByTypes=NEAR_BY&sortType=AGE_IN_DAYS&sortDir=ASC" +
		"&startYear=2019&endYear=2019&offset=96&maxResults=48"
	if got := BuildURL(URLOptions{StartYear: &y, EndYear: &y, Offset: 96}); got != want {
		t.Errorf("url\n got %s\nwant %s", got, want)
	}
}

func TestShards(t *testing.T) {
	tests := []struct {
		name     string
		from, to int
		want     []Shard
	}{
		{"every year once, old stock in one bucket", 2005, 2007, []Shard{
			{1900, 2004}, {2005, 2005}, {2006, 2006}, {2007, 2007},
		}},
		{"from past to is just the old bucket", 2005, 2004, []Shard{{1900, 2004}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Shards(tt.from, tt.to)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
	d := DefaultShards(clock)
	if last := d[len(d)-1]; last != (Shard{2027, 2027}) {
		t.Errorf("default shards end at %v, want next year", last)
	}
}

func TestScopeFor(t *testing.T) {
	tests := []struct {
		geo     string
		want    Scope
		wantErr bool
	}{
		{"", Montreal, false},
		{"montreal", Montreal, false},
		{"Toronto", Toronto, false},
		{"vancouver", Scope{}, true},
	}
	for _, tt := range tests {
		got, err := ScopeFor(tt.geo)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ScopeFor(%q) = %v, %v", tt.geo, got, err)
		}
	}
	if got := Montreal.GeoSlug(); got != "cargurus-H2X2L5-100km" {
		t.Errorf("GeoSlug = %q", got)
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]any
		check     func(t *testing.T, l listing.Listing, x Extras)
	}{
		{"takes make, model and year from fields, not the title", nil, func(t *testing.T, l listing.Listing, x Extras) {
			got := map[string]string{
				"id": deref(l.ID), "ref": deref(l.ReferenceID), "url": deref(l.URL), "make": deref(l.Make),
				"model": deref(l.Model), "year": numStr(l.Year), "km": numStr(l.Km), "price": numStr(l.Price),
				"msrp": numStr(l.SuggestedRetailPrice), "seller": deref(l.SellerType), "sellerId": deref(l.SellerID),
				"sellerName": deref(l.SellerName), "city": deref(l.City), "fuel": deref(l.Fuel),
				"trans": deref(l.Transmission), "trim": deref(l.TrimText), "result": deref(l.ResultType),
				"cond": deref(l.Condition), "rating": deref(x.PriceRating), "listedAt": deref(x.ListedAt),
				"vin": deref(x.VIN), "title": x.Title,
			}
			want := map[string]string{
				"id": "cargurus:1000001", "ref": "1000001",
				"url":  "https://www.cargurus.ca/Cars/inventorylisting/vdp.action?listingId=1000001",
				"make": "Toyota", "model": "RAV4", "year": "2019", "km": "129000", "price": "22995",
				"msrp": "23600", "seller": "Dealer", "sellerId": "42", "sellerName": "Example Motors",
				"city": "Exampleville", "fuel": "Essence", "trans": "Automatique", "trim": "LE AWD",
				"result": "Organic", "cond": "U", "rating": "GOOD", "listedAt": "2026-09-21T00:00:00.000Z",
				"vin": "TESTVIN0000000000", "title": "2019 Toyota RAV4 LE AWD",
			}
			for k, w := range want {
				if got[k] != w {
					t.Errorf("%s = %q, want %q", k, got[k], w)
				}
			}
			if l.Source != "cargurus" || l.ImageCount != 24 || len(l.ImageURLs) != 1 || l.IsDamaged || l.IsParts {
				t.Errorf("unexpected %+v", l)
			}
		}},
		{"converts a mileage reported in miles", map[string]any{"unitMileage": map[string]any{"value": float64(1000), "unit": "MILES"}},
			func(t *testing.T, l listing.Listing, _ Extras) {
				if numStr(l.Km) != "1609" {
					t.Errorf("km = %s", numStr(l.Km))
				}
			}},
		{"falls back to plain mileage", map[string]any{"unitMileage": deleted, "mileage": float64(5000)},
			func(t *testing.T, l listing.Listing, _ Extras) {
				if numStr(l.Km) != "5000" {
					t.Errorf("km = %s", numStr(l.Km))
				}
			}},
		{"no mileage at all is null", map[string]any{"unitMileage": deleted, "mileage": deleted},
			func(t *testing.T, l listing.Listing, _ Extras) {
				if l.Km != nil {
					t.Errorf("km = %s", numStr(l.Km))
				}
			}},
		{"private seller, dealerName fallback, trim falls back to title",
			map[string]any{"sellerType": "PRIVATE", "serviceProviderName": deleted, "dealerName": "Other Lot", "trimName": deleted},
			func(t *testing.T, l listing.Listing, _ Extras) {
				if deref(l.SellerType) != "PrivateSeller" || deref(l.SellerName) != "Other Lot" || deref(l.TrimText) != "2019 Toyota RAV4 LE AWD" {
					t.Errorf("got %q %q %q", deref(l.SellerType), deref(l.SellerName), deref(l.TrimText))
				}
			}},
		{"zero price is no price; unmapped values pass through",
			map[string]any{"price": float64(0), "localizedTransmission": "CVT", "localizedFuelType": "Hydrogen", "dealRating": "NO_RATING", "inclusionType": "FEATURED"},
			func(t *testing.T, l listing.Listing, x Extras) {
				if l.Price != nil || deref(l.Transmission) != "CVT" || deref(l.Fuel) != "Hydrogen" || x.PriceRating != nil || deref(l.ResultType) != "FEATURED" {
					t.Errorf("got %+v %+v", l, x)
				}
			}},
		{"rounds prices like Math.round", map[string]any{"price": 100.5, "expectedPrice": 99.4},
			func(t *testing.T, l listing.Listing, _ Extras) {
				if numStr(l.Price) != "101" || numStr(l.SuggestedRetailPrice) != "99" {
					t.Errorf("got %s %s", numStr(l.Price), numStr(l.SuggestedRetailPrice))
				}
			}},
		{"no pictures, no city, no days on market",
			map[string]any{"pictureCount": float64(0), "originalPictureData": deleted, "sellerCity": " , QC", "daysOnMarket": deleted},
			func(t *testing.T, l listing.Listing, x Extras) {
				if l.ImageCount != 0 || l.ImageURLs == nil || len(l.ImageURLs) != 0 || l.City != nil || x.ListedAt != nil {
					t.Errorf("got %+v %+v", l, x)
				}
			}},
		{"one picture counted when only the main one is given", map[string]any{"pictureCount": deleted},
			func(t *testing.T, l listing.Listing, _ Extras) {
				if l.ImageCount != 1 {
					t.Errorf("imageCount = %d", l.ImageCount)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, x := Normalize(raw(tt.overrides), clock)
			tt.check(t, l, x)
		})
	}
}

func TestParsePage(t *testing.T) {
	cars := func(n int) string {
		var out []any
		for i := 0; i < n; i++ {
			out = append(out, raw(map[string]any{"id": float64(i + 1)}))
		}
		b, _ := json.Marshal(out)
		return string(b)
	}
	tests := []struct {
		name      string
		body      string
		wantN     int
		wantEnd   bool
		wantParse bool
	}{
		{"null is the end of the results", "null", 0, true, false},
		{"a short page is the last one", cars(1), 1, true, false},
		{"a full page is not", cars(PageSize), PageSize, false, false},
		{"entries without an id are skipped", `[null, {"id": null}, {"id": 7}]`, 1, true, false},
		{"the captcha page is a ParseError", "<html>captcha</html>", 0, false, true},
		{"an empty body is a ParseError", "", 0, false, true},
		{"an object is a ParseError", `{"error":"x"}`, 0, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ParsePage(tt.body, clock)
			var pe *fetch.ParseError
			if tt.wantParse {
				if !errors.As(err, &pe) {
					t.Fatalf("err = %v, want ParseError", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Listings) != tt.wantN || p.End != tt.wantEnd || len(p.Extras) != len(p.Listings) {
				t.Errorf("got %d listings end=%v, want %d end=%v", len(p.Listings), p.End, tt.wantN, tt.wantEnd)
			}
		})
	}
}

// fakeSite serves `count` cars per shard, 48 per page, then null, and refuses
// offsets at the ceiling like the real endpoint. Never the network.
type fakeSite struct {
	count    int
	idOffset map[string]int // startYear -> id base, so shards can overlap or not
	urls     []string
}

func (f *fakeSite) Fetch(_ context.Context, u string) (string, error) {
	f.urls = append(f.urls, u)
	q, err := url.Parse(u)
	if err != nil {
		return "", err
	}
	offset, _ := strconv.Atoi(q.Query().Get("offset"))
	if offset >= min(f.count, OffsetCeiling) {
		return "null", nil
	}
	base := f.idOffset[q.Query().Get("startYear")]
	n := min(PageSize, f.count-offset)
	out := make([]any, n)
	for i := range out {
		out[i] = raw(map[string]any{"id": float64(base + offset + i + 1)})
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

func TestSearch(t *testing.T) {
	one := []Shard{{2019, 2019}}
	two := []Shard{{2018, 2018}, {2019, 2019}}
	tests := []struct {
		name            string
		site            *fakeSite
		shards          []Shard
		query           listing.Query
		known           func(string) bool
		wantSeen        int
		wantRequests    int
		wantTruncated   bool
		wantCeiling     bool
		wantStoppedEarl bool
	}{
		{name: "walks a shard to the end", site: &fakeSite{count: 100}, shards: one,
			wantSeen: 100, wantRequests: 3},
		{name: "an exact multiple ends on null", site: &fakeSite{count: 96}, shards: one,
			wantSeen: 96, wantRequests: 3},
		{name: "a shard still full at the ceiling is truncated", site: &fakeSite{count: 20_000}, shards: one,
			wantSeen: 209 * PageSize, wantRequests: 209, wantTruncated: true, wantCeiling: true},
		{name: "MaxPages caps a shard without blaming the site", site: &fakeSite{count: 500}, shards: one,
			query: listing.Query{MaxPages: 2}, wantSeen: 96, wantRequests: 2, wantTruncated: true},
		{name: "the same car in two shards is kept once", site: &fakeSite{count: 10}, shards: two,
			wantSeen: 10, wantRequests: 2},
		{name: "distinct shards add up", site: &fakeSite{count: 10, idOffset: map[string]int{"2019": 1000}}, shards: two,
			wantSeen: 20, wantRequests: 2},
		{name: "year bounds drop shards outside them", site: &fakeSite{count: 10}, shards: two,
			query: listing.Query{YearFrom: listing.Num(2019)}, wantSeen: 10, wantRequests: 1},
		{name: "a new-listings run stops after three known pages", site: &fakeSite{count: 1000}, shards: one,
			known: func(string) bool { return true }, wantSeen: 3 * PageSize, wantRequests: 3,
			wantTruncated: true, wantStoppedEarl: true},
		{name: "a new-listings run keeps going while pages hold new cars", site: &fakeSite{count: 100}, shards: one,
			known: func(string) bool { return false }, wantSeen: 100, wantRequests: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := []Option{WithShards(tt.shards), WithClock(func() time.Time { return clock })}
			if tt.known != nil {
				opts = append(opts, WithKnown(tt.known))
			}
			var events []source.PageEvent
			w, err := New(tt.site, opts...).Walk(context.Background(), tt.query, func(e source.PageEvent) { events = append(events, e) })
			if err != nil {
				t.Fatal(err)
			}
			if len(w.Listings) != tt.wantSeen || len(tt.site.urls) != tt.wantRequests ||
				w.Truncated != tt.wantTruncated || w.HitSiteCeiling != tt.wantCeiling || w.StoppedEarly != tt.wantStoppedEarl {
				t.Errorf("seen=%d requests=%d truncated=%v ceiling=%v early=%v",
					len(w.Listings), len(tt.site.urls), w.Truncated, w.HitSiteCeiling, w.StoppedEarly)
			}
			if w.PagesWalked != len(tt.site.urls) || len(events) != len(tt.site.urls) {
				t.Errorf("pagesWalked=%d events=%d", w.PagesWalked, len(events))
			}
			if len(w.Extras) != len(w.Listings) {
				t.Errorf("extras=%d listings=%d", len(w.Extras), len(w.Listings))
			}
		})
	}
}

func TestSearchGeoAndErrors(t *testing.T) {
	site := &fakeSite{count: 1}
	s := New(site, WithShards([]Shard{{2019, 2019}}))
	if _, err := s.Search(context.Background(), listing.Query{Geo: "toronto"}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(site.urls[0], "zip=M5V2T6&distance=100") {
		t.Errorf("toronto url = %s", site.urls[0])
	}
	if _, err := s.Search(context.Background(), listing.Query{Geo: "nowhere"}, nil); err == nil {
		t.Error("unknown geo should fail")
	}
	var pe *fetch.ParseError
	_, err := New(fetch.Static{Body: "<html>captcha</html>"}, WithShards([]Shard{{2019, 2019}})).
		Search(context.Background(), listing.Query{}, nil)
	if !errors.As(err, &pe) {
		t.Errorf("captcha err = %v", err)
	}
	if New(site).Name() != "cargurus" {
		t.Error("name")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Without browser-shaped headers CarGurus answers 403 with a captcha. The
// transport is faked: the request never leaves the process.
func TestNewHTTPFetcherSendsBrowserHeaders(t *testing.T) {
	var seen http.Header
	f := NewHTTPFetcher()
	f.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		seen = r.Header.Clone()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("null")), Header: http.Header{}}, nil
	})}
	s := New(f, WithShards([]Shard{{2019, 2019}}))
	if _, err := s.Search(context.Background(), listing.Query{}, nil); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Sec-Fetch-Mode": "cors", "Sec-Fetch-Site": "same-origin", "Sec-Fetch-Dest": "empty",
		"Accept": "application/json, text/plain, */*", "Accept-Language": "en-CA,en;q=0.9",
		"Referer": "https://www.cargurus.ca/",
	}
	for k, v := range want {
		if got := seen.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// The package-level map is copied, not shared.
	f.Headers["referer"] = "x"
	if Headers["referer"] != "https://www.cargurus.ca/" {
		t.Error("NewHTTPFetcher aliased Headers")
	}
}
