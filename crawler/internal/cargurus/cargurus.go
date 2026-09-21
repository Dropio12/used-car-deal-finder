// Package cargurus is the CarGurus implementation of source.Source.
// Port of src/cargurus.js and of crawlCargurus in src/crawl.js.
//
// In Canada it is a dealer site: every listing in a 100 km Montréal sample was
// `sellerType: "DEALER"`. So it adds comps, not private deals — but good ones.
// Each listing carries a VIN, the days it has been on the market, and CarGurus'
// own estimate of what it should cost (`expectedPrice`), which no other source
// publishes.
//
// Transport (probed 2026-09-24): /Cars/searchResults.action returns a bare
// JSON array of listings, 48 at a time. A request without browser-shaped
// headers gets HTTP 403 and a captcha page; with them, no cookie is needed.
//
// Two limits shape the crawl:
//
//   - Paging stops at offset 10 000. Past it the endpoint answers `null`, the
//     same answer as "no more results", so the two cannot be told apart from
//     the response. The crawl shards by model year to stay under it.
//   - startYear/endYear filter. minYear/maxYear are accepted and silently
//     ignored — check a filter against the returned cars, not the status code.
package cargurus

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"carbuyer/crawler/internal/fetch"
	"carbuyer/crawler/internal/jstext"
	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/source"
)

const (
	// Origin is the Canadian site every URL is built on.
	Origin = "https://www.cargurus.ca"
	// PageSize is listings per page (the maxResults the site honours).
	PageSize = 48
	// OffsetCeiling: offsets at or past this answer `null` whatever the scope holds.
	OffsetCeiling = 10_000
	// FirstShardYear is where one-year shards start; everything older is one shard.
	FirstShardYear = 2005
)

// Scope is the postal code a search is centred on and the radius around it in km.
type Scope struct {
	Zip      string
	Distance int
}

// GeoSlug is the scope key the JS crawler stores listings under
// ("cargurus-H2X2L5-100km"), which removal detection is scoped to.
func (s Scope) GeoSlug() string { return fmt.Sprintf("cargurus-%s-%dkm", s.Zip, s.Distance) }

var (
	// Montreal is the JS CARGURUS_MONTREAL scope, and the default.
	Montreal = Scope{Zip: "H2X2L5", Distance: 100}
	// Toronto is centred on M5V 2T6 (downtown), same radius. Written without
	// the space, like the Montréal code.
	Toronto = Scope{Zip: "M5V2T6", Distance: 100}
)

// Scopes maps listing.Query.Geo to a centre. "" means Montréal.
var Scopes = map[string]Scope{"": Montreal, "montreal": Montreal, "toronto": Toronto}

// ScopeFor resolves a Query.Geo value.
func ScopeFor(geo string) (Scope, error) {
	s, ok := Scopes[strings.ToLower(geo)]
	if !ok {
		return Scope{}, fmt.Errorf("cargurus: unknown geo %q (want montreal or toronto)", geo)
	}
	return s, nil
}

// Headers make the request look like the site's own XHR. Without them
// CarGurus answers 403 with a captcha.
var Headers = map[string]string{
	"accept":          "application/json, text/plain, */*",
	"accept-language": "en-CA,en;q=0.9",
	"sec-fetch-mode":  "cors",
	"sec-fetch-site":  "same-origin",
	"sec-fetch-dest":  "empty",
	"referer":         Origin + "/",
}

// NewHTTPFetcher returns an HTTP fetcher that sends Headers on every request.
// fetch.Fetcher carries no per-request headers, so the headers live on the
// fetcher: wire it as throttle.Wrap(cargurus.NewHTTPFetcher()).
func NewHTTPFetcher() *fetch.HTTPFetcher {
	f := fetch.NewHTTPFetcher()
	f.Headers = map[string]string{}
	for k, v := range Headers {
		f.Headers[k] = v
	}
	return f
}

// Shard is one model-year slice of the search.
type Shard struct{ StartYear, EndYear int }

func (s Shard) label() string {
	if s.StartYear == s.EndYear {
		return strconv.Itoa(s.StartYear)
	}
	return fmt.Sprintf("%d-%d", s.StartYear, s.EndYear)
}

// Shards returns model-year shards, each small enough to page through under
// the ceiling. Everything before `from` goes in one shard: old stock is thin
// on a dealer site.
func Shards(from, to int) []Shard {
	shards := []Shard{{StartYear: 1900, EndYear: from - 1}}
	for year := from; year <= to; year++ {
		shards = append(shards, Shard{StartYear: year, EndYear: year})
	}
	return shards
}

// DefaultShards is Shards(2005, next year), as the JS computes it at run time.
func DefaultShards(now time.Time) []Shard { return Shards(FirstShardYear, now.Year()+1) }

// URLOptions are the parts of a search URL that vary.
type URLOptions struct {
	Scope              Scope
	StartYear, EndYear *int
	Offset             int
}

// BuildURL is the JS buildCargurusUrl. Parameter order matches it, so the URL
// is byte-identical.
func BuildURL(o URLOptions) string {
	scope := o.Scope
	if scope.Zip == "" {
		scope.Zip = Montreal.Zip
	}
	if scope.Distance == 0 {
		scope.Distance = Montreal.Distance
	}
	var b strings.Builder
	b.WriteString(Origin + "/Cars/searchResults.action")
	sep := byte('?')
	add := func(k, v string) {
		b.WriteByte(sep)
		sep = '&'
		b.WriteString(url.QueryEscape(k) + "=" + url.QueryEscape(v))
	}
	add("zip", scope.Zip)
	add("distance", strconv.Itoa(scope.Distance))
	add("inventorySearchWidgetType", "AUTO")
	add("shopByTypes", "NEAR_BY")
	// Newest listing first, so a new-listings run can stop once pages go quiet.
	// NEWEST_CAR_FIRST sounds like it but sorts by something else entirely: its
	// first page ran 34, 142, 20, 27 days on market.
	add("sortType", "AGE_IN_DAYS")
	add("sortDir", "ASC")
	if o.StartYear != nil {
		add("startYear", strconv.Itoa(*o.StartYear))
	}
	if o.EndYear != nil {
		add("endYear", strconv.Itoa(*o.EndYear))
	}
	add("offset", strconv.Itoa(o.Offset))
	add("maxResults", strconv.Itoa(PageSize))
	return b.String()
}

var transmissions = map[string]string{"Automatic": "Automatique", "Manual": "Manuelle"}

var fuels = map[string]string{
	"Gasoline": "Essence", "Diesel": "Diesel", "Hybrid": "Hybride",
	"Plug-In Hybrid": "Hybride rechargeable", "Electric": "Électrique",
}

// dealRatings puts CarGurus' verdict on Kijiji's scale so the column means one thing.
var dealRatings = map[string]string{
	"GREAT_PRICE": "GREAT", "GOOD_PRICE": "GOOD", "FAIR_PRICE": "FAIR",
	"HIGH_PRICE": "HIGH", "OVERPRICED": "OVERPRICED", "POOR_PRICE": "POOR",
}

// Extras are the JS fields listing.Listing has no column for yet.
type Extras struct {
	Title       string  `json:"title"`
	ListedAt    *string `json:"listedAt"`
	PriceRating *string `json:"priceRating"`
	VIN         *string `json:"vin"`
}

// jsRound is Math.round: halves go up, also for negatives.
func jsRound(x float64) float64 { return math.Floor(x + 0.5) }

// jsString is String(v) for the scalar values the feed carries.
func jsString(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case float64:
		return jstext.Number(c)
	case bool:
		return strconv.FormatBool(c)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(c)
		return string(b)
	}
}

// str is `raw[key] ?? null`.
func str(raw map[string]any, key string) *string {
	v, ok := raw[key]
	if !ok || v == nil {
		return nil
	}
	return listing.Str(jsString(v))
}

// num is `typeof raw[key] === 'number' ? raw[key] : null`.
func num(raw map[string]any, key string) (float64, bool) {
	f, ok := raw[key].(float64)
	return f, ok
}

// mapped is `table[raw[key]] ?? raw[key] ?? null`.
func mapped(raw map[string]any, key string, table map[string]string) *string {
	v := str(raw, key)
	if v != nil {
		if m, ok := table[*v]; ok {
			return &m
		}
	}
	return v
}

func kilometres(raw map[string]any) *float64 {
	if m, ok := raw["unitMileage"].(map[string]any); ok {
		if v, ok := m["value"].(float64); ok {
			if m["unit"] == "MILES" {
				return listing.Num(jsRound(v * 1.609344))
			}
			return listing.Num(jsRound(v))
		}
	}
	if v, ok := num(raw, "mileage"); ok {
		return listing.Num(v)
	}
	return nil
}

// Normalize maps one raw CarGurus listing (decoded JSON object) to a Listing
// (JS normalizeCargurus). `now` dates listedAt, which is counted back from
// the days on market.
func Normalize(raw map[string]any, now time.Time) (listing.Listing, Extras) {
	id := jsString(raw["id"])
	city := jstext.Trim(strings.SplitN(jsString(raw["sellerCity"]), ",", 2)[0])
	title := ""
	if t := str(raw, "listingTitle"); t != nil {
		title = *t
	}

	l := listing.Listing{
		ID:          listing.Str("cargurus:" + id),
		ReferenceID: listing.Str(id),
		URL:         listing.Str(Origin + "/Cars/inventorylisting/vdp.action?listingId=" + id),
		Source:      "cargurus",

		// Structured fields, unlike LesPAC and Craigslist: nothing is guessed
		// from the title.
		Make:     str(raw, "makeName"),
		Model:    str(raw, "modelName"),
		TrimText: str(raw, "trimName"),

		Km:           kilometres(raw),
		Transmission: mapped(raw, "localizedTransmission", transmissions),
		Fuel:         mapped(raw, "localizedFuelType", fuels),
		// The search feed publishes no accident or title flag.
		Condition: listing.Str("U"),

		SellerType: listing.Str("Dealer"),
		SellerID:   str(raw, "sellerId"),
		SellerName: str(raw, "serviceProviderName"),

		PostalCode: str(raw, "sellerPostalCode"),
		Province:   str(raw, "sellerRegion"),
		ImageURLs:  []string{},
		ResultType: str(raw, "inclusionType"),
	}
	if p, ok := num(raw, "price"); ok && p > 0 {
		l.Price = listing.Num(jsRound(p))
	}
	if p, ok := num(raw, "expectedPrice"); ok {
		l.SuggestedRetailPrice = listing.Num(jsRound(p))
	}
	if y, ok := num(raw, "carYear"); ok {
		l.Year = listing.Num(y)
	}
	if l.TrimText == nil {
		l.TrimText = listing.Str(title)
	}
	if raw["sellerType"] == "PRIVATE" {
		l.SellerType = listing.Str("PrivateSeller")
	}
	if l.SellerName == nil {
		l.SellerName = str(raw, "dealerName")
	}
	if city != "" {
		l.City = &city
	}
	pic := ""
	if p, ok := raw["originalPictureData"].(map[string]any); ok {
		pic, _ = p["url"].(string)
	}
	if n, ok := num(raw, "pictureCount"); ok && n != 0 {
		l.ImageCount = int(n)
	} else if pic != "" {
		l.ImageCount = 1
	}
	if pic != "" {
		l.ImageURLs = []string{pic}
	}
	if l.ResultType != nil && *l.ResultType == "DEFAULT" {
		l.ResultType = listing.Str("Organic")
	}

	x := Extras{Title: title, VIN: str(raw, "vin")}
	// Only "days on market" is published; the date is counted back from now.
	if d, ok := num(raw, "daysOnMarket"); ok {
		at := now.Add(-time.Duration(d * float64(24*time.Hour))).UTC()
		x.ListedAt = listing.Str(at.Format("2006-01-02T15:04:05.000Z"))
	}
	if r, ok := dealRatings[jsString(raw["dealRating"])]; ok {
		x.PriceRating = &r
	}
	return l, x
}

// Page is one parsed response.
type Page struct {
	Listings []listing.Listing
	Extras   []Extras // parallel to Listings
	// End: the endpoint had nothing more to give — which past the ceiling is
	// not the same as done.
	End bool
}

func parseErr(msg string) error { return &fetch.ParseError{Msg: msg} }

// ParsePage reads one searchResults.action body (JS parseCargurusPage).
func ParsePage(body string, now time.Time) (Page, error) {
	if body == "" {
		return Page{}, parseErr("empty response body")
	}
	var payload any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		// The captcha page is HTML.
		return Page{}, parseErr("CarGurus answered with a page, not JSON — likely a captcha")
	}
	if payload == nil {
		return Page{Listings: []listing.Listing{}, End: true}, nil
	}
	items, ok := payload.([]any)
	if !ok {
		return Page{}, parseErr("CarGurus response was not a listing array")
	}
	p := Page{Listings: []listing.Listing{}, End: len(items) < PageSize}
	for _, item := range items {
		raw, ok := item.(map[string]any)
		if !ok || raw["id"] == nil {
			continue
		}
		l, x := Normalize(raw, now)
		p.Listings = append(p.Listings, l)
		p.Extras = append(p.Extras, x)
	}
	return p, nil
}

// StopAfterKnownPages is how many consecutive all-known pages end a
// new-listings run (JS STOP_AFTER_KNOWN_PAGES).
const StopAfterKnownPages = 3

// Option configures a Source.
type Option func(*Source)

// WithShards replaces the default year shards.
func WithShards(shards []Shard) Option { return func(s *Source) { s.shards = shards } }

// WithKnown turns on a new-listings run (JS mode "new"): each shard stops
// after StopAfterKnownPages consecutive pages holding only ids known reports
// as already seen. Such a run is not a full sweep and reports Truncated.
func WithKnown(known func(id string) bool) Option { return func(s *Source) { s.known = known } }

// WithClock sets the time listedAt is counted back from and the default
// shards end at.
func WithClock(now func() time.Time) Option { return func(s *Source) { s.now = now } }

// WithLog receives the per-shard progress lines the JS crawler logs.
func WithLog(log func(string)) Option { return func(s *Source) { s.log = log } }

// Source searches cargurus.ca.
type Source struct {
	fetcher fetch.Fetcher
	shards  []Shard
	known   func(id string) bool
	now     func() time.Time
	log     func(string)
}

// New returns a CarGurus source that fetches through f. f must send Headers:
// use NewHTTPFetcher (optionally throttled) for the live site.
func New(f fetch.Fetcher, opts ...Option) *Source {
	s := &Source{fetcher: f, now: time.Now, log: func(string) {}}
	for _, o := range opts {
		o(s)
	}
	return s
}

var _ source.Source = (*Source)(nil)

// Name implements source.Source.
func (s *Source) Name() string { return "cargurus" }

// Walk is a Search with the CarGurus-specific detail kept.
type Walk struct {
	Scope Scope
	source.Result
	Extras          map[string]Extras // by listing id
	TruncatedShards int
	// StoppedEarly: a new-listings run went quiet before the end.
	StoppedEarly bool
}

// shardsFor clips the shards to the query's year bounds. The site applies
// startYear/endYear itself, so this saves requests without filtering locally.
func (s *Source) shardsFor(q listing.Query) []Shard {
	shards := s.shards
	if shards == nil {
		shards = DefaultShards(s.now())
	}
	out := []Shard{}
	for _, sh := range shards {
		if q.YearFrom != nil && float64(sh.StartYear) < *q.YearFrom {
			sh.StartYear = int(math.Ceil(*q.YearFrom))
		}
		if q.YearTo != nil && float64(sh.EndYear) > *q.YearTo {
			sh.EndYear = int(math.Floor(*q.YearTo))
		}
		if sh.StartYear <= sh.EndYear {
			out = append(out, sh)
		}
	}
	return out
}

// Walk pages every year shard of the Geo's scope to its end or to the offset
// ceiling (JS crawlCargurus, minus the database writes). A shard still full
// at the ceiling is truncated, and so is the whole result: coverage unknown.
// q.MaxPages, when set, caps pages per shard. Make, model, price and seller
// type are not supported by this source and are ignored.
func (s *Source) Walk(ctx context.Context, q listing.Query, onPage func(source.PageEvent)) (Walk, error) {
	scope, err := ScopeFor(q.Geo)
	if err != nil {
		return Walk{}, err
	}
	now := s.now()
	shards := s.shardsFor(q)
	maxOffset := OffsetCeiling
	if q.MaxPages > 0 && q.MaxPages*PageSize < maxOffset {
		maxOffset = q.MaxPages * PageSize
	}

	w := Walk{Scope: scope, Extras: map[string]Extras{}}
	var order []string
	collected := map[string]listing.Listing{}

	for _, shard := range shards {
		// Every shard is its own newest-first list, so each gets its own count.
		quiet := 0
		offset := 0
		reachedEnd := false
		for ; offset < maxOffset; offset += PageSize {
			start, end := shard.StartYear, shard.EndYear
			u := BuildURL(URLOptions{Scope: scope, StartYear: &start, EndYear: &end, Offset: offset})
			if w.URL == "" {
				w.URL = u
			}
			body, err := s.fetcher.Fetch(ctx, u)
			if err != nil {
				return Walk{}, err
			}
			page, err := ParsePage(body, now)
			if err != nil {
				return Walk{}, err
			}
			w.PagesWalked++
			fresh := 0
			for i, l := range page.Listings {
				key := l.Key()
				if _, seen := collected[key]; !seen {
					order = append(order, key)
				}
				collected[key] = l
				w.Extras[key] = page.Extras[i]
				if s.known != nil && !s.known(key) {
					fresh++
				}
			}
			if onPage != nil {
				onPage(source.PageEvent{Page: offset/PageSize + 1, Of: maxOffset / PageSize, Found: len(page.Listings)})
			}
			if page.End {
				reachedEnd = true
				break
			}
			if s.known != nil {
				if fresh == 0 {
					quiet++
				} else {
					quiet = 0
				}
				if quiet >= StopAfterKnownPages {
					w.StoppedEarly = true
					break
				}
			}
		}
		if !reachedEnd && offset >= maxOffset {
			w.TruncatedShards++
			if maxOffset == OffsetCeiling {
				w.HitSiteCeiling = true
			}
			s.log(fmt.Sprintf("  ! %s: hit the %d ceiling — split this shard", shard.label(), maxOffset))
		} else {
			s.log(fmt.Sprintf("  %s: %d page(s)", shard.label(), offset/PageSize+1))
		}
	}

	w.Listings = make([]listing.Listing, 0, len(order))
	for _, k := range order {
		w.Listings = append(w.Listings, collected[k])
	}
	w.Truncated = w.TruncatedShards > 0 || w.StoppedEarly
	return w, nil
}

// Search implements source.Source.
func (s *Source) Search(ctx context.Context, q listing.Query, onPage func(source.PageEvent)) (source.Result, error) {
	w, err := s.Walk(ctx, q, onPage)
	if err != nil {
		return source.Result{}, err
	}
	return w.Result, nil
}
