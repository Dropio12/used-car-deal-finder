package craigslist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"carbuyer/crawler/internal/fetch"
	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/source"
)

// All fixtures are synthetic: made-up ids, tokens, places and VINs.

// match is a stand-in for the vocabulary matcher: "<year> <make> <model>".
func match(title string) Vehicle {
	f := strings.Fields(title)
	if len(f) < 3 {
		return Vehicle{}
	}
	var y float64
	fmt.Sscan(f[0], &y)
	return Vehicle{Make: listing.Str(f[1]), Model: listing.Str(f[2]), Year: listing.Num(y)}
}

const (
	minPostingID  = 7000000000
	minPostedDate = 1780000000
	syntheticVIN  = "2T1AA11111X000000"
)

// decodeBlock is shaped like the live feed's, trimmed.
func decodeBlock() map[string]any {
	return map[string]any{
		"minPostingId":         minPostingID,
		"minPostedDate":        minPostedDate,
		"locations":            []any{0, []any{49, "montreal"}, []any{175, "quebec"}, []any{25, "toronto"}},
		"locationDescriptions": []any{0, "Anytown", "VIN # " + strings.ToLower(syntheticVIN), "call 555 0100"},
	}
}

type itemOpts struct {
	id       int
	place    string
	category int
	price    any
	title    string
	km       any
}

func item(o itemOpts) []any {
	if o.place == "" {
		o.place = "1:1~45.5~-73.6"
	}
	if o.category == 0 {
		o.category = 145
	}
	if o.price == nil {
		o.price = 3995
	}
	if o.title == "" {
		o.title = "2006 Toyota Avalon XLS"
	}
	if o.km == nil {
		o.km = 159000
	}
	return []any{
		o.id, 3600, o.category, o.price, o.place, 0,
		[]any{13, fmt.Sprintf("tok%d", o.id)}, []any{4, "3:img_a", "img_b"}, []any{6, fmt.Sprintf("anytown-%d", o.id)},
		[]any{9, o.km}, []any{10, "$3995"}, o.title,
	}
}

// roundTrip passes an item through JSON so numbers are float64, as decoded.
func roundTrip(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func feed(items [][]any, total int) string {
	b, _ := json.Marshal(map[string]any{"data": map[string]any{"items": items, "decode": decodeBlock(), "totalResultCount": total}})
	return string(b)
}

func postPage(body, title, trans string) string {
	return `<html><div class="attrgroup"><span class="valu year">2006</span>
  <span class="valu makemodel"><a>toyota avalon xls</a></span></div>
  <div class="attr auto_fuel_type"><span class="labl">fuel:</span><span class="valu"><a>gas</a></span></div>
  <div class="attr auto_miles"><span class="labl">odometer:</span><span class="valu">159 000</span></div>
  <div class="attr auto_title_status"><span class="labl">title status:</span><span class="valu"><a>` + title + `</a></span></div>
  <div class="attr auto_transmission"><span class="labl">transmission:</span><span class="valu"><a>` + trans + `</a></span></div>
  <section id="postingbody"><div class="print-information print-qrcode-container"><p>QR</p><div class="print-qrcode"></div></div>` + body + `</section></html>`
}

func defaultPost() string { return postPage("Très propre, jamais accidenté.", "clean", "automatic") }

func TestBuildURL(t *testing.T) {
	cases := []struct {
		area, seller, batch, path, err string
	}{
		{area: "montreal", seller: "all", batch: "49-0-360-0-0", path: "cta"},
		{area: "quebec", seller: "private", batch: "175-0-360-0-0", path: "cto"},
		{area: "sherbrooke", seller: "dealer", batch: "390-0-360-0-0", path: "ctd"},
		{area: "toronto", seller: "all", batch: "25-0-360-0-0", path: "cta"},
		{area: "london", seller: "all", batch: "234-0-360-0-0", path: "cta"},
		// The feed defaults to sfbay when the area id is missing: refuse instead.
		{area: "atlantis", seller: "all", err: "unknown Craigslist area"},
		{area: "montreal", seller: "rental", err: "unknown Craigslist seller type"},
	}
	for _, c := range cases {
		t.Run(c.area+"/"+c.seller, func(t *testing.T) {
			got, err := BuildURL(c.area, c.seller)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(got)
			if u.Host != "sapi.craigslist.org" || u.Query().Get("batch") != c.batch ||
				u.Query().Get("searchPath") != c.path || u.Query().Get("cc") != "CA" || u.Query().Get("lang") != "en" {
				t.Errorf("url = %s", got)
			}
		})
	}
}

func TestAreasAndHeaders(t *testing.T) {
	cases := []struct {
		area, referer, province string
		id                      int
	}{
		{"montreal", "https://montreal.craigslist.org/", "QC", 49},
		{"quebec", "https://quebec.craigslist.org/", "QC", 175},
		{"sherbrooke", "https://sherbrooke.craigslist.org/", "QC", 390},
		{"toronto", "https://toronto.craigslist.org/", "ON", 25},
		{"ottawa", "https://ottawa.craigslist.org/", "ON", 76},
		{"hamilton", "https://hamilton.craigslist.org/", "ON", 213},
		{"kitchener", "https://kitchener.craigslist.org/", "ON", 214},
		{"london", "https://londonon.craigslist.org/", "ON", 234},
		{"windsor", "https://windsor.craigslist.org/", "ON", 235},
	}
	for _, c := range cases {
		a := Areas[c.area]
		if a.ID != c.id || a.Province != c.province {
			t.Errorf("%s = %+v", c.area, a)
		}
		h := Headers(c.area)
		if h["referer"] != c.referer || h["accept"] != "application/json, text/plain, */*" {
			t.Errorf("%s headers = %v", c.area, h)
		}
	}
}

func TestDecodeItem(t *testing.T) {
	decode := roundTrip(t, decodeBlock()).(map[string]any)
	d := DecodeItem(roundTrip(t, item(itemOpts{id: 5})).([]any), decode)
	if d == nil {
		t.Fatal("nil")
	}
	wantAt := time.Unix(minPostedDate+3600, 0).UTC().Format("2006-01-02T15:04:05.000Z")
	checks := []struct {
		name      string
		got, want any
	}{
		{"postingId", d.PostingID, float64(minPostingID + 5)},
		{"postedAt", *d.PostedAt, wantAt},
		{"odometer", *d.Odometer, 159000.0},
		{"areaId", d.AreaID, 49.0},
		{"locationText", *d.LocationText, "Anytown"},
		{"latitude", *d.Latitude, 45.5},
		{"longitude", *d.Longitude, -73.6},
		{"price", *d.Price, 3995.0},
		{"url", *d.URL, "https://www.craigslist.org/view/d/anytown-5/tok5"},
		{"title", *d.Title, "2006 Toyota Avalon XLS"},
		{"images", strings.Join(d.Images, " "),
			"https://images.craigslist.org/img_a_600x450.jpg https://images.craigslist.org/img_b_600x450.jpg"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestDecodeItemEdges(t *testing.T) {
	decode := roundTrip(t, decodeBlock()).(map[string]any)
	cases := []struct {
		name  string
		item  []any
		check func(*Decoded) bool
	}{
		{"too short", []any{1, 2, 3}, func(d *Decoded) bool { return d == nil }},
		{"zero price is no price", item(itemOpts{price: 0}), func(d *Decoded) bool { return d.Price == nil }},
		{"string price is no price", item(itemOpts{price: "$5"}), func(d *Decoded) bool { return d.Price == nil }},
		{"text odometer is no odometer", item(itemOpts{km: "159k"}), func(d *Decoded) bool { return d.Odometer == nil }},
		{"no description index", item(itemOpts{place: "1~45~-73"}), func(d *Decoded) bool { return d.LocationText == nil && d.AreaID == 49.0 }},
		{"unknown location", item(itemOpts{place: "9:1~~"}), func(d *Decoded) bool { return d.AreaID == nil && d.Latitude == nil }},
		{"no slug no url", []any{0, 0, 145, 1, "", 0, []any{13, "tok"}}, func(d *Decoded) bool { return d.URL == nil && d.Title == nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if d := DecodeItem(roundTrip(t, c.item).([]any), decode); !c.check(d) {
				t.Errorf("got %+v", d)
			}
		})
	}
}

func TestParseSearch(t *testing.T) {
	t.Run("drops the nearby-area padding a thin area comes back with", func(t *testing.T) {
		p, err := ParseSearch(feed([][]any{item(itemOpts{id: 1}), item(itemOpts{id: 2, place: "2:0~46.8~-71.2"})}, 1), match, "montreal")
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Listings) != 1 || p.OutOfArea != 1 || *p.Total != 1 {
			t.Errorf("listings=%d outOfArea=%d total=%v", len(p.Listings), p.OutOfArea, *p.Total)
		}
	})

	t.Run("reads the car from the title and the seller from the category", func(t *testing.T) {
		p, err := ParseSearch(feed([][]any{item(itemOpts{id: 3, category: 146})}, 1), match, "montreal")
		if err != nil {
			t.Fatal(err)
		}
		l := p.Listings[0]
		if *l.Make != "Toyota" || *l.Model != "Avalon" || *l.Year != 2006 || *l.Km != 159000 ||
			*l.SellerType != "Dealer" || *l.Province != "QC" || *l.City != "Anytown" ||
			*l.ID != "craigslist:7000000003" || *l.ReferenceID != "7000000003" || *l.Condition != "U" ||
			*l.ResultType != "Organic" || l.ImageCount != 2 || l.Source != "craigslist" {
			b, _ := json.Marshal(l)
			t.Errorf("listing = %s", b)
		}
	})

	t.Run("an Ontario area is ON", func(t *testing.T) {
		p, err := ParseSearch(feed([][]any{item(itemOpts{place: "3:1~43.6~-79.4"})}, 1), match, "toronto")
		if err != nil || len(p.Listings) != 1 || *p.Listings[0].Province != "ON" {
			t.Fatalf("%v %+v", err, p)
		}
	})

	locationCases := []struct {
		name, place string
		city, vin   *string
	}{
		{"keeps a VIN typed into the location line, not as a city", "1:2~45.5~-73.6", nil, listing.Str(syntheticVIN)},
		{"a phone number is not a city", "1:3~45.5~-73.6", nil, nil},
	}
	for _, c := range locationCases {
		t.Run(c.name, func(t *testing.T) {
			p, err := ParseSearch(feed([][]any{item(itemOpts{place: c.place})}, 1), match, "montreal")
			if err != nil {
				t.Fatal(err)
			}
			_, x := p.Listings[0], p.Extras[0]
			if listing.Deref(p.Listings[0].City) != listing.Deref(c.city) || listing.Deref(x.VIN) != listing.Deref(c.vin) {
				t.Errorf("city=%v vin=%v", p.Listings[0].City, x.VIN)
			}
		})
	}

	// A page that is not the feed is a ParseError, not an empty market.
	for _, body := range []string{"", "<html>blocked</html>", `{"data":{}}`, `{"data":{"items":[],"decode":null}}`, `[1]`} {
		t.Run("parse error "+body, func(t *testing.T) {
			_, err := ParseSearch(body, match, "montreal")
			var pe *fetch.ParseError
			if !errors.As(err, &pe) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestParsePost(t *testing.T) {
	p, err := ParsePost(defaultPost())
	if err != nil || p == nil {
		t.Fatal(err)
	}
	checks := []struct {
		name, got, want string
	}{
		{"transmission", listing.Deref(p.Transmission), "Automatique"},
		{"fuel", listing.Deref(p.Fuel), "Essence"},
		{"titleStatus", listing.Deref(p.TitleStatus), "clean"},
		{"makeModel", listing.Deref(p.MakeModel), "toyota avalon xls"},
		{"description", listing.Deref(p.Description), "Très propre, jamais accidenté."},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if *p.Km != 159000 || *p.Year != 2006 || p.VIN != nil {
		t.Errorf("km=%v year=%v vin=%v", *p.Km, *p.Year, p.VIN)
	}

	if p, err := ParsePost("<html>This posting has been deleted</html>"); p != nil || err != nil {
		t.Errorf("deleted post = %v, %v", p, err)
	}
	var pe *fetch.ParseError
	if _, err := ParsePost(""); !errors.As(err, &pe) {
		t.Errorf("empty = %v", err)
	}
}

func TestMergePost(t *testing.T) {
	cases := []struct {
		name, title, body string
		damaged, parts    bool
	}{
		{"clean title, clean text", "clean", "Très propre.", false, false},
		{"rebuilt title is damaged", "rebuilt", "Très propre.", true, false},
		{"salvage title is damaged", "salvage", "Très propre.", true, false},
		{"parts only is parts", "parts only", "Très propre.", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, _ := ParseSearch(feed([][]any{item(itemOpts{})}, 1), match, "montreal")
			post, _ := ParsePost(postPage(c.body, c.title, "manual"))
			l, x := MergePost(p.Listings[0], p.Extras[0], post, "2026-01-01T00:00:00.000Z")
			if l.IsDamaged != c.damaged || l.IsParts != c.parts || *l.Transmission != "Manuelle" ||
				listing.Deref(x.PageReadAt) != "2026-01-01T00:00:00.000Z" {
				t.Errorf("damaged=%v parts=%v x=%+v", l.IsDamaged, l.IsParts, x)
			}
		})
	}

	l := listing.Listing{Km: listing.Num(1)}
	if got, x := MergePost(l, Extra{}, nil, "now"); got.Km != l.Km || x.PageReadAt != nil {
		t.Error("nil post must change nothing")
	}
}

// fakeFetcher serves the feed and post pages by URL. Never the network.
type fakeFetcher struct {
	feed    string
	posts   map[string]error
	calls   []string
	headers map[string]string
}

func (f *fakeFetcher) Fetch(_ context.Context, u string) (string, error) {
	f.calls = append(f.calls, u)
	if strings.Contains(u, "sapi.craigslist.org") {
		return f.feed, nil
	}
	if err := f.posts[u]; err != nil {
		return "", err
	}
	return defaultPost(), nil
}

func (f *fakeFetcher) FetchWithHeaders(ctx context.Context, u string, h map[string]string) (string, error) {
	f.headers = h
	return f.Fetch(ctx, u)
}

func TestCrawl(t *testing.T) {
	ctx := context.Background()

	t.Run("reads each new post page once, then carries the description forward", func(t *testing.T) {
		f := &fakeFetcher{feed: feed([][]any{item(itemOpts{id: 1})}, 1)}
		c, err := New(f, WithMatcher(match)).Crawl(ctx, listing.Query{})
		if err != nil {
			t.Fatal(err)
		}
		l := c.Listings[0]
		if len(f.calls) != 2 || c.PagesRead != 1 || *l.Transmission != "Automatique" || c.Extras[l.Key()].PageReadAt == nil {
			t.Fatalf("calls=%v read=%d", f.calls, c.PagesRead)
		}
		if f.headers["referer"] != "https://montreal.craigslist.org/" {
			t.Errorf("feed headers = %v", f.headers)
		}

		// The second crawl must neither re-open the page nor blank what it read.
		stored := map[string]*string{l.Key(): l.Description}
		f.calls = nil
		lookup := func(id string) (*string, bool) { d, ok := stored[id]; return d, ok }
		c, err = New(f, WithMatcher(match), WithStored(lookup)).Crawl(ctx, listing.Query{})
		if err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != 1 || listing.Deref(c.Listings[0].Description) != listing.Deref(l.Description) {
			t.Errorf("calls=%v desc=%v", f.calls, c.Listings[0].Description)
		}
	})

	t.Run("one dead post page does not cost the others their write", func(t *testing.T) {
		f := &fakeFetcher{
			feed:  feed([][]any{item(itemOpts{id: 1}), item(itemOpts{id: 2})}, 2),
			posts: map[string]error{"https://www.craigslist.org/view/d/anytown-1/tok1": &fetch.HTTPError{Status: 410}},
		}
		var logs []string
		c, err := New(f, WithLog(func(s string) { logs = append(logs, s) })).Crawl(ctx, listing.Query{})
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Listings) != 2 || c.PagesFailed != 1 || c.PagesRead != 1 || len(logs) != 3 {
			t.Errorf("listings=%d failed=%d read=%d logs=%v", len(c.Listings), c.PagesFailed, c.PagesRead, logs)
		}
	})

	t.Run("the read budget caps post pages", func(t *testing.T) {
		f := &fakeFetcher{feed: feed([][]any{item(itemOpts{id: 1}), item(itemOpts{id: 2}), item(itemOpts{id: 3})}, 3)}
		c, err := New(f, WithReadPages(1)).Crawl(ctx, listing.Query{})
		if err != nil || len(f.calls) != 2 || len(c.Listings) != 3 || c.PagesRead != 1 {
			t.Errorf("err=%v calls=%d read=%d", err, len(f.calls), c.PagesRead)
		}
	})

	coverage := []struct {
		name      string
		total     int
		shortfall int
		truncated bool
	}{
		{"complete", 1, 0, false},
		{"a few light", 3, 2, false},
		{"over one feed page", 500, 499, true},
	}
	for _, cv := range coverage {
		t.Run("coverage "+cv.name, func(t *testing.T) {
			f := &fakeFetcher{feed: feed([][]any{item(itemOpts{id: 1})}, cv.total)}
			var ev source.PageEvent
			r, err := New(f, WithReadPages(0)).Search(ctx, listing.Query{Geo: "montreal", SellerType: "P"}, func(e source.PageEvent) { ev = e })
			if err != nil {
				t.Fatal(err)
			}
			if r.Shortfall != cv.shortfall || r.Truncated != cv.truncated || r.PagesWalked != 1 || ev.Of != 1 ||
				!strings.Contains(r.URL, "searchPath=cto") {
				t.Errorf("%+v", r)
			}
		})
	}

	t.Run("year bounds are applied locally", func(t *testing.T) {
		f := &fakeFetcher{feed: feed([][]any{item(itemOpts{id: 1}), item(itemOpts{id: 2, title: "2019 Mitsubishi RVR"})}, 2)}
		r, err := New(f, WithMatcher(match), WithReadPages(0)).Search(ctx, listing.Query{YearFrom: listing.Num(2010)}, nil)
		if err != nil || len(r.Listings) != 1 || *r.Listings[0].Model != "RVR" || !r.LocallyFiltered {
			t.Errorf("err=%v %+v", err, r)
		}
	})

	t.Run("an unknown area is refused before any request", func(t *testing.T) {
		f := &fakeFetcher{}
		if _, err := New(f).Search(ctx, listing.Query{Geo: "atlantis"}, nil); err == nil || len(f.calls) != 0 {
			t.Errorf("err=%v calls=%v", err, f.calls)
		}
	})

	if got := New(nil).Name(); got != "craigslist" {
		t.Errorf("Name = %q", got)
	}
}

func TestHTTPAdapterSendsFeedHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	client := fetch.NewHTTPFetcher()
	client.Headers = map[string]string{"X-Shared": "1"}
	h := HTTP{Client: client}
	if _, err := h.FetchWithHeaders(context.Background(), srv.URL, Headers("london")); err != nil {
		t.Fatal(err)
	}
	if got.Get("Referer") != "https://londonon.craigslist.org/" || got.Get("Accept") != "application/json, text/plain, */*" ||
		got.Get("X-Shared") != "1" {
		t.Errorf("headers = %v", got)
	}
	if len(client.Headers) != 1 {
		t.Errorf("shared client was mutated: %v", client.Headers)
	}
}
