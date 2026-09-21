package lespac

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"carbuyer/crawler/internal/fetch"
	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/source"
)

// Every fixture here is synthetic: invented ids, example.test hosts, no real
// sellers, cities or phone numbers.

var vocab = Vocabulary{
	Makes: []string{"Toyota", "Honda", "Chevrolet", "Mercedes-Benz", "Cadillac", "Kia", "Jeep", "Mazda", "Ford", "Chrysler"},
	Models: []Model{
		{Make: "Toyota", Model: "Corolla"}, {Make: "Toyota", Model: "Yaris"},
		{Make: "Honda", Model: "Civic"}, {Make: "Chevrolet", Model: "Corvette"},
		{Make: "Kia", Model: "Rio"}, {Make: "Jeep", Model: "Wrangler"},
		{Make: "Jeep", Model: "Grand Cherokee"}, {Make: "Jeep", Model: "Cherokee"},
		// The shapes that used to be unmatchable: punctuation, and names so
		// short a length floor deleted them.
		{Make: "Mazda", Model: "CX-5"}, {Make: "Mazda", Model: "3"},
		{Make: "Honda", Model: "CR-V"}, {Make: "Ford", Model: "F-150"},
		{Make: "Mercedes-Benz", Model: "C-Class"}, {Make: "Chrysler", Model: "300"},
	},
}

var match = NewMatcher(vocab)

func show(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func showN(p *float64) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprint(*p)
}

func vehicleString(v Vehicle) string {
	return show(v.Make) + "|" + show(v.Model) + "|" + showN(v.Year)
}

func TestMatcher(t *testing.T) {
	bmwFord := NewMatcher(Vocabulary{
		Makes:  []string{"BMW", "Ford"},
		Models: []Model{{Make: "Ford", Model: "Fusion"}, {Make: "BMW", Model: "X5"}},
	})
	lexus := NewMatcher(Vocabulary{
		Makes:  []string{"Lexus", "Suzuki", "BMW"},
		Models: []Model{{Make: "Lexus", Model: "IS"}, {Make: "BMW", Model: "X5"}},
	})
	canon := NewMatcher(Vocabulary{
		Makes:  []string{"Honda"},
		Models: []Model{{Make: "Honda", Model: "CR-V", As: "Crv"}},
	})
	cases := []struct {
		name        string
		m           Matcher
		title, desc string
		want        string // make|model|year
	}{
		{"reads make, model and year from a title", match, "Honda Civic Lx 2015 ,82 000km , automatique", "", "Honda|Civic|2015"},
		{"hyphenated model CX-5", match, "2023 Mazda CX-5 GT AWD", "", "Mazda|CX-5|2023"},
		{"hyphenated model CR-V", match, "2016 Honda CR-V EX-L", "", "Honda|CR-V|2016"},
		{"hyphenated model F-150", match, "2018 Ford F-150 XLT", "", "Ford|F-150|2018"},
		{"hyphenated make", match, "2011 Mercedes-Benz C-Class C250", "", "Mercedes-Benz|C-Class|2011"},
		{"one-character model once the make is known", match, "2019 Mazda 3 GS", "", "Mazda|3|2019"},
		{"one-character model never identifies the make", match, "moteur 3 litres, pas de marque ici", "", "<nil>|<nil>|<nil>"},
		{"a model is never borrowed from another marque", match, "2026 Mercedes-Benz C 300", "", "Mercedes-Benz|<nil>|2026"},
		{"the longest name still wins", match, "2018 Jeep Grand Cherokee Limited", "", "Jeep|Grand Cherokee|2018"},
		{"year-first titles", match, "2021 Toyota Corolla SE", "", "Toyota|Corolla|2021"},
		{"infers the make from a model", match, "Corvette C8 ZR1 3LZ 2026", "", "Chevrolet|Corvette|2026"},
		{"prefers the longer model name", match, "2015 Jeep Grand Cherokee Limited", "", "Jeep|Grand Cherokee|2015"},
		{"year from the description", match, "Kia Rio à vendre", "Belle Kia Rio 2014, 223 000 km", "Kia|Rio|2014"},
		{"mileage and phone are not years", match, "Honda Civic 82 000km 555 123 4567", "", "Honda|Civic|<nil>"},
		{"earliest plausible year", match, "2015 Honda Civic, inspecte 2024", "", "Honda|Civic|2015"},
		{"nulls on junk", match, "Convertible  Dame  $3ooo", "", "<nil>|<nil>|<nil>"},
		{"never pairs a make with another make's model", bmwFord, "2016 BMW, comme une Fusion", "", "BMW|<nil>|2016"},
		{"make and own model", bmwFord, "2016 BMW X5", "", "BMW|X5|2016"},
		{"a model still implies its make", match, "Corvette 2020", "", "Chevrolet|Corvette|2020"},
		{"two-letter model does not identify a make", lexus, "2009 Suzuki SV650, it is clean", "", "Suzuki|<nil>|2009"},
		{"two-letter model once the make agrees", lexus, "2012 Lexus IS 250", "", "Lexus|IS|2012"},
		{"two-letter model X5", lexus, "2016 BMW X5", "", "BMW|X5|2016"},
		{"searches the written spelling, answers the agreed one", canon, "Honda Crv 2012", "", "Honda|CR-V|2012"},
		{"accents do not block a match", NewMatcher(Vocabulary{Makes: []string{"Citroën"}}), "citroen 2001", "", "Citroën|<nil>|2001"},
		{"a combining accent does not split a word", NewMatcher(Vocabulary{Makes: []string{"Citroen"}}), "CITROËN 1999", "", "Citroen|<nil>|1999"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := vehicleString(c.m(c.title, c.desc)); got != c.want {
				t.Fatalf("match(%q) = %s, want %s", c.title, got, c.want)
			}
		})
	}
}

func TestNorm(t *testing.T) {
	cases := map[string]string{
		"Mercedes-Benz":   "mercedes benz",
		"  CX-5 ":         "cx 5",
		"Hyundai Élantra": "hyundai elantra",
		"Škoda Octavia!!": "skoda octavia",
		"Straße":          "stra e",
		"":                "",
		"---":             "",
		"été 2020":      "ete 2020",
	}
	for in, want := range cases {
		if got := norm(in); got != want {
			t.Errorf("norm(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLooksLikePart(t *testing.T) {
	cases := []struct {
		title string
		price *float64
		want  bool
	}{
		{"Control plafond pour Mercedes W204", listing.Num(250), true},
		{"4 pneus hiver pour Civic", listing.Num(400), true},
		{"Honda Civic 2005 à vendre", listing.Num(2500), false},
		// A $12 000 listing is a car even if it says "pneus inclus".
		{"Civic 2015 avec pneus hiver", listing.Num(12000), false},
		{"Jantes 17 pouces", nil, true},
		{"Pièces Civic", listing.Num(2999), true},
	}
	for _, c := range cases {
		if got := LooksLikePart(c.title, c.price); got != c.want {
			t.Errorf("LooksLikePart(%q, %s) = %v, want %v", c.title, showN(c.price), got, c.want)
		}
	}
}

func rawListing(overrides map[string]any) map[string]any {
	raw := map[string]any{
		"listingPublicId":        "100000001",
		"categoryCode":           "VEHICULES_AUTOS",
		"title":                  "Honda Civic Lx 2015 ,82 000km , automatique",
		"description":            "Belle voiture bien entretenue",
		"publicReleaseTimestamp": float64(1788298199000),
		"price":                  float64(12900),
		"priceLabel":             "12 900 $",
		"advertiserType":         "INDIVIDUAL",
		"cityLabel":              "Ville-Exemple",
		"distanceLabel":          "26 km",
		"listingDisplayUrl":      "https://listings.example.test/x/car_100000001.jsa",
		"mainImageUrl":           "https://img.example.test/binary/zoomedGallery/1.jpg",
		"images":                 []any{map[string]any{"formattableImageUrl": "https://img.example.test/binary/%FORMAT%/1.jpg"}},
		"characteristics": []any{
			map[string]any{"label": "Kilométrage", "value": "82 000 km"},
			map[string]any{"label": "Transmission", "value": "Automatique"},
		},
	}
	for k, v := range overrides {
		if v == nil {
			delete(raw, k)
		} else {
			raw[k] = v
		}
	}
	return raw
}

func TestNormalizeMapsIntoSharedSchema(t *testing.T) {
	l := Normalize(rawListing(nil), match)
	checks := []struct{ field, got, want string }{
		{"id", show(l.ID), "lespac:100000001"},
		{"referenceId", show(l.ReferenceID), "100000001"},
		{"source", l.Source, "lespac"},
		{"make", show(l.Make), "Honda"},
		{"model", show(l.Model), "Civic"},
		{"year", showN(l.Year), "2015"},
		{"km (structured, not the title)", showN(l.Km), "82000"},
		{"transmission", show(l.Transmission), "Automatique"},
		{"sellerType", show(l.SellerType), "PrivateSeller"},
		{"province", show(l.Province), "QC"},
		{"price (already dollars)", showN(l.Price), "12900"},
		{"distanceKm", showN(l.DistanceKm), "26"},
		{"listedAt", show(l.ListedAt), time.UnixMilli(1788298199000).UTC().Format("2006-01-02T15:04:05.000Z")},
		{"imageUrls[0]", l.ImageURLs[0], "https://img.example.test/binary/zoomedGallery/1.jpg"},
		{"imageCount", fmt.Sprint(l.ImageCount), "1"},
		{"city", show(l.City), "Ville-Exemple"},
		{"condition", show(l.Condition), "U"},
		{"resultType", show(l.ResultType), "Organic"},
		{"resultSection", show(l.ResultSection), "<nil>"},
		{"trimText", show(l.TrimText), l.Title},
		{"postalCode", show(l.PostalCode), "<nil>"},
		{"hadPriceDrop", fmt.Sprint(l.HadPriceDrop), "false"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
}

func TestNormalizeVariants(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]any
		check     func(Listing) (got, want string)
	}{
		{"native price-drop flag", map[string]any{"tag": "PRICE_REDUCTION"},
			func(l Listing) (string, string) { return fmt.Sprint(l.HadPriceDrop), "true" }},
		{"dealer listings", map[string]any{"advertiserType": "CORPORATE"},
			func(l Listing) (string, string) { return show(l.SellerType), "Dealer" }},
		{"price rounds half up", map[string]any{"price": 12899.5},
			func(l Listing) (string, string) { return showN(l.Price), "12900" }},
		{"non-numeric price is null", map[string]any{"price": "12 900 $"},
			func(l Listing) (string, string) { return showN(l.Price), "<nil>" }},
		{"numeric id", map[string]any{"listingPublicId": float64(42)},
			func(l Listing) (string, string) { return show(l.ID) + " " + show(l.ReferenceID), "lespac:42 42" }},
		{"missing id reads as JS would", map[string]any{"listingPublicId": nil},
			func(l Listing) (string, string) { return show(l.ID) + "|" + show(l.ReferenceID), "lespac:undefined|" }},
		{"no images falls back to the main image", map[string]any{"images": nil},
			func(l Listing) (string, string) { return fmt.Sprint(l.ImageCount, len(l.ImageURLs)), "1 0" }},
		{"no images at all", map[string]any{"images": nil, "mainImageUrl": nil},
			func(l Listing) (string, string) { return fmt.Sprint(l.ImageCount), "0" }},
		{"no timestamp", map[string]any{"publicReleaseTimestamp": nil},
			func(l Listing) (string, string) { return show(l.ListedAt), "<nil>" }},
		{"no characteristics", map[string]any{"characteristics": nil},
			func(l Listing) (string, string) { return showN(l.Km) + " " + show(l.Transmission), "<nil> <nil>" }},
		{"description HTML is stripped", map[string]any{"description": "Moteur <b>neuf</b><br>Aucune rouille"},
			func(l Listing) (string, string) { return show(l.Description), "Moteur neuf\nAucune rouille" }},
		{"description read for damage", map[string]any{"description": "Vendu pour pièces seulement"},
			func(l Listing) (string, string) { return fmt.Sprint(l.IsParts), "true" }},
		{"placement flag kept", map[string]any{"priorityPlacementLayoutFlag": "TOP"},
			func(l Listing) (string, string) { return show(l.ResultSection), "TOP" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, want := c.check(Normalize(rawListing(c.overrides), match))
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestNormalizeCapsImagesAtEight(t *testing.T) {
	var imgs []any
	for i := 0; i < 10; i++ {
		imgs = append(imgs, map[string]any{"formattableImageUrl": fmt.Sprintf("https://img.example.test/%%FORMAT%%/%d.jpg", i)})
	}
	imgs = append([]any{map[string]any{}}, imgs...) // one without a URL is filtered out
	l := Normalize(rawListing(map[string]any{"images": imgs}), match)
	if l.ImageCount != 11 || len(l.ImageURLs) != 8 || l.ImageURLs[0] != "https://img.example.test/zoomedGallery/0.jpg" {
		t.Fatalf("imageCount=%d urls=%v", l.ImageCount, l.ImageURLs)
	}
}

func TestNormalizeWithoutMatcher(t *testing.T) {
	l := Normalize(rawListing(nil), nil)
	if l.Make != nil || l.Model != nil || l.Year != nil {
		t.Fatalf("expected null vehicle, got %s|%s|%s", show(l.Make), show(l.Model), showN(l.Year))
	}
}

func renderPage(listings []map[string]any, total, pages any) string {
	body := map[string]any{"searchResults": listings, "totalResults": total, "totalPages": pages}
	b, _ := json.Marshal(body)
	return "<html><script>var searchResponse = " + string(b) + ";\n</script></html>"
}

func TestParsePage(t *testing.T) {
	t.Run("reads the embedded searchResponse", func(t *testing.T) {
		p, err := ParsePage(renderPage([]map[string]any{rawListing(nil)}, 312, 16), match)
		if err != nil {
			t.Fatal(err)
		}
		if showN(p.Total) != "312" || showN(p.TotalPages) != "16" || len(p.Listings) != 1 {
			t.Fatalf("total=%s pages=%s n=%d", showN(p.Total), showN(p.TotalPages), len(p.Listings))
		}
	})
	t.Run("drops parts before they reach the comps", func(t *testing.T) {
		p, err := ParsePage(renderPage([]map[string]any{
			rawListing(nil),
			rawListing(map[string]any{"listingPublicId": "9", "title": "Control plafond pour Mercedes W204", "price": float64(250)}),
		}, 312, 16), match)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Listings) != 1 || p.Skipped != 1 {
			t.Fatalf("listings=%d skipped=%d", len(p.Listings), p.Skipped)
		}
	})
	t.Run("missing totals are null", func(t *testing.T) {
		p, err := ParsePage(renderPage(nil, nil, nil), match)
		if err != nil {
			t.Fatal(err)
		}
		if p.Total != nil || p.TotalPages != nil || len(p.Listings) != 0 {
			t.Fatalf("got %+v", p)
		}
	})
	for _, bad := range []string{"<html>denied</html>", "", "var searchResponse = {not json};\n"} {
		t.Run("throws a ParseError on "+fmt.Sprintf("%q", bad), func(t *testing.T) {
			_, err := ParsePage(bad, nil)
			var pe *fetch.ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("want ParseError, got %v", err)
			}
			if !fetch.Answered(err) {
				t.Fatal("a parse error means the site answered")
			}
		})
	}
}

func TestBuildURL(t *testing.T) {
	q := func(o URLOptions) url.Values {
		u, err := url.Parse(BuildURL(o))
		if err != nil {
			t.Fatal(err)
		}
		return u.Query()
	}
	cases := []struct {
		name  string
		opts  URLOptions
		key   string
		want  string
		unset bool
	}{
		{"filters to private sellers", URLOptions{AdvertiserType: AdvertiserPrivate}, "advertiserType", "INDIVIDUAL", false},
		{"cars category", URLOptions{}, "categoryId", "286", false},
		{"no seller filter by default", URLOptions{}, "advertiserType", "", true},
		{"a real distance radius", URLOptions{MaxDistance: 300}, "maxDistance", "300", false},
		{"default radius", URLOptions{}, "maxDistance", "200", false},
		{"yearMin", URLOptions{YearMin: listing.Num(2006), YearMax: listing.Num(2011)}, "yearMin", "2006", false},
		{"yearMax", URLOptions{YearMin: listing.Num(2006), YearMax: listing.Num(2011)}, "yearMax", "2011", false},
		{"never the inert year= form", URLOptions{YearMin: listing.Num(2006), YearMax: listing.Num(2011)}, "year", "", true},
		{"paginates", URLOptions{Page: 4}, "pageNumber", "4", false},
		{"first page by default", URLOptions{}, "pageNumber", "1", false},
		{"custom place", URLOptions{Place: &Place{Latitude: 46.8, Longitude: -71.2, GeographicAreaID: 1, RegionID: 2, GroupID: 3}}, "latitude", "46.8", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := q(c.opts)
			if c.unset {
				if v.Has(c.key) {
					t.Fatalf("%s should be absent, got %q", c.key, v.Get(c.key))
				}
				return
			}
			if got := v.Get(c.key); got != c.want {
				t.Fatalf("%s = %q, want %q", c.key, got, c.want)
			}
		})
	}
}

func TestBuildURLExact(t *testing.T) {
	// Same string URLSearchParams produces: insertion order, spaces as '+'.
	want := "https://www.lespac.com/search/results.jsa?pageNumber=2&pageSize=20&latitude=45.525" +
		"&longitude=-73.558333333&cityLocation=true&geographicAreaId=17567&sortOrder=Distance+asc" +
		"&viewType=detailed&localisationMode=LOCATIONS&maxDistance=200&categoryId=286&domCatId=286" +
		"&domGroupId=285&groupId=285&regionId=17373&lang=fr&advertiserType=INDIVIDUAL&yearMin=2006"
	got := BuildURL(URLOptions{AdvertiserType: AdvertiserPrivate, Page: 2, YearMin: listing.Num(2006)})
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

// fakeFetcher serves pages by pageNumber. Never the network.
type fakeFetcher struct {
	pages map[int]string
	urls  []string
	err   error
}

func (f *fakeFetcher) Fetch(_ context.Context, u string) (string, error) {
	f.urls = append(f.urls, u)
	if f.err != nil {
		return "", f.err
	}
	parsed, _ := url.Parse(u)
	var n int
	fmt.Sscan(parsed.Query().Get("pageNumber"), &n)
	body, ok := f.pages[n]
	if !ok {
		return "", fmt.Errorf("unexpected page %d", n)
	}
	return body, nil
}

func cars(prefix string, n int) []map[string]any {
	var out []map[string]any
	for i := 0; i < n; i++ {
		out = append(out, rawListing(map[string]any{"listingPublicId": fmt.Sprintf("%s%d", prefix, i)}))
	}
	return out
}

func TestSearch(t *testing.T) {
	part := rawListing(map[string]any{"listingPublicId": "p1", "title": "Kit pour Civic", "price": float64(100)})
	cases := []struct {
		name        string
		pages       map[int]string
		opts        []Option
		query       listing.Query
		wantWalked  int
		wantN       int
		wantShort   int
		wantTrunc   bool
		wantParts   int
		wantStopped bool
	}{
		{
			name: "stops at the site's last page",
			pages: map[int]string{
				1: renderPage(cars("a", 20), 25, 2),
				2: renderPage(cars("b", 5), 25, 2),
			},
			wantWalked: 2, wantN: 25,
		},
		{
			name: "stops on an empty page",
			pages: map[int]string{
				1: renderPage(cars("a", 20), nil, nil),
				2: renderPage(nil, nil, nil),
			},
			wantWalked: 2, wantN: 20,
		},
		{
			name: "parts count toward the total, not the shortfall",
			pages: map[int]string{
				1: renderPage(append(cars("a", 3), part), 4, 1),
			},
			wantWalked: 1, wantN: 3, wantParts: 1,
		},
		{
			name: "coming up short of the total is truncation",
			pages: map[int]string{
				1: renderPage(cars("a", 20), 45, 3),
				2: renderPage(cars("b", 20), 45, 3),
			},
			query:      listing.Query{MaxPages: 2},
			wantWalked: 2, wantN: 40, wantShort: 5, wantTrunc: true,
		},
		{
			name: "duplicates across pages are stored once",
			pages: map[int]string{
				1: renderPage(cars("a", 3), 6, 2),
				2: renderPage(cars("a", 3), 6, 2),
			},
			wantWalked: 2, wantN: 3, wantShort: 3, wantTrunc: true,
		},
		{
			name: "a new-listings run stops after three quiet pages",
			pages: map[int]string{
				1: renderPage(cars("new", 2), 200, 10),
				2: renderPage(cars("old", 2), 200, 10),
				3: renderPage(cars("old", 2), 200, 10),
				4: renderPage(cars("old", 2), 200, 10),
			},
			opts:       []Option{WithKnown(func(id string) bool { return strings.HasPrefix(id, "lespac:old") })},
			wantWalked: 4, wantN: 4, wantShort: 196, wantTrunc: true, wantStopped: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeFetcher{pages: c.pages}
			s := New(f, append([]Option{WithMatcher(match)}, c.opts...)...)
			var events []source.PageEvent
			w, err := s.WalkAll(context.Background(), c.query, func(e source.PageEvent) { events = append(events, e) })
			if err != nil {
				t.Fatal(err)
			}
			if w.PagesWalked != c.wantWalked || len(w.Listings) != c.wantN || w.Shortfall != c.wantShort ||
				w.SkippedParts != c.wantParts || w.StoppedEarly != c.wantStopped {
				t.Fatalf("walked=%d n=%d short=%d parts=%d stopped=%v", w.PagesWalked, len(w.Listings), w.Shortfall, w.SkippedParts, w.StoppedEarly)
			}
			if len(events) != c.wantWalked {
				t.Fatalf("%d progress events, want %d", len(events), c.wantWalked)
			}

			f.urls = nil
			res, err := s.Search(context.Background(), c.query, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Listings) != c.wantN || res.Truncated != c.wantTrunc || res.Shortfall != c.wantShort {
				t.Fatalf("result n=%d truncated=%v shortfall=%d", len(res.Listings), res.Truncated, res.Shortfall)
			}
			if res.URL != f.urls[0] || res.Listings[0].Source != "lespac" {
				t.Fatalf("url=%s source=%s", res.URL, res.Listings[0].Source)
			}
		})
	}
}

func TestNoveltyCountsNewIDs(t *testing.T) {
	f := &fakeFetcher{pages: map[int]string{1: renderPage(cars("x", 3), 3, 1)}}
	s := New(f, WithKnown(func(id string) bool { return id == "lespac:x0" }))
	w, err := s.WalkAll(context.Background(), listing.Query{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w.NewIDs == nil || *w.NewIDs != 2 {
		t.Fatalf("newIds = %v", w.NewIDs)
	}
	if w2, _ := New(f).WalkAll(context.Background(), listing.Query{}, nil); w2.NewIDs != nil {
		t.Fatal("a full run reports no newIds")
	}
}

func TestSearchQueryMapping(t *testing.T) {
	cases := []struct {
		name  string
		opts  []Option
		query listing.Query
		want  map[string]string // "" = absent
	}{
		{"private by default", nil, listing.Query{}, map[string]string{"advertiserType": "INDIVIDUAL", "maxDistance": "200"}},
		{"dealer query", nil, listing.Query{SellerType: "D"}, map[string]string{"advertiserType": "CORPORATE"}},
		{"both sellers when configured", []Option{WithAdvertiserType("")}, listing.Query{}, map[string]string{"advertiserType": ""}},
		{"private query overrides a dealer default", []Option{WithAdvertiserType(AdvertiserDealer)}, listing.Query{SellerType: "P"}, map[string]string{"advertiserType": "INDIVIDUAL"}},
		{"radius option", []Option{WithMaxDistance(50)}, listing.Query{}, map[string]string{"maxDistance": "50"}},
		{"years go to the site", nil, listing.Query{YearFrom: listing.Num(2006), YearTo: listing.Num(2011)}, map[string]string{"yearMin": "2006", "yearMax": "2011"}},
		{"place option", []Option{WithPlace(Place{Latitude: 46.81, Longitude: -71.21, GeographicAreaID: 7, RegionID: 8, GroupID: 9})}, listing.Query{}, map[string]string{"latitude": "46.81", "regionId": "8", "groupId": "9"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeFetcher{pages: map[int]string{1: renderPage(nil, 0, 0)}}
			if _, err := New(f, c.opts...).Search(context.Background(), c.query, nil); err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(f.urls[0])
			v := u.Query()
			for k, want := range c.want {
				if want == "" {
					if v.Has(k) {
						t.Errorf("%s should be absent", k)
					}
				} else if got := v.Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestSearchPropagatesErrors(t *testing.T) {
	boom := &fetch.HTTPError{Status: 503, URL: "x"}
	if _, err := New(&fakeFetcher{err: boom}).Search(context.Background(), listing.Query{}, nil); !errors.Is(err, boom) {
		t.Fatalf("fetch error: %v", err)
	}
	f := &fakeFetcher{pages: map[int]string{1: "<html>blocked</html>"}}
	var pe *fetch.ParseError
	if _, err := New(f).Search(context.Background(), listing.Query{}, nil); !errors.As(err, &pe) {
		t.Fatalf("parse error: %v", err)
	}
}

func TestNameAndGeoSlug(t *testing.T) {
	s := New(fetch.Static{})
	if s.Name() != "lespac" || s.GeoSlug() != "lespac-200km" || New(nil, WithMaxDistance(300)).GeoSlug() != "lespac-300km" {
		t.Fatalf("name=%s geo=%s", s.Name(), s.GeoSlug())
	}
}

func TestListingJSONKeepsExtras(t *testing.T) {
	b, err := json.Marshal(Normalize(rawListing(map[string]any{"tag": "PRICE_REDUCTION"}), match))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["id"] != "lespac:100000001" || m["hadPriceDrop"] != true || m["distanceKm"] != float64(26) || m["title"] == nil {
		t.Fatalf("json = %s", b)
	}
}
