# Architecture

C4 diagrams in Mermaid (they render on GitHub), then how SOLID shows up in the code.

## Level 1: System context

```mermaid
C4Context
  title Used-Car Deal Finder (carbuyer): system context
  Person(user, "Car flipper", "Wants underpriced used cars in Quebec before anyone else")
  System(carbuyer, "Used-Car Deal Finder", "Crawls listings on a schedule, stores them, prices each car against local comparable cars, raises snipe alerts")
  System_Ext(autohebdo, "autohebdo.net", "Used-car listing site. Server-rendered Next.js pages with __NEXT_DATA__ JSON")
  System_Ext(gha, "GitHub Actions", "Runs the scheduled crawl every 5 minutes (or Windows Task Scheduler on the owner's PC)")
  System_Ext(access, "Cloudflare Access", "Zero Trust login in front of the dashboard: only the owner's email gets in")
  Rel(gha, carbuyer, "Runs the crawler on a cron", "*/5 * * * *")
  Rel(user, carbuyer, "Runs searches from the CLI, edits searches.yml", "command line")
  Rel(user, access, "Opens the dashboard, checks New deals", "HTTPS, email one-time PIN")
  Rel(access, carbuyer, "Lets authenticated requests through", "HTTPS")
  Rel(carbuyer, autohebdo, "Fetches search pages", "HTTPS GET, browser User-Agent, one at a time, >= 1.5 s apart")
```

## Level 2: Containers

```mermaid
C4Container
  title Used-Car Deal Finder: containers
  Person(user, "Car flipper")
  System_Ext(autohebdo, "autohebdo.net")
  System_Ext(access, "Cloudflare Access", "Allow policy: owner's email")
  System_Boundary(sched, "Scheduler (one of)") {
    Container(gha, "crawl workflow", "GitHub Actions .github/workflows/crawl.yml", "Every 5 min: builds the Go CLI, runs --searches searches.yml --no-score --push; token from a repo secret, URL from a repo variable")
    Container(task, "crawl-and-push.ps1", "PowerShell + Windows Task Scheduler", "Same run from the owner's PC; token from the user environment; lock file, daily log")
  }
  System_Boundary(local, "Owner's machine") {
    Container(cli, "carbuyer CLI", "Go", "Builds URLs, fetches, parses, stores, calls the scorer, prints deals; --searches runs searches.yml; --push mirrors saved listings to the Worker")
    Container(scorer, "carbuyer-scorer", "Rust binary", "Builds the price baseline and scores listings")
    ContainerDb(db, "carbuyer.db", "SQLite (modernc.org/sqlite, pure Go)", "listings, price_history, crawls, meta: same schema as the JS project")
  }
  System_Boundary(cf, "Cloudflare") {
    Container(pages, "carbuyer-dashboard", "Cloudflare Pages: static HTML/CSS/JS + one Pages Function", "New deals (alerts) + deals table; /api/* forwarded to the Worker over a service binding")
    Container(worker, "carbuyer-api", "Cloudflare Worker, Rust (workers-rs) compiled to wasm; reuses the scorer crate", "POST /api/listings (bearer) + snipe alerts, GET /api/deals, /api/alerts, /api/snapshots/latest, scheduled handler")
    Container(cron, "Cron Trigger", "0 11 * * * (UTC)", "Fires the Worker's scheduled handler once a day")
    ContainerDb(d1, "carbuyer", "Cloudflare D1 (SQLite)", "listings/price_history/crawls/meta + deal_snapshots, deal_snapshot_items + new_deal_alerts")
  }
  Rel(gha, cli, "go build + run", "ubuntu runner")
  Rel(task, cli, "go build + run", "Windows")
  Rel(user, cli, "Runs", "command line")
  Rel(cli, autohebdo, "GET search pages, one at a time", "HTTPS")
  Rel(cli, db, "Upserts listings, loads comps", "SQL")
  Rel(cli, scorer, "Listings JSON on stdin, scores JSON on stdout (skipped with --no-score)", "exec")
  Rel(cli, worker, "POST /api/listings, batches of 50", "HTTPS + Bearer INGEST_TOKEN")
  Rel(user, access, "Opens dashboard", "HTTPS")
  Rel(access, pages, "Authenticated requests", "HTTPS")
  Rel(pages, worker, "GET /api/deals, /api/alerts, /api/snapshots/latest", "service binding (internal host)")
  Rel(cron, worker, "scheduled()", "daily")
  Rel(worker, d1, "Upserts, loads active listings, writes alerts and snapshots", "D1 binding, batched statements")
```

## Level 3: Components of the Go crawler

```mermaid
C4Component
  title Go crawler: components (crawler/internal/...)
  Container_Boundary(go, "carbuyer CLI (Go)") {
    Component(main, "cmd/carbuyer", "composition root", "Parses flags, picks concrete types, injects them; --searches loops over a Plan and stops at the first error")
    Component(searches, "searches", "Load(), Plan", "searches.yml -> []listing.Query; polite limits (delay floor, max searches/pages); unknown keys refused")
    Component(pipeline, "pipeline", "Pipeline", "Search -> save -> removal check -> score -> rank")
    Component(sourceI, "source", "interface Source", "Name(), Search(ctx, Query, onPage)")
    Component(autohebdo, "autohebdo", "implements Source", "search/searchAll, short-page retry, query verification")
    Component(fetchI, "fetch", "interface Fetcher + HTTPFetcher, Throttle, Static", "Browser UA, retries 5xx/429, spacing; one Throttle per run")
    Component(searchurl, "searchurl", "Build()", "Geo as path segment, never sends zip or year")
    Component(parse, "parse", "SearchPage()", "__NEXT_DATA__ -> normalized listings")
    Component(describe, "describe", "Read()", "Seller text -> damage / parts flags")
    Component(storeI, "store", "interfaces Writer, Reader, Remover, CrawlLog", "Small storage contracts")
    Component(sqlite, "store/sqlitestore", "implements store.Store", "Same schema and upsert rules as src/db.js")
    Component(tee, "store.Tee", "Store decorator", "Saves to the primary Store, then to each mirror Writer; everything else goes to the primary")
    Component(apistore, "store/apistore", "implements store.Writer", "POSTs batches to the Worker's /api/listings with the bearer token; sums stats and newAlerts; https only (except localhost)")
    Component(scoringI, "scoring", "interface Scorer + ExecScorer, None", "Runs the Rust binary over JSON; None prices nothing (--no-score)")
    Component(geo, "geo", "FromHome()", "Distance by coordinates or postal-code area (library, not yet used by the CLI)")
  }
  Rel(main, searches, "loads the plan")
  Rel(main, pipeline, "constructs with Source, Store, Scorer")
  Rel(pipeline, sourceI, "uses")
  Rel(pipeline, storeI, "uses")
  Rel(pipeline, scoringI, "uses")
  Rel(autohebdo, fetchI, "uses")
  Rel(autohebdo, searchurl, "uses")
  Rel(autohebdo, parse, "uses")
  Rel(parse, describe, "uses")
  Rel(sqlite, storeI, "implements")
  Rel(tee, storeI, "implements Store, wraps sqlitestore")
  Rel(apistore, storeI, "implements Writer")
  Rel(main, tee, "with --push: Tee(sqlitestore, apistore)")
  Rel(autohebdo, sourceI, "implements")
```

## Level 3: Components of the Rust scorer

```mermaid
C4Component
  title Rust scorer: components (scorer/src/...)
  Container_Boundary(rs, "carbuyer-scorer (Rust)") {
    Component(main, "main.rs", "binary", "stdin JSON -> score::run -> stdout JSON")
    Component(score, "score.rs", "score_listing, score_against, Appraiser", "Deal scores; wires dealer model, discounts, private model")
    Component(baseline, "baseline.rs", "traits Baseline, SellerDiscount", "What scoring depends on")
    Component(model, "price_model.rs", "PriceModel implements Baseline", "Buckets, fallback tiers, km slope, trim factors")
    Component(discount, "discount.rs", "Discount implements SellerDiscount", "Private-party ratio per model and global")
    Component(trim, "trim.rs", "induce_vocabulary, assign_trim", "Trim names learned from the corpus")
    Component(support, "stats.rs, js.rs, listing.rs", "helpers", "Median/percentile, JS rounding and formatting, input shape")
  }
  Rel(main, score, "calls")
  Rel(score, baseline, "depends on traits only")
  Rel(model, baseline, "implements Baseline")
  Rel(discount, baseline, "implements SellerDiscount; calls Baseline::estimate")
  Rel(model, trim, "uses")
  Rel(score, model, "Appraiser builds")
  Rel(score, discount, "Appraiser builds")
```

## Level 3: Components of the Cloudflare Worker

```mermaid
C4Component
  title Cloudflare Worker: components (cloudflare/worker/src/...)
  Container_Boundary(w, "carbuyer-api (Rust, wasm32)") {
    Component(entry, "entry.rs", "#[event(fetch)], #[event(scheduled)]", "Composition root: routes, picks D1Store and the Notifier (NoNotifier today), reads the AlertRule vars, maps ApiError to HTTP")
    Component(auth, "auth.rs", "bearer_ok, read_allowed", "Constant-time token check; reads only via the internal service-binding host unless PUBLIC_READ_API")
    Component(service, "service.rs", "ingest_and_alert, deals, recent_alerts, take_snapshot; traits ListingStore, SnapshotStore, AlertStore", "Use cases written against small ports")
    Component(ingest, "ingest.rs", "Planner", "The Go store's upsert rules planned as statements; records fresh ids (new, relisted, cheaper)")
    Component(alerts, "alerts.rs", "AlertRule, find, statements, trait Notifier + NoNotifier", "Which fresh listings are strong deals; once-only keys (listing id + price); where notifications plug in")
    Component(deals, "deals.rs", "best_deals, DealsQuery", "Scores all active listings, filters make/model/seller, ranks, limits")
    Component(snapshot, "snapshot.rs", "statements, assemble", "Daily snapshot rows + pruning to the last 30")
    Component(sql, "sql.rs", "Stmt, Param", "Database-neutral SQL + positional params")
    Component(d1, "d1.rs", "D1Store implements ListingStore + SnapshotStore + AlertStore", "The only D1-aware code; runs batches atomically")
    Component(scorerlib, "scorer crate", "scorer::score::run", "Same library as the native binary (path dependency ../../scorer)")
  }
  ContainerDb(d1db, "D1 carbuyer", "SQLite", "... + new_deal_alerts")
  System_Ext(mail, "Email / push (later)", "Cloudflare Email Workers send_email, or an HTTP push service")
  Rel(entry, auth, "checks")
  Rel(entry, service, "calls with &D1Store, &NoNotifier, &AlertRule")
  Rel(service, ingest, "plans writes, gets fresh ids")
  Rel(service, alerts, "finds and records alerts, calls Notifier")
  Rel(service, deals, "scores")
  Rel(service, snapshot, "builds snapshot statements")
  Rel(deals, scorerlib, "calls")
  Rel(alerts, scorerlib, "calls")
  Rel(ingest, sql, "produces Stmt")
  Rel(alerts, sql, "produces Stmt")
  Rel(snapshot, sql, "produces Stmt")
  Rel(d1, service, "implements the ports")
  Rel(d1, d1db, "prepare/bind/batch")
  Rel(alerts, mail, "a future Notifier implementation sends here")
```

Snipe-alert flow on `POST /api/listings`: the planner upserts the batch and returns the
fresh ids; if there are none (the usual case once a search has been seen) nothing is
scored. Otherwise every active listing is scored once, fresh listings that pass the
`AlertRule` become `Alert`s, keys already in `new_deal_alerts` are dropped, the rest are
inserted (`INSERT OR IGNORE`) and handed to the `Notifier`. Alert errors are returned as
`alertError` and never undo the stored listings.

The Pages side is one file, `cloudflare/pages/functions/api/[[path]].js`: it
lets only GET/HEAD on the read routes (`/api/deals`, `/api/alerts`,
`/api/snapshots/latest`, `/api/health`) through and calls the Worker with
`env.API.fetch(...)` on the internal host, so the dashboard and its data share
one origin and one Access application.

## SOLID in this code

**Single responsibility.** Each package or module has one job.
- Go: `crawler/internal/searchurl` (URLs), `fetch` (HTTP), `parse` (page -> listings), `describe` (seller text), `autohebdo` (walking a query), `store/sqlitestore` (SQL), `scoring` (talking to the scorer), `pipeline` (the flow), `searches` (reading the scheduled-search file), `cmd/carbuyer` (flags and output).
- Rust: `scorer/src/price_model.rs` (baseline), `discount.rs` (private discount), `trim.rs` (trims), `score.rs` (deal scores), `main.rs` (I/O only).
- Go, Cloudflare side: `store/apistore` only speaks HTTP to the Worker; `store.Tee` only fans a save out to several writers.
- Worker: `ingest.rs` (upsert rules), `alerts.rs` (what is a strong new deal, its statements, the notifier port), `deals.rs` (scoring and ranking), `snapshot.rs` (cron rows), `auth.rs` (who may call what), `d1.rs` (D1 calls), `entry.rs` (HTTP and cron wiring). The scoring itself is not re-implemented: the Worker links the same `scorer` crate as the native binary.
- Scheduling is outside the code: `.github/workflows/crawl.yml` and `scripts/crawl-and-push.ps1` only build and run the CLI with `--searches`.

**Open/closed.** A new site, notifier or schedule is a new type or file, not an edit to the core.
- `crawler/internal/source/source.go` defines `Source`. Kijiji, LesPAC or Marketplace would each be a new package that implements it; `pipeline.go` does not change.
- `scorer/src/baseline.rs` defines `Baseline`. `score_listing` / `score_against` take `&dyn Baseline`, so a different baseline plugs in without changing the scoring rules.
- Pushing to Cloudflare was added without touching `pipeline.go`: `apistore` is a new `store.Writer`, and `store.Tee` wraps the existing store. The scorer crate was reused by the Worker without a single change.
- Snipe alerts were added without touching the scorer, `deals.rs` or `snapshot.rs`; the planner only gained a list of fresh ids. Email or push notifications are a new `Notifier` implementation chosen in `entry.rs`; `service.rs` and `alerts.rs` do not change (DEPLOY.md, "Email notifications later").
- Scheduled crawling reuses the pipeline as is: `--searches` just runs it once per entry of the plan.

**Liskov substitution.** Anything that satisfies the interface works in its place.
- The pipeline test (`crawler/internal/pipeline/pipeline_test.go`) runs the real `Pipeline` with fake source, store and scorer.
- `fetch.Static`, `fetch.HTTPFetcher` and the throttled wrapper are interchangeable `Fetcher`s; the CLI swaps them for `--offline`.
- `scoring.None` and `scoring.ExecScorer` are interchangeable `Scorer`s; `--no-score` swaps them and the pipeline runs unchanged.
- In Rust, the dealer and private `PriceModel`s are passed to `score_against` as the same `&dyn Baseline`.
- `store.Tee(db, pusher)` is a `store.Store` like `sqlitestore.DB`; the pipeline cannot tell them apart (`crawler/internal/store/tee_test.go`).
- In the Worker, `D1Store` and the in-memory `Fake` in `service.rs` tests both satisfy `ListingStore` + `SnapshotStore` + `AlertStore`; `NoNotifier` and the tests' recording `Outbox` (delivering or failing) both satisfy `Notifier`. The use cases run unchanged on either.

**Interface segregation.** Small interfaces.
- `crawler/internal/store/store.go` splits storage into `Writer`, `Reader`, `Remover`, `CrawlLog`; `Store` only composes them.
- `fetch.Fetcher` is one method. `scoring.Scorer` is one method. `SellerDiscount` in Rust is one method. `Notifier` is one method.
- `apistore.Client` implements only `store.Writer`: a push sink does not pretend to read, remove or log crawls.
- The Worker's ports are split: `ListingStore` (known, apply, active) for ingest and deals, `SnapshotStore` for the cron, `AlertStore` (alerted, save_alerts, recent_alerts) for alerts; only the use cases that need two of them ask for both (`take_snapshot`, `ingest_and_alert`), and `recent_alerts` needs `AlertStore` alone.

**Dependency inversion.** High-level code depends on abstractions; concrete types are injected.
- `pipeline.New(src, store, scorer)` takes interfaces. Only `crawler/cmd/carbuyer/main.go` names `autohebdo.New`, `sqlitestore.Open`, `scoring.ExecScorer` / `scoring.None`.
- `autohebdo.New(fetcher)` takes a `fetch.Fetcher`, not an HTTP client.
- No package-level mutable globals; clocks and sleeps are fields so tests can replace them.
- `main.go` is still the only place that picks concrete stores: `sqlitestore.Open`, and with `--push` `apistore.New` + `store.Tee`.
- Worker use cases take `&impl ListingStore` / `&impl AlertStore` / `&impl Notifier` and an `AlertRule` value; only `entry.rs` (the Worker's composition root) builds `D1Store` from the `DB` binding, picks `NoNotifier`, and reads the rule from `[vars]`. Planning code returns plain `Stmt` values, so everything except `d1.rs`/`entry.rs` is tested natively with `cargo test`.
- The Pages dashboard depends on the Worker through a service binding name (`API`), not a URL. The schedulers depend on the Worker through `CARBUYER_API_URL` and a token from the environment, never a value in the repo.
