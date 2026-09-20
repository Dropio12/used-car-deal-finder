package parse

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"carbuyer/crawler/internal/fetch"
	"carbuyer/crawler/internal/listing"
)

func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "rav4-qc.html"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSearchPageFixture(t *testing.T) {
	page, err := SearchPage(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if page.Total == nil || *page.Total != 861 || page.Pages == nil || *page.Pages != 44 {
		t.Errorf("total/pages = %v/%v", page.Total, page.Pages)
	}
	if len(page.Listings) != 20 {
		t.Fatalf("a full page is 20 organic listings, got %d", len(page.Listings))
	}
	if len(page.AllListings) != len(page.Listings)+page.InjectedCount {
		t.Error("organic + injected must account for the whole page")
	}
	if page.Query["cat"] != "ma70gr201439" {
		t.Errorf("pageQuery.cat = %v", page.Query["cat"])
	}

	usable := 0
	for _, l := range page.Listings {
		where := listing.Deref(l.URL)
		if l.ID == nil || l.Price == nil || l.Make == nil || l.Model == nil {
			t.Errorf("required field missing on %s", where)
		}
		if !strings.HasPrefix(where, "https://") {
			t.Errorf("url missing on %v", l.ID)
		}
		if st := listing.Deref(l.SellerType); st != "Dealer" && st != "PrivateSeller" {
			t.Errorf("sellerType %q on %s", st, where)
		}
		if listing.Deref(l.Province) != "QC" {
			t.Errorf("a /reg_qc search returned %q organic listing", listing.Deref(l.Province))
		}
		if !strings.Contains(strings.ToUpper(listing.Deref(l.Model)), "RAV") {
			t.Errorf("model %q", listing.Deref(l.Model))
		}
		if ok, missing := CompsQuality(l); ok {
			usable++
			if *l.Price <= 500 || *l.Price >= 500_000 || *l.Year < 1990 || *l.Year > 2030 || *l.Km < 0 || *l.Km >= 1_000_000 {
				t.Errorf("implausible values on %s", where)
			}
		} else if len(missing) == 0 {
			t.Error("unusable listing must say what is missing")
		}
		for _, u := range l.ImageURLs {
			// A size segment at the very end is upgraded; one followed by a query
			// string (?promo=...) is left alone, exactly as the JS regex does.
			if strings.HasSuffix(u, "250x188.webp") {
				t.Errorf("thumbnail size kept: %s", u)
			}
		}
		if len(l.ImageURLs) > MaxStoredImages {
			t.Errorf("%d images stored", len(l.ImageURLs))
		}
	}
	if float64(usable)/float64(len(page.Listings)) < 0.8 {
		t.Errorf("only %d/%d usable as comps", usable, len(page.Listings))
	}

	first := page.Listings[0]
	if listing.Deref(first.ID) != "00000000-0000-4000-8000-000000000001" || *first.Price != 33490 || *first.Km != 74574 ||
		*first.Year != 2022 || listing.Deref(first.ReferenceID) != "90000001" || listing.Deref(first.PostalCode) != "J4K 0A0" {
		t.Errorf("first listing = %+v", first)
	}

	var described *listing.Listing
	for i := range page.Listings {
		if d := page.Listings[i].Description; d != nil && len(*d) > 50 {
			described = &page.Listings[i]
			break
		}
	}
	if described == nil {
		t.Fatal("at least one listing should carry a description")
	}
	if strings.Contains(*described.Description, "<br") || regexp.MustCompile(`(?i)&#x[0-9a-f]+;`).MatchString(*described.Description) {
		t.Error("description not decoded")
	}
}

func TestInjectedListingsAreSeparated(t *testing.T) {
	data, err := ExtractNextData(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	props := data.(map[string]any)["props"].(map[string]any)["pageProps"].(map[string]any)
	props["listings"] = append(props["listings"].([]any), map[string]any{
		"id": "injected-1", "searchResultType": "Deliverable",
		"price":    map[string]any{"priceRaw": 19999.0},
		"vehicle":  map[string]any{"make": "Toyota", "modelGroup": "RAV4", "modelYear": 2020.0},
		"location": map[string]any{"provinceCode": "ON", "city": "Toronto"},
	})
	b, _ := json.Marshal(data)
	page, err := SearchPage(`<script id="__NEXT_DATA__" type="application/json">` + string(b) + `</script>`)
	if err != nil {
		t.Fatal(err)
	}
	if page.InjectedCount != 1 || len(page.Listings) != 20 || len(page.AllListings) != 21 {
		t.Errorf("injected=%d organic=%d all=%d", page.InjectedCount, len(page.Listings), len(page.AllListings))
	}
	for _, l := range page.Listings {
		if listing.Deref(l.ID) == "injected-1" {
			t.Error("injected car reached the organic list")
		}
	}
}

func TestSearchPageErrors(t *testing.T) {
	cases := []struct{ name, html, want string }{
		{"empty", "", "empty response body"},
		{"blocked", "<html><body>blocked</body></html>", "not found"},
		{"bad json", `<script id="__NEXT_DATA__">{nope</script>`, "not valid JSON"},
		{"no pageProps", `<script id="__NEXT_DATA__">{"props":{}}</script>`, "no props.pageProps"},
		{"no listings", `<script id="__NEXT_DATA__">{"props":{"pageProps":{"listings":5}}}</script>`, "not an array"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := SearchPage(c.html)
			var pe *ParseError
			if !errors.As(err, &pe) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want ParseError containing %q", err, c.want)
			}
			if !fetch.Answered(err) {
				t.Error("a parse failure means the site answered")
			}
		})
	}
}

func TestParseNumber(t *testing.T) {
	cases := []struct {
		in   any
		want any // float64 or nil
	}{
		{"38 495 km", 38495.0}, {"29 480 $", 29480.0}, {"2 500 cm³", 2500.0}, {29480.0, 29480.0},
		{"- (Année)", nil}, {"", nil}, {nil, nil}, {true, nil},
	}
	for _, c := range cases {
		got := ParseNumber(c.in)
		if (got == nil) != (c.want == nil) || (got != nil && *got != c.want.(float64)) {
			t.Errorf("ParseNumber(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestStripHTML(t *testing.T) {
	cases := map[string]string{
		"a<br />b":                   "a\nb",
		"jusqu&#x27;à":               "jusqu'à",
		"<strong>x</strong> &amp; y": "x & y",
		"a&#233;b &unknown; c":       "aéb &unknown; c",
		"  x \t\t y\n\n\n\nz  ":      "x y\n\nz",
		"a<BR>b":                     "a\nb",
	}
	for in, want := range cases {
		if got := StripHTML(in); got == nil || *got != want {
			t.Errorf("StripHTML(%q) = %v, want %q", in, got, want)
		}
	}
	if StripHTML(nil) != nil || StripHTML(5.0) != nil {
		t.Error("non-strings give nil")
	}
}

func TestFullSizeImage(t *testing.T) {
	thumb := "https://images.example.com/listing-images/00000000-0000-4000-8000-000000000001-1.jpg/250x188.webp"
	cases := map[string]string{
		thumb: strings.Replace(thumb, "250x188", FullImageSize, 1),
		strings.Replace(thumb, ".webp", ".jpg", 1):             strings.Replace(strings.Replace(thumb, ".webp", ".jpg", 1), "250x188", FullImageSize, 1),
		"https://example.test/photo.jpg":                       "https://example.test/photo.jpg",
		"https://scontent.xx.fbcdn.net/v/793337105_252911.jpg": "https://scontent.xx.fbcdn.net/v/793337105_252911.jpg",
	}
	for in, want := range cases {
		if got := FullSizeImage(in); got != want {
			t.Errorf("FullSizeImage(%q) = %q", in, got)
		}
	}
}

func TestIsOrganic(t *testing.T) {
	cases := []struct {
		rt   *string
		want bool
	}{{listing.Str("Organic"), true}, {listing.Str("Deliverable"), false}, {nil, false}}
	for _, c := range cases {
		if got := IsOrganic(listing.Listing{ResultType: c.rt}); got != c.want {
			t.Errorf("IsOrganic(%v) = %v", c.rt, got)
		}
	}
}
