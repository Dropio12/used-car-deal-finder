// Package searchurl builds autohebdo.net search URLs. Port of src/url.js.
//
// Every rule here was found by probing the live site (see PLAN.md §0 in the
// original project). Two of them are easy to break by accident:
//
//   - Geo is a PATH segment (/reg_qc, /cit_montreal), never a query param.
//   - `zip` is NEVER sent. zip=H2Y1C6 next to /reg_qc silently narrows a
//     province search to Montreal (858 RAV4s become 699). The postal code is
//     only used locally, for distances.
//   - The year filter (fregfrom/fregto) is inert on Canadian listings, so it is
//     never sent either; years are filtered client-side.
package searchurl

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"carbuyer/crawler/internal/jstext"
)

// Origin is the site root.
const Origin = "https://www.autohebdo.net"

// Geo path tokens: reg_* = province, cit_* = city.
const (
	GeoQuebec     = "reg_qc"
	GeoMontreal   = "cit_montreal"
	GeoQuebecCity = "cit_quebec"
)

// HomePostalCode is home base for distance calculations. NOT sent to the site.
const HomePostalCode = "H2Y1C6"

var postalCode = regexp.MustCompile(`^[ABCEGHJ-NPRSTVXY]\d[ABCEGHJ-NPRSTV-Z]\d[ABCEGHJ-NPRSTV-Z]\d$`)
var spaces = regexp.MustCompile(`[` + jstext.SpaceClass + `]+`)

// NormalizePostalCode canonicalizes a Canadian postal code ("h2y 1c6" -> "H2Y1C6").
// A partial or malformed code is accepted by the site but silently searches
// all of Canada, so this fails loudly instead.
func NormalizePostalCode(input string) (string, error) {
	cleaned := strings.ToUpper(spaces.ReplaceAllString(input, ""))
	if !postalCode.MatchString(cleaned) {
		return "", fmt.Errorf("invalid postal code %q: need all 6 characters (e.g. \"H2Y1C6\"). "+
			"A partial or malformed code is accepted by the site but silently searches all of Canada", input)
	}
	return cleaned, nil
}

// Options are the search parameters the site actually honours.
type Options struct {
	Make       string
	Model      string
	Geo        string // path token; "" means province-wide Quebec
	SellerType string // "P" private, "D" dealer
	Page       int    // 1-based; 20 listings per page
	Sort       string // "standard" | "price" | "age"
	Descending *bool
	PriceFrom  *float64
	PriceTo    *float64
	Damaged    string // "exclude" (default) | "include"
	Condition  string // "U" (default) | "N" | "N,U"
}

// ErrModelWithoutMake is returned when a model is given with no make.
var ErrModelWithoutMake = errors.New("`model` requires `make` (the path is /autos/{make}/{model})")

// Build returns the search URL for opts.
func Build(opts Options) (string, error) {
	if opts.Model != "" && opts.Make == "" {
		return "", ErrModelWithoutMake
	}
	geo := opts.Geo
	if geo == "" {
		geo = GeoQuebec
	}
	damaged := opts.Damaged
	if damaged == "" {
		damaged = "exclude"
	}
	condition := opts.Condition
	if condition == "" {
		condition = "U"
	}

	var segments []string
	for _, s := range []string{"autos", opts.Make, opts.Model, geo} {
		if s != "" {
			segments = append(segments, encodeURIComponent(s))
		}
	}

	// Insertion order matters to match the JS output byte for byte, so the
	// query string is built by hand rather than with url.Values (which sorts).
	params := [][2]string{
		{"atype", "C"}, // cars
		{"cy", "CA"},   // Canada
		{"ustate", condition},
		{"damaged_listing", damaged},
	}
	if opts.SellerType != "" {
		params = append(params, [2]string{"custtype", opts.SellerType})
	}
	if opts.Page > 1 {
		params = append(params, [2]string{"page", fmt.Sprint(opts.Page)})
	}
	if opts.Sort != "" {
		params = append(params, [2]string{"sort", opts.Sort})
	}
	if opts.Descending != nil {
		d := "0"
		if *opts.Descending {
			d = "1"
		}
		params = append(params, [2]string{"desc", d})
	}
	if opts.PriceFrom != nil {
		params = append(params, [2]string{"pricefrom", jstext.Number(*opts.PriceFrom)})
	}
	if opts.PriceTo != nil {
		params = append(params, [2]string{"priceto", jstext.Number(*opts.PriceTo)})
	}

	query := make([]string, len(params))
	for i, p := range params {
		query[i] = formEncode(p[0]) + "=" + formEncode(p[1])
	}
	return Origin + "/" + strings.Join(segments, "/") + "?" + strings.Join(query, "&"), nil
}

// encodeURIComponent keeps A-Z a-z 0-9 - _ . ! ~ * ' ( ) and escapes the rest.
func encodeURIComponent(s string) string {
	return escape(s, "-_.!~*'()", false)
}

// formEncode is application/x-www-form-urlencoded, as URLSearchParams writes it.
func formEncode(s string) string {
	return escape(s, "*-._", true)
}

func escape(s, keep string, spaceAsPlus bool) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', strings.IndexByte(keep, c) >= 0:
			b.WriteByte(c)
		case c == ' ' && spaceAsPlus:
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
