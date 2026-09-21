// Package lespac is the LesPAC implementation of source.Source.
// Port of src/lespac.js and the crawlLespac walk in src/crawl.js.
//
// LesPAC is worth having for its older inventory: it roughly doubles the
// pre-2012 dealer comps, which is where private cars otherwise cannot be
// priced. It is a Québec-only site (src/region.js lists it for Québec only).
//
// Transport: /search/results.jsa returns HTML with the payload embedded in a
// `var searchResponse = {...}` assignment. The data carries no structured
// make/model/year — those come out of the title and description through a
// Matcher built from vocabulary the other sources already stored.
package lespac

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"carbuyer/crawler/internal/describe"
	"carbuyer/crawler/internal/fetch"
	"carbuyer/crawler/internal/jstext"
	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/parse"
	"carbuyer/crawler/internal/source"
)

const (
	// Origin is the site root.
	Origin = "https://www.lespac.com"
	// Category is "cars and trucks".
	Category = 286
	// PageSize is listings per page.
	PageSize = 20
	// MaxPages is the default walk cap (crawlLespac's maxPages).
	MaxPages = 200
	// DefaultMaxDistance is the radius, in km, crawled by default.
	DefaultMaxDistance = 200
	// StopAfterKnownPages ends a new-listings walk after this many consecutive
	// pages with nothing unseen (STOP_AFTER_KNOWN_PAGES in crawl.js).
	StopAfterKnownPages = 3
)

// Advertiser types, as the site spells them.
const (
	AdvertiserPrivate = "INDIVIDUAL"
	AdvertiserDealer  = "CORPORATE"
)

// Place is a search centre and the geographic ids the site wants alongside it.
type Place struct {
	Latitude, Longitude                 float64
	GeographicAreaID, RegionID, GroupID int
}

// Montreal is the default search centre.
var Montreal = Place{
	Latitude: 45.525, Longitude: -73.558333333,
	GeographicAreaID: 17567, RegionID: 17373, GroupID: 285,
}

// URLOptions is what BuildURL takes. Zero MaxDistance means DefaultMaxDistance,
// zero Page means 1, nil Place means Montreal.
type URLOptions struct {
	AdvertiserType   string // AdvertiserPrivate, AdvertiserDealer, or "" for both
	MaxDistance      int    // km from the centre: a real radius, unlike the other sources
	Page             int
	YearMin, YearMax *float64
	Place            *Place
}

// BuildURL is buildLespacUrl. Parameters keep the JS insertion order.
func BuildURL(o URLOptions) string {
	place := Montreal
	if o.Place != nil {
		place = *o.Place
	}
	if o.MaxDistance == 0 {
		o.MaxDistance = DefaultMaxDistance
	}
	if o.Page == 0 {
		o.Page = 1
	}
	params := [][2]string{
		{"pageNumber", fmt.Sprint(o.Page)},
		{"pageSize", fmt.Sprint(PageSize)},
		{"latitude", jstext.Number(place.Latitude)},
		{"longitude", jstext.Number(place.Longitude)},
		{"cityLocation", "true"},
		{"geographicAreaId", fmt.Sprint(place.GeographicAreaID)},
		{"sortOrder", "Distance asc"},
		{"viewType", "detailed"},
		{"localisationMode", "LOCATIONS"},
		{"maxDistance", fmt.Sprint(o.MaxDistance)},
		{"categoryId", fmt.Sprint(Category)},
		{"domCatId", fmt.Sprint(Category)},
		{"domGroupId", fmt.Sprint(place.GroupID)},
		{"groupId", fmt.Sprint(place.GroupID)},
		{"regionId", fmt.Sprint(place.RegionID)},
		{"lang", "fr"},
	}
	if o.AdvertiserType != "" {
		params = append(params, [2]string{"advertiserType", o.AdvertiserType})
	}
	// Range filters are yearMin/yearMax. `year=2006__2011` is accepted and
	// silently ignored — it returns the unfiltered count. Confirm any new
	// filter by checking it appears in the response's `selections`.
	if o.YearMin != nil {
		params = append(params, [2]string{"yearMin", jstext.Number(*o.YearMin)})
	}
	if o.YearMax != nil {
		params = append(params, [2]string{"yearMax", jstext.Number(*o.YearMax)})
	}
	parts := make([]string, len(params))
	for i, p := range params {
		parts[i] = url.QueryEscape(p[0]) + "=" + url.QueryEscape(p[1])
	}
	return Origin + "/search/results.jsa?" + strings.Join(parts, "&")
}

// ---------------------------------------------------------------------------
// Vehicle matcher

// Model is one vocabulary entry. As is the spelling searched for ("Crv");
// Model is the agreed spelling answered with ("CR-V"). Empty As means Model.
type Model struct{ Make, Model, As string }

// Vocabulary is what the matcher is built from (db.vehicleVocabulary in JS).
type Vocabulary struct {
	Makes  []string
	Models []Model
}

// Vehicle is what a Matcher reads out of a title and description.
type Vehicle struct {
	Make, Model *string
	Year        *float64
}

// Matcher reads make, model and year from free text.
type Matcher func(title, description string) Vehicle

// latinFold maps U+00C0..U+017F to the ASCII letter NFD decomposes it to, or
// '_' when it has no canonical decomposition. Stands in for
// normalize('NFD') + stripping U+0300..U+036F without golang.org/x/text.
const latinFold = "" +
	"aaaaaa_ceeeeiiii" + "_nooooo__uuuuy__" + "aaaaaa_ceeeeiiii" + "_nooooo__uuuuy_y" +
	"aaaaaaccccccccdd" + "__eeeeeeeeeegggg" + "gggghh__iiiiiiii" + "i___jjkk_llllll_" +
	"___nnnnnn___oooo" + "oo__rrrrrrssssss" + "sstttt__uuuuuuuu" + "uuuuwwyyyzzzzzz_"

// norm is the one normaliser for both sides: accents stripped, lowercased,
// every run of anything but [a-z0-9] collapsed to one space, trimmed. So
// "CX-5" and "cx 5" meet, and so do "Mercedes-Benz" and "mercedes benz".
func norm(s string) string {
	var b strings.Builder
	space := false
	put := func(c byte) {
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteByte(c)
	}
	for _, r := range s {
		switch {
		case r >= 0x300 && r <= 0x36F:
			// A combining mark vanishes; it does not separate words.
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			put(byte(r))
		case r >= 'A' && r <= 'Z':
			put(byte(r - 'A' + 'a'))
		case r >= 0xC0 && r <= 0x17F && latinFold[r-0xC0] != '_':
			put(latinFold[r-0xC0])
		case r == 0x212A: // KELVIN SIGN, NFD "K"
			put('k')
		case r == 0x212B: // ANGSTROM SIGN, NFD "A" + ring
			put('a')
		default:
			space = true
		}
	}
	return b.String()
}

type indexEntry struct {
	key         string
	make, model string
}

var yearRe = regexp.MustCompile(`\b(19[5-9]\d|20[0-4]\d)\b`)

// NewMatcher is createVehicleMatcher. Longest names are tried first, so
// "Grand Cherokee" wins over "Cherokee".
func NewMatcher(v Vocabulary) Matcher {
	var makeIndex []indexEntry
	seenMake := map[string]bool{}
	for _, m := range v.Makes {
		if seenMake[m] {
			continue
		}
		seenMake[m] = true
		if key := norm(m); key != "" {
			makeIndex = append(makeIndex, indexEntry{key: key, make: m})
		}
	}
	sort.SliceStable(makeIndex, func(i, j int) bool { return len(makeIndex[i].key) > len(makeIndex[j].key) })

	var modelIndex []indexEntry
	seen := map[string]bool{}
	for _, m := range v.Models {
		// Search for the spelling as written, answer with the agreed one.
		as := m.As
		if as == "" {
			as = m.Model
		}
		// Nothing is dropped for being short: Mazda's "3" is a real model. The
		// guard against short names matching noise is at match time.
		key := norm(as)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		modelIndex = append(modelIndex, indexEntry{key: key, make: m.Make, model: m.Model})
	}
	sort.SliceStable(modelIndex, func(i, j int) bool { return len(modelIndex[i].key) > len(modelIndex[j].key) })

	return func(title, description string) Vehicle {
		text := title + " " + description
		haystack := " " + norm(text) + " "

		var out Vehicle
		for _, e := range makeIndex {
			if strings.Contains(haystack, " "+e.key+" ") {
				out.Make = listing.Str(e.make)
				break
			}
		}
		for _, e := range modelIndex {
			if !strings.Contains(haystack, " "+e.key+" ") {
				continue
			}
			// A model must belong to the make found: otherwise "2016 BMW,
			// comme une Fusion" becomes a BMW Fusion, a car that does not exist.
			if out.Make != nil && e.make != *out.Make {
				continue
			}
			// Two-letter names (X5, IS) are also ordinary words; they may not
			// identify the make on their own.
			if out.Make == nil && len(e.key) < 3 {
				continue
			}
			out.Model = listing.Str(e.model)
			if out.Make == nil {
				out.Make = listing.Str(e.make)
			}
			break
		}

		// Plausible model years only, so "82 000 km" or a phone number cannot
		// be read as one. The earliest wins: "2015 Civic, inspecte 2024" is a
		// 2015.
		for _, m := range yearRe.FindAllStringSubmatch(text, -1) {
			var y float64
			fmt.Sscan(m[1], &y)
			if out.Year == nil || y < *out.Year {
				out.Year = listing.Num(y)
			}
		}
		return out
	}
}

// partWords are the tells of a part filed under cars.
var partWords = regexp.MustCompile(`\b(pour|kit|pneus?|jantes?|roues?|mags?|moteur seul|pi[èe]ces?|bumper|pare-choc|console|radio|si[èe]ge|hood|door|porte)\b`)

// LooksLikePart is looksLikePart: "for a Mercedes" is a part, "Mercedes for
// sale" is a car. Parts are cheap and name a donor vehicle, so a price of
// 3000 or more rescues a title that mentions one.
func LooksLikePart(title string, price *float64) bool {
	return partWords.MatchString(strings.ToLower(title)) && (price == nil || *price < 3000)
}

// ---------------------------------------------------------------------------
// Normalization

// Listing is a LesPAC listing: the shared schema plus the fields
// normalizeLespac also returns that listing.Listing has no slot for.
type Listing struct {
	listing.Listing
	Title        string   `json:"title"`
	ListedAt     *string  `json:"listedAt"`
	HadPriceDrop bool     `json:"hadPriceDrop"` // LesPAC flags its own price cuts
	DistanceKm   *float64 `json:"distanceKm"`
}

// jsString is String(v) for JSON values.
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "undefined"
	case string:
		return x
	case float64:
		return jstext.Number(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func optString(v any) *string {
	if v == nil {
		return nil
	}
	s := jsString(v)
	return &s
}

// parseInteger keeps the digits of "82 000 km" or "26 km".
func parseInteger(v any) *float64 {
	if v == nil {
		return nil
	}
	return parse.ParseNumber(jsString(v))
}

func characteristic(raw map[string]any, label string) any {
	list, _ := raw["characteristics"].([]any)
	for _, c := range list {
		if m, ok := c.(map[string]any); ok && m["label"] == label {
			return m["value"]
		}
	}
	return nil
}

// isoMillis is Date.prototype.toISOString for an epoch-millisecond value.
func isoMillis(ms float64) string {
	return time.UnixMilli(int64(ms)).UTC().Format("2006-01-02T15:04:05.000Z")
}

// Normalize is normalizeLespac. match may be nil: make/model/year stay null.
func Normalize(raw map[string]any, match Matcher) Listing {
	title, _ := raw["title"].(string)
	description := ""
	if d := parse.StripHTML(raw["description"]); d != nil {
		description = *d
	}
	var vehicle Vehicle
	if match != nil {
		vehicle = match(title, description)
	}
	// LesPAC publishes no damage flag, so the seller's own words are the only
	// signal. Hardcoding false let parts cars into the comps as whole ones.
	damage := describe.Read(&description)

	var price *float64
	if p, ok := raw["price"].(float64); ok {
		// Already dollars, unlike Kijiji's cents. Math.round rounds .5 up.
		price = listing.Num(math.Floor(p + 0.5))
	}

	ref := ""
	if raw["listingPublicId"] != nil {
		ref = jsString(raw["listingPublicId"])
	}

	sellerType := "PrivateSeller"
	if raw["advertiserType"] == AdvertiserDealer {
		sellerType = "Dealer"
	}

	imageCount := 0
	images, hasImages := raw["images"].([]any)
	if hasImages {
		imageCount = len(images)
	} else if u, _ := raw["mainImageUrl"].(string); u != "" {
		imageCount = 1
	}
	imageURLs := []string{}
	for _, img := range images {
		m, _ := img.(map[string]any)
		u, _ := m["formattableImageUrl"].(string)
		if u == "" {
			continue
		}
		imageURLs = append(imageURLs, strings.Replace(u, "%FORMAT%", "zoomedGallery", 1))
		if len(imageURLs) == 8 {
			break
		}
	}

	var listedAt *string
	if ts, ok := raw["publicReleaseTimestamp"].(float64); ok && ts != 0 {
		listedAt = listing.Str(isoMillis(ts))
	}

	return Listing{
		Listing: listing.Listing{
			ID:          listing.Str("lespac:" + jsString(raw["listingPublicId"])),
			ReferenceID: &ref,
			URL:         optString(raw["listingDisplayUrl"]),
			Source:      "lespac",

			Price: price,

			Year:     vehicle.Year,
			Make:     vehicle.Make,
			Model:    vehicle.Model,
			TrimText: listing.Str(title),

			// Structured on every listing — never parsed out of prose.
			Km:           parseInteger(characteristic(raw, "Kilométrage")),
			Transmission: optString(characteristic(raw, "Transmission")),
			IsDamaged:    damage.IsDamaged,
			IsParts:      damage.IsParts,
			Condition:    listing.Str("U"),

			SellerType: listing.Str(sellerType),

			City: optString(raw["cityLabel"]),
			// No postal code is published; the province comes from the crawl
			// being anchored on a Québec centre with a bounded radius.
			Province: listing.Str("QC"),

			Description: listing.Str(description),
			ImageCount:  imageCount,
			ImageURLs:   imageURLs,

			ResultType:    listing.Str("Organic"),
			ResultSection: optString(raw["priorityPlacementLayoutFlag"]),
		},
		Title:        title,
		ListedAt:     listedAt,
		HadPriceDrop: raw["tag"] == "PRICE_REDUCTION",
		DistanceKm:   parseInteger(raw["distanceLabel"]),
	}
}

// Page is one parsed results page.
type Page struct {
	Total, TotalPages *float64
	Listings          []Listing
	// Skipped counts parts dropped before they could become comps.
	Skipped int
}

var payloadRe = regexp.MustCompile(`var searchResponse = (\{[\s\S]*?\});[` + jstext.SpaceClass + `]*\n`)

func parseErr(format string, a ...any) error {
	return &fetch.ParseError{Msg: fmt.Sprintf(format, a...)}
}

// ParsePage is parseLespacPage.
func ParsePage(html string, match Matcher) (Page, error) {
	if html == "" {
		return Page{}, parseErr("empty response body")
	}
	found := payloadRe.FindStringSubmatch(html)
	if found == nil {
		return Page{}, parseErr("LesPAC searchResponse not found — the page shape changed, or this is a block page")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(found[1]), &payload); err != nil {
		return Page{}, parseErr("LesPAC searchResponse was not valid JSON: %v", err)
	}

	var results []any
	switch r := payload["searchResults"].(type) {
	case nil:
	case []any:
		results = r
	default:
		return Page{}, fmt.Errorf("LesPAC searchResults is not a list")
	}

	page := Page{Listings: []Listing{}}
	if n, ok := payload["totalResults"].(float64); ok {
		page.Total = &n
	}
	if n, ok := payload["totalPages"].(float64); ok {
		page.TotalPages = &n
	}
	for _, r := range results {
		raw, ok := r.(map[string]any)
		if !ok {
			return Page{}, fmt.Errorf("LesPAC search result is not an object")
		}
		l := Normalize(raw, match)
		if LooksLikePart(l.Title, l.Price) {
			page.Skipped++
			continue
		}
		page.Listings = append(page.Listings, l)
	}
	return page, nil
}

// ---------------------------------------------------------------------------
// Source

// Source searches lespac.com.
type Source struct {
	fetcher        fetch.Fetcher
	match          Matcher
	advertiserType string
	maxDistance    int
	place          Place
	known          func(id string) bool
}

// Option configures a Source.
type Option func(*Source)

// WithMatcher sets the make/model/year reader. Without one, those stay null.
func WithMatcher(m Matcher) Option { return func(s *Source) { s.match = m } }

// WithAdvertiserType sets who is searched when the query's SellerType is
// empty: AdvertiserPrivate (the default), AdvertiserDealer, or "" for both.
func WithAdvertiserType(t string) Option { return func(s *Source) { s.advertiserType = t } }

// WithMaxDistance sets the search radius in km (default 200).
func WithMaxDistance(km int) Option { return func(s *Source) { s.maxDistance = km } }

// WithPlace sets the search centre (default Montreal).
func WithPlace(p Place) Option { return func(s *Source) { s.place = p } }

// WithKnown turns a walk into a new-listings run: it stops after
// StopAfterKnownPages consecutive pages where known reports every id seen.
// Such a run has not seen the whole scope and must never drive removals.
func WithKnown(known func(id string) bool) Option { return func(s *Source) { s.known = known } }

// New returns a LesPAC source that fetches through f. Throttling is the
// fetcher's job (wrap it in a fetch.Throttle).
func New(f fetch.Fetcher, opts ...Option) *Source {
	s := &Source{fetcher: f, advertiserType: AdvertiserPrivate, maxDistance: DefaultMaxDistance, place: Montreal}
	for _, o := range opts {
		o(s)
	}
	return s
}

var _ source.Source = (*Source)(nil)

// Name implements source.Source.
func (s *Source) Name() string { return "lespac" }

// GeoSlug is the geo part of this source's crawl identity ("lespac-200km"),
// so removal detection only reaches listings this radius covered.
func (s *Source) GeoSlug() string { return fmt.Sprintf("lespac-%dkm", s.maxDistance) }

func (s *Source) urlOptions(q listing.Query, page int) URLOptions {
	adv := s.advertiserType
	switch q.SellerType {
	case "P":
		adv = AdvertiserPrivate
	case "D":
		adv = AdvertiserDealer
	}
	place := s.place
	return URLOptions{
		AdvertiserType: adv, MaxDistance: s.maxDistance, Page: page,
		YearMin: q.YearFrom, YearMax: q.YearTo, Place: &place,
	}
}

// Walk is a whole LesPAC walk, with what source.Result has no room for.
type Walk struct {
	URL               string
	Total, TotalPages *float64
	PagesWalked       int
	Listings          []Listing
	SkippedParts      int
	// StoppedEarly: a WithKnown run ended on quiet pages.
	StoppedEarly bool
	// NewIDs counts unseen ids on a WithKnown run (nil otherwise).
	NewIDs *int
	// Shortfall is how many of Total were neither collected nor skipped.
	Shortfall int
}

// novelty is makeNoveltyTracker: counts consecutive pages with nothing new.
type novelty struct {
	known      func(string) bool
	quietPages int
	newIDs     int
}

func (n *novelty) page(ls []Listing) bool {
	fresh := 0
	for _, l := range ls {
		if !n.known(l.Key()) {
			fresh++
		}
	}
	n.newIDs += fresh
	if fresh == 0 {
		n.quietPages++
	} else {
		n.quietPages = 0
	}
	return n.quietPages >= StopAfterKnownPages
}

// WalkAll walks a query page by page (the loop in crawlLespac). It stops on
// an empty page, on the last page the site reports, at q.MaxPages (default
// MaxPages), or — on a WithKnown run — after enough pages with nothing new.
func (s *Source) WalkAll(ctx context.Context, q listing.Query, onPage func(source.PageEvent)) (Walk, error) {
	maxPages := q.MaxPages
	if maxPages <= 0 {
		maxPages = MaxPages
	}
	var tracker *novelty
	if s.known != nil {
		tracker = &novelty{known: s.known}
	}

	var w Walk
	var order []string
	collected := map[string]Listing{}
	of := maxPages
	for page := 1; page <= maxPages; page++ {
		w.PagesWalked = page
		u := BuildURL(s.urlOptions(q, page))
		html, err := s.fetcher.Fetch(ctx, u)
		if err != nil {
			return Walk{}, err
		}
		parsed, err := ParsePage(html, s.match)
		if err != nil {
			return Walk{}, err
		}
		if page == 1 {
			w.URL, w.Total, w.TotalPages = u, parsed.Total, parsed.TotalPages
			if parsed.TotalPages != nil && *parsed.TotalPages < float64(of) {
				of = int(*parsed.TotalPages)
			}
		}

		w.SkippedParts += parsed.Skipped
		for _, l := range parsed.Listings {
			key := l.Key()
			if _, seen := collected[key]; !seen {
				order = append(order, key)
			}
			collected[key] = l
		}
		if onPage != nil {
			onPage(source.PageEvent{Page: page, Of: of, Found: len(parsed.Listings), Total: w.Total})
		}

		if len(parsed.Listings) == 0 && parsed.Skipped == 0 {
			break
		}
		if tracker != nil && tracker.page(parsed.Listings) {
			w.StoppedEarly = true
			break
		}
		if parsed.TotalPages != nil && float64(page) >= *parsed.TotalPages {
			break
		}
	}
	if tracker != nil {
		w.NewIDs = &tracker.newIDs
	}

	w.Listings = make([]Listing, 0, len(order))
	for _, k := range order {
		w.Listings = append(w.Listings, collected[k])
	}
	if w.Total != nil {
		if reached := float64(len(collected) + w.SkippedParts); reached < *w.Total {
			w.Shortfall = int(*w.Total - reached)
		}
	}
	return w, nil
}

// Search implements source.Source. Year bounds go to the site (yearMin and
// yearMax are honoured there); make, model, geo, price and sort are not
// supported and are ignored. Truncated is set when the walk came up short of
// the site's total or stopped early on a WithKnown run: either way coverage
// is not good enough to conclude that an unseen listing is gone.
func (s *Source) Search(ctx context.Context, q listing.Query, onPage func(source.PageEvent)) (source.Result, error) {
	w, err := s.WalkAll(ctx, q, onPage)
	if err != nil {
		return source.Result{}, err
	}
	res := source.Result{
		URL: w.URL, Total: w.Total, Pages: w.TotalPages, PagesWalked: w.PagesWalked,
		Listings:  make([]listing.Listing, len(w.Listings)),
		Shortfall: w.Shortfall,
		Truncated: w.Shortfall > 0 || w.StoppedEarly,
	}
	for i, l := range w.Listings {
		res.Listings[i] = l.Listing
	}
	if w.Total != nil && *w.Total != 0 {
		res.ShortfallRatio = float64(w.Shortfall) / *w.Total
	}
	return res, nil
}
