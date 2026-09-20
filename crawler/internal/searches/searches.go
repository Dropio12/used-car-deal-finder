// Package searches reads the list of saved searches a scheduled crawl runs
// (crawler/searches.yml). It only turns the file into listing.Query values;
// running them is the CLI's job.
package searches

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"carbuyer/crawler/internal/fetch"
	"carbuyer/crawler/internal/listing"
)

// MinDelay is the shortest spacing between two requests a config may ask
// for: the crawler's normal polite delay.
const MinDelay = fetch.DefaultDelay

// MaxSearches and MaxPages keep a scheduled run small, whatever the file says.
const (
	MaxSearches = 25
	MaxPages    = 10
)

// Search is one entry. Zero values fall back to the file's defaults.
type Search struct {
	Name     string   `yaml:"name"`
	Make     string   `yaml:"make"`
	Model    string   `yaml:"model"`
	Geo      string   `yaml:"geo"`
	Seller   string   `yaml:"seller"` // any | dealer | private
	MinPrice *float64 `yaml:"minPrice"`
	MaxPrice *float64 `yaml:"maxPrice"`
	MinYear  *float64 `yaml:"minYear"`
	MaxYear  *float64 `yaml:"maxYear"`
	Pages    int      `yaml:"pages"`
	// Newest walks newest listings first (sort=age&desc=1), which is what a
	// frequent "anything new?" crawl wants.
	Newest *bool `yaml:"newest"`
}

// File is the whole config.
type File struct {
	// DelaySeconds between two page requests (never below MinDelay).
	DelaySeconds float64  `yaml:"delaySeconds"`
	Defaults     Search   `yaml:"defaults"`
	Searches     []Search `yaml:"searches"`
}

// Named is a ready-to-run query with a label for logs.
type Named struct {
	Name  string
	Query listing.Query
}

// Plan is what a scheduled run does, in order.
type Plan struct {
	Delay    time.Duration
	Searches []Named
}

// Load reads and validates a searches file.
func Load(path string) (Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Plan{}, err
	}
	p, err := Parse(b)
	if err != nil {
		return Plan{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Parse validates the YAML text. Unknown keys are errors, so a typo like
// `maxprice` cannot silently drop the price cap.
func Parse(b []byte) (Plan, error) {
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return Plan{}, err
	}
	if len(f.Searches) == 0 {
		return Plan{}, errors.New("no searches: add at least one entry under `searches:`")
	}
	if len(f.Searches) > MaxSearches {
		return Plan{}, fmt.Errorf("%d searches: at most %d per run, to stay polite", len(f.Searches), MaxSearches)
	}
	delay := time.Duration(f.DelaySeconds * float64(time.Second))
	if delay < MinDelay {
		delay = MinDelay
	}
	plan := Plan{Delay: delay}
	for i, s := range f.Searches {
		n, err := merge(f.Defaults, s).named()
		if err != nil {
			return Plan{}, fmt.Errorf("searches[%d]: %w", i, err)
		}
		plan.Searches = append(plan.Searches, n)
	}
	return plan, nil
}

func merge(d, s Search) Search {
	str := func(v, def string) string {
		if v != "" {
			return v
		}
		return def
	}
	num := func(v, def *float64) *float64 {
		if v != nil {
			return v
		}
		return def
	}
	out := Search{
		Name: s.Name, Make: s.Make, Model: s.Model,
		Geo: str(s.Geo, d.Geo), Seller: str(s.Seller, d.Seller),
		MinPrice: num(s.MinPrice, d.MinPrice), MaxPrice: num(s.MaxPrice, d.MaxPrice),
		MinYear: num(s.MinYear, d.MinYear), MaxYear: num(s.MaxYear, d.MaxYear),
		Pages: s.Pages, Newest: s.Newest,
	}
	if out.Pages == 0 {
		out.Pages = d.Pages
	}
	if out.Newest == nil {
		out.Newest = d.Newest
	}
	return out
}

func (s Search) named() (Named, error) {
	if s.Make == "" {
		return Named{}, errors.New("`make` is required (a slug, e.g. toyota)")
	}
	for _, v := range []string{s.Make, s.Model, s.Geo} {
		if strings.ContainsAny(v, "/?#&= ") {
			return Named{}, fmt.Errorf("%q is not a slug", v)
		}
	}
	q := listing.Query{
		Make: strings.ToLower(s.Make), Model: strings.ToLower(s.Model), Geo: s.Geo,
		PriceFrom: s.MinPrice, PriceTo: s.MaxPrice, YearFrom: s.MinYear, YearTo: s.MaxYear,
		MaxPages: s.Pages,
	}
	if q.MaxPages <= 0 {
		q.MaxPages = 1
	}
	if q.MaxPages > MaxPages {
		return Named{}, fmt.Errorf("pages %d: at most %d per search", q.MaxPages, MaxPages)
	}
	switch strings.ToLower(s.Seller) {
	case "", "any":
	case "dealer":
		q.SellerType = "D"
	case "private":
		q.SellerType = "P"
	default:
		return Named{}, fmt.Errorf("seller %q: use any, dealer or private", s.Seller)
	}
	for _, p := range []*float64{q.PriceFrom, q.PriceTo, q.YearFrom, q.YearTo} {
		if p != nil && *p < 0 {
			return Named{}, errors.New("prices and years cannot be negative")
		}
	}
	if s.Newest != nil && *s.Newest {
		q.Sort = "age"
		desc := true
		q.Descending = &desc
	}
	name := s.Name
	if name == "" {
		name = strings.TrimSpace(q.Make + " " + q.Model)
	}
	return Named{Name: name, Query: q}, nil
}
