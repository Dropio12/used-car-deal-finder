// Package craigslist is the Craigslist implementation of source.Source.
// Port of src/craigslist.js and the crawlCraigslist part of src/crawl.js.
//
// Small in Québec (probed 2026-09-24: Montréal 243 cars, 35 private; Québec
// City and Sherbrooke one each). Ontario is where Craigslist is big.
//
// Transport: the search page is rendered in the browser from a JSON feed at
// sapi.craigslist.org. The feed is public, needs no cookie, and carries the
// odometer, the post date and a true total. It is compact rather than
// self-describing — see DecodeItem. What it does not carry (description,
// transmission, title status) is on the post page, read by ParsePost.
package craigslist

import (
	"context"
	"encoding/json"
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

// API is the search feed the browser page is rendered from.
const API = "https://sapi.craigslist.org/web/v8/postings/search/full"

// PageSize is the most items one feed call returns. A Québec area never comes
// close, so one call covers it; a bigger area would need `batch` paging.
const PageSize = 360

// DefaultReadPages caps how many post pages one crawl opens.
const DefaultReadPages = 100

// Area is one Craigslist site: the subdomain, the area id the feed wants, and
// the province its cars are in.
type Area struct {
	Host     string
	ID       int
	Province string
}

// Areas are the Craigslist sites this crawler knows, keyed by geo name. The
// key is the subdomain except for London, whose subdomain is "londonon"
// ("london" is the UK). Every id was confirmed against Craigslist's own area
// list (reference.craigslist.org/Areas, 2026-09-29). Trois-Rivières and
// Saguenay are left out: they redirect to the regional hub and have no cars.
var Areas = map[string]Area{
	"montreal":   {"montreal", 49, "QC"},
	"quebec":     {"quebec", 175, "QC"},
	"sherbrooke": {"sherbrooke", 390, "QC"},

	"toronto":      {"toronto", 25, "ON"},
	"ottawa":       {"ottawa", 76, "ON"},
	"hamilton":     {"hamilton", 213, "ON"},
	"kitchener":    {"kitchener", 214, "ON"},
	"london":       {"londonon", 234, "ON"},
	"windsor":      {"windsor", 235, "ON"},
	"niagara":      {"niagara", 386, "ON"},
	"guelph":       {"guelph", 482, "ON"},
	"barrie":       {"barrie", 389, "ON"},
	"kingston":     {"kingston", 385, "ON"},
	"peterborough": {"peterborough", 388, "ON"},
	"sudbury":      {"sudbury", 384, "ON"},
}

// DefaultArea is searched when the query names none.
const DefaultArea = "montreal"

// Seller maps a seller type to the feed's search path: `cta` is everything,
// `cto` and `ctd` split it by who is selling.
var Seller = map[string]string{"all": "cta", "private": "cto", "dealer": "ctd"}

// categorySeller maps the per-item category id to a seller type.
var categorySeller = map[string]string{"145": "PrivateSeller", "146": "Dealer"}

// BuildURL is the feed URL for one area and seller type (JS buildCraigslistUrl).
// An unknown area is an error rather than a default: the feed answers a missing
// area id with San Francisco.
func BuildURL(area, sellerType string) (string, error) {
	a, ok := Areas[area]
	if !ok {
		return "", fmt.Errorf("unknown Craigslist area %q", area)
	}
	path, ok := Seller[sellerType]
	if !ok {
		return "", fmt.Errorf("unknown Craigslist seller type %q", sellerType)
	}
	return fmt.Sprintf("%s?batch=%d-0-%d-0-0&cc=CA&lang=en&searchPath=%s", API, a.ID, PageSize, path), nil
}

// Headers are what the feed checks, as a browser's call would carry.
func Headers(area string) map[string]string {
	host := area
	if a, ok := Areas[area]; ok {
		host = a.Host
	}
	return map[string]string{
		"accept":  "application/json, text/plain, */*",
		"referer": "https://" + host + ".craigslist.org/",
	}
}

// HeaderFetcher is a Fetcher that can add request headers. The feed call goes
// through it when the source's fetcher has it; a plain Fetcher sends none.
type HeaderFetcher interface {
	fetch.Fetcher
	FetchWithHeaders(ctx context.Context, url string, headers map[string]string) (string, error)
}

// HTTP adapts a fetch.HTTPFetcher, optionally throttled, into a HeaderFetcher.
type HTTP struct {
	Client   *fetch.HTTPFetcher
	Throttle *fetch.Throttle // nil: no spacing
}

var _ HeaderFetcher = HTTP{}

// Fetch implements fetch.Fetcher.
func (h HTTP) Fetch(ctx context.Context, url string) (string, error) {
	return h.FetchWithHeaders(ctx, url, nil)
}

// FetchWithHeaders implements HeaderFetcher on a copy of the client, so the
// shared one is never mutated.
func (h HTTP) FetchWithHeaders(ctx context.Context, url string, headers map[string]string) (string, error) {
	c := *h.Client
	if len(headers) > 0 {
		c.Headers = map[string]string{}
		for k, v := range h.Client.Headers {
			c.Headers[k] = v
		}
		for k, v := range headers {
			c.Headers[k] = v
		}
	}
	var f fetch.Fetcher = &c
	if h.Throttle != nil {
		f = h.Throttle.Wrap(f)
	}
	return f.Fetch(ctx, url)
}

// Decoded is one feed item with its fields named (JS decodeItem's return).
// AreaID is the raw JSON value (nil when absent), compared strictly as JS did.
type Decoded struct {
	PostingID    float64
	PostedAt     *string
	CategoryID   any
	Price        *float64
	AreaID       any
	LocationText *string
	Latitude     *float64
	Longitude    *float64
	Odometer     *float64
	Images       []string
	URL          *string
	Title        *string
}

// Decode is the feed's `data.decode` block, kept raw.
type Decode map[string]any

var leadingSize = regexp.MustCompile(`^\d+:`)

// imageURL: "3:00M0M_abc" — the prefix is a size hint, the rest is the id.
func imageURL(token any) string {
	id := leadingSize.ReplaceAllString(jsString(token), "")
	if id == "" {
		return ""
	}
	return "https://images.craigslist.org/" + id + "_600x450.jpg"
}

// DecodeItem turns one positional feed item into named fields, or nil when it
// is not an item.
//
// The first six slots are fixed:
//
//	[postingIdOffset, postedDateOffset, categoryId, price, "loc:desc~lat~lon", thumb]
//
// and the id and date are offsets from decode.minPostingId and
// decode.minPostedDate. Then come tagged slots [tag, value...] in no promised
// order, and finally the title as a bare string:
//
//	4 image tokens   6 URL slug   9 odometer   10 price as shown   13 URL token
func DecodeItem(item []any, decode Decode) *Decoded {
	if len(item) < 6 {
		return nil
	}
	idOffset, dateOffset, categoryID, price, place := item[0], item[1], item[2], item[3], item[4]

	tagged := map[float64][]any{}
	var title *string
	for _, slot := range item[6:] {
		switch s := slot.(type) {
		case []any:
			// Only numeric tags are ever looked up.
			if len(s) > 0 {
				if tag, ok := s[0].(float64); ok {
					tagged[tag] = s[1:]
				}
			}
		case string:
			title = listing.Str(s)
		}
	}
	first := func(tag float64) (any, bool) {
		v, ok := tagged[tag]
		if !ok || len(v) == 0 {
			return nil, false
		}
		return v[0], true
	}

	placeText := ""
	if place != nil {
		placeText = jsString(place)
	}
	parts := strings.Split(placeText, "~")
	where := parts[0]
	lat, lon := at(parts, 1), at(parts, 2)
	idx := strings.Split(where, ":")
	locationIndex := numberFromString(idx[0])
	descriptionIndex := math.NaN()
	if len(idx) > 1 {
		descriptionIndex = numberFromString(idx[1])
	}
	location, _ := index(decode["locations"], locationIndex)

	d := &Decoded{
		PostingID:  jsNumber(decode["minPostingId"], has(decode, "minPostingId")) + jsNumber(idOffset, true),
		CategoryID: categoryID,
		Title:      title,
		Images:     []string{},
	}
	if seconds := jsNumber(decode["minPostedDate"], has(decode, "minPostedDate")) + jsNumber(dateOffset, true); !math.IsNaN(seconds) && !math.IsInf(seconds, 0) {
		d.PostedAt = isoString(seconds * 1000)
	}
	if p, ok := price.(float64); ok && p > 0 {
		d.Price = listing.Num(p)
	}
	if loc, ok := location.([]any); ok && len(loc) > 0 {
		d.AreaID = loc[0]
	}
	if descriptionIndex > 0 {
		if text, ok := index(decode["locationDescriptions"], descriptionIndex); ok {
			// JS kept any value here and then called .match on it; only text
			// can be a location line.
			if s, isText := text.(string); isText {
				d.LocationText = listing.Str(s)
			}
		}
	}
	if lat != nil && *lat != "" {
		d.Latitude = finite(numberFromString(*lat))
	}
	if lon != nil && *lon != "" {
		d.Longitude = finite(numberFromString(*lon))
	}
	if km, ok := first(9); ok {
		if n, isNum := km.(float64); isNum {
			d.Odometer = listing.Num(n)
		}
	}
	for _, token := range tagged[4] {
		if u := imageURL(token); u != "" {
			d.Images = append(d.Images, u)
		}
	}
	slug, _ := first(6)
	token, _ := first(13)
	if truthy(slug) && truthy(token) {
		d.URL = listing.Str("https://www.craigslist.org/view/d/" + jsString(slug) + "/" + jsString(token))
	}
	return d
}

// Vehicle is what a title matcher reads out of a free-text title.
type Vehicle struct {
	Make, Model *string
	Year        *float64
}

// Matcher reads make, model and year out of a title (JS createVehicleMatcher).
type Matcher func(title string) Vehicle

// Extra holds what the JS listing carried that listing.Listing has no field
// for. Keyed by listing id in Crawl.
type Extra struct {
	Title      string   `json:"title"`
	ListedAt   *string  `json:"listedAt"`
	VIN        *string  `json:"vin"`
	Latitude   *float64 `json:"latitude"`
	Longitude  *float64 `json:"longitude"`
	PageReadAt *string  `json:"pageReadAt"`
}

// The location line is free text: sellers put a VIN, a phone number or a
// street address in it. A VIN is worth keeping; the rest is only a city if it
// does not look like something else.
var (
	vinRe      = regexp.MustCompile(`\b[A-HJ-NPR-Za-hj-npr-z0-9]{17}\b`)
	notACityRe = regexp.MustCompile(`(?i)\d{3}[` + jstext.SpaceClass + `.-]?\d{4}|call|appel|\$`)
)

func readLocation(text *string) (city, vin *string) {
	if text == nil || *text == "" {
		return nil, nil
	}
	if m := vinRe.FindString(*text); m != "" {
		vin = listing.Str(strings.ToUpper(m))
	}
	junk := vin != nil || notACityRe.MatchString(*text) || len(utf16.Encode([]rune(*text))) > 40
	if !junk {
		city = listing.Str(jstext.Trim(*text))
	}
	return city, vin
}

// Normalize maps a decoded item into a Listing (JS normalizeCraigslist).
// match may be nil, which leaves make, model and year unset.
func Normalize(d *Decoded, match Matcher, province string) (listing.Listing, Extra) {
	title := ""
	if d.Title != nil {
		title = *d.Title
	}
	var v Vehicle
	if match != nil {
		v = match(title)
	}
	city, vin := readLocation(d.LocationText)
	id := jstext.Number(d.PostingID)

	images := d.Images
	if len(images) > parse.MaxStoredImages {
		images = images[:parse.MaxStoredImages]
	}
	l := listing.Listing{
		ID: listing.Str("craigslist:" + id), ReferenceID: listing.Str(id), URL: d.URL, Source: "craigslist",
		Price: d.Price,
		Year:  v.Year, Make: v.Make, Model: v.Model, TrimText: listing.Str(title),
		// Canadian areas report distance in km, and sellers fill the odometer
		// in the same unit: a 2006 Avalon reads 159 000, not 99 000.
		Km: d.Odometer,
		// Transmission, fuel and the damage flags are not in the feed; the
		// post page has them (MergePost).
		Condition: listing.Str("U"),
		City:      city, Province: listing.Str(province),
		ImageCount: len(d.Images), ImageURLs: append([]string{}, images...),
		ResultType: listing.Str("Organic"),
	}
	if s, ok := categorySeller[jsString(d.CategoryID)]; ok && d.CategoryID != nil {
		l.SellerType = listing.Str(s)
	}
	return l, Extra{Title: title, ListedAt: d.PostedAt, VIN: vin, Latitude: d.Latitude, Longitude: d.Longitude}
}

// SearchPage is one parsed feed response.
type SearchPage struct {
	Total     *float64
	Listings  []listing.Listing
	Extras    []Extra // parallel to Listings
	OutOfArea int
}

func parseErr(format string, a ...any) error {
	return &fetch.ParseError{Msg: fmt.Sprintf(format, a...)}
}

// ParseSearch reads a feed response (JS parseCraigslistSearch).
//
// A thin area is padded with postings from nearby areas: Québec City's one car
// came back with 59 neighbours. Those belong to the other area's crawl, and
// keeping them would let this scope's removal detection retire them, so they
// are dropped and counted in OutOfArea.
func ParseSearch(body string, match Matcher, area string) (SearchPage, error) {
	if body == "" {
		return SearchPage{}, parseErr("empty response body")
	}
	var payload any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return SearchPage{}, parseErr("Craigslist feed was not JSON — a block page, or the API moved: %v", err)
	}
	root, _ := payload.(map[string]any)
	data, _ := root["data"].(map[string]any)
	items, isList := data["items"].([]any)
	if data == nil || !isList || !truthy(data["decode"]) {
		return SearchPage{}, parseErr("Craigslist feed had no items/decode — the response shape changed")
	}
	decode, _ := data["decode"].(map[string]any)

	a, known := Areas[area]
	province := "QC"
	if known {
		province = a.Province
	}
	page := SearchPage{Listings: []listing.Listing{}, Extras: []Extra{}}
	if t, ok := data["totalResultCount"].(float64); ok {
		page.Total = listing.Num(t)
	}
	decoded := 0
	for _, raw := range items {
		arr, ok := raw.([]any)
		if !ok {
			continue
		}
		d := DecodeItem(arr, decode)
		if d == nil {
			continue
		}
		decoded++
		if known && d.AreaID != nil {
			if id, isNum := d.AreaID.(float64); !isNum || id != float64(a.ID) {
				continue
			}
		}
		l, x := Normalize(d, match, province)
		page.Listings = append(page.Listings, l)
		page.Extras = append(page.Extras, x)
	}
	page.OutOfArea = decoded - len(page.Listings)
	return page, nil
}

// Post is what only a post page carries (JS parseCraigslistPost).
type Post struct {
	Year         *float64
	MakeModel    *string
	Km           *float64
	Transmission *string
	Fuel         *string
	TitleStatus  *string
	Condition    *string
	VIN          *string
	Description  *string
}

var (
	transmissions = map[string]string{"automatic": "Automatique", "manual": "Manuelle"}
	fuels         = map[string]string{"gas": "Essence", "diesel": "Diesel", "hybrid": "Hybride", "electric": "Électrique"}

	// badTitle: title statuses that mean the car was written off at some point.
	badTitle  = regexp.MustCompile(`(?i)salvage|rebuilt|parts only|lien`)
	partsOnly = regexp.MustCompile(`(?i)parts only`)

	postBody  = regexp.MustCompile(`(?s)<section id="postingbody">(.*?)</section>`)
	printInfo = regexp.MustCompile(`(?s)<div class="print-information.*?</div>[` + jstext.SpaceClass + `]*</div>`)
	yearRe    = regexp.MustCompile(`<span class="valu year">[` + jstext.SpaceClass + `]*(\d{4})[` + jstext.SpaceClass + `]*<`)
	makeModel = regexp.MustCompile(`(?s)<span class="valu makemodel">(.*?)</span>`)
	nonDigits = regexp.MustCompile(`[^\d]`)
)

// ParsePost reads a post page. It returns nil, nil for a page with no posting
// on it: Craigslist answers a deleted post with HTTP 200 and a notice.
func ParsePost(html string) (*Post, error) {
	if html == "" {
		return nil, parseErr("empty response body")
	}
	if !strings.Contains(html, `id="postingbody"`) {
		return nil, nil
	}
	attr := func(name string) *string {
		re := regexp.MustCompile(`(?s)class="attr ` + regexp.QuoteMeta(name) + `".*?<span class="valu">(.*?)</span>`)
		m := re.FindStringSubmatch(html)
		if m == nil {
			return nil
		}
		if s := parse.StripHTML(m[1]); s != nil && *s != "" {
			return listing.Str(strings.ToLower(*s))
		}
		return nil
	}

	body := ""
	if m := postBody.FindStringSubmatch(html); m != nil {
		body = m[1]
	}
	// The body opens with a print-only QR block; the seller's text follows it.
	if loc := printInfo.FindStringIndex(body); loc != nil {
		body = body[:loc[0]] + body[loc[1]:]
	}
	p := &Post{
		TitleStatus: attr("auto_title_status"),
		Condition:   attr("condition"),
	}
	if d := parse.StripHTML(body); *d != "" {
		p.Description = d
	}
	if m := yearRe.FindStringSubmatch(html); m != nil {
		if y, _ := strconv.ParseFloat(m[1], 64); y != 0 {
			p.Year = listing.Num(y)
		}
	}
	mm := ""
	if m := makeModel.FindStringSubmatch(html); m != nil {
		mm = m[1]
	}
	if s := parse.StripHTML(mm); *s != "" {
		p.MakeModel = s
	}
	if odo := attr("auto_miles"); odo != nil {
		if km := numberFromString(nonDigits.ReplaceAllString(*odo, "")); km != 0 && !math.IsNaN(km) {
			p.Km = listing.Num(km)
		}
	}
	if t := attr("auto_transmission"); t != nil {
		if v, ok := transmissions[*t]; ok {
			p.Transmission = listing.Str(v)
		}
	}
	if f := attr("auto_fuel_type"); f != nil {
		if v, ok := fuels[*f]; ok {
			p.Fuel = listing.Str(v)
		}
	}
	if vin := attr("auto_vin"); vin != nil {
		p.VIN = listing.Str(strings.ToUpper(*vin))
	}
	return p, nil
}

// MergePost folds a post page into the listing the feed produced (JS
// mergeCraigslistPost). The feed's values win where both have one: it is the
// fresher read of the same ad. A nil post leaves both unchanged.
func MergePost(l listing.Listing, x Extra, post *Post, readAt string) (listing.Listing, Extra) {
	if post == nil {
		return l, x
	}
	damage := describe.Read(post.Description)
	status := ""
	if post.TitleStatus != nil {
		status = *post.TitleStatus
	}
	if l.Year == nil {
		l.Year = post.Year
	}
	if l.Km == nil {
		l.Km = post.Km
	}
	l.Transmission = post.Transmission
	l.Fuel = post.Fuel
	l.Description = post.Description
	l.IsDamaged = damage.IsDamaged || (post.TitleStatus != nil && badTitle.MatchString(status))
	l.IsParts = damage.IsParts || partsOnly.MatchString(status)
	if post.VIN != nil {
		x.VIN = post.VIN
	}
	x.PageReadAt = listing.Str(readAt)
	return l, x
}

// Source searches one Craigslist area per query.
type Source struct {
	fetcher fetch.Fetcher
	// Match reads the car out of the title. Nil leaves make/model/year unset.
	Match Matcher
	// ReadPages caps post pages opened per crawl (JS readPages). Default 100.
	ReadPages int
	// Stored reports a listing whose post page was already read, with the
	// description it gave; such a listing is not re-read and keeps that
	// description, which a feed-only write would otherwise blank. Nil: none.
	Stored func(id string) (description *string, read bool)
	// Log receives progress lines. Nil: silent.
	Log func(string)
	// Now stamps pageReadAt. Nil: time.Now.
	Now func() time.Time
}

// Option configures a Source.
type Option func(*Source)

// WithMatcher sets the title matcher.
func WithMatcher(m Matcher) Option { return func(s *Source) { s.Match = m } }

// WithReadPages sets the post-page budget per crawl; 0 reads none.
func WithReadPages(n int) Option { return func(s *Source) { s.ReadPages = n } }

// WithStored sets the lookup for listings already read.
func WithStored(f func(id string) (*string, bool)) Option { return func(s *Source) { s.Stored = f } }

// WithLog sets the progress logger.
func WithLog(f func(string)) Option { return func(s *Source) { s.Log = f } }

// New returns a Craigslist source that fetches through f. When f is a
// HeaderFetcher, the feed call carries Headers(area).
func New(f fetch.Fetcher, opts ...Option) *Source {
	s := &Source{fetcher: f, ReadPages: DefaultReadPages}
	for _, o := range opts {
		o(s)
	}
	return s
}

var _ source.Source = (*Source)(nil)

// Name implements source.Source.
func (s *Source) Name() string { return "craigslist" }

func (s *Source) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log(fmt.Sprintf(format, a...))
	}
}

// Crawl is one area's crawl with everything JS returned.
type Crawl struct {
	source.Result
	Area        string
	Extras      map[string]Extra // by listing id
	OutOfArea   int
	PagesRead   int
	PagesFailed int
}

func areaAndSeller(q listing.Query) (string, string) {
	area := q.Geo
	if area == "" {
		area = DefaultArea
	}
	seller := "all"
	switch q.SellerType {
	case "P":
		seller = "private"
	case "D":
		seller = "dealer"
	}
	return area, seller
}

// Crawl fetches one area's feed, then opens the post page of each listing not
// read before, before it is written: transmission, fuel and the damage flags
// are only set on insert, so reading the page later would be too late. One
// feed request covers a whole Québec area; a total above PageSize is reported
// as Truncated.
func (s *Source) Crawl(ctx context.Context, q listing.Query) (Crawl, error) {
	area, seller := areaAndSeller(q)
	url, err := BuildURL(area, seller)
	if err != nil {
		return Crawl{}, err
	}
	var body string
	if hf, ok := s.fetcher.(HeaderFetcher); ok {
		body, err = hf.FetchWithHeaders(ctx, url, Headers(area))
	} else {
		body, err = s.fetcher.Fetch(ctx, url)
	}
	if err != nil {
		return Crawl{}, err
	}
	page, err := ParseSearch(body, s.Match, area)
	if err != nil {
		return Crawl{}, err
	}
	total := "?"
	if page.Total != nil {
		total = jstext.Number(*page.Total)
	}
	s.logf("  %d of %s listings (+%d from nearby areas skipped)", len(page.Listings), total, page.OutOfArea)

	now := s.Now
	if now == nil {
		now = time.Now
	}
	readAt := now().UTC().Format("2006-01-02T15:04:05.000Z")

	c := Crawl{Area: area, Extras: map[string]Extra{}, OutOfArea: page.OutOfArea}
	c.URL, c.Total, c.Pages, c.PagesWalked = url, page.Total, listing.Num(1), 1
	ready := make([]listing.Listing, 0, len(page.Listings))
	for i, l := range page.Listings {
		x := page.Extras[i]
		if s.Stored != nil {
			if desc, read := s.Stored(l.Key()); read {
				l.Description = desc
				ready, c.Extras[l.Key()] = append(ready, l), x
				continue
			}
		}
		if c.PagesRead+c.PagesFailed >= s.ReadPages || l.URL == nil || *l.URL == "" {
			ready, c.Extras[l.Key()] = append(ready, l), x
			continue
		}
		html, err := s.fetcher.Fetch(ctx, *l.URL)
		var post *Post
		if err == nil {
			post, err = ParsePost(html)
		}
		if err != nil {
			if ctx.Err() != nil {
				return Crawl{}, ctx.Err()
			}
			// One dead post must not cost the others their write.
			c.PagesFailed++
			msg := err.Error()
			if len(msg) > 80 {
				msg = msg[:80]
			}
			s.logf("  ! %s: %s", l.Key(), msg)
			ready, c.Extras[l.Key()] = append(ready, l), x
			continue
		}
		c.PagesRead++
		l, x = MergePost(l, x, post, readAt)
		ready, c.Extras[l.Key()] = append(ready, l), x
	}
	s.logf("  read %d post page(s), %d failed", c.PagesRead, c.PagesFailed)

	if page.Total != nil && float64(len(page.Listings)) < *page.Total {
		c.Shortfall = int(*page.Total) - len(page.Listings)
		c.ShortfallRatio = float64(c.Shortfall) / *page.Total
		c.Truncated = *page.Total > PageSize
	}
	c.Listings = applyLocalFilters(ready, q)
	c.LocallyFiltered = q.YearFrom != nil || q.YearTo != nil
	return c, nil
}

// applyLocalFilters honours the Query's year bounds, as every source does;
// the JS crawl had none to apply.
func applyLocalFilters(listings []listing.Listing, q listing.Query) []listing.Listing {
	if q.YearFrom == nil && q.YearTo == nil {
		return listings
	}
	out := []listing.Listing{}
	for _, l := range listings {
		if l.Year == nil || (q.YearFrom != nil && *l.Year < *q.YearFrom) || (q.YearTo != nil && *l.Year > *q.YearTo) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// Search implements source.Source: one feed call per area, so one page.
func (s *Source) Search(ctx context.Context, q listing.Query, onPage func(source.PageEvent)) (source.Result, error) {
	c, err := s.Crawl(ctx, q)
	if err != nil {
		return source.Result{}, err
	}
	if onPage != nil {
		onPage(source.PageEvent{Page: 1, Of: 1, Found: len(c.Listings), Total: c.Total})
	}
	return c.Result, nil
}

// --- JavaScript value rules the feed decoding depends on -------------------

func has(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

func at(parts []string, i int) *string {
	if i < len(parts) {
		return &parts[i]
	}
	return nil
}

// index is `array?.[n]` for a JSON value: only a whole, in-range n hits.
func index(v any, n float64) (any, bool) {
	arr, ok := v.([]any)
	if !ok || n != math.Trunc(n) || n < 0 || n >= float64(len(arr)) {
		return nil, false
	}
	return arr[int(n)], true
}

func finite(f float64) *float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return listing.Num(f)
}

// truthy is JavaScript's Boolean(v) for a decoded JSON value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	}
	return true
}

// jsString is String(v) for a decoded JSON value (JSON null is "null").
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return jstext.Number(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e != nil {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// jsNumber is Number(v); present=false is `undefined`, which is NaN.
func jsNumber(v any, present bool) float64 {
	if !present {
		return math.NaN()
	}
	switch x := v.(type) {
	case nil:
		return 0
	case bool:
		if x {
			return 1
		}
		return 0
	case float64:
		return x
	case []any:
		if len(x) == 0 {
			return 0
		}
		if len(x) == 1 {
			if x[0] == nil {
				return 0
			}
			return numberFromString(jsString(x[0]))
		}
		return math.NaN()
	case string:
		return numberFromString(x)
	}
	return math.NaN()
}

var decimalRe = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)

// numberFromString is Number(string): trimmed, "" is 0, junk is NaN.
func numberFromString(s string) float64 {
	s = jstext.Trim(s)
	switch s {
	case "":
		return 0
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if len(s) > 2 && s[0] == '0' {
		base := map[byte]int{'x': 16, 'X': 16, 'o': 8, 'O': 8, 'b': 2, 'B': 2}[s[1]]
		if base != 0 {
			if n, err := strconv.ParseUint(s[2:], base, 64); err == nil && !strings.Contains(s, "_") {
				return float64(n)
			}
			return math.NaN()
		}
	}
	if !decimalRe.MatchString(s) {
		return math.NaN()
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !math.IsInf(f, 0) {
		return math.NaN()
	}
	return f
}

// isoString is new Date(ms).toISOString(), or nil where JS would throw.
func isoString(ms float64) *string {
	ms = math.Trunc(ms)
	if math.Abs(ms) > 8.64e15 {
		return nil
	}
	t := time.UnixMilli(int64(ms)).UTC()
	y := t.Year()
	rest := t.Format("-01-02T15:04:05.000Z")
	if y >= 0 && y <= 9999 {
		return listing.Str(fmt.Sprintf("%04d%s", y, rest))
	}
	sign := "+"
	if y < 0 {
		sign, y = "-", -y
	}
	return listing.Str(fmt.Sprintf("%s%06d%s", sign, y, rest))
}
