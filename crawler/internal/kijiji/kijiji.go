// Package kijiji is the Kijiji Autos implementation of source.Source.
// Port of src/kijiji.js plus the page walk of crawlKijiji() in src/crawl.js.
//
// Same technique as AutoHebdo (server-rendered Next.js, plain HTTP, no login,
// no browser) but a different payload shape: Kijiji normalizes its data into an
// Apollo cache keyed `AutosListing:<id>` rather than a plain array.
//
// This is where the private sellers are, and Kijiji supplies fields AutoHebdo
// never had (structured trim, VIN, CARFAX link, listing date, price-drop flag,
// coordinates, a deal rating). listing.Listing has no slot for those, so they
// are returned alongside it as Extra.
package kijiji

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"carbuyer/crawler/internal/describe"
	"carbuyer/crawler/internal/fetch"
	"carbuyer/crawler/internal/jstext"
	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/parse"
	"carbuyer/crawler/internal/source"
)

// Origin is the site root every search URL hangs off.
const Origin = "https://www.kijiji.ca"

const (
	// PageSize is listings per page when the payload does not say.
	PageSize = 40
	// MaxPages is high enough to walk any single region whole. The old default
	// of 100 silently stopped Ontario at 4 000 of 10 191 and reported success.
	MaxPages = 400
	// StopAfterKnownPages is how many consecutive all-seen pages end a
	// new-listings run.
	StopAfterKnownPages = 3
	// ImageRule asks for a 960px photo instead of the 200px search thumbnail:
	// rust at a wheel arch is invisible at 200px, and 960 is ~240 KB vs ~630 KB
	// at 1600px.
	ImageRule = "kijijica-960-jpg"
)

// Geo is a Kijiji location token. The trailing id is the location; c174 is cars.
type Geo struct{ Path, Code string }

// Geos are the known locations, keyed as in the JS KIJIJI_GEO.
var Geos = map[string]Geo{
	"montreal":   {"ville-de-montreal", "c174l1700281"},
	"quebecCity": {"ville-de-quebec", "c174l1700124"},
	"quebec":     {"quebec", "c174l9001"},
	// Ontario is safe to store only because comps are keyed by province.
	"ontario": {"ontario", "c174l9004"},
	// Spans the provincial line: Gatineau listings come back as QC, which is
	// fine because comps key on each listing's own province.
	"ottawa":         {"ottawa", "c174l1700185"},
	"ottawaGatineau": {"ottawa-gatineau-area", "c174l1700184"},
}

// geoAliases lets listing.Query.Geo carry either a Kijiji key or the region /
// AutoHebdo token the rest of the crawler already uses. "" means Québec, the
// default region (src/region.js kijijiGeo).
var geoAliases = map[string]string{
	"": "quebec", "qc": "quebec", "reg_qc": "quebec",
	"on": "ontario", "reg_on": "ontario",
	"cit_montreal": "montreal", "cit_quebec": "quebecCity",
}

// ResolveGeo maps a query geo onto a Kijiji location key and token.
func ResolveGeo(name string) (string, Geo, error) {
	key := strings.TrimSpace(name)
	if alias, ok := geoAliases[strings.ToLower(key)]; ok {
		key = alias
	}
	if g, ok := Geos[key]; ok {
		return key, g, nil
	}
	for k, g := range Geos {
		if strings.EqualFold(k, key) {
			return k, g, nil
		}
	}
	return "", Geo{}, fmt.Errorf("kijiji: unknown geo %q", name)
}

// sellerParam maps listing.Query.SellerType onto `for-sale-by`.
func sellerParam(sellerType string) (string, error) {
	switch sellerType {
	case "":
		return "", nil
	case "P", "ownr":
		return "ownr", nil
	case "D", "delr":
		return "delr", nil
	}
	return "", fmt.Errorf("kijiji: unknown seller type %q", sellerType)
}

// BuildURL is buildKijijiUrl. sellerType is "ownr" (private), "delr" or "";
// page is 1-based and only appears in the path past page 1.
//
// `for-sale-by` genuinely filters (Montréal 4 574 -> 1 138). `vehicle-type=used`
// does NOT: it is accepted and ignored, so used/new is filtered locally.
func BuildURL(geo Geo, sellerType string, page int) string {
	segments := []string{"b-cars-trucks", geo.Path}
	if page > 1 {
		segments = append(segments, fmt.Sprintf("page-%d", page))
	}
	segments = append(segments, geo.Code)
	u := Origin + "/" + strings.Join(segments, "/")
	if sellerType != "" {
		u += "?" + url.Values{"for-sale-by": {sellerType}}.Encode()
	}
	return u
}

// UpgradeImageRule swaps the thumbnail `?rule=` for ImageRule. Same path, any size.
func UpgradeImageRule(u string) string {
	if u == "" {
		return u
	}
	base, _, _ := strings.Cut(u, "?")
	return base + "?rule=" + ImageRule
}

// --- field helpers -----------------------------------------------------------

func obj(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// attr is the first canonical value of a named attribute, or nil.
func attr(raw map[string]any, name string) any {
	all, _ := obj(raw["attributes"])["all"].([]any)
	for _, a := range all {
		m := obj(a)
		if m["canonicalName"] == name {
			vals, _ := m["canonicalValues"].([]any)
			if len(vals) == 0 {
				return nil
			}
			return vals[0]
		}
	}
	return nil
}

// jsString is String(v) for the JSON scalars Kijiji emits.
func jsString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case float64:
		return jstext.Number(x), true
	case bool:
		return strconv.FormatBool(x), true
	}
	return "", false
}

// strOf keeps a string or number value, as JS `?? null` would carry either.
func strOf(v any) *string {
	if s, ok := jsString(v); ok {
		return &s
	}
	return nil
}

// strOnly is v when it is a string, else nil.
func strOnly(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

func floatOf(v any) *float64 {
	if f, ok := v.(float64); ok {
		return &f
	}
	return nil
}

var leadingInt = regexp.MustCompile(`^-?\d+`)

// num is JS parseInt over the digits and minus signs of String(v):
// "60 300 km" -> 60300, "-" -> nil.
func num(v any) *float64 {
	s, ok := jsString(v)
	if v == nil || !ok {
		return nil
	}
	var kept strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || r == '-' {
			kept.WriteRune(r)
		}
	}
	m := leadingInt.FindString(kept.String())
	if m == "" || m == "-" {
		return nil
	}
	n, err := strconv.ParseFloat(m, 64)
	if err != nil || math.IsInf(n, 0) {
		return nil
	}
	return &n
}

// jsRound is Math.round.
func jsRound(x float64) float64 { return math.Floor(x + 0.5) }

// cents turns a price in cents into dollars. 3664322 is $36 643.22; reading
// it as dollars would put every Kijiji car a hundredfold above the market.
func cents(v any) *float64 {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return nil
		}
		f = n
	default:
		return nil
	}
	r := jsRound(f / 100)
	return &r
}

// --- make, model, trim -------------------------------------------------------

// placeholder is Kijiji's "other" taxonomy: Othrmake/Othrmdl name no vehicle
// and must never become a comps bucket.
var placeholder = map[string]bool{"othrmake": true, "othrmdl": true, "other": true, "othr": true}

// makeAliases reconcile Kijiji's make vocabulary with AutoHebdo's. This is the
// cross-source join key: acronyms title-casing ruins, names Kijiji collapses,
// and one outright misspelling ("Volkwagen").
var makeAliases = map[string]string{
	"volkwagen": "Volkswagen", "vw": "Volkswagen",
	"bmw": "BMW", "gmc": "GMC", "ram": "RAM", "mini": "MINI",
	"mercedes": "Mercedes-Benz", "mercedes-amg": "Mercedes-Benz", "mercedesbenz": "Mercedes-Benz",
	"landrover": "Land Rover", "alpharomeo": "Alfa Romeo", "alfaromeo": "Alfa Romeo",
	"chevy": "Chevrolet",
}

var (
	spacesOrUnderscores = regexp.MustCompile(`[` + jstext.SpaceClass + `_]+`)
	// JS /[a-zà-ÿ0-9]+/gi, spelled out: Go's (?i) would also fold in ſ and K.
	titleWord = regexp.MustCompile(`[a-zA-Z0-9\x{C0}-\x{D6}\x{D8}-\x{FF}\x{178}]+`)
)

// CanonicalMake maps a Kijiji make onto the shared vocabulary, or nil.
func CanonicalMake(value string) *string {
	if value == "" {
		return nil
	}
	lower := strings.ToLower(value)
	key := spacesOrUnderscores.ReplaceAllString(lower, "")
	if placeholder[key] {
		return nil
	}
	if a, ok := makeAliases[key]; ok {
		return &a
	}
	if a, ok := makeAliases[lower]; ok {
		return &a
	}
	return TitleCase(value)
}

// jsUpperFirst is word[0].toUpperCase(): JS upper-cases ß to "SS".
func jsUpperFirst(r rune) string {
	if r == 'ß' {
		return "SS"
	}
	return strings.ToUpper(string(r))
}

// TitleCase is "chevrolet" -> "Chevrolet", "mercedes-benz" -> "Mercedes-Benz",
// "Model_3" -> "Model 3" (Kijiji uses underscores for spaces in model slugs).
func TitleCase(value string) *string {
	if value == "" || placeholder[strings.ToLower(value)] {
		return nil
	}
	s := strings.ReplaceAll(value, "_", " ")
	s = titleWord.ReplaceAllStringFunc(s, func(w string) string {
		runes := []rune(w)
		return jsUpperFirst(runes[0]) + strings.ToLower(string(runes[1:]))
	})
	return &s
}

func titleCaseAny(v any) *string {
	if s := strOnly(v); s != nil {
		return TitleCase(*s)
	}
	return nil
}

var jsWhitespace = regexp.MustCompile(`[` + jstext.SpaceClass + `]+`)

// TrimFromTitle recovers a trim from the ad title when `cartrim` is empty (it
// is filled on only ~40% of private listings): "2019 Audi A3 Komfort" minus
// year, make and model leaves "Komfort". nil tokens are skipped.
func TrimFromTitle(title *string, year *float64, mk, model *string) *string {
	if title == nil {
		return nil
	}
	rest := *title
	var tokens []string
	if year != nil {
		tokens = append(tokens, jstext.Number(*year))
	}
	for _, t := range []*string{mk, model} {
		if t != nil {
			tokens = append(tokens, *t)
		}
	}
	for _, token := range tokens {
		escaped := spacesOrUnderscores.ReplaceAllString(regexp.QuoteMeta(token), `[`+jstext.SpaceClass+`_]+`)
		re, err := regexp.Compile(`(?i)\b` + escaped + `\b`)
		if err != nil {
			continue
		}
		if loc := re.FindStringIndex(rest); loc != nil {
			rest = rest[:loc[0]] + " " + rest[loc[1]:]
		}
	}
	cleaned := jstext.Trim(jsWhitespace.ReplaceAllString(rest, " "))
	if cleaned == "" {
		return nil
	}
	return &cleaned
}

// --- address -----------------------------------------------------------------

// ci spells an ASCII word case-insensitively without (?i), which in Go would
// also match the Kelvin sign and long s.
func ci(word string) string {
	var b strings.Builder
	for _, r := range word {
		b.WriteString("[" + strings.ToUpper(string(r)) + strings.ToLower(string(r)) + "]")
	}
	return b.String()
}

var provinceCodes = []string{"QC", "ON", "NB", "NS", "PE", "NL", "MB", "SK", "AB", "BC", "YT", "NT", "NU"}

func provinceAlternation() string {
	parts := make([]string, len(provinceCodes))
	for i, p := range provinceCodes {
		parts[i] = ci(p)
	}
	return "(?:" + strings.Join(parts, "|") + ")"
}

const letter = `[A-Za-z]`

var (
	fullPostal   = regexp.MustCompile(`\b` + letter + `\d` + letter + `[` + jstext.SpaceClass + `]?\d` + letter + `\d\b`)
	fsa          = regexp.MustCompile(`\b` + letter + `\d` + letter + `\b`)
	fsaFollowing = regexp.MustCompile(`^[` + jstext.SpaceClass + `]?\d` + letter + `\d`)
	provinceWord = regexp.MustCompile(`\b` + provinceAlternation() + `\b`)
	provinceOnly = regexp.MustCompile(`^` + provinceAlternation() + `$`)
)

// postalPrefixProvince: the first letter of a postal code names the province.
// About a sixth of listings state no province, and the price model refuses to
// price a car whose region is unknown.
var postalPrefixProvince = map[byte]string{
	'A': "NL", 'B': "NS", 'C': "PE", 'E': "NB",
	'G': "QC", 'H': "QC", 'J': "QC",
	'K': "ON", 'L': "ON", 'M': "ON", 'N': "ON", 'P': "ON",
	'R': "MB", 'S': "SK", 'T': "AB", 'V': "BC",
	'X': "NT", 'Y': "YT",
}

// ProvinceFromPostalCode maps a postal code (or just its FSA) to a province, or nil.
func ProvinceFromPostalCode(postalCode string) *string {
	if postalCode == "" {
		return nil
	}
	c := postalCode[0]
	if c >= 'a' && c <= 'z' {
		c -= 'a' - 'A'
	}
	if p, ok := postalPrefixProvince[c]; ok {
		return &p
	}
	return nil
}

// Address is a parsed Kijiji location.
type Address struct {
	City, Province, PostalCode *string
}

// fsaMatch is /\b[A-Z]\d[A-Z]\b(?!\s?\d[A-Z]\d)/i: an FSA not followed by the
// rest of a postal code. Go has no lookahead, so the tail is checked by hand.
func fsaMatch(s string) string {
	for _, m := range fsa.FindAllStringIndex(s, -1) {
		if !fsaFollowing.MatchString(s[m[1]:]) {
			return s[m[0]:m[1]]
		}
	}
	return ""
}

func replaceFirst(re *regexp.Regexp, s string) string {
	if loc := re.FindStringIndex(s); loc != nil {
		return s[:loc[0]] + s[loc[1]:]
	}
	return s
}

// ParseAddress splits a freehand seller address into city, province and postal
// code. The city is the LAST comma part that is neither a province nor a postal
// chunk: "QC H2G" is not a city, and "Rue X, Montréal, H1X 1A1" is Montréal.
func ParseAddress(address *string) Address {
	if address == nil || jstext.Trim(*address) == "" {
		return Address{}
	}
	a := *address
	postal := fullPostal.FindString(a)
	if postal == "" {
		postal = fsaMatch(a)
	}
	province := provinceWord.FindString(a)

	var city *string
	for _, part := range strings.Split(a, ",") {
		part = jstext.Trim(part)
		if part == "" {
			continue
		}
		rest := jstext.Trim(replaceFirst(fsa, replaceFirst(fullPostal, part)))
		if rest == "" || provinceOnly.MatchString(rest) {
			continue
		}
		p := part
		city = &p
	}

	out := Address{City: city}
	if postal != "" {
		pc := jsWhitespace.ReplaceAllString(strings.ToUpper(postal), "")
		out.PostalCode = &pc
	}
	// A stated province wins; otherwise derive it rather than leave the car unpriceable.
	if province != "" {
		p := strings.ToUpper(province)
		out.Province = &p
	} else if out.PostalCode != nil {
		out.Province = ProvinceFromPostalCode(*out.PostalCode)
	}
	return out
}

// --- normalize ---------------------------------------------------------------

// transmission: Kijiji ships it as a numeric code.
var transmission = map[string]string{"1": "Manuelle", "2": "Automatique"}

// fuel maps Kijiji fuel slugs; "other" deliberately maps to nothing.
var fuel = map[string]*string{
	"gasoline": listing.Str("Essence"), "diesel": listing.Str("Diesel"), "electric": listing.Str("Électrique"),
	"hybrid": listing.Str("Hybride"), "plug-in hybrid": listing.Str("Hybride rechargeable"), "other": nil,
}

// Extra holds the fields Kijiji publishes that listing.Listing has no slot for.
type Extra struct {
	Title        *string  `json:"title"`
	ListedAt     *string  `json:"listedAt"`
	PriceRating  *string  `json:"priceRating"`
	VIN          *string  `json:"vin"`
	CarfaxURL    *string  `json:"carfaxUrl"`
	Latitude     *float64 `json:"latitude"`
	Longitude    *float64 `json:"longitude"`
	HadPriceDrop bool     `json:"hadPriceDrop"`
}

func firstStr(vs ...*string) *string {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

// Normalize maps one raw `AutosListing` into the shared schema (normalizeKijiji).
func Normalize(raw map[string]any) (listing.Listing, Extra) {
	loc := obj(raw["location"])
	addr := ParseAddress(strOnly(loc["address"]))
	price := obj(raw["price"])
	year := num(attr(raw, "caryear"))
	var mk *string
	if s := strOnly(attr(raw, "carmake")); s != nil {
		mk = CanonicalMake(*s)
	}
	model := titleCaseAny(attr(raw, "carmodel"))
	description := parse.StripHTML(raw["description"])
	// Kijiji publishes no damage flag; what the seller wrote is the only signal.
	damage := describe.Read(description)
	title := strOnly(raw["title"])

	id, _ := jsString(raw["id"])
	if raw["id"] == nil {
		id = "undefined" // `kijiji:${undefined}`, as the JS writes it
	}
	refID, _ := jsString(raw["id"])

	var fuelOut *string
	if f, ok := attr(raw, "carfueltype").(string); ok {
		if mapped, known := fuel[f]; known && mapped != nil {
			fuelOut = mapped
		} else {
			fuelOut = TitleCase(f)
		}
	}
	var trans *string
	if code, ok := jsString(attr(raw, "cartransmission")); ok {
		if t, known := transmission[code]; known {
			trans = &t
		}
	}
	condition := "U"
	if attr(raw, "vehicletype") == "new" {
		condition = "N"
	}
	seller := "Dealer"
	if attr(raw, "forsaleby") == "ownr" {
		seller = "PrivateSeller"
	}
	resultType := "Promoted"
	if raw["adSource"] == "ORGANIC" {
		// ORGANIC matched the search; TOP_AD / PROV_TOP_AD are paid placements.
		resultType = "Organic"
	}

	images := []string{}
	if all, ok := raw["imageUrls"].([]any); ok {
		for i, img := range all {
			if i >= parse.MaxStoredImages {
				break
			}
			if s, ok := img.(string); ok {
				images = append(images, UpgradeImageRule(s))
			}
		}
	}
	imageCount := 0
	if n := floatOf(raw["imageCount"]); n != nil {
		imageCount = int(*n)
	}

	priceType := price["type"]
	l := listing.Listing{
		// Namespaced so a Kijiji id can never collide with an AutoHebdo one.
		ID:                   listing.Str("kijiji:" + id),
		ReferenceID:          &refID,
		URL:                  strOf(raw["url"]),
		Source:               "kijiji",
		Price:                cents(price["amount"]),
		SuggestedRetailPrice: cents(price["msrp"]),
		IsConditionalPrice:   priceType != nil && priceType != "FIXED",
		Year:                 year,
		Make:                 mk,
		Model:                model,
		// The structured trim wins; the ad title is the fallback.
		TrimText:      firstStr(strOf(attr(raw, "cartrim")), TrimFromTitle(title, year, mk, model)),
		Km:            num(attr(raw, "carmileageinkms")),
		Transmission:  trans,
		Fuel:          fuelOut,
		IsDamaged:     damage.IsDamaged,
		IsParts:       damage.IsParts,
		Condition:     &condition,
		SellerType:    &seller,
		SellerID:      strOf(obj(raw["posterInfo"])["posterId"]),
		City:          addr.City,
		Province:      addr.Province,
		PostalCode:    addr.PostalCode,
		Description:   description,
		ImageCount:    imageCount,
		ImageURLs:     images,
		ResultType:    &resultType,
		ResultSection: strOf(raw["adSource"]),
	}
	coords := obj(loc["coordinates"])
	extra := Extra{
		Title:        title,
		ListedAt:     firstStr(strOf(raw["activationDate"]), strOf(raw["sortingDate"])),
		PriceRating:  firstStr(strOf(obj(price["classification"])["rating"]), strOf(attr(raw, "pricerating"))),
		VIN:          strOf(attr(raw, "vin")),
		CarfaxURL:    strOf(attr(raw, "carprooflink")),
		Latitude:     floatOf(coords["latitude"]),
		Longitude:    floatOf(coords["longitude"]),
		HadPriceDrop: obj(raw["flags"])["priceDrop"] == true,
	}
	return l, extra
}

// IsUsed is true for anything not explicitly new.
func IsUsed(l listing.Listing) bool { return l.Condition != nil && *l.Condition == "U" }

// --- page --------------------------------------------------------------------

var nextData = regexp.MustCompile(`(?s)<script id="__NEXT_DATA__"[^>]*>(.*?)</script>`)

func parseErr(format string, a ...any) error {
	return &parse.ParseError{Msg: fmt.Sprintf(format, a...)}
}

// orderedObject decodes a JSON object keeping key order, which Go maps lose
// and JS Object.keys() keeps. Duplicate keys keep their first position and
// last value, as JSON.parse does. ok is false when raw is not an object.
func orderedObject(raw json.RawMessage) (keys []string, vals map[string]json.RawMessage, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, nil, false
	}
	vals = map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, false
		}
		key, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, false
		}
		if _, seen := vals[key]; !seen {
			keys = append(keys, key)
		}
		vals[key] = v
	}
	return keys, vals, true
}

func field(raw json.RawMessage, key string) json.RawMessage {
	_, vals, ok := orderedObject(raw)
	if !ok {
		return nil
	}
	return vals[key]
}

// Page is one parsed search result page (parseKijijiPage).
type Page struct {
	Total    *float64 `json:"total"`
	PageSize float64  `json:"pageSize"`
	Offset   float64  `json:"offset"`
	// Listings are organic results only. This is what you want.
	Listings []listing.Listing `json:"listings"`
	// AllListings includes paid placements.
	AllListings   []listing.Listing `json:"allListings"`
	PromotedCount int               `json:"promotedCount"`
	// Extras holds the Kijiji-only fields, keyed by listing id.
	Extras map[string]Extra `json:"-"`
}

func truthy(raw json.RawMessage) bool {
	switch t := strings.TrimSpace(string(raw)); t {
	case "", "null", "false", "0", `""`:
		return false
	}
	return true
}

// ParsePage reads listings out of the Apollo cache embedded in a results page.
func ParsePage(html string) (Page, error) {
	if html == "" {
		return Page{}, parseErr("empty response body")
	}
	m := nextData.FindStringSubmatch(html)
	if m == nil {
		return Page{}, parseErr("Kijiji __NEXT_DATA__ not found — the page shape changed, or this is a block page")
	}
	body := json.RawMessage(m[1])
	if !json.Valid(body) {
		var v any
		err := json.Unmarshal(body, &v)
		return Page{}, parseErr("Kijiji __NEXT_DATA__ was not valid JSON: %v", err)
	}
	apollo := field(field(field(body, "props"), "pageProps"), "__APOLLO_STATE__")
	if !truthy(apollo) {
		return Page{}, parseErr("Kijiji payload has no __APOLLO_STATE__ — page shape changed")
	}
	keys, vals, _ := orderedObject(apollo)

	out := Page{PageSize: PageSize, Listings: []listing.Listing{}, AllListings: []listing.Listing{}, Extras: map[string]Extra{}}
	for _, k := range keys {
		if !strings.HasPrefix(k, "AutosListing:") {
			continue
		}
		var raw any
		_ = json.Unmarshal(vals[k], &raw)
		l, extra := Normalize(obj(raw))
		out.AllListings = append(out.AllListings, l)
		out.Extras[l.Key()] = extra
		if *l.ResultType == "Organic" {
			out.Listings = append(out.Listings, l)
		}
	}
	out.PromotedCount = len(out.AllListings) - len(out.Listings)

	rootKeys, rootVals, _ := orderedObject(vals["ROOT_QUERY"])
	for _, k := range rootKeys {
		if !strings.HasPrefix(k, "searchResultsPageByUrl") {
			continue
		}
		var search any
		_ = json.Unmarshal(rootVals[k], &search)
		pagination := obj(obj(search)["pagination"])
		out.Total = floatOf(pagination["totalCount"])
		if n := floatOf(pagination["limit"]); n != nil {
			out.PageSize = *n
		}
		if n := floatOf(pagination["offset"]); n != nil {
			out.Offset = *n
		}
		break
	}
	return out, nil
}

// --- novelty -----------------------------------------------------------------

// NoveltyTracker ends a "find what's new" walk after StopAfter consecutive
// pages with nothing unseen (makeNoveltyTracker). It does not rely on date
// order, which promoted placements break; it degrades to a full crawl rather
// than to a wrong answer.
type NoveltyTracker struct {
	Known      func(id string) bool
	StopAfter  int
	NewIDs     int
	QuietPages int
}

// Page records one page and reports whether the walk should stop.
func (n *NoveltyTracker) Page(ls []listing.Listing) bool {
	fresh := 0
	for _, l := range ls {
		if !n.Known(l.Key()) {
			fresh++
		}
	}
	n.NewIDs += fresh
	if fresh == 0 {
		n.QuietPages++
	} else {
		n.QuietPages = 0
	}
	return n.QuietPages >= n.StopAfter
}

// --- source ------------------------------------------------------------------

// Source searches kijiji.ca.
type Source struct {
	fetcher   fetch.Fetcher
	usedOnly  bool
	known     func(id string) bool
	stopAfter int
}

// Option configures a Source.
type Option func(*Source)

// WithUsedOnly keeps (true, the default) or drops the local new-car filter.
func WithUsedOnly(used bool) Option { return func(s *Source) { s.usedOnly = used } }

// WithKnown switches to a new-listings run ("mode: new"): the walk stops after
// StopAfterKnownPages consecutive pages whose ids all satisfy known. known
// should cover every id ever stored for "kijiji", not just this scope.
func WithKnown(known func(id string) bool) Option { return func(s *Source) { s.known = known } }

// WithStopAfter overrides StopAfterKnownPages for a new-listings run.
func WithStopAfter(pages int) Option { return func(s *Source) { s.stopAfter = pages } }

// New returns a Kijiji source that fetches through f.
func New(f fetch.Fetcher, opts ...Option) *Source {
	s := &Source{fetcher: f, usedOnly: true, stopAfter: StopAfterKnownPages}
	for _, o := range opts {
		o(s)
	}
	return s
}

var _ source.Source = (*Source)(nil)

// Name implements source.Source.
func (s *Source) Name() string { return "kijiji" }

// Walk is a Search result plus what only a Kijiji walk knows.
type Walk struct {
	source.Result
	// GeoKey is the Kijiji location key walked ("quebec", "ontario", ...).
	GeoKey string
	// SkippedNew counts new cars dropped by the used-only filter.
	SkippedNew int
	// StoppedEarly: a new-listings run ended on novelty, so it saw only part
	// of the scope and must never drive removal detection.
	StoppedEarly bool
	// NewIDs is set on new-listings runs only.
	NewIDs *int
	// Extras holds the Kijiji-only fields of every returned listing, by id.
	Extras map[string]Extra
}

// yearOK applies the query's year bounds, which Kijiji's URL cannot express.
func yearOK(l listing.Listing, q listing.Query) bool {
	if q.YearFrom == nil && q.YearTo == nil {
		return true
	}
	if l.Year == nil {
		return false
	}
	return (q.YearFrom == nil || *l.Year >= *q.YearFrom) && (q.YearTo == nil || *l.Year <= *q.YearTo)
}

// Search implements source.Source.
func (s *Source) Search(ctx context.Context, q listing.Query, onPage func(source.PageEvent)) (source.Result, error) {
	w, err := s.Walk(ctx, q, onPage)
	return w.Result, err
}

// Walk walks a Kijiji region page by page (crawlKijiji without the database).
// No sharding (a region is small enough to walk whole) and no query-resolution
// guard (the geo is a fixed path token the site cannot silently ignore).
//
// Kijiji cannot filter by make, model or price, so a query naming one is
// refused rather than answered with every car in the region.
func (s *Source) Walk(ctx context.Context, q listing.Query, onPage func(source.PageEvent)) (Walk, error) {
	if q.Make != "" || q.Model != "" || q.PriceFrom != nil || q.PriceTo != nil {
		return Walk{}, fmt.Errorf("kijiji: cannot filter by make, model or price; walk a whole region instead")
	}
	geoKey, geo, err := ResolveGeo(q.Geo)
	if err != nil {
		return Walk{}, err
	}
	seller, err := sellerParam(q.SellerType)
	if err != nil {
		return Walk{}, err
	}
	maxPages := q.MaxPages
	if maxPages <= 0 {
		maxPages = MaxPages
	}
	hasLocalFilter := q.YearFrom != nil || q.YearTo != nil
	var novelty *NoveltyTracker
	if s.known != nil {
		novelty = &NoveltyTracker{Known: s.known, StopAfter: s.stopAfter}
	}

	w := Walk{GeoKey: geoKey, Extras: map[string]Extra{}}
	w.LocallyFiltered = hasLocalFilter
	var order []string
	collected := map[string]listing.Listing{}
	yearDropped := 0
	endedEmpty := false

	for page := 1; page <= maxPages; page++ {
		w.PagesWalked = page
		u := BuildURL(geo, seller, page)
		html, err := s.fetcher.Fetch(ctx, u)
		if err != nil {
			return Walk{}, err
		}
		parsed, err := ParsePage(html)
		if err != nil {
			return Walk{}, err
		}
		if page == 1 {
			w.URL, w.Total = u, parsed.Total
			if parsed.Total != nil && parsed.PageSize > 0 {
				pages := math.Ceil(*parsed.Total / parsed.PageSize)
				w.Pages = &pages
			}
		}

		wanted := parsed.Listings
		if s.usedOnly {
			wanted = []listing.Listing{}
			for _, l := range parsed.Listings {
				if IsUsed(l) {
					wanted = append(wanted, l)
				}
			}
		}
		w.SkippedNew += len(parsed.Listings) - len(wanted)
		w.Injected += parsed.PromotedCount
		kept := 0
		for _, l := range wanted {
			if !yearOK(l, q) {
				yearDropped++
				continue
			}
			kept++
			key := l.Key()
			if _, seen := collected[key]; !seen {
				order = append(order, key)
			}
			collected[key] = l
			w.Extras[key] = parsed.Extras[key]
		}
		if onPage != nil {
			of := maxPages
			if w.Pages != nil && *w.Pages < float64(of) {
				of = int(*w.Pages)
			}
			onPage(source.PageEvent{Page: page, Of: of, Found: kept, Total: w.Total})
		}
		if len(parsed.Listings) == 0 {
			endedEmpty = true
			break
		}
		if novelty != nil && novelty.Page(wanted) {
			w.StoppedEarly = true
			break
		}
		if w.Total != nil && parsed.Offset+parsed.PageSize >= *w.Total {
			break
		}
	}
	if novelty != nil {
		n := novelty.NewIDs
		w.NewIDs = &n
	}

	w.Listings = make([]listing.Listing, 0, len(order))
	for _, k := range order {
		w.Listings = append(w.Listings, collected[k])
	}

	// Compare what was collected against what the site claimed rather than
	// trusting the loop's exit: Kijiji stops paginating at ~101 pages by
	// answering an empty page, which no page-limit check ever notices.
	if w.Total != nil {
		total := int(*w.Total)
		if !s.usedOnly && !hasLocalFilter && len(collected) < total {
			w.Shortfall = total - len(collected)
		}
		// `total` counts new and used together, so skipped new cars count as reached.
		if reached := len(collected) + w.SkippedNew + yearDropped; reached < total {
			w.Truncated = true
			w.Shortfall = total - reached
		}
		if total != 0 {
			w.ShortfallRatio = float64(w.Shortfall) / float64(total)
		}
	}
	w.HitSiteCeiling = w.Truncated && endedEmpty
	return w, nil
}
