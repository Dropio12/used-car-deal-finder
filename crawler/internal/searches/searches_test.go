package searches

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDefaultsAndOverrides(t *testing.T) {
	plan, err := Parse([]byte(`
delaySeconds: 4
defaults:
  maxPrice: 30000
  pages: 2
  newest: true
searches:
  - make: Toyota
    model: RAV4
  - make: honda
    model: civic
    name: cheap private civics
    seller: private
    maxPrice: 12000
    minYear: 2015
    pages: 1
    newest: false
`))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Delay != 4*time.Second || len(plan.Searches) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	a := plan.Searches[0]
	if a.Name != "toyota rav4" || a.Query.Make != "toyota" || a.Query.Model != "rav4" || a.Query.MaxPages != 2 ||
		*a.Query.PriceTo != 30000 || a.Query.Sort != "age" || a.Query.Descending == nil || !*a.Query.Descending ||
		a.Query.SellerType != "" {
		t.Errorf("first = %+v", a)
	}
	b := plan.Searches[1]
	if b.Name != "cheap private civics" || b.Query.SellerType != "P" || *b.Query.PriceTo != 12000 ||
		*b.Query.YearFrom != 2015 || b.Query.MaxPages != 1 || b.Query.Sort != "" || b.Query.Descending != nil {
		t.Errorf("second = %+v", b)
	}
}

func TestParsePoliteLimits(t *testing.T) {
	plan, err := Parse([]byte("delaySeconds: 0.1\nsearches:\n  - make: toyota\n"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Delay != MinDelay {
		t.Errorf("delay %v, want the %v floor", plan.Delay, MinDelay)
	}
	if plan.Searches[0].Query.MaxPages != 1 {
		t.Errorf("pages default to 1, got %d", plan.Searches[0].Query.MaxPages)
	}
	many := "searches:\n" + strings.Repeat("  - make: toyota\n", MaxSearches+1)
	if _, err := Parse([]byte(many)); err == nil {
		t.Error("too many searches must be refused")
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"no searches":   "delaySeconds: 3\n",
		"typo key":      "searches:\n  - make: toyota\n    maxprice: 100\n",
		"no make":       "searches:\n  - model: rav4\n",
		"bad seller":    "searches:\n  - make: toyota\n    seller: robot\n",
		"too many page": "searches:\n  - make: toyota\n    pages: 50\n",
		"not a slug":    "searches:\n  - make: toyota\n    model: rav4?x=1\n",
		"negative":      "searches:\n  - make: toyota\n    maxPrice: -5\n",
		"bad yaml":      "searches: [\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(text)); err == nil {
				t.Errorf("%q: want an error", text)
			}
		})
	}
}

// The file shipped in the repo must always load.
func TestShippedSearchesFile(t *testing.T) {
	plan, err := Load(filepath.Join("..", "..", "searches.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Searches) == 0 || plan.Delay < MinDelay {
		t.Errorf("plan = %+v", plan)
	}
	for _, s := range plan.Searches {
		if s.Query.PriceTo == nil || s.Query.MaxPages > 3 {
			t.Errorf("%s: keep the default crawl capped and small: %+v", s.Name, s.Query)
		}
	}
}
