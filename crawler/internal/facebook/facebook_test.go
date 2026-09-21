package facebook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"carbuyer/crawler/internal/fetch"
	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/parse"
	"carbuyer/crawler/internal/source"
)

// All fixtures below are synthetic: made-up ids, example.* hosts, no people.

// rec is one feed record, in the order Facebook serves it anonymously. The
// order matters: the listing id, then the cover photo's id, then the city
// page's id, and only then the title. A fixture without the decoy ids could
// not catch the parser picking the wrong one.
type rec struct {
	id, title, amount, subtitle, city, cityID, photoID, state string
	created                                                   int64
	sold                                                      bool
}

func (r rec) json() string {
	def := func(v, d string) string {
		if v == "" {
			return d
		}
		return v
	}
	created := r.created
	if created == 0 {
		created = 1787965826
	}
	return fmt.Sprintf(`"listing":{"__typename":"GroupCommerceProductItem","id":"%s",`+
		`"primary_listing_photo":{"image":{"uri":"https:\/\/scontent.example\/p.jpg"},"id":"%s"},`+
		`"creation_time":%d,`+
		`"listing_price":{"formatted_amount":"%s","amount":"4500.00"},`+
		`"location":{"reverse_geocode":{"city":"%s","state":"%s",`+
		`"city_page":{"display_name":"%s","id":"%s"}}},`+
		`"is_sold":%t,"marketplace_listing_title":"%s",`+
		`"custom_sub_titles_with_rendering_flags":[{"subtitle":"%s"}]}`,
		def(r.id, "900000000000100"), def(r.photoID, "900000000000200"), created,
		def(r.amount, `4\u00a0500\u00a0C$`), def(r.city, `Montr\u00e9al`), def(r.state, "QC"),
		def(r.city, `Montr\u00e9al`), def(r.cityID, "900000000000300"),
		r.sold, def(r.title, "2013 Subaru Impreza"), def(r.subtitle, `183\u00a0k\u00a0km`))
}

func feed(records ...string) string {
	return `<html><script>{"edges":[` + strings.Join(records, ",") + `]}</script></html>`
}

func mustSearch(t *testing.T, html string, o ParseOptions) []Item {
	t.Helper()
	p, err := ParseSearch(html, o)
	if err != nil {
		t.Fatal(err)
	}
	return p.Listings
}

func f(v float64) *float64 { return &v }

func eqNum(a, b *float64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

func show(p *float64) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprint(*p)
}

func TestParsePrice(t *testing.T) {
	cases := []struct {
		in   *string
		want *float64
	}{
		// The separator is U+00A0, which is why a naive digit scan found none.
		{listing.Str("4 500 C$"), f(4500)},
		{listing.Str(`12\u00a0000\u00a0C$`), f(12000)},
		{listing.Str("1 C$"), f(1)},
		// Null rather than zero on junk.
		{listing.Str(""), nil},
		{nil, nil},
		{listing.Str("Gratuit"), nil},
	}
	for _, c := range cases {
		if got := ParsePrice(c.in); !eqNum(got, c.want) {
			t.Errorf("ParsePrice(%q) = %s, want %s", listing.Deref(c.in), show(got), show(c.want))
		}
	}
}

func TestParseMileage(t *testing.T) {
	cases := []struct {
		in   *string
		want *float64
	}{
		{listing.Str("183 k km"), f(183000)},
		{listing.Str("87,5 k km"), f(87500)},
		{listing.Str(`183\u00a0k\u00a0km`), f(183000)},
		{listing.Str("87 000 km"), f(87000)},
		{nil, nil},
		{listing.Str("Montréal"), nil},
	}
	for _, c := range cases {
		if got := ParseMileage(c.in); !eqNum(got, c.want) {
			t.Errorf("ParseMileage(%q) = %s, want %s", listing.Deref(c.in), show(got), show(c.want))
		}
	}
}

func TestParseSearchReadsOneListing(t *testing.T) {
	ls := mustSearch(t, feed(rec{}.json()), ParseOptions{})
	if len(ls) != 1 {
		t.Fatalf("got %d listings", len(ls))
	}
	l := ls[0]
	checks := []struct{ name, got, want string }{
		{"id", l.Key(), "fbmp:900000000000100"},
		{"title", listing.Deref(l.Title), "2013 Subaru Impreza"},
		{"trimText", listing.Deref(l.TrimText), "2013 Subaru Impreza"},
		{"price", show(l.Price), "4500"},
		{"km", show(l.Km), "183000"},
		{"city", listing.Deref(l.City), "Montréal"},
		{"province", listing.Deref(l.Province), "QC"},
		{"listedAt", listing.Deref(l.ListedAt), time.Unix(1787965826, 0).UTC().Format("2006-01-02T15:04:05.000Z")},
		{"source", l.Source, "marketplace"},
		// Assumes private, which is the safe direction.
		{"sellerType", listing.Deref(l.SellerType), "PrivateSeller"},
		{"image", strings.Join(l.ImageURLs, ","), "https://scontent.example/p.jpg"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	// Never served anonymously: nil, so "not fetched" stays visible.
	if l.Description != nil {
		t.Errorf("description = %q, want nil", *l.Description)
	}
	// Unidentified rather than guessed without a matcher.
	if l.Make != nil || l.Model != nil || l.Year != nil {
		t.Error("make/model/year should be nil without a matcher")
	}
}

func TestParseSearchRecords(t *testing.T) {
	cases := []struct {
		name string
		html string
		ids  []string
	}{
		{"de-duplicates repeated ids", feed(rec{}.json(), rec{}.json()), []string{"900000000000100"}},
		{"reads several distinct listings",
			feed(rec{id: "900000000000101", title: "2012 Nissan Rogue"}.json(), rec{id: "900000000000102", title: "2009 Honda Civic"}.json()),
			[]string{"900000000000101", "900000000000102"}},
		// The id decides the item URL, so the wrong pick links to nothing.
		{"takes the listing id, not the photo or city id",
			feed(rec{id: "900000000000001", photoID: "900000000000002", cityID: "900000000000003"}.json()),
			[]string{"900000000000001"}},
		// The city id repeats for every car in a city; reading it as the
		// listing id collapsed a page of 24 to 8.
		{"keeps two cars from the same city apart",
			feed(rec{id: "111111111111111", cityID: "5555555"}.json(), rec{id: "222222222222222", cityID: "5555555"}.json()),
			[]string{"111111111111111", "222222222222222"}},
		// Other story shapes share the feed; no title means no car.
		{"skips rows that carry no car",
			feed(`"listing":{"__typename":"Other","id":"777777777777"}`, rec{}.json()),
			[]string{"900000000000100"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, l := range mustSearch(t, c.html, ParseOptions{}) {
				got = append(got, listing.Deref(l.ReferenceID))
				if want := "https://www.facebook.com/marketplace/item/" + listing.Deref(l.ReferenceID); listing.Deref(l.URL) != want {
					t.Errorf("url = %s, want %s", listing.Deref(l.URL), want)
				}
			}
			if strings.Join(got, ",") != strings.Join(c.ids, ",") {
				t.Errorf("ids = %v, want %v", got, c.ids)
			}
		})
	}
}

func TestParseSearchMatcherAndFlags(t *testing.T) {
	match := func(title, _ string) Vehicle {
		if strings.Contains(title, "Subaru Impreza") {
			return Vehicle{Make: listing.Str("Subaru"), Model: listing.Str("Impreza"), Year: f(2013)}
		}
		return Vehicle{}
	}
	l := mustSearch(t, feed(rec{}.json()), ParseOptions{Match: match})[0]
	if listing.Deref(l.Make) != "Subaru" || listing.Deref(l.Model) != "Impreza" || show(l.Year) != "2013" {
		t.Errorf("matcher result not applied: %s %s %s", listing.Deref(l.Make), listing.Deref(l.Model), show(l.Year))
	}
	if !mustSearch(t, feed(rec{sold: true}.json()), ParseOptions{})[0].IsSold {
		t.Error("sold flag not carried")
	}
	// A state outside QC/ON/NB/NS falls back to the searched city's province.
	if p := mustSearch(t, feed(rec{state: "BC"}.json()), ParseOptions{Province: "ON"})[0].Province; listing.Deref(p) != "ON" {
		t.Errorf("fallback province = %s", listing.Deref(p))
	}
}

func TestParseSearchPageStates(t *testing.T) {
	cases := []struct {
		name    string
		html    string
		empty   bool
		errLike string
	}{
		// Silence would look identical to a quiet day on the market.
		{"login wall", "<html>Log in to Facebook</html>", false, "never reached Marketplace"},
		{"french login wall", "<html>Connectez-vous à Facebook</html>", false, "never reached Marketplace"},
		{"empty body", "", false, "empty response body"},
		// Sherbrooke has no $38 000 cars. Nine of eighty bands are like this.
		{"empty band", `<html><script>{"MarketplaceSearchFeedStories":{"edges":[]}}</script></html>`, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := ParseSearch(c.html, ParseOptions{})
			if c.errLike != "" {
				var pe *fetch.ParseError
				if !errors.As(err, &pe) || !strings.Contains(err.Error(), c.errLike) {
					t.Fatalf("err = %v, want ParseError %q", err, c.errLike)
				}
				return
			}
			if err != nil || p.Empty != c.empty || len(p.Listings) != 0 {
				t.Fatalf("got %+v, %v", p, err)
			}
		})
	}
}

func TestSearchURL(t *testing.T) {
	cases := []struct {
		name string
		o    URLOptions
		want string
	}{
		// A bare location returns nothing while looking like a success.
		{"cars category by default", URLOptions{}, "https://www.facebook.com/marketplace/montreal/cars"},
		{"price band", URLOptions{MinPrice: intp(2000), MaxPrice: intp(4000)},
			"https://www.facebook.com/marketplace/montreal/cars?minPrice=2000&maxPrice=4000"},
		{"query switches to search", URLOptions{Query: "honda civic"},
			"https://www.facebook.com/marketplace/montreal/search?query=honda+civic"},
		{"all params in JS order", URLOptions{Place: "toronto", Query: "cx-5", MinPrice: intp(0), MaxPrice: intp(8000), SortBy: "price_ascend"},
			"https://www.facebook.com/marketplace/toronto/search?query=cx-5&minPrice=0&maxPrice=8000&sortBy=price_ascend"},
		{"quebec city word", URLOptions{Place: QuebecCities[1].Token}, "https://www.facebook.com/marketplace/quebec/cars"},
		{"sherbrooke id", URLOptions{Place: QuebecCities[3].Token}, "https://www.facebook.com/marketplace/116139965062971/cars"},
		{"URLSearchParams escaping", URLOptions{Query: "é~*'"}, "https://www.facebook.com/marketplace/montreal/search?query=%C3%A9%7E*%27"},
	}
	for _, c := range cases {
		if got := SearchURL(c.o); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestItemURL(t *testing.T) {
	for _, id := range []string{"123456", "fbmp:123456"} {
		if got := ItemURL(id); got != "https://www.facebook.com/marketplace/item/123456" {
			t.Errorf("ItemURL(%s) = %s", id, got)
		}
	}
}

// itemPage is the item page shape, suggestion strip included. The strip
// carries unrelated listings with their own prices, sometimes before the
// subject: taking the first price read a $6 995 car as $50 of earbuds.
type itemOpts struct {
	title, desc, odoUnit string
	photos, odo          int
	sold                 bool
	price                *string
	noMarker             bool
}

func itemPage(o itemOpts) string {
	if o.title == "" {
		o.title = "2013 Ford Fusion"
	}
	if o.desc == "" {
		o.desc = "The car starts and runs, but it has mechanical issues"
	}
	if o.odoUnit == "" {
		o.odoUnit = "KILOMETERS"
	}
	if o.photos == 0 {
		o.photos = 3
	}
	if o.odo == 0 {
		o.odo = 188000
	}
	var b strings.Builder
	b.WriteString(`<html><script>{`)
	for _, s := range [][2]string{{"Wireless earbuds", "50.00"}, {"Gaming PC", "700.00"}, {"1980 Roadster", "5500.00"}} {
		fmt.Fprintf(&b, `"listing_price":{"amount":"%s"},"formatted_price":{"text":"%s\u00a0$"},"marketplace_listing_title":"%s",`, s[1], s[1], s[0])
	}
	b.WriteString(`"listing_photos":[`)
	for i := 0; i < o.photos; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"__typename":"Photo","image":{"height":960,"width":720,"uri":"https:\/\/scontent.example\/p%d.jpg"},"id":"9%d"}`, i, i)
	}
	fmt.Fprintf(&b, `],"redacted_description":{"text":"%s"},"vehicle_odometer_data":{"unit":"%s","value":%d},`, o.desc, o.odoUnit, o.odo)
	if o.price != nil {
		fmt.Fprintf(&b, `"listing_price":{"amount_with_offset":"0","currency":"CAD","amount":"%s"},`, *o.price)
		if !o.noMarker {
			b.WriteString(`"__isMarketplaceVehicleListing":"GroupCommerceProductItem",`)
		}
	}
	fmt.Fprintf(&b, `"is_sold":%t,"marketplace_listing_title":"%s"}</script></html>`, o.sold, o.title)
	return b.String()
}

func mustItem(t *testing.T, html string) Detail {
	t.Helper()
	d, err := ParseItem(html)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestParseItem(t *testing.T) {
	p := listing.Str("1600.00")
	cases := []struct {
		name  string
		html  string
		check func(Detail) string
	}{
		{"whole gallery", itemPage(itemOpts{photos: 5, price: p}), func(d Detail) string {
			if d.ImageCount != 5 || len(d.ImageURLs) != 5 || d.ImageURLs[0] != "https://scontent.example/p0.jpg" {
				return fmt.Sprintf("%d %v", d.ImageCount, d.ImageURLs)
			}
			return ""
		}},
		// Stored photos are capped; the true count is still reported.
		{"caps stored photos", itemPage(itemOpts{photos: 19, price: p}), func(d Detail) string {
			if len(d.ImageURLs) != parse.MaxStoredImages || d.ImageCount != 19 {
				return fmt.Sprintf("%d stored of %d", len(d.ImageURLs), d.ImageCount)
			}
			return ""
		}},
		{"description", itemPage(itemOpts{price: p}), func(d Detail) string {
			if !strings.Contains(listing.Deref(d.Description), "mechanical issues") {
				return listing.Deref(d.Description)
			}
			return ""
		}},
		{"description newlines and tags", itemPage(itemOpts{desc: `line one\nline <b>two</b>`, price: p}), func(d Detail) string {
			if listing.Deref(d.Description) != "line one\nline  two" {
				return fmt.Sprintf("%q", listing.Deref(d.Description))
			}
			return ""
		}},
		{"odometer", itemPage(itemOpts{price: p}), func(d Detail) string {
			if show(d.Km) != "188000" {
				return show(d.Km)
			}
			return ""
		}},
		// 88 000 miles read as km would look like a low-mileage bargain.
		{"miles converted", itemPage(itemOpts{odoUnit: "MILES", odo: 100000, price: p}), func(d Detail) string {
			if show(d.Km) != "160934" {
				return show(d.Km)
			}
			return ""
		}},
		{"seller stays nil, sold flag carried", itemPage(itemOpts{sold: true, price: p}), func(d Detail) string {
			if d.SellerName != nil || !d.IsSold {
				return "seller or sold wrong"
			}
			return ""
		}},
		{"price of this car, not the strip", itemPage(itemOpts{price: listing.Str("6995.00")}), func(d Detail) string {
			if show(d.Price) != "6995" || d.PriceUnreadable {
				return show(d.Price)
			}
			return ""
		}},
		// A seller really can set $0: a price, not a parse failure.
		{"zero is priced", itemPage(itemOpts{price: listing.Str("0.00")}), func(d Detail) string {
			if show(d.Price) != "0" || d.PriceUnreadable {
				return show(d.Price)
			}
			return ""
		}},
		{"strip is in the fixture, still ignored", itemPage(itemOpts{price: listing.Str("12500.00")}), func(d Detail) string {
			if show(d.Price) != "12500" {
				return show(d.Price)
			}
			return ""
		}},
		// The tripwire: a renamed key has to surface, not fall back silently.
		{"renamed price key is unreadable", regexp.MustCompile(`"listing_price":\{[^{}]*"amount":"6995\.00"\},`).
			ReplaceAllString(itemPage(itemOpts{price: listing.Str("6995.00")}), `"listing_price":{"someNewKey":"6995"},`),
			func(d Detail) string {
				if d.Price != nil || !d.PriceUnreadable || !strings.Contains(listing.Deref(d.Description), "mechanical issues") {
					return fmt.Sprintf("%s %v", show(d.Price), d.PriceUnreadable)
				}
				return ""
			}},
		// No cry of wolf on a page that is not a vehicle listing at all.
		{"not a vehicle", itemPage(itemOpts{}), func(d Detail) string {
			if d.Price != nil || d.PriceUnreadable {
				return "flagged a non-vehicle"
			}
			return ""
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if msg := c.check(mustItem(t, c.html)); msg != "" {
				t.Error(msg)
			}
		})
	}
}

func TestParseItemRefusesAPageWithNoCar(t *testing.T) {
	// A pulled listing still answers HTTP 200, so the body decides.
	for _, html := range []string{"<html>Log in to Facebook</html>", ""} {
		_, err := ParseItem(html)
		var pe *fetch.ParseError
		if !errors.As(err, &pe) {
			t.Errorf("ParseItem(%q) err = %v, want ParseError", html, err)
		}
	}
	_, err := ParseItem("<html>Log in to Facebook</html>")
	if !IsNoListing(err) {
		t.Errorf("IsNoListing(%v) = false", err)
	}
}

func TestCities(t *testing.T) {
	keys := func(cs []City) string {
		var k []string
		for _, c := range cs {
			k = append(k, c.Key)
		}
		return strings.Join(k, ",")
	}
	cases := []struct{ geo, want string }{
		{"", "montreal,quebecCity,troisRivieres,sherbrooke"},
		{"quebec", "montreal,quebecCity,troisRivieres,sherbrooke"},
		{"ontario", keys(OntarioCities)},
		{"toronto", "toronto"},
		{"Montreal", "montreal"},
		{"stcatharines", "stCatharines"},
	}
	for _, c := range cases {
		got, err := Cities(c.geo)
		if err != nil || keys(got) != c.want {
			t.Errorf("Cities(%q) = %s, %v; want %s", c.geo, keys(got), err, c.want)
		}
	}
	if _, err := Cities("atlantis"); err == nil {
		t.Error("an unknown geo must fail, not guess")
	}
}

func TestCityTables(t *testing.T) {
	// 109475462419642 sat in the table as quebecCity and served Ottawa.
	if QuebecCities[1].Token != "quebec" {
		t.Errorf("quebecCity token = %s", QuebecCities[1].Token)
	}
	wordOK := map[string]bool{"montreal": true, "quebec": true, "toronto": true, "ottawa": true, "windsor": true}
	numeric := regexp.MustCompile(`^\d+$`)
	for _, set := range []struct {
		cities   []City
		province string
	}{{QuebecCities, "QC"}, {OntarioCities, "ON"}} {
		for _, c := range set.cities {
			if c.Token == "109475462419642" {
				t.Error("the token that served Ottawa is back")
			}
			if c.Province != set.province {
				t.Errorf("%s province = %s", c.Key, c.Province)
			}
			// A word slug like `kingston` silently serves Montréal.
			if !wordOK[c.Token] && !numeric.MatchString(c.Token) {
				t.Errorf("%s uses an unverified word slug %q", c.Key, c.Token)
			}
		}
	}
	if len(OntarioCities) != 14 {
		t.Errorf("%d Ontario cities", len(OntarioCities))
	}
}

func inProvince(province string, n int, city string) []Item {
	out := make([]Item, n)
	for i := range out {
		p, c := province, city
		out[i].Province, out[i].City = &p, &c
		out[i].ID = listing.Str(fmt.Sprintf("%s%d", province, i))
	}
	return out
}

func TestKeepInRegion(t *testing.T) {
	mix := append(append(inProvince("QC", 5, "x"), inProvince("ON", 6, "Ottawa")...), inProvince("NB", 4, "Edmundston")...)
	unknown := append([]Item{{}}, inProvince("QC", 4, "x")...)
	cases := []struct {
		name          string
		in            []Item
		place         string
		kept, dropped int
		errLike       string
	}{
		{"genuinely in region", inProvince("QC", 20, "x"), "quebecCity", 20, 0, ""},
		// Rejecting the band would throw away the five real cars.
		{"drops the padding", mix, "quebecCity", 5, 10, ""},
		// HTTP 200, a full page, none of it local: a wrong token.
		{"nothing in region", inProvince("ON", 14, "Ottawa"), "sherbrooke", 0, 0, "resolved elsewhere"},
		{"names the place and what it saw", inProvince("ON", 14, "Gatineau"), "troisRivieres", 0, 0, `"troisRivieres" resolved elsewhere: none of 14 listings are in QC (saw Gatineau)`},
		// Absent is not "elsewhere".
		{"unknown province kept", unknown, "montreal", 5, 0, ""},
		{"thin page is not a fallback", inProvince("ON", 3, "x"), "montreal", 0, 3, ""},
		{"empty page", nil, "montreal", 0, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kept, dropped, err := KeepInRegion(c.in, "QC", c.place)
			if c.errLike != "" {
				if err == nil || !strings.Contains(err.Error(), c.errLike) {
					t.Fatalf("err = %v, want %q", err, c.errLike)
				}
				return
			}
			if err != nil || len(kept) != c.kept || dropped != c.dropped {
				t.Fatalf("kept %d dropped %d err %v", len(kept), dropped, err)
			}
			for _, l := range kept {
				if l.Province != nil && *l.Province != "QC" {
					t.Errorf("kept a %s listing", *l.Province)
				}
			}
		})
	}
}

func TestPlans(t *testing.T) {
	// Sort order is the lever: 20 bands x 1 sort reached 268, x 3 sorts 997.
	plan := SweepPlan(10000, 2000, nil)
	if len(plan) != 5*len(Sorts) {
		t.Fatalf("plan has %d shards", len(plan))
	}
	distinct := map[string]bool{}
	perSort := map[string]int{}
	for _, p := range plan {
		distinct[fmt.Sprint(*p.MinPrice, p.SortBy)] = true
		perSort[p.SortBy]++
		// Never the filters Facebook silently ignores.
		if strings.Contains(SearchURL(p.urlOptions("montreal")), "daysSinceListed") ||
			strings.Contains(SearchURL(p.urlOptions("montreal")), "radius") {
			t.Error("sent a filter Facebook ignores")
		}
	}
	if len(distinct) != len(plan) {
		t.Error("duplicate shards")
	}
	for _, s := range Sorts {
		if perSort[s] != 5 {
			t.Errorf("%s visited %d bands", s, perSort[s])
		}
	}
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"narrower bands", len(SweepPlan(8000, 500, nil)), 16 * len(Sorts)},
		{"narrowed sorts", len(SweepPlan(4000, 2000, []string{"price_ascend"})), 2},
		{"empty model list", len(ModelPlan(nil, nil)), 0},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: %d, want %d", c.name, c.got, c.want)
		}
	}

	mp := ModelPlan([]string{"honda civic", "audi a3"}, nil)
	if len(mp) != 2 || mp[0].Query != "honda civic" || mp[1].Query != "audi a3" {
		t.Fatalf("model plan %+v", mp)
	}
	// Newest-first only, and no band: the response decides whether to split.
	if mp[0].SortBy != "creation_time_descend" || mp[0].MinPrice != nil {
		t.Errorf("model shard %+v", mp[0])
	}
}

func TestPriceBands(t *testing.T) {
	bands := PriceBands(10000, 2000)
	if len(bands) != 5 || *bands[0].MinPrice != 0 || *bands[4].MaxPrice != 10000 {
		t.Fatalf("bands %+v", bands)
	}
	for i := 1; i < len(bands); i++ {
		if *bands[i].MinPrice != *bands[i-1].MaxPrice {
			t.Errorf("gap at %d", i)
		}
	}
}

func bandsOf(shards []Shard) string {
	var s []string
	for _, x := range shards {
		s = append(s, fmt.Sprintf("%d-%d/%d", *x.MinPrice, *x.MaxPrice, x.Depth))
	}
	return strings.Join(s, " ")
}

func TestSplitShard(t *testing.T) {
	q := Shard{Query: "honda civic", SortBy: "creation_time_descend"}
	band := func(lo, hi, depth int) Shard {
		s := q
		s.MinPrice, s.MaxPrice, s.Depth = intp(lo), intp(hi), depth
		return s
	}
	cases := []struct {
		name  string
		shard Shard
		count int
		o     SplitOptions
		want  string
	}{
		{"full page splits by price", q, PageCeiling, DefaultSplit, "0-8000/1 8000-16000/1 16000-24000/1 24000-32000/1 32000-40000/1"},
		{"short page is exhausted", q, PageCeiling - 1, DefaultSplit, ""},
		// The missing car was a $3 000 Civic inside a still-full $0-8 000 band.
		{"full band is subdivided", band(0, 8000, 1), PageCeiling, DefaultSplit, "0-2000/2 2000-4000/2 4000-6000/2 6000-8000/2"},
		{"only the band it holds", band(24000, 32000, 1), PageCeiling, DefaultSplit, "24000-26000/2 26000-28000/2 28000-30000/2 30000-32000/2"},
		{"stops at maxDepth", band(0, 2000, 2), PageCeiling, DefaultSplit, ""},
		{"too narrow to cut", band(1000, 1001, 1), PageCeiling, DefaultSplit, ""},
		// The absent query is the tell, not the band.
		{"plain sweep never split", Shard{MinPrice: intp(0), MaxPrice: intp(2000), SortBy: "price_ascend"}, 24, DefaultSplit, ""},
		{"empty response", q, 0, DefaultSplit, ""},
		{"rounds uneven widths", band(0, 1000, 1), PageCeiling, SplitOptions{Max: 40000, Step: 8000, MaxDepth: 2, Into: 3}, "0-333/2 333-667/2 667-1000/2"},
	}
	for _, c := range cases {
		got := SplitShard(c.shard, c.count, c.o)
		if bandsOf(got) != c.want {
			t.Errorf("%s: %s, want %s", c.name, bandsOf(got), c.want)
		}
		for _, g := range got {
			if g.Query != q.Query || g.SortBy != q.SortBy {
				t.Errorf("%s: lost the query %+v", c.name, g)
			}
			// The depth marker never reaches the URL.
			if strings.Contains(SearchURL(g.urlOptions("montreal")), "depth") {
				t.Errorf("%s: depth in URL", c.name)
			}
		}
	}
}

func TestSplitByFreshness(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	item := func(id string, ago time.Duration) Item {
		it := Item{}
		it.ID = listing.Str(id)
		if ago >= 0 {
			it.ListedAt = listing.Str(now.Add(-ago).Format("2006-01-02T15:04:05.000Z"))
		}
		return it
	}
	items := []Item{item("new", 10*time.Minute), item("old", 13*24*time.Hour), item("undated", -1), item("edge", 24*time.Hour)}
	cases := []struct {
		name          string
		maxAge        time.Duration
		fresh, backfl string
	}{
		{"a day", 24 * time.Hour, "new,undated,edge", "old"},
		{"half an hour", 30 * time.Minute, "new,undated", "old,edge"},
		{"no limit", 0, "new,old,undated,edge", ""},
	}
	ids := func(its []Item) string {
		var s []string
		for _, it := range its {
			s = append(s, it.Key())
		}
		return strings.Join(s, ",")
	}
	for _, c := range cases {
		fresh, back := SplitByFreshness(items, c.maxAge, now)
		if ids(fresh) != c.fresh || ids(back) != c.backfl {
			t.Errorf("%s: fresh %s backfill %s", c.name, ids(fresh), ids(back))
		}
	}
}

// fakeFetcher answers by URL; anything unplanned is a failure. Never the network.
type fakeFetcher struct {
	pages map[string]string
	errs  map[string]error
	def   func(url string) (string, error)
	calls []string
}

func (f *fakeFetcher) Fetch(_ context.Context, url string) (string, error) {
	f.calls = append(f.calls, url)
	if err, ok := f.errs[url]; ok {
		return "", err
	}
	if body, ok := f.pages[url]; ok {
		return body, nil
	}
	if f.def != nil {
		return f.def(url)
	}
	return "", fmt.Errorf("unexpected url %s", url)
}

// fullFeed is n distinct QC records with ids from base.
func fullFeed(base, n int) string {
	var rs []string
	for i := 0; i < n; i++ {
		rs = append(rs, rec{id: fmt.Sprint(base + i), title: fmt.Sprintf("2015 Honda Civic %d", i)}.json())
	}
	return feed(rs...)
}

func TestSourceIdentity(t *testing.T) {
	var s source.Source = New(&fakeFetcher{})
	if s.Name() != "marketplace" {
		t.Errorf("Name() = %s", s.Name())
	}
}

func TestSearchNewOnlyOneCity(t *testing.T) {
	url := "https://www.facebook.com/marketplace/toronto/cars?sortBy=creation_time_descend"
	ff := &fakeFetcher{pages: map[string]string{url: feed(
		rec{id: "1", state: "ON", city: "Toronto"}.json(),
		rec{id: "2", state: "QC"}.json(), // padding from another province
	)}}
	var events []source.PageEvent
	res, err := New(ff, WithNewOnly()).Search(context.Background(), listing.Query{Geo: "toronto"}, func(e source.PageEvent) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if len(ff.calls) != 1 || res.URL != url {
		t.Fatalf("calls %v", ff.calls)
	}
	if len(res.Listings) != 1 || res.Listings[0].Key() != "fbmp:1" || res.Truncated || res.PagesWalked != 1 {
		t.Errorf("result %+v", res)
	}
	if len(events) != 1 || events[0].Found != 1 {
		t.Errorf("events %+v", events)
	}
}

func TestSweepSplitsFullModelQueries(t *testing.T) {
	// The bare model query comes back full, so it is split into five bands;
	// the cheapest band is still full and split again; everything else is short.
	base := "https://www.facebook.com/marketplace/montreal/search?query=honda+civic"
	ff := &fakeFetcher{
		pages: map[string]string{
			base + "&sortBy=creation_time_descend":                              fullFeed(1000, 24),
			base + "&minPrice=0&maxPrice=8000&sortBy=creation_time_descend":     fullFeed(2000, 24),
			base + "&minPrice=0&maxPrice=2000&sortBy=creation_time_descend":     fullFeed(3000, 24),
			base + "&minPrice=8000&maxPrice=16000&sortBy=creation_time_descend": fullFeed(1000, 3),
		},
		def: func(string) (string, error) { return fullFeed(4000, 2), nil },
	}
	var log []string
	sw, err := New(ff, WithLog(func(s string) { log = append(log, s) })).
		Sweep(context.Background(), listing.Query{Geo: "montreal", Make: "Honda", Model: "Civic"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 1 bare + 5 bands + 4 sub-bands.
	if sw.Stats.Shards != 10 || len(ff.calls) != 10 {
		t.Fatalf("shards %d calls %d", sw.Stats.Shards, len(ff.calls))
	}
	// 0-2000 is at depth 2 and still full: truncated, said out loud.
	if sw.Stats.Truncated != 1 {
		t.Errorf("truncated %d", sw.Stats.Truncated)
	}
	// Stored once each, whichever shard found them.
	if len(sw.Items) != 24+24+24+2 {
		t.Errorf("%d distinct", len(sw.Items))
	}
	for _, it := range sw.Items {
		if it.GeoSlug != "fbmp-montreal" {
			t.Errorf("geo slug %s", it.GeoSlug)
		}
	}
	if !strings.Contains(strings.Join(log, "\n"), "(+2 model queries split by price)") {
		t.Errorf("log %v", log)
	}
}

func TestSweepBudgetAndGiveUp(t *testing.T) {
	t.Run("budget cuts the plan out loud", func(t *testing.T) {
		ff := &fakeFetcher{def: func(string) (string, error) { return fullFeed(1, 2), nil }}
		sw, err := New(ff, WithBands(4000, 2000), WithMaxRequests(4)).Sweep(context.Background(), listing.Query{Geo: "montreal"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if sw.Stats.Shards != 4 || sw.Stats.OverBudget != 2 {
			t.Errorf("stats %+v", sw.Stats)
		}
	})
	t.Run("six refusals in a row leave the city alone", func(t *testing.T) {
		ff := &fakeFetcher{def: func(url string) (string, error) {
			if strings.Contains(url, "/montreal/") {
				return "<html>Log in</html>", nil
			}
			return fullFeed(1, 2), nil
		}}
		sw, err := New(ff, WithBands(40000, 2000)).Sweep(context.Background(), listing.Query{Geo: "quebec"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if sw.Stats.Blocked != giveUpAfter || sw.Stats.OverBudget != 60-giveUpAfter || sw.Stats.Regions != 3 {
			t.Errorf("stats %+v", sw.Stats)
		}
	})
}

func TestSweepAllFailed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"network", errors.New("dial tcp: connection refused"), "the network, not the site"},
		{"blocked", &fetch.HTTPError{Status: 400}, "blocked, or the page shape changed"},
	}
	for _, c := range cases {
		ff := &fakeFetcher{def: func(string) (string, error) { return "", c.err }}
		_, err := New(ff, WithNewOnly()).Sweep(context.Background(), listing.Query{Geo: "montreal"}, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestSweepWrongTokenCountsAsBlocked(t *testing.T) {
	var rs []string
	for i := 0; i < 14; i++ {
		rs = append(rs, rec{id: fmt.Sprint(500 + i), state: "ON", city: "Ottawa"}.json())
	}
	ff := &fakeFetcher{def: func(string) (string, error) { return feed(rs...), nil }}
	_, err := New(ff, WithNewOnly()).Sweep(context.Background(), listing.Query{Geo: "sherbrooke"}, nil)
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Errorf("err = %v", err)
	}
}

func TestSweepLocalFilters(t *testing.T) {
	now := time.Unix(1787965826, 0).Add(2 * time.Hour)
	match := func(title, _ string) Vehicle {
		if strings.HasPrefix(title, "2013") {
			return Vehicle{Make: listing.Str("Subaru"), Model: listing.Str("Impreza"), Year: f(2013)}
		}
		return Vehicle{Year: f(2020)}
	}
	ff := &fakeFetcher{def: func(string) (string, error) {
		return feed(rec{id: "1"}.json(), rec{id: "2", title: "2020 Kia Soul"}.json(),
			rec{id: "3", created: 1787965826 - 86400*5}.json()), nil
	}}
	src := New(ff, WithNewOnly(), WithMatcher(match), WithMaxAge(time.Hour*24), WithNow(func() time.Time { return now }))
	res, err := src.Search(context.Background(), listing.Query{Geo: "montreal", YearTo: f(2015)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Listings) != 1 || res.Listings[0].Key() != "fbmp:1" || !res.LocallyFiltered {
		t.Errorf("result %+v", res.Listings)
	}
}

func TestEnrich(t *testing.T) {
	items := mustSearch(t, feed(rec{id: "11"}.json(), rec{id: "12"}.json(), rec{id: "13"}.json()), ParseOptions{})
	ff := &fakeFetcher{pages: map[string]string{
		ItemURL("11"): itemPage(itemOpts{desc: "Moteur fait un bruit, vendu pour pieces", photos: 4, price: listing.Str("1600.00")}),
		ItemURL("12"): "<html>This content isn't available</html>",
		ItemURL("13"): regexp.MustCompile(`"listing_price":\{[^{}]*"amount":"9\.00"\},`).
			ReplaceAllString(itemPage(itemOpts{price: listing.Str("9.00")}), `"listing_price":{"x":"9"},`),
	}}
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	var log []string
	out := New(ff, WithNow(func() time.Time { return now }), WithLog(func(s string) { log = append(log, s) })).
		Enrich(context.Background(), items, 10)
	if len(out.Items) != 2 {
		t.Fatalf("%d enriched", len(out.Items))
	}
	a := out.Items[0]
	if show(a.Price) != "1600" || show(a.Km) != "188000" || a.ImageCount != 4 || !a.IsParts ||
		listing.Deref(a.PageReadAt) != "2026-09-29T12:00:00.000Z" || listing.Deref(a.Make) != "" {
		t.Errorf("merged %+v", a)
	}
	// The unreadable page keeps the feed's price.
	if b := out.Items[1]; show(b.Price) != "4500" {
		t.Errorf("fell back to %s", show(b.Price))
	}
	if strings.Join(out.Gone, ",") != "fbmp:12" || out.Unreadable != 1 {
		t.Errorf("gone %v unreadable %d", out.Gone, out.Unreadable)
	}
	if !strings.Contains(strings.Join(log, "\n"), "1 of 3 page(s) are cars with no readable price") {
		t.Errorf("log %v", log)
	}

	// A login wall on every page retires nothing.
	walled := &fakeFetcher{def: func(string) (string, error) { return "<html>Log in</html>", nil }}
	if out := New(walled).Enrich(context.Background(), items, 10); len(out.Gone) != 0 {
		t.Errorf("retired %v behind a login wall", out.Gone)
	}
	// The limit is honoured.
	counted := &fakeFetcher{def: func(string) (string, error) { return itemPage(itemOpts{}), nil }}
	New(counted).Enrich(context.Background(), items, 1)
	if len(counted.calls) != 1 {
		t.Errorf("%d calls for a limit of 1", len(counted.calls))
	}
}

func TestNewHTTPFetcherSendsBrowserHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		fmt.Fprint(w, `<html>{"MarketplaceSearchFeedStories":{}}</html>`)
	}))
	defer srv.Close()
	if _, err := NewHTTPFetcher().Fetch(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	for k, v := range Headers {
		if got.Get(k) != v {
			t.Errorf("header %s = %q, want %q", k, got.Get(k), v)
		}
	}
}
