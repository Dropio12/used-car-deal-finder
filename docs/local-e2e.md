# Local end-to-end test (Worker + D1 + Pages, no Cloudflare account)

Recorded on 2026-09-23 on Windows 11, wrangler 4.136.3 (workerd 2026-09-21),
worker-build 0.8.6, workers-rs 0.8.6, Go 1.27, Rust 1.94. Nothing was deployed.

Goal: push the offline fixture (`crawler/testdata/rav4-qc.html`, 20 listings)
through the Go CLI into the Worker running locally, then check that
`GET /api/deals` returns the same deals, in the same order, with the same
scores as the native pipeline (Go CLI + native `carbuyer-scorer` binary).

## Setup

```sh
# once
rustup target add wasm32-unknown-unknown
cargo install worker-build                     # build.mjs does this if missing
(cd scorer && cargo build --release)           # the native scorer the Go CLI uses

# fresh local D1 (state lives in cloudflare/worker/.wrangler/, git-ignored)
cd cloudflare/worker
cp .dev.vars.example .dev.vars                 # INGEST_TOKEN=local-dev-token-change-me, PUBLIC_READ_API=true
npx wrangler d1 migrations apply carbuyer --local
```

```
🚣 4 commands executed successfully.
┌─────────────────────────┬────────┐
│ name                    │ status │
├─────────────────────────┼────────┤
│ 0001_listings.sql       │ ✅     │
├─────────────────────────┼────────┤
│ 0002_deal_snapshots.sql │ ✅     │
└─────────────────────────┴────────┘
```

Two terminals (use the same `WRANGLER_REGISTRY_PATH` in both, see the notes at the end):

```sh
# terminal 1: the Worker (builds the wasm first via build.mjs)
cd cloudflare/worker
npx wrangler dev --local --port 8787 --ip 127.0.0.1 --test-scheduled
#   Using secrets defined in .dev.vars
#   env.DB (carbuyer)  D1 Database  local
#   [wrangler:info] Ready on http://127.0.0.1:8787

# terminal 2: the dashboard, bound to the Worker
cd cloudflare/pages
npx wrangler pages dev public --port 8788 --ip 127.0.0.1 --service API=carbuyer-api
#   env.API (carbuyer-api)  Worker  local [connected]
#   [wrangler:info] Ready on http://127.0.0.1:8788
```

`--service` is needed locally: `wrangler pages dev` does not apply the
`[[services]]` table from `cloudflare/pages/wrangler.toml` (deploys do).

## Run (commands from `crawler/`, exact output)

```
$ curl -s http://127.0.0.1:8787/api/deals | jq -c "{comps,scored,deals:(.deals|length)}"
{"comps":0,"scored":0,"deals":0}

$ curl -s -w " %{http_code}\n" -X POST -H "Authorization: Bearer wrong" http://127.0.0.1:8787/api/listings -d "[]"
{"error":"missing or wrong bearer token"} 401

$ CARBUYER_INGEST_TOKEN=local-dev-token-change-me go run ./cmd/carbuyer --make toyota --model rav4 --offline testdata/rav4-qc.html --min-comps 3 --db e2e.db --push http://127.0.0.1:8787 --json > cli.json
  pushed to http://127.0.0.1:8787/api/listings: 20 seen, 20 added, 0 relisted, 0 price drops, 0 price rises
  ! showing 1 of 44 pages — pass --pages 44 for all 861.
(exit 0)

$ curl -s "http://127.0.0.1:8787/api/deals?make=toyota&model=rav4&minComps=3&limit=200" > worker.json
$ node ../cloudflare/e2e/compare-deals.mjs cli.json worker.json
native: 20 comps, 15 priced | worker: 20 comps, 15 priced
  #1 +21.1%  2025 Toyota RAV4  36488 vs baseline 46241.5  (model-year, n=4)
  #2 +8.3%  2021 Toyota RAV4  25995 vs baseline 28349  (model-year, n=4)
  #3 +6.3%  2022 Toyota RAV4  25998 vs baseline 27748  (year-range, n=7)
  #4 +3.8%  2020 Toyota RAV4  24995 vs baseline 25995  (year-range, n=7)
  #5 +3%  2021 Toyota RAV4  22499 vs baseline 23185  (trim-year-range, n=3)
  (worker also returns: kmSlope, medianKm, trimFactor, yearsPooled)
MATCH: 15 deals, same order, identical scores

$ go run ./cmd/carbuyer --make toyota --model rav4 --offline testdata/rav4-qc.html --db e2e.db --json > cli8.json   # default minComps 8, no push
$ curl -s "http://127.0.0.1:8787/api/deals?make=toyota&model=rav4&limit=200" > worker8.json && node ../cloudflare/e2e/compare-deals.mjs cli8.json worker8.json
  (worker also returns: kmSlope, medianKm, trimFactor, yearsPooled)
MATCH: 8 deals, same order, identical scores

$ curl -s "http://127.0.0.1:8787/__scheduled?cron=0+11+*+*+*"
Ran scheduled event
$ curl -s http://127.0.0.1:8787/api/snapshots/latest | jq -c "{id,cron,comps,scored,deals:(.deals|length)}"
{"id":1,"cron":"0 11 * * *","comps":20,"scored":8,"deals":8}

$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8788/
200
$ curl -s "http://127.0.0.1:8788/api/deals?make=toyota&model=rav4&minComps=3&limit=200" > pages.json && node ../cloudflare/e2e/compare-deals.mjs cli.json pages.json
MATCH: 15 deals, same order, identical scores
$ curl -s -w " %{http_code}\n" -X POST http://127.0.0.1:8788/api/listings -d "[]"
{"error":"not found"} 404
```

A second push of the same page answers `20 seen, 0 added` (upsert, no duplicates).

With the production default (Worker restarted with `--var PUBLIC_READ_API:false`,
same D1 state; Pages restarted after it), reads are closed on the Worker's own
hostname and still work through the dashboard's service binding:

```
$ curl -s -w " %{http_code}\n" "http://127.0.0.1:8787/api/deals?minComps=3"
{"error":"not found"} 404
$ curl -s -w " %{http_code}\n" http://127.0.0.1:8787/api/snapshots/latest
{"error":"not found"} 404
$ curl -s "http://127.0.0.1:8788/api/deals?make=toyota&model=rav4&minComps=3&limit=200" > pages.json && node ../cloudflare/e2e/compare-deals.mjs cli.json pages.json
MATCH: 15 deals, same order, identical scores
$ curl -s http://127.0.0.1:8788/api/snapshots/latest | jq -c "{id,comps,scored,deals:(.deals|length)}"
{"id":1,"comps":20,"scored":8,"deals":8}
```

The dashboard at `http://127.0.0.1:8788/?make=toyota&model=rav4&minComps=3`
renders the 15 deals (price, baseline, discount, km, basis, link) with no
console errors (checked in headless Chromium).

## What "MATCH" means

`cloudflare/e2e/compare-deals.mjs` takes the CLI's `--json` report (scores from
the native Rust binary, through the Go pipeline) and the Worker's answer
(scores from the same `scorer` crate compiled to wasm, over rows read back from
D1), and requires: the same number of priced cars, the same ids in the same
rank order, and every score field the CLI reports identical at full float
precision (baseline, low, high, n, basis, trim, kmAdjustment, spread, warnings,
reliable, price, delta, discountPct, belowP25, damaged, thin, market). The
four fields listed as "worker also returns" are scorer diagnostics that the
Go `scoring.Score` struct does not decode, so they are absent from the CLI
JSON; the Rust test `deals::tests::scores_ranks_filters_and_limits` checks the
whole score object against a direct `scorer::score::run` call.

Why 15 priced here and 17 in the README's parity table: the parity script
scores the parsed page directly, while both the CLI and the Worker score the
rows stored in the database (SQLite / D1), which carry fewer fields. Native
and Worker agree on the stored rows.

## Snipe alerts (migration 0003)

Recorded 2026-09-23 with a fresh local D1 in a throwaway folder, with alerts lowered to
3 comparable cars so the 20-car fixture can trigger one (the production default is 6).
The token comes from `.dev.vars` and is not shown.

```sh
cd cloudflare/worker
npx wrangler d1 migrations apply carbuyer --local --persist-to <tmp>   # 0001, 0002, 0003 applied
npx wrangler dev --local --port 8797 --ip 127.0.0.1 --persist-to <tmp> \
  --var ALERT_MIN_COMPS:3 --var PUBLIC_READ_API:true
```

From `crawler/`:

```
$ curl -s http://127.0.0.1:8797/api/alerts
{"generatedAt":"...","rule":{"enabled":true,"minDiscountPct":15,"minComps":3,"includeDamaged":false},"alerts":[]}

$ go run ./cmd/carbuyer --make toyota --model rav4 --offline testdata/rav4-qc.html --no-score --db e1.db --push http://127.0.0.1:8797
  pushed to http://127.0.0.1:8797/api/listings: 20 seen, 20 added, 0 relisted, 0 price drops, 0 price rises, 1 new deal alerts

$ go run ./cmd/carbuyer --make toyota --model rav4 --offline testdata/rav4-qc.html --no-score --db e2.db --push http://127.0.0.1:8797
  pushed to http://127.0.0.1:8797/api/listings: 20 seen, 0 added, 0 relisted, 0 price drops, 0 price rises, 0 new deal alerts

$ curl -s "http://127.0.0.1:8797/api/alerts?limit=5"      # summarized
1 alert: id 00000000-0000-4000-8000-000000000007, price 36488, discountPct 21.1, comps 4, notifiedAt null

$ curl -s -w " %{http_code}\n" "http://127.0.0.1:8797/api/alerts?limit=x"
{"error":"limit: \"x\" is not a whole number"} 400
```

That is the car the native scorer puts 21.1 % under its baseline with `minComps 3`.
The second push has the same cars at the same prices, so nothing is fresh, and nothing
is scored or alerted again.

## Notes for this Windows machine

- `workerd.exe` (the local Workers runtime) exits with `0xC0000022` because
  `C:\Windows\System32\vcruntime140.dll`, `vcruntime140_1.dll` and
  `msvcp140.dll` are not readable here. Wrangler then fails with
  `✘ [ERROR] write EOF`. Workaround used: copy those three DLLs from
  `C:\Program Files\Common Files\microsoft shared\ClickToRun\` next to
  `workerd.exe` in the npx cache. Proper fix: repair the Visual C++
  Redistributable. Deploying does not run workerd.
- The same unreadable DLL breaks dynamically linked Rust build scripts; the
  Worker's `.cargo/config.toml` + `build.mjs` link them statically.
- `wrangler dev` registered the Worker in
  `%APPDATA%\xdg.config\.wrangler\registry`, but `wrangler pages dev` looked
  elsewhere and reported the binding `[not connected]`. Setting the same
  `WRANGLER_REGISTRY_PATH=<some folder>` in both terminals fixes it. After
  restarting the Worker, restart `pages dev` too.
