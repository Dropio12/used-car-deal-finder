package facebook

// Marketplace through Apify. Port of src/marketplace.js.
//
// The actor (curious_coder/facebook-marketplace) runs on Apify's own
// infrastructure with its own accounts and proxies, which keeps any personal
// account out of it. It is the one source that bills per result, so
// EstimateCost makes the price visible before a run and RunActor refuses to
// start, or aborts, above the ceiling the caller sets.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"carbuyer/crawler/internal/describe"
	"carbuyer/crawler/internal/jstext"
	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/parse"
)

// Actor is the Apify actor id.
const Actor = "curious_coder~facebook-marketplace"

// ApifyBase is the Apify API root.
const ApifyBase = "https://api.apify.com/v2"

// CostPer1000 is the actor's published price. Used only to warn, never to bill.
const CostPer1000 = 1.5

// EstimateCost is the dollar cost of n results, to the cent.
func EstimateCost(n int) float64 {
	return jsRound(float64(n)/1000*CostPer1000*100) / 100
}

// MarketplaceURLOptions is a hand-pinned vehicle search. Nil values are left out.
type MarketplaceURLOptions struct {
	City                      string // default "montreal"
	MinPrice, MaxPrice        *int
	DaysSinceListed, RadiusKm *int // radius default 100
}

// MarketplaceSearchURL builds a /vehicles search URL, newest first. Kept for
// callers that pin an exact search; the actor's own filters are preferred.
func MarketplaceSearchURL(o MarketplaceURLOptions) string {
	city := o.City
	if city == "" {
		city = "montreal"
	}
	radius := 100
	if o.RadiusKm != nil {
		radius = *o.RadiusKm
	}
	var params [][2]string
	if o.MinPrice != nil {
		params = append(params, [2]string{"minPrice", strconv.Itoa(*o.MinPrice)})
	}
	if o.MaxPrice != nil {
		params = append(params, [2]string{"maxPrice", strconv.Itoa(*o.MaxPrice)})
	}
	if o.DaysSinceListed != nil {
		params = append(params, [2]string{"daysSinceListed", strconv.Itoa(*o.DaysSinceListed)})
	}
	params = append(params, [2]string{"radius", strconv.Itoa(radius)}, [2]string{"sortBy", "creation_time_descend"})
	return Origin + "/marketplace/" + city + "/vehicles" + formEncode(params)
}

// ActorOptions build the actor's input. Zero values take the JS defaults.
type ActorOptions struct {
	City                string  // "montreal"
	Location            string  // "Montreal, Quebec, Canada"
	Latitude, Longitude float64 // 45.5019, -73.5674
	RadiusKm            int     // 100
	DaysSinceListed     *int
	MinPrice, MaxPrice  *int
	MaxItems            int // 200
	OnlyNew             bool
	CacheStorageID      string
	URLs                []string
}

// Proxy pins the actor's exit country.
type Proxy struct {
	UseApifyProxy     bool   `json:"useApifyProxy"`
	ApifyProxyCountry string `json:"apifyProxyCountry"`
}

// ActorInput is the actor's published input schema; field names are not
// guesses (an earlier `startUrls` run "succeeded" returning one error object).
type ActorInput struct {
	URLs                []string `json:"urls"`
	Location            string   `json:"location"`
	Latitude            float64  `json:"latitude"`
	Longitude           float64  `json:"longitude"`
	RadiusKm            int      `json:"radiusKm"`
	SortBy              string   `json:"sortBy"`
	Proxy               Proxy    `json:"proxy"`
	StrictFiltering     bool     `json:"strictFiltering"`
	GetListingDetails   bool     `json:"getListingDetails"`
	GetAllListingPhotos bool     `json:"getAllListingPhotos"`
	MaxPagesPerURL      int      `json:"maxPagesPerUrl"`
	MaxItems            int      `json:"maxItems"`
	ResultsLimit        int      `json:"resultsLimit"`
	DaysSinceListed     *string  `json:"daysSinceListed,omitempty"`
	MinPrice            *int     `json:"minPrice,omitempty"`
	MaxPrice            *int     `json:"maxPrice,omitempty"`
	OnlyNewListings     bool     `json:"onlyNewListings,omitempty"`
	CacheStorageID      string   `json:"cacheStorageId,omitempty"`
}

// BuildActorInput fills the actor input.
//
// `/cars`, not `/vehicles`: the latter came back half snowmobiles and
// tractors, billed per result. OnlyNew requires daysSinceListed=1 in every URL
// and a stable cache id, which together make a daily run cheap. The actor
// ignores location/radius once URLs are given; they are a fallback only.
// strictFiltering stays off (with a URL every result is a "broad match").
// maxPagesPerUrl is the cap the actor actually honours; maxItems is not.
func BuildActorInput(o ActorOptions) ActorInput {
	if o.City == "" {
		o.City = "montreal"
	}
	if o.Location == "" {
		o.Location = "Montreal, Quebec, Canada"
	}
	if o.Latitude == 0 && o.Longitude == 0 {
		o.Latitude, o.Longitude = 45.5019, -73.5674
	}
	if o.RadiusKm == 0 {
		o.RadiusKm = 100
	}
	if o.MaxItems == 0 {
		o.MaxItems = 200
	}
	urls := o.URLs
	if len(urls) == 0 {
		u := Origin + "/marketplace/" + o.City + "/cars"
		if o.OnlyNew {
			u += "?daysSinceListed=1"
		}
		urls = []string{u}
	}
	in := ActorInput{
		URLs: urls, Location: o.Location, Latitude: o.Latitude, Longitude: o.Longitude,
		RadiusKm: o.RadiusKm, SortBy: "creation_time_descend",
		// An uncontrolled exit node returns another country's cars.
		Proxy:             Proxy{UseApifyProxy: true, ApifyProxyCountry: "CA"},
		GetListingDetails: true,
		MaxPagesPerURL:    int(math.Max(1, math.Ceil(float64(o.MaxItems)/24))),
		MaxItems:          o.MaxItems,
		ResultsLimit:      o.MaxItems,
		MinPrice:          o.MinPrice,
		MaxPrice:          o.MaxPrice,
	}
	if o.DaysSinceListed != nil {
		in.DaysSinceListed = listing.Str(strconv.Itoa(*o.DaysSinceListed))
	}
	if o.OnlyNew {
		in.OnlyNewListings = true
		in.CacheStorageID = o.CacheStorageID
	}
	return in
}

// ActorRun configures RunActor. Zero values take the JS defaults.
type ActorRun struct {
	Input     *ActorInput // default BuildActorInput({MaxItems})
	MaxItems  int         // 200; also the cost ceiling
	MaxCost   float64     // 1.0; refuse above this estimate, abort above this spend
	Token     string      // APIFY_TOKEN; required
	Client    *http.Client
	BaseURL   string        // ApifyBase
	PollEvery time.Duration // 5s
	Timeout   time.Duration // 30 minutes
	Sleep     func(time.Duration)
}

// ActorResult is a finished run's dataset.
type ActorResult struct {
	RunID string
	Items []map[string]any
}

// RunActor starts the actor, polls until it finishes, and returns its dataset.
// It watches the meter as well as the clock: the actor does not honour an item
// cap, so aborting on reported spend is the only ceiling that holds. An error
// object in a SUCCEEDED dataset is a rejected input, not "zero cars today".
func RunActor(ctx context.Context, r ActorRun) (ActorResult, error) {
	if r.MaxItems == 0 {
		r.MaxItems = 200
	}
	if r.MaxCost == 0 {
		r.MaxCost = 1.0
	}
	if r.Token == "" {
		return ActorResult{}, fmt.Errorf("APIFY_TOKEN is not set: get one from console.apify.com and put it in .env")
	}
	if est := EstimateCost(r.MaxItems); est > r.MaxCost {
		return ActorResult{}, fmt.Errorf("refusing to start: %d items would cost about $%s, over the $%s ceiling. "+
			"Raise maxCost deliberately or lower maxItems.", r.MaxItems, jstext.Number(est), jstext.Number(r.MaxCost))
	}
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	base := r.BaseURL
	if base == "" {
		base = ApifyBase
	}
	poll := r.PollEvery
	if poll == 0 {
		poll = 5 * time.Second
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	sleep := r.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	input := r.Input
	if input == nil {
		in := BuildActorInput(ActorOptions{MaxItems: r.MaxItems})
		input = &in
	}
	body, _ := json.Marshal(input)
	token := "token=" + formEscape(r.Token)

	do := func(method, url string, payload []byte) (int, []byte, error) {
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rd)
		if err != nil {
			return 0, nil, err
		}
		if payload != nil {
			req.Header.Set("content-type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return resp.StatusCode, b, err
	}
	ok := func(status int) bool { return status >= 200 && status < 300 }

	status, b, err := do(http.MethodPost, base+"/acts/"+Actor+"/runs?"+token, body)
	if err != nil {
		return ActorResult{}, err
	}
	if !ok(status) {
		return ActorResult{}, fmt.Errorf("Apify refused the run: HTTP %d %s", status, jstext.SliceUTF16(string(b), 0, 200))
	}
	var start struct {
		Data struct{ ID, Status string } `json:"data"`
	}
	if err := json.Unmarshal(b, &start); err != nil {
		return ActorResult{}, err
	}
	run := start.Data

	abort := func(why string) error {
		_, _, _ = do(http.MethodPost, base+"/actor-runs/"+run.ID+"/abort?"+token, nil)
		return fmt.Errorf("Apify run %s aborted: %s", run.ID, why)
	}

	deadline := time.Now().Add(timeout)
	state := run.Status
	for state == "READY" || state == "RUNNING" {
		if time.Now().After(deadline) {
			return ActorResult{}, abort(fmt.Sprintf("still %s after %dms", state, timeout.Milliseconds()))
		}
		sleep(poll)
		status, b, err := do(http.MethodGet, base+"/actor-runs/"+run.ID+"?"+token, nil)
		if err != nil {
			return ActorResult{}, err
		}
		if !ok(status) {
			return ActorResult{}, fmt.Errorf("Apify status check failed: HTTP %d", status)
		}
		var p struct {
			Data struct {
				Status        string   `json:"status"`
				UsageTotalUsd *float64 `json:"usageTotalUsd"`
			} `json:"data"`
		}
		if err := json.Unmarshal(b, &p); err != nil {
			return ActorResult{}, err
		}
		state = p.Data.Status
		spent := 0.0
		if p.Data.UsageTotalUsd != nil {
			spent = *p.Data.UsageTotalUsd
		}
		if spent > r.MaxCost {
			return ActorResult{}, abort(fmt.Sprintf("spend reached $%.2f, over the $%s ceiling", spent, jstext.Number(r.MaxCost)))
		}
	}
	if state != "SUCCEEDED" {
		return ActorResult{}, fmt.Errorf("Apify run %s finished as %s", run.ID, state)
	}

	status, b, err = do(http.MethodGet, base+"/actor-runs/"+run.ID+"/dataset/items?"+token+"&clean=true", nil)
	if err != nil {
		return ActorResult{}, err
	}
	if !ok(status) {
		return ActorResult{}, fmt.Errorf("Apify dataset fetch failed: HTTP %d", status)
	}
	var items []map[string]any
	if err := json.Unmarshal(b, &items); err != nil {
		return ActorResult{}, err
	}
	for _, it := range items {
		if truthy(it["error"]) && !truthy(it["id"]) {
			return ActorResult{}, fmt.Errorf("Apify actor rejected the input: %v", it["error"])
		}
	}
	return ActorResult{RunID: run.ID, Items: items}, nil
}

// ---------------------------------------------------------------------------
// Normalizing the actor's items

// truthy is JavaScript truthiness for decoded JSON.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0 && !math.IsNaN(x)
	}
	return true
}

// at walks a path of object keys, nil when any step is missing.
func at(v any, path ...string) any {
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// coalesce is `a ?? b ?? …`: the first value that is not null/undefined.
func coalesce(vs ...any) any {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

func strPtr(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

func numPtr(v any) *float64 {
	if f, ok := v.(float64); ok {
		return &f
	}
	return nil
}

var moneyJunk = regexp.MustCompile(`[^\d.]`)

// ParseMoney reads Marketplace's decimal strings: "2990.00" is $2 990.
// Stripping every non-digit, as the other sources do, would read $299 000.
func ParseMoney(v any) *float64 {
	switch x := v.(type) {
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return nil
		}
		return listing.Num(jsRound(x))
	case string:
		cleaned := moneyJunk.ReplaceAllString(x, "")
		if cleaned == "" {
			return nil
		}
		f, ok := parseFloatPrefix(cleaned)
		if !ok {
			return nil
		}
		return listing.Num(jsRound(f))
	}
	return nil
}

// parseFloatPrefix is Number.parseFloat on a string of digits and dots: the
// longest leading number ("1.2.3" is 1.2).
func parseFloatPrefix(s string) (float64, bool) {
	end, dot := 0, false
	for end < len(s) {
		if s[end] == '.' {
			if dot {
				break
			}
			dot = true
		}
		end++
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(s[:end], "."), 64)
	if err != nil {
		f, err = strconv.ParseFloat(s[:end], 64)
	}
	return f, err == nil
}

// parseInteger keeps only the digits of a string; numbers are rounded.
func parseInteger(v any) *float64 {
	switch x := v.(type) {
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return nil
		}
		return listing.Num(jsRound(x))
	case string:
		digits := nonDigit.ReplaceAllString(x, "")
		if digits == "" {
			return nil
		}
		f, err := strconv.ParseFloat(digits, 64)
		if err != nil {
			return nil
		}
		return &f
	}
	return nil
}

// postalPrefix: the first letter of a Canadian postal code is the province.
var postalPrefix = map[byte]string{
	'A': "NL", 'B': "NS", 'C': "PE", 'E': "NB", 'G': "QC", 'H': "QC", 'J': "QC",
	'K': "ON", 'L': "ON", 'M': "ON", 'N': "ON", 'P': "ON",
	'R': "MB", 'S': "SK", 'T': "AB", 'V': "BC", 'X': "NT", 'Y': "YT",
}

// idText is `${id}` for a decoded JSON value.
func idText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return jstext.Number(x)
	case bool:
		return strconv.FormatBool(x)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// NormalizeMarketplace maps one actor item into the shared schema. No make,
// model, year or seller type as fields: everything comes out of the title
// through the matcher. Every seller is treated as private, the conservative
// error. The description is the seller's: data, never instructions.
func NormalizeMarketplace(raw map[string]any, match Matcher) Item {
	title, _ := coalesce(raw["marketplace_listing_title"], raw["title"], "").(string)
	description := ""
	if d := parse.StripHTML(coalesce(at(raw, "redacted_description", "text"), raw["description"], "")); d != nil {
		description = *d
	}
	damage := describe.Read(&description)
	vehicle := Vehicle{}
	if match != nil {
		vehicle = match(title, description)
	}
	price := ParseMoney(coalesce(at(raw, "listing_price", "amount"), at(raw, "price", "amount"),
		at(raw, "listing_price", "formatted_amount"), raw["price"]))

	// `raw.id ?? raw.listingId ?? raw.listing_id`, keeping JS's "undefined"
	// (absent) apart from "null" (present and null).
	id := coalesce(raw["id"], raw["listingId"], raw["listing_id"])
	idKey := "undefined"
	if _, present := raw["listing_id"]; present {
		idKey = "null"
	}
	if id != nil {
		idKey = idText(id)
	}
	ref := ""
	if id != nil {
		ref = idText(id)
	}
	var url *string
	if u, ok := raw["listingUrl"].(string); ok {
		url = &u
	} else if truthy(id) {
		url = listing.Str(ItemURL(idText(id)))
	}

	// The reverse geocode carries a trimmed postal code but often no city or
	// province, so the province comes from the postal prefix.
	geo, _ := at(raw, "location", "reverse_geocode").(map[string]any)
	postal, _ := coalesce(geo["postal_code_trimmed"], geo["postal_code"], "").(string)
	postal = strings.Map(func(r rune) rune {
		if jstext.IsSpace(r) {
			return -1
		}
		return r
	}, strings.ToUpper(postal))
	var postalCode, province *string
	if postal != "" {
		postalCode = &postal
		if p, ok := postalPrefix[postal[0]]; ok {
			province = &p
		}
	}
	if st := strPtr(geo["state"]); st != nil {
		province = st
	}

	// The full gallery is switched off to save requests; the cover still comes.
	photos, _ := raw["listing_photos"].([]any)
	cover := strPtr(raw["primary_listing_photo_url"])
	imageCount := len(photos)
	if imageCount == 0 && cover != nil && *cover != "" {
		imageCount = 1
	}
	var candidates []any
	if len(photos) > 0 {
		for _, p := range photos {
			candidates = append(candidates, coalesce(at(p, "image", "uri"), at(p, "uri")))
		}
	} else {
		candidates = []any{coalesce(raw["primary_listing_photo_url"], at(raw, "primary_listing_photo", "image", "uri"))}
	}
	images := []string{}
	for _, c := range candidates {
		if truthy(c) && len(images) < parse.MaxStoredImages {
			images = append(images, idText(c))
		}
	}

	var listedAt *string
	if c := raw["creation_time"]; truthy(c) {
		if sec, ok := c.(float64); ok {
			listedAt = listing.Str(isoSeconds(sec))
		} else if s, ok := c.(string); ok {
			if sec, err := strconv.ParseFloat(jstext.Trim(s), 64); err == nil {
				listedAt = listing.Str(isoSeconds(sec))
			}
		}
	}

	return Item{
		Listing: listing.Listing{
			ID:           listing.Str("fbmp:" + idKey),
			ReferenceID:  &ref,
			URL:          url,
			Source:       SourceName,
			Price:        price,
			Make:         vehicle.Make,
			Model:        vehicle.Model,
			Year:         vehicle.Year,
			TrimText:     &title,
			Km:           parseInteger(coalesce(at(raw, "vehicle_odometer_data", "value"), raw["odometer"], raw["mileage"])),
			Transmission: strPtr(raw["vehicle_transmission_type"]),
			Fuel:         strPtr(raw["vehicle_fuel_type"]),
			// Marketplace's apparent discount is mostly disclosed defects.
			IsDamaged:   damage.IsDamaged,
			IsParts:     damage.IsParts,
			Condition:   listing.Str("U"),
			SellerType:  listing.Str("PrivateSeller"),
			SellerID:    strPtr(at(raw, "marketplace_listing_seller", "id")),
			SellerName:  strPtr(at(raw, "marketplace_listing_seller", "name")),
			City:        strPtr(coalesce(geo["city"], at(geo, "city_page", "display_name"), at(raw, "location_text", "text"))),
			PostalCode:  postalCode,
			Province:    province,
			Description: &description,
			ImageCount:  imageCount,
			ImageURLs:   images,
			ResultType:  listing.Str("Organic"),
		},
		Title:     &title,
		ListedAt:  listedAt,
		Latitude:  numPtr(at(raw, "location", "latitude")),
		Longitude: numPtr(at(raw, "location", "longitude")),
	}
}

var partWords = regexp.MustCompile(`\b(pour|kit|pneus?|jantes?|roues?|mags?|moteur seul|pi[èe]ces?|bumper|pare-choc|console|radio|si[èe]ge|hood|door|porte)\b`)

// looksLikePart is lespac.js looksLikePart: "for a Mercedes" is a part,
// "Mercedes for sale" is a car. Parts are cheap and name a donor vehicle.
func looksLikePart(title string, price *float64) bool {
	return partWords.MatchString(strings.ToLower(title)) && (price == nil || *price < 3000)
}

// NormalizeMarketplaceItems normalizes a dataset, dropping parts and rows
// with no id.
func NormalizeMarketplaceItems(items []map[string]any, match Matcher) (listings []Item, skipped int) {
	listings = []Item{}
	for _, raw := range items {
		it := NormalizeMarketplace(raw, match)
		if it.Key() == "fbmp:undefined" || looksLikePart(listing.Deref(it.Title), it.Price) {
			skipped++
			continue
		}
		listings = append(listings, it)
	}
	return listings, skipped
}
