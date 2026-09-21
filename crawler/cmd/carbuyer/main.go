// Command carbuyer searches used-car sites (AutoHebdo, Kijiji, LesPAC,
// Facebook Marketplace, Craigslist, CarGurus), stores what it finds in SQLite, and
// prints the best deals as scored by the Rust carbuyer-scorer.
// Go port of src/cli.js, plus the store and score steps.
//
//	carbuyer --make toyota --model rav4
//	carbuyer --make honda --model civic --private --max-price 15000 --pages 3
//	carbuyer --make toyota --model rav4 --offline testdata/rav4-qc.html
//	carbuyer --source kijiji --geo ontario --pages 2
//	carbuyer --source craigslist --geo toronto --private
//	CARBUYER_INGEST_TOKEN=... carbuyer --make toyota --model rav4 --push https://carbuyer-api.example.workers.dev
//	CARBUYER_INGEST_TOKEN=... carbuyer --searches searches.yml --no-score --push https://carbuyer-api.example.workers.dev
//
// This is the composition root: the only place concrete types are chosen.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"carbuyer/crawler/internal/fetch"
	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/pipeline"
	"carbuyer/crawler/internal/scoring"
	"carbuyer/crawler/internal/searches"
	"carbuyer/crawler/internal/searchurl"
	"carbuyer/crawler/internal/source"
	"carbuyer/crawler/internal/store"
	"carbuyer/crawler/internal/store/apistore"
	"carbuyer/crawler/internal/store/sqlitestore"
)

// TokenEnv holds the bearer token for --push (the Worker's INGEST_TOKEN).
// It is read from the environment, never from a flag, so it stays out of
// shell history and process listings.
const TokenEnv = "CARBUYER_INGEST_TOKEN"

const help = `carbuyer — search used-car sites, store, and score

  --source <site>       autohebdo (default) | kijiji | lespac | facebook |
                        craigslist | cargurus
  --make <slug>         e.g. toyota (autohebdo, facebook; kijiji refuses it)
  --model <slug>        e.g. rav4 (requires --make)
  --geo <token>         where to look; each site has its own tokens:
                          autohebdo   reg_qc (default) | reg_on | cit_montreal | cit_quebec
                          kijiji      quebec (default) | ontario | montreal | ottawa ...
                          lespac      Quebec only (Montreal, 200 km)
                          facebook    quebec (default, 4 cities) | ontario (14) | a city: toronto ...
                          craigslist  montreal (default) | quebec | sherbrooke | toronto | ottawa ...
                          cargurus    montreal (default) | toronto (100 km around)
  --private / --dealer  restrict to private sellers or dealers
  --min-price --max-price      applied server-side
  --min-year --max-year        applied locally; the site's year filter is inert
  --sort <standard|price|age>
  --pages <n>           pages to walk, 20 listings each (default 1)
  --json                emit JSON instead of tables

  --db <path>           SQLite file (default carbuyer.db)
  --scorer <path>       carbuyer-scorer binary (default $CARBUYER_SCORER or
                        ../scorer/target/release/carbuyer-scorer)
  --min-comps <n>       comps needed before a bucket counts (scorer default 8)
  --top <n>             deals to print (default 15)
  --offline <file.html> replay a saved search page instead of the network
  --push <api-url>      also send every saved listing to the carbuyer Worker
                        (POST <api-url>/api/listings); the bearer token comes
                        from $CARBUYER_INGEST_TOKEN

  --searches <file>     run every search in a YAML file (see searches.yml)
                        one after another, one request at a time, and stop at
                        the first error. Replaces --make/--model/--pages/...
  --no-score            store (and push) only; skip the local Rust scorer
`

type options struct {
	source                                      string
	make, model, geo, sort, db, scorer, offline string
	push, searches                              string
	noScore                                     bool
	private, dealer, json, help                 bool
	minPrice, maxPrice, minYear, maxYear        string
	pages, top                                  int
	minComps                                    string
}

func defaultScorer() string {
	if env := os.Getenv("CARBUYER_SCORER"); env != "" {
		return env
	}
	name := "carbuyer-scorer"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join("..", "scorer", "target", "release", name)
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("carbuyer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, help) }
	fs.StringVar(&o.source, "source", "autohebdo", "")
	fs.StringVar(&o.make, "make", "", "")
	fs.StringVar(&o.model, "model", "", "")
	fs.StringVar(&o.geo, "geo", "", "")
	fs.BoolVar(&o.private, "private", false, "")
	fs.BoolVar(&o.dealer, "dealer", false, "")
	fs.StringVar(&o.minPrice, "min-price", "", "")
	fs.StringVar(&o.maxPrice, "max-price", "", "")
	fs.StringVar(&o.minYear, "min-year", "", "")
	fs.StringVar(&o.maxYear, "max-year", "", "")
	fs.StringVar(&o.sort, "sort", "", "")
	fs.IntVar(&o.pages, "pages", 1, "")
	fs.BoolVar(&o.json, "json", false, "")
	fs.BoolVar(&o.help, "help", false, "")
	fs.StringVar(&o.db, "db", "carbuyer.db", "")
	fs.StringVar(&o.scorer, "scorer", defaultScorer(), "")
	fs.StringVar(&o.minComps, "min-comps", "", "")
	fs.IntVar(&o.top, "top", 15, "")
	fs.StringVar(&o.offline, "offline", "", "")
	fs.StringVar(&o.push, "push", "", "")
	fs.StringVar(&o.searches, "searches", "", "")
	fs.BoolVar(&o.noScore, "no-score", false, "")
	return o, fs.Parse(args)
}

func number(name, v string) (*float64, error) {
	if v == "" {
		return nil, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil, fmt.Errorf("--%s: %q is not a number", name, v)
	}
	return &f, nil
}

func (o options) query() (listing.Query, error) {
	q := listing.Query{Make: o.make, Model: o.model, Geo: o.geo, Sort: o.sort, MaxPages: o.pages}
	if o.private {
		q.SellerType = "P"
	} else if o.dealer {
		q.SellerType = "D"
	}
	var err error
	for _, n := range []struct {
		name, val string
		dst       **float64
	}{
		{"min-price", o.minPrice, &q.PriceFrom}, {"max-price", o.maxPrice, &q.PriceTo},
		{"min-year", o.minYear, &q.YearFrom}, {"max-year", o.maxYear, &q.YearTo},
	} {
		if *n.dst, err = number(n.name, n.val); err != nil {
			return q, err
		}
	}
	return q, nil
}

// money formats a whole number with space-separated thousands: "33 490".
func money(f *float64) string {
	if f == nil {
		return "—"
	}
	v := *f
	neg := v < 0
	if neg {
		v = -v
	}
	s := strconv.FormatInt(int64(v+0.5), 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func seller(l listing.Listing) string {
	if listing.Deref(l.SellerType) == "PrivateSeller" {
		return "private"
	}
	return "dealer"
}

func year(l listing.Listing) string {
	if l.Year == nil {
		return "—"
	}
	return strconv.FormatFloat(*l.Year, 'f', -1, 64)
}

func printReport(w io.Writer, rep pipeline.Report, top int) {
	res := rep.Search
	// Listings, cheapest first (what src/cli.js prints).
	ls := append([]listing.Listing(nil), res.Listings...)
	sort.SliceStable(ls, func(i, j int) bool {
		if ls[i].Price == nil || ls[j].Price == nil {
			return ls[i].Price != nil
		}
		return *ls[i].Price < *ls[j].Price
	})
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "year\tcar\ttrim\tprice\tkm\tseller\tcity")
	for _, l := range ls {
		fmt.Fprintf(tw, "%s\t%s %s\t%s\t%s\t%s\t%s\t%s\n", year(l), listing.Deref(l.Make), listing.Deref(l.Model),
			cut(listing.Deref(l.TrimText), 28), money(l.Price), money(l.Km), seller(l), listing.Deref(l.City))
	}
	tw.Flush()

	var notes []string
	if res.Injected > 0 {
		notes = append(notes, fmt.Sprintf("%d out-of-region listings filtered out", res.Injected))
	}
	if res.ShortPages > 0 {
		notes = append(notes, fmt.Sprintf("%d short pages re-fetched", res.ShortPages))
	}
	if res.Shortfall > 0 && !res.Truncated {
		notes = append(notes, fmt.Sprintf("%d sold mid-search", res.Shortfall))
	}
	line := fmt.Sprintf("%d listings shown · %s match the server-side query", len(res.Listings), money(res.Total))
	if len(notes) > 0 {
		line += " · " + strings.Join(notes, " · ")
	}
	fmt.Fprintln(w, line)
	fmt.Fprintln(w, res.URL)

	// Best deals, as scored by the Rust scorer.
	fmt.Fprintf(w, "\nBest deals (%d of %d found cars could be priced against %d stored comps)\n",
		rep.Scored, len(res.Listings), rep.Comps)
	if rep.Scored == 0 {
		fmt.Fprintln(w, "  none yet: too few comparable cars stored. Crawl more pages, or try --min-comps 3.")
		return
	}
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "under\tyear\tcar\tprice\tbaseline\tcomps\tbasis\tseller\turl")
	shown := 0
	for _, d := range rep.Deals {
		if d.Score == nil || shown >= top {
			continue
		}
		shown++
		s := d.Score
		flags := ""
		if s.Damaged {
			flags = " (damaged)"
		}
		fmt.Fprintf(tw, "%+.1f%%\t%s\t%s %s\t%s\t%s\t%d\t%s\t%s%s\t%s\n", s.DiscountPct, year(d.Listing),
			listing.Deref(d.Listing.Make), listing.Deref(d.Listing.Model), money(&s.Price), money(&s.Baseline),
			s.N, s.Basis, seller(d.Listing), flags, listing.Deref(d.Listing.URL))
	}
	tw.Flush()
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) || o.help {
		fmt.Fprint(stdout, help)
		return 0
	}
	if err != nil {
		return 2
	}
	q, err := o.query()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	src, err := searches.SourceName(o.source)
	if err != nil {
		fmt.Fprintln(stderr, "--"+err.Error())
		return 2
	}
	if src == "autohebdo" && q.Geo == "" {
		q.Geo = searchurl.GeoQuebec
	}
	delay := fetch.DefaultDelay
	var plan searches.Plan
	if o.searches != "" {
		if o.make != "" || o.model != "" {
			fmt.Fprintln(stderr, "--searches runs the searches in the file; do not also pass --make/--model")
			return 2
		}
		if plan, err = searches.Load(o.searches); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		delay = plan.Delay
	}

	// Composition root: concrete implementations are chosen here and injected.
	// One throttled fetcher for the whole run: requests never overlap and are
	// spaced by `delay`, across searches too.
	kit := &sourceKit{throttle: fetch.NewThrottle(delay)}
	if o.offline != "" {
		body, err := os.ReadFile(o.offline)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		kit.offline = fetch.Static{Body: string(body)}
	}
	exec := &scoring.ExecScorer{Binary: o.scorer}
	if o.minComps != "" {
		n, err := strconv.ParseFloat(o.minComps, 64)
		if err != nil {
			fmt.Fprintf(stderr, "--min-comps: %q is not a number\n", o.minComps)
			return 2
		}
		exec.Options = map[string]any{"model": map[string]any{"minComps": n}}
	}
	var scorer scoring.Scorer = exec
	if o.noScore {
		scorer = scoring.None{}
	}
	// --push: the Worker API is a second writer next to SQLite (store.Tee).
	var pusher *apistore.Client
	if o.push != "" {
		if pusher, err = apistore.New(o.push, os.Getenv(TokenEnv)); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	db, err := sqlitestore.Open(o.db)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer db.Close()
	var st store.Store = db
	if pusher != nil {
		st = store.Tee(db, pusher)
	}
	kit.db = db
	pipelines := map[string]*pipeline.Pipeline{}
	pipelineFor := func(name string) (*pipeline.Pipeline, error) {
		if p, ok := pipelines[name]; ok {
			return p, nil
		}
		s, err := kit.build(ctx, name)
		if err != nil {
			return nil, err
		}
		p := pipeline.New(s, st, scorer)
		p.CompsFromAllSources = name != "autohebdo"
		pipelines[name] = p
		return p, nil
	}

	var onPage func(source.PageEvent)
	if !o.json {
		onPage = func(e source.PageEvent) {
			fmt.Fprintf(stderr, "  page %d: +%d (of %s total)\n", e.Page, e.Found, money(e.Total))
		}
	}
	if o.searches != "" {
		code := runPlan(ctx, pipelineFor, plan, onPage, o.json, stdout, stderr)
		printPushTotals(stderr, pusher)
		return code
	}
	p, err := pipelineFor(src)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	rep, err := p.Run(ctx, q, onPage)
	if err != nil {
		// Expected failures (bad slug, dead site) read as a message, not a trace.
		fmt.Fprintf(stderr, "\n%v\n", err)
		return 1
	}

	printPushTotals(stderr, pusher)
	res := rep.Search
	if res.Truncated {
		if res.HitSiteCeiling {
			fmt.Fprintf(stderr, "  ! %s results exist but the site stops at %d pages. Narrow by price.\n", money(res.Total), res.PagesWalked)
		} else {
			fmt.Fprintf(stderr, "  ! showing %d of %s pages — pass --pages %s for all %s.\n",
				res.PagesWalked, money(res.Pages), money(res.Pages), money(res.Total))
		}
	}
	if o.json {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	printReport(stdout, rep, o.top)
	return 0
}

func printPushTotals(stderr io.Writer, pusher *apistore.Client) {
	if pusher == nil {
		return
	}
	t := pusher.Totals
	fmt.Fprintf(stderr, "  pushed to %s: %d seen, %d added, %d relisted, %d price drops, %d price rises",
		pusher.Endpoint, t.Seen, t.Added, t.Relisted, t.PriceDrops, t.PriceRises)
	if t.Skipped > 0 {
		fmt.Fprintf(stderr, ", %d skipped (no id)", t.Skipped)
	}
	fmt.Fprintf(stderr, ", %d new deal alerts\n", t.NewAlerts)
	if t.AlertError != "" {
		fmt.Fprintf(stderr, "  ! the Worker stored the listings but alerting failed: %s\n", t.AlertError)
	}
}

// planResult is one search of a --searches run, for --json.
type planResult struct {
	Name   string          `json:"name"`
	Report pipeline.Report `json:"report"`
}

// runPlan runs the searches one after another through the same pipeline and
// stops at the first error: a failing or blocking site gets no more requests.
func runPlan(ctx context.Context, pipelineFor func(string) (*pipeline.Pipeline, error), plan searches.Plan, onPage func(source.PageEvent),
	asJSON bool, stdout, stderr io.Writer) int {
	var results []planResult
	for i, s := range plan.Searches {
		fmt.Fprintf(stderr, "[%d/%d] %s\n", i+1, len(plan.Searches), s.Name)
		p, err := pipelineFor(s.Source)
		if err != nil {
			fmt.Fprintf(stderr, "\n%s: %v\nstopping: %d of %d searches done\n", s.Name, err, i, len(plan.Searches))
			return 1
		}
		rep, err := p.Run(ctx, s.Query, onPage)
		if err != nil {
			fmt.Fprintf(stderr, "\n%s: %v\nstopping: %d of %d searches done\n", s.Name, err, i, len(plan.Searches))
			return 1
		}
		results = append(results, planResult{Name: s.Name, Report: rep})
		if !asJSON {
			sv := rep.Saved
			fmt.Fprintf(stdout, "%s: %d listings (%s match), %d new, %d relisted, %d price drops, %d priced locally\n",
				s.Name, len(rep.Search.Listings), money(rep.Search.Total), sv.Added, sv.Relisted, sv.PriceDrops, rep.Scored)
		}
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	return 0
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
