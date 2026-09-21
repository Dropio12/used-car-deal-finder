package kijiji

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/parse"
	"carbuyer/crawler/internal/source"
)

// All fixtures below are synthetic: made-up ids, placeholder VIN, and
// street/postal pairs that do not name a real address.

// rawListing is one listing in the raw shape Kijiji emits. attrs override (or,
// with a nil value, remove) attributes; overrides replace top-level fields.
func rawListing(attrs map[string]any, overrides map[string]any) map[string]any {
	base := map[string]any{
		"caryear": "2019", "carmake": "audi", "carmodel": "a3", "cartrim": "Komfort",
		"carmileageinkms": "60300", "cartransmission": "2", "carfueltype": "gasoline",
		"forsaleby": "ownr", "vehicletype": "used",
	}
	for k, v := range attrs {
		base[k] = v
	}
	all := []any{}
	for _, k := range []string{"caryear", "carmake", "carmodel", "cartrim", "carmileageinkms",
		"cartransmission", "carfueltype", "forsaleby", "vehicletype", "vin", "pricerating"} {
		if v, ok := base[k]; ok && v != nil {
			all = append(all, map[string]any{"canonicalName": k, "canonicalValues": []any{v}})
		}
	}
	raw := map[string]any{
		"__typename":     "AutosListing",
		"id":             "1000000001",
		"title":          "2019 Audi A3 Komfort",
		"description":    "Belle voiture<br />bien entretenue",
		"imageCount":     6.0,
		"url":            "https://www.kijiji.ca/v-cars-trucks/x/1000000001",
		"activationDate": "2026-08-25T03:48:58.000Z",
		"sortingDate":    "2026-08-25T03:48:58.000Z",
		"adSource":       "ORGANIC",
		"location": map[string]any{
			"address":     "Pointe-Claire, QC H9R 1A1",
			"coordinates": map[string]any{"latitude": 45.44, "longitude": -73.81},
		},
		"price":      map[string]any{"type": "FIXED", "amount": 2099900.0, "msrp": nil, "classification": map[string]any{"rating": "FAIR"}},
		"flags":      map[string]any{"priceDrop": false},
		"posterInfo": map[string]any{"posterId": "PRIVATE_123"},
		"attributes": map[string]any{"all": all},
	}
	for k, v := range overrides {
		raw[k] = v
	}
	return raw
}

// roundTrip passes a raw listing through JSON, as ParsePage would see it.
func roundTrip(t *testing.T, raw map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func normalize(t *testing.T, attrs, overrides map[string]any) (listing.Listing, Extra) {
	t.Helper()
	return Normalize(roundTrip(t, rawListing(attrs, overrides)))
}

// renderPage builds a results page. Listing keys are written in the given
// order, which matters: Object.keys order is what the JS walks.
func renderPage(listings []map[string]any, totalCount, limit, offset float64) string {
	var b strings.Builder
	b.WriteString(`{"props":{"pageProps":{"__APOLLO_STATE__":{"ROOT_QUERY":{"searchResultsPageByUrl:/b-cars-trucks/x":{"pagination":`)
	p, _ := json.Marshal(map[string]any{"totalCount": totalCount, "limit": limit, "offset": offset})
	b.Write(p)
	b.WriteString(`}}`)
	for _, l := range listings {
		v, _ := json.Marshal(l)
		fmt.Fprintf(&b, `,"AutosListing:%v":%s`, l["id"], v)
	}
	b.WriteString(`}}}}`)
	return `<html><script id="__NEXT_DATA__" type="application/json">` + b.String() + `</script></html>`
}

func deref(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func derefNum(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestNormalize(t *testing.T) {
	l, extra := normalize(t, map[string]any{"vin": "TESTVIN0000000001"}, nil)
	cases := []struct {
		name string
		got  any
		want any
	}{
		// 2099900 is $20 999, not $2 099 900.
		{"price in dollars not cents", derefNum(l.Price), 20999.0},
		{"id namespaced", deref(l.ID), "kijiji:1000000001"},
		{"referenceId", deref(l.ReferenceID), "1000000001"},
		{"source", l.Source, "kijiji"},
		{"year", derefNum(l.Year), 2019.0},
		{"make", deref(l.Make), "Audi"},
		{"model", deref(l.Model), "A3"},
		{"trim", deref(l.TrimText), "Komfort"},
		{"km", derefNum(l.Km), 60300.0},
		{"seller", deref(l.SellerType), "PrivateSeller"},
		{"transmission", deref(l.Transmission), "Automatique"},
		{"fuel", deref(l.Fuel), "Essence"},
		{"condition", deref(l.Condition), "U"},
		{"city", deref(l.City), "Pointe-Claire"},
		{"province", deref(l.Province), "QC"},
		{"postal", deref(l.PostalCode), "H9R1A1"},
		{"sellerId", deref(l.SellerID), "PRIVATE_123"},
		{"msrp absent", derefNum(l.SuggestedRetailPrice), nil},
		{"fixed price is not conditional", l.IsConditionalPrice, false},
		{"organic", deref(l.ResultType), "Organic"},
		{"section", deref(l.ResultSection), "ORGANIC"},
		{"image count", l.ImageCount, 6},
		// Fields AutoHebdo never had.
		{"listedAt", deref(extra.ListedAt), "2026-08-25T03:48:58.000Z"},
		{"latitude", derefNum(extra.Latitude), 45.44},
		{"priceRating", deref(extra.PriceRating), "FAIR"},
		{"vin", deref(extra.VIN), "TESTVIN0000000001"},
		{"no price drop", extra.HadPriceDrop, false},
		{"title", deref(extra.Title), "2019 Audi A3 Komfort"},
		// Decoded to plain text.
		{"description", deref(l.Description), "Belle voiture\nbien entretenue"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %#v, want %#v", c.name, c.got, c.want)
		}
	}
}

func TestNormalizeVariants(t *testing.T) {
	cases := []struct {
		name      string
		attrs     map[string]any
		overrides map[string]any
		check     func(listing.Listing, Extra) (any, any)
	}{
		{"dealer listings map too", map[string]any{"forsaleby": "delr"}, nil,
			func(l listing.Listing, _ Extra) (any, any) { return deref(l.SellerType), "Dealer" }},
		{"new cars flagged", map[string]any{"vehicletype": "new"}, nil,
			func(l listing.Listing, _ Extra) (any, any) { return IsUsed(l), false }},
		{"used cars kept", nil, nil,
			func(l listing.Listing, _ Extra) (any, any) { return IsUsed(l), true }},
		{"placeholder make rejected", map[string]any{"carmake": "Othrmake"}, nil,
			func(l listing.Listing, _ Extra) (any, any) { return deref(l.Make), nil }},
		{"placeholder model rejected", map[string]any{"carmodel": "Othrmdl"}, nil,
			func(l listing.Listing, _ Extra) (any, any) { return deref(l.Model), nil }},
		{"make is canonical (bmw)", map[string]any{"carmake": "bmw"}, nil,
			func(l listing.Listing, _ Extra) (any, any) { return deref(l.Make), "BMW" }},
		{"make is canonical (volkwagen)", map[string]any{"carmake": "volkwagen"}, nil,
			func(l listing.Listing, _ Extra) (any, any) { return deref(l.Make), "Volkswagen" }},
		{"trim recovered from title", map[string]any{"cartrim": nil}, nil,
			func(l listing.Listing, _ Extra) (any, any) { return deref(l.TrimText), "Komfort" }},
		{"manual transmission", map[string]any{"cartransmission": "1"}, nil,
			func(l listing.Listing, _ Extra) (any, any) { return deref(l.Transmission), "Manuelle" }},
		{"unknown fuel title-cased", map[string]any{"carfueltype": "hydrogen"}, nil,
			func(l listing.Listing, _ Extra) (any, any) { return deref(l.Fuel), "Hydrogen" }},
		{"other fuel is nothing", map[string]any{"carfueltype": "other"}, nil,
			func(l listing.Listing, _ Extra) (any, any) { return deref(l.Fuel), nil }},
		{"non-fixed price is conditional", nil, map[string]any{"price": map[string]any{"type": "CONTACT", "amount": 100.0}},
			func(l listing.Listing, _ Extra) (any, any) { return l.IsConditionalPrice, true }},
		{"msrp in dollars", nil, map[string]any{"price": map[string]any{"type": "FIXED", "amount": 100.0, "msrp": 3664322.0}},
			func(l listing.Listing, _ Extra) (any, any) { return derefNum(l.SuggestedRetailPrice), 36643.0 }},
		{"price rating falls back to attribute", map[string]any{"pricerating": "GOOD"}, map[string]any{"price": map[string]any{"amount": 100.0}},
			func(_ listing.Listing, e Extra) (any, any) { return deref(e.PriceRating), "GOOD" }},
		{"listedAt falls back to sortingDate", nil, map[string]any{"activationDate": nil},
			func(_ listing.Listing, e Extra) (any, any) { return deref(e.ListedAt), "2026-08-25T03:48:58.000Z" }},
		{"price drop flag", nil, map[string]any{"flags": map[string]any{"priceDrop": true}},
			func(_ listing.Listing, e Extra) (any, any) { return e.HadPriceDrop, true }},
		{"paid placement is promoted", nil, map[string]any{"adSource": "TOP_AD"},
			func(l listing.Listing, _ Extra) (any, any) { return deref(l.ResultType), "Promoted" }},
		{"missing id", nil, map[string]any{"id": nil},
			func(l listing.Listing, _ Extra) (any, any) {
				return fmt.Sprint(deref(l.ID), "|", deref(l.ReferenceID)), "kijiji:undefined|"
			}},
		{"images upgraded and capped", nil, map[string]any{"imageUrls": []any{
			"https://img.example/a.jpg?rule=kijijica-200-jpg", "b", "c", "d", "e", "f", "g", "h", "i"}},
			func(l listing.Listing, _ Extra) (any, any) {
				return fmt.Sprint(len(l.ImageURLs), " ", l.ImageURLs[0]), "8 https://img.example/a.jpg?rule=kijijica-960-jpg"
			}},
		// Damage reaches the listing, not just the printout.
		{"disclosed damage", nil, map[string]any{"description": "Moteur brisé, vendu pour les pièces"},
			func(l listing.Listing, _ Extra) (any, any) {
				return [2]bool{l.IsDamaged, l.IsParts}, [2]bool{true, true}
			}},
		{"praise is not damage", nil, map[string]any{"description": "Aucune rouille, jamais accidentée"},
			func(l listing.Listing, _ Extra) (any, any) {
				return [2]bool{l.IsDamaged, l.IsParts}, [2]bool{false, false}
			}},
		{"rust is not a parts car", nil, map[string]any{"description": "Belle voiture, un peu de rouille"},
			func(l listing.Listing, _ Extra) (any, any) { return l.IsParts, false }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, want := c.check(normalize(t, c.attrs, c.overrides))
			if got != want {
				t.Errorf("got %#v, want %#v", got, want)
			}
		})
	}
}

func TestCanonicalMake(t *testing.T) {
	// Every case is a value observed in live Kijiji data; if these disagree
	// with AutoHebdo, a Kijiji car can never be priced against dealer comps.
	cases := []struct{ in, want any }{
		{"volkwagen", "Volkswagen"}, {"bmw", "BMW"}, {"gmc", "GMC"}, {"ram", "RAM"},
		{"mercedes", "Mercedes-Benz"}, {"mercedes-amg", "Mercedes-Benz"},
		{"landrover", "Land Rover"}, {"alpharomeo", "Alfa Romeo"}, {"mini", "MINI"},
		{"toyota", "Toyota"}, {"honda", "Honda"}, {"Land Rover", "Land Rover"},
		{"Othrmake", nil}, {"", nil},
	}
	for _, c := range cases {
		if got := deref(CanonicalMake(c.in.(string))); got != c.want {
			t.Errorf("CanonicalMake(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestTitleCase(t *testing.T) {
	cases := []struct{ in, want any }{
		{"chevrolet", "Chevrolet"}, {"mercedes-benz", "Mercedes-Benz"}, {"Model_3", "Model 3"},
		{"ÉLAN", "Élan"}, {"other", nil}, {"", nil},
	}
	for _, c := range cases {
		if got := deref(TitleCase(c.in.(string))); got != c.want {
			t.Errorf("TitleCase(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestParseAddress(t *testing.T) {
	// Same shapes as the real strings from one results page; streets and
	// postal codes are made up.
	cases := []struct{ address, city, province, postal any }{
		{"Verdun, QC H4G 1A1", "Verdun", "QC", "H4G1A1"},
		{"Chemin Exemple, Pierrefonds, QC", "Pierrefonds", "QC", nil},
		{"Montreal, QC H2G", "Montreal", "QC", "H2G"},
		// No province stated, but the postal code's first letter gives it away.
		{"Av Exemple, Senneville, H9X 1A1", "Senneville", "QC", "H9X1A1"},
		{"Rue Exemple, Montréal, H1X 1A1", "Montréal", "QC", "H1X1A1"},
		{"Notre-Dame-de-l'Île-Perrot, QC J7V 1A1", "Notre-Dame-de-l'Île-Perrot", "QC", "J7V1A1"},
		{"Rue A / Rue B, Montréal, H3N 1A1", "Montréal", "QC", "H3N1A1"},
		{"Montreal, QC, H4P 1A1", "Montreal", "QC", "H4P1A1"},
		{"Dorval, QC, H8S 1a1", "Dorval", "QC", "H8S1A1"},
		// Never a street for a city.
		{"Terr. Exemple, Montréal, H1S 1A1", "Montréal", "QC", "H1S1A1"},
		// Province + postal, no city.
		{"QC J7V 1A1", nil, "QC", "J7V1A1"},
		// A stated province still wins over the derived one.
		{"Ottawa, ON K1A 0B1", "Ottawa", "ON", "K1A0B1"},
	}
	for _, c := range cases {
		a := c.address.(string)
		got := ParseAddress(&a)
		if deref(got.City) != c.city || deref(got.Province) != c.province || deref(got.PostalCode) != c.postal {
			t.Errorf("ParseAddress(%q) = {%v %v %v}, want {%v %v %v}", a,
				deref(got.City), deref(got.Province), deref(got.PostalCode), c.city, c.province, c.postal)
		}
	}
	for _, empty := range []*string{nil, listing.Str(""), listing.Str("   ")} {
		if got := ParseAddress(empty); got != (Address{}) {
			t.Errorf("ParseAddress(%v) = %+v, want empty", empty, got)
		}
	}
}

func TestProvinceFromPostalCode(t *testing.T) {
	cases := []struct{ code, want any }{
		{"H2X2L5", "QC"}, {"G1V", "QC"}, {"J7V7P2", "QC"},
		{"K1A0B1", "ON"}, {"L4C", "ON"}, {"M5V2T6", "ON"}, {"N2L", "ON"}, {"P7B", "ON"}, {"m5v", "ON"},
		{"T2P", "AB"}, {"V6B", "BC"}, {"B3J", "NS"},
		// Null rather than a guess on junk; D is not an assigned prefix.
		{"", nil}, {"12345", nil}, {"D1A", nil},
	}
	for _, c := range cases {
		if got := deref(ProvinceFromPostalCode(c.code.(string))); got != c.want {
			t.Errorf("ProvinceFromPostalCode(%q) = %#v, want %#v", c.code, got, c.want)
		}
	}
}

func TestTrimFromTitle(t *testing.T) {
	cases := []struct {
		title       *string
		year        *float64
		make, model *string
		want        any
	}{
		{listing.Str("2019 Audi A3 Komfort"), listing.Num(2019), listing.Str("Audi"), listing.Str("A3"), "Komfort"},
		// A model whose canonical form came from an underscore.
		{listing.Str("2021 Tesla Model 3 RWD"), listing.Num(2021), listing.Str("Tesla"), listing.Str("Model 3"), "RWD"},
		{listing.Str("2021 Tesla Model_3 RWD"), listing.Num(2021), listing.Str("Tesla"), listing.Str("Model 3"), "RWD"},
		{listing.Str("2019 Audi A3"), listing.Num(2019), listing.Str("Audi"), listing.Str("A3"), nil},
		{listing.Str("Mazda 3 GS 2015"), nil, listing.Str("Mazda"), listing.Str("3"), "GS 2015"},
		{nil, nil, nil, nil, nil},
	}
	for _, c := range cases {
		if got := deref(TrimFromTitle(c.title, c.year, c.make, c.model)); got != c.want {
			t.Errorf("TrimFromTitle(%v) = %#v, want %#v", deref(c.title), got, c.want)
		}
	}
}

func TestParsePage(t *testing.T) {
	t.Run("reads the Apollo cache and the total", func(t *testing.T) {
		p, err := ParsePage(renderPage([]map[string]any{rawListing(nil, nil)}, 1138, 40, 0))
		if err != nil {
			t.Fatal(err)
		}
		if derefNum(p.Total) != 1138.0 || len(p.Listings) != 1 || deref(p.Listings[0].Make) != "Audi" {
			t.Fatalf("got total=%v listings=%d", derefNum(p.Total), len(p.Listings))
		}
		if p.PageSize != 40 || p.Offset != 0 {
			t.Errorf("pagination = %v/%v", p.PageSize, p.Offset)
		}
		if deref(p.Extras["kijiji:1000000001"].PriceRating) != "FAIR" {
			t.Errorf("extras not keyed by id: %+v", p.Extras)
		}
	})

	t.Run("separates paid placements and keeps key order", func(t *testing.T) {
		p, err := ParsePage(renderPage([]map[string]any{
			rawListing(nil, map[string]any{"id": "3", "adSource": "ORGANIC"}),
			rawListing(nil, map[string]any{"id": "2", "adSource": "TOP_AD"}),
			rawListing(nil, map[string]any{"id": "1", "adSource": "PROV_TOP_AD"}),
			rawListing(nil, map[string]any{"id": "0", "adSource": "ORGANIC"}),
		}, 10, 40, 0))
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Listings) != 2 || p.PromotedCount != 2 || len(p.AllListings) != 4 {
			t.Fatalf("organic=%d promoted=%d all=%d", len(p.Listings), p.PromotedCount, len(p.AllListings))
		}
		if deref(p.AllListings[0].ID) != "kijiji:3" || deref(p.Listings[1].ID) != "kijiji:0" {
			t.Errorf("order lost: %v, %v", deref(p.AllListings[0].ID), deref(p.Listings[1].ID))
		}
	})

	t.Run("defaults when pagination is absent", func(t *testing.T) {
		html := `<script id="__NEXT_DATA__">{"props":{"pageProps":{"__APOLLO_STATE__":{"ROOT_QUERY":{}}}}}</script>`
		p, err := ParsePage(html)
		if err != nil {
			t.Fatal(err)
		}
		if p.Total != nil || p.PageSize != 40 || p.Offset != 0 || len(p.Listings) != 0 {
			t.Errorf("got %+v", p)
		}
	})

	errCases := []struct{ name, html, want string }{
		{"empty body", "", "empty response body"},
		{"block page", "<html>denied</html>", "__NEXT_DATA__ not found"},
		{"bad json", `<script id="__NEXT_DATA__">{nope</script>`, "not valid JSON"},
		{"no apollo", `<html><script id="__NEXT_DATA__">{"props":{"pageProps":{}}}</script></html>`, "__APOLLO_STATE__"},
		{"null apollo", `<script id="__NEXT_DATA__">{"props":{"pageProps":{"__APOLLO_STATE__":null}}}</script>`, "__APOLLO_STATE__"},
	}
	for _, c := range errCases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParsePage(c.html)
			var pe *parse.ParseError
			if !errors.As(err, &pe) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want ParseError containing %q", err, c.want)
			}
		})
	}
}

func TestBuildURL(t *testing.T) {
	cases := []struct {
		name   string
		geo    string
		seller string
		page   int
		want   string
	}{
		{"montreal", "montreal", "", 1, "https://www.kijiji.ca/b-cars-trucks/ville-de-montreal/c174l1700281"},
		{"quebec private", "quebec", "ownr", 1, "https://www.kijiji.ca/b-cars-trucks/quebec/c174l9001?for-sale-by=ownr"},
		{"ontario page 3", "ontario", "", 3, "https://www.kijiji.ca/b-cars-trucks/ontario/page-3/c174l9004"},
		{"page 1 not in path", "montreal", "delr", 1, "https://www.kijiji.ca/b-cars-trucks/ville-de-montreal/c174l1700281?for-sale-by=delr"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BuildURL(Geos[c.geo], c.seller, c.page)
			if got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
			// vehicle-type is accepted and ignored by the site: never send it.
			u, _ := url.Parse(got)
			if u.Query().Has("vehicle-type") {
				t.Error("sent vehicle-type")
			}
		})
	}
}

func TestResolveGeo(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "quebec"}, {"reg_qc", "quebec"}, {"quebec", "quebec"}, {"QC", "quebec"},
		{"ontario", "ontario"}, {"reg_on", "ontario"}, {"ON", "ontario"},
		{"montreal", "montreal"}, {"cit_montreal", "montreal"}, {"quebeccity", "quebecCity"},
		{"ottawaGatineau", "ottawaGatineau"},
	}
	for _, c := range cases {
		key, _, err := ResolveGeo(c.in)
		if err != nil || key != c.want {
			t.Errorf("ResolveGeo(%q) = %q, %v; want %q", c.in, key, err, c.want)
		}
	}
	if _, _, err := ResolveGeo("atlantis"); err == nil {
		t.Error("unknown geo accepted")
	}
}

// fakeFetcher serves canned pages by URL and records requests. Never the network.
type fakeFetcher struct {
	pages map[string]string
	calls []string
}

func (f *fakeFetcher) Fetch(_ context.Context, u string) (string, error) {
	f.calls = append(f.calls, u)
	body, ok := f.pages[u]
	if !ok {
		return "", fmt.Errorf("unexpected url %s", u)
	}
	return body, nil
}

// cars builds n used organic listings with ids prefix-0..n-1.
func cars(prefix string, n int, attrs map[string]any) []map[string]any {
	out := []map[string]any{}
	for i := 0; i < n; i++ {
		out = append(out, rawListing(attrs, map[string]any{"id": fmt.Sprintf("%s-%d", prefix, i)}))
	}
	return out
}

func qcURL(page int) string { return BuildURL(Geos["quebec"], "ownr", page) }

func TestSearchWalksUntilTotal(t *testing.T) {
	// 5 listings, 2 per page: pages 1..3, stopping on offset+limit >= total.
	f := &fakeFetcher{pages: map[string]string{
		qcURL(1): renderPage(append(cars("a", 2, nil), rawListing(nil, map[string]any{"id": "ad", "adSource": "TOP_AD"})), 5, 2, 0),
		qcURL(2): renderPage(cars("b", 2, nil), 5, 2, 2),
		qcURL(3): renderPage(cars("c", 1, nil), 5, 2, 4),
	}}
	var events []source.PageEvent
	s := New(f)
	if s.Name() != "kijiji" {
		t.Fatalf("name %q", s.Name())
	}
	res, err := s.Search(context.Background(), listing.Query{SellerType: "P"}, func(e source.PageEvent) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Listings) != 5 || res.PagesWalked != 3 || res.Injected != 1 || res.Truncated || res.Shortfall != 0 {
		t.Fatalf("got %d listings, %d pages, %d injected, truncated=%v shortfall=%d",
			len(res.Listings), res.PagesWalked, res.Injected, res.Truncated, res.Shortfall)
	}
	if res.URL != qcURL(1) || derefNum(res.Total) != 5.0 || derefNum(res.Pages) != 3.0 {
		t.Errorf("url=%s total=%v pages=%v", res.URL, derefNum(res.Total), derefNum(res.Pages))
	}
	if len(events) != 3 || events[2].Of != 3 || events[0].Found != 2 {
		t.Errorf("events = %+v", events)
	}
}

func TestSearchGeoAndSeller(t *testing.T) {
	cases := []struct {
		name string
		q    listing.Query
		want string
	}{
		{"default is Québec, all sellers", listing.Query{}, "https://www.kijiji.ca/b-cars-trucks/quebec/c174l9001"},
		{"Ontario private", listing.Query{Geo: "ontario", SellerType: "P"}, "https://www.kijiji.ca/b-cars-trucks/ontario/c174l9004?for-sale-by=ownr"},
		{"Ontario region token, dealers", listing.Query{Geo: "reg_on", SellerType: "D"}, "https://www.kijiji.ca/b-cars-trucks/ontario/c174l9004?for-sale-by=delr"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeFetcher{pages: map[string]string{c.want: renderPage(cars("x", 1, nil), 1, 40, 0)}}
			if _, err := New(f).Search(context.Background(), c.q, nil); err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 1 || f.calls[0] != c.want {
				t.Errorf("calls = %v", f.calls)
			}
		})
	}
}

func TestSearchRefusesWhatKijijiCannotFilter(t *testing.T) {
	cases := []listing.Query{
		{Make: "toyota"}, {Model: "rav4"}, {PriceFrom: listing.Num(1000)}, {PriceTo: listing.Num(9000)},
		{Geo: "atlantis"}, {SellerType: "X"},
	}
	for _, q := range cases {
		f := &fakeFetcher{}
		if _, err := New(f).Search(context.Background(), q, nil); err == nil || len(f.calls) != 0 {
			t.Errorf("%+v: err=%v calls=%d", q, err, len(f.calls))
		}
	}
}

func TestSearchUsedOnly(t *testing.T) {
	pages := func() *fakeFetcher {
		return &fakeFetcher{pages: map[string]string{
			qcURL(1): renderPage(append(cars("u", 2, nil), cars("n", 1, map[string]any{"vehicletype": "new"})...), 3, 40, 0),
		}}
	}
	w, err := New(pages()).Walk(context.Background(), listing.Query{SellerType: "P"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Skipped new cars count as reached: total counts new and used together.
	if len(w.Listings) != 2 || w.SkippedNew != 1 || w.Truncated || w.Shortfall != 0 {
		t.Errorf("used only: %d listings, %d skipped, truncated=%v", len(w.Listings), w.SkippedNew, w.Truncated)
	}
	w, err = New(pages(), WithUsedOnly(false)).Walk(context.Background(), listing.Query{SellerType: "P"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Listings) != 3 || w.SkippedNew != 0 {
		t.Errorf("all: %d listings, %d skipped", len(w.Listings), w.SkippedNew)
	}
}

func TestSearchDetectsSiteCeiling(t *testing.T) {
	// The site claims 100 but answers an empty page 3: the ~101-page ceiling.
	f := &fakeFetcher{pages: map[string]string{
		qcURL(1): renderPage(cars("a", 2, nil), 100, 2, 0),
		qcURL(2): renderPage(cars("b", 2, nil), 100, 2, 2),
		qcURL(3): renderPage(nil, 100, 2, 4),
	}}
	res, err := New(f).Search(context.Background(), listing.Query{SellerType: "P"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || !res.HitSiteCeiling || res.Shortfall != 96 || res.ShortfallRatio != 0.96 || res.PagesWalked != 3 {
		t.Errorf("got %+v", res)
	}
}

func TestSearchCallerCapIsNotSiteCeiling(t *testing.T) {
	f := &fakeFetcher{pages: map[string]string{qcURL(1): renderPage(cars("a", 2, nil), 100, 2, 0)}}
	res, err := New(f).Search(context.Background(), listing.Query{SellerType: "P", MaxPages: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.HitSiteCeiling {
		t.Errorf("truncated=%v hitSiteCeiling=%v", res.Truncated, res.HitSiteCeiling)
	}
}

func TestSearchDedupesAcrossPages(t *testing.T) {
	// A listing shifting between pages is kept once, in first-seen position.
	f := &fakeFetcher{pages: map[string]string{
		qcURL(1): renderPage(cars("a", 2, nil), 3, 2, 0),
		qcURL(2): renderPage(append(cars("a", 1, map[string]any{"carmileageinkms": "99"}), cars("b", 1, nil)...), 3, 2, 2),
	}}
	res, err := New(f).Search(context.Background(), listing.Query{SellerType: "P"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Listings) != 3 || deref(res.Listings[0].ID) != "kijiji:a-0" || derefNum(res.Listings[0].Km) != 99.0 {
		t.Errorf("got %d listings, first %v km %v", len(res.Listings), deref(res.Listings[0].ID), derefNum(res.Listings[0].Km))
	}
}

func TestSearchYearFilterIsLocal(t *testing.T) {
	f := &fakeFetcher{pages: map[string]string{
		qcURL(1): renderPage(append(cars("old", 1, map[string]any{"caryear": "2010"}), cars("new", 1, nil)...), 2, 40, 0),
	}}
	res, err := New(f).Search(context.Background(), listing.Query{SellerType: "P", YearFrom: listing.Num(2015)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Listings) != 1 || !res.LocallyFiltered || res.Truncated {
		t.Errorf("got %d listings, locallyFiltered=%v truncated=%v", len(res.Listings), res.LocallyFiltered, res.Truncated)
	}
}

func TestNewListingsRunStopsOnKnownPages(t *testing.T) {
	known := map[string]bool{}
	f := &fakeFetcher{pages: map[string]string{}}
	for p := 1; p <= 6; p++ {
		prefix := fmt.Sprintf("p%d", p)
		f.pages[qcURL(p)] = renderPage(cars(prefix, 2, nil), 100, 2, float64(2*(p-1)))
		if p >= 2 {
			// Everything past page 1 is already in the database.
			known["kijiji:"+prefix+"-0"], known["kijiji:"+prefix+"-1"] = true, true
		}
	}
	w, err := New(f, WithKnown(func(id string) bool { return known[id] })).Walk(context.Background(), listing.Query{SellerType: "P"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !w.StoppedEarly || w.PagesWalked != 4 || w.NewIDs == nil || *w.NewIDs != 2 || w.HitSiteCeiling {
		t.Errorf("stoppedEarly=%v pages=%d newIDs=%v", w.StoppedEarly, w.PagesWalked, w.NewIDs)
	}

	w, err = New(f, WithKnown(func(id string) bool { return known[id] }), WithStopAfter(1)).Walk(context.Background(), listing.Query{SellerType: "P"}, nil)
	if err != nil || w.PagesWalked != 2 {
		t.Errorf("stopAfter 1: pages=%d err=%v", w.PagesWalked, err)
	}
}

func TestNoveltyTracker(t *testing.T) {
	seen := func(id string) bool { return id == "kijiji:old" }
	l := func(id string) listing.Listing { return listing.Listing{ID: listing.Str(id)} }
	n := &NoveltyTracker{Known: seen, StopAfter: 2}
	steps := []struct {
		page []listing.Listing
		stop bool
	}{
		{[]listing.Listing{l("kijiji:old")}, false},
		{[]listing.Listing{l("kijiji:new")}, false}, // a fresh page resets the count
		{[]listing.Listing{l("kijiji:old")}, false},
		{[]listing.Listing{}, true},
	}
	for i, s := range steps {
		if got := n.Page(s.page); got != s.stop {
			t.Errorf("step %d: stop=%v, want %v", i, got, s.stop)
		}
	}
	if n.NewIDs != 1 {
		t.Errorf("newIDs = %d", n.NewIDs)
	}
}

func TestSearchPropagatesErrors(t *testing.T) {
	f := &fakeFetcher{pages: map[string]string{qcURL(1): "<html>denied</html>"}}
	_, err := New(f).Search(context.Background(), listing.Query{SellerType: "P"}, nil)
	var pe *parse.ParseError
	if !errors.As(err, &pe) {
		t.Errorf("err = %v, want ParseError", err)
	}
	_, err = New(&fakeFetcher{}).Search(context.Background(), listing.Query{SellerType: "P"}, nil)
	if err == nil {
		t.Error("fetch error swallowed")
	}
}
