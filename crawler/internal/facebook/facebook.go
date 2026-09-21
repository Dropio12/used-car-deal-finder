// Package facebook is the Facebook Marketplace implementation of
// source.Source, read anonymously. Port of src/facebook.js and of the
// crawlFacebook / enrichListings / splitByFreshness parts of src/crawl.js and
// src/watch.js.
//
// No account, no cookies, no proxies. Facebook embeds a slice of real listing
// data in the page it serves to a logged-out browser: title, formatted price,
// a "183 k km" subtitle, city and province, creation time and the sold flag.
// Only 15-24 come back per request, so coverage comes from sharding the view
// by price band, sort order, model query and city.
//
// The search feed carries no description; the listing's own page does, along
// with the full gallery and the odometer as a number (see ParseItem). So:
// shard the feed to discover, open the page for anything that scores.
//
// The exposure is an IP throttle, per-path and temporary. Keep the rate low
// (wrap the fetcher in a fetch.Throttle) and stop on the first login wall.
package facebook

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"carbuyer/crawler/internal/describe"
	"carbuyer/crawler/internal/fetch"
	"carbuyer/crawler/internal/jstext"
	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/parse"
	"carbuyer/crawler/internal/source"
)

// Origin is where every Marketplace URL starts.
const Origin = "https://www.facebook.com"

// SourceName is the value stored in listings.source. The free and the Apify
// paths write the same one on purpose: they are the same cars by two routes.
const SourceName = "marketplace"

// PageCeiling is the most records one response carries. A full page means
// "there are more, you cannot see them"; anything short means exhausted.
const PageCeiling = 24

// giveUpAfter is how many consecutive refusals from one city end its sweep.
const giveUpAfter = 6

// Sorts is the second sharding dimension. Each sort returns a *different* 24
// for the same query, because the cap is on the response and not on the
// result set: 20 bands with one sort reached 268 Montréal cars, the same 20
// bands across these three reached 997.
var Sorts = []string{"creation_time_descend", "price_ascend", "distance_ascend"}

// Headers is the browser-shaped header set Facebook requires. Without it the
// site answers HTTP 400 regardless of the content being public. fetch.Fetcher
// carries no headers, so set these on the HTTP client (NewHTTPFetcher does).
var Headers = map[string]string{
	"accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
	"accept-language":           "fr-CA,fr;q=0.9,en-CA;q=0.8,en;q=0.7",
	"sec-fetch-dest":            "document",
	"sec-fetch-mode":            "navigate",
	"sec-fetch-site":            "none",
	"sec-fetch-user":            "?1",
	"upgrade-insecure-requests": "1",
}

// NewHTTPFetcher returns the default HTTP fetcher with Headers applied.
func NewHTTPFetcher() *fetch.HTTPFetcher {
	f := fetch.NewHTTPFetcher()
	f.Headers = map[string]string{}
	for k, v := range Headers {
		f.Headers[k] = v
	}
	return f
}

// City is one Marketplace place and the province its results must be in.
//
// Token is the path segment. Guessing slugs fails *silently*: `sherbrooke`
// and `troisrivieres` answer 200 with Ottawa cars, `kingston` and `kitchener`
// with Montréal cars, and `london` serves London, England. Only a few words
// work; everything else is a city page id harvested from a feed
// (`"city_page":{"display_name":…,"id":…}`).
type City struct {
	Key, Token, Label, Province string
}

// GeoSlug is the scope a city's listings are stored under ("fbmp-montreal").
func (c City) GeoSlug() string { return "fbmp-" + c.Key }

// QuebecCities are the four Québec metro markets, each verified live to serve
// its own region.
var QuebecCities = []City{
	{Key: "montreal", Token: "montreal", Label: "Montréal", Province: "QC"},
	{Key: "quebecCity", Token: "quebec", Label: "Québec", Province: "QC"},
	{Key: "troisRivieres", Token: "110315092330541", Label: "Trois-Rivières", Province: "QC"},
	{Key: "sherbrooke", Token: "116139965062971", Label: "Sherbrooke", Province: "QC"},
}

// OntarioCities were verified live on 2026-09-25, each with a distance-sorted
// feed from its own city. Words only work for toronto, ottawa and windsor.
// Mississauga, Brampton, Vaughan and Markham are inside the toronto feed.
var OntarioCities = []City{
	{Key: "toronto", Token: "toronto", Label: "Toronto", Province: "ON"},
	{Key: "ottawa", Token: "ottawa", Label: "Ottawa", Province: "ON"},
	{Key: "hamilton", Token: "104011556303312", Label: "Hamilton", Province: "ON"},
	{Key: "kitchener", Token: "104045032964460", Label: "Kitchener-Waterloo", Province: "ON"},
	{Key: "london", Token: "107624535933778", Label: "London", Province: "ON"},
	{Key: "windsor", Token: "windsor", Label: "Windsor", Province: "ON"},
	{Key: "kingston", Token: "105443806157102", Label: "Kingston", Province: "ON"},
	{Key: "oshawa", Token: "114418101908145", Label: "Oshawa", Province: "ON"},
	{Key: "barrie", Token: "110893392264912", Label: "Barrie", Province: "ON"},
	{Key: "stCatharines", Token: "106063096092020", Label: "St. Catharines-Niagara", Province: "ON"},
	{Key: "guelph", Token: "106514426051396", Label: "Guelph", Province: "ON"},
	{Key: "sudbury", Token: "101877776521079", Label: "Sudbury", Province: "ON"},
	{Key: "peterborough", Token: "107401009289940", Label: "Peterborough", Province: "ON"},
	{Key: "thunderBay", Token: "111551465530472", Label: "Thunder Bay", Province: "ON"},
}

// Cities resolves listing.Query.Geo to the cities to sweep:
//
//	""  or "quebec"   every Québec city (the JS default, CARBUYER_REGION unset)
//	"ontario"         every Ontario city
//	a city key        that one city ("montreal", "toronto", "stCatharines")
//
// Keys match case-insensitively. An unknown name is an error, never a guess:
// an unknown token would silently serve another region.
func Cities(geo string) ([]City, error) {
	g := strings.ToLower(strings.TrimSpace(geo))
	switch g {
	case "", "quebec":
		return QuebecCities, nil
	case "ontario":
		return OntarioCities, nil
	}
	for _, list := range [][]City{QuebecCities, OntarioCities} {
		for _, c := range list {
			if strings.ToLower(c.Key) == g {
				return []City{c}, nil
			}
		}
	}
	return nil, fmt.Errorf("unknown Facebook geo %q: use quebec, ontario or a city key such as montreal or toronto", geo)
}

// Vehicle is what a title matcher identifies.
type Vehicle struct {
	Make, Model *string
	Year        *float64
}

// Matcher identifies make/model/year from free text (the shared dictionary
// matcher, lespac.js createVehicleMatcher). Facebook publishes only a title,
// so without one every car parses fine and prices as "no comparable market".
type Matcher func(title, description string) Vehicle

// Item is a Marketplace listing plus what listing.Listing has no field for.
// The embedded fields keep the flat JSON shape of the JS object.
type Item struct {
	listing.Listing
	Title *string `json:"title"`
	// ISO time the seller posted it, from creation_time.
	ListedAt *string `json:"listedAt"`
	IsSold   bool    `json:"isSold"`
	// Only the Apify path carries coordinates.
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
	// The scope it was found under ("fbmp-montreal"); not part of the JS listing.
	GeoSlug string `json:"-"`
	// Set only when the item page was actually opened (Enrich).
	PageReadAt *string `json:"pageReadAt,omitempty"`
}

// ---------------------------------------------------------------------------
// URLs

// URLOptions is one shard's request. Nil prices are left out.
type URLOptions struct {
	Place              string
	MinPrice, MaxPrice *int
	SortBy             string
	Query              string
}

// SearchURL builds a Marketplace URL. A category path is required: a bare
// location produces a search with no query, which returns nothing while still
// looking like a success. A query switches to the /search path.
func SearchURL(o URLOptions) string {
	place := o.Place
	if place == "" {
		place = "montreal"
	}
	path := "/cars"
	var params [][2]string
	if o.Query != "" {
		path = "/search"
		params = append(params, [2]string{"query", o.Query})
	}
	if o.MinPrice != nil {
		params = append(params, [2]string{"minPrice", strconv.Itoa(*o.MinPrice)})
	}
	if o.MaxPrice != nil {
		params = append(params, [2]string{"maxPrice", strconv.Itoa(*o.MaxPrice)})
	}
	if o.SortBy != "" {
		params = append(params, [2]string{"sortBy", o.SortBy})
	}
	return Origin + "/marketplace/" + place + path + formEncode(params)
}

// ItemURL is the public page for one listing, from either id form.
func ItemURL(id string) string {
	return Origin + "/marketplace/item/" + strings.TrimPrefix(id, "fbmp:")
}

// formEncode is URLSearchParams.toString(): insertion order kept (url.Values
// sorts), space as '+', and only [A-Za-z0-9*-._] left unescaped.
func formEncode(params [][2]string) string {
	if len(params) == 0 {
		return ""
	}
	var b strings.Builder
	for i, p := range params {
		if i == 0 {
			b.WriteByte('?')
		} else {
			b.WriteByte('&')
		}
		b.WriteString(formEscape(p[0]))
		b.WriteByte('=')
		b.WriteString(formEscape(p[1]))
	}
	return b.String()
}

func formEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '*', c == '-', c == '.', c == '_':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Parsing

var (
	unicodeRun = regexp.MustCompile(`(?:\\u[0-9a-fA-F]{4})+`)
	// Marks the start of one record. A record carries three "id" fields (the
	// listing, its cover photo, the city page); only anchoring on the record
	// and reading forward takes the right one. The city id repeats for every
	// car in a city, so a wrong pick collapses a page to one car per city.
	record = regexp.MustCompile(`"listing":\{(?:"__typename":"[^"]*",)?"id":"(\d+)"`)

	titleRe    = regexp.MustCompile(`"marketplace_listing_title":"((?:[^"\\]|\\.)*)"`)
	priceRe    = regexp.MustCompile(`"listing_price":\{"formatted_amount":"((?:[^"\\]|\\.)*)"`)
	subtitleRe = regexp.MustCompile(`"subtitle":"((?:[^"\\]|\\.)*)"`)
	cityRe     = regexp.MustCompile(`"reverse_geocode":\{"city":"((?:[^"\\]|\\.)*)"`)
	stateRe    = regexp.MustCompile(`"state":"([A-Z]{2})"`)
	createdRe  = regexp.MustCompile(`"creation_time":(\d+)`)
	photoRe    = regexp.MustCompile(`"uri":"(https:\\?/\\?/scontent[^"]+)"`)
	soldRe     = regexp.MustCompile(`"is_sold":true`)

	compactKm = regexp.MustCompile(`(?i)(\d+(?:[.,]\d+)?)[` + jstext.SpaceClass + `]*k[` + jstext.SpaceClass + `]*km`)
	nonDigit  = regexp.MustCompile(`[^\d]`)

	uriRe       = regexp.MustCompile(`"uri":"((?:[^"\\]|\\.)*)"`)
	descRe      = regexp.MustCompile(`"redacted_description":\{"text":"((?:[^"\\]|\\.)*)"`)
	odometerRe  = regexp.MustCompile(`"vehicle_odometer_data":\{"unit":"([A-Z]+)","value":(\d+)`)
	subjectRe   = regexp.MustCompile(`"listing_price":\{[^{}]*"amount":"(\d+(?:\.\d+)?)"\},"__isMarketplaceVehicleListing"`)
	anyTagRe    = regexp.MustCompile(`<[^>]+>`)
	postalProv  = map[string]string{"QC": "QC", "ON": "ON", "NB": "NB", "NS": "NS"}
	defaultProv = "QC"
)

// decode undoes the JSON string escaping the embedded payload uses, turns
// non-breaking spaces into spaces and trims. Returns nil for nil.
func decode(s *string) *string {
	if s == nil {
		return nil
	}
	out := unicodeRun.ReplaceAllStringFunc(*s, func(run string) string {
		units := make([]uint16, 0, len(run)/6)
		for i := 0; i+6 <= len(run); i += 6 {
			n, _ := strconv.ParseUint(run[i+2:i+6], 16, 16)
			units = append(units, uint16(n))
		}
		return string(utf16.Decode(units))
	})
	out = strings.ReplaceAll(out, `\/`, "/")
	out = strings.ReplaceAll(out, `\"`, `"`)
	out = strings.ReplaceAll(out, "\u00a0", " ")
	out = jstext.Trim(out)
	return &out
}

// group returns capture 1 of the first match, or nil.
func group(re *regexp.Regexp, s string) *string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	return &m[1]
}

// jsRound is Math.round (halves toward +infinity).
func jsRound(x float64) float64 { return math.Floor(x + 0.5) }

// ParsePrice reads "4 500 C$" as 4500. "1 C$" is 1, which the price model
// then rejects; junk is nil rather than zero.
func ParsePrice(formatted *string) *float64 {
	text := decode(formatted)
	if text == nil {
		return nil
	}
	digits := nonDigit.ReplaceAllString(*text, "")
	if digits == "" {
		return nil
	}
	n, err := strconv.ParseFloat(digits, 64)
	if err != nil {
		return nil
	}
	return &n
}

// ParseMileage reads "183 k km" as 183000 and "87 000 km" as 87000.
func ParseMileage(subtitle *string) *float64 {
	text := decode(subtitle)
	if text == nil || *text == "" {
		return nil
	}
	if m := compactKm.FindStringSubmatch(*text); m != nil {
		f, err := strconv.ParseFloat(strings.Replace(m[1], ",", ".", 1), 64)
		if err == nil {
			return listing.Num(jsRound(f * 1000))
		}
	}
	digits := nonDigit.ReplaceAllString(*text, "")
	if digits == "" {
		return nil
	}
	n, err := strconv.ParseFloat(digits, 64)
	if err != nil {
		return nil
	}
	return &n
}

// isoSeconds formats a Unix time like Date.prototype.toISOString.
func isoSeconds(sec float64) string {
	ms := int64(sec * 1000)
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

// SearchPage is one parsed search response.
type SearchPage struct {
	Listings []Item
	// Empty is a page that reached Marketplace with nothing in it: a real
	// answer (Sherbrooke has no $38 000 cars), not a block.
	Empty bool
}

// ParseOptions tune ParseSearch.
type ParseOptions struct {
	Match Matcher
	// Province assigned when the record's state is missing or not one of
	// QC/ON/NB/NS. The JS used the running instance's province; pass the
	// searched city's. Defaults to "QC".
	Province string
}

// ParseSearch pulls listings out of the ~950 KB embedded payload. Each record
// is sliced out by its boundaries and read on its own, so no field can bleed
// across listings.
func ParseSearch(html string, o ParseOptions) (SearchPage, error) {
	if html == "" {
		return SearchPage{}, &fetch.ParseError{Msg: "empty response body"}
	}
	if !strings.Contains(html, "marketplace_listing_title") {
		// The feed container is the tell: present means the page reached
		// Marketplace and the band is empty; absent means it never got there.
		// Login wording cannot decide it, since every logged-out page has some.
		if strings.Contains(html, "MarketplaceSearchFeedStories") {
			return SearchPage{Listings: []Item{}, Empty: true}, nil
		}
		return SearchPage{}, &fetch.ParseError{Msg: "never reached Marketplace — a login wall, a block, or the page shape changed"}
	}
	province := o.Province
	if province == "" {
		province = defaultProv
	}

	starts := record.FindAllStringSubmatchIndex(html, -1)
	listings := []Item{}
	seen := map[string]bool{}
	for i, m := range starts {
		id := html[m[2]:m[3]]
		if seen[id] {
			continue
		}
		end := len(html)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		window := html[m[0]:end]
		title := decode(group(titleRe, window))
		// A record without a title is not a car listing (Facebook mixes other
		// story shapes into the same feed).
		if title == nil || *title == "" {
			continue
		}
		seen[id] = true

		vehicle := Vehicle{}
		if o.Match != nil {
			vehicle = o.Match(*title, "")
		}
		prov := province
		if st := group(stateRe, window); st != nil {
			if p, ok := postalProv[*st]; ok {
				prov = p
			}
		}
		var listedAt *string
		if c := group(createdRe, window); c != nil {
			if sec, err := strconv.ParseFloat(*c, 64); err == nil {
				listedAt = listing.Str(isoSeconds(sec))
			}
		}
		images := []string{}
		if photo := group(photoRe, window); photo != nil {
			images = append(images, *decode(photo))
		}

		listings = append(listings, Item{
			Listing: listing.Listing{
				ID:          listing.Str("fbmp:" + id),
				ReferenceID: listing.Str(id),
				URL:         listing.Str(ItemURL(id)),
				Source:      SourceName,
				Price:       ParsePrice(group(priceRe, window)),
				Make:        vehicle.Make,
				Model:       vehicle.Model,
				Year:        vehicle.Year,
				TrimText:    title,
				Km:          ParseMileage(group(subtitleRe, window)),
				Condition:   listing.Str("U"),
				// Anonymously the seller is null, so private is assumed: it
				// prices the car against the lower baseline.
				SellerType: listing.Str("PrivateSeller"),
				City:       decode(group(cityRe, window)),
				Province:   &prov,
				// Not served to anonymous callers. Nil rather than empty keeps
				// "not fetched" distinct from "no description".
				Description: nil,
				ImageCount:  len(images),
				ImageURLs:   images,
				ResultType:  listing.Str("Organic"),
			},
			Title:    title,
			ListedAt: listedAt,
			IsSold:   soldRe.MatchString(window),
		})
	}
	return SearchPage{Listings: listings}, nil
}

// KeepInRegion drops results from another province, and refuses a page that
// is entirely from one.
//
// Facebook *pads* a thin band (five Québec cars then ten from Ottawa): those
// go, the real five stay. An unrecognised place token answers 200 with a full
// page of another region and nothing local: that is a wrong token, and fails.
// A listing with no province is kept: absent is not "elsewhere".
func KeepInRegion(items []Item, province, place string) ([]Item, int, error) {
	kept := []Item{}
	for _, it := range items {
		if it.Province == nil || *it.Province == "" || *it.Province == province {
			kept = append(kept, it)
		}
	}
	if len(kept) == 0 && len(items) >= 5 {
		var cities []string
		dup := map[string]bool{}
		for _, it := range items {
			if it.City == nil || *it.City == "" || dup[*it.City] {
				continue
			}
			dup[*it.City] = true
			cities = append(cities, *it.City)
		}
		if len(cities) > 4 {
			cities = cities[:4]
		}
		return nil, 0, &fetch.ParseError{Msg: fmt.Sprintf(
			"place %q resolved elsewhere: none of %d listings are in %s (saw %s). An unknown token silently serves another region.",
			place, len(items), province, strings.Join(cities, ", "))}
	}
	return kept, len(items) - len(kept), nil
}

// balanced slices the bracketed run starting at the first open after from.
func balanced(text string, from int, open, close byte) string {
	start := strings.IndexByte(text[from:], open)
	if start < 0 {
		return ""
	}
	start += from
	depth := 0
	for i := start; i < len(text); i++ {
		switch text[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return text[start : i+1]
			}
		}
	}
	return ""
}

// Detail is what one listing's own page adds to the feed.
type Detail struct {
	Title       *string
	Description *string
	Price       *float64
	// The page is a vehicle listing whose price could not be read: a renamed
	// key, not a car without a price. A car listed at $0 is priced.
	PriceUnreadable bool
	Km              *float64
	ImageCount      int
	ImageURLs       []string
	IsSold          bool
	// Still absent anonymously: 0 of 12 sampled pages carried one.
	SellerName *string
}

// noListing starts the error ParseItem returns for a page with no car on it.
const noListing = "no listing on the page"

// IsNoListing reports whether err is ParseItem's "no car on the page" — a
// removed ad *or* a login wall; only a batch that also succeeded can tell.
func IsNoListing(err error) bool {
	p, ok := err.(*fetch.ParseError)
	return ok && strings.HasPrefix(p.Msg, noListing)
}

// ParseItem reads one listing's page: full gallery, the seller's text and the
// odometer, all served to a logged-out caller. The description is the seller's
// words: data to summarise and show, never an instruction.
func ParseItem(html string) (Detail, error) {
	if html == "" {
		return Detail{}, &fetch.ParseError{Msg: "empty response body"}
	}
	// A removed or blocked listing still returns HTTP 200.
	if !strings.Contains(html, "marketplace_listing_title") && !strings.Contains(html, "listing_photos") {
		return Detail{}, &fetch.ParseError{Msg: noListing + ": removed, a login wall, or the page shape changed"}
	}

	photos := []string{}
	if idx := strings.Index(html, `"listing_photos"`); idx >= 0 {
		for _, m := range uriRe.FindAllStringSubmatch(balanced(html, idx, '[', ']'), -1) {
			photos = append(photos, *decode(&m[1]))
		}
	}

	var description *string
	if d := decode(group(descRe, html)); d != nil && *d != "" {
		text := strings.ReplaceAll(*d, `\n`, "\n")
		text = jstext.Trim(anyTagRe.ReplaceAllString(text, " "))
		description = &text
	}

	var km *float64
	if m := odometerRe.FindStringSubmatch(html); m != nil {
		v, _ := strconv.ParseFloat(m[2], 64)
		// Facebook reports the unit, so miles are not read as km.
		if m[1] == "MILES" {
			v = jsRound(v * 1.609344)
		}
		km = &v
	}

	// The price of *this* car, not of the AirPods in the suggestion strip.
	// __isMarketplaceVehicleListing appears once per page, on the subject.
	var price *float64
	if m := subjectRe.FindStringSubmatch(html); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		price = listing.Num(jsRound(v))
	}
	isVehicle := strings.Contains(html, "__isMarketplaceVehicleListing")

	stored := photos
	if len(stored) > parse.MaxStoredImages {
		stored = stored[:parse.MaxStoredImages]
	}
	return Detail{
		Title:           decode(group(titleRe, html)),
		Description:     description,
		Price:           price,
		PriceUnreadable: isVehicle && price == nil,
		Km:              km,
		ImageCount:      len(photos),
		ImageURLs:       stored,
		IsSold:          soldRe.MatchString(html),
	}, nil
}

// ---------------------------------------------------------------------------
// Sharding

// Shard is one request's scope. Depth counts price splits of a model query
// and never reaches the URL.
type Shard struct {
	Query              string
	MinPrice, MaxPrice *int
	SortBy             string
	Depth              int
}

func (s Shard) urlOptions(place string) URLOptions {
	return URLOptions{Place: place, MinPrice: s.MinPrice, MaxPrice: s.MaxPrice, SortBy: s.SortBy, Query: s.Query}
}

func intp(n int) *int { return &n }

// PriceBands tiles 0..max in steps. Narrow bands at the bottom are where the
// inventory is, but narrowing them is not the lever sort order is.
func PriceBands(max, step int) []Shard {
	if step <= 0 {
		return nil
	}
	var bands []Shard
	for lo := 0; lo < max; lo += step {
		bands = append(bands, Shard{MinPrice: intp(lo), MaxPrice: intp(lo + step)})
	}
	return bands
}

// SweepPlan is every (band, sort) request needed to sweep a city. It never
// sends daysSinceListed or radius: Facebook accepts both and ignores both.
func SweepPlan(max, step int, sorts []string) []Shard {
	if sorts == nil {
		sorts = Sorts
	}
	var plan []Shard
	for _, b := range PriceBands(max, step) {
		for _, s := range sorts {
			b.SortBy = s
			plan = append(plan, b)
		}
	}
	return plan
}

// ModelPlan is the third dimension: name the model. Band and sort cap
// Montréal at ~1 400 cars; a model query reaches cars they never show.
// Newest-first only by default: reach beats a second sort of the same 24.
func ModelPlan(models []string, sorts []string) []Shard {
	if sorts == nil {
		sorts = []string{"creation_time_descend"}
	}
	var plan []Shard
	for _, m := range models {
		for _, s := range sorts {
			plan = append(plan, Shard{Query: m, SortBy: s})
		}
	}
	return plan
}

// SplitOptions bound SplitShard.
type SplitOptions struct {
	Max, Step, MaxDepth, Into int
}

// DefaultSplit is what the crawl uses.
var DefaultSplit = SplitOptions{Max: 40_000, Step: 8000, MaxDepth: 2, Into: 4}

// SplitShard says what to ask next, given what a shard returned. A full model
// query hides cars behind the ceiling, so it is split by price; a short one is
// exhausted. Depth 1 tiles the range, depth 2 subdivides whichever band stayed
// full. Past MaxDepth it stops and the caller counts it truncated. The plain
// band-and-sort sweep (no query) is never split.
func SplitShard(s Shard, count int, o SplitOptions) []Shard {
	if count < PageCeiling || s.Query == "" || s.Depth >= o.MaxDepth {
		return nil
	}
	if s.Depth == 0 {
		var out []Shard
		for _, b := range PriceBands(o.Max, o.Step) {
			f := s
			f.MinPrice, f.MaxPrice, f.Depth = b.MinPrice, b.MaxPrice, 1
			out = append(out, f)
		}
		return out
	}
	// Only this slice is known to be full, so only it is subdivided.
	lo, hi := 0.0, float64(o.Max)
	if s.MinPrice != nil {
		lo = float64(*s.MinPrice)
	}
	if s.MaxPrice != nil {
		hi = float64(*s.MaxPrice)
	}
	width := (hi - lo) / float64(o.Into)
	if width < 1 {
		return nil
	}
	out := make([]Shard, o.Into)
	for i := range out {
		f := s
		f.MinPrice = intp(int(jsRound(lo + float64(i)*width)))
		f.MaxPrice = intp(int(jsRound(lo + float64(i+1)*width)))
		f.Depth = s.Depth + 1
		out[i] = f
	}
	return out
}

// SplitByFreshness separates "just posted" from "new to us". After a full
// sweep a car can be new to the database and already thirteen days old.
// Facebook's "Date listed" filter is ignored as a URL parameter, so the cut is
// made here on creation_time. The window has to clear the worst detection lag
// (a city comes round once per rotation), not the poll interval. An item with
// no date is fresh: silently discarding it is the worse error. 0 = no limit.
func SplitByFreshness(items []Item, maxAge time.Duration, now time.Time) (fresh, backfill []Item) {
	if maxAge <= 0 {
		return items, nil
	}
	for _, it := range items {
		if it.ListedAt != nil {
			if posted, err := time.Parse(time.RFC3339Nano, *it.ListedAt); err == nil && now.Sub(posted) > maxAge {
				backfill = append(backfill, it)
				continue
			}
		}
		fresh = append(fresh, it)
	}
	return fresh, backfill
}

// ---------------------------------------------------------------------------
// Source

// Source searches Facebook Marketplace anonymously.
type Source struct {
	fetcher     fetch.Fetcher
	match       Matcher
	models      []string
	sorts       []string
	modelSorts  []string
	maxPrice    int
	step        int
	maxRequests int
	newOnly     bool
	maxAge      time.Duration
	now         func() time.Time
	log         func(string)
}

// Option configures a Source.
type Option func(*Source)

// WithMatcher identifies make/model/year from titles.
func WithMatcher(m Matcher) Option { return func(s *Source) { s.match = m } }

// WithModels adds model queries ("honda civic") after the band sweep. They
// split by price while they come back full.
func WithModels(models ...string) Option { return func(s *Source) { s.models = models } }

// WithSorts narrows the sweep's sort orders (default Sorts).
func WithSorts(sorts ...string) Option { return func(s *Source) { s.sorts = sorts } }

// WithModelSorts sets the sorts for model queries (default newest-first).
func WithModelSorts(sorts ...string) Option { return func(s *Source) { s.modelSorts = sorts } }

// WithBands sets the band sweep's ceiling and step (default 40 000 / 2 000).
func WithBands(max, step int) Option { return func(s *Source) { s.maxPrice, s.step = max, step } }

// WithMaxRequests caps successful requests per city (default 400, 80 when new-only).
func WithMaxRequests(n int) Option { return func(s *Source) { s.maxRequests = n } }

// WithNewOnly reads the newest-first page only, instead of the band sweep.
func WithNewOnly() Option { return func(s *Source) { s.newOnly = true } }

// WithMaxAge keeps only listings posted within d (see SplitByFreshness).
func WithMaxAge(d time.Duration) Option { return func(s *Source) { s.maxAge = d } }

// WithNow replaces the clock (tests).
func WithNow(now func() time.Time) Option { return func(s *Source) { s.now = now } }

// WithLog receives the crawl's progress lines.
func WithLog(log func(string)) Option { return func(s *Source) { s.log = log } }

// New returns a Marketplace source that fetches through f. f must send
// Headers (see NewHTTPFetcher) and should be throttled.
func New(f fetch.Fetcher, opts ...Option) *Source {
	s := &Source{fetcher: f, maxPrice: 40_000, step: 2000, now: time.Now, log: func(string) {}}
	for _, o := range opts {
		o(s)
	}
	return s
}

var _ source.Source = (*Source)(nil)

// Name implements source.Source.
func (s *Source) Name() string { return SourceName }

// Stats is what a sweep did, including what it could not do.
type Stats struct {
	Shards      int // requests that answered with a page
	Blocked     int // the site answered, but not with a page (login wall, 4xx)
	Offline     int // no answer at all: our side of the wire
	Truncated   int // split to MaxDepth and still full
	OverBudget  int // shards never asked (budget or give-up)
	OutOfRegion int // padding from other provinces, dropped
	Regions     int // cities that answered at least once
	Backfill    int // dropped by WithMaxAge as too old
	Identified  int // with make, model and year
}

// Sweep is a whole crawl: the distinct items found and the stats.
type Sweep struct {
	URL   string
	Items []Item
	Stats Stats
}

func ptrOr(p *int, fallback string) string {
	if p == nil {
		return fallback
	}
	return strconv.Itoa(*p)
}

// Sweep walks every city Geo names, over band x sort (or newest-first only)
// plus model queries, splitting full model queries by price. Port of
// crawlFacebook minus the database write.
//
// q.Make/q.Model, when set, replace the band sweep with one model query
// ("honda civic"). q.PriceTo sets the sweep's ceiling, q.MaxPages the
// per-city request budget; year bounds are applied locally.
func (s *Source) Sweep(ctx context.Context, q listing.Query, onPage func(source.PageEvent)) (Sweep, error) {
	cities, err := Cities(q.Geo)
	if err != nil {
		return Sweep{}, err
	}
	maxPrice := s.maxPrice
	if q.PriceTo != nil {
		maxPrice = int(*q.PriceTo)
	}
	models := s.models
	var bands []Shard
	if named := strings.ToLower(strings.TrimSpace(q.Make + " " + q.Model)); named != "" {
		models = append([]string{named}, models...)
	} else if s.newOnly {
		bands = []Shard{{SortBy: "creation_time_descend"}}
	} else {
		bands = SweepPlan(maxPrice, s.step, s.sorts)
	}
	planned := append(bands, ModelPlan(models, s.modelSorts)...)

	// Breadth-first per city, so a budget always cuts the deepest splits and
	// never a model's first look.
	maxRequests := s.maxRequests
	if q.MaxPages > 0 {
		maxRequests = q.MaxPages
	}
	if maxRequests <= 0 {
		maxRequests = 400
		if s.newOnly {
			maxRequests = 80
		}
	}

	var out Sweep
	st := &out.Stats
	// Keyed by listing so a car found from two neighbouring cities is stored
	// once. Like a JS Map: first-seen order, last-seen value.
	var order []string
	collected := map[string]Item{}

	for _, city := range cities {
		count, splits, inARow := 0, 0, 0
		queue := append([]Shard(nil), planned...)
		for len(queue) > 0 {
			if err := ctx.Err(); err != nil {
				return Sweep{}, err
			}
			if count >= maxRequests {
				// Said out loud: a cap reported as a finished sweep is a
				// silent success.
				st.OverBudget += len(queue)
				s.log(fmt.Sprintf("  %s: request budget of %d reached, %d shard(s) not asked", city.Key, maxRequests, len(queue)))
				break
			}
			band := queue[0]
			queue = queue[1:]
			url := SearchURL(band.urlOptions(city.Token))
			if out.URL == "" {
				out.URL = url
			}

			var kept []Item
			// Measured before the region filter: the ceiling is a property of
			// the response, and dropping padding first would read a full page
			// as a thin one and never split it.
			returned := 0
			html, err := s.fetcher.Fetch(ctx, url)
			if err == nil {
				var page SearchPage
				page, err = ParseSearch(html, ParseOptions{Match: s.match, Province: city.Province})
				if err == nil {
					returned = len(page.Listings)
					var dropped int
					kept, dropped, err = KeepInRegion(page.Listings, city.Province, city.Key)
					st.OutOfRegion += dropped
				}
			}
			if err != nil {
				if ctx.Err() != nil {
					return Sweep{}, ctx.Err()
				}
				// One band on a login wall is recorded, not fatal.
				if fetch.Answered(err) {
					st.Blocked++
					inARow++
				} else {
					st.Offline++
				}
				sort := ""
				if band.SortBy != "" {
					sort = "/" + strings.SplitN(band.SortBy, "_", 2)[0]
				}
				s.log(fmt.Sprintf("  %s %s %s-%s%s: %s", city.Key, band.Query,
					ptrOr(band.MinPrice, "0"), ptrOr(band.MaxPrice, "∞"), sort, jstext.SliceUTF16(err.Error(), 0, 70)))
				// Hammering a throttle is how a few hours of it becomes days.
				if inARow >= giveUpAfter {
					s.log(fmt.Sprintf("  %s: %d refusals in a row — leaving this region alone", city.Key, inARow))
					st.OverBudget += len(queue)
					break
				}
				continue
			}
			st.Shards++
			count++
			inARow = 0
			for _, it := range kept {
				it.GeoSlug = city.GeoSlug()
				key := it.Key()
				if _, dup := collected[key]; !dup {
					order = append(order, key)
				}
				collected[key] = it
			}
			if onPage != nil {
				onPage(source.PageEvent{Page: st.Shards, Of: st.Shards + len(queue), Found: len(kept)})
			}

			if follow := SplitShard(band, returned, DefaultSplit); len(follow) > 0 {
				queue = append(queue, follow...)
				splits++
			} else if returned >= PageCeiling {
				// Already split, still full: a real limit, said out loud.
				st.Truncated++
			}
		}
		if count > 0 {
			st.Regions++
		}
		extra := ""
		if splits > 0 {
			extra = fmt.Sprintf(" (+%d model queries split by price)", splits)
		}
		s.log(fmt.Sprintf("  %s: %d shards%s, %d distinct so far", city.Label, count, extra, len(order)))
	}

	// Every band failing is never a quiet market. And *why* matters: if
	// nothing answered, it was our own wire, not a Facebook block.
	if st.Shards == 0 {
		attempted := st.Blocked + st.Offline
		if st.Offline == attempted && attempted > 0 {
			return Sweep{}, fmt.Errorf("all %d requests failed with no reply — the network, not the site", attempted)
		}
		return Sweep{}, fmt.Errorf("all %d requests failed — blocked, or the page shape changed", attempted)
	}

	items := make([]Item, 0, len(order))
	for _, k := range order {
		it := collected[k]
		if !inYears(it.Listing, q) {
			continue
		}
		items = append(items, it)
	}
	fresh, backfill := SplitByFreshness(items, s.maxAge, s.now())
	st.Backfill = len(backfill)
	for _, it := range fresh {
		// A car with no make/model/year cannot be priced; counting keeps the
		// matcher's real coverage visible.
		if it.Make != nil && it.Model != nil && it.Year != nil {
			st.Identified++
		}
	}
	out.Items = fresh
	s.log(fmt.Sprintf("  %d of %d identified to make/model/year across %d region(s); %d out-of-province listings dropped",
		st.Identified, len(fresh), st.Regions, st.OutOfRegion))
	return out, nil
}

// inYears applies year bounds locally, dropping cars of unknown year.
func inYears(l listing.Listing, q listing.Query) bool {
	if q.YearFrom == nil && q.YearTo == nil {
		return true
	}
	if l.Year == nil {
		return false
	}
	return (q.YearFrom == nil || *l.Year >= *q.YearFrom) && (q.YearTo == nil || *l.Year <= *q.YearTo)
}

// Search implements source.Source. There is no total to compare against:
// Truncated means a shard stayed at the ceiling or the budget cut the plan.
func (s *Source) Search(ctx context.Context, q listing.Query, onPage func(source.PageEvent)) (source.Result, error) {
	sw, err := s.Sweep(ctx, q, onPage)
	if err != nil {
		return source.Result{}, err
	}
	res := source.Result{
		URL:             sw.URL,
		PagesWalked:     sw.Stats.Shards,
		Listings:        make([]listing.Listing, 0, len(sw.Items)),
		Truncated:       sw.Stats.Truncated > 0 || sw.Stats.OverBudget > 0,
		HitSiteCeiling:  sw.Stats.Truncated > 0,
		LocallyFiltered: q.YearFrom != nil || q.YearTo != nil || s.maxAge > 0,
	}
	for _, it := range sw.Items {
		res.Listings = append(res.Listings, it.Listing)
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Item pages

// Enriched is the outcome of reading item pages for a shortlist.
type Enriched struct {
	Items []Item
	// Ads whose page had no car on it, reported only when another page in the
	// same batch did (proof we are not simply locked out). Safe to retire.
	Gone []string
	// Vehicle pages whose price could not be read: a renamed key.
	Unreadable int
}

// Enrich opens the item page for up to limit Marketplace items and merges in
// description, odometer, price, gallery and the sold flag, re-reading the
// description for damage. One request per car, so run it on a shortlist.
func (s *Source) Enrich(ctx context.Context, items []Item, limit int) Enriched {
	seenAt := isoSeconds(float64(s.now().UnixMilli()) / 1000)
	var out Enriched
	var empty []string
	if limit < len(items) {
		items = items[:limit]
	}
	for _, it := range items {
		if it.Source != SourceName {
			continue
		}
		ref := it.Key()
		if it.ReferenceID != nil {
			ref = *it.ReferenceID
		}
		html, err := s.fetcher.Fetch(ctx, ItemURL(ref))
		var d Detail
		if err == nil {
			d, err = ParseItem(html)
		}
		if err != nil {
			// A listing pulled between the feed and the fetch is normal.
			s.log(fmt.Sprintf("      could not read %s: %s", it.Key(), jstext.SliceUTF16(err.Error(), 0, 60)))
			if IsNoListing(err) {
				empty = append(empty, it.Key())
			}
			continue
		}
		if d.PriceUnreadable {
			out.Unreadable++
		}
		m := it
		if d.Description != nil {
			m.Description = d.Description
		}
		damage := describe.Read(m.Description)
		if d.Km != nil {
			m.Km = d.Km
		}
		if d.Price != nil {
			m.Price = d.Price
		}
		if d.ImageCount != 0 {
			m.ImageCount = d.ImageCount
		}
		if len(d.ImageURLs) > 0 {
			m.ImageURLs = d.ImageURLs
		}
		// Facebook hides sold ads from search, so only the page can say.
		m.IsSold = d.IsSold
		m.PageReadAt = listing.Str(seenAt)
		m.IsDamaged, m.IsParts = damage.IsDamaged, damage.IsParts
		out.Items = append(out.Items, m)
	}
	// An empty body means "gone" and "login wall" alike; one success in the
	// same batch is what proves it was the former.
	if len(empty) > 0 && len(out.Items) > 0 {
		out.Gone = empty
	}
	if out.Unreadable > 0 {
		s.log(fmt.Sprintf("      ! %d of %d page(s) are cars with no readable price"+
			" — ParseItem may be reading a key Facebook has renamed", out.Unreadable, len(out.Items)+out.Unreadable))
	}
	return out
}
