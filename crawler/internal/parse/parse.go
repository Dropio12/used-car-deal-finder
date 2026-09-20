// Package parse reads autohebdo.net search result pages. Port of src/parse.js.
//
// The site is server-rendered Next.js: the whole listing payload is embedded in
// <script id="__NEXT_DATA__">. No GraphQL, no headless browser, no JS needed.
// This is the code most likely to break when the site ships a redesign; the
// fixture tests exist to catch that.
package parse

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"carbuyer/crawler/internal/describe"
	"carbuyer/crawler/internal/fetch"
	"carbuyer/crawler/internal/jstext"
	"carbuyer/crawler/internal/listing"
)

// MaxStoredImages caps photo URLs kept per listing: enough to judge, not bloat.
const MaxStoredImages = 8

// FullImageSize is the photo size requested instead of the 250px thumbnail.
const FullImageSize = "2048x1536"

var (
	nextData     = regexp.MustCompile(`(?s)<script id="__NEXT_DATA__"[^>]*>(.*?)</script>`)
	autoscoutRe  = regexp.MustCompile(`(?i)/\d+x\d+\.(webp|jpg|png)$`)
	brTag        = regexp.MustCompile(`(?i)<br[` + jstext.SpaceClass + `]*/?>`)
	anyTag       = regexp.MustCompile(`<[^>]+>`)
	hexEntity    = regexp.MustCompile(`(?i)&#x([0-9a-f]+);`)
	decEntity    = regexp.MustCompile(`&#(\d+);`)
	namedEntity  = regexp.MustCompile(`&\w+;`)
	blanks       = regexp.MustCompile(`[ \t]+`)
	manyNewlines = regexp.MustCompile(`\n{3,}`)
)

var entities = map[string]string{"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`, "&nbsp;": " "}

// ParseError is the fetch package's "body we could not read" error, so
// fetch.Answered recognises parse failures.
type ParseError = fetch.ParseError

func parseErr(format string, a ...any) error { return &ParseError{Msg: fmt.Sprintf(format, a...)} }

// FullSizeImage swaps the thumbnail size segment for a large one.
func FullSizeImage(url string) string {
	return autoscoutRe.ReplaceAllString(url, "/"+FullImageSize+".$1")
}

// ExtractNextData pulls the raw Next.js payload out of a search page.
func ExtractNextData(html string) (any, error) {
	if html == "" {
		return nil, parseErr("empty response body")
	}
	m := nextData.FindStringSubmatch(html)
	if m == nil {
		return nil, parseErr("__NEXT_DATA__ script tag not found — the page shape changed, or this is a " +
			"block/captcha page rather than a search result page")
	}
	var data any
	if err := json.Unmarshal([]byte(m[1]), &data); err != nil {
		return nil, parseErr("__NEXT_DATA__ was not valid JSON: %v", err)
	}
	return data, nil
}

// ParseNumber turns "38 495 km" into 38495 and "- (Année)" into nil.
func ParseNumber(v any) *float64 {
	switch x := v.(type) {
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return nil
		}
		return &x
	case string:
		var digits strings.Builder
		for i := 0; i < len(x); i++ {
			if x[i] >= '0' && x[i] <= '9' {
				digits.WriteByte(x[i])
			}
		}
		if digits.Len() == 0 {
			return nil
		}
		n, err := strconv.ParseFloat(digits.String(), 64)
		if err != nil || math.IsInf(n, 0) {
			return nil
		}
		return &n
	}
	return nil
}

func codePoint(n uint64, ok bool, original string) string {
	if !ok || n > utf8.MaxRune {
		// JS String.fromCodePoint would throw here; keep the text instead.
		return original
	}
	return string(rune(n))
}

// StripHTML turns a dealer's HTML description fragment into plain text.
func StripHTML(v any) *string {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	s = brTag.ReplaceAllString(s, "\n")
	s = anyTag.ReplaceAllString(s, "")
	s = hexEntity.ReplaceAllStringFunc(s, func(m string) string {
		n, err := strconv.ParseUint(hexEntity.FindStringSubmatch(m)[1], 16, 64)
		return codePoint(n, err == nil, m)
	})
	s = decEntity.ReplaceAllStringFunc(s, func(m string) string {
		n, err := strconv.ParseUint(decEntity.FindStringSubmatch(m)[1], 10, 64)
		return codePoint(n, err == nil, m)
	})
	s = namedEntity.ReplaceAllStringFunc(s, func(m string) string {
		if r, ok := entities[m]; ok {
			return r
		}
		return m
	})
	s = blanks.ReplaceAllString(s, " ")
	s = manyNewlines.ReplaceAllString(s, "\n\n")
	s = jstext.Trim(s)
	return &s
}

func obj(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func str(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

func num(v any) *float64 {
	if f, ok := v.(float64); ok {
		return &f
	}
	return nil
}

// firstNum is JS `a ?? b` for two optional numbers.
func firstNum(a, b *float64) *float64 {
	if a != nil {
		return a
	}
	return b
}

// NormalizeListing flattens one raw listing into the shared schema.
func NormalizeListing(raw map[string]any) listing.Listing {
	vehicle := obj(raw["vehicle"])
	price := obj(raw["price"])
	location := obj(raw["location"])
	seller := obj(raw["seller"])
	tracking := obj(raw["tracking"])
	description := StripHTML(raw["description"])
	damage := describe.Read(description)

	referenceID := str(raw["crossReferenceId"])
	if raw["crossReferenceId"] == nil {
		referenceID = str(obj(raw["identifier"])["crossReferenceId"])
	}
	trimText := str(vehicle["modelVersionInput"])
	if vehicle["modelVersionInput"] == nil {
		trimText = str(vehicle["variant"])
	}
	priceValue := num(price["priceRaw"])
	if price["priceRaw"] == nil {
		priceValue = ParseNumber(price["priceFormatted"])
	}

	images, _ := raw["images"].([]any)
	urls := []string{}
	for i, img := range images {
		if i >= MaxStoredImages {
			break
		}
		if s, ok := img.(string); ok {
			urls = append(urls, FullSizeImage(s))
		}
	}

	return listing.Listing{
		ID:                   str(raw["id"]),
		ReferenceID:          referenceID,
		URL:                  str(raw["url"]),
		Source:               "autohebdo",
		Price:                priceValue,
		SuggestedRetailPrice: ParseNumber(price["suggestedRetailPrice"]),
		IsConditionalPrice:   price["isConditionalPrice"] == true,
		Year:                 num(vehicle["modelYear"]),
		Make:                 str(vehicle["make"]),
		Model:                str(vehicle["modelGroup"]),
		ModelDetail:          str(vehicle["model"]),
		TrimText:             trimText,
		// tracking.mileage is a clean integer string; the display field needs parsing.
		Km:           firstNum(ParseNumber(tracking["mileage"]), ParseNumber(vehicle["mileageInKm"])),
		Transmission: str(vehicle["transmission"]),
		Fuel:         str(vehicle["fuel"]),
		EngineCcm:    ParseNumber(vehicle["engineDisplacementInCCM"]),
		// The site's own flag is never set in practice; the seller's words are the signal.
		IsDamaged:     vehicle["isCurrentlyDamaged"] == true || damage.IsDamaged,
		IsParts:       damage.IsParts,
		Condition:     str(vehicle["offerType"]),
		SellerType:    str(seller["type"]),
		SellerID:      str(seller["id"]),
		SellerName:    str(seller["companyName"]),
		City:          str(location["city"]),
		PostalCode:    str(location["zip"]),
		Province:      str(location["provinceCode"]),
		Description:   description,
		ImageCount:    len(images),
		ImageURLs:     urls,
		ResultType:    str(raw["searchResultType"]),
		ResultSection: str(raw["searchResultSection"]),
	}
}

// IsOrganic is true when a listing actually matched the search. "Deliverable"
// listings are injected by the site, ignore the geo filter, and must never
// reach the comps: a Québec baseline built from Ontario cars looks fine and is wrong.
func IsOrganic(l listing.Listing) bool {
	return l.ResultType != nil && *l.ResultType == "Organic"
}

// CompsRequired are the fields the price model cannot work without.
var CompsRequired = []string{"price", "year", "km", "make", "model"}

// CompsQuality reports whether a listing can be a comparable, and what is missing.
func CompsQuality(l listing.Listing) (bool, []string) {
	missing := []string{}
	present := map[string]bool{
		"price": l.Price != nil, "year": l.Year != nil, "km": l.Km != nil, "make": l.Make != nil, "model": l.Model != nil,
	}
	for _, f := range CompsRequired {
		if !present[f] {
			missing = append(missing, f)
		}
	}
	return len(missing) == 0, missing
}

// Page is one parsed search result page.
type Page struct {
	Total *float64 `json:"total"`
	Pages *float64 `json:"pages"`
	// Listings are query-matching (organic) results only. This is what you want.
	Listings []listing.Listing `json:"listings"`
	// AllListings includes injected out-of-region "Deliverable" cars.
	AllListings   []listing.Listing `json:"allListings"`
	InjectedCount int               `json:"injectedCount"`
	Query         map[string]any    `json:"query"`
}

// SearchPage parses a search results page.
func SearchPage(html string) (Page, error) {
	data, err := ExtractNextData(html)
	if err != nil {
		return Page{}, err
	}
	props, ok := obj(data)["props"].(map[string]any)
	var pageProps map[string]any
	if ok {
		pageProps, _ = props["pageProps"].(map[string]any)
	}
	if pageProps == nil {
		return Page{}, parseErr("__NEXT_DATA__ has no props.pageProps — page shape changed")
	}
	rawListings, ok := pageProps["listings"].([]any)
	if !ok {
		return Page{}, parseErr("props.pageProps.listings is missing or not an array — page shape changed")
	}

	all := make([]listing.Listing, 0, len(rawListings))
	organic := []listing.Listing{}
	for _, raw := range rawListings {
		l := NormalizeListing(obj(raw))
		all = append(all, l)
		if IsOrganic(l) {
			organic = append(organic, l)
		}
	}
	query, _ := pageProps["pageQuery"].(map[string]any)
	if query == nil {
		query = map[string]any{}
	}
	return Page{
		Total:         num(pageProps["numberOfResults"]),
		Pages:         num(pageProps["numberOfPages"]),
		Listings:      organic,
		AllListings:   all,
		InjectedCount: len(all) - len(organic),
		Query:         query,
	}, nil
}
