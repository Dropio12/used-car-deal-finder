package searchurl

import (
	"net/url"
	"strings"
	"testing"
)

func f(v float64) *float64 { return &v }
func b(v bool) *bool       { return &v }

func mustBuild(t *testing.T, o Options) *url.URL {
	t.Helper()
	raw, err := Build(o)
	if err != nil {
		t.Fatalf("Build(%+v): %v", o, err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestBuildExactURLs(t *testing.T) {
	// Expected strings produced by the original buildSearchUrl() in Node.
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"defaults", Options{}, "https://www.autohebdo.net/autos/reg_qc?atype=C&cy=CA&ustate=U&damaged_listing=exclude"},
		{"fixture query", Options{Make: "toyota", Model: "rav4", Geo: GeoQuebec},
			"https://www.autohebdo.net/autos/toyota/rav4/reg_qc?atype=C&cy=CA&ustate=U&damaged_listing=exclude"},
		{"all params", Options{Make: "honda", Model: "civic", Geo: GeoMontreal, SellerType: "P", Page: 3, Sort: "price",
			Descending: b(false), PriceFrom: f(5000), PriceTo: f(20000)},
			"https://www.autohebdo.net/autos/honda/civic/cit_montreal?atype=C&cy=CA&ustate=U&damaged_listing=exclude&custtype=P&page=3&sort=price&desc=0&pricefrom=5000&priceto=20000"},
		{"new and used encoded", Options{Condition: "N,U"}, "https://www.autohebdo.net/autos/reg_qc?atype=C&cy=CA&ustate=N%2CU&damaged_listing=exclude"},
		{"slug escaped", Options{Make: "mercedes benz"}, "https://www.autohebdo.net/autos/mercedes%20benz/reg_qc?atype=C&cy=CA&ustate=U&damaged_listing=exclude"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Build(c.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

func TestGeoIsAPathSegmentAndZipIsNeverSent(t *testing.T) {
	u := mustBuild(t, Options{Make: "toyota", Model: "rav4", Geo: GeoMontreal})
	if u.Path != "/autos/toyota/rav4/cit_montreal" {
		t.Errorf("path = %s", u.Path)
	}
	for _, forbidden := range []string{"zip", "zipr", "fregfrom", "fregto", "cit_montreal"} {
		if u.Query().Has(forbidden) {
			t.Errorf("query must not carry %q: %s", forbidden, u.RawQuery)
		}
	}
}

func TestPageOneIsOmitted(t *testing.T) {
	for _, page := range []int{0, 1} {
		if mustBuild(t, Options{Page: page}).Query().Has("page") {
			t.Errorf("page=%d should not be sent", page)
		}
	}
	if got := mustBuild(t, Options{Page: 2}).Query().Get("page"); got != "2" {
		t.Errorf("page 2 = %q", got)
	}
}

func TestDescendingTrue(t *testing.T) {
	if got := mustBuild(t, Options{Descending: b(true)}).Query().Get("desc"); got != "1" {
		t.Errorf("desc = %q", got)
	}
}

func TestModelWithoutMakeIsRejected(t *testing.T) {
	if _, err := Build(Options{Model: "rav4"}); err == nil || !strings.Contains(err.Error(), "requires `make`") {
		t.Errorf("err = %v", err)
	}
}

func TestNormalizePostalCode(t *testing.T) {
	good := map[string]string{"H2Y1C6": "H2Y1C6", "H2Y 1C6": "H2Y1C6", "h2y 1c6": "H2Y1C6", "h2y 1c6": "H2Y1C6"}
	for in, want := range good {
		got, err := NormalizePostalCode(in)
		if err != nil || got != want {
			t.Errorf("NormalizePostalCode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"H2X", "H2X2L", "12345", "", "D2X2L5"} {
		if _, err := NormalizePostalCode(bad); err == nil || !strings.Contains(err.Error(), "invalid postal code") {
			t.Errorf("NormalizePostalCode(%q) err = %v", bad, err)
		}
	}
}
