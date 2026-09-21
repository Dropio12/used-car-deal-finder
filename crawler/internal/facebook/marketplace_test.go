package facebook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"carbuyer/crawler/internal/listing"
)

// Synthetic actor items: made-up ids, example.* hosts, no real sellers.
func rawItem(over map[string]any) map[string]any {
	raw := map[string]any{
		"id":                        "1234567890",
		"marketplace_listing_title": "2015 Honda Civic LX",
		"redacted_description":      map[string]any{"text": "Bien entretenue, 120 000 km"},
		"listing_price":             map[string]any{"amount": "9500.00", "currency": "CAD"},
		"creation_time":             float64(1788298199),
		"location": map[string]any{"latitude": 45.5, "longitude": -73.5,
			"reverse_geocode": map[string]any{"city": "Montréal", "postal_code_trimmed": "H2X 2L5"}},
		"listing_photos":             []any{map[string]any{"image": map[string]any{"uri": "https://scontent.example/1.jpg"}}},
		"marketplace_listing_seller": map[string]any{"id": "u1", "name": "A Seller"},
		"vehicle_odometer_data":      map[string]any{"value": float64(120000)},
	}
	for k, v := range over {
		if v == nil {
			delete(raw, k)
			continue
		}
		raw[k] = v
	}
	return raw
}

func civicMatcher(title, _ string) Vehicle {
	if strings.Contains(strings.ToLower(title), "civic") && strings.HasPrefix(title, "2015") {
		return Vehicle{Make: listing.Str("Honda"), Model: listing.Str("Civic"), Year: f(2015)}
	}
	return Vehicle{}
}

func TestEstimateCost(t *testing.T) {
	// The only paid source: the price has to be visible before a run.
	cases := []struct {
		n    int
		want float64
	}{{1000, CostPer1000}, {200, 0.3}, {0, 0}}
	for _, c := range cases {
		if got := EstimateCost(c.n); got != c.want {
			t.Errorf("EstimateCost(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}

func TestMarketplaceSearchURL(t *testing.T) {
	// daysSinceListed is what makes a nightly run cheap.
	got := MarketplaceSearchURL(MarketplaceURLOptions{DaysSinceListed: intp(1), MaxPrice: intp(15000)})
	want := "https://www.facebook.com/marketplace/montreal/vehicles?maxPrice=15000&daysSinceListed=1&radius=100&sortBy=creation_time_descend"
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

func TestParseMoney(t *testing.T) {
	// "2990.00" stripped of non-digits would be $299 000.
	cases := []struct {
		in   any
		want *float64
	}{
		{"2990.00", f(2990)},
		{"12500.50", f(12501)},
		{"CA$2,990", f(2990)},
		{float64(9500), f(9500)},
		{"", nil},
		{nil, nil},
		{"Free", nil},
		{"1.2.3", f(1)},
	}
	for _, c := range cases {
		if got := ParseMoney(c.in); !eqNum(got, c.want) {
			t.Errorf("ParseMoney(%v) = %s, want %s", c.in, show(got), show(c.want))
		}
	}
}

func TestBuildActorInput(t *testing.T) {
	cases := []struct {
		name  string
		o     ActorOptions
		check func(ActorInput) string
	}{
		// Given only a location the actor searches with no query and returns 0.
		{"cars category URL", ActorOptions{City: "montreal"}, func(in ActorInput) string {
			if len(in.URLs) != 1 || in.URLs[0] != "https://www.facebook.com/marketplace/montreal/cars" {
				return fmt.Sprint(in.URLs)
			}
			return ""
		}},
		{"proxy pinned to Canada", ActorOptions{}, func(in ActorInput) string {
			if in.Proxy.ApifyProxyCountry != "CA" || !in.Proxy.UseApifyProxy {
				return fmt.Sprint(in.Proxy)
			}
			return ""
		}},
		{"incremental carries cache id and daysSinceListed=1", ActorOptions{OnlyNew: true, CacheStorageID: "carbuyer-fbmp-montreal"}, func(in ActorInput) string {
			if !in.OnlyNewListings || in.CacheStorageID != "carbuyer-fbmp-montreal" || !strings.Contains(in.URLs[0], "daysSinceListed=1") {
				return fmt.Sprintf("%+v", in)
			}
			return ""
		}},
		{"not incremental by default", ActorOptions{}, func(in ActorInput) string {
			if in.OnlyNewListings || strings.Contains(in.URLs[0], "daysSinceListed") {
				return fmt.Sprintf("%+v", in)
			}
			return ""
		}},
		// maxPagesPerUrl is the cap the actor honours.
		{"page cap from 48 items", ActorOptions{MaxItems: 48}, func(in ActorInput) string {
			if in.MaxPagesPerURL != 2 {
				return fmt.Sprint(in.MaxPagesPerURL)
			}
			return ""
		}},
		{"page cap floor", ActorOptions{MaxItems: 10}, func(in ActorInput) string {
			if in.MaxPagesPerURL != 1 {
				return fmt.Sprint(in.MaxPagesPerURL)
			}
			return ""
		}},
		{"optional filters", ActorOptions{DaysSinceListed: intp(7), MinPrice: intp(1000)}, func(in ActorInput) string {
			if listing.Deref(in.DaysSinceListed) != "7" || in.MinPrice == nil || *in.MinPrice != 1000 || in.MaxPrice != nil {
				return fmt.Sprintf("%+v", in)
			}
			return ""
		}},
	}
	for _, c := range cases {
		if msg := c.check(BuildActorInput(c.o)); msg != "" {
			t.Errorf("%s: %s", c.name, msg)
		}
	}
	b, _ := json.Marshal(BuildActorInput(ActorOptions{}))
	for _, absent := range []string{"onlyNewListings", "cacheStorageId", "daysSinceListed", "minPrice"} {
		if strings.Contains(string(b), absent) {
			t.Errorf("%s sent when unset: %s", absent, b)
		}
	}
	if !strings.Contains(string(b), `"strictFiltering":false`) {
		t.Errorf("strictFiltering must be sent off: %s", b)
	}
}

func TestNormalizeMarketplace(t *testing.T) {
	l := NormalizeMarketplace(rawItem(nil), civicMatcher)
	checks := []struct{ name, got, want string }{
		{"id", l.Key(), "fbmp:1234567890"},
		{"source", l.Source, "marketplace"},
		{"make", listing.Deref(l.Make), "Honda"},
		{"model", listing.Deref(l.Model), "Civic"},
		{"year", show(l.Year), "2015"},
		{"price", show(l.Price), "9500"},
		{"km", show(l.Km), "120000"},
		{"province", listing.Deref(l.Province), "QC"},
		{"postal", listing.Deref(l.PostalCode), "H2X2L5"},
		{"city", listing.Deref(l.City), "Montréal"},
		{"listedAt", listing.Deref(l.ListedAt), time.Unix(1788298199, 0).UTC().Format("2006-01-02T15:04:05.000Z")},
		// Dealers post here too, but private is the safe direction to be wrong.
		{"sellerType", listing.Deref(l.SellerType), "PrivateSeller"},
		{"url", listing.Deref(l.URL), "https://www.facebook.com/marketplace/item/1234567890"},
		{"image", strings.Join(l.ImageURLs, ","), "https://scontent.example/1.jpg"},
		{"latitude", show(l.Latitude), "45.5"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}

	cases := []struct {
		name  string
		raw   map[string]any
		check func(Item) string
	}{
		{"formatted price string", rawItem(map[string]any{"listing_price": map[string]any{"formatted_amount": "$9,500"}}), func(l Item) string {
			if show(l.Price) != "9500" {
				return show(l.Price)
			}
			return ""
		}},
		// The gallery is switched off to save requests; one cover still triages.
		{"cover photo fallback", rawItem(map[string]any{"listing_photos": nil, "primary_listing_photo_url": "https://scontent.example/cover.jpg"}), func(l Item) string {
			if l.ImageCount != 1 || strings.Join(l.ImageURLs, ",") != "https://scontent.example/cover.jpg" {
				return fmt.Sprint(l.ImageCount, l.ImageURLs)
			}
			return ""
		}},
		{"almost empty item", map[string]any{"id": "7"}, func(l Item) string {
			if l.Key() != "fbmp:7" || l.Price != nil || l.Make != nil || len(l.ImageURLs) != 0 || l.URL == nil {
				return fmt.Sprintf("%+v", l)
			}
			return ""
		}},
		{"damage read from the description", rawItem(map[string]any{"redacted_description": map[string]any{"text": "Vendu pour pieces, ne demarre pas"}}), func(l Item) string {
			if !l.IsParts || !l.IsDamaged {
				return fmt.Sprintf("parts %v damaged %v", l.IsParts, l.IsDamaged)
			}
			return ""
		}},
		{"numeric id and explicit state", map[string]any{"id": float64(42), "location": map[string]any{"reverse_geocode": map[string]any{"state": "ON", "postal_code": "k1a 0b1"}}}, func(l Item) string {
			if l.Key() != "fbmp:42" || listing.Deref(l.Province) != "ON" || listing.Deref(l.PostalCode) != "K1A0B1" {
				return fmt.Sprintf("%s %s %s", l.Key(), listing.Deref(l.Province), listing.Deref(l.PostalCode))
			}
			return ""
		}},
		{"no id at all", map[string]any{"title": "x"}, func(l Item) string {
			if l.Key() != "fbmp:undefined" || l.URL != nil || listing.Deref(l.ReferenceID) != "" {
				return l.Key()
			}
			return ""
		}},
	}
	for _, c := range cases {
		if msg := c.check(NormalizeMarketplace(c.raw, civicMatcher)); msg != "" {
			t.Errorf("%s: %s", c.name, msg)
		}
	}
}

func TestNormalizeMarketplaceItems(t *testing.T) {
	// Drops parts and rows that carry no id.
	items := []map[string]any{
		rawItem(nil),
		rawItem(map[string]any{"id": "2", "marketplace_listing_title": "4 pneus hiver pour Civic", "listing_price": map[string]any{"amount": "300"}}),
		rawItem(map[string]any{"id": nil}),
	}
	listings, skipped := NormalizeMarketplaceItems(items, civicMatcher)
	if len(listings) != 1 || skipped != 2 {
		t.Errorf("kept %d skipped %d", len(listings), skipped)
	}
}

// apify is a fake Apify API. Never the network.
type apify struct {
	mu      sync.Mutex
	polls   int
	aborted int
	tokens  []string
	start   func(w http.ResponseWriter)
	poll    func(n int) string
	dataset string
}

func (a *apify) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.tokens = append(a.tokens, r.URL.Query().Get("token"))
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/abort"):
			a.aborted++
			fmt.Fprint(w, `{}`)
		case r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if !json.Valid(body) || r.Header.Get("content-type") != "application/json" {
				t.Errorf("bad start request: %s", body)
			}
			if a.start != nil {
				a.start(w)
				return
			}
			fmt.Fprint(w, `{"data":{"id":"run1","status":"RUNNING"}}`)
		case strings.HasSuffix(r.URL.Path, "/dataset/items"):
			if r.URL.Query().Get("clean") != "true" {
				t.Error("dataset fetched without clean=true")
			}
			fmt.Fprint(w, a.dataset)
		default:
			a.polls++
			fmt.Fprint(w, a.poll(a.polls))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runWith(srv *httptest.Server, r ActorRun) (ActorResult, error) {
	r.BaseURL = srv.URL
	r.Client = srv.Client()
	r.Sleep = func(time.Duration) {}
	if r.Token == "" {
		r.Token = "secret-token"
	}
	return RunActor(context.Background(), r)
}

func TestRunActor(t *testing.T) {
	t.Run("refuses a run over the cost ceiling", func(t *testing.T) {
		// A fat-fingered maxItems here wastes money, not time.
		_, err := RunActor(context.Background(), ActorRun{MaxItems: 10000, MaxCost: 1, Token: "t"})
		if err == nil || !strings.Contains(err.Error(), "refusing to start") {
			t.Fatal(err)
		}
	})
	t.Run("needs a token", func(t *testing.T) {
		if _, err := RunActor(context.Background(), ActorRun{}); err == nil || !strings.Contains(err.Error(), "APIFY_TOKEN") {
			t.Fatal(err)
		}
	})
	t.Run("polls until it succeeds, then returns the dataset", func(t *testing.T) {
		a := &apify{
			poll: func(n int) string {
				if n >= 2 {
					return `{"data":{"status":"SUCCEEDED"}}`
				}
				return `{"data":{"status":"RUNNING"}}`
			},
			dataset: `[{"id":"1234567890","marketplace_listing_title":"2015 Honda Civic LX"}]`,
		}
		res, err := runWith(a.server(t), ActorRun{MaxItems: 100})
		if err != nil {
			t.Fatal(err)
		}
		if res.RunID != "run1" || len(res.Items) != 1 || a.polls != 2 {
			t.Errorf("run %s, %d items, %d polls", res.RunID, len(res.Items), a.polls)
		}
		for _, tok := range a.tokens {
			if tok != "secret-token" {
				t.Errorf("token %q", tok)
			}
		}
	})
	t.Run("surfaces a failed run", func(t *testing.T) {
		a := &apify{poll: func(int) string { return `{"data":{"status":"FAILED"}}` }}
		_, err := runWith(a.server(t), ActorRun{MaxItems: 10})
		if err == nil || !strings.Contains(err.Error(), "finished as FAILED") {
			t.Fatal(err)
		}
	})
	t.Run("aborts when spend passes the ceiling", func(t *testing.T) {
		// maxItems is not honoured: a run asked for 40 returned 814.
		a := &apify{poll: func(int) string { return `{"data":{"status":"RUNNING","usageTotalUsd":2.5}}` }}
		_, err := runWith(a.server(t), ActorRun{MaxItems: 40, MaxCost: 0.5})
		if err == nil || !strings.Contains(err.Error(), "aborted: spend reached $2.50") {
			t.Fatal(err)
		}
		if a.aborted != 1 {
			t.Errorf("aborted %d times; the run must actually be stopped", a.aborted)
		}
	})
	t.Run("aborts at the deadline", func(t *testing.T) {
		a := &apify{poll: func(int) string { return `{"data":{"status":"RUNNING"}}` }}
		_, err := runWith(a.server(t), ActorRun{MaxItems: 10, Timeout: -time.Second})
		if err == nil || !strings.Contains(err.Error(), "still RUNNING") || a.aborted != 1 {
			t.Fatal(err)
		}
	})
	t.Run("surfaces an HTTP error from Apify", func(t *testing.T) {
		a := &apify{start: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, "unauthorized")
		}}
		_, err := runWith(a.server(t), ActorRun{MaxItems: 10, Token: "bad"})
		if err == nil || !strings.Contains(err.Error(), "Apify refused the run: HTTP 401 unauthorized") {
			t.Fatal(err)
		}
	})
	t.Run("an error object in the dataset is a rejected input", func(t *testing.T) {
		a := &apify{poll: func(int) string { return `{"data":{"status":"SUCCEEDED"}}` }, dataset: `[{"error":"bad input"}]`}
		_, err := runWith(a.server(t), ActorRun{MaxItems: 10})
		if err == nil || !strings.Contains(err.Error(), "rejected the input: bad input") {
			t.Fatal(err)
		}
	})
	t.Run("token is query-escaped", func(t *testing.T) {
		a := &apify{poll: func(int) string { return `{"data":{"status":"SUCCEEDED"}}` }, dataset: `[]`}
		if _, err := runWith(a.server(t), ActorRun{MaxItems: 10, Token: "a&b c"}); err != nil {
			t.Fatal(err)
		}
		if a.tokens[0] != "a&b c" {
			t.Errorf("token arrived as %q", a.tokens[0])
		}
	})
}
