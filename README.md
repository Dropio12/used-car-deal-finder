# Used-Car Deal Finder (carbuyer)

A tool for flipping used cars in Quebec: it crawls real listings, prices each car against
similar local listings, and flags the ones listed well under their local market value,
fast enough to get to them first. It is a Go + Rust port of the core of carbuyer, an
earlier JavaScript tool (not part of this repo).

- **Go** (`crawler/`): builds AutoHebdo search URLs, fetches pages, parses the listings,
  stores them in SQLite, calls the scorer, prints the best deals. `--searches` runs a
  list of saved searches politely; `--push` sends the listings to the Worker.
- **Rust** (`scorer/`): builds the local price baseline and scores each listing
  (how far under the baseline it is priced).
- **Cloudflare** (`cloudflare/`): a Rust Worker (workers-rs) that reuses the scorer crate,
  stores listings in D1, records **snipe alerts** for strong new deals (optionally sent to
  Telegram through Composio, with an AI photo check on Baseten), re-scores daily
  with a Cron Trigger, and a small Pages dashboard with a "New deals" section. Runs
  privately behind Cloudflare Access; [DEPLOY.md](DEPLOY.md) has every step.
- **Schedule** (`.github/workflows/crawl.yml`, `scripts/crawl-and-push.ps1`): crawls the
  searches in `crawler/searches.yml` every 30 minutes and pushes them to the Worker.

The flow in production:

```
 GitHub Actions (every 30 min)          Cloudflare                          You
 or Windows Task Scheduler
 carbuyer --searches searches.yml  ──►  Worker POST /api/listings  ──► D1 listings
   (1 request at a time, polite)          │ new / cheaper listing?
                                          ▼ score vs local baseline
                                        >= 15 % under, >= 6 comps ──► D1 new_deal_alerts
                                                                        │
                                        Pages dashboard "New deals" ◄───┘ ◄── Cloudflare Access
```

The goal of the port was to keep behavior identical to the JavaScript. The parity check below
shows that it is, on the saved test page and on a large synthetic market.

Architecture diagrams (C4) and how the code follows SOLID: [docs/architecture.md](docs/architecture.md).

## How it fits together

```
                 carbuyer CLI (Go)                               carbuyer-scorer (Rust)
 ┌──────────────────────────────────────────────┐         ┌──────────────────────────────┐
 │ searchurl ─► fetch ─► parse(+describe)        │         │ trim  ─► price_model (dealer) │
 │     └──────── autohebdo (Source) ──┐          │  JSON   │            │   (private)      │
 │                                    ▼          │ stdin   │ discount ◄─┘                  │
 │                               pipeline ───────┼────────►│ score (Appraiser)             │
 │                                 │  ▲          │ stdout  │                               │
 │                                 ▼  │          │◄────────┤                               │
 │                          store/sqlitestore    │         └──────────────────────────────┘
 └─────────────────────────────────┬────────────┘
                                   ▼
                             carbuyer.db (SQLite, same schema as the JS project)
```

1. `autohebdo` builds the URL (geo is a path segment like `/reg_qc`; `zip` and the
   year filter are never sent, see PLAN.md in the original), fetches with a normal
   browser User-Agent, 1.5 s between requests, and parses `__NEXT_DATA__`.
2. Injected out-of-region "Deliverable" listings are dropped; year filters run locally;
   a typo'd make/model is caught via `pageQuery.cat`.
3. `sqlitestore` upserts listings with price history (same rules as `src/db.js`).
4. The pipeline loads every active AutoHebdo listing (dealer + private) and sends it
   to the Rust scorer as JSON. The scorer builds the dealer baseline, the per-source
   private-party discount and the pooled private baseline, exactly like `buildAppraiser`.
5. The CLI prints the found listings, then the best deals.

## Build, test, run

Needs Go (1.27, see crawler/go.mod) and Rust (stable). No C compiler: SQLite is `modernc.org/sqlite` (pure Go).

```sh
# Rust scorer
cd scorer
cargo test
cargo clippy --all-targets      # clean
cargo build --release           # -> scorer/target/release/carbuyer-scorer(.exe)

# Go crawler
cd ../crawler
go vet ./...                    # clean
go test ./...

# Run offline against the saved page (no network)
go run ./cmd/carbuyer --make toyota --model rav4 --offline testdata/rav4-qc.html --min-comps 3

# Run live (polite: 1 page = 1 request)
go run ./cmd/carbuyer --make toyota --model rav4 --pages 3
go run ./cmd/carbuyer --make honda --model civic --private --max-price 15000
```

CLI flags match `src/cli.js` (`--make --model --geo --private --dealer --min-price
--max-price --min-year --max-year --sort --pages --json`), plus `--db`, `--scorer`,
`--min-comps`, `--top`, `--offline`, `--push`, `--searches` and `--no-score` (see below). The scorer binary can also be used alone:

```sh
carbuyer-scorer < listings.json > scores.json
# input: [listing, ...]  or  {"listings": [...], "options": {"model": {"minComps": 3}}}
```

## Cloudflare: Worker + D1 + Pages

```
cloudflare/
  worker/         Rust Worker (workers-rs 0.8), wrangler.toml, build.mjs (worker-build)
  d1/migrations/  0001 = the crawler's SQLite schema, 0002 = deal snapshot tables,
                  0003 = new_deal_alerts (snipe alerts)
  pages/          static dashboard (public/) + Pages Function proxy (functions/api/[[path]].js)
  e2e/            compare-deals.mjs: Worker answer vs native CLI answer
```

| Route (Worker) | What | Auth |
|---|---|---|
| `POST /api/listings` | Ingest listings (the crawler's JSON: `[...]` or `{"listings": [...], "scope": {...}}`), upserted into D1 with the same rules as `sqlitestore`; then new/relisted/cheaper listings are checked for snipe alerts. Answers the save stats plus `newAlerts` | `Authorization: Bearer <INGEST_TOKEN>` (Worker secret) |
| `GET /api/deals?make=&model=&seller=dealer\|private&limit=&minComps=` | Scores every active listing with the Rust scorer, returns the best deals (price, baseline, discount, link, full score) | via the dashboard (service binding + Access) |
| `GET /api/snapshots/latest` | What the daily cron stored | via the dashboard |
| `GET /api/alerts?limit=` | Newest snipe alerts (listing + score + `alertedAt`), and the alert rule in use | via the dashboard |
| `GET /api/health` | liveness | none |
| Cron `0 11 * * *` | Re-score, store top 100 in `deal_snapshots` / `deal_snapshot_items`, keep 30 runs | - |

Push from the crawler (listings still go to SQLite first; the Worker is a mirror):

```sh
export CARBUYER_INGEST_TOKEN=...            # never a flag, so it stays out of shell history
go run ./cmd/carbuyer --make toyota --model rav4 --push https://carbuyer-api.<you>.workers.dev
```

Build and run locally (no Cloudflare account needed):

```sh
cd cloudflare/worker
cargo test                                         # 31 native unit tests (no wasm needed)
cp .dev.vars.example .dev.vars
npx wrangler d1 migrations apply carbuyer --local
npx wrangler dev --local --test-scheduled          # builds the wasm, serves :8787
# other terminal
cd cloudflare/pages && npx wrangler pages dev public --service API=carbuyer-api   # :8788
```

Local end-to-end result (exact commands and output: [docs/local-e2e.md](docs/local-e2e.md)):
the fixture pushed through `--push` into the local Worker gives **the same 15 deals
(minComps 3) and 8 deals (default), same order, identical scores** as the native
CLI + scorer binary, directly and through the Pages proxy. The cron snapshot works.
Snipe alerts, same local setup with `ALERT_MIN_COMPS=3`: the first push of the fixture
raises exactly 1 alert (a RAV4 21.1 % under a 4-car baseline), the second push of the
same cars raises 0, and `/api/alerts` lists it.
Deploying: [DEPLOY.md](DEPLOY.md).

**Windows note:** `scorer/.cargo/config.toml` links the C runtime statically. On the
machine this was built on, the shared `vcruntime140.dll` could not be read, so normal
Rust binaries (and cargo build scripts) failed to start. Static linking fixes that and
makes the scorer a single self-contained file.

## Scheduled crawl on real listings

`crawler/searches.yml` lists the searches (make/model slugs, price cap, pages, newest
first). Edit it freely; unknown keys are refused so a typo cannot drop a filter.

```sh
cd crawler
go run ./cmd/carbuyer --searches searches.yml --no-score            # dry run: local SQLite only
CARBUYER_INGEST_TOKEN=... go run ./cmd/carbuyer --searches searches.yml --no-score   --push https://carbuyer-api.<you>.workers.dev                      # what the schedule runs
```

Politeness is built in: searches run one after another through one throttled fetcher
(one request at a time, at least `delaySeconds` apart, never under 1.5 s), with a normal
browser User-Agent, at most 25 searches x 10 pages, and the run **stops at the first
error** (a block page or HTTP error gets no follow-up requests).

Two ways to run it every 30 minutes (pick one, not both):

- **GitHub Actions**: `.github/workflows/crawl.yml` (cron `*/30 * * * *` + manual
  "Run workflow"). Needs the repository secret `CARBUYER_INGEST_TOKEN` and variable
  `CARBUYER_API_URL` ([DEPLOY.md](DEPLOY.md), step 10). Skipped until the variable is set.
- **Windows**: `scripts/crawl-and-push.ps1` reads `CARBUYER_INGEST_TOKEN` and
  `CARBUYER_API_URL` from your user environment, builds the crawler, logs to
  `%LOCALAPPDATA%carbuyerlogs`; the script's header has the Task Scheduler command.
  Useful if the site ever blocks cloud IP ranges.

## Snipe alerts

Each push is checked on the Worker: a listing that is **new**, **relisted** or **cheaper
than before** is scored against every stored listing, and if it is at least
`ALERT_MIN_DISCOUNT_PCT` (15) % under the local baseline built from at least
`ALERT_MIN_COMPS` (6) comparable cars (not damaged, not a thin estimate, not a parts car,
not a conditional price), it is stored in `new_deal_alerts`. The key is listing id + price,
so a car is alerted once, and once more only if its price drops again. The dashboard shows
them at the top under **New deals** (refreshed every 2 minutes). Settings are `[vars]` in
`cloudflare/worker/wrangler.toml`.

Notifications: alerts go through a `Notifier` trait (`cloudflare/worker/src/alerts.rs`).
Today it is `NoNotifier` (dashboard only). Email or push is one new type plus one line in
`entry.rs`; [DEPLOY.md](DEPLOY.md) ("Email notifications later") has the recipe.

## What is ported

| JS file | Port | Notes |
|---|---|---|
| `src/url.js` | `crawler/internal/searchurl` | Same URLs byte for byte |
| `src/fetch.js` | `crawler/internal/fetch` | Retries, backoff, throttle, `answered()` |
| `src/parse.js` | `crawler/internal/parse` | |
| `src/describe.js` | `crawler/internal/describe` | Needed by the parser for damage/parts flags |
| `src/geo.js` | `crawler/internal/geo` | Ported and tested; the CLI does not use it yet |
| `src/index.js` (search, searchAll, verifyQueryResolved) | `crawler/internal/autohebdo` | |
| `src/db.js` (schema, upsert, markRemoved, crawls, loadListings, stats) | `crawler/internal/store/sqlitestore` | |
| `src/cli.js` | `crawler/cmd/carbuyer` | Also stores and scores |
| `src/price-model.js` | `scorer/src/price_model.rs`, `score.rs` | |
| `src/discount.js` | `scorer/src/discount.rs` | |
| `src/trim.js` | `scorer/src/trim.rs` | Lives in Rust because the price model needs it |
| `buildAppraiser` from `src/watch.js` | `scorer/src/score.rs` (`Appraiser`) | Only the wiring, not the watcher |

## Not ported yet

- Facebook Marketplace (`src/facebook.js`, `src/marketplace.js`), Kijiji (`src/kijiji.js`),
  LesPAC (`src/lespac.js`) scrapers. The `source.Source` interface is where they would plug in.
- The watcher (`src/watch.js`, `scripts/watch.js`), the site and server
  (`scripts/site.js`, `scripts/serve.js`), and the other scripts (deals page, inspect, backup, ...).
- From `src/index.js` / `src/crawl.js`: price-band sharding (`planShards`), `crawlScope`
  and incremental "new only" crawls. The CLI walks one query; removal detection runs only
  when that walk covered the whole scope.
- From `src/db.js`: meta/lastRun helpers, canonical names, vocabulary, price-drops query.

## Parity result

`node parity/parity.mjs` runs the original JS modules (read-only, from `../carbuyer`,
or `CARBUYER_JS=...`) next to the port. No network. Last run: **all 15 checks passed.**

| Check | Result |
|---|---|
| Parser: 20 listings from `rav4-qc.html`, every field, plus totals/query | identical |
| Scorer on the fixture, default options | identical (8 priced, 12 refused) |
| Scorer on the fixture, `minComps 3` | identical (17 priced, 3 refused) |
| Scorer on 4 000 synthetic listings, 3 option sets | identical, every number to full float precision (about 2 700 priced per run, all fallback tiers, trim factors, per-source discounts, private fallback) |
| Appraiser summary (discounts, coverage, rejects) | identical |
| SQLite schema: JS `openDb()` vs Go `Open()` | identical tables, columns, indexes |
| JS `loadListings()` reading the rows Go stored | identical |

The fixture alone is only 20 cars, so with default settings most cars have too few
comparables to be priced. That is correct behavior (the JS refuses them too), which is
why the synthetic market is there to exercise the scorer properly.

Known, deliberate edge differences (none show up in the fixture or the synthetic data):
- HTML entities above U+10FFFF: JS throws, Go leaves the text as-is.
- A JSON field of the wrong type (e.g. a year sent as a string) is treated as missing.
- Regex character counts use runes in Go and UTF-16 units in JS; they differ only for
  emoji inside the 60-character windows of the description reader. The 30-character
  negation window is counted in UTF-16, like JS.
- The Go CLI refuses a non-number `--max-price` instead of sending `NaN`.

## Tests

- Rust: 46 unit tests across all modules (`cargo test`), clippy clean.
- Worker (`cloudflare/worker`): 31 unit tests (`cargo test`, native) for the upsert planner, deal
  ranking against the scorer, snapshot statements, auth, the alert rule and statements, and
  the use cases over a fake store (ingest + alerts with a recording notifier, alert list);
  clippy clean for the host and for `wasm32-unknown-unknown`.
- Go: 137 test cases (table-driven, per package), `go vet` clean. Includes offline parser
  tests on `testdata/rav4-qc.html`, a local `httptest` server for fetch, fake fetchers for
  the AutoHebdo walk, a temp SQLite file for the store, a helper-process fake for the
  scorer exec client, and in-memory fakes for the pipeline. Nothing hits the live site.
- Go, Cloudflare additions: `store.Tee` (fakes), `store/apistore` (`httptest` fake Worker: batching,
  bearer token, scope, error messages that never contain the token, URL rules), the CLI with
  `--push` on the offline fixture against an `httptest` server, and a schema test that applies
  `cloudflare/d1/migrations` to SQLite and checks they match `sqlitestore` column for column.

`testdata/rav4-qc.html` is a saved public AutoHebdo search page, **anonymized** with
`node tools/anonymize-fixture/anonymize.mjs <saved.html> crawler/testdata/rav4-qc.html`:
only the `__NEXT_DATA__` payload is kept (the parsers read nothing else), and dealer
names, phone numbers, street addresses, listing/seller ids, listing and image URLs are
replaced with consistent fakes ("Dealer A", 514-555-01xx, example.com, 00000000-...-0001).
Everything the parser and scorer use for pricing (price, year, km, make/model/trim,
seller type, province, city, postal-code area, description text) is unchanged, so
every test and parity number above is the same as on the original page.
To refresh the fixture: save a new page, run the script on it, and check it with
`--verify-against <saved.html>` before committing. Never commit the raw page.
